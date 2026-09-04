// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".spec.openStackRelease"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ControlPlane is the Schema for the controlplanes API. It is the
// top-level aggregate that projects an OpenStack control plane: it owns shared
// infrastructure references (database, cache) and a curated set of service
// specs (today: keystone) that the reconciler (L2) materializes into the
// per-service CRs.
type ControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControlPlaneSpec   `json:"spec,omitempty"`
	Status ControlPlaneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControlPlaneList contains a list of ControlPlane.
type ControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlane `json:"items"`
}

// ControlPlaneSpec defines the desired state of a ControlPlane.
type ControlPlaneSpec struct {
	// OpenStackRelease is the OpenStack release the control plane targets,
	// e.g. "2025.2". The reconciler (L2) projects this into each service CR's
	// image tag. The pattern matches the OpenStack date-based release scheme
	// (YYYY.N where N is 1 or 2 — the two-releases-per-year cadence, e.g.
	// 2024.1, 2025.2). The [12] minor class keeps this CRD pattern, the
	// webhook's controlPlaneReleaseRegexp, and release.ParseRelease in agreement
	// so a non-cadence minor (e.g. 2025.9) is rejected at every layer.
	//
	// The field stays required in both keystone modes. In External mode it is
	// ADVISORY: no images are deployed, so the value only needs to match the
	// external installation's release at the phase-3 managed takeover — until
	// then it is recorded but unused by the External-mode reconciler.
	// +kubebuilder:validation:Pattern=`^\d{4}\.[12]$`
	OpenStackRelease string `json:"openStackRelease"`

	// Region is the OpenStack region name applied across the control plane.
	// DECISION (plan decision #4): defaults to "RegionOne" via both the
	// CRD schema default (normal admission path) and the defaulting webhook
	// (callers that bypass the CRD default), mirroring BootstrapSpec.Region in the
	// keystone operator.
	// +kubebuilder:default="RegionOne"
	// +optional
	Region string `json:"region,omitempty"`

	// Infrastructure declares the shared backing services (database, cache)
	// that the control plane's services connect to.
	//
	// Required in Managed keystone mode (or when services.keystone is unset) and
	// forbidden in External keystone mode. Preserving today's contract, the
	// validating webhook rejects a non-External ControlPlane without it and the
	// defaulting webhook materializes the omitted block; an External-mode
	// ControlPlane manages identity against a pre-existing Keystone and provisions
	// no backing services, so infrastructure is forbidden (phase 2 will relax this
	// to optional). The Go field is a pointer (hence +optional at the CRD schema
	// layer) so External mode can omit it; the mode-conditional required/forbidden
	// rules live in the validating webhook because CEL cannot express a
	// cross-field rule spanning spec.infrastructure and spec.services.keystone.
	// +optional
	Infrastructure *InfrastructureSpec `json:"infrastructure,omitempty"`

	// SecretStoreRef selects the External Secrets store the ControlPlane and its
	// service children route ExternalSecrets and PushSecrets through. When
	// omitted the operator PROVISIONS a per-tenant namespaced store
	// (openbao-tenant-store) in the control plane's namespace and defaults the
	// control plane and its Keystone/Horizon children onto it, so every control
	// plane reaches OpenBao as its own tenant identity — the enforced default that
	// makes OpenBao itself, not a naming convention, isolate one control plane's
	// secret material from another. Set this field to override that default with an
	// explicit store (e.g. a namespaced store you manage, or the shared
	// cluster-scoped openbao-cluster-store); the operator then provisions nothing
	// and uses the store you name. The reference is projected onto the Keystone and
	// Horizon children, so setting it here is the single place operators configure
	// it. It is deliberately MUTABLE: switching stores re-points the identity while
	// the operator moves the fernet/credential key material in place, never
	// re-creating it.
	// +optional
	SecretStoreRef *commonv1.SecretStoreRefSpec `json:"secretStoreRef,omitempty"`

	// Services declares the per-service configuration projected into the
	// individual service CRs.
	Services ServicesSpec `json:"services"`

	// GlobalPolicyOverrides defines oslo.policy overrides applied across every
	// service in the control plane. Named to parallel
	// services.keystone.policyOverrides, whose per-service rules take precedence
	// over these global rules when both are set.
	// +optional
	GlobalPolicyOverrides *commonv1.PolicySpec `json:"globalPolicyOverrides,omitempty"`

	// GlobalExtraConfig is a free-form INI block applied to every INI-configured
	// service the ControlPlane declares (Keystone and Glance today). It is merged
	// key by key with each service's own extraConfig — sections are unioned, the
	// per-service value wins per key, and a global key with no per-service
	// counterpart stays effective — before the merged result is projected onto
	// the child CR. It NEVER applies to Horizon: the dashboard renders flat Django
	// settings, not INI, so services.horizon.extraConfig stands alone. Legal but
	// inert in External mode (no INI-configured workload is deployed), the same
	// posture globalPolicyOverrides has.
	// +optional
	GlobalExtraConfig map[string]map[string]string `json:"globalExtraConfig,omitempty"`

	// KORC configures the K-ORC (OpenStack Resource Controller) integration used
	// to bootstrap and rotate the admin application credential and any declared
	// bootstrap resources.
	KORC KORCSpec `json:"korc"`
}

// InfrastructureSpec declares the shared backing services for the control
// plane. All three fields reuse the canonical commonv1 shapes so the
// ControlPlane and the per-service CRs validate the database, cache, and
// messaging the same way. Database and cache are always present; messaging is
// an optional pointer, so a ControlPlane that declares no message bus gets none.
type InfrastructureSpec struct {
	// Database defines the MariaDB connection parameters shared by the control
	// plane. Supports managed (clusterRef) and brownfield (host) modes; exactly
	// one must be set. That invariant is carried by the embedded commonv1.DatabaseSpec
	// type-level CEL rule (and the validating webhook), mirroring keystone — no
	// field-level marker is needed here, and duplicating it would emit the rule twice.
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached configuration shared by the control plane.
	// Supports managed (clusterRef) and brownfield (servers) modes; exactly one
	// must be set. That invariant is carried by the embedded commonv1.CacheSpec
	// type-level CEL rule (and the validating webhook), mirroring keystone — no
	// field-level marker is needed here, and duplicating it would emit the rule twice.
	Cache commonv1.CacheSpec `json:"cache"`

	// Messaging declares the shared RabbitMQ message bus. It is opt-in: the
	// defaulting webhook never materializes this block, and a ControlPlane
	// that omits it provisions no broker. When set in managed mode
	// (clusterRef), the reconciler provisions exactly one RabbitmqCluster in
	// the ControlPlane's own namespace whether or not a service consumes it:
	// a bus is shared across services by nature, so "declared" means
	// "wanted". This differs from database and cache, which follow the
	// services that consume them. Consumers reach the bus at
	// <name>.<namespace>.svc. Once declared the block cannot be removed
	// (the validating webhook rejects it); delete the ControlPlane to tear
	// the bus down. Managed vs brownfield is a type-level CEL rule on the
	// embedded commonv1.MessagingSpec, as for database and cache.
	// +optional
	Messaging *commonv1.MessagingSpec `json:"messaging,omitempty"`
}

// ServicesSpec declares the per-service configuration of the control plane.
// Keystone, Horizon, Glance, Placement, Barbican, and Neutron are modeled
// today; additional services are added as optional pointer fields as the
// operator grows.
type ServicesSpec struct {
	// Keystone configures the Keystone service projected by the reconciler.
	// Optional: a ControlPlane with services.keystone unset manages no Keystone
	// service (staged adoption, or an externally-managed Keystone), and the
	// reconciler reports KeystoneReady as not-managed. Flipping this from set to
	// nil deletes the previously-projected Keystone child.
	// +optional
	Keystone *ServiceKeystoneSpec `json:"keystone,omitempty"`

	// Horizon configures the Horizon dashboard projected by the reconciler.
	// Optional: a ControlPlane with services.horizon unset manages no dashboard
	// and the reconciler reports HorizonReady as not-managed. The projection is
	// gated on KeystoneReady — the dashboard authenticates against the
	// ControlPlane's Keystone child, so it is only created once that child is
	// ready.
	// +optional
	Horizon *ServiceHorizonSpec `json:"horizon,omitempty"`

	// Glance configures the Glance image service projected by the reconciler.
	// Optional: a ControlPlane with services.glance unset manages no image
	// service and the reconciler reports GlanceReady as not-managed. The
	// projection is gated on KeystoneReady — the image service authenticates
	// against the ControlPlane's Keystone child, so it is only created once that
	// child is ready. Flipping this from set to nil deletes the
	// previously-projected Glance child.
	// +optional
	Glance *ServiceGlanceSpec `json:"glance,omitempty"`

	// Placement configures the Placement service projected by the reconciler.
	// Optional: a ControlPlane with services.placement unset manages no placement
	// service and the reconciler reports PlacementReady as not-managed. The
	// projection is gated on KeystoneReady — the placement service authenticates
	// against the ControlPlane's Keystone child, so it is only created once that
	// child is ready. Flipping this from set to nil deletes the
	// previously-projected Placement child.
	// +optional
	Placement *ServicePlacementSpec `json:"placement,omitempty"`

	// Barbican configures the Barbican key manager projected by the reconciler.
	// Optional: a ControlPlane with services.barbican unset manages no key
	// manager and the reconciler reports BarbicanReady as not-managed. The
	// projection is gated on KeystoneReady — the key manager authenticates
	// against the ControlPlane's Keystone child, so it is only created once that
	// child is ready. Flipping this from set to nil deletes the
	// previously-projected Barbican child.
	// +optional
	Barbican *ServiceBarbicanSpec `json:"barbican,omitempty"`

	// Neutron configures the network service projected by the reconciler. The
	// ControlPlane projects one Neutron CR, registers the network catalog entry
	// and the neutron service account for it, and hands the child the shared bus
	// from spec.infrastructure.messaging as a brownfield Secret. The projection
	// is gated on KeystoneReady (the network service authenticates against the
	// ControlPlane's Keystone child) and on OVNReady, the condition that mirrors
	// the OVNCentral named by ovn.centralRef: the ML2/OVN mechanism driver has no
	// logical network model to write to until that central serves its databases.
	// Optional: a ControlPlane with services.neutron unset manages no network
	// service and the reconciler reports NeutronReady as not-managed
	// (NeutronNotManaged). Flipping this from set to nil preserves the
	// previously-projected Neutron child unless the ControlPlane carries the
	// annotation c5c3.io/allow-neutron-deletion: "true".
	// +optional
	Neutron *ServiceNeutronSpec `json:"neutron,omitempty"`
}

