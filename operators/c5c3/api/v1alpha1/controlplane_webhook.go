// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/policy"
	"github.com/c5c3/cobaltcore/internal/common/release"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// ControlPlane defaulting constants. These are the single source of
// truth shared by the defaulting webhook and (where relevant) the validation
// error messages, so the defaults cannot drift across call sites. The matching
// +kubebuilder:default markers on the spec fields remain as defense-in-depth
// for callers that bypass this webhook (e.g. envtest without the defaulter
// wired up) — kubebuilder markers require literals and cannot reference these
// Go constants.
const (
	// DefaultRegion is materialized when spec.region is empty (plan decision #4).
	DefaultRegion = "RegionOne"
	// DefaultCloudCredentialsSecretName is materialized when
	// spec.korc.adminCredential.cloudCredentialsRef.secretName is empty.
	DefaultCloudCredentialsSecretName = "k-orc-clouds-yaml" //nolint:gosec // G101 false positive: Secret name, not a credential
	// well-known defaults for the database, cache, and admin-credential
	// fields so a minimal managed-mode ControlPlane can omit spec.infrastructure
	// and the spec.korc.adminCredential body. The shared commonv1 leaves
	// (DatabaseSpec, CacheSpec, SecretRefSpec) are defaulted webhook-only — never
	// via a +kubebuilder:default marker — because the keystone operator reuses
	// those types and a c5c3-specific default would leak.
	//
	// DefaultDatabaseName is materialized when spec.infrastructure.database.database is empty.
	DefaultDatabaseName = "keystone"
	// DefaultDatabaseSecretName is materialized when spec.infrastructure.database.secretRef.name is empty.
	DefaultDatabaseSecretName = "keystone-db" //nolint:gosec // G101 false positive: Secret name, not a credential
	// DefaultDatabaseClusterRefName is the managed MariaDB CR name materialized when
	// spec.infrastructure.database is in managed mode (host unset).
	DefaultDatabaseClusterRefName = "openstack-db"
	// DefaultCacheBackend is materialized when spec.infrastructure.cache.backend
	// is empty. It aliases commonv1.DefaultCacheBackend so the keystone and c5c3
	// operators share one source of truth for the cache backend default.
	DefaultCacheBackend = commonv1.DefaultCacheBackend
	// DefaultCacheClusterRefName is the managed Memcached CR name materialized when
	// spec.infrastructure.cache is in managed mode (servers unset).
	DefaultCacheClusterRefName = "openstack-memcached"
	// DefaultMessagingClusterRefName is the managed RabbitmqCluster CR name
	// materialized when spec.infrastructure.messaging is declared in managed
	// mode (secretRef unset) without a clusterRef name.
	DefaultMessagingClusterRefName = "openstack-rabbitmq"
	// The dedicated-backing-service clusterRef names are derived from the
	// ControlPlane's own name so a per-service instance never collides with the
	// shared one (openstack-db / openstack-memcached) nor with another
	// ControlPlane's instance in a shared namespace. They are materialized when a
	// dedicated block declares a managed instance without naming it.
	//
	// DedicatedKeystoneDatabaseClusterRefSuffix names the MariaDB CR of a
	// dedicated Keystone database.
	DedicatedKeystoneDatabaseClusterRefSuffix = "-keystone-db" //nolint:gosec // G101 false positive: CR name suffix, not a credential
	// DedicatedKeystoneCacheClusterRefSuffix names the Memcached CR of a dedicated
	// Keystone cache.
	DedicatedKeystoneCacheClusterRefSuffix = "-keystone-cache"
	// DedicatedHorizonCacheClusterRefSuffix names the Memcached CR of a dedicated
	// Horizon cache.
	DedicatedHorizonCacheClusterRefSuffix = "-horizon-cache"
	// DedicatedGlanceDatabaseClusterRefSuffix names the MariaDB CR of a dedicated
	// Glance database.
	DedicatedGlanceDatabaseClusterRefSuffix = "-glance-db" //nolint:gosec // G101 false positive: CR name suffix, not a credential
	// DedicatedGlanceCacheClusterRefSuffix names the Memcached CR of a dedicated
	// Glance cache.
	DedicatedGlanceCacheClusterRefSuffix = "-glance-cache"
	// DedicatedPlacementDatabaseClusterRefSuffix names the MariaDB CR of a
	// dedicated Placement database.
	DedicatedPlacementDatabaseClusterRefSuffix = "-placement-db" //nolint:gosec // G101 false positive: CR name suffix, not a credential
	// DedicatedPlacementCacheClusterRefSuffix names the Memcached CR of a
	// dedicated Placement cache.
	DedicatedPlacementCacheClusterRefSuffix = "-placement-cache"
	// DedicatedBarbicanDatabaseClusterRefSuffix names the MariaDB CR of a
	// dedicated Barbican database.
	DedicatedBarbicanDatabaseClusterRefSuffix = "-barbican-db" //nolint:gosec // G101 false positive: CR name suffix, not a credential
	// DedicatedBarbicanCacheClusterRefSuffix names the Memcached CR of a
	// dedicated Barbican cache.
	DedicatedBarbicanCacheClusterRefSuffix = "-barbican-cache"
	// DedicatedNeutronDatabaseClusterRefSuffix names the MariaDB CR of a
	// dedicated Neutron database.
	DedicatedNeutronDatabaseClusterRefSuffix = "-neutron-db" //nolint:gosec // G101 false positive: CR name suffix, not a credential
	// DedicatedNeutronCacheClusterRefSuffix names the Memcached CR of a
	// dedicated Neutron cache.
	DedicatedNeutronCacheClusterRefSuffix = "-neutron-cache"
	// DefaultDatabaseStorageSize is the effective per-replica MariaDB volume size
	// when spec.infrastructure.database.storageSize is empty. It aliases
	// commonv1.DatabaseStorageSizeDefault (also the CRD +kubebuilder:default and
	// the c5c3 fresh-create fallback) so validateImmutable normalizes an empty
	// stored value to the exact size the live MariaDB already uses. StorageSize is
	// defaulted by the CRD marker rather than Default() below, so this constant is
	// only consulted by the immutability check, not materialized onto the object.
	DefaultDatabaseStorageSize = commonv1.DatabaseStorageSizeDefault
	// DefaultAdminPasswordSecretName is materialized when
	// spec.korc.adminCredential.passwordSecretRef.name is empty.
	DefaultAdminPasswordSecretName = "keystone-admin" //nolint:gosec // G101 false positive: Secret name, not a credential
	// DefaultAdminPasswordSecretKey is materialized when
	// spec.korc.adminCredential.passwordSecretRef.key is empty. Unlike the Secret
	// *name* constants above (which carry a //nolint:gosec G101 false-positive
	// annotation), "password" is the Secret data KEY — the field name within the
	// Secret (SecretRefSpec.Key), not credential material — so it correctly needs
	// no G101 nolint.
	DefaultAdminPasswordSecretKey = "password"
	// DefaultCloudName is materialized when
	// spec.korc.adminCredential.cloudCredentialsRef.cloudName is empty.
	DefaultCloudName = "admin"
	// DefaultExternalEndpointType is materialized when
	// spec.services.keystone.external.endpointType is empty. It mirrors the
	// +kubebuilder:default=public marker on ExternalKeystoneSpec.EndpointType.
	DefaultExternalEndpointType = ExternalEndpointTypePublic
	// DefaultCABundleSecretKey is materialized when
	// spec.services.keystone.external.caBundleSecretRef.key is empty. It is
	// webhook-only because the shared SecretRefSpec carries no c5c3-specific
	// marker (the same discipline as passwordSecretRef.Key). "ca.crt" matches the
	// PEM key K-ORC reads inline from the credentials Secret.
	DefaultCABundleSecretKey = "ca.crt"
	// DefaultAdminUserName is materialized when
	// spec.korc.adminCredential.userName is empty. Webhook-only: the field carries
	// a +kubebuilder:default=admin marker for the normal admission path.
	DefaultAdminUserName = "admin"
	// DefaultAdminProjectName is materialized when
	// spec.korc.adminCredential.projectName is empty (mirrors the CRD default).
	DefaultAdminProjectName = "admin"
	// DefaultBarbicanKVMountpoint is materialized when
	// spec.services.barbican.secretStore.external.kvMountpoint is empty. It
	// mirrors the +kubebuilder:default=barbican marker on
	// BarbicanExternalSecretStoreSpec.KVMountpoint.
	DefaultBarbicanKVMountpoint = "barbican"
	// DefaultAdminDomainName is materialized when
	// spec.korc.adminCredential.domainName is empty (mirrors the CRD default). The
	// single domain feeds both user_domain_name and project_domain_name in the
	// generated clouds.yaml.
	DefaultAdminDomainName = "Default"
)

// controlPlaneReleaseRegexp mirrors the +kubebuilder:validation:Pattern marker
// on ControlPlaneSpec.OpenStackRelease. The [12] minor class matches the
// two-releases-per-year OpenStack cadence that release.ParseRelease also
// enforces, so validate() rejects a non-cadence minor (e.g. 2025.9) instead of
// letting validateReleaseNotDowngraded silently skip the downgrade check for a
// value ParseRelease cannot parse. The validating webhook re-checks the pattern
// as defense-in-depth for callers that bypass CRD schema admission.
var controlPlaneReleaseRegexp = regexp.MustCompile(`^\d{4}\.[12]$`)

// maxExternalAuthURLBytes mirrors the +kubebuilder:validation:MaxLength=2048 marker
// on ExternalKeystoneSpec.AuthURL. The cap exists because the reconciler
// interpolates authURL into status.conditions[].message, whose 32768-byte apiserver
// limit is a whole-object constraint — see the marker's doc comment. It is applied
// at the authURL call site rather than inside validateHTTPURL: the cap belongs to
// this one field, and the helper's other callers carry MaxLength markers of their
// own (512 on services.horizon.publicEndpoint).
const maxExternalAuthURLBytes = 2048

// maxCatalogEndpointURLBytes mirrors the MaxLength marker on
// KeystoneServiceEndpointSpec.URL, which in turn mirrors K-ORC's own
// EndpointResourceSpec.URL cap. A URL admitted here can therefore never be
// rejected downstream by the K-ORC CRD.
const maxCatalogEndpointURLBytes = 1024

// validateHTTPURL enforces that raw is a well-formed absolute HTTP(S) URL with a
// host, going beyond the coarse ^https?:// CRD Pattern markers: the unusable
// shapes (missing host, non-http(s) scheme, opaque or relative references,
// control characters) are rejected at admission rather than wedging the
// reconciler that consumes them. This is a shape gate, not an SSRF control —
// admission cannot resolve where the host points, so a dialing reconciler must
// still enforce network egress restrictions. It returns the parsed URL so
// callers can apply further per-field rules (byte caps, path/query checks)
// without re-parsing.
func validateHTTPURL(path *field.Path, raw string) (*url.URL, *field.Error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, field.Invalid(path, raw, "must be a valid http(s) URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, field.Invalid(path, raw, "must be an http(s) URL (scheme http or https)")
	case u.Host == "":
		return nil, field.Invalid(path, raw, "must include a host")
	}
	return u, nil
}

// validateHorizonPublicEndpoint enforces the rules on
// services.horizon.publicEndpoint that the CRD markers cannot express. The
// reconciler derives the dashboard's WebSSO origin from the value
// (publicEndpoint + "/auth/websso/") and projects it onto the Keystone child's
// [federation] trusted_dashboard.
//
//   - Shape, as defense-in-depth alongside the ^https?:// Pattern marker:
//     Keystone compares the origin verbatim, so a value that parses to no host
//     could never match any dashboard.
//   - A bare origin, with no path, query or fragment. The Pattern marker anchors
//     only the prefix, so "https://horizon.example.com?utm=1" is schema-legal,
//     survives the URL parse, agrees with gateway.hostname, and yields the
//     nonsense trusted origin "https://horizon.example.com?utm=1/auth/websso/" —
//     which Keystone's own validation accepts and then never matches, failing
//     every federated login AFTER the user authenticated at the IdP, with no
//     status, log, or admission error naming the cause. A path fails the same
//     way: Django derives the origin it sends from the request Host header and
//     mounts the dashboard at the root unless FORCE_SCRIPT_NAME is configured,
//     which this operator does not manage.
//   - With a gateway configured the listener terminates TLS, so the
//     browser-observed scheme is https — the same rule the Keystone CR applies
//     to its bootstrap.publicEndpoint. An http origin is also a token leak:
//     after the IdP authenticates the user, Keystone POSTs the unscoped WebSSO
//     token to this origin, and that bearer token grants the user's full API
//     privileges until it expires.
//   - With a gateway configured the host must equal gateway.hostname. Django
//     derives the origin it sends to Keystone from the request's Host header —
//     i.e. from gateway.hostname, never from this field — so a divergent host
//     produces an origin Keystone rejects, and it rejects it only AFTER the
//     user has already entered their corporate credentials at the IdP. The port
//     may still differ: Gateway API hostnames carry none, so a dashboard
//     published off 443 has to spell the port out here.
func validateHorizonPublicEndpoint(specPath *field.Path, hz *ServiceHorizonSpec) field.ErrorList {
	if hz == nil || hz.PublicEndpoint == "" {
		return nil
	}
	pePath := specPath.Child("services", "horizon", "publicEndpoint")
	u, err := validateHTTPURL(pePath, hz.PublicEndpoint)
	if err != nil {
		return field.ErrorList{err}
	}

	var errs field.ErrorList
	// A single trailing slash is the one path the reconciler tolerates:
	// DerivedPublicEndpoint trims it before appending "/auth/websso/".
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, field.Invalid(pePath, hz.PublicEndpoint,
			"must be a bare origin (scheme://host[:port]) with no path, query, or fragment: the WebSSO origin is "+
				`derived as publicEndpoint+"/auth/websso/" and Keystone compares it verbatim`))
	}

	g := hz.Gateway
	if g == nil || g.Hostname == "" {
		return errs
	}

	if u.Scheme != "https" {
		errs = append(errs, field.Invalid(pePath, hz.PublicEndpoint,
			"scheme must be https when services.horizon.gateway is configured (the Gateway listener terminates TLS): "+
				"Keystone POSTs the unscoped WebSSO token to this origin, so http would deliver a bearer token in cleartext"))
	}
	if u.Hostname() != g.Hostname {
		errs = append(errs, field.Invalid(pePath, hz.PublicEndpoint,
			fmt.Sprintf("host %q must equal services.horizon.gateway.hostname %q: the dashboard derives the WebSSO "+
				"origin it sends from the request Host header, and Keystone compares it verbatim",
				u.Hostname(), g.Hostname)))
	}
	return errs
}

// warnInsecureHorizonPublicEndpoint surfaces a cleartext WebSSO hand-off that
// validateHorizonPublicEndpoint cannot reject: without a gateway the dashboard
// is published by some other means, and a plain-http origin is a legal, if
// unwise, development setup that the ^https?:// CRD Pattern deliberately allows.
// The downgrade must never be silent, though — the token Keystone POSTs to this
// origin is readable by any on-path observer and grants the user's full API
// privileges, not just dashboard access.
func warnInsecureHorizonPublicEndpoint(cp *ControlPlane) admission.Warnings {
	hz := cp.Spec.Services.Horizon
	if hz == nil || hz.PublicEndpoint == "" {
		return nil
	}
	if u, err := url.Parse(hz.PublicEndpoint); err != nil || u.Scheme != "http" {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.services.horizon.publicEndpoint %q uses http://: the WebSSO origin derived from it is projected onto "+
			"the Keystone child's trusted_dashboard, and Keystone POSTs the unscoped WebSSO token to that origin, so "+
			"every federated login would deliver a bearer token in cleartext. Use https://.",
		hz.PublicEndpoint,
	)}
}

// maxGatewayHostnameLen is the maximum length of a DNS name (RFC 1035). The
// commonv1.GatewaySpec.Hostname marker is MinLength=1 only, so admission would
// otherwise accept a hostname long enough to overrun the children's own
// MaxLength markers on the origins derived from it — see validateGatewayHostname.
const maxGatewayHostnameLen = 253

// validateGatewayHostname enforces that a services.<svc>.gateway.hostname is a
// concrete, port-free DNS name of usable length. The CRD marker on
// commonv1.GatewaySpec.Hostname is MinLength=1 only, but the reconciler derives
// BROWSER-facing origins from the value ("https://"+hostname) and projects them
// onto the children: the Keystone child's [federation] trusted_dashboard, which
// Keystone compares against the dashboard's origin byte-for-byte, and the
// Horizon child's websso.keystoneURL. Four shapes the reconciler cannot use
// pass every other gate:
//
//   - A control character (a pasted newline) survives the children's
//     ^https?:// Pattern markers — RE2 anchors ^ at start-of-text, not
//     start-of-line — and is caught only by the child's own webhook, so a typo
//     in a horizon field would take the healthy Keystone projection down with
//     an error naming neither the field nor the ControlPlane.
//   - A Gateway API wildcard ("*.example.com") is a legal HTTPRoute hostname
//     but yields a trusted origin that matches no dashboard, silently breaking
//     WebSSO forever.
//   - An embedded port is forbidden by Gateway API for the same field, and
//     would be carried into the origin verbatim.
//   - An over-long hostname overruns the children's MaxLength markers on the
//     derived origins (512 on both trustedDashboards[] and websso.keystoneURL),
//     so the API server rejects a child the operator never wrote.
//
// Rejecting them here surfaces the error on the ControlPlane the operator
// actually edits.
func validateGatewayHostname(path *field.Path, hostname string) *field.Error {
	u, err := url.Parse("https://" + hostname)
	switch {
	case err != nil || u.Host != hostname:
		return field.Invalid(path, hostname, "must be a bare DNS hostname")
	case strings.Contains(hostname, "*"):
		return field.Invalid(path, hostname,
			"must not be a wildcard hostname: the derived WebSSO origin is compared verbatim and would match no dashboard")
	case u.Port() != "":
		return field.Invalid(path, hostname,
			"must not include a port: set services.horizon.publicEndpoint to publish the dashboard on a non-default port")
	case len(hostname) > maxGatewayHostnameLen:
		return field.Invalid(path, hostname,
			fmt.Sprintf("must be at most %d characters (the maximum DNS name length)", maxGatewayHostnameLen))
	}
	return nil
}

// catalogEntryTypePattern mirrors the Pattern marker on
// KeystoneServiceCatalogSpec.ServiceType. The type is embedded verbatim in the
// names of the child K-ORC CRs, so it must be a DNS-1123 label.
var catalogEntryTypePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	// maxObjectNameBytes is the apiserver's cap on metadata.name. Nothing bounds
	// the ControlPlane's own name below 253, so a composed child name can overflow
	// a CR that admission already accepted.
	maxObjectNameBytes = 253

	// maxServiceNameBytes is the apiserver's cap on a Service name. Unlike most
	// object names it is a DNS-1035 label rather than a DNS-1123 subdomain, so it
	// is far tighter than maxObjectNameBytes — and a service operator that names
	// its API Service after its CR inherits the tighter bound.
	maxServiceNameBytes = 63

	// identityImportChildNameOverhead is the longest fixed part of an identity
	// Endpoint import name, "{cp}-identity-endpoint-{interface}". External mode
	// creates those imports unconditionally, with or without a catalog block, so
	// the guard hangs off the mode rather than off the catalog block.
	identityImportChildNameOverhead = len("-identity-endpoint") + len("-internal")
)

// validateExternalCatalog mirrors the one declarative constraint on
// ExternalCatalogSpec as defense-in-depth for callers that bypass CRD schema
// admission: the Pattern marker on identityServiceName, which K-ORC casts to its
// OpenStackName on the Service import filter.
func validateExternalCatalog(path *field.Path, catalog *ExternalCatalogSpec) field.ErrorList {
	if !strings.Contains(catalog.IdentityServiceName, ",") {
		return nil
	}
	return field.ErrorList{field.Invalid(path.Child("identityServiceName"), catalog.IdentityServiceName,
		"must not contain a comma (mirrors K-ORC's OpenStackName pattern ^[^,]+$)")}
}

// maxServiceRegistrationNamespaces mirrors the MaxItems marker on
// ServiceRegistrationsSpec.AllowedNamespaces.
const maxServiceRegistrationNamespaces = 32

// validateServiceRegistrations mirrors the declarative constraints on
// ServiceRegistrationsSpec.AllowedNamespaces as defense-in-depth for callers that
// bypass CRD schema admission: the RFC-1123 label shape of each entry, the
// listType=set duplicate rejection, and the MaxItems cap.
//
// It deliberately adds NO rule of its own. In particular an entry naming the
// ControlPlane's own namespace or one of its dedicated service namespaces is a
// redundant no-op rather than an error: both are admitted implicitly, and
// rejecting them would couple the allowlist to unrelated spec edits — dropping a
// service's namespace block would invalidate an allowlist entry that never
// changed, on the very update that removed the block.
func validateServiceRegistrations(cp *ControlPlane) field.ErrorList {
	sr := cp.Spec.KORC.ServiceRegistrations
	if sr == nil || len(sr.AllowedNamespaces) == 0 {
		return nil
	}
	var allErrs field.ErrorList
	basePath := field.NewPath("spec", "korc", "serviceRegistrations", "allowedNamespaces")

	if len(sr.AllowedNamespaces) > maxServiceRegistrationNamespaces {
		allErrs = append(allErrs, field.TooMany(basePath, len(sr.AllowedNamespaces), maxServiceRegistrationNamespaces))
	}

	seen := make(map[string]struct{}, len(sr.AllowedNamespaces))
	for i, ns := range sr.AllowedNamespaces {
		entryPath := basePath.Index(i)
		if !namespaceNamePattern.MatchString(ns) {
			allErrs = append(allErrs, field.Invalid(entryPath, ns,
				"must be a lowercase alphanumeric RFC-1123 label (it names a Kubernetes namespace)"))
		}
		if _, dup := seen[ns]; dup {
			allErrs = append(allErrs, field.Duplicate(entryPath, ns))
		}
		seen[ns] = struct{}{}
	}

	return allErrs
}

// glanceBackendChildNameOverhead is the fixed part of a projected GlanceBackend
// child CR name, "{cp}-glance-{name}", i.e. everything except the ControlPlane
// name and the backend entry name. It mirrors identityImportChildNameOverhead
// one level up: nothing bounds the ControlPlane name below 253, so a backend
// name admitted here must leave room for the composed child name to stay within
// the apiserver's metadata.name cap. Without the guard the reconciler wedges
// projecting a GlanceBackend CR the apiserver rejects, on an Invalid the
// ControlPlane admission already accepted.
const glanceBackendChildNameOverhead = len("-glance-")

// glanceChildNameOverhead is the fixed part of the projected Glance child CR
// name, "{cp}-glance". Unlike its siblings the budget it eats into is not the
// apiserver's 253-byte cap but the far tighter one the Glance CRD's own
// admission applies to metadata.name (glancev1alpha1.MaxGlanceNameLength): the
// Glance operator appends a suffix of its own for the db-purge CronJob, and
// Kubernetes caps CronJob names at 52 characters.
const glanceChildNameOverhead = len("-glance")

