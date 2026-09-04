// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esmetav1 "github.com/external-secrets/external-secrets/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

const (
	// esoTenantStoreName is the namespaced SecretStore the operator provisions
	// per ControlPlane and defaults the control plane onto. It MUST match the
	// name set by deploy/openbao/bootstrap/setup-eso-tenant.sh (the manual
	// onboarding path for standalone Keystone/Horizon) so both provisioning
	// routes converge on the same store, and it is the name standalone CRs set in
	// spec.secretStoreRef.
	esoTenantStoreName = "openbao-tenant-store"
	// esoTenantServiceAccountName is the fixed name of the ServiceAccount the
	// tenant SecretStore authenticates as. The eso-tenant OpenBao role binds this
	// SA name in ANY namespace (setup-auth.sh); the eso-tenant templated policy
	// then confines the token to the caller's OWN namespace, so a fixed name is
	// safe under the one-ControlPlane-per-namespace admission contract.
	esoTenantServiceAccountName = "eso-tenant-auth" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
	// esoTenantClientCertName is the fixed name of the per-ControlPlane
	// cert-manager Certificate / Secret carrying the mTLS client keypair (plus the
	// CA under ca.crt) the tenant SecretStore presents to the OpenBao listener.
	// Fixed to match setup-eso-tenant.sh.
	esoTenantClientCertName = "eso-tenant-client-tls" //nolint:gosec // G101 false positive: cert name, not a credential.
	// esoTenantVaultRole is the OpenBao Kubernetes-auth role the tenant store
	// authenticates against (see setup-auth.sh); it is bound to the eso-tenant
	// templated policy scoping every readable/writable path to the caller's own
	// namespace.
	esoTenantVaultRole = "eso-tenant"
	// esoTenantKVMountPath is the KV-v2 secrets-engine mount the tenant store
	// reads from, matching the shared cluster store (deploy/eso/clustersecretstore.yaml).
	esoTenantKVMountPath = "kv-v2"
	// esoTenantClientCertDuration / renewBefore reuse the DB-credential client
	// certificate lifetimes (dbCredentialClientCert*) so client-cert rotation
	// cadences stay aligned by construction rather than by two literals that can
	// silently drift.
	esoTenantClientCertDuration    = dbCredentialClientCertDuration
	esoTenantClientCertRenewBefore = dbCredentialClientCertRenewBefore
)

// effectiveControlPlaneStoreRef resolves the store the ControlPlane's own
// ExternalSecrets/PushSecrets — and the ref projected onto its Keystone/Horizon
// children — route through. An explicit spec.secretStoreRef is an override and
// wins (normalised via secrets.EffectiveStoreRef so an empty kind resolves to
// ClusterSecretStore); when omitted the operator DEFAULTS to the per-tenant
// namespaced SecretStore (openbao-tenant-store) it provisions in the child
// namespace, so a control plane reaches OpenBao as its own tenant identity
// rather than the shared cluster store. This deliberately differs from
// secrets.EffectiveStoreRef, whose nil default stays the shared cluster store —
// that default is for standalone Keystone/Horizon, which have no operator above
// them to provision a tenant store and onboard the per-tenant identity manually.
func effectiveControlPlaneStoreRef(cp *c5c3v1alpha1.ControlPlane) commonv1.SecretStoreRefSpec {
	if cp.Spec.SecretStoreRef != nil {
		return secrets.EffectiveStoreRef(cp.Spec.SecretStoreRef)
	}
	return commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced,
		Name: esoTenantStoreName,
	}
}

// effectiveControlPlaneStoreRefPtr returns effectiveControlPlaneStoreRef as an
// independent pointer suitable for projection onto a child CR's
// spec.secretStoreRef. The projected ref is always concrete (never nil), so a
// child never falls back to its own shared-store default — the control plane's
// store selection is authoritative.
func effectiveControlPlaneStoreRefPtr(cp *c5c3v1alpha1.ControlPlane) *commonv1.SecretStoreRefSpec {
	ref := effectiveControlPlaneStoreRef(cp)
	return &ref
}