// ServiceKeystoneSpec is a CURATED LOCAL subset of the knobs the ControlPlane
// exposes for the Keystone service.
//
// DECISION (plan decision #2): this is intentionally NOT an import of
// keystonev1alpha1.KeystoneSpec. The reconciler (L2) PROJECTS this
// struct into a Keystone CR; the database, cache, and Fernet rotation schedule
// of that Keystone CR are DERIVED from the ControlPlane (infrastructure.* and
// operator policy) rather than set by the user here. Keeping a curated subset
// avoids leaking every Keystone knob through the aggregate and keeps the L1 api
// package free of a dependency on the keystone module (see DECISION on L2
// dependency coordinates below).
//
// DECISION (plan decision #3 — L2 dependency coordinates): the L1 api
// package originally imported ONLY commonv1, k8s.io/apimachinery/*,
// k8s.io/api/core/v1, and sigs.k8s.io/controller-runtime/* (all already in
// go.mod). `go mod tidy` therefore pruned any service-module require because
// nothing here imported them. The L2 reconciler will need these coordinates
// (recorded here so the orchestrator does not have to re-resolve them):
//   - keystone           => ../keystone (local replace directive)
//   - mariadb-operator    => github.com/mariadb-operator/mariadb-operator v0.38.1
//   - external-secrets    => github.com/external-secrets/external-secrets/apis
//     (match the pin in operators/keystone/go.mod)
//   - K-ORC               => github.com/k-orc/openstack-resource-controller/v2 v2.5.0
//   - memcached.c5c3.io   => NO public Go module; L2 uses unstructured.Unstructured
//
// DECISION AMENDED (admission-time extraConfig validation): the api package now
// ADDITIONALLY imports the service api packages —
// operators/keystone/api/v1alpha1, operators/glance/api/v1alpha1,
// operators/horizon/api/v1alpha1, and operators/placement/api/v1alpha1 — to
// reach their embedded option catalogs and ownership registries, so the merged
// extraConfig is validated at admission against the same catalogs the
// reconciler projects. This gives up only package-level import purity, not a
// new module dependency: each of those modules is a direct require in
// operators/c5c3/go.mod with a local replace, so `go mod tidy` keeps them.
// Reaching the catalogs in place keeps them single-source rather than
// duplicating them into the api package.
//
// Mode is the Managed|External discriminator (default Managed). In Managed mode
// (or unset) the reconciler projects a full Keystone service exactly as before.
// In External mode the ControlPlane manages identity against a pre-existing,
// externally-operated Keystone (external.authURL) and deploys no Keystone
// workload; the managed-only knobs below are forbidden and the typed external
// block is required. The intra-struct invariants are expressed as type-level CEL
// rules so they hold at the CRD schema layer even when the validating webhook is
// bypassed; the validating webhook mirrors them (and enforces the cross-field
// rules CEL cannot express: External forbids spec.infrastructure and
// services.horizon, and Managed requires spec.infrastructure).
//
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || has(self.external)",message="services.keystone.external is required when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="(has(self.mode) && self.mode == 'External') || !has(self.external)",message="services.keystone.external may only be set when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.replicas)",message="services.keystone.replicas is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.image)",message="services.keystone.image is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.policyOverrides)",message="services.keystone.policyOverrides is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.rotationInterval)",message="services.keystone.rotationInterval is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.gateway)",message="services.keystone.gateway is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.publicEndpoint)",message="services.keystone.publicEndpoint is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.federationProxyImage)",message="services.keystone.federationProxyImage is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.dedicatedBackingServices)",message="services.keystone.dedicatedBackingServices is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.namespace)",message="services.keystone.namespace is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.databaseCredentialsMode)",message="services.keystone.databaseCredentialsMode is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.extraConfig)",message="services.keystone.extraConfig is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.targetClusterRef)",message="services.keystone.targetClusterRef is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.caBundleSecretRef)",message="services.keystone.caBundleSecretRef is forbidden when services.keystone.mode is External"
type ServiceKeystoneSpec struct {
	// Mode selects whether the Keystone service is Managed (the reconciler
	// deploys and owns a full Keystone workload, today's behavior) or External
	// (identity is managed against a pre-existing, externally-operated Keystone
	// reachable at external.authURL and no Keystone workload is deployed).
	// Defaults to Managed via both the CRD schema default and the defaulting
	// webhook. In External mode the typed external block is required and every
	// managed-only field below is forbidden (CEL + webhook enforced).
	// +kubebuilder:default=Managed
	// +optional
	Mode KeystoneMode `json:"mode,omitempty"`

	// External carries the connection parameters for an externally-operated
	// Keystone. Required when mode is External and forbidden otherwise (CEL +
	// webhook enforced).
	// +optional
	External *ExternalKeystoneSpec `json:"external,omitempty"`

	// Replicas overrides the number of Keystone API replicas. When nil the
	// reconciler applies the Keystone operator's own default (3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Image optionally overrides the Keystone container image. When nil the
	// reconciler derives the image from spec.openStackRelease.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// PolicyOverrides defines per-service oslo.policy overrides for Keystone.
	// When set, these take precedence over spec.global for the Keystone service.
	// +optional
	PolicyOverrides *commonv1.PolicySpec `json:"policyOverrides,omitempty"`

	// ExtraConfig is a free-form INI block for the Keystone service. It is merged
	// key by key with spec.globalExtraConfig — sections unioned, this per-service
	// value winning per key — and the merged result is projected onto the Keystone
	// child's spec.extraConfig. Forbidden in External mode (CEL + webhook
	// enforced): no Keystone workload is deployed, so there is no config to render.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`

	// RotationInterval optionally overrides the Fernet key rotation interval the
	// reconciler derives for the projected Keystone CR. When nil the reconciler
	// derives a default schedule.
	// +optional
	RotationInterval *metav1.Duration `json:"rotationInterval,omitempty"`

	// Gateway optionally exposes the projected Keystone API externally via a
	// Gateway API HTTPRoute. When nil (the default) the reconciler does NOT
	// project a gateway and the Keystone API is reachable in-cluster only (its
	// ClusterIP Service); when set, the reconciler projects this onto the Keystone
	// CR's spec.gateway so the keystone-operator attaches an HTTPRoute to the
	// referenced Gateway.
	//
	// this is the shared commonv1.GatewaySpec — the curated local copy
	// was consolidated into internal/common/types so both operators reuse one
	// source of truth.
	// +optional
	Gateway *commonv1.GatewaySpec `json:"gateway,omitempty"`

	// PublicEndpoint is the externally routable Keystone identity endpoint URL
	// (e.g. "https://keystone.127-0-0-1.nip.io:8443/v3"). The reconciler projects
	// it into the Keystone bootstrap (--bootstrap-public-url) and uses it for the
	// K-ORC identity catalog Endpoint, so external clients resolve the same URL
	// Keystone advertises. When empty and Gateway is set, the reconciler derives
	// "https://{gateway.hostname}/v3" (the default-443 form); set it explicitly
	// when the externally reachable port differs (e.g. a kind host-port mapping
	// like :8443), since the port cannot be derived from the hostname alone.
	// The pattern enforces an HTTP(S) URL shape so a malformed endpoint is
	// rejected at admission rather than wedging the projected Keystone CR (the
	// keystone webhook later rejects a non-URL publicEndpoint post-admission).
	// The 512-character bound mirrors the Horizon child's bound on
	// websso.keystoneURL, which the reconciler projects this value onto: a
	// longer value would be schema-legal here and rejected on the child.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://`
	PublicEndpoint string `json:"publicEndpoint,omitempty"`

	// FederationProxyImage optionally overrides the mod_auth_openidc sidecar
	// image the reconciler projects onto the Keystone child's
	// spec.federation.proxyImage. When nil the reconciler projects
	// "ghcr.io/c5c3/keystone-federation-proxy:latest".
	//
	// That default is a MUTABLE tag: every node re-pulls it on each pod start,
	// and there is no way to exercise a locally built sidecar. Override it with
	// a digest-carrying ImageSpec for the immutable pin published images are
	// expected to carry, or with a locally loaded tag to test a sidecar under
	// review. The image is inert until a federation-typed
	// KeystoneIdentityBackend attaches — only then does the keystone-operator
	// project the sidecar.
	//
	// Forbidden in External mode (CEL + webhook enforced): no Keystone workload
	// is deployed, so there is no sidecar to image.
	// +optional
	FederationProxyImage *commonv1.ImageSpec `json:"federationProxyImage,omitempty"`

	// DatabaseCredentialsMode overrides spec.infrastructure.database.credentialsMode
	// for THIS service on the managed SHARED database, so a staged migration can run
	// Keystone on one mode while another service stays on the other. Empty (the
	// default) inherits the ControlPlane-wide mode; it is deliberately NOT
	// materialized by the defaulting webhook, so "inherit" stays distinguishable
	// from an explicit override. A dedicated per-service database is Static-only
	// (its own credentialsMode lives in dedicatedBackingServices.database, where the
	// webhook already rejects Dynamic), so a Dynamic override on a service that
	// declares one is rejected; Dynamic also requires the shared database to be
	// managed (clusterRef set), mirroring the commonv1.DatabaseSpec contract, so a
	// Dynamic override on a brownfield shared database is rejected too. A Static
	// override is always admitted.
	//
	// Forbidden when services.keystone.mode is External (CEL + webhook enforced): no
	// managed database is provisioned, so there is no credentials mode to override.
	// +optional
	// +kubebuilder:validation:Enum=Static;Dynamic
	DatabaseCredentialsMode string `json:"databaseCredentialsMode,omitempty"`

	// DedicatedBackingServices opts the Keystone service out of the
	// ControlPlane-wide shared instances declared in spec.infrastructure and gives
	// it backing services of its own. Omitting it (the default) keeps Keystone on
	// the ControlPlane's shared database cluster and cache, isolated only logically
	// (its own logical database, its own credentials).
	//
	// Forbidden when services.keystone.mode is External: no backing services are
	// provisioned at all when identity is managed against a pre-existing Keystone.
	// +optional
	DedicatedBackingServices *KeystoneDedicatedBackingServicesSpec `json:"dedicatedBackingServices,omitempty"`

	// Namespace places the Keystone service — and the backing services, secret
	// store, and credential material that follow it — in a namespace of its own
	// instead of the ControlPlane's. Omitting it (the default) keeps Keystone in
	// the ControlPlane's namespace, exactly as before.
	//
	// Forbidden when services.keystone.mode is External: no Keystone workload is
	// deployed, so there is nothing to place.
	// +optional
	Namespace *ServiceNamespaceSpec `json:"namespace,omitempty"`

	// TargetClusterRef names the registered target cluster the Keystone service is
	// placed on. The projected Keystone CR and the per-namespace objects that
	// support it (its database, its cache, its credential material) are created
	// there instead of on the local cluster. Omitting it (the default) keeps
	// everything on the local cluster, meaning the management cluster the operator
	// itself runs on, so a ControlPlane without a ref resolves exactly as it did
	// before the field existed.
	//
	// A placed service needs a namespace of its own: the namespace block above is
	// required whenever this ref is set (webhook enforced), since the namespace is
	// the tenant key the placed database, secret store, and credential material are
	// scoped by.
	//
	// The assignment is create-only: the validating webhook freezes the ref after
	// creation, because re-pointing a live service at another cluster would strand
	// its workload, its database, and its credential material on the cluster it came
	// from. The freeze is deliberately webhook-only, with NO CEL transition rule, so
	// it can be relaxed to a gated migration later — the same rationale as the
	// namespace freeze.
	//
	// Forbidden when services.keystone.mode is External: no Keystone workload is
	// deployed, so there is nothing to place.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`

	// CABundleSecretRef optionally references a Secret carrying the private CA
	// bundle the operator trusts when verifying a PLACED Keystone's publicEndpoint.
	// K-ORC runs on the management cluster and dials that endpoint over https on
	// every mint and re-mint, and the operator's container ships nothing but the
	// system trust store — so a target published with a cert-manager-issued private
	// CA, the default posture of this stack, would fail verification with no way to
	// supply the anchor. The bundle is projected verbatim as the inline `cacert`
	// key into BOTH generated K-ORC credentials Secrets, exactly as the External
	// mode's external.caBundleSecretRef is. Key defaults to "ca.crt"
	// (webhook-only, the same discipline as that field).
	//
	// The Secret is read in the ControlPlane's OWN namespace on the management
	// cluster, because that is where the credentials Secrets and K-ORC live —
	// not in the placed service's namespace on the target.
	//
	// Required only by the deployment's trust posture, never by admission: a
	// publicly trusted certificate needs no bundle. It is forbidden without
	// targetClusterRef (a co-located Keystone is dialled over its in-cluster
	// Service URL, which performs no TLS handshake for a bundle to verify) and
	// forbidden in External mode (external.caBundleSecretRef is that mode's
	// field). Both are webhook enforced.
	// +optional
	CABundleSecretRef *commonv1.SecretRefSpec `json:"caBundleSecretRef,omitempty"`
}

// ServiceNamespaceLifecycle selects who owns the lifecycle of a service's
// dedicated namespace. It mirrors the managed-vs-brownfield split the backing
// services already use, one level up: the operator either creates and destroys
// the namespace, or it uses one it must never touch.
// +kubebuilder:validation:Enum=Managed;External
type ServiceNamespaceLifecycle string

const (
	// ServiceNamespaceLifecycleManaged (the default) has the operator CREATE the
	// namespace, stamp it with the ControlPlane's ownership labels, and DELETE it
	// when the ControlPlane is torn down. A namespace that already exists without
	// those labels is never adopted: the sub-reconciler fails loud with
	// NamespacesReady=False/NamespaceNotOwned rather than taking over — and
	// eventually deleting — a namespace somebody else provisioned.
	ServiceNamespaceLifecycleManaged ServiceNamespaceLifecycle = "Managed"
	// ServiceNamespaceLifecycleExternal has the operator USE a pre-existing
	// namespace it does not own: it is never created, never labelled, and never
	// deleted. Teardown removes the children the ControlPlane placed in it and
	// leaves the namespace itself standing. Use it for namespaces whose quotas,
	// RBAC, and policies are provisioned out-of-band.
	ServiceNamespaceLifecycleExternal ServiceNamespaceLifecycle = "External"
)

// ServiceNamespaceSpec assigns one service of the ControlPlane to a namespace of
// its own. The assignment is create-only: the validating webhook freezes the
// block's presence, name, and lifecycle after creation, because moving a live
// service across namespaces would leave its database, its secret material, and
// its per-namespace tenant store behind with no migration path.
//
// The namespace is a TENANT KEY throughout the stack — the OpenBao paths, the
// database-engine role, and the templated eso-tenant policy are all scoped by it
// — so a namespace may be claimed by exactly one ControlPlane: the webhook
// rejects a name another ControlPlane already occupies (as its own namespace or
// as one of its service namespaces), and vice versa.
type ServiceNamespaceSpec struct {
	// Name is the namespace the service is placed in. It must differ from the
	// ControlPlane's own namespace — omit the whole block to keep the service
	// there — and be an RFC-1123 label, the shape Kubernetes requires of a
	// namespace name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Lifecycle selects whether the operator owns the namespace (Managed: it is
	// created, labelled, and deleted with the ControlPlane) or merely uses a
	// pre-existing one (External: never created, never deleted). Defaults to
	// Managed via both the CRD schema default and the defaulting webhook.
	// +kubebuilder:default=Managed
	// +optional
	Lifecycle ServiceNamespaceLifecycle `json:"lifecycle,omitempty"`
}

// KeystoneDedicatedBackingServicesSpec declares the backing-service instances
// the Keystone service gets for itself instead of the ControlPlane-wide shared
// ones. Each backing-service class is one optional pointer field, so a service
// can take a dedicated database, a dedicated cache, or both; a class left unset
// resolves to the ControlPlane-wide instance in spec.infrastructure.
//
// A dedicated instance supports the same managed (clusterRef) and brownfield
// (database.host / cache.servers) modes as the shared block. In managed mode the
// clusterRef name defaults to "{controlplane}-keystone-db" /
// "{controlplane}-keystone-cache" and must not collide with the shared instance
// or with another service's dedicated instance of the same class.
//
// The block is optional, but must declare at least one class when present.
//
// +kubebuilder:validation:XValidation:rule="has(self.database) || has(self.cache)",message="dedicatedBackingServices must declare at least one backing-service class (database, cache)"
type KeystoneDedicatedBackingServicesSpec struct {
	// Database gives Keystone its own database cluster instead of the shared
	// spec.infrastructure.database. Managed (clusterRef) and brownfield (host)
	// modes are both supported.
	//
	// A dedicated managed database uses credentialsMode Static (the webhook
	// materializes it and rejects Dynamic): the OpenBao database engine is
	// bootstrapped once per namespace against the shared cluster, so no engine role
	// exists that could issue credentials for a dedicated instance. Seed and rotate
	// the credential at the OpenBao source.
	// +optional
	Database *commonv1.DatabaseSpec `json:"database,omitempty"`

	// Cache gives Keystone its own cache instead of the shared
	// spec.infrastructure.cache. Managed (clusterRef) and brownfield (servers)
	// modes are both supported.
	// +optional
	Cache *commonv1.CacheSpec `json:"cache,omitempty"`
}

// HorizonDedicatedBackingServicesSpec declares the backing-service instances the
// Horizon dashboard gets for itself instead of the ControlPlane-wide shared
// ones. The dashboard consumes no database, so cache is the only class it can
// take dedicated. See KeystoneDedicatedBackingServicesSpec for the full
// contract.
//
// +kubebuilder:validation:XValidation:rule="has(self.cache)",message="dedicatedBackingServices must declare at least one backing-service class (cache)"
type HorizonDedicatedBackingServicesSpec struct {
	// Cache gives the dashboard its own cache instead of the shared
	// spec.infrastructure.cache. Managed (clusterRef) and brownfield (servers)
	// modes are both supported; a managed clusterRef defaults to
	// "{controlplane}-horizon-cache".
	// +optional
	Cache *commonv1.CacheSpec `json:"cache,omitempty"`
}

// KeystoneMode selects whether the ControlPlane's Keystone service is deployed
// and owned by the operator (Managed) or backed by a pre-existing, externally-
// operated Keystone (External). It mirrors the managed-vs-brownfield split of
// the infrastructure specs at the service level.
// +kubebuilder:validation:Enum=Managed;External
type KeystoneMode string

const (
	// KeystoneModeManaged (the default) deploys and owns a full Keystone
	// workload — today's behavior, byte-identical.
	KeystoneModeManaged KeystoneMode = "Managed"
	// KeystoneModeExternal manages identity against a pre-existing, externally-
	// operated Keystone (external.authURL) and deploys no Keystone workload.
	KeystoneModeExternal KeystoneMode = "External"
)

// ExternalEndpointType selects which Keystone catalog interface the control
// plane authenticates against. It maps to the clouds.yaml `endpoint_type` key —
// deliberately named endpointType rather than interface because K-ORC drops
// gophercloud's Interface field and only honours endpoint_type; the authoritative
// note lives on buildAppCredCloudsYAML in the reconciler's korc_cloudsyaml.go.
// +kubebuilder:validation:Enum=public;internal;admin
type ExternalEndpointType string