// GlanceServiceAccountName is the OpenStack user name of the Keystone account
// Glance authenticates as, carried as spec.account.userName on the
// KeystoneService child projected for Glance. One account per service, named
// after it.
const GlanceServiceAccountName = "glance"

// GlanceServiceProjectName is the Keystone project the KeystoneService child
// projected for Glance creates and owns its service user in. One project per
// service, named after it, so a service's role assignments and its project's
// lifecycle belong to exactly that service.
const GlanceServiceProjectName = "service-glance"

// PlacementServiceAccountName is the OpenStack user name of the Keystone account
// Placement authenticates as, carried as spec.account.userName on the
// KeystoneService child projected for Placement, following the per-service
// convention GlanceServiceAccountName describes.
const PlacementServiceAccountName = "placement"

// PlacementServiceProjectName is the Keystone project the KeystoneService child
// projected for Placement creates and owns its service user in, following the
// per-service convention GlanceServiceProjectName describes.
const PlacementServiceProjectName = "service-placement"

// placementChildNameOverhead is the fixed part of the projected Placement child
// CR name, "{cp}-placement". The budget it eats into is neither the apiserver's
// 253-byte metadata.name cap nor a bound of the Placement CRD (which declines to
// set one), but the API Service the placement operator names after the CR: it
// applies subResourceName(placement) — the bare CR name, untruncated — and a
// Service name is a DNS-1035 label, capped at maxServiceNameBytes.
const placementChildNameOverhead = len("-placement")

// BarbicanServiceAccountName is the OpenStack user name of the Keystone account
// Barbican authenticates as, carried as spec.account.userName on the
// KeystoneService child projected for Barbican, following the per-service
// convention GlanceServiceAccountName describes.
const BarbicanServiceAccountName = "barbican"

// BarbicanServiceProjectName is the Keystone project the KeystoneService child
// projected for Barbican creates and owns its service user in, following the
// per-service convention GlanceServiceProjectName describes.
const BarbicanServiceProjectName = "service-barbican"

// barbicanChildNameOverhead is the fixed part of the projected Barbican child CR
// name, "{cp}-barbican". Like its Glance sibling the budget it eats into is not
// the apiserver's 253-byte cap but the tighter one the Barbican CRD's own
// admission applies to metadata.name (barbicanv1alpha1.MaxBarbicanNameLength):
// the barbican operator appends a suffix of its own for the db-clean CronJob,
// and Kubernetes caps CronJob names at 52 characters.
const barbicanChildNameOverhead = len("-barbican")

// NeutronServiceAccountName is the OpenStack user name of the Keystone account
// Neutron authenticates as, carried as spec.account.userName on the
// KeystoneService child projected for Neutron, following the per-service
// convention GlanceServiceAccountName describes.
const NeutronServiceAccountName = "neutron"

// NeutronServiceProjectName is the Keystone project the KeystoneService child
// projected for Neutron creates and owns its service user in, following the
// per-service convention GlanceServiceProjectName describes.
const NeutronServiceProjectName = "service-neutron"

// neutronChildNameOverhead is the fixed part of the projected Neutron child CR
// name, "{cp}-neutron". Like its Glance and Barbican siblings the budget it eats
// into is not the apiserver's 253-byte cap but the tighter one the Neutron CRD's
// own admission applies to metadata.name (neutronv1alpha1.MaxNeutronNameLength,
// 40): the neutron operator appends "-ovn-db-sync" for the OVN
// database-synchronisation CronJob, and Kubernetes caps CronJob names at 52
// characters.
const neutronChildNameOverhead = len("-neutron")

// validateGlanceChildName enforces that the Glance child this ControlPlane would
// project carries a name the Glance CRD's own validating webhook admits.
// Without it a longer ControlPlane admits cleanly and reconcileGlance then fails
// to apply the child on every pass: GlanceReady never goes True, the
// ControlPlane never reaches Ready, and metadata.name is immutable — so the only
// recovery is deleting and recreating the whole control plane.
//
// It is a create-and-newly-enabled rule rather than part of validateGlance,
// which every update re-runs: the ControlPlane name is immutable, so on a
// routine update the rule could only ever fire against a CR a pre-upgrade
// operator already admitted — including the finalizer-removal update that
// completes its deletion.
func validateGlanceChildName(cp *ControlPlane) field.ErrorList {
	if cp.Spec.Services.Glance == nil {
		return nil
	}
	n := len(cp.Name) + glanceChildNameOverhead
	if n <= glancev1alpha1.MaxGlanceNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), cp.Name, fmt.Sprintf(
		"the projected Glance child CR name would be %d characters; the Glance CRD caps metadata.name at %d "+
			"(its db-purge CronJob appends a suffix, and Kubernetes caps CronJob names at %d characters), so the "+
			"ControlPlane name must be at most %d characters when spec.services.glance is set",
		n, glancev1alpha1.MaxGlanceNameLength, glancev1alpha1.MaxCronJobNameLength,
		glancev1alpha1.MaxGlanceNameLength-glanceChildNameOverhead,
	))}
}

// validatePlacementChildName enforces that the Placement child this ControlPlane
// would project carries a name the placement operator can name its API Service
// after. Without it a longer ControlPlane admits cleanly, the Placement CR is
// created (the CRD sets no bound of its own), and the placement operator then
// fails on every reconcile applying a Service whose metadata.name exceeds the
// DNS-1035 label cap: PlacementReady parks on WaitingForPlacement, the
// ControlPlane never reaches Ready, and metadata.name is immutable — so the only
// recovery is deleting and recreating the whole control plane.
//
// It is a create-and-newly-enabled rule rather than part of validatePlacement,
// which every update re-runs, for the same reason as its Glance sibling: the
// ControlPlane name is immutable, so on a routine update the rule could only ever
// fire against a CR a pre-upgrade operator already admitted — including the
// finalizer-removal update that completes its deletion.
func validatePlacementChildName(cp *ControlPlane) field.ErrorList {
	if cp.Spec.Services.Placement == nil {
		return nil
	}
	n := len(cp.Name) + placementChildNameOverhead
	if n <= maxServiceNameBytes {
		return nil
	}
	return field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), cp.Name, fmt.Sprintf(
		"the projected Placement child CR name would be %d characters; the placement operator names its API "+
			"Service after the CR and Kubernetes caps Service names at %d, so the ControlPlane name must be at "+
			"most %d characters when spec.services.placement is set",
		n, maxServiceNameBytes, maxServiceNameBytes-placementChildNameOverhead,
	))}
}

// validateBarbicanChildName enforces that the Barbican child this ControlPlane
// would project carries a name the Barbican CRD's own validating webhook
// admits. Without it a longer ControlPlane admits cleanly and reconcileBarbican
// then fails to apply the child on every pass: BarbicanReady never goes True,
// the ControlPlane never reaches Ready, and metadata.name is immutable — so the
// only recovery is deleting and recreating the whole control plane.
//
// It is a create-and-newly-enabled rule rather than part of validateBarbican,
// which every update re-runs, for the same reason as its Glance and Placement
// siblings: the ControlPlane name is immutable, so on a routine update the rule
// could only ever fire against a CR a pre-upgrade operator already admitted —
// including the finalizer-removal update that completes its deletion.
//
// This is the only name bound Barbican needs. A dedicated secret store puts an
// OpenBao instance named "{cp}-barbican-bao" next to the service, and the
// openbao-operator derives its per-cluster objects by appending suffixes to that
// name; the longest is the default ServiceAccount
// "{cp}-barbican-bao-serviceaccount", which at the bound this rule enforces is
// 34+13+15 = 62 bytes and therefore still inside the 63-byte label cap.
func validateBarbicanChildName(cp *ControlPlane) field.ErrorList {
	if cp.Spec.Services.Barbican == nil {
		return nil
	}
	n := len(cp.Name) + barbicanChildNameOverhead
	if n <= barbicanv1alpha1.MaxBarbicanNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), cp.Name, fmt.Sprintf(
		"the projected Barbican child CR name would be %d characters; the Barbican CRD caps metadata.name at %d "+
			"(its db-clean CronJob appends a suffix, and Kubernetes caps CronJob names at %d characters), so the "+
			"ControlPlane name must be at most %d characters when spec.services.barbican is set",
		n, barbicanv1alpha1.MaxBarbicanNameLength, barbicanv1alpha1.MaxCronJobNameLength,
		barbicanv1alpha1.MaxBarbicanNameLength-barbicanChildNameOverhead,
	))}
}

// validateNeutronChildName enforces that the Neutron child this ControlPlane
// would project carries a name the Neutron CRD's own validating webhook admits.
// Without it a longer ControlPlane admits cleanly and the Neutron projection
// then fails to apply the child on every pass: NeutronReady never goes True,
// the ControlPlane never reaches Ready, and metadata.name is immutable, so the
// only recovery is deleting and recreating the whole control plane.
//
// It is a create-and-newly-enabled rule rather than part of validateNeutron,
// which every update re-runs, for the same reason as its Glance, Placement and
// Barbican siblings: the ControlPlane name is immutable, so on a routine update
// the rule could only ever fire against a CR a pre-upgrade operator already
// admitted, including the finalizer-removal update that completes its deletion.
func validateNeutronChildName(cp *ControlPlane) field.ErrorList {
	if cp.Spec.Services.Neutron == nil {
		return nil
	}
	n := len(cp.Name) + neutronChildNameOverhead
	if n <= neutronv1alpha1.MaxNeutronNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), cp.Name, fmt.Sprintf(
		"the projected Neutron child CR name would be %d characters; the Neutron CRD caps metadata.name at %d "+
			"(its ovn-db-sync CronJob appends a suffix, and Kubernetes caps CronJob names at %d characters), so the "+
			"ControlPlane name must be at most %d characters when spec.services.neutron is set",
		n, neutronv1alpha1.MaxNeutronNameLength, neutronv1alpha1.MaxCronJobNameLength,
		neutronv1alpha1.MaxNeutronNameLength-neutronChildNameOverhead,
	))}
}

// validateGlance enforces the rules on the services.glance block. It mirrors the
// declarative constraints as defense-in-depth for callers that bypass CRD schema
// admission — the gateway hostname shape, the image tag/digest XOR, the
// per-backend type/s3 union, the S3 endpoint URL shape, the non-empty
// credentialsSecretRef, and the non-empty-backends / exactly-one-default
// invariants — and adds the rules the CRD schema cannot express: the public
// endpoint's origin shape and its agreement with the gateway
// (validateGlancePublicEndpoint) and the composed GlanceBackend child-name
// length bound.
//
// The cross-field rule that services.glance is forbidden in External mode lives
// in validateKeystoneMode with the rest of the External-mode matrix.
func validateGlance(cp *ControlPlane) field.ErrorList {
	gl := cp.Spec.Services.Glance
	if gl == nil {
		return nil
	}
	var allErrs field.ErrorList
	glPath := field.NewPath("spec", "services", "glance")

	// When a gateway is configured, its hostname must be set and usable as the
	// host of the derived public endpoint. Mirrors the MinLength=1 marker on
	// commonv1.GatewaySpec.Hostname.
	if g := gl.Gateway; g != nil {
		hostnamePath := glPath.Child("gateway", "hostname")
		if g.Hostname == "" {
			allErrs = append(allErrs, field.Required(hostnamePath,
				"must be set when a gateway is configured"))
		} else if err := validateGatewayHostname(hostnamePath, g.Hostname); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	// When the Glance image is overridden, mirror the ImageSpec tag/digest XOR
	// (the +kubebuilder:validation:XValidation rule on commonv1.ImageSpec).
	if img := gl.Image; img != nil && (img.Tag != "") == (img.Digest != "") {
		allErrs = append(allErrs, field.Invalid(glPath.Child("image"), img,
			"exactly one of image.tag or image.digest must be set"))
	}

	allErrs = append(allErrs, validateGlancePublicEndpoint(glPath, gl)...)
	// services.glance.importFiltering carries the glance module's own
	// ImportFilteringSpec, so its defense-in-depth checks come from that module's
	// exported validator rather than a mirror maintained here: the bounds, the
	// scheme enum, and the three mutual-exclusivity messages then have exactly one
	// source, and this webhook cannot start admitting values the projected Glance
	// child would reject.
	allErrs = append(allErrs, glancev1alpha1.ValidateImportFiltering(
		glPath.Child("importFiltering"), gl.ImportFiltering,
	)...)
	// services.glance.staging carries the glance module's own StagingSpec for the
	// same single-source reason, and ValidateStaging is the only gate on its size
	// floor — see that validator for why no schema rule can express it.
	allErrs = append(allErrs, glancev1alpha1.ValidateStaging(
		glPath.Child("staging"), gl.Staging,
	)...)
	// services.glance.imageCache is the same arrangement one field over:
	// ValidateImageCache is the sole gate on both the cache's size floor and its
	// maintenance-interval floor, since neither a Quantity nor a Duration carries a
	// Minimum marker in the schema.
	allErrs = append(allErrs, glancev1alpha1.ValidateImageCache(
		glPath.Child("imageCache"), gl.ImageCache,
	)...)
	// services.glance.importPlugins carries the glance module's own
	// ImportPluginsSpec, and ValidateImportPlugins is its single source too: the
	// output-format enum, the property-count bounds, and above all the injected
	// property names, which no CRD marker reaches — a map key has no schema
	// counterpart, so this call is the only gate a name breaking the oslo Dict
	// syntax meets on the ControlPlane.
	allErrs = append(allErrs, glancev1alpha1.ValidateImportPlugins(
		glPath.Child("importPlugins"), gl.ImportPlugins,
	)...)
	// The projection hands both blocks to the child Glance untouched, so the rule
	// tying the decompression plugin to an explicitly chosen staging bound has to
	// hold here too — otherwise the ControlPlane would admit a service block whose
	// projected Glance child is then rejected on every reconcile.
	allErrs = append(allErrs, glancev1alpha1.ValidateImportDecompressionStaging(
		glPath, gl.ImportPlugins, gl.Staging,
	)...)
	allErrs = append(allErrs, validateGlanceBackends(cp, glPath.Child("backends"))...)

	return allErrs
}

// validateGlancePublicEndpoint enforces the rules on
// services.glance.publicEndpoint that the CRD markers cannot express. The value
// is advertised VERBATIM as the K-ORC public image catalog Endpoint — the URL
// every authenticated OpenStack client resolves for `openstack image ...` and
// sends its scoped Keystone token (X-Auth-Token) to. Unlike the keystone
// override it is projected into no child CR, so no downstream webhook re-checks
// it: whatever admission accepts here is what lands in the Keystone catalog.
//
//   - Shape, as defense-in-depth alongside the ^https?:// Pattern marker:
//     "https://" alone matches the pattern and stays under the 512-byte cap, yet
//     registers a hostless URL no client can resolve.
//   - A bare origin, with no path, query or fragment. The Pattern marker anchors
//     only the prefix, so "https://glance.example.com?utm=1" is schema-legal;
//     the Glance API is served at the root and clients append the API path to
//     the catalog URL, yielding "https://glance.example.com?utm=1/v2/images" and
//     a 404 on every image call. A single trailing slash is tolerated —
//     OpenStack clients normalize the catalog endpoint before appending.
//   - With a gateway configured the listener terminates TLS, so the externally
//     observed scheme is https — the same rule the Horizon public endpoint
//     applies. An http endpoint is also a token leak, and a worse one than the
//     dashboard's: the scoped Keystone token rides EVERY image call rather than
//     one WebSSO hand-off per login, and the image payload goes with it.
//   - With a gateway configured the host must equal gateway.hostname. The
//     Gateway listener is what routes that hostname to the Glance Service, so a
//     divergent host advertises an endpoint that never reaches the API — failing
//     client-side, with no status condition and no admission error naming the
//     cause. The port may still differ: Gateway API hostnames carry none, so an
//     API published off 443 has to spell the port out here.
func validateGlancePublicEndpoint(glPath *field.Path, gl *ServiceGlanceSpec) field.ErrorList {
	if gl.PublicEndpoint == "" {
		return nil
	}
	pePath := glPath.Child("publicEndpoint")
	u, err := validateHTTPURL(pePath, gl.PublicEndpoint)
	if err != nil {
		return field.ErrorList{err}
	}

	var errs field.ErrorList
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, field.Invalid(pePath, gl.PublicEndpoint,
			"must be a bare origin (scheme://host[:port]) with no path, query, or fragment: the Glance API is served "+
				"at the root and clients append the API path to the catalog endpoint"))
	}

	g := gl.Gateway
	if g == nil || g.Hostname == "" {
		return errs
	}

	if u.Scheme != "https" {
		errs = append(errs, field.Invalid(pePath, gl.PublicEndpoint,
			"scheme must be https when services.glance.gateway is configured (the Gateway listener terminates TLS): "+
				"every image call sends the caller's scoped Keystone token, and the image payload, to this endpoint"))
	}
	if u.Hostname() != g.Hostname {
		errs = append(errs, field.Invalid(pePath, gl.PublicEndpoint,
			fmt.Sprintf("host %q must equal services.glance.gateway.hostname %q: the Gateway listener routes that "+
				"hostname to the Glance API, so the catalog would direct image clients to a host that never reaches it",
				u.Hostname(), g.Hostname)))
	}
	return errs
}

// warnInsecureGlancePublicEndpoint surfaces a cleartext image endpoint that
// validateGlancePublicEndpoint cannot reject: without a gateway Glance is
// published by some other means, and a plain-http endpoint is a legal, if
// unwise, development setup that the ^https?:// CRD Pattern deliberately allows.
// The downgrade must never be silent, though — every authenticated image call
// carries the caller's scoped Keystone token to this URL, and that bearer token
// grants the caller's full API privileges, not just image access.
func warnInsecureGlancePublicEndpoint(cp *ControlPlane) admission.Warnings {
	gl := cp.Spec.Services.Glance
	if gl == nil || gl.PublicEndpoint == "" {
		return nil
	}
	if u, err := url.Parse(gl.PublicEndpoint); err != nil || u.Scheme != "http" {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.services.glance.publicEndpoint %q uses http://: it is advertised as the public image catalog endpoint, "+
			"so every authenticated image call would deliver the caller's scoped Keystone token — and the image "+
			"payload — in cleartext. Use https://.",
		gl.PublicEndpoint,
	)}
}

// insecurePublicEndpointWarnings collects the cleartext-endpoint admission
// warnings the validating webhook attaches to every create and update. Neither
// shape can be rejected outright (the ^https?:// markers admit http:// on
// purpose), so surfacing them together keeps a gateway-less deployment from
// downgrading a bearer-token path silently.
func insecurePublicEndpointWarnings(cp *ControlPlane) admission.Warnings {
	warnings := warnInsecureHorizonPublicEndpoint(cp)
	warnings = append(warnings, warnInsecureGlancePublicEndpoint(cp)...)
	warnings = append(warnings, warnInsecurePlacementPublicEndpoint(cp)...)
	warnings = append(warnings, warnInsecureBarbicanPublicEndpoint(cp)...)
	return append(warnings, warnInsecureNeutronPublicEndpoint(cp)...)
}

// glanceImportFilteringWarnings surfaces the two admissible-but-misleading
// web-download filter shapes on services.glance.importFiltering: a deny-list
// glance never evaluates, and an allow-list widened past the operator default.
// They come from the glance module's exported helper for the same reason the
// errors come from ValidateImportFiltering — the ControlPlane carries that
// module's own type, and this is the surface most deployments author, so a
// warning raised only on the projected Glance child would never reach anyone.
func glanceImportFilteringWarnings(cp *ControlPlane) admission.Warnings {
	gl := cp.Spec.Services.Glance
	if gl == nil {
		return nil
	}
	return glancev1alpha1.WarnImportFiltering(
		field.NewPath("spec", "services", "glance", "importFiltering"), gl.ImportFiltering,
	)
}

// validateGlanceBackends mirrors the declarative constraints on
// services.glance.backends: the MinItems floor, the single-isDefault CEL rule,
// and the per-entry type/s3 union CEL rule on GlanceBackendEntry, plus the S3
// store's own shape (endpoint URL, non-empty credentialsSecretRef) and the
// composed GlanceBackend child-name length bound the CRD schema cannot express.
func validateGlanceBackends(cp *ControlPlane, backendsPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	backends := cp.Spec.Services.Glance.Backends

	if len(backends) == 0 {
		allErrs = append(allErrs, field.Required(backendsPath,
			"at least one backend must be declared"))
	}

	defaults := 0
	for i := range backends {
		entry := backends[i]
		entryPath := backendsPath.Index(i)
		if entry.IsDefault {
			defaults++
		}

		// The s3 block must be set exactly when type is S3.
		if (entry.Type == "S3") != (entry.S3 != nil) {
			allErrs = append(allErrs, field.Invalid(entryPath, entry.Type,
				"the s3 block must be set exactly when type is S3"))
		}
		if s3 := entry.S3; s3 != nil {
			s3Path := entryPath.Child("s3")
			if _, err := validateHTTPURL(s3Path.Child("endpoint"), s3.Endpoint); err != nil {
				allErrs = append(allErrs, err)
			}
			if s3.CredentialsSecretRef.Name == "" {
				allErrs = append(allErrs, field.Required(s3Path.Child("credentialsSecretRef", "name"),
					"must be set"))
			}
		}

		if entry.Name != "" {
			if n := len(cp.Name) + glanceBackendChildNameOverhead + len(entry.Name); n > maxObjectNameBytes {
				allErrs = append(allErrs, field.Invalid(entryPath.Child("name"), entry.Name, fmt.Sprintf(
					"the child GlanceBackend CR name would be %d bytes; shorten the ControlPlane name or the "+
						"backend name so the total stays within the %d-byte Kubernetes object-name limit",
					n, maxObjectNameBytes,
				)))
			}
		}
	}
	if len(backends) > 0 && defaults != 1 {
		allErrs = append(allErrs, field.Invalid(backendsPath, defaults,
			"exactly one backends entry must set isDefault"))
	}

	return allErrs
}