// esoTenantServiceAccount builds the ServiceAccount the tenant SecretStore in
// namespace authenticates as. PURE builder: the reconciler claims ownership in
// the apply path.
//
// One is provisioned per namespace the ControlPlane occupies. The fixed name is
// safe because the eso-tenant OpenBao role binds it in ANY namespace and the
// templated policy then confines the token to the caller's OWN namespace — and
// because a namespace belongs to at most one ControlPlane (the webhook's
// namespace-claim check), so two ControlPlanes can never contend for this name.
func esoTenantServiceAccount(namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: esoTenantServiceAccountName, Namespace: namespace},
	}
}

// applyESOTenantCertificateSpec sets the desired spec fields on the tenant mTLS
// client Certificate, mirroring applyDBCredentialCertificateSpec. Extracted so
// the CreateOrUpdate mutate closure can re-assert the spec on an existing object
// without clobbering cert-manager-managed status.
func applyESOTenantCertificateSpec(u *unstructured.Unstructured, namespace string) {
	// SetNested* only errors on a type conflict at an existing path; on a
	// freshly-built or Certificate-typed object the writes cannot fail, so the
	// errors are intentionally ignored here.
	_ = unstructured.SetNestedField(u.Object, esoTenantClientCertName, "spec", "secretName")
	_ = unstructured.SetNestedField(u.Object, esoTenantClientCertDuration, "spec", "duration")
	_ = unstructured.SetNestedField(u.Object, esoTenantClientCertRenewBefore, "spec", "renewBefore")
	_ = unstructured.SetNestedStringSlice(u.Object, []string{"client auth"}, "spec", "usages")
	_ = unstructured.SetNestedField(u.Object, esoTenantClientCertName+"."+namespace+".svc", "spec", "commonName")
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{
		"name": openBaoCAIssuerName,
		"kind": "ClusterIssuer",
	}, "spec", "issuerRef")
}

// esoTenantSecretStore builds the namespaced per-ControlPlane SecretStore that
// authenticates to OpenBao as the eso-tenant role with the eso-tenant-auth SA.
// All Secret references are same-namespace (a namespaced SecretStore resolves
// them locally): the client cert/key and CA all come from the tenant client
// Certificate's Secret. server and mountPath are the OpenBao connection resolved
// from the SHARED cluster store (the tenant store cannot describe its own
// bootstrap). Mirrors deploy/openbao/bootstrap/setup-eso-tenant.sh.
func esoTenantSecretStore(namespace, server, mountPath string) *esov1.SecretStore {
	return &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: namespace},
		Spec: esov1.SecretStoreSpec{
			Provider: &esov1.SecretStoreProvider{
				Vault: &esov1.VaultProvider{
					Server: server,
					Path:   ptr.To(esoTenantKVMountPath),
					// Version has no omitempty, so leaving it unset serializes as
					// "" and the ESO CRD enum (v1|v2) rejects the object — the CRD
					// default only applies to ABSENT fields. Pin the ESO default.
					Version: esov1.VaultKVStoreV2,
					Auth: &esov1.VaultAuth{
						Kubernetes: &esov1.VaultKubernetesAuth{
							Path:              mountPath,
							Role:              esoTenantVaultRole,
							ServiceAccountRef: &esmetav1.ServiceAccountSelector{Name: esoTenantServiceAccountName},
						},
					},
					ClientTLS: esov1.VaultClientTLS{
						CertSecretRef: &esmetav1.SecretKeySelector{Name: esoTenantClientCertName},
						KeySecretRef:  &esmetav1.SecretKeySelector{Name: esoTenantClientCertName},
					},
					CAProvider: &esov1.CAProvider{
						Type: esov1.CAProviderTypeSecret,
						Name: esoTenantClientCertName,
						Key:  "ca.crt",
					},
				},
			},
		},
	}
}