const (
	// ExternalEndpointTypePublic is the default: the public catalog interface.
	ExternalEndpointTypePublic ExternalEndpointType = "public"
	// ExternalEndpointTypeInternal selects the internal catalog interface.
	ExternalEndpointTypeInternal ExternalEndpointType = "internal"
	// ExternalEndpointTypeAdmin selects the admin catalog interface.
	ExternalEndpointTypeAdmin ExternalEndpointType = "admin"
)

// ExternalKeystoneSpec declares how the control plane reaches a pre-existing,
// externally-operated Keystone in External mode. It mirrors the brownfield
// infrastructure shape at the identity level: the endpoint and, optionally, a
// private-CA bundle are supplied here, and the reconciler manages identity
// against that endpoint rather than deploying a Keystone workload.
type ExternalKeystoneSpec struct {
	// AuthURL is the identity endpoint of the external Keystone (e.g.
	// "https://keystone.example.com/v3"). Required in External mode. The pattern
	// enforces an HTTP(S) URL shape with a non-empty host so a malformed or
	// hostless endpoint is rejected at admission; the validating webhook mirrors
	// it with a full net/url parse as defense-in-depth. Neither gate is an SSRF
	// control — admission cannot resolve where the host points, so the reconciler
	// that dials this endpoint must still enforce network egress restrictions.
	//
	// maxLength bounds the ONE unbounded input the reconciler interpolates into
	// status.conditions[].message. The pattern is end-unanchored, so without a cap
	// a multi-kilobyte path is admissible and the assembled message can exceed the
	// apiserver's 32768-byte message cap — which fails the WHOLE status.conditions
	// write, so no condition persists and the reconciler spins in a backoff loop.
	// 2048 is the conventional practical URL ceiling and far above any real
	// identity endpoint. Callers that bypass both gates are caught by
	// truncateConditionMessage at every interpolation site.
	// +kubebuilder:validation:Pattern=`^https?://[^\s/]+`
	// +kubebuilder:validation:MaxLength=2048
	AuthURL string `json:"authURL"`

	// EndpointType selects which Keystone catalog interface to authenticate
	// against. Defaults to public via both the CRD schema default and the
	// defaulting webhook. It is rendered as the clouds.yaml `endpoint_type` key
	// in both generated credentials Secrets (see ExternalEndpointType). The
	// selected interface must exist in the external Keystone's service catalog
	// for spec.region, otherwise the control plane fails loud with
	// KORCReady=False/CatalogEndpointMismatch.
	// +kubebuilder:default=public
	// +optional
	EndpointType ExternalEndpointType `json:"endpointType,omitempty"`

	// CABundleSecretRef optionally references a Secret carrying a private CA
	// bundle the client trusts when verifying the external Keystone endpoint.
	// The referenced bundle is projected verbatim as the inline `cacert` key
	// into BOTH generated K-ORC credentials Secrets — K-ORC reads that key
	// natively from the same Secret that carries clouds.yaml, so no mount and no
	// upstream change are needed. Key defaults to "ca.crt"; the default is
	// webhook-only because the shared SecretRefSpec carries no c5c3-specific
	// marker (the same discipline as passwordSecretRef.Key).
	//
	// Rotating or removing the bundle converges the Secrets immediately, but
	// K-ORC's provider-client cache keys on the parsed cloud struct only —
	// `cacert` is not part of the key — so the new trust store only takes effect
	// once the cached client expires (~token lifetime / 2).
	// +optional
	CABundleSecretRef *commonv1.SecretRefSpec `json:"caBundleSecretRef,omitempty"`

	// Catalog tunes how the control plane stewards the external Keystone's
	// service catalog. It is optional and defaults to the conservative posture:
	// the identity service and all three of its endpoint interfaces are IMPORTED
	// as unmanaged K-ORC CRs and ZERO catalog entries are created.
	// +optional
	Catalog *ExternalCatalogSpec `json:"catalog,omitempty"`
}

// IdentityCatalogServiceType is the OpenStack service type of the Keystone
// catalog entry. It is the `type` filter of the External-mode identity Service
// import, and the one entry type a KeystoneService catalog block rejects. It is
// the single source of truth both the validating webhook and the reconciler
// reference, so the rule and the import can never drift apart.
const IdentityCatalogServiceType = "identity"

// ExternalCatalogSpec tunes External-mode catalog stewardship. Its single field
// is optional, and the zero value is the conservative default: import the
// existing identity service (and its public/internal/admin endpoints), create
// nothing.
type ExternalCatalogSpec struct {
	// IdentityServiceName disambiguates the identity Service import when the
	// external catalog carries more than one `identity`-type service. When empty
	// the import filters on type alone; a filter matching zero entries surfaces
	// CatalogReady=False/ImportStalled, and a filter matching several surfaces
	// CatalogReady=False/CatalogFailed naming this field — the reconciler never
	// guesses and never imports all matches.
	//
	// Disambiguation is by NAME only, deliberately: K-ORC's ServiceImport.ID
	// carries a `Format:=uuid` marker (the RFC-4122 dashed form) while Keystone
	// mints service IDs as dashless `uuid4().hex`, so an ID-based import is
	// rejected by K-ORC's own CRD schema and cannot be offered here. A catalog
	// holding two identically NAMED identity services therefore cannot be
	// disambiguated from the spec; the condition says so and the external catalog
	// must be repaired.
	//
	// The pattern and the caps mirror K-ORC's own OpenStackName, which the name is
	// cast to on the Service import filter. A comma is not exotic input here
	// (OpenStack list filters are comma-separated, which is why OpenStackName
	// forbids it), and admitting one would only move the rejection to the K-ORC
	// CRD, where it wedges the reconcile in an exponential backoff no ControlPlane
	// field error explains. The validating webhook mirrors the pattern.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	IdentityServiceName string `json:"identityServiceName,omitempty"`
}