// validatePlacement enforces the rules on the services.placement block. It
// mirrors the declarative constraints as defense-in-depth for callers that
// bypass CRD schema admission (the gateway hostname shape and the image
// tag/digest XOR) and adds the rule the CRD schema cannot express: the public
// endpoint's origin shape and its agreement with the gateway
// (validatePlacementPublicEndpoint).
//
// The projected-child-name bound lives in validatePlacementChildName, which runs
// only on create and on the update that newly enables Placement.
//
// The cross-field rule that services.placement is forbidden in External mode
// lives in validateKeystoneMode with the rest of the External-mode matrix; the
// extraConfig rules live in the two extraConfig admission families, which walk
// every declared service block at once.
func validatePlacement(cp *ControlPlane) field.ErrorList {
	pl := cp.Spec.Services.Placement
	if pl == nil {
		return nil
	}
	var allErrs field.ErrorList
	plPath := field.NewPath("spec", "services", "placement")

	// When a gateway is configured, its hostname must be set and usable as the
	// host of the derived public endpoint. Mirrors the MinLength=1 marker on
	// commonv1.GatewaySpec.Hostname.
	if g := pl.Gateway; g != nil {
		hostnamePath := plPath.Child("gateway", "hostname")
		if g.Hostname == "" {
			allErrs = append(allErrs, field.Required(hostnamePath,
				"must be set when a gateway is configured"))
		} else if err := validateGatewayHostname(hostnamePath, g.Hostname); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	// When the Placement image is overridden, mirror the ImageSpec tag/digest XOR
	// (the +kubebuilder:validation:XValidation rule on commonv1.ImageSpec).
	if img := pl.Image; img != nil && (img.Tag != "") == (img.Digest != "") {
		allErrs = append(allErrs, field.Invalid(plPath.Child("image"), img,
			"exactly one of image.tag or image.digest must be set"))
	}

	allErrs = append(allErrs, validatePlacementPublicEndpoint(plPath, pl)...)

	return allErrs
}

// validatePlacementPublicEndpoint enforces the rules on
// services.placement.publicEndpoint that the CRD markers cannot express. The
// value is advertised VERBATIM as the K-ORC public placement catalog Endpoint:
// the URL every compute service resolves to read and write its allocations, and
// sends its scoped Keystone token (X-Auth-Token) to. Unlike the keystone
// override it is projected into no child CR, so no downstream webhook re-checks
// it: whatever admission accepts here is what lands in the Keystone catalog.
//
//   - Shape, as defense-in-depth alongside the ^https?:// Pattern marker:
//     "https://" alone matches the pattern and stays under the 512-byte cap, yet
//     registers a hostless URL no client can resolve.
//   - A bare origin, with no path, query or fragment. The Pattern marker anchors
//     only the prefix, so "https://placement.example.com?utm=1" is schema-legal;
//     the Placement API is served at the root and clients append the API path to
//     the catalog URL, yielding "https://placement.example.com?utm=1/resource_providers"
//     and a 404 on every allocation call. A single trailing slash is tolerated:
//     OpenStack clients normalize the catalog endpoint before appending.
//   - With a gateway configured the listener terminates TLS, so the externally
//     observed scheme is https, the same rule the Glance public endpoint applies.
//     An http endpoint is also a token leak: the scoped Keystone token rides
//     every allocation call, and a compute service places allocations on every
//     instance boot.
//   - With a gateway configured the host must equal gateway.hostname. The
//     Gateway listener is what routes that hostname to the Placement Service, so
//     a divergent host advertises an endpoint that never reaches the API,
//     failing client-side with no status condition and no admission error naming
//     the cause. The port may still differ: Gateway API hostnames carry none, so
//     an API published off 443 has to spell the port out here.
func validatePlacementPublicEndpoint(plPath *field.Path, pl *ServicePlacementSpec) field.ErrorList {
	if pl.PublicEndpoint == "" {
		return nil
	}
	pePath := plPath.Child("publicEndpoint")
	u, err := validateHTTPURL(pePath, pl.PublicEndpoint)
	if err != nil {
		return field.ErrorList{err}
	}

	var errs field.ErrorList
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, field.Invalid(pePath, pl.PublicEndpoint,
			"must be a bare origin (scheme://host[:port]) with no path, query, or fragment: the Placement API is "+
				"served at the root and clients append the API path to the catalog endpoint"))
	}

	g := pl.Gateway
	if g == nil || g.Hostname == "" {
		return errs
	}

	if u.Scheme != "https" {
		errs = append(errs, field.Invalid(pePath, pl.PublicEndpoint,
			"scheme must be https when services.placement.gateway is configured (the Gateway listener terminates "+
				"TLS): every allocation call sends the caller's scoped Keystone token to this endpoint"))
	}
	if u.Hostname() != g.Hostname {
		errs = append(errs, field.Invalid(pePath, pl.PublicEndpoint,
			fmt.Sprintf("host %q must equal services.placement.gateway.hostname %q: the Gateway listener routes that "+
				"hostname to the Placement API, so the catalog would direct compute services to a host that never "+
				"reaches it", u.Hostname(), g.Hostname)))
	}
	return errs
}

// warnInsecurePlacementPublicEndpoint surfaces a cleartext placement endpoint
// that validatePlacementPublicEndpoint cannot reject: without a gateway
// Placement is published by some other means, and a plain-http endpoint is a
// legal, if unwise, development setup that the ^https?:// CRD Pattern
// deliberately allows. The downgrade must never be silent, though: every
// allocation call carries the caller's scoped Keystone token to this URL, and
// that bearer token grants the caller's full API privileges, not just placement
// access.
func warnInsecurePlacementPublicEndpoint(cp *ControlPlane) admission.Warnings {
	pl := cp.Spec.Services.Placement
	if pl == nil || pl.PublicEndpoint == "" {
		return nil
	}
	if u, err := url.Parse(pl.PublicEndpoint); err != nil || u.Scheme != "http" {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.services.placement.publicEndpoint %q uses http://: it is advertised as the public placement catalog "+
			"endpoint, so every allocation call would deliver the caller's scoped Keystone token in cleartext. "+
			"Use https://.",
		pl.PublicEndpoint,
	)}
}

// validateBarbican enforces the rules on the services.barbican block. It mirrors
// the declarative constraints as defense-in-depth for callers that bypass CRD
// schema admission (the gateway hostname shape, the image tag/digest XOR, the
// secret-store dedicated/external union and the external store's URL and
// credentials reference) and adds the rules the CRD schema cannot express: the
// public endpoint's origin shape and its agreement with the gateway
// (validateBarbicanPublicEndpoint).
//
// The projected-child-name bound lives in validateBarbicanChildName, which runs
// only on create and on the update that newly enables Barbican.
//
// The cross-field rule that services.barbican is forbidden in External mode
// lives in validateKeystoneMode with the rest of the External-mode matrix; the
// extraConfig rules live in the two extraConfig admission families, which walk
// every declared service block at once.
func validateBarbican(cp *ControlPlane) field.ErrorList {
	bn := cp.Spec.Services.Barbican
	if bn == nil {
		return nil
	}
	var allErrs field.ErrorList
	bnPath := field.NewPath("spec", "services", "barbican")

	// When a gateway is configured, its hostname must be set and usable as the
	// host of the derived public endpoint. Mirrors the MinLength=1 marker on
	// commonv1.GatewaySpec.Hostname.
	if g := bn.Gateway; g != nil {
		hostnamePath := bnPath.Child("gateway", "hostname")
		if g.Hostname == "" {
			allErrs = append(allErrs, field.Required(hostnamePath,
				"must be set when a gateway is configured"))
		} else if err := validateGatewayHostname(hostnamePath, g.Hostname); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	// When the Barbican image is overridden, mirror the ImageSpec tag/digest XOR
	// (the +kubebuilder:validation:XValidation rule on commonv1.ImageSpec).
	if img := bn.Image; img != nil && (img.Tag != "") == (img.Digest != "") {
		allErrs = append(allErrs, field.Invalid(bnPath.Child("image"), img,
			"exactly one of image.tag or image.digest must be set"))
	}

	allErrs = append(allErrs, validateBarbicanSecretStore(bnPath.Child("secretStore"), &bn.SecretStore)...)
	allErrs = append(allErrs, validateBarbicanPublicEndpoint(bnPath, bn)...)

	return allErrs
}

// validateBarbicanSecretStore mirrors the declarative constraints on
// services.barbican.secretStore: the dedicated/external union carried by the
// type-level CEL rule, and the external store's own MinLength/Pattern markers.
// The mirror is load-bearing rather than decorative — an API server that skips
// the x-kubernetes-validations rule admits a store naming neither mode, and the
// projection would then have no server to point barbican at.
func validateBarbicanSecretStore(storePath *field.Path, store *ServiceBarbicanSecretStoreSpec) field.ErrorList {
	var allErrs field.ErrorList

	if (store.Dedicated != nil) == (store.External != nil) {
		allErrs = append(allErrs, field.Invalid(storePath, store,
			"exactly one of dedicated or external must be set"))
	}

	ext := store.External
	if ext == nil {
		return allErrs
	}
	extPath := storePath.Child("external")

	// TLS is mandatory: the AppRole login and every stored secret travel this
	// URL. Mirrors the ^https:// Pattern marker.
	urlPath := extPath.Child("url")
	if u, err := validateHTTPURL(urlPath, ext.URL); err != nil {
		allErrs = append(allErrs, err)
	} else if u.Scheme != "https" {
		allErrs = append(allErrs, field.Invalid(urlPath, ext.URL,
			"scheme must be https: the AppRole credentials and every secret barbican stores travel this URL"))
	}

	if ext.CredentialsSecretRef.Name == "" {
		allErrs = append(allErrs, field.Required(extPath.Child("credentialsSecretRef", "name"),
			"must be set: the external store authenticates with the AppRole credentials in that Secret"))
	}
	if ref := ext.CABundleSecretRef; ref != nil && ref.Name == "" {
		allErrs = append(allErrs, field.Required(extPath.Child("caBundleSecretRef", "name"),
			"must be set when caBundleSecretRef is configured"))
	}

	return allErrs
}

// validateBarbicanPublicEndpoint enforces the rules on
// services.barbican.publicEndpoint that the CRD markers cannot express. The
// value is advertised VERBATIM as the K-ORC public key-manager catalog Endpoint:
// the URL every client resolves to store and read secret material, and sends its
// scoped Keystone token (X-Auth-Token) to. Unlike the keystone override it is
// projected into no child CR, so no downstream webhook re-checks it: whatever
// admission accepts here is what lands in the Keystone catalog.
//
//   - Shape, as defense-in-depth alongside the ^https?:// Pattern marker:
//     "https://" alone matches the pattern and stays under the 512-byte cap, yet
//     registers a hostless URL no client can resolve.
//   - A bare origin, with no path, query or fragment. The Pattern marker anchors
//     only the prefix, so "https://barbican.example.com?utm=1" is schema-legal;
//     the Barbican API is served at the root and clients append the API path to
//     the catalog URL, yielding "https://barbican.example.com?utm=1/v1/secrets"
//     and a 404 on every secret call. A single trailing slash is tolerated:
//     OpenStack clients normalize the catalog endpoint before appending.
//   - With a gateway configured the listener terminates TLS, so the externally
//     observed scheme is https, the same rule the Glance public endpoint applies.
//     An http endpoint is also a token leak, and what it leaks alongside the
//     scoped Keystone token is the secret material the service exists to protect.
//   - With a gateway configured the host must equal gateway.hostname. The
//     Gateway listener is what routes that hostname to the Barbican Service, so
//     a divergent host advertises an endpoint that never reaches the API,
//     failing client-side with no status condition and no admission error naming
//     the cause. The port may still differ: Gateway API hostnames carry none, so
//     an API published off 443 has to spell the port out here.
func validateBarbicanPublicEndpoint(bnPath *field.Path, bn *ServiceBarbicanSpec) field.ErrorList {
	if bn.PublicEndpoint == "" {
		return nil
	}
	pePath := bnPath.Child("publicEndpoint")
	u, err := validateHTTPURL(pePath, bn.PublicEndpoint)
	if err != nil {
		return field.ErrorList{err}
	}

	var errs field.ErrorList
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, field.Invalid(pePath, bn.PublicEndpoint,
			"must be a bare origin (scheme://host[:port]) with no path, query, or fragment: the Barbican API is "+
				"served at the root and clients append the API path to the catalog endpoint"))
	}

	g := bn.Gateway
	if g == nil || g.Hostname == "" {
		return errs
	}

	if u.Scheme != "https" {
		errs = append(errs, field.Invalid(pePath, bn.PublicEndpoint,
			"scheme must be https when services.barbican.gateway is configured (the Gateway listener terminates "+
				"TLS): every call sends the caller's scoped Keystone token, and the secret payload, to this endpoint"))
	}
	if u.Hostname() != g.Hostname {
		errs = append(errs, field.Invalid(pePath, bn.PublicEndpoint,
			fmt.Sprintf("host %q must equal services.barbican.gateway.hostname %q: the Gateway listener routes that "+
				"hostname to the Barbican API, so the catalog would direct clients to a host that never reaches it",
				u.Hostname(), g.Hostname)))
	}
	return errs
}

// warnInsecureBarbicanPublicEndpoint surfaces a cleartext key-manager endpoint
// that validateBarbicanPublicEndpoint cannot reject: without a gateway Barbican
// is published by some other means, and a plain-http endpoint is a legal, if
// unwise, development setup that the ^https?:// CRD Pattern deliberately allows.
// The downgrade must never be silent, though: every call carries the caller's
// scoped Keystone token to this URL, and the stored secret material travels the
// same connection.
func warnInsecureBarbicanPublicEndpoint(cp *ControlPlane) admission.Warnings {
	bn := cp.Spec.Services.Barbican
	if bn == nil || bn.PublicEndpoint == "" {
		return nil
	}
	if u, err := url.Parse(bn.PublicEndpoint); err != nil || u.Scheme != "http" {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.services.barbican.publicEndpoint %q uses http://: it is advertised as the public key-manager catalog "+
			"endpoint, so every call would deliver the caller's scoped Keystone token — and the secret material "+
			"itself — in cleartext. Use https://.",
		bn.PublicEndpoint,
	)}
}

// warnDevelopmentBarbicanSecretStore surfaces what services.barbican.secretStore.dedicated
// actually provisions. The block is fieldless, so nothing in the CR, in the CRD
// schema, or in any validation error tells an operator that the store they just
// asked for is a proving-grade one — and Barbican is the service the rest of the
// cloud puts its key material in.
//
// Two properties are worth the warning. The instance runs the openbao-operator's
// Development profile at a single replica with no PodDisruptionBudget, so a node
// drain or a failed image pull takes every secret read and write with it. And its
// seal is static: the key lives in a plain Secret, "<instance>-unseal-key", in the
// SAME namespace as the raft volume it seals, so `get secrets` there — or one etcd
// snapshot, or one namespace backup — yields both halves at once. Neither is a
// reason to reject the choice: it is the only self-service store mode, and a kind
// or proving cluster has no KMS to unseal against. It is a reason not to let it
// land silently.
func warnDevelopmentBarbicanSecretStore(cp *ControlPlane) admission.Warnings {
	bn := cp.Spec.Services.Barbican
	if bn == nil || bn.SecretStore.Dedicated == nil {
		return nil
	}
	return admission.Warnings{
		"spec.services.barbican.secretStore.dedicated provisions a single-replica OpenBao instance on the " +
			"openbao-operator's Development profile, sealed by a static key stored in a Secret in the same " +
			"namespace as the volume it seals. It has no HA, and the seal protects nothing against a read of " +
			"that namespace's Secrets or against an etcd or namespace backup. For a production key manager use " +
			"spec.services.barbican.secretStore.external against a hardened server with a KMS unseal.",
	}
}

// validateNeutron enforces the rules on the services.neutron block. It mirrors
// the declarative constraints as defense-in-depth for callers that bypass CRD
// schema admission (the gateway hostname shape, the image tag/digest XOR, and
// the name of the OVNCentral reference) and adds the rules the CRD schema cannot
// express: the public endpoint's origin shape and its agreement with the gateway
// (validateNeutronPublicEndpoint) and the shared message bus the projected child
// cannot come up without.
//
// The projected-child-name bound lives in validateNeutronChildName and the
// OVNCentral reach check in ValidateNeutronOVNCentralNamespace, neither of which
// runs on every update.
//
// The cross-field rule that services.neutron is forbidden in External mode lives
// in validateKeystoneMode with the rest of the External-mode matrix; the
// extraConfig rules live in the two extraConfig admission families, which walk
// every declared service block at once.
func validateNeutron(cp *ControlPlane) field.ErrorList {
	nn := cp.Spec.Services.Neutron
	if nn == nil {
		return nil
	}
	var allErrs field.ErrorList
	nnPath := field.NewPath("spec", "services", "neutron")

	// When a gateway is configured, its hostname must be set and usable as the
	// host of the derived public endpoint. Mirrors the MinLength=1 marker on
	// commonv1.GatewaySpec.Hostname.
	if g := nn.Gateway; g != nil {
		hostnamePath := nnPath.Child("gateway", "hostname")
		if g.Hostname == "" {
			allErrs = append(allErrs, field.Required(hostnamePath,
				"must be set when a gateway is configured"))
		} else if err := validateGatewayHostname(hostnamePath, g.Hostname); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	// When the Neutron image is overridden, mirror the ImageSpec tag/digest XOR
	// (the +kubebuilder:validation:XValidation rule on commonv1.ImageSpec).
	if img := nn.Image; img != nil && (img.Tag != "") == (img.Digest != "") {
		allErrs = append(allErrs, field.Invalid(nnPath.Child("image"), img,
			"exactly one of image.tag or image.digest must be set"))
	}

	allErrs = append(allErrs, validateNeutronPublicEndpoint(nnPath, nn)...)

	// The OVNCentral reference, mirroring the MinLength marker on its name and the
	// RFC-1123 Pattern marker on its namespace as defense-in-depth. The ML2/OVN
	// mechanism driver writes every network, subnet and port into that central's
	// Northbound database, so a reference naming nothing leaves the child with no
	// database to program.
	if nn.OVN.CentralRef.Name == "" {
		allErrs = append(allErrs, field.Required(nnPath.Child("ovn", "centralRef", "name"),
			"must be set: it names the OVNCentral the projected Neutron programs"))
	}

	// The bus is not optional for this service. The Neutron CRD requires
	// spec.messaging, and the ControlPlane derives the child's transport URL from
	// spec.infrastructure.messaging, so a ControlPlane declaring the network
	// service without one would project a child its own admission rejects on every
	// pass. A nil infrastructure block is reported by validateKeystoneMode already
	// (it is required outside External mode, and External mode forbids
	// services.neutron outright), so this arm stays silent there rather than
	// naming the same missing block twice.
	if cp.Spec.Infrastructure != nil && cp.Spec.Infrastructure.Messaging == nil {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "infrastructure", "messaging"),
			"is required when services.neutron is set: the Neutron CRD requires spec.messaging, and the "+
				"ControlPlane derives the child's transport URL from the shared bus"))
	}

	return allErrs
}

// claimedServiceNamespace returns the namespace assignment cp claims under the
// given name, or nil when this ControlPlane claims no namespace of that name.
// The ControlPlane's own namespace is not a claim: DedicatedServiceNamespaces
// enumerates the namespaces OUTSIDE it.
func claimedServiceNamespace(cp *ControlPlane, name string) *ServiceNamespaceSpec {
	claims := cp.DedicatedServiceNamespaces()
	for i := range claims {
		if claims[i].Name == name {
			return &claims[i]
		}
	}
	return nil
}

// ValidateNeutronOVNCentralNamespace enforces which namespaces
// services.neutron.ovn.centralRef may reach. It is the ONE pointer on the
// ControlPlane that addresses material in another namespace — the infrastructure
// clusterRefs and every secretRef are namespace-less and resolve in the
// ControlPlane's own — so this field alone decides whose OVN control plane a
// plane can attach itself to.
//
//   - The RFC-1123 label shape, as defense-in-depth alongside the Pattern marker.
//   - The namespace must be one this ControlPlane already reaches: its own, or
//     one it claims through a services.<service>.namespace assignment. Naming a
//     foreign central is not a read-only act. The neutron-operator mirrors that
//     central's client Secret out of its namespace into the Neutron's, which
//     hands this plane a full mTLS identity for another plane's Northbound and
//     Southbound databases — its networks, ports and security groups, readable
//     and writable — and OVNReady relays the central's database addresses and
//     status message on the way. It is the same isolation validateNamespaceClaims
//     enforces for service namespaces, one field over.
//   - A claimed namespace whose lifecycle is Managed is refused from the other
//     direction: the teardown deletes such a namespace together with the plane,
//     and the cascade would take the referenced central, and the logical network
//     model its databases hold, with it. An External claim is the one this
//     ControlPlane never deletes.
//
// It runs on CREATE and on the two UPDATEs that can newly violate it — the one
// that enables the network service and the one that moves the ref — and NOT on
// every update, for the reason validateNeutronChildName gives one rule over: an
// unconditional rule can only ever reject a CR a previous operator build already
// admitted, and one of those rejections lands on the finalizer-removal update
// that completes a deletion, wedging the ControlPlane in Terminating with no
// recovery but stripping the finalizer by hand.
//
// Admission is not the only enforcement point. A CR can reach etcd without ever
// passing through here — an unregistered webhook during install, a GitOps or
// etcd restore replaying stored objects — so reconcileOVN re-runs this same
// check before it reads the central, the backstop keystoneServiceNamespaceAllowed
// is for the sibling cross-namespace trust decision.
//
// It is exported for that controller-side caller. Its path is fixed rather than
// passed in, so both callers report the same field.
func ValidateNeutronOVNCentralNamespace(cp *ControlPlane) field.ErrorList {
	if cp.Spec.Services.Neutron == nil {
		return nil
	}
	// An empty namespace is the ControlPlane's own: the defaulting webhook fills
	// it in, and NeutronOVNCentralNamespace resolves an unset value the same way.
	ns := cp.Spec.Services.Neutron.OVN.CentralRef.Namespace
	if ns == "" || ns == cp.Namespace {
		return nil
	}
	nsPath := field.NewPath("spec", "services", "neutron", "ovn", "centralRef", "namespace")
	if !namespaceNamePattern.MatchString(ns) {
		return field.ErrorList{field.Invalid(nsPath, ns,
			"must be a lowercase alphanumeric RFC-1123 label (it names a Kubernetes namespace)")}
	}
	switch claim := claimedServiceNamespace(cp, ns); {
	case claim == nil:
		return field.ErrorList{field.Forbidden(nsPath, fmt.Sprintf(
			"namespace %q is neither this ControlPlane's own nor one it claims through a "+
				"services.<service>.namespace assignment: consuming an OVNCentral from a foreign namespace "+
				"mirrors that central's client certificate into this ControlPlane's, which is a full mTLS "+
				"identity for its Northbound and Southbound databases", ns))}
	case claim.Lifecycle != ServiceNamespaceLifecycleExternal:
		return field.ErrorList{field.Forbidden(nsPath, fmt.Sprintf(
			"namespace %q is claimed by this ControlPlane with lifecycle Managed, so the teardown deletes it "+
				"together with the plane and the cascade would take the referenced OVNCentral, and the logical "+
				"network model in its databases, with it: give that assignment lifecycle External, or put the "+
				"central in the ControlPlane's own namespace", ns))}
	}
	return nil
}