// reconcileESOTenantStore provisions the in-cluster half of the ControlPlane's
// per-tenant OpenBao identity — the eso-tenant-auth ServiceAccount, the tenant
// mTLS client Certificate, and the namespaced openbao-tenant-store SecretStore —
// and drives ESOTenantStoreReady from that SecretStore's Ready condition. It is
// the enforced default: every ControlPlane that omits spec.secretStoreRef routes
// its (and its children's) secret traffic through this per-tenant store, so
// OpenBao itself enforces cross-tenant isolation via the templated eso-tenant
// policy rather than a naming convention. Registered ahead of the store-consuming
// sub-reconcilers (DBCredentials, AdminPassword, Keystone, ...) so the store
// exists before they gate on it.
//
// OVERRIDE: a ControlPlane that sets an explicit spec.secretStoreRef opts out of
// the operator-provisioned store — it owns the referenced store's lifecycle — so
// the operator provisions nothing and reports ESOTenantStoreReady=True with the
// StoreRefOverridden reason. The store-consuming sub-reconcilers still gate on
// the selected store's own readiness.
func (r *ControlPlaneReconciler) reconcileESOTenantStore(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if cp.Spec.SecretStoreRef != nil {
		ref := secrets.EffectiveStoreRef(cp.Spec.SecretStoreRef)
		logger.Info("explicit secretStoreRef set; not provisioning the per-tenant store",
			"kind", ref.Kind, "name", ref.Name)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeESOTenantStoreReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "StoreRefOverridden",
			Message: fmt.Sprintf("explicit spec.secretStoreRef (%s %q) overrides the operator-provisioned per-tenant "+
				"store; its readiness is gated by the store-consuming sub-reconcilers", ref.Kind, ref.Name),
		})
		return ctrl.Result{}, nil
	}

	// Each namespace's trio is written to the clusters that namespace's delivery
	// paths read it from (see esoTenantStoreClusters). A tenant store has to
	// authenticate from the cluster whose ESO materialises the Secrets, and its
	// client certificate has to be issued by the cert-manager there. Every cluster
	// is resolved before the first write, so one that does not resolve leaves the
	// whole ensemble unwritten.
	namespaces := controlPlaneNamespaces(cp)
	children, err := r.childrenClientsFor(ctx, cp, namespaces...)
	if err != nil {
		conditionFailer(cp, conditionTypeESOTenantStoreReady)(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: esoTenantStoreRequeueAfter}, nil
	}

	if err := r.ensureESOTenantStoreObjects(ctx, cp, children); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeESOTenantStoreReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ProvisioningError",
			Message:            fmt.Sprintf("ensuring per-tenant secret store objects: %v", err),
		})
		return ctrl.Result{}, err
	}

	// EVERY store must be Ready, not just the one in the ControlPlane's namespace:
	// a service placed in a namespace of its own materialises its secret material
	// through THAT namespace's store, so a store still issuing its client cert
	// there is as load-bearing as the one at home. The gate covers exactly the
	// copies that were written, each read from the cluster it was written to,
	// because each carries a delivery path of its own. The first unready store is
	// named with its namespace and its cluster, so the condition says which one to
	// look at and where.
	for _, ns := range namespaces {
		for _, c := range r.esoTenantStoreClusters(cp, ns, children[ns]) {
			ready, err := secrets.IsSecretStoreReady(ctx, c, esoTenantStoreName, ns)
			if err != nil {
				return ctrl.Result{}, err
			}
			if ready {
				continue
			}
			cluster := "management cluster"
			if commonmulticluster.IsRemote(c) {
				cluster = "target cluster"
			}
			logger.Info("per-tenant secret store not ready yet, requeuing", "namespace", ns, "cluster", cluster)
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeESOTenantStoreReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "SecretStoreNotReady",
				Message: fmt.Sprintf("per-tenant SecretStore %q in namespace %q on the %s is not ready yet; waiting "+
					"on cert issuance and the OpenBao backend", esoTenantStoreName, ns, cluster),
			})
			return ctrl.Result{RequeueAfter: esoTenantStoreRequeueAfter}, nil
		}
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeESOTenantStoreReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "ESOTenantStoreReady",
		Message: fmt.Sprintf("per-tenant SecretStore %q is Ready in all %d namespace(s) of this ControlPlane",
			esoTenantStoreName, len(namespaces)),
	})
	return ctrl.Result{}, nil
}