// ServiceHorizonSpec is a CURATED LOCAL subset of the knobs the ControlPlane
// exposes for the Horizon dashboard, mirroring the ServiceKeystoneSpec
// DECISION above: the reconciler (L2) PROJECTS this struct into a Horizon CR;
// the cache and the Keystone endpoint of that Horizon CR are DERIVED from the
// ControlPlane (infrastructure.cache and the Keystone child's naming
// convention) rather than set by the user here, and the L1 api package stays
// free of a dependency on the horizon module.
type ServiceHorizonSpec struct {
	// Replicas overrides the number of dashboard replicas. When nil the
	// reconciler applies the Horizon operator's own default (3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Image optionally overrides the Horizon container image. When nil the
	// reconciler derives the image from spec.openStackRelease.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// Gateway optionally exposes the projected dashboard externally via a
	// Gateway API HTTPRoute. When nil (the default) the reconciler does NOT
	// project a gateway and the dashboard is reachable in-cluster only.
	// +optional
	Gateway *commonv1.GatewaySpec `json:"gateway,omitempty"`

	// SecretKeyRef optionally overrides the Secret holding the Django
	// SECRET_KEY the dashboard replicas share. When nil the reconciler defaults
	// to the kind-infrastructure shim Secret "horizon-secret-key" (key
	// "secret-key"), which is pinned to the default ControlPlane identity —
	// multi-ControlPlane deployments MUST set this field explicitly so each
	// dashboard reads its own key material.
	// +optional
	SecretKeyRef *commonv1.SecretRefSpec `json:"secretKeyRef,omitempty"`

	// PublicEndpoint is the BROWSER-observed dashboard base URL, without a
	// trailing slash and INCLUDING a non-default port
	// (e.g. "https://horizon.127-0-0-1.nip.io" or
	// "https://horizon.example.com:8443"). The reconciler derives the WebSSO
	// origin from it — publicEndpoint + "/auth/websso/" — and projects that
	// onto the Keystone child's spec.federation.trustedDashboards.
	//
	// Keystone matches the origin the dashboard sends VERBATIM, so the value
	// must reproduce exactly what the browser's address bar shows. When empty
	// and Gateway is set, the reconciler derives "https://{gateway.hostname}",
	// the default-443 form; any deployment publishing the dashboard on another
	// port MUST set this field explicitly, since the port cannot be derived
	// from the hostname alone and the WebSSO hand-off would be rejected.
	//
	// NOTE: Django derives the origin it sends from the request's Host header,
	// i.e. from gateway.hostname — not from this field. Setting a publicEndpoint
	// whose host differs from gateway.hostname therefore produces an origin
	// Keystone will reject, so whenever a gateway is configured the validating
	// webhook enforces that the two hostnames agree (and that the scheme is
	// https, since the Gateway listener terminates TLS).
	//
	// The 499-character bound is the Keystone child's 512-character bound on
	// spec.federation.trustedDashboards[] minus the 13 characters the derived
	// origin appends ("/auth/websso/"). Without it a schema-legal value here
	// would be rejected on the projected child, wedging the whole ControlPlane
	// behind an error naming a field the operator never wrote.
	//
	// This mirrors ServiceKeystoneSpec.PublicEndpoint. It needs no External-mode
	// forbid-rule: the validating webhook already forbids services.horizon
	// entirely when services.keystone.mode is External.
	// +optional
	// +kubebuilder:validation:MaxLength=499
	// +kubebuilder:validation:Pattern=`^https?://`
	PublicEndpoint string `json:"publicEndpoint,omitempty"`

	// ExtraConfig carries free-form flat Django settings, mirroring the Horizon
	// child's spec.extraConfig (operators/horizon/api/v1alpha1/horizon_types.go).
	// Keys are Django setting names and values are arbitrary JSON; the reconciler
	// projects the block verbatim onto the Horizon child. It is NOT an INI block,
	// so spec.globalExtraConfig — which is INI, applied only to the INI-configured
	// services — never applies to it.
	// +optional
	ExtraConfig map[string]apiextensionsv1.JSON `json:"extraConfig,omitempty"`

	// DedicatedBackingServices opts the dashboard OUT of the ControlPlane-wide
	// shared cache declared in spec.infrastructure and gives it a cache of its
	// own. Omitting it (the default) keeps today's behavior — the dashboard shares
	// the ControlPlane's cache. See HorizonDedicatedBackingServicesSpec.
	// +optional
	DedicatedBackingServices *HorizonDedicatedBackingServicesSpec `json:"dedicatedBackingServices,omitempty"`

	// Namespace places the dashboard — and the cache and secret store that follow
	// it — in a namespace of its own instead of the ControlPlane's. Omitting it
	// (the default) keeps the dashboard in the ControlPlane's namespace.
	//
	// A dashboard in a namespace of its own reads its Django SECRET_KEY from THAT
	// namespace: the default "horizon-secret-key" shim Secret is namespace-local,
	// so such a deployment must supply the key material there (and name it via
	// secretKeyRef if it is named differently).
	// +optional
	Namespace *ServiceNamespaceSpec `json:"namespace,omitempty"`

	// TargetClusterRef names the registered target cluster the dashboard is placed
	// on. The projected Horizon CR and the per-namespace objects that support it
	// (its cache, its secret material) are created there instead of on the local
	// cluster; omitting it (the default) keeps everything on the local (management)
	// cluster. A placed dashboard needs a namespace of its own (webhook enforced),
	// and the ref is frozen after creation by the validating webhook. See
	// ServiceKeystoneSpec.TargetClusterRef.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// ServiceGlanceSpec is a CURATED LOCAL subset of the knobs the ControlPlane
// exposes for the Glance image service, mirroring the ServiceKeystoneSpec and
// ServiceHorizonSpec DECISION above: the reconciler (L2) PROJECTS this struct
// into a Glance CR; the database, cache, and Keystone endpoint of that Glance CR
// are DERIVED from the ControlPlane (infrastructure.* and the Keystone child's
// naming convention) rather than set by the user here, and the L1 api package
// stays free of a dependency on the glance module.
//
// DECISION (phase-0 D10): the image stores are a CURATED list of backends — each
// a name, a driver type (S3 today), the typed driver block, and an isDefault flag
// — projected one-to-one into GlanceBackend child CRs. The api package models its
// own S3 shape (GlanceBackendS3Spec) rather than importing the glance module's
// S3BackendSpec, keeping the curated-subset contract (see the ServiceKeystoneSpec
// DECISION on L2 dependency coordinates). The validating webhook enforces the
// exactly-one-default invariant; the MinItems floor and the XValidation rule
// below make it hold at the CRD schema layer too.
type ServiceGlanceSpec struct {
	// Replicas overrides the number of Glance API replicas. When nil the
	// reconciler applies the Glance operator's own default (3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Image optionally overrides the Glance container image. When nil the
	// reconciler derives the image from spec.openStackRelease.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// Gateway optionally exposes the projected Glance API externally via a
	// Gateway API HTTPRoute. When nil (the default) the reconciler does NOT
	// project a gateway and the Glance API is reachable in-cluster only.
	// +optional
	Gateway *commonv1.GatewaySpec `json:"gateway,omitempty"`

	// PublicEndpoint is the externally routable Glance image endpoint URL
	// (e.g. "https://glance.127-0-0-1.nip.io:8443"). It is used ONLY for the
	// K-ORC public image catalog Endpoint (glanceCatalogURL); unlike the
	// keystone override it is projected into no child CR — the Glance child's
	// keystoneEndpoint is Keystone's endpoint, a separate concern. When empty
	// and Gateway is set, the reconciler derives "https://{gateway.hostname}"
	// (the default-443 form); set it explicitly when the externally reachable
	// port differs (e.g. a kind host-port mapping like :8443), since the port
	// cannot be derived from the hostname alone. The pattern and the
	// 512-character bound mirror ServiceKeystoneSpec.PublicEndpoint, whose
	// value flows into the same K-ORC Endpoint URL field.
	//
	// The keystone override is re-validated on the projected Keystone child;
	// this one is projected nowhere, so the validating webhook is the only gate
	// on the URL that every authenticated image client resolves and sends its
	// scoped Keystone token to. It therefore enforces what the markers cannot:
	// a parseable bare origin (no path, query, or fragment — the Glance API is
	// served at the root), and, whenever a gateway is configured, an https
	// scheme and a host equal to gateway.hostname. Without a gateway an http://
	// value stays legal for development but raises an admission warning.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://`
	PublicEndpoint string `json:"publicEndpoint,omitempty"`

	// Backends is the curated list of image stores projected into GlanceBackend
	// child CRs, one per entry. Exactly one entry must set isDefault, promoting
	// its store to the Glance default_backend; the MinItems floor plus the
	// XValidation single-default rule below hold that invariant at the CRD schema
	// layer, and the validating webhook mirrors it.
	//
	// maxItems bounds the child-CR amplification of one admission: every entry
	// projects one GlanceBackend CR. The name keys the listType=map list, so the
	// apiserver rejects duplicate names.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.filter(b, has(b.isDefault) && b.isDefault).size() == 1",message="exactly one backends entry must set isDefault"
	Backends []GlanceBackendEntry `json:"backends"`

	// ImportFiltering constrains the URIs the child Glance's web-download image
	// import may fetch from. It is projected UNCONDITIONALLY onto the child's
	// spec.importFiltering: clearing it here removes the field from the child, so
	// the glance operator's restrictive defaults (HTTPS-only on port 443 plus a
	// literal host denylist) apply again rather than the last projected value
	// staying pinned. The URI filter is platform security policy, which is why it
	// is projected at all — unlike spec.apiServer, whose child-side defaults stay
	// authoritative.
	//
	// An explicitly EMPTY list does not survive the projection: the lists are
	// serialized with omitempty, so an empty one reaches the child as nil and the
	// child resolves it to the operator default. Glance's own opt-out semantics
	// ("this restriction is deliberately empty") are therefore only expressible on
	// the Glance CR directly. Projection thus always resolves MORE restrictively
	// than the request, never less.
	//
	// The field is typed as the glance module's own ImportFilteringSpec rather
	// than a curated local mirror: this api package already imports that module
	// (see the DECISION AMENDED above), the ControlPlane exposes the filter
	// unreduced, and reusing the type keeps the bounds and the CEL rules
	// single-source in Go — the webhook validates through
	// glancev1alpha1.ValidateImportFiltering, not a copy.
	//
	// controller-gen COPIES the resulting schema into this CRD; it is not resolved
	// against the Glance CRD at runtime, and the two ship in separate Helm charts.
	// So "admitted here, accepted by the Glance child" holds only while both charts
	// come from the same release: upgrade the c5c3-operator chart ahead of the
	// glance-operator chart and a newly added field is admitted here while the
	// older Glance CRD still prunes or rejects it. Roll the two together.
	// +optional
	ImportFiltering *glancev1alpha1.ImportFilteringSpec `json:"importFiltering,omitempty"`

	// Staging bounds the node-local scratch space an image import may consume on
	// the child Glance. The glance operator stamps the resolved sizeLimit on BOTH
	// scratch emptyDirs — the staging store and the tasks-work store — so one
	// glance-api pod is expected to occupy at most twice the configured value on
	// its node; see glancev1alpha1.StagingSpec for the limits of that guarantee.
	// It is projected UNCONDITIONALLY onto the child's spec.staging: clearing it here
	// removes the field from the child, so the glance operator's 10Gi default
	// applies again rather than the last projected value staying pinned.
	//
	// Like ImportFiltering above, the field is typed as the glance module's own
	// StagingSpec so the bound stays single-source in Go — the webhook validates
	// through glancev1alpha1.ValidateStaging, not a copy — and the chart-skew
	// caveat documented there applies to this copied schema too.
	// +optional
	Staging *glancev1alpha1.StagingSpec `json:"staging,omitempty"`

	// ImageCache turns on the local image cache on the child Glance: every
	// glance-api pod then keeps a copy of the image data it has served on its own
	// disk, so a repeat download of the same image is answered from there instead
	// of from the backing store. The cache is per replica rather than shared, so
	// an image is cached once per replica that served it and the disk budget
	// multiplies by replicas; see glancev1alpha1.ImageCacheSpec for what the cache
	// does and does not promise. It is projected UNCONDITIONALLY onto the child's
	// spec.imageCache: setting it here enables the cache, and clearing it removes
	// the field from the child, which disables the cache again on the next rollout
	// rather than leaving the last projected value pinned.
	//
	// Like Staging above, the field is typed as the glance module's own
	// ImageCacheSpec so the floors stay single-source in Go — the webhook
	// validates through glancev1alpha1.ValidateImageCache, not a copy — and the
	// chart-skew caveat documented on ImportFiltering applies to this copied
	// schema too.
	// +optional
	ImageCache *glancev1alpha1.ImageCacheSpec `json:"imageCache,omitempty"`

	// ImportPlugins enables glance's image-import plugins on the child Glance:
	// unpacking a compressed image after staging, converting it to one target disk
	// format, and stamping a fixed set of image properties onto every image the
	// plugin applies to. Presence of a sub-block enables that plugin, the rendered
	// order is fixed (decompression, conversion, inject_image_metadata) rather than
	// an input, and every default resolves at render time in the glance operator;
	// see glancev1alpha1.ImportPluginsSpec for what each plugin does and which
	// upload paths bypass it. It is projected UNCONDITIONALLY onto the child's
	// spec.importPlugins: setting it here selects the plugins, and clearing it
	// removes the field from the child, so the glance operator's defaults (no
	// plugins, `image_import_plugins = []`) apply again on the next rollout rather
	// than leaving the last projected selection pinned.
	//
	// Like ImageCache above, the field is typed as the glance module's own
	// ImportPluginsSpec so the output-format enum and the property-name rules stay
	// single-source in Go — the webhook validates through
	// glancev1alpha1.ValidateImportPlugins, not a copy — and the chart-skew caveat
	// documented on ImportFiltering applies to this copied schema too.
	// +optional
	ImportPlugins *glancev1alpha1.ImportPluginsSpec `json:"importPlugins,omitempty"`

	// DatabaseCredentialsMode overrides spec.infrastructure.database.credentialsMode
	// for THIS service on the managed SHARED database, so a staged migration can run
	// Glance on one mode while another service stays on the other. Empty (the
	// default) inherits the ControlPlane-wide mode; it is deliberately NOT
	// materialized by the defaulting webhook, so "inherit" stays distinguishable
	// from an explicit override. A dedicated per-service database is Static-only
	// (its own credentialsMode lives in dedicatedBackingServices.database, where the
	// webhook already rejects Dynamic), so a Dynamic override on a service that
	// declares one is rejected; Dynamic also requires the shared database to be
	// managed (clusterRef set), mirroring the commonv1.DatabaseSpec contract, so a
	// Dynamic override on a brownfield shared database is rejected too. A Static
	// override is always admitted.
	// +optional
	// +kubebuilder:validation:Enum=Static;Dynamic
	DatabaseCredentialsMode string `json:"databaseCredentialsMode,omitempty"`

	// ExtraConfig is a free-form INI block for the Glance service. It is merged
	// key by key with spec.globalExtraConfig — sections unioned, this per-service
	// value winning per key — and the merged result is projected onto the Glance
	// child's spec.extraConfig.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`

	// DedicatedBackingServices opts the Glance service out of the ControlPlane-wide
	// shared instances declared in spec.infrastructure and gives it backing
	// services of its own. Omitting it (the default) keeps Glance on the
	// ControlPlane's shared database cluster and cache, isolated only logically
	// (its own logical database, its own credentials). See
	// GlanceDedicatedBackingServicesSpec.
	// +optional
	DedicatedBackingServices *GlanceDedicatedBackingServicesSpec `json:"dedicatedBackingServices,omitempty"`

	// Namespace places the Glance service — and the backing services, secret
	// store, and credential material that follow it — in a namespace of its own
	// instead of the ControlPlane's. Omitting it (the default) keeps Glance in the
	// ControlPlane's namespace, exactly as before. The assignment is create-only:
	// the validating webhook freezes the block after creation (see
	// ServiceNamespaceSpec), because moving a live service across namespaces would
	// strand its database, its object-store credentials, and its tenant store with
	// no migration path.
	// +optional
	Namespace *ServiceNamespaceSpec `json:"namespace,omitempty"`

	// TargetClusterRef names the registered target cluster the Glance service is
	// placed on. The projected Glance CR and the per-namespace objects that support
	// it (its database, its cache, its credential material) are created there
	// instead of on the local cluster; omitting it (the default) keeps everything on
	// the local (management) cluster. A placed service needs a namespace of its own
	// (webhook enforced), and the ref is frozen after creation by the validating
	// webhook. See ServiceKeystoneSpec.TargetClusterRef.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// GlanceBackendEntry declares one curated image store of the Glance service. The
// reconciler (L2) projects each entry one-to-one into a GlanceBackend child CR;
// the driver block matching type carries the store parameters. The type/s3 union
// rule enforces "the s3 block is set exactly when type is S3" at the CRD schema
// layer so it holds even when the validating webhook is bypassed, mirroring the
// GlanceBackend CR's own union rule.
// +kubebuilder:validation:XValidation:rule="(self.type == 'S3') == has(self.s3)",message="the s3 block must be set exactly when type is S3"
type GlanceBackendEntry struct {
	// Name keys the listType=map Backends list (the apiserver rejects duplicates)
	// and is embedded verbatim in the name of the projected GlanceBackend child
	// CR, hence the DNS-1123 label shape.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Type selects the image-store driver. Phase 1 supports S3 only; the enum
	// mirrors the GlanceBackend CR's own type enum so an entry admitted here can
	// never be rejected downstream by the GlanceBackend CRD.
	// +kubebuilder:validation:Enum=S3
	Type string `json:"type"`

	// S3 configures the S3-compatible object store. Required exactly when type is
	// S3 (union rule above) and forbidden otherwise.
	// +optional
	S3 *GlanceBackendS3Spec `json:"s3,omitempty"`

	// IsDefault marks this backend as the Glance default store. Exactly one entry
	// in services.glance.backends must set it (CEL + webhook enforced); the
	// reconciler projects it onto the child GlanceBackend's spec.isDefault.
	// +optional
	IsDefault bool `json:"isDefault,omitempty"`
}

// GlanceBackendS3Spec is the CURATED LOCAL S3 store shape the ControlPlane
// projects onto a GlanceBackend child CR's spec.s3. It deliberately models only
// the D10 subset (endpoint, bucket, region, bucketURLFormat, credentials) and
// defines its own field types rather than importing the glance module's
// S3BackendSpec, keeping the L1 api package free of a dependency on the glance
// module (see the ServiceKeystoneSpec DECISION on L2 dependency coordinates). The
// field bounds mirror that S3BackendSpec so a value admitted here can never be
// rejected downstream by the GlanceBackend CRD.
type GlanceBackendS3Spec struct {
	// Endpoint is the S3 endpoint URL, e.g. "https://s3.example.com". Projected
	// onto the child's spec.s3.host.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	Endpoint string `json:"endpoint"`

	// Bucket is the S3 bucket images are stored in. Projected onto the child's
	// spec.s3.bucket.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Region is the S3 region the bucket lives in. Optional; only projected onto
	// the child's spec.s3.region when set.
	// +optional
	Region string `json:"region,omitempty"`

	// BucketURLFormat selects how the bucket is addressed in request URLs: "path"
	// (https://host/bucket) or "virtual" (https://bucket.host). It carries NO
	// schema default on purpose — when unset the projection leaves the child's
	// field unset so the GlanceBackend CR's own default ("path") applies at exactly
	// one layer.
	// +kubebuilder:validation:Enum=path;virtual
	// +optional
	BucketURLFormat string `json:"bucketURLFormat,omitempty"`

	// CredentialsSecretRef references the Secret holding the S3 credentials,
	// resolved in the namespace the Glance service is placed in. Required.
	CredentialsSecretRef SecretNameRef `json:"credentialsSecretRef"`
}

// SecretNameRef is a name-only reference to a Kubernetes Secret, resolved in the
// namespace the Glance service is placed in. commonv1 carries no name-only ref
// (its SecretRefSpec always pairs a key), and this L1 api package must not import
// the glance module's SecretNameRefSpec — so the small name-only shape is defined
// locally here. The data keys the referenced Secret must expose are fixed by
// contract downstream (the GlanceBackend controller reads them), so there is no
// key to select.
type SecretNameRef struct {
	// Name is the referenced Secret's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// GlanceDedicatedBackingServicesSpec declares the backing-service instances the
// Glance service gets for itself instead of the ControlPlane-wide shared ones.
// Glance consumes both a database and a cache, so it can take either or both
// dedicated; a class left unset resolves to the ControlPlane-wide instance in
// spec.infrastructure. See KeystoneDedicatedBackingServicesSpec for the full
// contract.
//
// The block is optional, but must declare at least one class when present.
//
// +kubebuilder:validation:XValidation:rule="has(self.database) || has(self.cache)",message="dedicatedBackingServices must declare at least one backing-service class (database, cache)"
type GlanceDedicatedBackingServicesSpec struct {
	// Database gives Glance its own database cluster instead of the shared
	// spec.infrastructure.database. Managed (clusterRef) and brownfield (host)
	// modes are both supported.
	//
	// A dedicated managed database uses credentialsMode Static (the webhook
	// materializes it and rejects Dynamic): the OpenBao database engine is
	// bootstrapped once per namespace against the shared cluster, so no engine role
	// exists that could issue credentials for a dedicated instance. Seed and rotate
	// the credential at the OpenBao source.
	// +optional
	Database *commonv1.DatabaseSpec `json:"database,omitempty"`

	// Cache gives Glance its own cache instead of the shared
	// spec.infrastructure.cache. Managed (clusterRef) and brownfield (servers)
	// modes are both supported.
	// +optional
	Cache *commonv1.CacheSpec `json:"cache,omitempty"`
}

// ServicePlacementSpec is a CURATED LOCAL subset of the knobs the ControlPlane
// exposes for the Placement service, mirroring the ServiceKeystoneSpec and
// ServiceGlanceSpec DECISION above: the reconciler (L2) PROJECTS this struct
// into a Placement CR; the database, cache, and Keystone endpoint of that
// Placement CR are DERIVED from the ControlPlane (infrastructure.* and the
// Keystone child's naming convention) rather than set by the user here, and the
// L1 api package stays free of a dependency on the placement module.
//
// The subset is smaller than the Glance one because Placement keeps its whole
// state in its database and serves it over HTTP: there are no image stores to
// curate and no scratch space to bound, so nothing here corresponds to the
// backends, import-filtering, staging, or image-cache blocks of
// ServiceGlanceSpec.
type ServicePlacementSpec struct {
	// Replicas overrides the number of Placement API replicas. When nil the
	// reconciler applies the Placement operator's own default (3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Image optionally overrides the Placement container image. When nil the
	// reconciler derives the image from spec.openStackRelease.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// Gateway optionally exposes the projected Placement API externally via a
	// Gateway API HTTPRoute. When nil (the default) the reconciler does NOT
	// project a gateway and the Placement API is reachable in-cluster only.
	// +optional
	Gateway *commonv1.GatewaySpec `json:"gateway,omitempty"`

	// PublicEndpoint is the externally routable Placement endpoint URL
	// (e.g. "https://placement.127-0-0-1.nip.io:8443"). It is used ONLY for the
	// K-ORC public placement catalog Endpoint; unlike the keystone override it is
	// projected into no child CR — the Placement child's keystoneEndpoint is
	// Keystone's endpoint, a separate concern. When empty and Gateway is set, the
	// reconciler derives "https://{gateway.hostname}" (the default-443 form); set
	// it explicitly when the externally reachable port differs (e.g. a kind
	// host-port mapping like :8443), since the port cannot be derived from the
	// hostname alone. The pattern and the 512-character bound mirror
	// ServiceKeystoneSpec.PublicEndpoint, whose value flows into the same K-ORC
	// Endpoint URL field.
	//
	// The keystone override is re-validated on the projected Keystone child; this
	// one is projected nowhere, so the validating webhook is the only gate on the
	// URL that every compute service resolves to place its allocations. It
	// therefore enforces what the markers cannot: a parseable bare origin (no
	// path, query, or fragment — the Placement API is served at the root), and,
	// whenever a gateway is configured, an https scheme and a host equal to
	// gateway.hostname. Without a gateway an http:// value stays legal for
	// development but raises an admission warning.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://`
	PublicEndpoint string `json:"publicEndpoint,omitempty"`

	// DatabaseCredentialsMode overrides spec.infrastructure.database.credentialsMode
	// for THIS service on the managed SHARED database, so a staged migration can run
	// Placement on one mode while another service stays on the other. Empty (the
	// default) inherits the ControlPlane-wide mode; it is deliberately NOT
	// materialized by the defaulting webhook, so "inherit" stays distinguishable
	// from an explicit override. A dedicated per-service database is Static-only
	// (its own credentialsMode lives in dedicatedBackingServices.database, where the
	// webhook already rejects Dynamic), so a Dynamic override on a service that
	// declares one is rejected; Dynamic also requires the shared database to be
	// managed (clusterRef set), mirroring the commonv1.DatabaseSpec contract, so a
	// Dynamic override on a brownfield shared database is rejected too. A Static
	// override is always admitted.
	// +optional
	// +kubebuilder:validation:Enum=Static;Dynamic
	DatabaseCredentialsMode string `json:"databaseCredentialsMode,omitempty"`

	// ExtraConfig is a free-form INI block for the Placement service. It is merged
	// key by key with spec.globalExtraConfig — sections unioned, this per-service
	// value winning per key — and the merged result is projected onto the
	// Placement child's spec.extraConfig.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`

	// DedicatedBackingServices opts the Placement service out of the
	// ControlPlane-wide shared instances declared in spec.infrastructure and gives
	// it backing services of its own. Omitting it (the default) keeps Placement on
	// the ControlPlane's shared database cluster and cache, isolated only logically
	// (its own logical database, its own credentials). See
	// PlacementDedicatedBackingServicesSpec.
	// +optional
	DedicatedBackingServices *PlacementDedicatedBackingServicesSpec `json:"dedicatedBackingServices,omitempty"`

	// Namespace places the Placement service — and the backing services, secret
	// store, and credential material that follow it — in a namespace of its own
	// instead of the ControlPlane's. Omitting it (the default) keeps Placement in
	// the ControlPlane's namespace. The assignment is create-only: the validating
	// webhook freezes the block after creation (see ServiceNamespaceSpec), because
	// moving a live service across namespaces would strand its database, its
	// credential material, and its tenant store with no migration path.
	// +optional
	Namespace *ServiceNamespaceSpec `json:"namespace,omitempty"`

	// TargetClusterRef names the registered target cluster the Placement service is
	// placed on. The projected Placement CR and the per-namespace objects that
	// support it (its database, its cache, its credential material) are created
	// there instead of on the local cluster; omitting it (the default) keeps
	// everything on the local (management) cluster. A placed service needs a
	// namespace of its own (webhook enforced), and the ref is frozen after creation
	// by the validating webhook. See ServiceKeystoneSpec.TargetClusterRef.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// PlacementDedicatedBackingServicesSpec declares the backing-service instances
// the Placement service gets for itself instead of the ControlPlane-wide shared
// ones. Placement consumes both a database and a cache, so it can take either or
// both dedicated; a class left unset resolves to the ControlPlane-wide instance
// in spec.infrastructure. See KeystoneDedicatedBackingServicesSpec for the full
// contract.
//
// The block is optional, but must declare at least one class when present.
//
// +kubebuilder:validation:XValidation:rule="has(self.database) || has(self.cache)",message="dedicatedBackingServices must declare at least one backing-service class (database, cache)"
type PlacementDedicatedBackingServicesSpec struct {
	// Database gives Placement its own database cluster instead of the shared
	// spec.infrastructure.database. Managed (clusterRef) and brownfield (host)
	// modes are both supported.
	//
	// A dedicated managed database uses credentialsMode Static (the webhook
	// materializes it and rejects Dynamic): the OpenBao database engine is
	// bootstrapped once per namespace against the shared cluster, so no engine role
	// exists that could issue credentials for a dedicated instance. Seed and rotate
	// the credential at the OpenBao source.
	// +optional
	Database *commonv1.DatabaseSpec `json:"database,omitempty"`

	// Cache gives Placement its own cache instead of the shared
	// spec.infrastructure.cache. Managed (clusterRef) and brownfield (servers)
	// modes are both supported.
	// +optional
	Cache *commonv1.CacheSpec `json:"cache,omitempty"`
}

// ServiceBarbicanSpec is a CURATED LOCAL subset of the knobs the ControlPlane
// exposes for the Barbican service, mirroring the ServiceKeystoneSpec and
// ServicePlacementSpec DECISION above: the reconciler (L2) PROJECTS this struct
// into a Barbican CR; the database, cache, and Keystone endpoint of that
// Barbican CR are DERIVED from the ControlPlane (infrastructure.* and the
// Keystone child's naming convention) rather than set by the user here.
//
// The curated fields match the Placement ones, plus the secretStore block that
// has no counterpart on any other service: Barbican keeps its secret material
// in an OpenBao (or API-compatible Vault) KV mount rather than in its own
// database, so the store is part of the service's definition, not an optional
// add-on.
type ServiceBarbicanSpec struct {
	// Replicas overrides the number of Barbican API replicas. When nil the
	// reconciler applies the Barbican operator's own default (3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Image optionally overrides the Barbican container image. When nil the
	// reconciler derives the image from spec.openStackRelease.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// Gateway optionally exposes the projected Barbican API externally via a
	// Gateway API HTTPRoute. When nil (the default) the reconciler does NOT
	// project a gateway and the Barbican API is reachable in-cluster only.
	// +optional
	Gateway *commonv1.GatewaySpec `json:"gateway,omitempty"`

	// PublicEndpoint is the externally routable Barbican endpoint URL
	// (e.g. "https://barbican.127-0-0-1.nip.io:8443"). It is used ONLY for the
	// K-ORC public key-manager catalog Endpoint; unlike the keystone override it
	// is projected into no child CR — the Barbican child's keystoneEndpoint is
	// Keystone's endpoint, a separate concern. When empty and Gateway is set, the
	// reconciler derives "https://{gateway.hostname}" (the default-443 form); set
	// it explicitly when the externally reachable port differs (e.g. a kind
	// host-port mapping like :8443), since the port cannot be derived from the
	// hostname alone. The pattern and the 512-character bound mirror
	// ServiceKeystoneSpec.PublicEndpoint, whose value flows into the same K-ORC
	// Endpoint URL field.
	//
	// The keystone override is re-validated on the projected Keystone child; this
	// one is projected nowhere, so the validating webhook is the only gate on the
	// URL every client resolves to store and read its secret material. It
	// therefore enforces what the markers cannot: a parseable bare origin (no
	// path, query, or fragment — the Barbican API is served at the root), and,
	// whenever a gateway is configured, an https scheme and a host equal to
	// gateway.hostname. Without a gateway an http:// value stays legal for
	// development but raises an admission warning.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://`
	PublicEndpoint string `json:"publicEndpoint,omitempty"`

	// DatabaseCredentialsMode overrides spec.infrastructure.database.credentialsMode
	// for THIS service on the managed SHARED database, so a staged migration can run
	// Barbican on one mode while another service stays on the other. Empty (the
	// default) inherits the ControlPlane-wide mode; it is deliberately NOT
	// materialized by the defaulting webhook, so "inherit" stays distinguishable
	// from an explicit override. A dedicated per-service database is Static-only
	// (its own credentialsMode lives in dedicatedBackingServices.database, where the
	// webhook already rejects Dynamic), so a Dynamic override on a service that
	// declares one is rejected; Dynamic also requires the shared database to be
	// managed (clusterRef set), mirroring the commonv1.DatabaseSpec contract, so a
	// Dynamic override on a brownfield shared database is rejected too. A Static
	// override is always admitted.
	// +optional
	// +kubebuilder:validation:Enum=Static;Dynamic
	DatabaseCredentialsMode string `json:"databaseCredentialsMode,omitempty"`

	// ExtraConfig is a free-form INI block for the Barbican service. It is merged
	// key by key with spec.globalExtraConfig — sections unioned, this per-service
	// value winning per key — and the merged result is projected onto the
	// Barbican child's spec.extraConfig.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`

	// SecretStore declares the secret-store backend the Barbican service writes
	// its secret material to, in one of two modes (see
	// ServiceBarbicanSecretStoreSpec). It is REQUIRED: a Barbican with no store
	// attached parks on SecretStoresReady=False/NoDefaultSecretStore for as long
	// as it exists, so a store-less service block would only ever project a child
	// that can never reach Ready.
	// +kubebuilder:validation:Required
	SecretStore ServiceBarbicanSecretStoreSpec `json:"secretStore"`

	// DedicatedBackingServices opts the Barbican service out of the
	// ControlPlane-wide shared instances declared in spec.infrastructure and gives
	// it backing services of its own. Omitting it (the default) keeps Barbican on
	// the ControlPlane's shared database cluster and cache, isolated only logically
	// (its own logical database, its own credentials). See
	// BarbicanDedicatedBackingServicesSpec.
	// +optional
	DedicatedBackingServices *BarbicanDedicatedBackingServicesSpec `json:"dedicatedBackingServices,omitempty"`

	// Namespace places the Barbican service — and the backing services, secret
	// store, and credential material that follow it — in a namespace of its own
	// instead of the ControlPlane's. Omitting it (the default) keeps Barbican in
	// the ControlPlane's namespace. The assignment is create-only: the validating
	// webhook freezes the block after creation (see ServiceNamespaceSpec), because
	// moving a live service across namespaces would strand its database, its
	// credential material, and its tenant store with no migration path.
	// +optional
	Namespace *ServiceNamespaceSpec `json:"namespace,omitempty"`

	// TargetClusterRef names the registered target cluster the Barbican service is
	// placed on. The projected Barbican CR and the per-namespace objects that
	// support it (its database, its cache, its credential material) are created
	// there instead of on the local cluster; omitting it (the default) keeps
	// everything on the local (management) cluster. A placed service needs a
	// namespace of its own (webhook enforced), and the ref is frozen after creation
	// by the validating webhook. See ServiceKeystoneSpec.TargetClusterRef.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// ServiceBarbicanSecretStoreSpec selects the secret-store backend of the
// Barbican service, in one of two modes.
//
// Dedicated mode has the ControlPlane provision an OpenBao instance for this
// Barbican and wire the store to it. External mode points the service at an
// OpenBao or HashiCorp Vault server run outside this control plane, whose
// AppRole credentials the operator only reads.
//
// The two modes address different servers with different credentials, so
// exactly one must be set. The rule is carried by CEL here AND mirrored in the
// validating webhook, because an API server old enough to skip the
// x-kubernetes-validations rule would otherwise admit a block naming neither.
// +kubebuilder:validation:XValidation:rule="has(self.dedicated) != has(self.external)",message="exactly one of dedicated or external must be set"
type ServiceBarbicanSecretStoreSpec struct {
	// Dedicated has the ControlPlane provision an OpenBao instance for this
	// Barbican and attach the store to it. The provisioned instance is
	// PROVING-GRADE, not production-grade: see BarbicanDedicatedSecretStoreSpec.
	// +optional
	Dedicated *BarbicanDedicatedSecretStoreSpec `json:"dedicated,omitempty"`

	// External attaches the store to an OpenBao or HashiCorp Vault server
	// provisioned outside this control plane.
	// +optional
	External *BarbicanExternalSecretStoreSpec `json:"external,omitempty"`
}

// BarbicanDedicatedSecretStoreSpec selects the dedicated OpenBao instance the
// ControlPlane provisions for this Barbican. It carries no fields: the instance
// name, its KV mount, and the AppRole credentials are all derived by convention
// from the ControlPlane, so there is nothing left for the operator to spell out.
//
// The instance it provisions is PROVING-GRADE. It runs the openbao-operator's
// Development profile at a single replica with no PodDisruptionBudget, so any
// disruption of that one pod stops every secret read and write; and it is sealed
// by a static key held in a plain Secret ("<instance>-unseal-key") in the same
// namespace as the raft volume that key seals, so read access to that namespace's
// Secrets — or a single etcd or namespace backup — yields both the ciphertext and
// the key. That is the trade a self-service store mode makes in a cluster with no
// KMS to unseal against; admission repeats it as a warning on every apply. A
// production key manager belongs on secretStore.external, against a hardened
// server with a KMS unseal and a real replica count.
type BarbicanDedicatedSecretStoreSpec struct{}

// BarbicanExternalSecretStoreSpec addresses an OpenBao or HashiCorp Vault server
// provisioned outside this control plane. The operator reads the referenced
// Secrets and renders the store configuration; it never creates a mount, a
// policy, or an AppRole on such a server. The field surface mirrors the barbican
// module's OpenBaoServerSpec / OpenBaoStoreSpec, so a value admitted here can
// never be rejected downstream by the BarbicanSecretStore CRD.
type BarbicanExternalSecretStoreSpec struct {
	// URL is the server's API base URL, e.g. "https://openbao.example.com:8200".
	// TLS is mandatory: the operator's AppRole login and every secret barbican
	// stores travel this URL, so a plaintext scheme would put the role ID, the
	// secret ID, and the private keys and certificates the service exists to
	// protect on the wire in the clear.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`

	// CredentialsSecretRef references the Secret holding the AppRole credentials
	// barbican authenticates with, under the fixed data keys "role-id" and
	// "secret-id". The barbican-operator reads it from the store's namespace, so
	// place the Secret in the namespace the Barbican service is placed in
	// (services.barbican.namespace, or the ControlPlane's own).
	CredentialsSecretRef barbicanv1alpha1.SecretNameRefSpec `json:"credentialsSecretRef"`

	// CABundleSecretRef references the Secret holding the PEM CA bundle that
	// authenticates the server, under the fixed data key "ca.crt". It resolves in
	// the same namespace as credentialsSecretRef. Omit it when the server presents
	// a certificate the pods already trust through their system store.
	// +optional
	CABundleSecretRef *barbicanv1alpha1.SecretNameRefSpec `json:"caBundleSecretRef,omitempty"`

	// KVMountpoint is the path the KV v2 secrets engine holding barbican's secret
	// material is mounted at on that server. The default only fills an absent
	// field, so the MinLength guard is what rejects an explicitly empty mount path.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default=barbican
	// +optional
	KVMountpoint string `json:"kvMountpoint,omitempty"`

	// Namespace scopes every request to an OpenBao/Vault namespace (the
	// enterprise-style multi-tenancy header). Brownfield only, which is the only
	// mode this block describes: a dedicated instance is provisioned at the root
	// namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// BarbicanDedicatedBackingServicesSpec declares the backing-service instances
// the Barbican service gets for itself instead of the ControlPlane-wide shared
// ones. Barbican consumes both a database and a cache, so it can take either or
// both dedicated; a class left unset resolves to the ControlPlane-wide instance
// in spec.infrastructure. See KeystoneDedicatedBackingServicesSpec for the full
// contract.
//
// The block is optional, but must declare at least one class when present. The
// Barbican child CRD requires spec.cache, so the effective cache — dedicated
// here or shared from spec.infrastructure — has to resolve on every path the
// projection takes.
//
// +kubebuilder:validation:XValidation:rule="has(self.database) || has(self.cache)",message="dedicatedBackingServices must declare at least one backing-service class (database, cache)"
type BarbicanDedicatedBackingServicesSpec struct {
	// Database gives Barbican its own database cluster instead of the shared
	// spec.infrastructure.database. Managed (clusterRef) and brownfield (host)
	// modes are both supported.
	//
	// A dedicated managed database uses credentialsMode Static (the webhook
	// materializes it and rejects Dynamic): the OpenBao database engine is
	// bootstrapped once per namespace against the shared cluster, so no engine role
	// exists that could issue credentials for a dedicated instance. Seed and rotate
	// the credential at the OpenBao source.
	// +optional
	Database *commonv1.DatabaseSpec `json:"database,omitempty"`

	// Cache gives Barbican its own cache instead of the shared
	// spec.infrastructure.cache. Managed (clusterRef) and brownfield (servers)
	// modes are both supported.
	// +optional
	Cache *commonv1.CacheSpec `json:"cache,omitempty"`
}

// ServiceNeutronSpec is a CURATED LOCAL subset of the knobs the ControlPlane
// exposes for the network service, mirroring the ServiceKeystoneSpec and
// ServicePlacementSpec DECISION above: the reconciler (L2) PROJECTS this struct
// into a Neutron CR; the database, cache, message bus, and Keystone endpoint of
// that Neutron CR are DERIVED from the ControlPlane (infrastructure.* and the
// Keystone child's naming convention) rather than set by the user here, and the
// L1 api package stays free of a dependency on the neutron module.
//
// Two fields have no counterpart on the other services: the required ovn block,
// because Neutron's ML2/OVN mechanism driver has no logical network model to
// write to without an OVN control plane, and workerReplicas, because the child
// runs its RPC workers in Deployments beside the API.
type ServiceNeutronSpec struct {
	// Replicas overrides the number of Neutron API replicas. When nil the
	// reconciler applies the neutron operator's own default (3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// WorkerReplicas overrides the replica count of the two RPC worker
	// Deployments the neutron child runs beside its API, the periodic workers and
	// the OVN maintenance worker. It is projected onto the child's
	// spec.workers.deployment.replicas, which sizes both. When nil the reconciler
	// applies the neutron operator's own default (3), which is six worker pods.
	// The knob exists because a single-node devstack cannot carry six idle worker
	// pods beside the rest of the control plane.
	// +optional
	// +kubebuilder:validation:Minimum=1
	WorkerReplicas *int32 `json:"workerReplicas,omitempty"`

	// Image optionally overrides the Neutron container image. When nil the
	// reconciler derives the image from spec.openStackRelease.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// Gateway optionally exposes the projected Neutron API externally via a
	// Gateway API HTTPRoute. When nil (the default) the reconciler does NOT
	// project a gateway and the Neutron API is reachable in-cluster only.
	// +optional
	Gateway *commonv1.GatewaySpec `json:"gateway,omitempty"`

	// PublicEndpoint is the externally routable Neutron endpoint URL
	// (e.g. "https://neutron.127-0-0-1.nip.io:8443"). It is used ONLY for the
	// K-ORC public network catalog Endpoint; unlike the keystone override it is
	// projected into no child CR (the Neutron child's keystoneEndpoint is
	// Keystone's endpoint, a separate concern). When empty and Gateway is set, the
	// reconciler derives "https://{gateway.hostname}" (the default-443 form); set
	// it explicitly when the externally reachable port differs (e.g. a kind
	// host-port mapping like :8443), since the port cannot be derived from the
	// hostname alone. The pattern and the 512-character bound mirror
	// ServiceKeystoneSpec.PublicEndpoint, whose value flows into the same K-ORC
	// Endpoint URL field.
	//
	// The keystone override is re-validated on the projected Keystone child; this
	// one is projected nowhere, so the validating webhook is the only gate on the
	// URL every client resolves to create its networks, subnets, and ports. It
	// therefore enforces what the markers cannot: a parseable bare origin (no
	// path, query, or fragment, since the Neutron API is served at the root), and,
	// whenever a gateway is configured, an https scheme and a host equal to
	// gateway.hostname. Without a gateway an http:// value stays legal for
	// development but raises an admission warning.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://`
	PublicEndpoint string `json:"publicEndpoint,omitempty"`

	// DatabaseCredentialsMode overrides spec.infrastructure.database.credentialsMode
	// for THIS service on the managed SHARED database, so a staged migration can run
	// Neutron on one mode while another service stays on the other. Empty (the
	// default) inherits the ControlPlane-wide mode; it is deliberately NOT
	// materialized by the defaulting webhook, so "inherit" stays distinguishable
	// from an explicit override. A dedicated per-service database is Static-only
	// (its own credentialsMode lives in dedicatedBackingServices.database, where the
	// webhook already rejects Dynamic), so a Dynamic override on a service that
	// declares one is rejected; Dynamic also requires the shared database to be
	// managed (clusterRef set), mirroring the commonv1.DatabaseSpec contract, so a
	// Dynamic override on a brownfield shared database is rejected too. A Static
	// override is always admitted.
	// +optional
	// +kubebuilder:validation:Enum=Static;Dynamic
	DatabaseCredentialsMode string `json:"databaseCredentialsMode,omitempty"`

	// ExtraConfig is a free-form INI block for the network service. It is merged
	// key by key with spec.globalExtraConfig (sections unioned, this per-service
	// value winning per key), and the merged result is projected onto the Neutron
	// child's spec.extraConfig, which carries the neutron.conf and ml2_conf.ini
	// sections alike.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`

	// OVN names the OVN control plane the projected Neutron programs. It is
	// REQUIRED: the ML2/OVN mechanism driver writes every network, subnet, and
	// port into an OVN Northbound database, so a Neutron with no central to
	// address would park unready for as long as it exists.
	OVN NeutronOVNSpec `json:"ovn"`

	// DedicatedBackingServices opts the network service out of the
	// ControlPlane-wide shared instances declared in spec.infrastructure and gives
	// it backing services of its own. Omitting it (the default) keeps Neutron on
	// the ControlPlane's shared database cluster and cache, isolated only logically
	// (its own logical database, its own credentials). See
	// NeutronDedicatedBackingServicesSpec.
	// +optional
	DedicatedBackingServices *NeutronDedicatedBackingServicesSpec `json:"dedicatedBackingServices,omitempty"`

	// Namespace places the network service, and the backing services, secret
	// store, and credential material that follow it, in a namespace of its own
	// instead of the ControlPlane's. Omitting it (the default) keeps Neutron in
	// the ControlPlane's namespace. The assignment is create-only: the validating
	// webhook freezes the block after creation (see ServiceNamespaceSpec), because
	// moving a live service across namespaces would strand its database, its
	// credential material, and its tenant store with no migration path.
	// +optional
	Namespace *ServiceNamespaceSpec `json:"namespace,omitempty"`

	// TargetClusterRef names the registered target cluster the network service is
	// placed on. The projected Neutron CR and the per-namespace objects that
	// support it (its database, its cache, its credential material) are created
	// there instead of on the local cluster; omitting it (the default) keeps
	// everything on the local (management) cluster. A placed service needs a
	// namespace of its own (webhook enforced), and the ref is frozen after creation
	// by the validating webhook. See ServiceKeystoneSpec.TargetClusterRef.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// NeutronOVNSpec names the OVN control plane the projected Neutron programs.
type NeutronOVNSpec struct {
	// CentralRef names the OVNCentral whose Northbound and Southbound databases
	// the ML2/OVN mechanism driver connects to.
	CentralRef NeutronOVNCentralRef `json:"centralRef"`
}

// NeutronOVNCentralRef names an OVNCentral the ControlPlane only REFERENCES. The
// central is deployed outside the plane, the way the infrastructure clusters in
// spec.infrastructure are: the ControlPlane never projects it, never updates it,
// and never deletes it, it only reads the databases it publishes and mirrors its
// readiness into the OVNReady condition.
//
// Deployed outside the plane does not mean shared BETWEEN planes: see the reach
// bound on Namespace.
type NeutronOVNCentralRef struct {
	// Name is the OVNCentral's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the namespace the OVNCentral lives in. The defaulting webhook
	// fills an empty value with the ControlPlane's own namespace.
	//
	// A central on a different cluster than the network service has to publish
	// both databases with externallyReachable: true, because the Neutron pods then
	// reach them over the node network rather than through cluster DNS.
	//
	// The value must name a namespace this ControlPlane already reaches: its own,
	// or one it claims through a services.<service>.namespace assignment whose
	// lifecycle is External. The webhook refuses any other namespace. A foreign one
	// is refused because consuming a central out of it mirrors that central's
	// client certificate — a full mTLS identity for its Northbound and Southbound
	// databases — into this plane; a claimed one with lifecycle Managed is refused
	// because the teardown deletes such a namespace together with the plane, and
	// the cascade would take the referenced central, and the logical network model
	// in its databases, with it.
	//
	// That reach rule bounds the topology: a namespace belongs to at most one
	// ControlPlane cluster-wide, so one OVNCentral serves one ControlPlane. A
	// central shared by several planes has no shape here — a second plane can
	// neither claim the namespace it lives in, which is already occupied, nor
	// reference it without claiming it. Give each ControlPlane its own OVNCentral.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
}

// NeutronDedicatedBackingServicesSpec declares the backing-service instances the
// network service gets for itself instead of the ControlPlane-wide shared ones.
// Neutron consumes both a database and a cache, so it can take either or both
// dedicated; a class left unset resolves to the ControlPlane-wide instance in
// spec.infrastructure. See KeystoneDedicatedBackingServicesSpec for the full
// contract.
//
// The block is optional, but must declare at least one class when present.
//
// +kubebuilder:validation:XValidation:rule="has(self.database) || has(self.cache)",message="dedicatedBackingServices must declare at least one backing-service class (database, cache)"
type NeutronDedicatedBackingServicesSpec struct {
	// Database gives Neutron its own database cluster instead of the shared
	// spec.infrastructure.database. Managed (clusterRef) and brownfield (host)
	// modes are both supported.
	//
	// A dedicated managed database uses credentialsMode Static (the webhook
	// materializes it and rejects Dynamic): the OpenBao database engine is
	// bootstrapped once per namespace against the shared cluster, so no engine role
	// exists that could issue credentials for a dedicated instance. Seed and rotate
	// the credential at the OpenBao source.
	// +optional
	Database *commonv1.DatabaseSpec `json:"database,omitempty"`

	// Cache gives Neutron its own cache instead of the shared
	// spec.infrastructure.cache. Managed (clusterRef) and brownfield (servers)
	// modes are both supported.
	// +optional
	Cache *commonv1.CacheSpec `json:"cache,omitempty"`
}

// KORCSpec configures the K-ORC (OpenStack Resource Controller) integration of
// the control plane. It declares how the admin application credential
// is bootstrapped and rotated and which bootstrap resources are reconciled.
type KORCSpec struct {
	// AdminCredential declares the admin OpenStack credential K-ORC uses to
	// reconcile resources, plus the application-credential rotation policy.
	AdminCredential AdminCredentialSpec `json:"adminCredential"`

	// ServiceRegistrations declares which namespaces this control plane consents
	// to standalone KeystoneService registrations from. See
	// ServiceRegistrationsSpec.
	// +optional
	ServiceRegistrations *ServiceRegistrationsSpec `json:"serviceRegistrations,omitempty"`
}

// ServiceRegistrationsSpec carries the control plane's consent to standalone
// KeystoneService CRs registering against it.
//
// A KeystoneService can mint a Keystone service user with arbitrary roles, so a
// CR in a namespace the control plane does not consent to would turn namespace
// access into cloud admin. The reconciler therefore gates every registration on
// this block and reports Ready=False/NamespaceNotAllowed for a namespace it does
// not admit, projecting nothing.
type ServiceRegistrationsSpec struct {
	// AllowedNamespaces admits KeystoneService CRs from namespaces OUTSIDE the ones
	// the control plane already owns. The control plane's own namespace and its
	// dedicated service namespaces (declared via the services' namespace blocks;
	// see ServiceNamespaceSpec) are always admitted and need no entry here — the
	// control plane provisions a tenant store in each of them, and their contents
	// are already its own.
	//
	// The list is an ADMISSION GATE, not a revocation tool. Removing a namespace
	// freezes reconciliation of the KeystoneService CRs in it — they report
	// Ready=False/NamespaceNotAllowed — while every Keystone user, catalog row and
	// delivered Secret already minted stays in place and keeps authenticating.
	// Teardown happens only through deletion of the KeystoneService CR itself, so
	// an edit here can never destroy credentials a running service depends on.
	//
	// listType=set makes the API server reject duplicate entries; the entries
	// carry the RFC-1123 label shape of a Kubernetes namespace name and the list
	// is capped.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// ServiceAccountProjectSpec declares the OpenStack project a service account is
// associated with.
type ServiceAccountProjectSpec struct {
	// Name is the OpenStack project name. The pattern and caps mirror K-ORC's
	// KeystoneName filter.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	Name string `json:"name"`

	// Create selects whether the project is referenced or managed. false (the
	// default) REFERENCES a pre-existing project via an unmanaged K-ORC import —
	// the control plane never creates or deletes it. true CREATES and OWNS a
	// managed K-ORC Project, gated by the same fail-loudly collision probe as the
	// user: a project of that name already existing in Keystone surfaces
	// ServiceAccountCollision rather than silently adopting it.
	// +optional
	Create bool `json:"create,omitempty"`
}

// ServiceAccountRotationMode selects how a service account's password is rotated.
// It is deliberately NOT the admin RotationMode: there is no external password
// source, so PasswordDriven does not apply.
// +kubebuilder:validation:Enum=Manual;Scheduled
type ServiceAccountRotationMode string

const (
	// ServiceAccountRotationModeManual (the default) rotates the password only
	// when a CredentialRotation CR requests it.
	ServiceAccountRotationModeManual ServiceAccountRotationMode = "Manual"
	// ServiceAccountRotationModeScheduled rotates on a schedule. DECISION:
	// surfaced in the enum now so the CRD schema is stable, but the scheduled
	// rotation logic is deferred to a later level; the deferral is NOT silent
	// (the reconciler emits a ScheduledRotationDeferred event), mirroring
	// RotationModeScheduled on the admin credential.
	ServiceAccountRotationModeScheduled ServiceAccountRotationMode = "Scheduled"
)

// ServiceAccountRotationSpec declares the rotation policy for a service account's
// password.
type ServiceAccountRotationSpec struct {
	// Mode selects the rotation strategy. Defaults to Manual via both the CRD
	// schema default and the defaulting webhook.
	// +kubebuilder:validation:Enum=Manual;Scheduled
	// +kubebuilder:default=Manual
	// +optional
	Mode ServiceAccountRotationMode `json:"mode,omitempty"`
}

// AdminCredentialSpec declares the admin OpenStack credential and the
// application-credential rotation policy for the control plane.
type AdminCredentialSpec struct {
	// CloudCredentialsRef references the clouds.yaml Secret K-ORC reads the
	// admin cloud entry from.
	CloudCredentialsRef CloudCredentialsRef `json:"cloudCredentialsRef"`

	// PasswordSecretRef references the Secret holding the admin password used to
	// (re-)mint the application credential. Reuses the canonical commonv1 shape.
	PasswordSecretRef commonv1.SecretRefSpec `json:"passwordSecretRef"`

	// UserName is the OpenStack admin user name the control plane authenticates
	// as. Defaults to "admin" via both the CRD schema default and the defaulting
	// webhook. Valid in both Managed and External modes.
	//
	// It is rendered as the clouds.yaml `username` AND used as the K-ORC admin
	// User import filter the application credential's UserRef resolves to. Those
	// two MUST name the same user: Keystone's default policy only lets a token
	// mint an application credential for its OWN user. Editing this field on a
	// live ControlPlane updates the import filter in place, but K-ORC imports
	// resolve once — the stale resolved id surfaces as
	// KORCReady=False/CredentialDrift rather than silently repointing.
	// +kubebuilder:default=admin
	// +optional
	UserName string `json:"userName,omitempty"`

	// ProjectName is the OpenStack admin project name, rendered as the clouds.yaml
	// `project_name`. Defaults to "admin" via both the CRD schema default and the
	// defaulting webhook. Valid in both modes.
	// +kubebuilder:default=admin
	// +optional
	ProjectName string `json:"projectName,omitempty"`

	// DomainName is the OpenStack admin domain name. Defaults to "Default" via
	// both the CRD schema default and the defaulting webhook. Valid in both
	// modes. Phase-1 nuance: the single DomainName sets BOTH user_domain_name
	// and project_domain_name in the generated clouds.yaml, and is the K-ORC
	// admin Domain import filter, so the admin user and project must live in the
	// same domain; a later userDomainName/projectDomainName split is a
	// compatible extension.
	// +kubebuilder:default=Default
	// +optional
	DomainName string `json:"domainName,omitempty"`

	// ApplicationCredential declares the policy for the K-ORC admin application
	// credential (restriction, access rules, rotation mode).
	ApplicationCredential ApplicationCredentialSpec `json:"applicationCredential"`

	// BootstrapResources declares the OpenStack resources K-ORC bootstraps
	// alongside the admin credential (e.g. the projects/roles a fresh control
	// plane needs). The element shape is intentionally minimal at L1; the
	// reconciler (L2) interprets it.
	//
	// RESERVED, unreconciled: no controller reads this field today. For service
	// users of other OpenStack services, declare a KeystoneService CR instead; it
	// owns the full user + project + password lifecycle. This field stays reserved
	// for a later bootstrap use case.
	// +optional
	BootstrapResources []BootstrapResourceSpec `json:"bootstrapResources,omitempty"`
}

// CloudCredentialsRef references the clouds.yaml Secret and the cloud entry
// within it that K-ORC authenticates as.
type CloudCredentialsRef struct {
	// CloudName is the entry in clouds.yaml K-ORC authenticates as.
	// DECISION defaults to "admin" via both the CRD schema default and
	// the defaulting webhook (for callers that bypass the CRD default), mirroring
	// the sibling SecretName field. The webhook is the load-bearing mechanism when
	// the whole korc block is omitted (the marker only fires when the parent
	// cloudCredentialsRef object is present), so cloudName is safe to drop from the
	// CRD's required list.
	// +kubebuilder:default="admin"
	// +optional
	CloudName string `json:"cloudName,omitempty"`

	// SecretName is the name of the Secret holding the clouds.yaml document.
	// DECISION defaults to "k-orc-clouds-yaml" via both the CRD schema
	// default and the defaulting webhook (for callers that bypass the CRD default),
	// mirroring the region defaulting discipline.
	// +kubebuilder:default="k-orc-clouds-yaml"
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// ApplicationCredentialSpec declares the K-ORC admin application-credential
// policy.
type ApplicationCredentialSpec struct {
	// Restricted controls whether the application credential is unrestricted
	// (able to create further application credentials) or restricted. Defaults
	// to true (the safe, least-privilege baseline) via both the CRD schema default
	// and the defaulting webhook.
	// +kubebuilder:default=true
	// +optional
	Restricted *bool `json:"restricted,omitempty"`

	// AccessRules optionally narrows the application credential to a specific set
	// of service/method/path rules. When empty the credential is not constrained
	// by access rules.
	// +optional
	AccessRules []AccessRule `json:"accessRules,omitempty"`

	// Rotation declares how the application credential is rotated.
	Rotation RotationSpec `json:"rotation"`
}

// AccessRule narrows an application credential to a specific service endpoint
// and method, mirroring the Keystone application-credential access
// rule shape (service / method / path).
type AccessRule struct {
	// Service is the OpenStack service type the rule applies to (e.g. "compute").
	Service string `json:"service"`

	// Method is the HTTP method the rule allows (e.g. "GET", "POST"). Optional:
	// projectAccessRules omits it from the projected K-ORC rule when empty. The
	// enum mirrors K-ORC's HTTPMethod type (the value is cast to it), so a value
	// the downstream ApplicationCredentialAccessRule would reject is caught at
	// admission instead.
	// +optional
	// +kubebuilder:validation:Enum=CONNECT;DELETE;GET;HEAD;OPTIONS;PATCH;POST;PUT;TRACE
	Method string `json:"method,omitempty"`

	// Path is the request path the rule allows (e.g. "/v2.1/servers"). Optional:
	// projectAccessRules omits it when empty. When set it must be an absolute
	// path (leading slash).
	// +optional
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path,omitempty"`
}

// RotationMode selects how the K-ORC admin application credential is rotated
// +kubebuilder:validation:Enum=PasswordDriven;Scheduled;Manual
type RotationMode string

const (
	// RotationModePasswordDriven re-mints the application credential whenever the
	// underlying admin password changes. This is the default.
	RotationModePasswordDriven RotationMode = "PasswordDriven"
	// RotationModeScheduled rotates the application credential on a schedule.
	// DECISION surfaced in the enum now so the CRD schema is stable,
	// but the scheduled rotation logic is deferred to a later level.
	RotationModeScheduled RotationMode = "Scheduled"
	// RotationModeManual rotates only when a CredentialRotation CR requests it.
	RotationModeManual RotationMode = "Manual"
)

// RotationSpec declares the rotation policy for the admin application
// credential.
type RotationSpec struct {
	// Mode selects the rotation strategy. Defaults to PasswordDriven via both the
	// CRD schema default and the defaulting webhook.
	// +kubebuilder:default=PasswordDriven
	// +optional
	Mode RotationMode `json:"mode,omitempty"`
}

// BootstrapResourceSpec declares an OpenStack resource K-ORC bootstraps with
// the control plane. The shape is intentionally minimal at L1 — the
// reconciler (L2) interprets the kind/name and applies it.
type BootstrapResourceSpec struct {
	// Kind is the K-ORC resource kind to bootstrap. Constrained to the kinds the
	// control plane bootstraps today; widen the enum when the L2 reconciler
	// learns to interpret additional kinds.
	// +kubebuilder:validation:Enum=Project;Role
	Kind string `json:"kind"`

	// Name is the name of the bootstrapped resource.
	Name string `json:"name"`
}

// UpdatePhase represents the current phase of a control-plane update.
//
// DECISION the enum surfaces the FUTURE phases (UpdatingServices,
// Verifying, RollingBack) alongside the active ones so the CRD schema is stable
// across levels and does not need a breaking change when the update state
// machine is implemented. The phases marked "not yet implemented" below are
// reserved values that the L1 reconciler never sets; they are documented here
// so consumers (dashboards, kubectl) see the full vocabulary.
// +kubebuilder:validation:Enum=Idle;Updating;UpdatingServices;Verifying;RollingBack
type UpdatePhase string

const (
	// UpdatePhaseIdle indicates no update is in progress.
	UpdatePhaseIdle UpdatePhase = "Idle"
	// UpdatePhaseUpdating indicates a release update has started.
	UpdatePhaseUpdating UpdatePhase = "Updating"
	// UpdatePhaseUpdatingServices indicates per-service CRs are being updated.
	// DECISION reserved; not yet implemented.
	UpdatePhaseUpdatingServices UpdatePhase = "UpdatingServices"
	// UpdatePhaseVerifying indicates the control plane is verifying an update.
	// DECISION reserved; not yet implemented.
	UpdatePhaseVerifying UpdatePhase = "Verifying"
	// UpdatePhaseRollingBack indicates a failed update is being rolled back.
	// DECISION reserved; not yet implemented.
	UpdatePhaseRollingBack UpdatePhase = "RollingBack"
)

// ControlPlaneStatus defines the observed state of a ControlPlane.
type ControlPlaneStatus struct {
	// Conditions represent the latest available observations of the control
	// plane state. Each condition carries an ObservedGeneration so consumers can
	// tell a stale condition from one reflecting the current spec; use the
	// conditions helper (internal/common/conditions) to upsert them.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the .metadata.generation the controller last
	// reconciled, so a stale status is distinguishable from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UpdatePhase is the current phase of a control-plane release update.
	// +optional
	UpdatePhase UpdatePhase `json:"updatePhase,omitempty"`

	// Services reports the per-service readiness of the projected service CRs,
	// as a list keyed by service name (e.g. "keystone"). A listType=map list so
	// per-service entries merge under server-side apply and can grow
	// per-service conditions cleanly.
	// +optional
	// +listType=map
	// +listMapKey=name
	Services []ServiceStatus `json:"services,omitempty"`

	// AdminApplicationCredential reports the observed state of the K-ORC admin
	// application credential.
	// +optional
	AdminApplicationCredential *AdminApplicationCredentialStatus `json:"adminApplicationCredential,omitempty"`

	// Catalog reports the observed state of the External-mode catalog imports. It
	// is nil in Managed mode, where the control plane creates the catalog entries
	// rather than importing them.
	// +optional
	Catalog *CatalogStatus `json:"catalog,omitempty"`
}

// CatalogStatus reports how the External-mode identity catalog imports resolved.
// It is the operator-visible answer to "did the ControlPlane find the catalog it
// was pointed at?" — the aggregate CatalogReady condition says whether they all
// resolved, this list says which ones did.
type CatalogStatus struct {
	// Imports lists the unmanaged K-ORC CRs importing the external identity
	// service and its endpoint interfaces, keyed by CR name.
	// +optional
	// +listType=map
	// +listMapKey=name
	Imports []CatalogImportStatus `json:"imports,omitempty"`
}

// CatalogImportStatus reports the observed state of a single unmanaged catalog
// import.
type CatalogImportStatus struct {
	// Name is the K-ORC CR name; it keys the listType=map Imports list.
	Name string `json:"name"`

	// Kind is the imported K-ORC kind, "Service" or "Endpoint".
	// +kubebuilder:validation:Enum=Service;Endpoint
	Kind string `json:"kind"`

	// Interface is the catalog interface of an imported Endpoint; empty for the
	// Service import.
	// +optional
	Interface ExternalEndpointType `json:"interface,omitempty"`

	// Resolved reports whether K-ORC has matched this import against a live
	// catalog entry (its Available condition is True for the CR's current
	// generation).
	Resolved bool `json:"resolved"`

	// ID is the OpenStack id K-ORC resolved the import to. Empty while the import
	// is unresolved.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id,omitempty"`
}

// ServiceStatus reports the observed readiness of a single projected service
// CR.
type ServiceStatus struct {
	// Name is the service name (e.g. "keystone"); it keys the listType=map
	// Services list.
	Name string `json:"name"`

	// Ready reports whether the projected service CR is Ready.
	Ready bool `json:"ready"`

	// Release is the OpenStack release the service currently reports installed.
	// +optional
	Release string `json:"release,omitempty"`
}

// AdminApplicationCredentialStatus reports the observed state of the K-ORC
// admin application credential.
type AdminApplicationCredentialStatus struct {
	// ID is the OpenStack application-credential ID currently in use.
	// +optional
	ID string `json:"id,omitempty"`

	// Restricted reports whether the active credential is restricted.
	// +optional
	Restricted bool `json:"restricted,omitempty"`

	// LastRotation is the timestamp of the last successful rotation.
	// +optional
	LastRotation *metav1.Time `json:"lastRotation,omitempty"`
}

// IsExternalKeystone reports whether the ControlPlane's Keystone service is in
// External mode: services.keystone is set and its mode is External. It is the
// single, nil-safe discriminator read shared by the webhook (transition gating)
// and the reconciler, so no call site re-implements the mode check. A nil
// services.keystone (no Keystone at all) is not External.
func (cp *ControlPlane) IsExternalKeystone() bool {
	ks := cp.Spec.Services.Keystone
	return ks != nil && ks.Mode == KeystoneModeExternal
}

// keystoneDedicatedBlock / horizonDedicatedBlock are the single nil-safe walk of
// the per-service dedicated BLOCK, shared by the webhook (defaulting, collision
// and immutability rules) and by the class accessors below, so no call site
// re-walks the optional chain. The immutability rules need the block itself, to
// tell "block absent" from "block present with the class unset"; the reconciler
// needs the individual classes, which the exported accessors expose.
func keystoneDedicatedBlock(cp *ControlPlane) *KeystoneDedicatedBackingServicesSpec {
	if ks := cp.Spec.Services.Keystone; ks != nil {
		return ks.DedicatedBackingServices
	}
	return nil
}

func horizonDedicatedBlock(cp *ControlPlane) *HorizonDedicatedBackingServicesSpec {
	if hz := cp.Spec.Services.Horizon; hz != nil {
		return hz.DedicatedBackingServices
	}
	return nil
}

func glanceDedicatedBlock(cp *ControlPlane) *GlanceDedicatedBackingServicesSpec {
	if gl := cp.Spec.Services.Glance; gl != nil {
		return gl.DedicatedBackingServices
	}
	return nil
}

func placementDedicatedBlock(cp *ControlPlane) *PlacementDedicatedBackingServicesSpec {
	if pl := cp.Spec.Services.Placement; pl != nil {
		return pl.DedicatedBackingServices
	}
	return nil
}

func barbicanDedicatedBlock(cp *ControlPlane) *BarbicanDedicatedBackingServicesSpec {
	if bn := cp.Spec.Services.Barbican; bn != nil {
		return bn.DedicatedBackingServices
	}
	return nil
}

func neutronDedicatedBlock(cp *ControlPlane) *NeutronDedicatedBackingServicesSpec {
	if nt := cp.Spec.Services.Neutron; nt != nil {
		return nt.DedicatedBackingServices
	}
	return nil
}

// DedicatedKeystoneDatabase returns the database instance declared FOR the
// Keystone service alone, or nil when Keystone shares the ControlPlane-wide
// instance (the default).
func (cp *ControlPlane) DedicatedKeystoneDatabase() *commonv1.DatabaseSpec {
	if b := keystoneDedicatedBlock(cp); b != nil {
		return b.Database
	}
	return nil
}

// DedicatedKeystoneCache returns the cache instance declared for the Keystone
// service alone, or nil when Keystone shares the ControlPlane-wide instance.
func (cp *ControlPlane) DedicatedKeystoneCache() *commonv1.CacheSpec {
	if b := keystoneDedicatedBlock(cp); b != nil {
		return b.Cache
	}
	return nil
}

// DedicatedHorizonCache returns the cache instance declared for the Horizon
// dashboard alone, or nil when the dashboard shares the ControlPlane-wide
// instance.
func (cp *ControlPlane) DedicatedHorizonCache() *commonv1.CacheSpec {
	if b := horizonDedicatedBlock(cp); b != nil {
		return b.Cache
	}
	return nil
}

// DedicatedGlanceDatabase returns the database instance declared FOR the Glance
// service alone, or nil when Glance shares the ControlPlane-wide instance (the
// default).
func (cp *ControlPlane) DedicatedGlanceDatabase() *commonv1.DatabaseSpec {
	if b := glanceDedicatedBlock(cp); b != nil {
		return b.Database
	}
	return nil
}

// DedicatedGlanceCache returns the cache instance declared for the Glance service
// alone, or nil when Glance shares the ControlPlane-wide instance.
func (cp *ControlPlane) DedicatedGlanceCache() *commonv1.CacheSpec {
	if b := glanceDedicatedBlock(cp); b != nil {
		return b.Cache
	}
	return nil
}

// DedicatedPlacementDatabase returns the database instance declared FOR the
// Placement service alone, or nil when Placement shares the ControlPlane-wide
// instance (the default).
func (cp *ControlPlane) DedicatedPlacementDatabase() *commonv1.DatabaseSpec {
	if b := placementDedicatedBlock(cp); b != nil {
		return b.Database
	}
	return nil
}

// DedicatedPlacementCache returns the cache instance declared for the Placement
// service alone, or nil when Placement shares the ControlPlane-wide instance.
func (cp *ControlPlane) DedicatedPlacementCache() *commonv1.CacheSpec {
	if b := placementDedicatedBlock(cp); b != nil {
		return b.Cache
	}
	return nil
}

// DedicatedBarbicanDatabase returns the database instance declared FOR the
// Barbican service alone, or nil when Barbican shares the ControlPlane-wide
// instance (the default).
func (cp *ControlPlane) DedicatedBarbicanDatabase() *commonv1.DatabaseSpec {
	if b := barbicanDedicatedBlock(cp); b != nil {
		return b.Database
	}
	return nil
}

// DedicatedBarbicanCache returns the cache instance declared for the Barbican
// service alone, or nil when Barbican shares the ControlPlane-wide instance.
func (cp *ControlPlane) DedicatedBarbicanCache() *commonv1.CacheSpec {
	if b := barbicanDedicatedBlock(cp); b != nil {
		return b.Cache
	}
	return nil
}

// DedicatedNeutronDatabase returns the database instance declared FOR the
// network service alone, or nil when Neutron shares the ControlPlane-wide
// instance (the default).
func (cp *ControlPlane) DedicatedNeutronDatabase() *commonv1.DatabaseSpec {
	if b := neutronDedicatedBlock(cp); b != nil {
		return b.Database
	}
	return nil
}

// DedicatedNeutronCache returns the cache instance declared for the network
// service alone, or nil when Neutron shares the ControlPlane-wide instance.
func (cp *ControlPlane) DedicatedNeutronCache() *commonv1.CacheSpec {
	if b := neutronDedicatedBlock(cp); b != nil {
		return b.Cache
	}
	return nil
}

// keystoneNamespaceBlock / horizonNamespaceBlock are the single nil-safe walk of
// the per-service namespace BLOCK, shared by the webhook (defaulting, claim and
// immutability rules) and by the resolvers below. The webhook needs the block
// itself, to tell "no assignment" from "assigned with the lifecycle defaulted";
// the reconciler needs the resolved namespace, which the exported accessors
// expose.
func keystoneNamespaceBlock(cp *ControlPlane) *ServiceNamespaceSpec {
	if ks := cp.Spec.Services.Keystone; ks != nil {
		return ks.Namespace
	}
	return nil
}

func horizonNamespaceBlock(cp *ControlPlane) *ServiceNamespaceSpec {
	if hz := cp.Spec.Services.Horizon; hz != nil {
		return hz.Namespace
	}
	return nil
}

func glanceNamespaceBlock(cp *ControlPlane) *ServiceNamespaceSpec {
	if gl := cp.Spec.Services.Glance; gl != nil {
		return gl.Namespace
	}
	return nil
}

func placementNamespaceBlock(cp *ControlPlane) *ServiceNamespaceSpec {
	if pl := cp.Spec.Services.Placement; pl != nil {
		return pl.Namespace
	}
	return nil
}

func barbicanNamespaceBlock(cp *ControlPlane) *ServiceNamespaceSpec {
	if bn := cp.Spec.Services.Barbican; bn != nil {
		return bn.Namespace
	}
	return nil
}

func neutronNamespaceBlock(cp *ControlPlane) *ServiceNamespaceSpec {
	if nt := cp.Spec.Services.Neutron; nt != nil {
		return nt.Namespace
	}
	return nil
}

// KeystoneNamespace resolves the namespace the Keystone service — and everything
// that follows it: its database, its cache, its tenant store, its admin-password
// and DB-credential material — is placed in. It is the assigned namespace when
// services.keystone.namespace is set, and the ControlPlane's own namespace
// otherwise (the default), so a ControlPlane without an assignment resolves
// exactly as it did before the field existed.
func (cp *ControlPlane) KeystoneNamespace() string {
	if ns := keystoneNamespaceBlock(cp); ns != nil && ns.Name != "" {
		return ns.Name
	}
	return cp.Namespace
}

// HorizonNamespace resolves the namespace the Horizon dashboard — and the cache
// and tenant store that follow it — is placed in. See KeystoneNamespace.
func (cp *ControlPlane) HorizonNamespace() string {
	if ns := horizonNamespaceBlock(cp); ns != nil && ns.Name != "" {
		return ns.Name
	}
	return cp.Namespace
}

// GlanceNamespace resolves the namespace the Glance service — and the database,
// cache, tenant store, and object-store credential material that follow it — is
// placed in. See KeystoneNamespace.
func (cp *ControlPlane) GlanceNamespace() string {
	if ns := glanceNamespaceBlock(cp); ns != nil && ns.Name != "" {
		return ns.Name
	}
	return cp.Namespace
}

// PlacementNamespace resolves the namespace the Placement service — and the
// database, cache, tenant store, and credential material that follow it — is
// placed in. See KeystoneNamespace.
func (cp *ControlPlane) PlacementNamespace() string {
	if ns := placementNamespaceBlock(cp); ns != nil && ns.Name != "" {
		return ns.Name
	}
	return cp.Namespace
}

// BarbicanNamespace resolves the namespace the Barbican service — and the
// database, cache, tenant store, and credential material that follow it — is
// placed in. See KeystoneNamespace.
func (cp *ControlPlane) BarbicanNamespace() string {
	if ns := barbicanNamespaceBlock(cp); ns != nil && ns.Name != "" {
		return ns.Name
	}
	return cp.Namespace
}

// NeutronNamespace resolves the namespace the network service, and the database,
// cache, tenant store, and credential material that follow it, is placed in. See
// KeystoneNamespace.
func (cp *ControlPlane) NeutronNamespace() string {
	if ns := neutronNamespaceBlock(cp); ns != nil && ns.Name != "" {
		return ns.Name
	}
	return cp.Namespace
}

// DedicatedServiceNamespaces returns the namespaces the ControlPlane places
// services in OUTSIDE its own, deduplicated by name and in a stable order
// (keystone first). It is the enumeration every cross-namespace concern walks:
// the namespace sub-reconciler creates/verifies them, the tenant-store
// sub-reconciler provisions a store in each, and the teardown sweeps each.
//
// An assignment naming the ControlPlane's own namespace contributes nothing (the
// webhook rejects it at admission; skipping it here keeps a webhook-bypassed CR
// from re-creating — and, at teardown, deleting — the ControlPlane's own
// namespace). Two services sharing one namespace yield ONE entry: they share
// that namespace's backing services and its tenant store.
func (cp *ControlPlane) DedicatedServiceNamespaces() []ServiceNamespaceSpec {
	var out []ServiceNamespaceSpec
	seen := map[string]struct{}{}
	for _, ns := range []*ServiceNamespaceSpec{
		keystoneNamespaceBlock(cp), horizonNamespaceBlock(cp), glanceNamespaceBlock(cp),
		placementNamespaceBlock(cp), barbicanNamespaceBlock(cp), neutronNamespaceBlock(cp),
	} {
		if ns == nil || ns.Name == "" || ns.Name == cp.Namespace {
			continue
		}
		if _, dup := seen[ns.Name]; dup {
			continue
		}
		seen[ns.Name] = struct{}{}
		out = append(out, *ns)
	}
	return out
}

// KeystoneTargetClusterRef resolves the target cluster the Keystone service —
// and everything that follows it: its database, its cache, its credential
// material — is placed on. It is nil when the service block carries no ref (and
// when there is no service block at all), which is the default: the service
// stays on the local cluster, the management cluster the operator runs on.
func (cp *ControlPlane) KeystoneTargetClusterRef() *commonv1.TargetClusterRefSpec {
	if ks := cp.Spec.Services.Keystone; ks != nil {
		return ks.TargetClusterRef
	}
	return nil
}

// HorizonTargetClusterRef resolves the target cluster the Horizon dashboard —
// and the cache and secret material that follow it — is placed on. See
// KeystoneTargetClusterRef.
func (cp *ControlPlane) HorizonTargetClusterRef() *commonv1.TargetClusterRefSpec {
	if hz := cp.Spec.Services.Horizon; hz != nil {
		return hz.TargetClusterRef
	}
	return nil
}

// GlanceTargetClusterRef resolves the target cluster the Glance service — and
// the database, cache, and credential material that follow it — is placed on.
// See KeystoneTargetClusterRef.
func (cp *ControlPlane) GlanceTargetClusterRef() *commonv1.TargetClusterRefSpec {
	if gl := cp.Spec.Services.Glance; gl != nil {
		return gl.TargetClusterRef
	}
	return nil
}

// PlacementTargetClusterRef resolves the target cluster the Placement service —
// and the database, cache, and credential material that follow it — is placed
// on. See KeystoneTargetClusterRef.
func (cp *ControlPlane) PlacementTargetClusterRef() *commonv1.TargetClusterRefSpec {
	if pl := cp.Spec.Services.Placement; pl != nil {
		return pl.TargetClusterRef
	}
	return nil
}

// BarbicanTargetClusterRef resolves the target cluster the Barbican service —
// and the database, cache, and credential material that follow it — is placed
// on. See KeystoneTargetClusterRef.
func (cp *ControlPlane) BarbicanTargetClusterRef() *commonv1.TargetClusterRefSpec {
	if bn := cp.Spec.Services.Barbican; bn != nil {
		return bn.TargetClusterRef
	}
	return nil
}

// NeutronTargetClusterRef resolves the target cluster the network service, and
// the database, cache, and credential material that follow it, is placed on. See
// KeystoneTargetClusterRef.
func (cp *ControlPlane) NeutronTargetClusterRef() *commonv1.TargetClusterRefSpec {
	if nt := cp.Spec.Services.Neutron; nt != nil {
		return nt.TargetClusterRef
	}
	return nil
}

// NeutronOVNCentralNamespace resolves the namespace the OVNCentral named by
// services.neutron.ovn.centralRef lives in: the namespace on the ref when it
// carries one, and the ControlPlane's own namespace otherwise. That is the same
// resolution the defaulting webhook writes into the ref, so a CR that bypassed
// admission resolves to the same central as one that went through it. A
// ControlPlane without a neutron block resolves to its own namespace.
func (cp *ControlPlane) NeutronOVNCentralNamespace() string {
	if nt := cp.Spec.Services.Neutron; nt != nil && nt.OVN.CentralRef.Namespace != "" {
		return nt.OVN.CentralRef.Namespace
	}
	return cp.Namespace
}

// TargetClusterNames returns the names of the target clusters the ControlPlane
// places services on, deduplicated and in a stable order (keystone, horizon,
// glance, placement, barbican, neutron — first occurrence wins), mirroring
// DedicatedServiceNamespaces one level up.
//
// Two services placed on one cluster yield ONE entry: the enumeration answers
// "which clusters does this ControlPlane reach", not "where does each service
// go" (the per-service accessors above answer that). A ControlPlane that places
// nothing returns an empty result, the default local-only shape.
func (cp *ControlPlane) TargetClusterNames() []string {
	var out []string
	seen := map[string]struct{}{}
	for _, ref := range []*commonv1.TargetClusterRefSpec{
		cp.KeystoneTargetClusterRef(), cp.HorizonTargetClusterRef(), cp.GlanceTargetClusterRef(),
		cp.PlacementTargetClusterRef(), cp.BarbicanTargetClusterRef(), cp.NeutronTargetClusterRef(),
	} {
		if ref == nil || ref.Name == "" {
			continue
		}
		if _, dup := seen[ref.Name]; dup {
			continue
		}
		seen[ref.Name] = struct{}{}
		out = append(out, ref.Name)
	}
	return out
}

func init() {
	SchemeBuilder.Register(&ControlPlane{}, &ControlPlaneList{})
}