// validateNeutronPublicEndpoint enforces the rules on
// services.neutron.publicEndpoint that the CRD markers cannot express. The value
// is advertised VERBATIM as the K-ORC public network catalog Endpoint: the URL
// every client resolves to create its networks, subnets and ports, and sends its
// scoped Keystone token (X-Auth-Token) to. Unlike the keystone override it is
// projected into no child CR, so no downstream webhook re-checks it: whatever
// admission accepts here is what lands in the Keystone catalog.
//
//   - Shape, as defense-in-depth alongside the ^https?:// Pattern marker:
//     "https://" alone matches the pattern and stays under the 512-byte cap, yet
//     registers a hostless URL no client can resolve.
//   - A bare origin, with no path, query or fragment. The Pattern marker anchors
//     only the prefix, so "https://neutron.example.com?utm=1" is schema-legal;
//     the Neutron API is served at the root and clients append the API path to
//     the catalog URL, yielding "https://neutron.example.com?utm=1/v2.0/networks"
//     and a 404 on every network call. A single trailing slash is tolerated:
//     OpenStack clients normalize the catalog endpoint before appending.
//   - With a gateway configured the listener terminates TLS, so the externally
//     observed scheme is https, the same rule the Glance public endpoint applies.
//     An http endpoint is also a token leak: the scoped Keystone token rides
//     every network call, and a compute service creates a port for every instance
//     it boots.
//   - With a gateway configured the host must equal gateway.hostname. The
//     Gateway listener is what routes that hostname to the Neutron Service, so a
//     divergent host advertises an endpoint that never reaches the API, failing
//     client-side with no status condition and no admission error naming the
//     cause. The port may still differ: Gateway API hostnames carry none, so an
//     API published off 443 has to spell the port out here.
func validateNeutronPublicEndpoint(nnPath *field.Path, nn *ServiceNeutronSpec) field.ErrorList {
	if nn.PublicEndpoint == "" {
		return nil
	}
	pePath := nnPath.Child("publicEndpoint")
	u, err := validateHTTPURL(pePath, nn.PublicEndpoint)
	if err != nil {
		return field.ErrorList{err}
	}

	var errs field.ErrorList
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, field.Invalid(pePath, nn.PublicEndpoint,
			"must be a bare origin (scheme://host[:port]) with no path, query, or fragment: the Neutron API is "+
				"served at the root and clients append the API path to the catalog endpoint"))
	}

	g := nn.Gateway
	if g == nil || g.Hostname == "" {
		return errs
	}

	if u.Scheme != "https" {
		errs = append(errs, field.Invalid(pePath, nn.PublicEndpoint,
			"scheme must be https when services.neutron.gateway is configured (the Gateway listener terminates "+
				"TLS): every network call sends the caller's scoped Keystone token to this endpoint"))
	}
	if u.Hostname() != g.Hostname {
		errs = append(errs, field.Invalid(pePath, nn.PublicEndpoint,
			fmt.Sprintf("host %q must equal services.neutron.gateway.hostname %q: the Gateway listener routes that "+
				"hostname to the Neutron API, so the catalog would direct clients to a host that never reaches it",
				u.Hostname(), g.Hostname)))
	}
	return errs
}

// warnInsecureNeutronPublicEndpoint surfaces a cleartext network endpoint that
// validateNeutronPublicEndpoint cannot reject: without a gateway Neutron is
// published by some other means, and a plain-http endpoint is a legal, if
// unwise, development setup that the ^https?:// CRD Pattern deliberately allows.
// The downgrade must never be silent, though: every network call carries the
// caller's scoped Keystone token to this URL, and that bearer token grants the
// caller's full API privileges, not just network access.
func warnInsecureNeutronPublicEndpoint(cp *ControlPlane) admission.Warnings {
	nn := cp.Spec.Services.Neutron
	if nn == nil || nn.PublicEndpoint == "" {
		return nil
	}
	if u, err := url.Parse(nn.PublicEndpoint); err != nil || u.Scheme != "http" {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.services.neutron.publicEndpoint %q uses http://: it is advertised as the public network catalog "+
			"endpoint, so every network call would deliver the caller's scoped Keystone token in cleartext. "+
			"Use https://.",
		nn.PublicEndpoint,
	)}
}

// externalAuthURLIsPlaintext reports whether raw is an http:// (non-TLS) endpoint.
// A parse failure reads as false: validateHTTPURL already rejects those on the same
// field, and a second error on it would only add noise.
func externalAuthURLIsPlaintext(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http"
}

// ControlPlaneWebhook implements defaulting and validation webhooks for the
// ControlPlane CRD. Client is injected at startup and used by
// ValidateCreate to enforce one ControlPlane per namespace.
// Production wiring injects mgr.GetAPIReader() — a direct, uncached reader —
// so concurrent or cache-sync-window CREATEs cannot both pass the check
// against an empty informer cache.
// +kubebuilder:object:generate=false
type ControlPlaneWebhook struct {
	commonwebhook.NoopDeleteValidator[*ControlPlane]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*ControlPlane] = &ControlPlaneWebhook{}
	_ admission.Validator[*ControlPlane] = &ControlPlaneWebhook{}
)

// +kubebuilder:webhook:path=/mutate-c5c3-io-v1alpha1-controlplane,mutating=true,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=controlplanes,verbs=create;update,versions=v1alpha1,name=mcontrolplane.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-c5c3-io-v1alpha1-controlplane,mutating=false,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=controlplanes,verbs=create;update,versions=v1alpha1,name=vcontrolplane.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *ControlPlaneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*ControlPlane](mgr, &ControlPlane{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// defaultDatabaseLeaves materializes the well-known leaves of a DatabaseSpec —
// the logical database name, the credential Secret name, and, in MANAGED mode
// only, the clusterRef naming the MariaDB CR. clusterRefName is the managed CR
// name to invent when the block names none: the shared, well-known
// DefaultDatabaseClusterRefName for spec.infrastructure, and a ControlPlane-
// derived name for a per-service dedicated instance.
//
// The managed clusterRef is invented only when the brownfield discriminator
// (host) is unset, so an explicit brownfield endpoint is never coerced into
// managed mode and the database XOR check still passes. Idempotent: only zero
// values are filled.
func defaultDatabaseLeaves(db *commonv1.DatabaseSpec, clusterRefName string) {
	if db.Database == "" {
		db.Database = DefaultDatabaseName
	}
	if db.SecretRef.Name == "" {
		db.SecretRef.Name = DefaultDatabaseSecretName
	}
	if db.Host == "" {
		if db.ClusterRef == nil {
			db.ClusterRef = &corev1.LocalObjectReference{Name: clusterRefName}
		} else if db.ClusterRef.Name == "" {
			db.ClusterRef.Name = clusterRefName
		}
	}
}

// defaultCacheLeaves materializes the well-known leaves of a CacheSpec — the
// oslo.cache backend and, in MANAGED mode only, the clusterRef naming the
// Memcached CR — with the same brownfield-preserving discipline as
// defaultDatabaseLeaves (see there).
func defaultCacheLeaves(cache *commonv1.CacheSpec, clusterRefName string) {
	if cache.Backend == "" {
		cache.Backend = DefaultCacheBackend
	}
	if len(cache.Servers) == 0 {
		if cache.ClusterRef == nil {
			cache.ClusterRef = &corev1.LocalObjectReference{Name: clusterRefName}
		} else if cache.ClusterRef.Name == "" {
			cache.ClusterRef.Name = clusterRefName
		}
	}
}

// defaultMessagingLeaves materializes the well-known leaves of a MessagingSpec
// with the same brownfield-preserving discipline as defaultDatabaseLeaves: the
// managed clusterRef is invented only when the brownfield discriminator
// (secretRef) is unset. A brownfield secretRef gets the shared transport-URL
// key, and a tls block gets the CA-bundle key. Idempotent: only zero values
// are filled. The block itself is never created: messaging is opt-in.
func defaultMessagingLeaves(m *commonv1.MessagingSpec, clusterRefName string) {
	if m.SecretRef == nil {
		if m.ClusterRef == nil {
			m.ClusterRef = &corev1.LocalObjectReference{Name: clusterRefName}
		} else if m.ClusterRef.Name == "" {
			m.ClusterRef.Name = clusterRefName
		}
	} else if m.SecretRef.Key == "" {
		m.SecretRef.Key = commonv1.DefaultTransportURLSecretKey
	}
	if m.TLS != nil && m.TLS.CABundleSecretRef.Key == "" {
		m.TLS.CABundleSecretRef.Key = DefaultCABundleSecretKey
	}
}

// Default implements admission.Defaulter[*ControlPlane].
// It fills only zero-valued fields with their documented defaults, leaving any
// explicit value untouched. It is idempotent: applying it twice produces the
// same result.
func (w *ControlPlaneWebhook) Default(_ context.Context, obj *ControlPlane) error {
	// Plan decision #4: region defaults to RegionOne.
	if obj.Spec.Region == "" {
		obj.Spec.Region = DefaultRegion
	}

	// Default the keystone mode to Managed when the service block is present with
	// an empty mode, so IsExternalKeystone() reads a definite discriminator below.
	// Mirrors the +kubebuilder:default=Managed marker on ServiceKeystoneSpec.Mode.
	if ks := obj.Spec.Services.Keystone; ks != nil && ks.Mode == "" {
		ks.Mode = KeystoneModeManaged
	}

	// A per-service namespace assignment defaults to the Managed lifecycle — the
	// operator creates, owns, and deletes the namespace. Defaulted only on a
	// DECLARED block: an absent assignment means "stay in the ControlPlane's
	// namespace", and the webhook must never invent a placement. Mirrors the
	// +kubebuilder:default=Managed marker on ServiceNamespaceSpec.Lifecycle.
	for _, ns := range []*ServiceNamespaceSpec{
		keystoneNamespaceBlock(obj), horizonNamespaceBlock(obj), glanceNamespaceBlock(obj),
		placementNamespaceBlock(obj), barbicanNamespaceBlock(obj), neutronNamespaceBlock(obj),
	} {
		if ns != nil && ns.Lifecycle == "" {
			ns.Lifecycle = ServiceNamespaceLifecycleManaged
		}
	}

	// An external Barbican secret store defaults to the mount the delivered
	// contract provisions, mirroring the +kubebuilder:default=barbican marker for
	// callers that bypass CRD schema admission. Like the lifecycle above it is
	// only applied to a DECLARED external block: the dedicated mode names no
	// mount, so there is nothing to fill in there.
	if bn := obj.Spec.Services.Barbican; bn != nil {
		if ext := bn.SecretStore.External; ext != nil && ext.KVMountpoint == "" {
			ext.KVMountpoint = DefaultBarbicanKVMountpoint
		}
	}

	if obj.IsExternalKeystone() {
		// External mode: the ControlPlane manages identity against a pre-existing
		// Keystone and provisions NO backing services, so the infrastructure
		// defaulting below is deliberately skipped — the webhook never invents a
		// managed database/cache clusterRef (spec.infrastructure stays nil and the
		// validating webhook forbids it in External mode). Only the external block's
		// own defaults are materialized here.
		if ext := obj.Spec.Services.Keystone.External; ext != nil {
			if ext.EndpointType == "" {
				ext.EndpointType = DefaultExternalEndpointType
			}
			if ext.CABundleSecretRef != nil && ext.CABundleSecretRef.Key == "" {
				ext.CABundleSecretRef.Key = DefaultCABundleSecretKey
			}
		}
	} else {
		// The placed Keystone's own CA bundle takes the same key default as the
		// External-mode one above, for the same reason: the shared SecretRefSpec
		// carries no c5c3-specific marker, so the default is webhook-only.
		if ks := obj.Spec.Services.Keystone; ks != nil &&
			ks.CABundleSecretRef != nil && ks.CABundleSecretRef.Key == "" {
			ks.CABundleSecretRef.Key = DefaultCABundleSecretKey
		}

		// Managed mode (or unset keystone): well-known infrastructure defaults so a
		// minimal managed-mode CR can omit spec.infrastructure entirely. The
		// mode-neutral leaves (database name, secretRef.name, cache backend) are
		// defaulted in BOTH managed and brownfield mode; the managed clusterRef is
		// only invented when the brownfield discriminator (database.host /
		// cache.servers) is unset, so the validating webhook's database/cache XOR
		// check still passes for a brownfield CR — the webhook never coerces an
		// explicit brownfield endpoint into managed mode. Materialize an empty block
		// when nil so the leaf defaulting preserves today's omit-infrastructure
		// contract.
		if obj.Spec.Infrastructure == nil {
			obj.Spec.Infrastructure = &InfrastructureSpec{}
		}
		defaultDatabaseLeaves(&obj.Spec.Infrastructure.Database, DefaultDatabaseClusterRefName)
		defaultCacheLeaves(&obj.Spec.Infrastructure.Cache, DefaultCacheClusterRefName)

		// Messaging is opt-in: the block is never materialized, only its leaves
		// are defaulted when the CR declares it.
		if m := obj.Spec.Infrastructure.Messaging; m != nil {
			defaultMessagingLeaves(m, DefaultMessagingClusterRefName)
		}

		// Per-service DEDICATED backing services take the same leaf defaults as the
		// shared block — the same helpers, so a dedicated instance can never drift
		// from the shared one on what an omitted leaf means — but derive their
		// managed clusterRef name from the ControlPlane so they never collide with
		// the shared instance. A dedicated block is only defaulted when the operator
		// declared it: absent means "share the ControlPlane-wide instances", and the
		// webhook must never invent an opt-in.
		if ks := keystoneDedicatedBlock(obj); ks != nil {
			if db := ks.Database; db != nil {
				defaultDatabaseLeaves(db, obj.Name+DedicatedKeystoneDatabaseClusterRefSuffix)
				// A dedicated MANAGED database is Static-only: the OpenBao
				// database-engine connection is bootstrapped once per namespace against
				// the SHARED cluster, so no engine role can issue credentials for a
				// dedicated instance. Materialize the mode so the stored spec states the
				// contract the reconciler applies; validate() rejects an explicit Dynamic.
				if db.ClusterRef != nil && db.CredentialsMode == "" {
					db.CredentialsMode = commonv1.CredentialsModeStatic
				}
			}
			if cache := ks.Cache; cache != nil {
				defaultCacheLeaves(cache, obj.Name+DedicatedKeystoneCacheClusterRefSuffix)
			}
		}
		if hz := horizonDedicatedBlock(obj); hz != nil {
			if cache := hz.Cache; cache != nil {
				defaultCacheLeaves(cache, obj.Name+DedicatedHorizonCacheClusterRefSuffix)
			}
		}
		if gl := glanceDedicatedBlock(obj); gl != nil {
			if db := gl.Database; db != nil {
				defaultDatabaseLeaves(db, obj.Name+DedicatedGlanceDatabaseClusterRefSuffix)
				// A dedicated MANAGED Glance database is Static-only for the same
				// reason the Keystone one is (see above): no per-instance OpenBao
				// engine role exists. Materialize the mode; validate() rejects Dynamic.
				if db.ClusterRef != nil && db.CredentialsMode == "" {
					db.CredentialsMode = commonv1.CredentialsModeStatic
				}
			}
			if cache := gl.Cache; cache != nil {
				defaultCacheLeaves(cache, obj.Name+DedicatedGlanceCacheClusterRefSuffix)
			}
		}
		if pl := placementDedicatedBlock(obj); pl != nil {
			if db := pl.Database; db != nil {
				defaultDatabaseLeaves(db, obj.Name+DedicatedPlacementDatabaseClusterRefSuffix)
				// A dedicated MANAGED Placement database is Static-only for the same
				// reason the Keystone one is (see above): no per-instance OpenBao
				// engine role exists. Materialize the mode; validate() rejects Dynamic.
				if db.ClusterRef != nil && db.CredentialsMode == "" {
					db.CredentialsMode = commonv1.CredentialsModeStatic
				}
			}
			if cache := pl.Cache; cache != nil {
				defaultCacheLeaves(cache, obj.Name+DedicatedPlacementCacheClusterRefSuffix)
			}
		}
		if bn := barbicanDedicatedBlock(obj); bn != nil {
			if db := bn.Database; db != nil {
				defaultDatabaseLeaves(db, obj.Name+DedicatedBarbicanDatabaseClusterRefSuffix)
				// A dedicated MANAGED Barbican database is Static-only for the same
				// reason the Keystone one is (see above): no per-instance OpenBao
				// engine role exists. Materialize the mode; validate() rejects Dynamic.
				if db.ClusterRef != nil && db.CredentialsMode == "" {
					db.CredentialsMode = commonv1.CredentialsModeStatic
				}
			}
			if cache := bn.Cache; cache != nil {
				defaultCacheLeaves(cache, obj.Name+DedicatedBarbicanCacheClusterRefSuffix)
			}
		}
		if nn := neutronDedicatedBlock(obj); nn != nil {
			if db := nn.Database; db != nil {
				defaultDatabaseLeaves(db, obj.Name+DedicatedNeutronDatabaseClusterRefSuffix)
				// A dedicated MANAGED Neutron database is Static-only for the same
				// reason the Keystone one is (see above): no per-instance OpenBao
				// engine role exists. Materialize the mode; validate() rejects Dynamic.
				if db.ClusterRef != nil && db.CredentialsMode == "" {
					db.CredentialsMode = commonv1.CredentialsModeStatic
				}
			}
			if cache := nn.Cache; cache != nil {
				defaultCacheLeaves(cache, obj.Name+DedicatedNeutronCacheClusterRefSuffix)
			}
		}

		// The OVNCentral the network service programs defaults to the
		// ControlPlane's own namespace, so a CR that names a central without
		// spelling its namespace out states the namespace the reconciler resolves
		// anyway: NeutronOVNCentralNamespace() reads an empty value as the
		// ControlPlane's namespace too, which keeps a CR that bypassed this webhook
		// on the same central as one that did not. A nil neutron block is left
		// untouched, like every other opt-in above.
		if nn := obj.Spec.Services.Neutron; nn != nil && nn.OVN.CentralRef.Namespace == "" {
			nn.OVN.CentralRef.Namespace = obj.Namespace
		}
	}

	// K-ORC admin-credential defaults. cloudCredentialsRef.secretName defaults to
	// the documented shared Secret name.
	korc := &obj.Spec.KORC.AdminCredential
	if korc.CloudCredentialsRef.SecretName == "" {
		korc.CloudCredentialsRef.SecretName = DefaultCloudCredentialsSecretName
	}
	// cloudCredentialsRef.cloudName defaults to the conventional admin
	// cloud entry; passwordSecretRef.name/.key default to the conventional admin
	// Secret and its data key. Defaulting .key makes the stored spec explicit and
	// consistent with the reconciler's existing readAdminPassword "password"
	// fallback. These are webhook-only (no marker on the shared commonv1 types).
	if korc.CloudCredentialsRef.CloudName == "" {
		korc.CloudCredentialsRef.CloudName = DefaultCloudName
	}
	if korc.PasswordSecretRef.Name == "" {
		korc.PasswordSecretRef.Name = DefaultAdminPasswordSecretName
	}
	if korc.PasswordSecretRef.Key == "" {
		korc.PasswordSecretRef.Key = DefaultAdminPasswordSecretKey
	}
	// admin identity: userName/projectName default to "admin", domainName to
	// "Default" — the three identities a stock Keystone bootstrap creates. Valid
	// in both keystone modes; consumed by the K-ORC clouds.yaml builders and the
	// admin import filters. Webhook-only mirror of the CRD markers.
	if korc.UserName == "" {
		korc.UserName = DefaultAdminUserName
	}
	if korc.ProjectName == "" {
		korc.ProjectName = DefaultAdminProjectName
	}
	if korc.DomainName == "" {
		korc.DomainName = DefaultAdminDomainName
	}

	// applicationCredential.restricted defaults to true (least-privilege). The
	// pointer lets us distinguish "unset" (nil → default true) from an explicit
	// false, which we must preserve.
	appCred := &korc.ApplicationCredential
	if appCred.Restricted == nil {
		restricted := true
		appCred.Restricted = &restricted
	}

	// applicationCredential.rotation.mode defaults to PasswordDriven.
	if appCred.Rotation.Mode == "" {
		appCred.Rotation.Mode = RotationModePasswordDriven
	}

	return nil
}

// ValidateCreate implements admission.Validator[*ControlPlane].
// DECISION boundary 6 = option (a): in addition to the spec
// checks in validate(), a CREATE is rejected when another ControlPlane already
// exists in the same namespace. The per-CR OpenBao credential paths (admin AC,
// bootstrap admin password, fernet/credential keys) are scoped by namespace+name
// and the CredentialRotation reconciler resolves its target by listing
// ControlPlanes in the namespace and expecting exactly one; enforcing
// one-ControlPlane-per-namespace at admission keeps that resolution unambiguous.
// The check runs only on CREATE (not UPDATE) so an existing CR stays mutable.
// Reviewer: please verify boundary 6 = option (a).
func (w *ControlPlaneWebhook) ValidateCreate(ctx context.Context, obj *ControlPlane) (admission.Warnings, error) {
	warnings := insecurePublicEndpointWarnings(obj)
	warnings = append(warnings, glanceImportFilteringWarnings(obj)...)
	warnings = append(warnings, warnDevelopmentBarbicanSecretStore(obj)...)

	// extraConfig admission checks: the un-gated shape/ownership family (A) and
	// the option-catalog family (B). Both fold their errors into the single
	// Invalid response alongside validate()'s, mirroring how the keystone child
	// folds its catalog errors in.
	ownershipWarnings, ownershipErrs := validateExtraConfigOwnership(obj)
	catalogWarnings, catalogErrs := validateExtraConfigCatalogs(obj)
	warnings = append(warnings, ownershipWarnings...)
	warnings = append(warnings, catalogWarnings...)

	allErrs := w.validate(obj)
	allErrs = append(allErrs, ownershipErrs...)
	allErrs = append(allErrs, catalogErrs...)
	allErrs = append(allErrs, validateGlanceChildName(obj)...)
	allErrs = append(allErrs, validatePlacementChildName(obj)...)
	allErrs = append(allErrs, validateBarbicanChildName(obj)...)
	allErrs = append(allErrs, validateNeutronChildName(obj)...)
	allErrs = append(allErrs, ValidateNeutronOVNCentralNamespace(obj)...)
	if err := newInvalidIfErrs(obj, allErrs); err != nil {
		return warnings, err
	}
	if err := w.validateUniqueInNamespace(ctx, obj); err != nil {
		return warnings, err
	}
	return warnings, w.validateNamespaceClaims(ctx, obj)
}