// ensureESOTenantStoreObjects create-or-updates the ServiceAccount, the mTLS
// client Certificate, and the namespaced SecretStore — in EVERY namespace the
// ControlPlane occupies. Ordering (SA → Certificate → SecretStore) makes the auth
// identity and TLS material exist before the store that references them,
// mirroring ensureDynamicDBCredentialObjects.
//
// SECRET DISTRIBUTION ACROSS NAMESPACES. An ESO SecretStore and the Secrets it
// materialises are namespace-local: a store in the ControlPlane's namespace
// cannot deliver anything into a service namespace. So each namespace hosting a
// service gets its OWN tenant store — its own ServiceAccount, its own client
// certificate, its own SecretStore object. That needs no OpenBao-side change: the
// eso-tenant role binds the SA name in ANY namespace and its templated policy
// scopes every path to the caller's own namespace, so each store authenticates as
// its own tenant identity and reaches only its own paths.
//
// In the ControlPlane's own namespace the objects are owner-referenced and the GC
// cascade reaps them; elsewhere they carry the ownership labels and the teardown
// deletes them explicitly.
//
// children holds the client each namespace resolved to, from which
// esoTenantStoreClusters derives the clusters its trio is written to. The caller
// resolves all of them before anything is written (see reconcileESOTenantStore).
func (r *ControlPlaneReconciler) ensureESOTenantStoreObjects(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, children map[string]client.Client,
) error {
	// server/mountPath come from the SHARED cluster store, not the tenant stores
	// this method is building — a tenant store cannot describe its own OpenBao
	// connection. openBaoConnection falls back to the documented defaults when the
	// shared store is unreadable, which match the tenant stores' connection anyway.
	// Every tenant store copies the same connection by construction, so it is
	// resolved once for all of them.
	server, mountPath := r.openBaoConnection(ctx, cp, secrets.EffectiveStoreRef(nil))

	for _, ns := range controlPlaneNamespaces(cp) {
		for _, c := range r.esoTenantStoreClusters(cp, ns, children[ns]) {
			if err := r.ensureESOTenantStoreTrioIn(ctx, c, cp, ns, server, mountPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// esoTenantStoreClusters returns the clusters ns's tenant-store trio is written
// to and gated on. An unplaced namespace exists at home alone, which is what
// clustersFor answers for it.
//
// A PLACED namespace exists on both clusters, but it only needs a store on both
// where it has a delivery path on both. Every path that follows the service to
// its cluster (the admin password, the Keystone and per-service DB credentials)
// resolves the store through childrenClientFor and reads it THERE. The one
// exception is a KeystoneService registration: its CR lives on the management
// cluster beside the ControlPlane, in the namespace of the service it registers,
// and its controller gates delivery on a SecretStore read locally in that
// namespace. So a placed namespace hosting a registration gets the home copy as
// well, and a placed Keystone or Horizon namespace does not — a store nothing
// reads would still be certificate-issued, ESO-validated against OpenBao for the
// life of the plane, and, sitting in the pipeline's blocking prefix, would park
// the whole chain behind its readiness.
func (r *ControlPlaneReconciler) esoTenantStoreClusters(
	cp *c5c3v1alpha1.ControlPlane, ns string, children client.Client,
) []client.Client {
	if !commonmulticluster.IsRemote(children) || hostsHomeRegistration(cp, ns) {
		return r.clustersFor(children)
	}
	return []client.Client{children}
}

// hostsHomeRegistration reports whether ns can host a KeystoneService
// registration, which is the same question as whether something reads its tenant
// store on the MANAGEMENT cluster: a registration is reconciled where its CR
// lives, and keystoneServiceStoreReady resolves the store through the home client
// in the CR's own namespace.
//
// Two kinds land in a namespace this ControlPlane occupies. The registrations it
// projects for its built-in services go to the namespace of the service they
// register. A THIRD-PARTY one goes to any namespace the serviceRegistrations
// allowlist admits — and that allowlist may name a namespace this ControlPlane
// also placed a service in (validateServiceRegistrations deliberately forbids
// nothing of the sort), which is precisely the case
// reconcileRegistrationTenantStores excludes as already covered here.
//
// An undeclared built-in service resolves to the ControlPlane's own namespace,
// which is never a placed one, so those arms are only ever consulted for a
// service that has a namespace of its own.
func hostsHomeRegistration(cp *c5c3v1alpha1.ControlPlane, ns string) bool {
	if ns == cp.GlanceNamespace() || ns == cp.PlacementNamespace() || ns == cp.BarbicanNamespace() ||
		ns == cp.NeutronNamespace() {
		return true
	}
	sr := cp.Spec.KORC.ServiceRegistrations
	return sr != nil && slices.Contains(sr.AllowedNamespaces, ns)
}

// ensureESOTenantStoreTrioIn create-or-updates ONE namespace's tenant-store trio
// with the client c, in the order the ensemble depends on: the auth identity and
// the TLS material before the store that references them.
//
// It is shared by the two sub-reconcilers that provision the trio — the
// ControlPlane's own namespaces (ensureESOTenantStoreObjects) and the allowlisted
// registration namespaces (reconcileRegistrationTenantStores) — so the ownership
// mechanism, the adoption refusal and the error wording are written once. server
// and mountPath are the OpenBao connection the caller resolved from the shared
// cluster store; every trio copies the same one.
func (r *ControlPlaneReconciler) ensureESOTenantStoreTrioIn(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, namespace, server, mountPath string,
) error {
	if err := r.ensureUnownedOrOwned(ctx, c, cp, esoTenantServiceAccount(namespace)); err != nil {
		return fmt.Errorf("ensuring per-tenant ServiceAccount in namespace %q: %w", namespace, err)
	}

	// The cert-manager Certificate is handled as an unstructured object (no Go
	// module ships its type), and apply.EnsureObject converts typed structs, not
	// unstructured inputs — so this projection stays read-modify-write while the
	// typed sibling objects around it use Server-Side Apply.
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(certificateGVK)
	live.SetName(esoTenantClientCertName)
	live.SetNamespace(namespace)
	if _, err := controllerutil.CreateOrUpdate(ctx, c, live, func() error {
		if err := refuseForeignAdoption(c, cp, live, r.Scheme); err != nil {
			return err
		}
		applyESOTenantCertificateSpec(live, namespace)
		return claimChildOwnership(c, cp, live, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring per-tenant client Certificate in namespace %q: %w", namespace, err)
	}

	if err := r.ensureUnownedOrOwned(ctx, c, cp, esoTenantSecretStore(namespace, server, mountPath)); err != nil {
		return fmt.Errorf("ensuring per-tenant SecretStore in namespace %q: %w", namespace, err)
	}
	return nil
}

// reconcileRegistrationTenantStores provisions the tenant-store trio in every
// ALLOWLISTED namespace hosting a KeystoneService registered against this control
// plane, and collects it again once the last registration there is gone. It drives
// RegistrationTenantStoresReady.
//
// It exists because credential delivery is namespace-local. A registration's
// consumer Secret is materialised through the tenant store in the CR's OWN
// namespace (keystoneServiceStoreReady), and reconcileESOTenantStore provisions one
// only in the namespaces the control plane occupies. Without this, an allowlisted
// foreign registration mints its Keystone account and then waits forever on a store
// nobody creates.
//
// It is a SEPARATE sub-reconciler in the tail group rather than an arm of
// reconcileESOTenantStore, and that separation is the point: the tenant-store step
// runs in the chain's BLOCKING PREFIX, where a non-zero result parks DBCredentials,
// AdminPassword and Keystone behind it. These namespaces are not the operator's —
// a foreign object can occupy the store's name, a certificate can fail to issue
// there — and none of that may reach the plane's core reconciliation. A failure
// here surfaces in this condition (and, through it, in the aggregate Ready) and
// stops there.
//
// The OpenBao side needs nothing: the eso-tenant role binds the ServiceAccount name
// in ANY namespace and its templated policy confines each token to its own, so a
// trio in a foreign namespace authenticates as that namespace's tenant identity and
// reaches only its own paths.
func (r *ControlPlaneReconciler) reconcileRegistrationTenantStores(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fail := conditionFailer(cp, conditionTypeRegistrationTenantStoresReady)

	// An explicit store reference opts the whole control plane out of
	// operator-provisioned stores, so there is nothing to provision — and nothing
	// this sub-reconciler ever created to collect either.
	if cp.Spec.SecretStoreRef != nil {
		ref := secrets.EffectiveStoreRef(cp.Spec.SecretStoreRef)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeRegistrationTenantStoresReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "StoreRefOverridden",
			Message: fmt.Sprintf("explicit spec.secretStoreRef (%s %q) overrides the operator-provisioned per-tenant "+
				"store; a registration resolves that store in its own namespace itself", ref.Kind, ref.Name),
		})
		return ctrl.Result{}, nil
	}

	// What this control plane already provisioned outside its own namespaces, found
	// by ownership label. Enumerated BEFORE the allowlist is consulted, so emptying
	// the allowlist still collects the trios whose registrations have left.
	provisioned, err := r.registrationTenantStoreNamespaces(ctx, cp)
	if err != nil {
		fail("ProvisioningError", err.Error())
		return ctrl.Result{}, err
	}

	var allowed []string
	if sr := cp.Spec.KORC.ServiceRegistrations; sr != nil {
		allowed = sr.AllowedNamespaces
	}

	// Nothing admitted and nothing standing: return before the registration List
	// below, so a control plane that never uses the feature never depends on the
	// field index the KeystoneService controller registers.
	if len(allowed) == 0 && len(provisioned) == 0 {
		setRegistrationTenantStoresReady(cp, "NoRegistrationNamespaces", noRegistrationNamespacesMessage)
		return ctrl.Result{}, nil
	}

	// Registrations per namespace. The count drives both halves: a namespace with
	// registrations is provisioned, one without them is collected.
	registrations, err := r.registrationCountsByNamespace(ctx, cp)
	if err != nil {
		fail("ProvisioningError", err.Error())
		return ctrl.Result{}, err
	}

	occupied := controlPlaneNamespaces(cp)
	var targets []string
	for ns := range registrations {
		if slices.Contains(occupied, ns) || !slices.Contains(allowed, ns) {
			continue
		}
		targets = append(targets, ns)
	}
	slices.Sort(targets)

	// Provision. Every namespace is attempted even after one fails, so a single
	// broken namespace cannot starve its peers; the failures are reported together.
	//
	// A failure here reports the condition and REQUEUES rather than returning an
	// error. The cause is usually a tenant-side one that no backoff resolves — a
	// foreign object holding the store's name — and returning an error would put the
	// whole ControlPlane reconcile into exponential backoff for it, which is the
	// blast radius this sub-reconciler is separate in order to avoid.
	server, mountPath := r.openBaoConnection(ctx, cp, secrets.EffectiveStoreRef(nil))
	var failed []string
	var errs []error
	for _, ns := range targets {
		if err := r.ensureESOTenantStoreTrioIn(ctx, r.Client, cp, ns, server, mountPath); err != nil {
			failed = append(failed, ns)
			errs = append(errs, err)
		}
	}

	// Collect what the spec no longer asks for. A namespace that still holds
	// registrations is FROZEN rather than collected, even when the allowlist stopped
	// admitting it: de-listing is an admission gate, and revoking the store under a
	// running service would destroy credentials it depends on.
	for _, ns := range provisioned {
		if slices.Contains(targets, ns) {
			continue
		}
		if registrations[ns] > 0 {
			logger.V(1).Info("namespace is no longer admitted but still hosts registrations; keeping its per-tenant store",
				"namespace", ns, "registrations", registrations[ns])
			continue
		}
		logger.Info("collecting the per-tenant store of a namespace with no service registrations left", "namespace", ns)
		if err := r.deleteESOTenantStoreTrioIn(ctx, cp, ns); err != nil {
			failed = append(failed, ns)
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		fail("ProvisioningError", fmt.Sprintf("registration tenant stores in namespace(s) %s: %v",
			strings.Join(failed, ", "), errors.Join(errs...)))
		return ctrl.Result{RequeueAfter: esoTenantStoreRequeueAfter}, nil
	}

	if len(targets) == 0 {
		setRegistrationTenantStoresReady(cp, "NoRegistrationNamespaces", noRegistrationNamespacesMessage)
		return ctrl.Result{}, nil
	}

	// Every store must be Ready, not just written: a registration's delivery leg
	// gates on exactly this, so reporting True while a store is still issuing its
	// client certificate would claim a delivery path that does not carry yet.
	for _, ns := range targets {
		ready, err := secrets.IsSecretStoreReady(ctx, r.Client, esoTenantStoreName, ns)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			logger.Info("registration tenant store not ready yet, requeuing", "namespace", ns)
			fail("SecretStoreNotReady", fmt.Sprintf(
				"per-tenant SecretStore %q in registration namespace %q is not ready yet; waiting on cert issuance "+
					"and the OpenBao backend", esoTenantStoreName, ns))
			return ctrl.Result{RequeueAfter: esoTenantStoreRequeueAfter}, nil
		}
	}

	setRegistrationTenantStoresReady(cp, "RegistrationTenantStoresReady", fmt.Sprintf(
		"per-tenant SecretStore %q is Ready in all %d allowlisted registration namespace(s)",
		esoTenantStoreName, len(targets)))
	return ctrl.Result{}, nil
}

// noRegistrationNamespacesMessage is the True/NoRegistrationNamespaces message.
// Two arms reach it — nothing admitted at all, and an allowlist no registration
// sits in — and they report the same state, so they share one literal rather than
// two that can drift.
const noRegistrationNamespacesMessage = "no allowlisted namespace hosts a service registration; " +
	"no per-tenant store is provisioned outside this control plane's own namespaces"

// setRegistrationTenantStoresReady upserts the True arms of the condition. The
// False arms go through conditionFailer, which truncates for them.
func setRegistrationTenantStoresReady(cp *c5c3v1alpha1.ControlPlane, reason, message string) {
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeRegistrationTenantStoresReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// registrationTenantStoreNamespaces returns the namespaces OUTSIDE the ones the
// control plane occupies where it has already provisioned a tenant store, sorted
// and deduplicated.
//
// The label selector IS the ownership test here: the trio of a namespace the
// control plane does not own carries the ownership labels rather than an owner
// reference (Kubernetes forbids one across namespaces), and the store in its OWN
// namespace is owner-referenced and label-less, so it cannot appear in this list at
// all. The namespaces it occupies are excluded on top, because the blocking
// tenant-store step owns those.
func (r *ControlPlaneReconciler) registrationTenantStoreNamespaces(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]string, error) {
	var stores esov1.SecretStoreList
	if err := r.List(ctx, &stores, client.MatchingLabels(controlPlaneChildLabels(cp))); err != nil {
		return nil, fmt.Errorf("listing the provisioned registration tenant stores: %w", err)
	}
	occupied := controlPlaneNamespaces(cp)
	var namespaces []string
	for i := range stores.Items {
		store := &stores.Items[i]
		if store.Name != esoTenantStoreName || slices.Contains(occupied, store.Namespace) {
			continue
		}
		namespaces = append(namespaces, store.Namespace)
	}
	slices.Sort(namespaces)
	return slices.Compact(namespaces), nil
}

// registrationCountsByNamespace returns how many KeystoneService CRs registered
// against cp live in each namespace, resolved through the field index the
// KeystoneService controller registers (KeystoneServiceControlPlaneRefIndexKey), so
// a registration naming a same-named control plane in another namespace is never
// counted here.
//
// A CR that is Terminating still counts. Its own teardown runs its ESO cleanup
// through this very store, so collecting the store while the CR is still going
// would strand the OpenBao path it is in the middle of purging.
func (r *ControlPlaneReconciler) registrationCountsByNamespace(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (map[string]int, error) {
	var registrations c5c3v1alpha1.KeystoneServiceList
	if err := r.List(ctx, &registrations, client.MatchingFields{
		KeystoneServiceControlPlaneRefIndexKey: cp.Namespace + "/" + cp.Name,
	}); err != nil {
		return nil, fmt.Errorf("listing KeystoneServices for registration tenant stores: %w", err)
	}
	counts := make(map[string]int, len(registrations.Items))
	for i := range registrations.Items {
		counts[registrations.Items[i].Namespace]++
	}
	return counts, nil
}

// deleteESOTenantStoreTrioIn removes one registration namespace's trio, in the
// reverse of the order it was created: the store first, then the identity and the
// TLS material it authenticated with, so nothing is left authenticating through
// material that is already gone.
//
// Each object is ownership-checked against its LIVE state, so a same-named object
// belonging to somebody else in this shared namespace is left alone. The reads go
// through the UNCACHED reader: a cached Get on a ServiceAccount would have
// controller-runtime start a cluster-wide ServiceAccount informer (see
// ControlPlaneReconciler.APIReader and adoptionPrecheckReader), and a collection
// runs only when a namespace's last registration has left, so the round trips are
// rare.
func (r *ControlPlaneReconciler) deleteESOTenantStoreTrioIn(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, namespace string,
) error {
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(certificateGVK)
	certificate.SetName(esoTenantClientCertName)
	certificate.SetNamespace(namespace)

	objs := []client.Object{
		&esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: namespace}},
		certificate,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: esoTenantServiceAccountName, Namespace: namespace}},
	}

	var errs []error
	for _, obj := range objs {
		key := client.ObjectKeyFromObject(obj)
		switch err := r.apiReader().Get(ctx, key, obj); {
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("reading %T %s before collecting it: %w", obj, key, err))
			continue
		}
		if !isControlPlaneChild(obj, cp) {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			errs = append(errs, fmt.Errorf("collecting %T %s: %w", obj, key, err))
		}
	}
	return errors.Join(errs...)
}