// ValidateUpdate implements admission.Validator[*ControlPlane].
// In addition to the spec checks in validate(), it enforces the create-only
// immutable fields between oldObj and newObj (validateImmutable): flipping the
// database/cache mode or renaming a managed clusterRef would orphan the
// previously-projected MariaDB/Memcached child (and its credentials), renaming
// cloudCredentialsRef.secretName would leak the previously-projected K-ORC
// clouds.yaml ExternalSecret (#476), and renaming the database or changing the
// region would re-point the projection at the now-immutable Keystone child
// fields and wedge the reconcile loop (#466). It additionally rejects an
// openStackRelease downgrade (validateReleaseNotDowngraded), since Keystone DB
// migrations are forward-only. Spec errors, immutability errors, and the
// downgrade error are accumulated into a single Invalid response so a reviewer
// sees all problems at once.
//
// It finally re-runs the cluster-wide namespace-claim check when — and only
// when — this update changed the claim set, because the declared-before carve-out
// in validateServiceNamespacesImmutable lets an UPDATE introduce a claim the
// create-only check never saw.
func (w *ControlPlaneWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *ControlPlane) (admission.Warnings, error) {
	warnings := insecurePublicEndpointWarnings(newObj)
	warnings = append(warnings, glanceImportFilteringWarnings(newObj)...)
	warnings = append(warnings, warnDevelopmentBarbicanSecretStore(newObj)...)

	allErrs := w.validate(newObj)
	allErrs = append(allErrs, validateImmutable(oldObj, newObj)...)
	allErrs = append(allErrs, validateReleaseNotDowngraded(oldObj, newObj)...)

	// Family A (shape/ownership) always re-runs: it depends on nothing a
	// regenerated catalog can invalidate, and a newly-derived Horizon endpoint
	// can turn a previously-admitted trusted_dashboard override into a rejection.
	ownershipWarnings, ownershipErrs := validateExtraConfigOwnership(newObj)
	warnings = append(warnings, ownershipWarnings...)
	allErrs = append(allErrs, ownershipErrs...)

	// Family B (option catalog) re-runs only when one of its inputs changed, so
	// an unrelated update (e.g. scaling replicas) never retroactively rejects a
	// CR whose extraConfig was accepted at create time but has since been
	// invalidated by a regenerated catalog.
	if controlPlaneExtraConfigCatalogInputsChanged(oldObj, newObj) {
		catalogWarnings, catalogErrs := validateExtraConfigCatalogs(newObj)
		warnings = append(warnings, catalogWarnings...)
		allErrs = append(allErrs, catalogErrs...)
	}

	// The projected-child-name bounds re-run only when this update is what enables
	// the service: they are derived from the immutable ControlPlane name, so on any
	// other update they could only reject a CR that was already admitted with the
	// service on — including the finalizer-removal update that deletes it.
	if oldObj.Spec.Services.Glance == nil {
		allErrs = append(allErrs, validateGlanceChildName(newObj)...)
	}
	if oldObj.Spec.Services.Placement == nil {
		allErrs = append(allErrs, validatePlacementChildName(newObj)...)
	}
	if oldObj.Spec.Services.Barbican == nil {
		allErrs = append(allErrs, validateBarbicanChildName(newObj)...)
	}
	if oldObj.Spec.Services.Neutron == nil {
		allErrs = append(allErrs, validateNeutronChildName(newObj)...)
	}

	// The OVNCentral reach check re-runs only when this update is what enables the
	// network service or moves the ref, the two updates that can newly violate it.
	// The claim set the reach is measured against cannot shrink — dropping a
	// service's namespace assignment, block and all, is refused by
	// validateServiceNamespacesImmutable — so on any other update the check could
	// only reject a CR a previous operator build already admitted, including the
	// finalizer-removal update that completes a deletion. failurePolicy: Fail would
	// then reject that removal on every attempt and leave the CR in Terminating
	// with no recovery but stripping the finalizer by hand. The CRs this gate
	// deliberately lets through are covered by the controller-side backstop in
	// reconcileOVN (see ValidateNeutronOVNCentralNamespace).
	if oldObj.Spec.Services.Neutron == nil ||
		oldObj.NeutronOVNCentralNamespace() != newObj.NeutronOVNCentralNamespace() {
		allErrs = append(allErrs, ValidateNeutronOVNCentralNamespace(newObj)...)
	}

	if err := newInvalidIfErrs(newObj, allErrs); err != nil {
		return warnings, err
	}

	// The tenant-isolation check is no longer create-only: the declared-before
	// carve-out in validateServiceNamespacesImmutable admits an update that assigns
	// a namespace to a service the ControlPlane did not declare before, so an UPDATE
	// can introduce a claim ValidateCreate never saw. Re-run it whenever the claim
	// set changed — and only then, so an unrelated update neither pays for the
	// cluster-wide List nor is retroactively rejected by a claim that landed after
	// this ControlPlane was admitted.
	if !slices.Equal(oldObj.DedicatedServiceNamespaces(), newObj.DedicatedServiceNamespaces()) {
		return warnings, w.validateNamespaceClaims(ctx, newObj)
	}
	return warnings, nil
}

// validate accumulates all spec validation errors for cp.
// The kubebuilder markers / CEL rules on the CRD are the primary enforcement
// point at admission time; the checks below are defense-in-depth (mirroring the
// KeystoneWebhook discipline) so callers that bypass CRD schema admission still
// get field-specific errors. It returns the accumulated field errors; callers
// wrap them via newInvalidIfErrs so ValidateUpdate can fold in the immutability
// errors before constructing a single Invalid response.
func (w *ControlPlaneWebhook) validate(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// openStackRelease must match the date-based release pattern.
	// Mirrors the +kubebuilder:validation:Pattern marker on
	// ControlPlaneSpec.OpenStackRelease.
	if !controlPlaneReleaseRegexp.MatchString(cp.Spec.OpenStackRelease) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("openStackRelease"),
			cp.Spec.OpenStackRelease,
			"must match the OpenStack release pattern ^\\d{4}\\.[12]$ (e.g. 2025.2)",
		))
	}

	// secretStoreRef is optional (nil defaults to the shared cluster store);
	// when set it must carry a name and a kind in the enum. Defense-in-depth
	// twin of the CRD markers on commonv1.SecretStoreRefSpec.
	allErrs = append(allErrs, validation.SecretStoreRef(specPath.Child("secretStoreRef"), cp.Spec.SecretStoreRef)...)

	// spec.infrastructure is optional at the Go/CRD layer now (External keystone
	// mode omits it), so the database/cache checks only run when the block is
	// present. The mode-conditional required/forbidden rules for the block itself
	// are added with the External-mode validation matrix; here a nil block simply
	// has no database/cache to validate.
	if infra := cp.Spec.Infrastructure; infra != nil {
		// database must use exactly one of clusterRef or host, and CredentialsMode
		// Dynamic (engine-issued credentials) requires managed mode (ClusterRef
		// set) — the shared validators mirroring the CEL rules on the shared
		// commonv1.DatabaseSpec.
		db := infra.Database
		allErrs = append(allErrs, validation.DatabaseXOR(specPath.Child("infrastructure", "database"), &db)...)
		allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(specPath.Child("infrastructure", "database"), &db)...)

		allErrs = append(allErrs, validateDatabaseReplicas(specPath.Child("infrastructure", "database"), &db)...)

		// cache must use exactly one of clusterRef or servers — the shared
		// validator mirroring the CEL rule on the shared commonv1.CacheSpec.
		cache := infra.Cache
		allErrs = append(allErrs, validation.CacheXOR(specPath.Child("infrastructure", "cache"), &cache)...)
		// The block is projected onto every service CR's spec.cache, where it
		// reaches the verbatim INI renderer. Rejecting a control character here
		// fails the ControlPlane at admission rather than leaving a projected
		// child CR to be rejected by its own webhook mid-reconcile.
		allErrs = append(allErrs, validation.CacheNoControlChars(specPath.Child("infrastructure", "cache"), &cache)...)

		// messaging must use exactly one of clusterRef or secretRef (the shared
		// validator mirroring the CEL rule on commonv1.MessagingSpec), and the
		// Secret names a brownfield or tls block points at cannot be empty. The
		// block is optional and never materialized, so a nil block has nothing
		// to validate.
		if m := infra.Messaging; m != nil {
			mPath := specPath.Child("infrastructure", "messaging")
			allErrs = append(allErrs, validation.MessagingXOR(mPath, m)...)
			if m.SecretRef != nil && m.SecretRef.Name == "" {
				allErrs = append(allErrs, field.Required(mPath.Child("secretRef", "name"),
					"must be set when messaging is brownfield (secretRef)"))
			}
			if m.TLS != nil {
				if m.TLS.CABundleSecretRef.Name == "" {
					allErrs = append(allErrs, field.Required(mPath.Child("tls", "caBundleSecretRef", "name"),
						"must be set when messaging.tls is configured"))
				}
				// tls carries CLIENT trust only, and ensureRabbitMQ projects
				// spec.replicas and nothing else — a managed broker therefore comes up
				// on the RabbitMQ Cluster Operator's default, plaintext listener.
				// Admitting tls beside a clusterRef would promise an encrypted
				// connection nothing provisions, and the mismatch would only surface
				// when the first consumer renders ssl = true against a broker that
				// never had a TLS listener.
				if m.ClusterRef != nil {
					allErrs = append(allErrs, field.Invalid(mPath.Child("tls"), *m.TLS,
						"messaging.tls configures client trust only; a managed RabbitmqCluster is "+
							"provisioned without a TLS listener, so tls is supported in brownfield mode only"))
				}
			}
		}
	}

	// the K-ORC admin-credential password Secret reference is required —
	// without it the reconciler cannot (re-)mint the admin application credential.
	if cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("korc", "adminCredential", "passwordSecretRef", "name"),
			"passwordSecretRef.name must be set",
		))
	}

	// reject a Keystone rotationInterval the reconciler's intervalToCron
	// cannot represent (only a positive whole number of days — 168h weekly or any
	// positive multiple of 24h daily) so a bad interval is a clean admission error
	// rather than a steady-state KeystoneReady=False with no requeue. Mirrors
	// intervalToCron in internal/controller/helpers.go and is kept in sync as
	// defense-in-depth, exactly like the openStackRelease pattern check above.
	// services.keystone is optional; all per-service checks below only apply when
	// the block is present.
	if ks := cp.Spec.Services.Keystone; ks != nil {
		if ri := ks.RotationInterval; ri != nil {
			if d := ri.Duration; d <= 0 || d%(24*time.Hour) != 0 {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("services", "keystone", "rotationInterval"),
					d.String(),
					"must be a positive whole number of days (e.g. 24h, 168h); only daily and weekly Fernet rotation schedules are supported",
				))
			}
		}

		// When a gateway is configured, its hostname must be set and usable as
		// the host of the derived public endpoint. Mirrors the
		// +kubebuilder:validation:MinLength=1 marker on commonv1.GatewaySpec.Hostname;
		// without it the reconciler derives an empty "https:///v3" public endpoint.
		if g := ks.Gateway; g != nil {
			hostnamePath := specPath.Child("services", "keystone", "gateway", "hostname")
			if g.Hostname == "" {
				allErrs = append(allErrs, field.Required(hostnamePath,
					"must be set when a gateway is configured"))
			} else if err := validateGatewayHostname(hostnamePath, g.Hostname); err != nil {
				allErrs = append(allErrs, err)
			}
		}

		// When the Keystone image is overridden, mirror the ImageSpec tag/digest
		// XOR (the +kubebuilder:validation:XValidation rule on commonv1.ImageSpec)
		// with a defense-in-depth check: exactly one of tag or digest must be set.
		if img := ks.Image; img != nil && (img.Tag != "") == (img.Digest != "") {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("services", "keystone", "image"),
				img,
				"exactly one of image.tag or image.digest must be set",
			))
		}
	}

	// services.horizon is optional; all per-service checks below only apply
	// when the block is present. Policy overrides are N/A for horizon (the
	// dashboard enforces no oslo.policy of its own), so unlike keystone there
	// is no per-service policy block to validate.
	if hz := cp.Spec.Services.Horizon; hz != nil {
		// When a gateway is configured, its hostname must be set and usable as
		// the host of the WebSSO origin derived from it. Mirrors the
		// +kubebuilder:validation:MinLength=1 marker on commonv1.GatewaySpec.Hostname.
		if g := hz.Gateway; g != nil {
			hostnamePath := specPath.Child("services", "horizon", "gateway", "hostname")
			if g.Hostname == "" {
				allErrs = append(allErrs, field.Required(hostnamePath,
					"must be set when a gateway is configured"))
			} else if err := validateGatewayHostname(hostnamePath, g.Hostname); err != nil {
				allErrs = append(allErrs, err)
			}
		}

		allErrs = append(allErrs, validateHorizonPublicEndpoint(specPath, hz)...)

		// When the Horizon image is overridden, mirror the ImageSpec tag/digest
		// XOR (the +kubebuilder:validation:XValidation rule on commonv1.ImageSpec)
		// with a defense-in-depth check: exactly one of tag or digest must be set.
		if img := hz.Image; img != nil && (img.Tag != "") == (img.Digest != "") {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("services", "horizon", "image"),
				img,
				"exactly one of image.tag or image.digest must be set",
			))
		}

		// When the SECRET_KEY Secret is overridden, its name must be non-empty.
		// Mirrors the MinLength marker on commonv1.SecretRefSpec.Name.
		if ref := hz.SecretKeyRef; ref != nil && ref.Name == "" {
			allErrs = append(allErrs, field.Required(
				specPath.Child("services", "horizon", "secretKeyRef", "name"),
				"must be set when secretKeyRef is configured",
			))
		}
	}

	// Reject empty policy rule names and values on both the global policy and the
	// per-service Keystone override. The c5c3 webhook previously validated policy
	// rules not at all; this mirrors the keystone webhook and the CEL rule on
	// commonv1.PolicySpec, closing the empty-value gap the audit reported
	// (issue #479).
	if g := cp.Spec.GlobalPolicyOverrides; g != nil {
		allErrs = append(allErrs, policy.ValidatePolicyRules(
			g.Rules, specPath.Child("globalPolicyOverrides", "rules"),
		)...)
	}
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.PolicyOverrides != nil {
		allErrs = append(allErrs, policy.ValidatePolicyRules(
			ks.PolicyOverrides.Rules, specPath.Child("services", "keystone", "policyOverrides", "rules"),
		)...)
	}

	allErrs = append(allErrs, validateGlance(cp)...)
	allErrs = append(allErrs, validatePlacement(cp)...)
	allErrs = append(allErrs, validateBarbican(cp)...)
	allErrs = append(allErrs, validateNeutron(cp)...)
	allErrs = append(allErrs, validateKeystoneMode(cp)...)
	allErrs = append(allErrs, validateServiceRegistrations(cp)...)
	allErrs = append(allErrs, validateDedicatedBackingServices(cp)...)
	allErrs = append(allErrs, validateServiceCredentialsModeOverrides(cp)...)
	allErrs = append(allErrs, validateServiceNamespaces(cp)...)
	allErrs = append(allErrs, validateServiceTargetClusters(cp)...)

	return allErrs
}

// namespaceNamePattern mirrors the Pattern marker on ServiceNamespaceSpec.Name:
// a namespace name must be an RFC-1123 label.
var namespaceNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// serviceNamespaceAssignment pairs one service's declared namespace block with
// the field path it lives at, so the validators below walk every service
// uniformly and a new service extends the walk rather than reshaping it.
type serviceNamespaceAssignment struct {
	path *field.Path
	ns   *ServiceNamespaceSpec
}

// declaredServiceNamespaces returns the namespace assignments cp actually
// declares, in a stable order. A service that stays in the ControlPlane's
// namespace (the default) contributes nothing.
func declaredServiceNamespaces(cp *ControlPlane) []serviceNamespaceAssignment {
	svcPath := field.NewPath("spec", "services")
	var out []serviceNamespaceAssignment
	if ns := keystoneNamespaceBlock(cp); ns != nil {
		out = append(out, serviceNamespaceAssignment{path: svcPath.Child("keystone", "namespace"), ns: ns})
	}
	if ns := horizonNamespaceBlock(cp); ns != nil {
		out = append(out, serviceNamespaceAssignment{path: svcPath.Child("horizon", "namespace"), ns: ns})
	}
	if ns := glanceNamespaceBlock(cp); ns != nil {
		out = append(out, serviceNamespaceAssignment{path: svcPath.Child("glance", "namespace"), ns: ns})
	}
	if ns := placementNamespaceBlock(cp); ns != nil {
		out = append(out, serviceNamespaceAssignment{path: svcPath.Child("placement", "namespace"), ns: ns})
	}
	if ns := barbicanNamespaceBlock(cp); ns != nil {
		out = append(out, serviceNamespaceAssignment{path: svcPath.Child("barbican", "namespace"), ns: ns})
	}
	if ns := neutronNamespaceBlock(cp); ns != nil {
		out = append(out, serviceNamespaceAssignment{path: svcPath.Child("neutron", "namespace"), ns: ns})
	}
	return out
}

// validateServiceNamespaces enforces the rules on the per-service namespace
// assignments. It mirrors the declarative constraints as defense-in-depth for
// callers that bypass CRD schema admission (the RFC-1123 name shape, the
// lifecycle enum) and adds the two the CRD schema cannot express:
//
//   - An assignment must not name the ControlPlane's OWN namespace. The block is
//     the opt-in to a SEPARATE namespace; naming the current one is a no-op the
//     reconciler would have to special-case at every cross-namespace site, and
//     under the Managed lifecycle it would make the operator claim ownership of —
//     and, at teardown, delete — the namespace the ControlPlane itself lives in.
//   - Two services placed in the SAME namespace must agree on its lifecycle. They
//     share that namespace's backing services and its tenant store, so they cannot
//     disagree on whether the operator owns it: one declaration would have the
//     teardown delete the namespace the other declared untouchable.
//
// The cross-field rule that a namespace assignment is forbidden in External mode
// lives in validateKeystoneMode with the rest of the External-mode matrix.
func validateServiceNamespaces(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList

	lifecycles := map[string]ServiceNamespaceLifecycle{}
	for _, a := range declaredServiceNamespaces(cp) {
		namePath := a.path.Child("name")
		switch {
		case a.ns.Name == "":
			allErrs = append(allErrs, field.Required(namePath, "must be set"))
		case a.ns.Name == cp.Namespace:
			allErrs = append(allErrs, field.Invalid(namePath, a.ns.Name,
				"must differ from the ControlPlane's own namespace; omit the block entirely to place the service "+
					"in the ControlPlane's namespace"))
		case !namespaceNamePattern.MatchString(a.ns.Name):
			allErrs = append(allErrs, field.Invalid(namePath, a.ns.Name,
				"must be a lowercase alphanumeric RFC-1123 label (it names a Kubernetes namespace)"))
		}

		switch a.ns.Lifecycle {
		case ServiceNamespaceLifecycleManaged, ServiceNamespaceLifecycleExternal, "":
		default:
			allErrs = append(allErrs, field.NotSupported(a.path.Child("lifecycle"), a.ns.Lifecycle,
				[]ServiceNamespaceLifecycle{ServiceNamespaceLifecycleManaged, ServiceNamespaceLifecycleExternal}))
		}

		if a.ns.Name == "" {
			continue
		}
		if seen, dup := lifecycles[a.ns.Name]; dup && seen != a.ns.Lifecycle {
			allErrs = append(allErrs, field.Invalid(a.path.Child("lifecycle"), a.ns.Lifecycle, fmt.Sprintf(
				"services co-located in namespace %q must declare the same lifecycle; they share that namespace's "+
					"backing services and secret store, so they cannot disagree on whether the operator owns it",
				a.ns.Name,
			)))
		}
		lifecycles[a.ns.Name] = a.ns.Lifecycle
	}

	return allErrs
}

// serviceTargetClusterAssignment pairs one service's declared block with the
// field path it lives at and the facts the placement rules read off that block:
// the target cluster it names, the namespace it declares, and whether it names
// an address reachable from outside the cluster it runs on. It mirrors
// serviceNamespaceAssignment, so the validator walks every service uniformly and
// a new service extends the walk rather than reshaping it.
type serviceTargetClusterAssignment struct {
	path *field.Path
	ref  *commonv1.TargetClusterRefSpec
	ns   *ServiceNamespaceSpec
	// catalog marks the services the ControlPlane advertises in the Keystone
	// service catalog. Horizon is the one declared service outside it: the
	// dashboard is reached by a browser, not looked up by an OpenStack client.
	catalog bool
	// published reports whether the block names an externally routable address,
	// either a publicEndpoint or the gateway a public endpoint is derived from.
	published bool
}

// sameTargetClusterName reports whether a and b resolve to the same cluster, with
// nil — no ref, so the local cluster — equal to nil. A ref carries nothing but a
// name, so the comparison is on the name alone.
func sameTargetClusterName(a, b *commonv1.TargetClusterRefSpec) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Name == b.Name
}

// declaredServiceTargetClusters returns one assignment per DECLARED service
// block, in a stable order. Unlike declaredServiceNamespaces it also returns the
// blocks that carry no ref: the co-location rule compares the services of one
// namespace against each other, and one placed next to one unplaced is exactly
// the disagreement it rejects.
func declaredServiceTargetClusters(cp *ControlPlane) []serviceTargetClusterAssignment {
	svcPath := field.NewPath("spec", "services")
	var out []serviceTargetClusterAssignment
	if ks := cp.Spec.Services.Keystone; ks != nil {
		out = append(out, serviceTargetClusterAssignment{
			path: svcPath.Child("keystone"), ref: ks.TargetClusterRef, ns: ks.Namespace,
			catalog: true, published: ks.PublicEndpoint != "" || ks.Gateway != nil,
		})
	}
	if hz := cp.Spec.Services.Horizon; hz != nil {
		out = append(out, serviceTargetClusterAssignment{
			path: svcPath.Child("horizon"), ref: hz.TargetClusterRef, ns: hz.Namespace,
			catalog: false, published: hz.PublicEndpoint != "" || hz.Gateway != nil,
		})
	}
	if gl := cp.Spec.Services.Glance; gl != nil {
		out = append(out, serviceTargetClusterAssignment{
			path: svcPath.Child("glance"), ref: gl.TargetClusterRef, ns: gl.Namespace,
			catalog: true, published: gl.PublicEndpoint != "" || gl.Gateway != nil,
		})
	}
	if pl := cp.Spec.Services.Placement; pl != nil {
		out = append(out, serviceTargetClusterAssignment{
			path: svcPath.Child("placement"), ref: pl.TargetClusterRef, ns: pl.Namespace,
			catalog: true, published: pl.PublicEndpoint != "" || pl.Gateway != nil,
		})
	}
	if bn := cp.Spec.Services.Barbican; bn != nil {
		out = append(out, serviceTargetClusterAssignment{
			path: svcPath.Child("barbican"), ref: bn.TargetClusterRef, ns: bn.Namespace,
			catalog: true, published: bn.PublicEndpoint != "" || bn.Gateway != nil,
		})
	}
	if nn := cp.Spec.Services.Neutron; nn != nil {
		out = append(out, serviceTargetClusterAssignment{
			path: svcPath.Child("neutron"), ref: nn.TargetClusterRef, ns: nn.Namespace,
			catalog: true, published: nn.PublicEndpoint != "" || nn.Gateway != nil,
		})
	}
	return out
}

// validateServiceTargetClusters enforces the rules on the per-service
// target-cluster assignments. It mirrors the name-only shape of the ref as
// defense-in-depth for callers that bypass CRD schema admission
// (validation.TargetClusterRef) and adds the six rules the CRD schema cannot
// express:
//
//   - A placed service must declare a namespace of its OWN. Every namespace maps
//     to exactly one cluster, and the ControlPlane's own namespace stays on the
//     local cluster the operator runs on, so a service placed elsewhere without a
//     namespace block would have its database, its tenant store, and its
//     credential material provisioned in a namespace that lives on another
//     cluster than the ref names.
//   - A placed CATALOG service must advertise a publicEndpoint or a gateway. What
//     the ControlPlane registers for an unpublished service is its in-cluster
//     Service DNS name, which resolves nowhere outside the cluster the service
//     runs on, so every client that reads the catalog from anywhere else gets an
//     address it cannot connect to. Horizon is exempt: it is not in the catalog.
//   - Two services declaring the SAME namespace must name the SAME cluster, and a
//     placed service must not share its namespace with an unplaced one. This is
//     the co-location rule of validateServiceNamespaces one level out: a namespace
//     exists on exactly one cluster, so the services in it cannot disagree on which.
//   - Keystone must advertise a publicEndpoint or a gateway as soon as ANY other
//     service is placed away from it, whether or not Keystone itself carries a
//     ref. Every dependent service validates its tokens against Keystone, and the
//     rule above only reaches the ones carrying a ref of their own.
//   - Keystone's publicEndpoint must use https as soon as ANY service is placed
//     away from it, Keystone itself included. That is the condition under which
//     the URL becomes the auth_url the operator renders the admin password and
//     every service-account password next to, and under which those credentials
//     cross a cluster boundary to reach it — whichever of the two ends moved.
//   - Keystone's caBundleSecretRef is forbidden as soon as a service does NOT
//     share Keystone's cluster. The bundle reaches K-ORC alone: it is projected
//     into the two K-ORC credentials Secrets, and no other consumer of that same
//     https URL has anywhere to put a trust anchor — a service's projected
//     spec.keystoneEndpoint renders into [keystone_authtoken], which carries no
//     cafile option. Admitting the pair would report trust the data plane cannot
//     enforce, and leave the service failing every token validation with no field
//     on any CR in the tree to fix it with.
//
// Whether the named cluster is REGISTERED is deliberately not checked. A cluster
// registration is a runtime fact that can appear and disappear long after the
// edit, so an unresolvable ref surfaces per CR as a condition rather than
// rejecting a ControlPlane the operator can still converge on later.
//
// The cross-field rule that a ref is forbidden in External mode lives in
// validateKeystoneMode with the rest of the External-mode matrix.
func validateServiceTargetClusters(cp *ControlPlane) field.ErrorList {
	// External mode deploys nothing to place: validateKeystoneMode forbids the ref
	// there, along with the namespace and the publicEndpoint this validator would
	// otherwise demand, and every other service block outright. Running the
	// prerequisites on top of that matrix would answer a forbidden publicEndpoint
	// with a requirement for one.
	if cp.IsExternalKeystone() {
		return nil
	}

	var allErrs field.ErrorList

	ksPath := field.NewPath("spec", "services", "keystone")
	ksRef := cp.KeystoneTargetClusterRef()
	// placedAwayFromKeystone: a service carries a ref while Keystone does not, so
	// Keystone's publication is what the placed service has to reach it by.
	// unsharedWithKeystone: the same question one step wider — a service resolves
	// to a DIFFERENT cluster than Keystone, whichever of the two carries the ref.
	// The first drives the publication requirement, which a Keystone carrying a ref
	// of its own already gets from the catalog rule in the loop; the second drives
	// the two rules that only care that a cluster boundary is crossed at all.
	placedAwayFromKeystone, unsharedWithKeystone := false, false

	placements := map[string]*commonv1.TargetClusterRefSpec{}
	for _, a := range declaredServiceTargetClusters(cp) {
		refPath := a.path.Child("targetClusterRef")
		allErrs = append(allErrs, validation.TargetClusterRef(refPath, a.ref)...)
		keystone := a.path.String() == ksPath.String()
		if !keystone && !sameTargetClusterName(a.ref, ksRef) {
			unsharedWithKeystone = true
		}

		if a.ref != nil {
			if a.ns == nil {
				allErrs = append(allErrs, field.Required(a.path.Child("namespace"),
					"is required when targetClusterRef is set: every namespace maps to exactly one cluster, and the "+
						"ControlPlane's own namespace stays on the local one, so a placed service needs a namespace "+
						"of its own"))
			}
			if a.catalog && !a.published {
				allErrs = append(allErrs, field.Required(a.path.Child("publicEndpoint"),
					"one of publicEndpoint or gateway is required when targetClusterRef is set: the service's "+
						"in-cluster Service DNS name resolves nowhere from another cluster, so the catalog entry "+
						"must advertise an externally routable address"))
			}
			if !keystone && ksRef == nil {
				placedAwayFromKeystone = true
			}
		}

		if a.ns == nil || a.ns.Name == "" {
			continue
		}
		seen, dup := placements[a.ns.Name]
		sameCluster := (seen == nil) == (a.ref == nil) &&
			(seen == nil || seen.Name == a.ref.Name)
		if dup && !sameCluster {
			allErrs = append(allErrs, field.Invalid(refPath, a.ref, fmt.Sprintf(
				"services co-located in namespace %q must be placed on the same target cluster; the namespace "+
					"exists on exactly one cluster, together with the backing services, the tenant store, and the "+
					"credential material scoped to it",
				a.ns.Name,
			)))
		}
		placements[a.ns.Name] = a.ref
	}

	// A service that does not share Keystone's cluster validates its tokens against
	// Keystone over the public URL, because Keystone's in-cluster Service DNS name
	// resolves only on the cluster Keystone runs on. The catalog rule above demands
	// that publication of a service carrying a ref of its OWN, which leaves the
	// case an unplaced Keystone falls into: the operator would project an empty
	// spec.keystoneEndpoint into the placed child, and the child's CRD refuses it
	// (MinLength=1, ^https?://) on every pass with nothing on the ControlPlane
	// naming the field to fix.
	if placedAwayFromKeystone {
		ks := cp.Spec.Services.Keystone
		if ks == nil || (ks.PublicEndpoint == "" && ks.Gateway == nil) {
			allErrs = append(allErrs, field.Required(ksPath.Child("publicEndpoint"),
				"one of publicEndpoint or gateway is required when another service is placed on a target cluster: "+
					"that service validates its tokens against Keystone and cannot resolve Keystone's in-cluster "+
					"Service DNS name from another cluster"))
		}
	}

	// The scheme of the URL every credential document carries the passwords next
	// to. It is the auth_url as soon as ONE end of the pair moves: K-ORC stays on
	// the management cluster and dials it whenever Keystone is placed, and a
	// service placed away from an unplaced Keystone dials it from the other side.
	// Either way the admin password, every service-account password, and the tokens
	// minted with them cross a cluster boundary to reach it. The ^https?:// pattern
	// on the field admits http:// for the case where neither end moved, where the
	// URL feeds only the bootstrap and the catalog.
	if ks := cp.Spec.Services.Keystone; ks != nil && (ksRef != nil || placedAwayFromKeystone) &&
		externalAuthURLIsPlaintext(ks.PublicEndpoint) {
		allErrs = append(allErrs, field.Invalid(ksPath.Child("publicEndpoint"), ks.PublicEndpoint,
			"must use scheme https when any service is placed away from Keystone: that URL becomes the auth_url "+
				"the operator renders the admin password and every service-account password next to, and those "+
				"credentials cross a cluster boundary to reach it"))
	}

	// The private-CA bundle verifying a placed Keystone's endpoint. It is only ever
	// consulted during a TLS handshake, and a co-located Keystone is dialled over
	// its in-cluster Service URL, which performs none — accepting the pair would
	// hand the operator positive confirmation that trust is enforced while nothing
	// verifies anything, the same hazard the External-mode plaintext rule rejects.
	//
	// The second rule is the same hazard from the other side. The bundle reaches
	// K-ORC and nothing else: it is projected as the inline cacert key into the two
	// K-ORC credentials Secrets, while a service that does not share Keystone's
	// cluster gets that same https URL as its projected spec.keystoneEndpoint and
	// renders it into [keystone_authtoken], which has no cafile option to put an
	// anchor in. Such a service would fail every token validation with
	// "certificate signed by unknown authority" and no field on any CR in the tree
	// to repair it with, so the combination is refused rather than admitted as
	// TLS-configured. Placing Keystone away from a service therefore requires a
	// publicly trusted certificate on its endpoint.
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.CABundleSecretRef != nil {
		ref, caPath := ks.CABundleSecretRef, ksPath.Child("caBundleSecretRef")
		if ksRef == nil {
			allErrs = append(allErrs, field.Forbidden(caPath,
				"forbidden without services.keystone.targetClusterRef: a co-located Keystone is reached over its "+
					"in-cluster Service URL, which performs no TLS handshake the bundle could verify"))
		} else if unsharedWithKeystone {
			allErrs = append(allErrs, field.Forbidden(caPath,
				"forbidden while a service does not share Keystone's target cluster: the bundle is projected into "+
					"the K-ORC credentials Secrets and nowhere else, and a service reaching that same endpoint "+
					"renders it into [keystone_authtoken], which carries no option for a trust anchor — publish the "+
					"placed Keystone with a publicly trusted certificate, or co-locate every service with it"))
		}
		if ref.Name == "" {
			allErrs = append(allErrs, field.Required(caPath.Child("name"),
				"must be set when caBundleSecretRef is configured"))
		}
	}

	return allErrs
}

// validateNamespaceClaims enforces that a namespace belongs to at most ONE
// ControlPlane. The namespace is the tenant key the whole secret stack is scoped
// by — the OpenBao KV paths (bootstrap/{ns}/…), the database-engine role
// (keystone-{ns}), and the templated eso-tenant policy that confines a store's
// token to its own namespace — so two ControlPlanes sharing one namespace would
// share that scope: exactly the isolation the per-tenant store exists to enforce.
// The one-ControlPlane-per-namespace rule (validateUniqueInNamespace) already
// says so for the ControlPlane's own namespace; a service namespace is the same
// tenant key one level out, so it takes the same rule.
//
// Both directions are rejected: a namespace this ControlPlane claims must not be
// another's (own or service) namespace, and this ControlPlane's own namespace
// must not be another's service namespace. The List is cluster-wide through the
// injected uncached API reader — a service namespace can be claimed from any
// namespace, so a namespace-scoped List would miss the claim it must find.
//
// It runs on CREATE, and on the UPDATEs that change the claim set: the
// assignments of a service declared on the old revision are frozen
// (validateServiceNamespacesImmutable), but that freeze carves out the service
// being ADDED by this update, whose namespace assignment is therefore a claim
// admission has not seen before. ValidateUpdate re-runs the check for exactly
// those updates. A nil w.Client skips the check, mirroring
// validateUniqueInNamespace.
func (w *ControlPlaneWebhook) validateNamespaceClaims(ctx context.Context, obj *ControlPlane) error {
	if w.Client == nil {
		return nil
	}
	claims := declaredServiceNamespaces(obj)
	var existing ControlPlaneList
	if err := w.Client.List(ctx, &existing); err != nil {
		return apierrors.NewInternalError(
			fmt.Errorf("listing ControlPlanes to enforce namespace-claim uniqueness: %w", err),
		)
	}

	var allErrs field.ErrorList
	for i := range existing.Items {
		other := &existing.Items[i]
		if other.UID == obj.UID && other.Namespace == obj.Namespace && other.Name == obj.Name {
			continue
		}
		// Everything the other ControlPlane occupies: its own namespace (whose
		// tenant scope validateUniqueInNamespace already guards) and every service
		// namespace it claims.
		occupied := map[string]struct{}{other.Namespace: {}}
		for _, ns := range other.DedicatedServiceNamespaces() {
			occupied[ns.Name] = struct{}{}
		}

		for _, a := range claims {
			if _, taken := occupied[a.ns.Name]; taken && a.ns.Name != "" {
				allErrs = append(allErrs, field.Invalid(a.path.Child("name"), a.ns.Name, fmt.Sprintf(
					"namespace %q is already occupied by ControlPlane %q in namespace %q; a namespace is the tenant "+
						"key the OpenBao paths and the per-tenant secret store are scoped by, so it belongs to at "+
						"most one ControlPlane",
					a.ns.Name, other.Name, other.Namespace,
				)))
			}
		}

		for _, ns := range other.DedicatedServiceNamespaces() {
			if ns.Name == obj.Namespace {
				allErrs = append(allErrs, field.Forbidden(field.NewPath("metadata", "namespace"), fmt.Sprintf(
					"namespace %q is already claimed as a service namespace by ControlPlane %q in namespace %q; a "+
						"namespace is the tenant key the OpenBao paths and the per-tenant secret store are scoped "+
						"by, so it belongs to at most one ControlPlane",
					obj.Namespace, other.Name, other.Namespace,
				)))
			}
		}
	}

	return newInvalidIfErrs(obj, allErrs)
}

// validateDatabaseReplicas enforces that a managed database's replica count is 1
// (standalone) or >=3 (a quorum-safe Galera cluster). Exactly 2 is rejected
// because the managed-mode MariaDB projection (ensureMariaDB) turns any
// replicas>1 into a Galera cluster, and a two-node Galera cluster cannot hold a
// majority — a single pod disruption (restart, OOM-kill, rolling update, network
// partition) then loses quorum and takes the whole database offline. The CRD
// marker only enforces Minimum=1, so this webhook is the enforcement point; the
// shared commonv1.DatabaseSpec must not carry a c5c3-specific CEL rule the
// keystone operator (which ignores replicas) would also inherit. A zero value
// (CRD/webhook default bypassed) is left to the reconciler's floor, so only an
// explicit 2 is rejected. It applies to the shared database and to every
// dedicated one alike: the projection that makes 2 unsafe is the same.
func validateDatabaseReplicas(fldPath *field.Path, db *commonv1.DatabaseSpec) field.ErrorList {
	if db.Replicas != 2 {
		return nil
	}
	return field.ErrorList{field.Invalid(
		fldPath.Child("replicas"),
		db.Replicas,
		"database replicas must be 1 (standalone) or >=3 (Galera needs quorum); 2 cannot hold a majority",
	)}
}

// dedicatedBackingServices pairs one service's declared dedicated block with the
// field path it lives at, so the validators below walk every service uniformly
// and a new backing-service class or a new service extends the walk rather than
// reshaping it.
type dedicatedBackingServices struct {
	path  *field.Path
	db    *commonv1.DatabaseSpec
	cache *commonv1.CacheSpec
}

// declaredDedicatedBackingServices returns the dedicated blocks cp actually
// declares, in a stable order. A service that shares the ControlPlane-wide
// instances (the default) contributes nothing.
func declaredDedicatedBackingServices(cp *ControlPlane) []dedicatedBackingServices {
	svcPath := field.NewPath("spec", "services")
	var out []dedicatedBackingServices
	if ks := keystoneDedicatedBlock(cp); ks != nil {
		out = append(out, dedicatedBackingServices{
			path:  svcPath.Child("keystone", "dedicatedBackingServices"),
			db:    ks.Database,
			cache: ks.Cache,
		})
	}
	if hz := horizonDedicatedBlock(cp); hz != nil {
		out = append(out, dedicatedBackingServices{
			path:  svcPath.Child("horizon", "dedicatedBackingServices"),
			cache: hz.Cache,
		})
	}
	if gl := glanceDedicatedBlock(cp); gl != nil {
		out = append(out, dedicatedBackingServices{
			path:  svcPath.Child("glance", "dedicatedBackingServices"),
			db:    gl.Database,
			cache: gl.Cache,
		})
	}
	if pl := placementDedicatedBlock(cp); pl != nil {
		out = append(out, dedicatedBackingServices{
			path:  svcPath.Child("placement", "dedicatedBackingServices"),
			db:    pl.Database,
			cache: pl.Cache,
		})
	}
	if bn := barbicanDedicatedBlock(cp); bn != nil {
		out = append(out, dedicatedBackingServices{
			path:  svcPath.Child("barbican", "dedicatedBackingServices"),
			db:    bn.Database,
			cache: bn.Cache,
		})
	}
	if nn := neutronDedicatedBlock(cp); nn != nil {
		out = append(out, dedicatedBackingServices{
			path:  svcPath.Child("neutron", "dedicatedBackingServices"),
			db:    nn.Database,
			cache: nn.Cache,
		})
	}
	return out
}

// validateDedicatedBackingServices enforces the rules on the per-service
// dedicated backing-service blocks. It mirrors the declarative constraints as
// defense-in-depth for callers that bypass CRD schema admission (the
// at-least-one-class CEL rule, and the database/cache XORs carried by the shared
// commonv1 types) and adds the rules the CRD schema cannot express:
//
//   - A dedicated MANAGED database may not use credentialsMode Dynamic. The
//     OpenBao database engine has exactly one connection and role per NAMESPACE
//     (deploy/openbao/bootstrap/setup-database-tenant.sh), bootstrapped against
//     the SHARED cluster, so no engine role exists that could issue credentials
//     for a dedicated instance — an admitted Dynamic dedicated database would
//     wedge on an ExternalSecret that can never sync. Static is the supported
//     mode (and the one the defaulting webhook materializes).
//   - Managed clusterRef NAMES must be unique per backing-service class across
//     the shared block and every dedicated instance. Two instances sharing a name
//     resolve to one child CR, which the projections would then fight over —
//     silently voiding the very isolation the opt-in exists for.
//   - The Galera-quorum replicas rule applies to a dedicated database exactly as
//     it does to the shared one.
//
// The cross-field rule that a dedicated block is forbidden in External mode
// lives in validateKeystoneMode with the rest of the External-mode matrix.
func validateDedicatedBackingServices(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList

	// Seed the per-class name sets with the SHARED instances so a dedicated
	// instance colliding with them is caught, not just a dedicated-vs-dedicated
	// collision.
	dbNames := map[string]struct{}{}
	cacheNames := map[string]struct{}{}
	if infra := cp.Spec.Infrastructure; infra != nil {
		if ref := infra.Database.ClusterRef; ref != nil {
			dbNames[ref.Name] = struct{}{}
		}
		if ref := infra.Cache.ClusterRef; ref != nil {
			cacheNames[ref.Name] = struct{}{}
		}
	}

	for _, d := range declaredDedicatedBackingServices(cp) {
		if d.db == nil && d.cache == nil {
			allErrs = append(allErrs, field.Required(d.path,
				"at least one backing-service class must be declared; omit the block entirely to share the "+
					"ControlPlane-wide instances"))
			continue
		}

		if db := d.db; db != nil {
			dbPath := d.path.Child("database")
			allErrs = append(allErrs, validation.DatabaseXOR(dbPath, db)...)
			allErrs = append(allErrs, validateDatabaseReplicas(dbPath, db)...)
			// Strictly stronger than the shared block's Dynamic-requires-clusterRef
			// rule (which the commonv1 CEL rule still carries): Dynamic is rejected on
			// a dedicated database in EITHER mode.
			if db.CredentialsMode == commonv1.CredentialsModeDynamic {
				allErrs = append(allErrs, field.Forbidden(dbPath.Child("credentialsMode"),
					"credentialsMode Dynamic is not supported on a dedicated database: the OpenBao database-engine "+
						"connection is bootstrapped once per namespace against the shared cluster, so no engine role "+
						"issues credentials for a dedicated instance; use Static"))
			}
			if ref := db.ClusterRef; ref != nil {
				if _, dup := dbNames[ref.Name]; dup {
					allErrs = append(allErrs, field.Duplicate(dbPath.Child("clusterRef", "name"), ref.Name))
				}
				dbNames[ref.Name] = struct{}{}
			}
		}

		if cache := d.cache; cache != nil {
			cachePath := d.path.Child("cache")
			allErrs = append(allErrs, validation.CacheXOR(cachePath, cache)...)
			allErrs = append(allErrs, validation.CacheNoControlChars(cachePath, cache)...)
			if ref := cache.ClusterRef; ref != nil {
				if _, dup := cacheNames[ref.Name]; dup {
					allErrs = append(allErrs, field.Duplicate(cachePath.Child("clusterRef", "name"), ref.Name))
				}
				cacheNames[ref.Name] = struct{}{}
			}
		}
	}

	return allErrs
}

// validateServiceCredentialsModeOverrides enforces the rules on the per-service
// spec.services.<svc>.databaseCredentialsMode override, which overrides the
// ControlPlane-wide spec.infrastructure.database.credentialsMode for one service
// on the managed SHARED database. The Enum marker already bounds the value to
// Static|Dynamic at the schema layer; the two rejections below are the cross-field
// invariants CEL cannot express, so they live in the webhook (per the established
// CEL-vs-webhook split):
//
//   - A Dynamic override is rejected on a service that declares a DEDICATED
//     database. The override targets the shared database, which that service does
//     not use; its dedicated database has its own credentialsMode (Static-only —
//     validateDedicatedBackingServices already rejects Dynamic there), so a Dynamic
//     override on such a service is meaningless. Static stays admissible.
//   - A Dynamic override is rejected when the shared database is brownfield
//     (clusterRef unset), mirroring the commonv1.DatabaseSpec Dynamic-requires-
//     clusterRef contract one level up: the dynamic engine issues per-tenant DB
//     users only against a cluster the operator provisions.
//
// A Static override is always admitted, and an empty override (inherit) is a no-op.
// The External-mode forbid on services.keystone.databaseCredentialsMode lives in
// validateKeystoneMode with the rest of the External-mode matrix (glance,
// placement, barbican, and neutron are forbidden entirely in External mode, so
// they need no such rule).
func validateServiceCredentialsModeOverrides(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	svcPath := field.NewPath("spec", "services")

	// The shared database is managed exactly when it names a clusterRef. A nil
	// infrastructure block (only reachable in External mode, which forbids the
	// keystone override via CEL and forbids glance, placement, barbican, and
	// neutron entirely) counts as not managed.
	sharedManaged := cp.Spec.Infrastructure != nil && cp.Spec.Infrastructure.Database.ClusterRef != nil

	check := func(svc string, mode string, dedicatedDB *commonv1.DatabaseSpec) {
		if mode != commonv1.CredentialsModeDynamic {
			return
		}
		modePath := svcPath.Child(svc, "databaseCredentialsMode")
		switch {
		case dedicatedDB != nil:
			allErrs = append(allErrs, field.Forbidden(modePath,
				"credentialsMode Dynamic is not supported as an override on a service with a dedicated database: "+
					"the override retargets the shared database this service does not use, and a dedicated database is "+
					"Static-only (set dedicatedBackingServices.database.credentialsMode instead)"))
		case !sharedManaged:
			allErrs = append(allErrs, field.Forbidden(modePath,
				"credentialsMode Dynamic requires the shared database to be managed (clusterRef): the dynamic engine "+
					"issues per-tenant DB users only against a cluster the operator provisions, so it cannot run against "+
					"a brownfield database"))
		}
	}

	if ks := cp.Spec.Services.Keystone; ks != nil {
		check("keystone", ks.DatabaseCredentialsMode, cp.DedicatedKeystoneDatabase())
	}
	if gl := cp.Spec.Services.Glance; gl != nil {
		check("glance", gl.DatabaseCredentialsMode, cp.DedicatedGlanceDatabase())
	}
	if pl := cp.Spec.Services.Placement; pl != nil {
		check("placement", pl.DatabaseCredentialsMode, cp.DedicatedPlacementDatabase())
	}
	if bn := cp.Spec.Services.Barbican; bn != nil {
		check("barbican", bn.DatabaseCredentialsMode, cp.DedicatedBarbicanDatabase())
	}
	if nn := cp.Spec.Services.Neutron; nn != nil {
		check("neutron", nn.DatabaseCredentialsMode, cp.DedicatedNeutronDatabase())
	}

	return allErrs
}

// validateKeystoneMode enforces the External-mode validation matrix. It mirrors
// the type-level CEL rules on ServiceKeystoneSpec as defense-in-depth for callers
// that bypass CRD schema admission (the same discipline as the release/database
// mirrors above) AND adds the cross-field rules CEL cannot express — the ones
// spanning spec.infrastructure and spec.services.{keystone,horizon}, which live
// in the webhook per the established CEL-vs-webhook split.
//
//   - External mode: metadata.name must leave room for the identity Endpoint
//     import CR names the mode composes from it; services.keystone.external is
//     required (with an http(s) authURL and a non-empty caBundleSecretRef.name when
//     the ref is set); the managed-only Keystone fields (replicas, image,
//     policyOverrides, rotationInterval, gateway, publicEndpoint) are forbidden;
//     spec.infrastructure is forbidden (phase 2 will relax this to optional) and so
//     is services.horizon (P2 — Horizon needs its own External-mode design).
//   - Not External (Managed, unset mode, or unset keystone): services.keystone.external
//     is forbidden and spec.infrastructure is required — preserving today's
//     contract now that the Go field is an optional pointer.
func validateKeystoneMode(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	if cp.IsExternalKeystone() {
		ks := cp.Spec.Services.Keystone
		ksPath := specPath.Child("services", "keystone")

		// External mode imports one identity Endpoint per interface, unconditionally.
		// Nothing bounds the ControlPlane's own name below 253, so the composed child
		// name can overflow a CR admission already accepted, and ensureExternalCatalogImports
		// then wedges in CatalogReady=False/ImportError backoff on an apiserver Invalid
		// the operator never asked for.
		if n := len(cp.Name) + identityImportChildNameOverhead; n > maxObjectNameBytes {
			allErrs = append(allErrs, field.Invalid(field.NewPath("metadata", "name"), cp.Name, fmt.Sprintf(
				"the identity Endpoint import CR name would be %d bytes; shorten the ControlPlane name "+
					"so it stays within the %d-byte Kubernetes object-name limit", n, maxObjectNameBytes,
			)))
		}

		if ks.External == nil {
			allErrs = append(allErrs, field.Required(ksPath.Child("external"),
				"external is required when services.keystone.mode is External"))
		} else {
			switch ks.External.AuthURL {
			case "":
				allErrs = append(allErrs, field.Required(ksPath.Child("external", "authURL"),
					"authURL is required when services.keystone.mode is External"))
			default:
				authURLPath := ksPath.Child("external", "authURL")
				if _, err := validateHTTPURL(authURLPath, ks.External.AuthURL); err != nil {
					allErrs = append(allErrs, err)
				} else if len(ks.External.AuthURL) > maxExternalAuthURLBytes {
					allErrs = append(allErrs, field.Invalid(authURLPath, ks.External.AuthURL,
						fmt.Sprintf("must be at most %d bytes", maxExternalAuthURLBytes)))
				}
			}
			if ref := ks.External.CABundleSecretRef; ref != nil {
				if ref.Name == "" {
					allErrs = append(allErrs, field.Required(ksPath.Child("external", "caBundleSecretRef", "name"),
						"must be set when caBundleSecretRef is configured"))
				}
				// A CA bundle is only ever consulted during a TLS handshake, and a
				// plaintext endpoint never performs one. Accepting the pair would hand
				// the operator full positive confirmation that trust is enforced —
				// readKeystoneCABundle blocks the mint on WaitingForCABundle until the
				// Secret exists, setCACertKey projects `cacert` into both credentials
				// Secrets — while buildPasswordCloudsYAML renders the admin password
				// next to an http:// auth_url and K-ORC POSTs it in the clear on every
				// mint and re-mint. Reject the combination rather than silently voiding
				// the bundle. Plain http:// WITHOUT a caBundleSecretRef stays admissible:
				// it claims no transport security, so it misleads nobody.
				if externalAuthURLIsPlaintext(ks.External.AuthURL) {
					allErrs = append(allErrs, field.Invalid(ksPath.Child("external", "authURL"), ks.External.AuthURL,
						"must use scheme https when caBundleSecretRef is set: a plaintext endpoint never "+
							"performs the TLS handshake the CA bundle would verify"))
				}
			}
			if ks.External.Catalog != nil {
				allErrs = append(allErrs,
					validateExternalCatalog(ksPath.Child("external", "catalog"), ks.External.Catalog)...)
			}
		}

		// Managed-only Keystone fields are forbidden in External mode: no Keystone
		// workload is deployed, and per P2 catalog advertisement (publicEndpoint) is
		// owned by the W5 catalog imports.
		if ks.Replicas != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("replicas"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.Image != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("image"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.PolicyOverrides != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("policyOverrides"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.RotationInterval != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("rotationInterval"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.Gateway != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("gateway"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.PublicEndpoint != "" {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("publicEndpoint"),
				"forbidden when services.keystone.mode is External (catalog advertisement is owned by the External Keystone)"))
		}
		if ks.FederationProxyImage != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("federationProxyImage"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed, so there is no sidecar to image)"))
		}
		if ks.DedicatedBackingServices != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("dedicatedBackingServices"),
				"forbidden when services.keystone.mode is External (no backing services are provisioned at all)"))
		}
		if ks.Namespace != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("namespace"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed, so there is nothing to place)"))
		}
		if ks.TargetClusterRef != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("targetClusterRef"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed, so there is nothing to place)"))
		}
		if ks.CABundleSecretRef != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("caBundleSecretRef"),
				"forbidden when services.keystone.mode is External (use services.keystone.external.caBundleSecretRef, "+
					"the bundle that verifies the external endpoint)"))
		}
		if ks.DatabaseCredentialsMode != "" {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("databaseCredentialsMode"),
				"forbidden when services.keystone.mode is External (no managed database is provisioned, so there is no credentials mode to override)"))
		}
		if ks.ExtraConfig != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("extraConfig"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed, so there is no config to render)"))
		}

		// Cross-field rules CEL cannot express.
		if cp.Spec.Infrastructure != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("infrastructure"),
				"forbidden when services.keystone.mode is External (phase 2 will relax this to optional)"))
		}
		if cp.Spec.Services.Horizon != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("services", "horizon"),
				"forbidden when services.keystone.mode is External (Horizon needs its own External-mode design)"))
		}
		if cp.Spec.Services.Glance != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("services", "glance"),
				"forbidden when services.keystone.mode is External (Glance needs its own External-mode design)"))
		}
		if cp.Spec.Services.Placement != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("services", "placement"),
				"forbidden when services.keystone.mode is External (Placement needs its own External-mode design)"))
		}
		if cp.Spec.Services.Barbican != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("services", "barbican"),
				"forbidden when services.keystone.mode is External (Barbican needs its own External-mode design)"))
		}
		if cp.Spec.Services.Neutron != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("services", "neutron"),
				"forbidden when services.keystone.mode is External (Neutron needs its own External-mode design)"))
		}

		return allErrs
	}

	// Not External (Managed, unset mode, or unset keystone service).
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.External != nil {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("services", "keystone", "external"),
			"may only be set when services.keystone.mode is External",
		))
	}
	if cp.Spec.Infrastructure == nil {
		allErrs = append(allErrs, field.Required(specPath.Child("infrastructure"),
			"is required unless services.keystone.mode is External"))
	}

	// Defense-in-depth federationProxyImage checks alongside the
	// commonv1.ImageSpec markers. The value is projected verbatim onto the
	// Keystone child's spec.federation.proxyImage, whose own webhook enforces
	// the same rules — rejecting here surfaces the error on the ControlPlane
	// the operator actually edits, rather than as an opaque
	// KeystoneProjectionRejected condition.
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.FederationProxyImage != nil {
		imgPath := specPath.Child("services", "keystone", "federationProxyImage")
		if ks.FederationProxyImage.Repository == "" {
			allErrs = append(allErrs, field.Required(imgPath.Child("repository"),
				"federationProxyImage.repository must be set"))
		}
		if (ks.FederationProxyImage.Tag != "") == (ks.FederationProxyImage.Digest != "") {
			allErrs = append(allErrs, field.Invalid(imgPath, ks.FederationProxyImage,
				"exactly one of federationProxyImage.tag or federationProxyImage.digest must be set"))
		}
	}

	return allErrs
}

// newInvalidIfErrs wraps a non-empty field.ErrorList in an apierrors.NewInvalid
// for the ControlPlane GroupKind, or returns nil when there are no errors. It is
// the single point where the validating webhook turns accumulated field errors
// into the admission response, so ValidateCreate and ValidateUpdate share an
// identical error shape.
func newInvalidIfErrs(cp *ControlPlane, allErrs field.ErrorList) error {
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "ControlPlane"},
		cp.Name,
		allErrs,
	)
}

// validateImmutable accumulates errors for every create-only-immutable field
// that changed between oldObj and newObj (#476). The validating webhook is the
// load-bearing mechanism here because the affected leaves live in the shared
// commonv1.DatabaseSpec/CacheSpec types, which the keystone operator reuses and
// which therefore must not carry c5c3-specific CEL immutability markers.
//
//   - The create-only leaves of every database/cache instance — the shared block
//     and each per-service dedicated one — via validateDatabaseImmutable /
//     validateCacheImmutable, and the shared<->dedicated presence freeze via
//     validateDedicatedBackingServicesImmutable.
//   - A cloudCredentialsRef.secretName change re-points the K-ORC clouds.yaml
//     projection and leaks the previously-named ExternalSecret.
//   - The region (spec.region) is projected verbatim into the Keystone child's
//     now-immutable spec.bootstrap.region (#466). Changing it here would make the
//     next reconcile attempt an update the Keystone CEL rule rejects, wedging the
//     loop; rejecting the change at the ControlPlane layer surfaces a clean error
//     instead.
//
// keystoneModeString returns cp's keystone service mode as a string for use in a
// transition-gating error message, or "unset" when the service block is absent.
func keystoneModeString(cp *ControlPlane) string {
	if ks := cp.Spec.Services.Keystone; ks != nil {
		return string(ks.Mode)
	}
	return "unset"
}

// validate() already enforces the database/cache XOR (exactly one of clusterRef
// or host/servers), so clusterRef nil-ness is an unambiguous mode discriminator
// here.
//
// It also gates the keystone MODE transition. This is webhook-only for the same
// reason as the leaves above but one level up: the rule is cross-field over the
// OLD and NEW objects (a comparison CEL x-kubernetes-validations cannot express),
// and — unlike the immutable leaves — External->Managed must become a *gated*
// takeover in phase 3, so both directions are rejected with distinct messages
// rather than marked immutable (an immutable marker could never be relaxed to a
// gated transition later).
func validateImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList

	// Keystone mode transition gating. Managed->External is rejected outright
	// (adoption of an existing installation must be a fresh External-mode
	// ControlPlane, not an in-place flip of a live one). External->Managed (or
	// away from External by removing the service) is rejected with a distinct
	// message naming the reserved phase-3 takeover, so the direction stays a
	// deliberate future transition rather than a hard immutable field.
	oldExternal := oldObj.IsExternalKeystone()
	newExternal := newObj.IsExternalKeystone()
	modePath := field.NewPath("spec", "services", "keystone", "mode")
	switch {
	case !oldExternal && newExternal:
		allErrs = append(allErrs, field.Invalid(modePath, string(KeystoneModeExternal),
			"keystone mode cannot be changed to External on an existing ControlPlane; "+
				"create a new External-mode ControlPlane to adopt an existing installation"))
	case oldExternal && !newExternal:
		allErrs = append(allErrs, field.Invalid(modePath, keystoneModeString(newObj),
			"switching an External-mode ControlPlane to Managed is not yet supported; "+
				"the managed takeover is reserved as the gated phase-3 transition"))
	}

	// Infrastructure presence flip (defense-in-depth for webhook-bypassed states,
	// e.g. a direct etcd write). Adding or removing the block on UPDATE is an
	// infrastructure-vs-mode transition that the mode gating above already covers
	// for a mode change; freezing presence independently rejects a bare
	// add/remove that leaves the mode unchanged.
	if (oldObj.Spec.Infrastructure == nil) != (newObj.Spec.Infrastructure == nil) {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "infrastructure"), newObj.Spec.Infrastructure,
			"infrastructure presence is immutable (adding or removing the block after creation is not permitted)",
		))
	}

	// spec.infrastructure is an optional pointer now (External keystone mode omits
	// it). The database/cache immutability comparisons only apply when the block is
	// present on BOTH revisions — a presence flip (block added or removed) is an
	// infrastructure-vs-mode transition governed by the External-mode gating, not a
	// database/cache field mutation. When either side is nil there are no managed
	// clusterRef/name/replicas/storageSize leaves to freeze. Messaging is compared
	// inside the same both-present guard; the block-level presence flip of
	// messaging itself is what validateMessagingImmutable freezes. The
	// cloudCredentialsRef.secretName and region immutability checks below are
	// mode-independent and always run.
	if oldInfra, newInfra := oldObj.Spec.Infrastructure, newObj.Spec.Infrastructure; oldInfra != nil && newInfra != nil {
		specPath := field.NewPath("spec", "infrastructure")
		allErrs = append(allErrs, validateDatabaseImmutable(specPath.Child("database"), &oldInfra.Database, &newInfra.Database)...)
		allErrs = append(allErrs, validateCacheImmutable(specPath.Child("cache"), &oldInfra.Cache, &newInfra.Cache)...)
		allErrs = append(allErrs, validateMessagingImmutable(specPath.Child("messaging"), oldInfra.Messaging, newInfra.Messaging)...)
	}

	allErrs = append(allErrs, validateDedicatedBackingServicesImmutable(oldObj, newObj)...)
	allErrs = append(allErrs, validateServiceNamespacesImmutable(oldObj, newObj)...)
	allErrs = append(allErrs, validateServiceTargetClustersImmutable(oldObj, newObj)...)
	allErrs = append(allErrs, validateBarbicanSecretStoreImmutable(oldObj, newObj)...)

	oldSecretName := oldObj.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	newSecretName := newObj.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if oldSecretName != newSecretName {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "korc", "adminCredential", "cloudCredentialsRef", "secretName"),
			newSecretName, "cloudCredentialsRef.secretName is immutable",
		))
	}

	if oldObj.Spec.Region != newObj.Spec.Region {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "region"),
			newObj.Spec.Region, "region is immutable",
		))
	}

	return allErrs
}

// validateDatabaseImmutable freezes the create-only leaves of ONE database
// instance — the shared spec.infrastructure.database and every per-service
// dedicated one alike, since each is projected onto the same MariaDB and
// Keystone-child fields:
//
//   - the MODE (managed clusterRef vs brownfield host): flipping it leaves the
//     previously-projected MariaDB child (and its credential ExternalSecret)
//     running and owned until the ControlPlane is deleted;
//   - a managed clusterRef.NAME change re-points the projection at a new child
//     and orphans the old one the same way;
//   - the database NAME, which is projected verbatim into the consuming service
//     child's now-immutable spec.database.database — renaming it here would make
//     the next reconcile attempt an update the child's CEL rule rejects, wedging
//     the loop behind a KeystoneProjectionRejected condition;
//   - replicas, which drives the owned MariaDB's replica count and the derived
//     Galera topology, so an in-place edit would toggle Galera off or scale a
//     running Galera cluster down — destructive on a live cluster;
//   - storageSize, which the mariadb-operator refuses to change on a live CR. The
//     comparison normalizes "" to the default the fresh-create projection
//     actually provisions (effectiveStorageSize), so a ControlPlane stored before
//     the field existed can migrate once to an explicit default while any OTHER
//     value is still rejected as a resize.
//
// The checks are webhook-only: the leaves live in the shared commonv1.DatabaseSpec,
// which the keystone operator reuses and which therefore must not carry
// c5c3-specific CEL immutability markers.
func validateDatabaseImmutable(fldPath *field.Path, oldDB, newDB *commonv1.DatabaseSpec) field.ErrorList {
	var allErrs field.ErrorList

	// validate() enforces the database XOR (exactly one of clusterRef or host), so
	// clusterRef nil-ness is an unambiguous mode discriminator here.
	switch {
	case (oldDB.ClusterRef != nil) != (newDB.ClusterRef != nil):
		allErrs = append(allErrs, field.Invalid(fldPath, *newDB,
			"database mode (managed clusterRef vs brownfield host) is immutable"))
	case oldDB.ClusterRef != nil && newDB.ClusterRef != nil && oldDB.ClusterRef.Name != newDB.ClusterRef.Name:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("clusterRef", "name"),
			newDB.ClusterRef.Name, "managed database clusterRef.name is immutable"))
	}
	if oldDB.Database != newDB.Database {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("database"),
			newDB.Database, "database name is immutable"))
	}
	if oldDB.Replicas != newDB.Replicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("replicas"),
			newDB.Replicas, "database replicas is immutable after creation "+
				"(toggling Galera or scaling down a live cluster is destructive)"))
	}
	if effectiveStorageSize(oldDB.StorageSize) != effectiveStorageSize(newDB.StorageSize) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("storageSize"),
			newDB.StorageSize, "database storageSize is immutable after creation "+
				"(the mariadb-operator rejects resizing a live volume)"))
	}
	return allErrs
}

// validateCacheImmutable freezes the create-only leaves of ONE cache instance —
// the shared spec.infrastructure.cache and every per-service dedicated one alike.
// Only the MODE and the managed clusterRef.name are frozen: replicas stays
// mutable, because ensureMemcached reconciles an owned Memcached's replica count
// in place (scaling a cache loses no data).
func validateCacheImmutable(fldPath *field.Path, oldCache, newCache *commonv1.CacheSpec) field.ErrorList {
	var allErrs field.ErrorList
	switch {
	case (oldCache.ClusterRef != nil) != (newCache.ClusterRef != nil):
		allErrs = append(allErrs, field.Invalid(fldPath, *newCache,
			"cache mode (managed clusterRef vs brownfield servers) is immutable"))
	case oldCache.ClusterRef != nil && newCache.ClusterRef != nil && oldCache.ClusterRef.Name != newCache.ClusterRef.Name:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("clusterRef", "name"),
			newCache.ClusterRef.Name, "managed cache clusterRef.name is immutable"))
	}
	return allErrs
}

// validateMessagingImmutable freezes the shared spec.infrastructure.messaging
// block as a one-way add IN BOTH MODES. Declaring the block on a live
// ControlPlane is always allowed; removing it never is.
//
// The managed half is the direct one: the owned RabbitmqCluster keeps the
// queues, so dropping the block leaves it running and unreferenced. The
// brownfield half provisions nothing at all (managedInfraInstances returns early
// on a nil clusterRef), so on its own the removal strands no state — but
// admitting it turns the mode freeze below into a two-step operation. Null the
// brownfield block (admitted), then re-add it with a clusterRef (admitted by the
// oldM == nil case), and the ControlPlane has reached exactly the state the mode
// freeze exists to reject, without a single admission error: ensureRabbitMQ
// provisions a fresh, empty RabbitmqCluster that every consumer renders a
// transport URL for, while the queues stay on the external broker the secretRef
// named. Neither spec nor status remembers the mode a previous revision
// declared, so the one-step rejection is only worth having while the two-step
// path is closed too. Re-pointing a brownfield bus at a different broker never
// needed the removal: secretRef stays mutable.
//
// The mode and the managed clusterRef.name are frozen like the cache ones.
// replicas, secretRef and tls stay mutable: ensureRabbitMQ re-projects the
// replica count onto the owned CR on every pass — converging a scale-DOWN by
// recreating the cluster, since the RabbitMQ Cluster Operator refuses an
// in-place shrink — and the brownfield Secret and the client trust are read on
// every reconcile. Webhook-only, like the cache freeze: no CEL transition rule.
func validateMessagingImmutable(fldPath *field.Path, oldM, newM *commonv1.MessagingSpec) field.ErrorList {
	var allErrs field.ErrorList
	switch {
	case oldM == nil:
		// Opt-in on a live ControlPlane.
	case newM == nil:
		allErrs = append(allErrs, field.Invalid(fldPath, newM,
			"spec.infrastructure.messaging cannot be removed once declared: a managed block's "+
				"owned RabbitmqCluster keeps the queues, and dropping a brownfield block would "+
				"launder the immutable messaging mode into a two-step flip; re-point secretRef "+
				"to move to another broker, or delete the ControlPlane to tear the bus down"))
	case (oldM.ClusterRef != nil) != (newM.ClusterRef != nil):
		allErrs = append(allErrs, field.Invalid(fldPath, *newM,
			"messaging mode (managed clusterRef vs brownfield secretRef) is immutable"))
	case oldM.ClusterRef != nil && newM.ClusterRef != nil && oldM.ClusterRef.Name != newM.ClusterRef.Name:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("clusterRef", "name"),
			newM.ClusterRef.Name, "managed messaging clusterRef.name is immutable"))
	}
	return allErrs
}

// serviceDeclaredBefore reports whether oldObj already carried the named service
// before this update: declared in spec (declaredInSpec), or — for a service
// DROPPED from spec whose projected child the ControlPlane preserves by default —
// still reported under that name in status.services.
//
// It is the gate both transition freezes below carve their "this is the service's
// CREATE, not a move" exception on, and the spec half alone is not enough: a
// shared-backed service drops cleanly, so "the service is absent from the old
// spec" conflates a service that never existed with one that was dropped a moment
// ago and is now coming back. Re-adding the latter with a dedicated block (or in
// another namespace) is the transition the freeze forbids — the preserved child
// would be re-pointed at a freshly projected, empty instance, stranding the schema
// it was running on — while re-adding it shared is the same no-op it always was.
//
// status.services is derived from spec, so the controller prunes the entry once it
// has observed the drop: the check holds for as long as the ControlPlane still
// reports the service, which is the window a spec edited back and forth actually
// lands in. Closing it for good needs a durable record of "this service was
// projected", which the API does not carry today.
func serviceDeclaredBefore(oldObj *ControlPlane, declaredInSpec bool, service string) bool {
	if declaredInSpec {
		return true
	}
	for _, s := range oldObj.Status.Services {
		if s.Name == service {
			return true
		}
	}
	return false
}

// validateBarbicanSecretStoreImmutable freezes the properties of
// services.barbican.secretStore that ADDRESS the secret material already written
// through the store: the mode (dedicated vs external), the KV mountpoint, and —
// on an external store — the server URL and the OpenBao namespace.
//
// They are frozen for the reason the BarbicanSecretStore CRD freezes its own
// spec.openBao.kvMountpoint and instanceRef/server discriminator: material
// written under one mount, on one server, is not reachable under another. The
// CRD's transition rule says "delete and recreate the store instead", and it says
// it to a human who has accepted that consequence. Without this rule the
// reconciler is the one that acts on it — reconcileBarbicanSecretStore sees the
// frozen-field drift, DELETES the live store, and recreates it against the new
// mount or the new server, leaving every previously stored secret at an address
// nobody holds any longer. A dedicated->external flip additionally strands the
// whole OpenBao ensemble the ControlPlane provisioned: the instance, its raft PVC
// with the secrets still in it, the seal key beside them, and the cluster-scoped
// auth-delegator binding, none of which any convergence loop reaps once the
// ControlPlane stops asking for a dedicated store.
//
// The external server ADDRESS is frozen here and nowhere else. The child CRD's
// transition rules cover the discriminator and the mount but not
// spec.openBao.server.url, and barbicanSecretStoreFor copies the URL straight
// through, so barbicanSecretStoreImmutableDrift reads no drift and a new address
// is SSA-updated into the live store in place: no delete, no recreation, no
// condition to show for it, and from the next barbican-operator pass every store
// and retrieve goes to the new server while the material on the old one is
// unreachable. An identity holding nothing but `patch controlplanes` must not be
// able to re-point a live key manager that way. The OpenBao namespace is frozen
// on the same argument: it scopes every request, so material written under the
// old one is not reachable under a new one.
//
// Only the credentials Secret and the CA bundle stay mutable: both
// re-authenticate the SAME store against the SAME server, which is what a
// rotation needs and what the CRD admits too.
//
// The freeze is gated on serviceDeclaredBefore rather than on a bare nil check,
// for the reason its two siblings are: dropping services.barbican preserves the
// child, the store and the whole OpenBao ensemble (nothing reaps them without the
// deletion annotation), so a drop-and-re-add would otherwise slip a frozen
// transition past a two-revision comparison. A re-add inside that window carries
// no old block to compare against and is refused outright. The update that FIRST
// declares services.barbican — on a ControlPlane that never reported it — is
// free to name any mode, and the update that drops the service leaves nothing to
// compare against.
func validateBarbicanSecretStoreImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	oldBN, newBN := oldObj.Spec.Services.Barbican, newObj.Spec.Services.Barbican
	if newBN == nil || !serviceDeclaredBefore(oldObj, oldBN != nil, "barbican") {
		return nil
	}

	storePath := field.NewPath("spec", "services", "barbican", "secretStore")
	if oldBN == nil {
		return field.ErrorList{field.Invalid(storePath, newBN.SecretStore,
			"the Barbican service was dropped from spec while the ControlPlane still reports it: its secret store, "+
				"the material behind it and — on the dedicated mode — the OpenBao instance holding that material are "+
				"all still live, and the revision that addressed them is gone, so this store cannot be checked "+
				"against it. Wait until the ControlPlane stops reporting barbican under status.services — one "+
				"reconcile pass after the operator observes the drop — and declare the service again then")}
	}

	var allErrs field.ErrorList

	if (oldBN.SecretStore.Dedicated != nil) != (newBN.SecretStore.Dedicated != nil) {
		allErrs = append(allErrs, field.Invalid(storePath, newBN.SecretStore,
			"the secret-store mode (dedicated vs external) is immutable: the secret material barbican has already "+
				"stored lives on the server the current mode names, and switching modes would re-point the store at "+
				"a different server while stranding that material — and, leaving dedicated, the OpenBao instance and "+
				"raft volume holding it. Delete and recreate the ControlPlane's Barbican service to change it"))
	}

	if oldMount, newMount := effectiveBarbicanKVMountpoint(oldBN), effectiveBarbicanKVMountpoint(newBN); oldMount != newMount {
		allErrs = append(allErrs, field.Invalid(storePath.Child("external", "kvMountpoint"), newMount,
			fmt.Sprintf("kvMountpoint is immutable (was %q): the secret material already written under the old "+
				"mount is not reachable under a new one", oldMount)))
	}

	if oldExt, newExt := oldBN.SecretStore.External, newBN.SecretStore.External; oldExt != nil && newExt != nil {
		if oldExt.URL != newExt.URL {
			allErrs = append(allErrs, field.Invalid(storePath.Child("external", "url"), newExt.URL,
				fmt.Sprintf("url is immutable (was %q): the secret material already written to the old server is "+
					"not reachable on a new one, and the update would re-point the live store at the new address "+
					"in place. Delete and recreate the ControlPlane's Barbican service to change it", oldExt.URL)))
		}
		if oldExt.Namespace != newExt.Namespace {
			allErrs = append(allErrs, field.Invalid(storePath.Child("external", "namespace"), newExt.Namespace,
				fmt.Sprintf("namespace is immutable (was %q): the OpenBao namespace scopes every request, so the "+
					"secret material already written under the old one is not reachable under a new one",
					oldExt.Namespace)))
		}
	}

	return allErrs
}

// effectiveBarbicanKVMountpoint returns the KV mount the store projects, with the
// external block's default filled in. A dedicated store names no mount of its own:
// the self-init contract provisions barbican/ and the BarbicanSecretStore CRD pins
// managed stores to it, which is the same value DefaultBarbicanKVMountpoint holds.
func effectiveBarbicanKVMountpoint(bn *ServiceBarbicanSpec) string {
	ext := bn.SecretStore.External
	if ext == nil || ext.KVMountpoint == "" {
		return DefaultBarbicanKVMountpoint
	}
	return ext.KVMountpoint
}

// dedicatedTransitionMessage is the single message every shared<->dedicated
// presence freeze reports, so the sites (the per-service block and each
// backing-service class within it, for each service) cannot drift apart.
const dedicatedTransitionMessage = "switching a service between shared and dedicated backing services on a live " +
	"ControlPlane is not yet supported; remove and recreate the ControlPlane to change it"

// validateDedicatedBackingServicesImmutable freezes the per-service dedicated
// backing-service declarations on UPDATE, in two layers:
//
//   - PRESENCE, both of the per-service block and of each backing-service class
//     within it. An in-place shared->dedicated flip (or back) would re-point the
//     consuming child's database/cache at a different instance — and those child
//     leaves (spec.database.database, spec.bootstrap.*) are themselves immutable,
//     so the projection would be rejected and wedge the reconcile behind a
//     KeystoneProjectionRejected condition — while the previously-provisioned
//     instance keeps running, owned, with no migration of the data on it.
//   - The create-only LEAVES of a dedicated instance that stays declared, via the
//     same validateDatabaseImmutable / validateCacheImmutable rules the shared
//     block gets.
//
// The presence freeze is deliberately webhook-only, with NO CEL transition rule:
// switching an existing service between shared and dedicated (with or without
// data migration) is a reserved future feature, and an immutable CEL marker could
// never be relaxed to a gated transition later. This mirrors the keystone-mode
// transition gating above.
//
// Every arm is gated on serviceDeclaredBefore. The accessors conflate "the
// service is not declared at all" with "the service shares the
// ControlPlane-wide instances", so without the gate an update that ADDS a
// previously-undeclared service with a dedicated block reads as a
// shared->dedicated switch and is rejected with an offer to remove and recreate
// the whole ControlPlane — destroying the databases of the services already
// running on it. Nothing is being switched there: the service is being created,
// exactly as it would have been at ControlPlane create time. This mirrors the
// spec.infrastructure presence-flip carve-out, which likewise compares the
// leaves only when the block is present on BOTH revisions. Dropping a
// dedicated-backed service still hits the freeze; a shared-backed one drops
// cleanly (the per-service deletion annotations govern whether its child comes
// down with it), which is why the gate must not key on spec alone — see
// serviceDeclaredBefore.
func validateDedicatedBackingServicesImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	svcPath := field.NewPath("spec", "services")

	if serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Keystone != nil, "keystone") {
		ksPath := svcPath.Child("keystone", "dedicatedBackingServices")
		oldKS := keystoneDedicatedBlock(oldObj)
		newKS := keystoneDedicatedBlock(newObj)
		if (oldKS == nil) != (newKS == nil) {
			allErrs = append(allErrs, field.Invalid(ksPath, newKS, dedicatedTransitionMessage))
		} else if oldKS != nil && newKS != nil {
			allErrs = append(allErrs, validateDedicatedDatabase(ksPath.Child("database"), oldKS.Database, newKS.Database)...)
			allErrs = append(allErrs, validateDedicatedCache(ksPath.Child("cache"), oldKS.Cache, newKS.Cache)...)
		}
	}

	if serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Horizon != nil, "horizon") {
		hzPath := svcPath.Child("horizon", "dedicatedBackingServices")
		oldHZ := horizonDedicatedBlock(oldObj)
		newHZ := horizonDedicatedBlock(newObj)
		if (oldHZ == nil) != (newHZ == nil) {
			allErrs = append(allErrs, field.Invalid(hzPath, newHZ, dedicatedTransitionMessage))
		} else if oldHZ != nil && newHZ != nil {
			allErrs = append(allErrs, validateDedicatedCache(hzPath.Child("cache"), oldHZ.Cache, newHZ.Cache)...)
		}
	}

	if serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Glance != nil, "glance") {
		glPath := svcPath.Child("glance", "dedicatedBackingServices")
		oldGL := glanceDedicatedBlock(oldObj)
		newGL := glanceDedicatedBlock(newObj)
		if (oldGL == nil) != (newGL == nil) {
			allErrs = append(allErrs, field.Invalid(glPath, newGL, dedicatedTransitionMessage))
		} else if oldGL != nil && newGL != nil {
			allErrs = append(allErrs, validateDedicatedDatabase(glPath.Child("database"), oldGL.Database, newGL.Database)...)
			allErrs = append(allErrs, validateDedicatedCache(glPath.Child("cache"), oldGL.Cache, newGL.Cache)...)
		}
	}

	if serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Placement != nil, "placement") {
		plPath := svcPath.Child("placement", "dedicatedBackingServices")
		oldPL := placementDedicatedBlock(oldObj)
		newPL := placementDedicatedBlock(newObj)
		if (oldPL == nil) != (newPL == nil) {
			allErrs = append(allErrs, field.Invalid(plPath, newPL, dedicatedTransitionMessage))
		} else if oldPL != nil && newPL != nil {
			allErrs = append(allErrs, validateDedicatedDatabase(plPath.Child("database"), oldPL.Database, newPL.Database)...)
			allErrs = append(allErrs, validateDedicatedCache(plPath.Child("cache"), oldPL.Cache, newPL.Cache)...)
		}
	}

	if serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Barbican != nil, "barbican") {
		bnPath := svcPath.Child("barbican", "dedicatedBackingServices")
		oldBN := barbicanDedicatedBlock(oldObj)
		newBN := barbicanDedicatedBlock(newObj)
		if (oldBN == nil) != (newBN == nil) {
			allErrs = append(allErrs, field.Invalid(bnPath, newBN, dedicatedTransitionMessage))
		} else if oldBN != nil && newBN != nil {
			allErrs = append(allErrs, validateDedicatedDatabase(bnPath.Child("database"), oldBN.Database, newBN.Database)...)
			allErrs = append(allErrs, validateDedicatedCache(bnPath.Child("cache"), oldBN.Cache, newBN.Cache)...)
		}
	}

	if serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Neutron != nil, "neutron") {
		nnPath := svcPath.Child("neutron", "dedicatedBackingServices")
		oldNN := neutronDedicatedBlock(oldObj)
		newNN := neutronDedicatedBlock(newObj)
		if (oldNN == nil) != (newNN == nil) {
			allErrs = append(allErrs, field.Invalid(nnPath, newNN, dedicatedTransitionMessage))
		} else if oldNN != nil && newNN != nil {
			allErrs = append(allErrs, validateDedicatedDatabase(nnPath.Child("database"), oldNN.Database, newNN.Database)...)
			allErrs = append(allErrs, validateDedicatedCache(nnPath.Child("cache"), oldNN.Cache, newNN.Cache)...)
		}
	}

	return allErrs
}

// validateDedicatedDatabase freezes one dedicated database class: its presence
// (adding or removing the class is the same shared<->dedicated transition as
// adding or removing the whole block) and, when it stays declared, its
// create-only leaves.
func validateDedicatedDatabase(fldPath *field.Path, oldDB, newDB *commonv1.DatabaseSpec) field.ErrorList {
	switch {
	case (oldDB == nil) != (newDB == nil):
		return field.ErrorList{field.Invalid(fldPath, newDB, dedicatedTransitionMessage)}
	case oldDB == nil:
		return nil
	}
	return validateDatabaseImmutable(fldPath, oldDB, newDB)
}

// validateDedicatedCache is the cache twin of validateDedicatedDatabase.
func validateDedicatedCache(fldPath *field.Path, oldCache, newCache *commonv1.CacheSpec) field.ErrorList {
	switch {
	case (oldCache == nil) != (newCache == nil):
		return field.ErrorList{field.Invalid(fldPath, newCache, dedicatedTransitionMessage)}
	case oldCache == nil:
		return nil
	}
	return validateCacheImmutable(fldPath, oldCache, newCache)
}

// serviceNamespaceTransitionMessage is the single message every per-service
// namespace freeze reports, so the sites (the block's presence, its name, its
// lifecycle, for each service) cannot drift apart.
const serviceNamespaceTransitionMessage = "the namespace a service is placed in is immutable; moving a live service " +
	"across namespaces would leave its backing services, its secret store, and the credential material scoped to " +
	"the old namespace behind with no migration path — remove and recreate the ControlPlane to change it"

// validateServiceNamespacesImmutable freezes the per-service namespace
// assignments on UPDATE: the PRESENCE of the block, the namespace NAME, and the
// LIFECYCLE.
//
//   - Presence and name: the namespace is where the service's MariaDB/Memcached,
//     its per-namespace tenant SecretStore, and every OpenBao path scoped by it
//     (bootstrap/{ns}/…, the keystone-{ns} database-engine role) live. Re-pointing
//     a live service at another namespace would strand all of it — and the child's
//     own database/bootstrap leaves are themselves immutable, so the re-projection
//     would be rejected and wedge the reconcile behind a ProjectionRejected
//     condition anyway.
//   - Lifecycle: flipping External->Managed would have the operator claim, and at
//     teardown DELETE, a namespace it was told it does not own; flipping
//     Managed->External would abandon a namespace it created.
//
// The freeze is deliberately webhook-only, with NO CEL transition rule: moving a
// service between namespaces (with the data migration that implies) is a reserved
// future feature, and an immutable CEL marker could never be relaxed to a gated
// transition later. This mirrors the dedicated-backing-services freeze above.
func validateServiceNamespacesImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	svcPath := field.NewPath("spec", "services")

	// declaredBefore (serviceDeclaredBefore) gates the whole freeze: assigning a
	// namespace to a service the ControlPlane did not carry on the old revision is
	// that service's CREATE, not a move. There is no live service, no backing
	// service, and no credential material scoped to an old namespace — so the
	// "remove and recreate the ControlPlane" remedy the message offers would destroy
	// the services already running on it to onboard one more.
	//
	// The carve-out does not weaken the tenant-isolation check: because it lets an
	// update introduce a namespace CLAIM, ValidateUpdate re-runs
	// validateNamespaceClaims whenever the claim set changed, so a newly assigned
	// namespace is still checked against every other ControlPlane's claims.
	freeze := func(path *field.Path, declaredBefore bool, oldNS, newNS *ServiceNamespaceSpec) {
		if !declaredBefore {
			return
		}
		switch {
		case (oldNS == nil) != (newNS == nil):
			allErrs = append(allErrs, field.Invalid(path, newNS, serviceNamespaceTransitionMessage))
		case oldNS == nil:
			return
		}
		if oldNS == nil || newNS == nil {
			return
		}
		if oldNS.Name != newNS.Name {
			allErrs = append(allErrs, field.Invalid(path.Child("name"), newNS.Name, serviceNamespaceTransitionMessage))
		}
		if oldNS.Lifecycle != newNS.Lifecycle {
			allErrs = append(allErrs, field.Invalid(path.Child("lifecycle"), newNS.Lifecycle,
				serviceNamespaceTransitionMessage))
		}
	}

	freeze(svcPath.Child("keystone", "namespace"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Keystone != nil, "keystone"),
		keystoneNamespaceBlock(oldObj), keystoneNamespaceBlock(newObj))
	freeze(svcPath.Child("horizon", "namespace"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Horizon != nil, "horizon"),
		horizonNamespaceBlock(oldObj), horizonNamespaceBlock(newObj))
	freeze(svcPath.Child("glance", "namespace"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Glance != nil, "glance"),
		glanceNamespaceBlock(oldObj), glanceNamespaceBlock(newObj))
	freeze(svcPath.Child("placement", "namespace"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Placement != nil, "placement"),
		placementNamespaceBlock(oldObj), placementNamespaceBlock(newObj))
	freeze(svcPath.Child("barbican", "namespace"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Barbican != nil, "barbican"),
		barbicanNamespaceBlock(oldObj), barbicanNamespaceBlock(newObj))
	freeze(svcPath.Child("neutron", "namespace"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Neutron != nil, "neutron"),
		neutronNamespaceBlock(oldObj), neutronNamespaceBlock(newObj))

	return allErrs
}

// validateServiceTargetClustersImmutable freezes the per-service target-cluster
// assignment on UPDATE. Adding a ref, removing it, or renaming it is rejected
// through the shared validator the workload CRDs enforce on their own
// spec.targetClusterRef (validation.TargetClusterRefImmutable), so a client
// reads the same wording whichever layer rejects the edit.
//
// It is the namespace freeze one level out. Re-pointing a live service at
// another cluster strands everything the previous one holds: the workload, the
// database, the tenant store, and the credential material in it, none of which
// the reconcile that follows the edit moves or reaps. The child's own
// spec.targetClusterRef carries a CEL transition rule, so the re-projection
// would be rejected and wedge the reconcile behind a ProjectionRejected
// condition anyway.
//
// The freeze is deliberately webhook-only, with NO CEL transition rule: moving a
// service between clusters (with the data migration that implies) is a reserved
// future feature, and an immutable CEL marker could never be relaxed to a gated
// transition later. This mirrors the namespace freeze above.
//
// serviceDeclaredBefore gates it exactly as it gates that freeze: placing a
// service the ControlPlane did not carry on the old revision is that service's
// CREATE, not a move, so there is no running workload anywhere to strand.
func validateServiceTargetClustersImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	svcPath := field.NewPath("spec", "services")

	freeze := func(path *field.Path, declaredBefore bool, oldRef, newRef *commonv1.TargetClusterRefSpec) {
		if !declaredBefore {
			return
		}
		allErrs = append(allErrs, validation.TargetClusterRefImmutable(path, oldRef, newRef)...)
	}

	freeze(svcPath.Child("keystone", "targetClusterRef"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Keystone != nil, "keystone"),
		oldObj.KeystoneTargetClusterRef(), newObj.KeystoneTargetClusterRef())
	freeze(svcPath.Child("horizon", "targetClusterRef"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Horizon != nil, "horizon"),
		oldObj.HorizonTargetClusterRef(), newObj.HorizonTargetClusterRef())
	freeze(svcPath.Child("glance", "targetClusterRef"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Glance != nil, "glance"),
		oldObj.GlanceTargetClusterRef(), newObj.GlanceTargetClusterRef())
	freeze(svcPath.Child("placement", "targetClusterRef"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Placement != nil, "placement"),
		oldObj.PlacementTargetClusterRef(), newObj.PlacementTargetClusterRef())
	freeze(svcPath.Child("barbican", "targetClusterRef"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Barbican != nil, "barbican"),
		oldObj.BarbicanTargetClusterRef(), newObj.BarbicanTargetClusterRef())
	freeze(svcPath.Child("neutron", "targetClusterRef"),
		serviceDeclaredBefore(oldObj, oldObj.Spec.Services.Neutron != nil, "neutron"),
		oldObj.NeutronTargetClusterRef(), newObj.NeutronTargetClusterRef())

	return allErrs
}

// effectiveStorageSize resolves an empty database.storageSize to the default the
// c5c3 fresh-create projection actually provisions (DefaultDatabaseStorageSize),
// so validateImmutable compares the sizes the live MariaDB runs at rather than
// the raw spec strings. This is what lets a pre-existing ControlPlane (stored
// with "" before the field existed) migrate once to an explicit default.
func effectiveStorageSize(size string) string {
	if size == "" {
		return DefaultDatabaseStorageSize
	}
	return size
}

// validateReleaseNotDowngraded rejects an openStackRelease downgrade on UPDATE.
// OpenStack/Keystone DB migrations are forward-only (keystone-manage db_sync has
// no down-migration path), so re-pointing a live control plane at an older
// release would project an older image whose schema is behind the already-migrated
// database -- an unrecoverable state. Upgrades and same-release updates are
// allowed. The shared release parser compares the (year, minor) integer tuples
// rather than the raw strings, so ordering stays correct even for hypothetical
// multi-digit minors where lexicographic comparison would silently invert. A
// release release.ParseRelease cannot parse (malformed, or a minor outside the
// two-releases-per-year OpenStack cadence) is left to validate()'s pattern
// check rather than mis-parsed here, so a malformed value yields the pattern
// error alone instead of a confusing downgrade message.
func validateReleaseNotDowngraded(oldObj, newObj *ControlPlane) field.ErrorList {
	oldRel, errOld := release.ParseRelease(oldObj.Spec.OpenStackRelease)
	newRel, errNew := release.ParseRelease(newObj.Spec.OpenStackRelease)
	if errOld != nil || errNew != nil {
		return nil
	}
	if release.IsDowngrade(oldRel, newRel) {
		return field.ErrorList{field.Invalid(
			field.NewPath("spec", "openStackRelease"),
			newRel.Raw,
			fmt.Sprintf("openStackRelease downgrade from %q to %q is not permitted; Keystone DB migrations are not reversible", oldRel.Raw, newRel.Raw),
		)}
	}
	return nil
}

// validateUniqueInNamespace enforces the one-ControlPlane-per-namespace contract
// It lists existing ControlPlanes in the new object's
// namespace; any pre-existing CR (len >= 1, since the object under admission is
// not yet persisted) makes this CREATE a Forbidden error naming the incumbent.
// The List goes through the injected uncached API reader (mgr.GetAPIReader() in
// production), so the check cannot admit a second CR off a stale or still-empty
// informer cache. The reconciler's duplicate-ControlPlane guard
// (duplicateControlPlaneIncumbent in operators/c5c3/internal/controller) is the
// defense-in-depth for CREATEs that race within the API server itself or bypass
// the webhook entirely.
//
// DECISION: when w.Client is nil (spec-level unit tests that construct a bare
// &ControlPlaneWebhook{}, or any caller that did not inject a client) the check
// is skipped rather than panicking. Production and envtest wiring always inject
// mgr.GetAPIReader() (operators/c5c3/main.go, integration_test.go), so the
// guard never trips at runtime; it only keeps the spec-validation unit tests
// client-free.
func (w *ControlPlaneWebhook) validateUniqueInNamespace(ctx context.Context, obj *ControlPlane) error {
	if w.Client == nil {
		return nil
	}
	var existing ControlPlaneList
	if err := w.Client.List(ctx, &existing, client.InNamespace(obj.Namespace)); err != nil {
		return apierrors.NewInternalError(
			fmt.Errorf("listing ControlPlanes in namespace %q to enforce one-per-namespace: %w", obj.Namespace, err),
		)
	}
	if len(existing.Items) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "ControlPlane"},
		obj.Name,
		field.ErrorList{field.Forbidden(
			field.NewPath("metadata", "namespace"),
			fmt.Sprintf("only one ControlPlane is permitted per namespace; %q already exists in namespace %q",
				existing.Items[0].Name, obj.Namespace),
		)},
	)
}
