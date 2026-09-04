// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the per-ControlPlane ESO-tenant-store sub-reconciler
// reconcileESOTenantStore and its pure builders/helpers. The tests cover default
// provisioning (nil secretStoreRef) and its not-ready gate, the Ready pass, the
// provisioning-error path, the explicit-ref override that provisions nothing, and
// the effectiveControlPlaneStoreRef resolution table.
package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// readyTenantStoreFor returns the operator-provisioned per-tenant SecretStore in
// cp's child namespace with a Ready status, so a store-gated sub-reconciler under
// test passes the ESOTenantStore gate without an ESO controller — the default
// store a nil-ref ControlPlane resolves to. The provider is bare so
// openBaoConnection falls back to the documented defaults.
func readyTenantStoreFor(cp *c5c3v1alpha1.ControlPlane) *esov1.SecretStore {
	return readyTenantSecretStore(esoTenantStoreName, childNamespace(cp), "", "")
}

// getTenantStore fetches the operator-provisioned tenant SecretStore.
func getTenantStore(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esov1.SecretStore, error) {
	t.Helper()
	store := &esov1.SecretStore{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: esoTenantStoreName}, store)
	return store, err
}

// esoTenantCondition returns the ESOTenantStoreReady condition off the CR.
func esoTenantCondition(cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeESOTenantStoreReady)
}

// TestReconcileESOTenantStore_ProvisionsObjects a managed CP with no explicit
// secretStoreRef drives reconcileESOTenantStore to provision the eso-tenant-auth
// ServiceAccount, the eso-tenant-client-tls Certificate, and the
// openbao-tenant-store SecretStore — all owner-referenced to the ControlPlane —
// with the OpenBao connection copied from the shared cluster store. While the
// store is not Ready the sub-reconciler requeues with ESOTenantStoreReady=False.
func TestReconcileESOTenantStore_ProvisionsObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	// A custom shared-store provider so we can assert openBaoConnection is sourced
	// from the SHARED store, never the tenant store this reconciler builds.
	sharedStore := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName},
		Spec: esov1.SecretStoreSpec{Provider: &esov1.SecretStoreProvider{Vault: &esov1.VaultProvider{
			Server: "https://openbao.example.svc:8200",
			Auth:   &esov1.VaultAuth{Kubernetes: &esov1.VaultKubernetesAuth{Path: "kubernetes/management"}},
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, sharedStore).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(), "provisioning must not error")
	g.Expect(res.RequeueAfter).To(Equal(esoTenantStoreRequeueAfter), "must requeue while the store is not Ready")

	// ServiceAccount.
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: esoTenantServiceAccountName}, sa)).To(Succeed())
	g.Expect(metav1.GetControllerOf(sa)).NotTo(BeNil(), "SA must be owner-referenced to the ControlPlane")

	// Certificate (unstructured cert-manager GVK).
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: esoTenantClientCertName}, cert)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal(openBaoCAIssuerName))
	usages, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "usages")
	g.Expect(usages).To(ContainElement("client auth"))
	// secretName is the store↔cert linchpin: cert-manager writes the keypair (plus
	// ca.crt) into this Secret, and the store's CertSecretRef/KeySecretRef/CAProvider
	// all read esoTenantClientCertName below — a divergent secretName would leave the
	// store authenticating against a Secret cert-manager never populates.
	secretName, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	g.Expect(secretName).To(Equal(esoTenantClientCertName))
	g.Expect(metav1.GetControllerOf(cert)).NotTo(BeNil(), "Certificate must be owner-referenced")

	// SecretStore: vault provider authenticating as the eso-tenant role/SA, with
	// the server/mount copied from the SHARED store.
	store, err := getTenantStore(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the tenant SecretStore")
	g.Expect(store.Spec.Provider).NotTo(BeNil())
	g.Expect(store.Spec.Provider.Vault).NotTo(BeNil())
	g.Expect(store.Spec.Provider.Vault.Server).To(Equal("https://openbao.example.svc:8200"),
		"server must be sourced from the shared cluster store, not the tenant store")
	g.Expect(store.Spec.Provider.Vault.Path).NotTo(BeNil())
	g.Expect(*store.Spec.Provider.Vault.Path).To(Equal(esoTenantKVMountPath))
	g.Expect(store.Spec.Provider.Vault.Version).To(Equal(esov1.VaultKVStoreV2),
		"version must be set explicitly — no omitempty, so \"\" fails the CRD enum")
	g.Expect(store.Spec.Provider.Vault.Auth.Kubernetes.Path).To(Equal("kubernetes/management"))
	g.Expect(store.Spec.Provider.Vault.Auth.Kubernetes.Role).To(Equal(esoTenantVaultRole))
	g.Expect(store.Spec.Provider.Vault.Auth.Kubernetes.ServiceAccountRef.Name).To(Equal(esoTenantServiceAccountName))
	g.Expect(store.Spec.Provider.Vault.CAProvider.Name).To(Equal(esoTenantClientCertName))
	g.Expect(store.Spec.Provider.Vault.CAProvider.Key).To(Equal("ca.crt"))
	g.Expect(store.Spec.Provider.Vault.ClientTLS.CertSecretRef.Name).To(Equal(esoTenantClientCertName))
	g.Expect(store.Spec.Provider.Vault.ClientTLS.KeySecretRef.Name).To(Equal(esoTenantClientCertName))
	g.Expect(metav1.GetControllerOf(store)).NotTo(BeNil(), "SecretStore must be owner-referenced")

	// Condition: not Ready while the store has no Ready status.
	cond := esoTenantCondition(cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
	g.Expect(cond.Message).To(ContainSubstring(esoTenantStoreName))
}

// TestReconcileESOTenantStore_ReadyWhenStoreReady when the tenant SecretStore
// already reports Ready, the sub-reconciler flips ESOTenantStoreReady=True and
// stops requeuing.
func TestReconcileESOTenantStore_ReadyWhenStoreReady(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	// Seed a Ready tenant SecretStore in the child namespace; the operator's
	// Server-Side Apply re-asserts the spec without clobbering the Ready status.
	readyStore := readyTenantSecretStore(esoTenantStoreName, childNamespace(cp), "", "")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyStore).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a Ready store must not requeue")

	cond := esoTenantCondition(cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("ESOTenantStoreReady"))
}

// TestReconcileESOTenantStore_StoreRefOverridden an explicit spec.secretStoreRef
// opts out of the operator-provisioned store: nothing is provisioned and
// ESOTenantStoreReady is True with reason StoreRefOverridden.
func TestReconcileESOTenantStore_StoreRefOverridden(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindCluster,
		Name: "my-own-store",
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "the override path must not requeue")

	// No per-tenant objects were provisioned.
	_, err = getTenantStore(t, r, cp)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no tenant SecretStore must be provisioned under an explicit ref")
	sa := &corev1.ServiceAccount{}
	err = r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: esoTenantServiceAccountName}, sa)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no tenant ServiceAccount must be provisioned under an explicit ref")

	cond := esoTenantCondition(cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("StoreRefOverridden"))
	g.Expect(cond.Message).To(ContainSubstring("my-own-store"))
}

// TestReconcileESOTenantStore_ProvisioningError when provisioning the per-tenant
// objects fails (here the ServiceAccount Server-Side Apply errors), the
// sub-reconciler surfaces the error and reports ESOTenantStoreReady=False with
// reason ProvisioningError — so a failed SA/Certificate/SecretStore apply is
// diagnosable from the CR status rather than silently swallowed.
func TestReconcileESOTenantStore_ProvisioningError(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	// Fail the ServiceAccount apply (the first object ensureESOTenantStoreObjects
	// writes via Server-Side Apply) so ensureESOTenantStoreObjects returns an error.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore()).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return errors.New("apply refused")
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).To(HaveOccurred(), "a failed provisioning apply must surface as an error for backoff")
	g.Expect(res.IsZero()).To(BeTrue(), "the error path returns an empty Result")

	cond := esoTenantCondition(cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ProvisioningError"))
	g.Expect(cond.ObservedGeneration).To(Equal(cp.Generation),
		"ProvisioningError must stamp ObservedGeneration for staleness detection")
}

// TestEffectiveControlPlaneStoreRef the nil default is the namespaced per-tenant
// store, an explicit ref wins, and an explicit ref with an empty Kind normalises
// to ClusterSecretStore (the webhook-bypass safety net).
func TestEffectiveControlPlaneStoreRef(t *testing.T) {
	g := NewGomegaWithT(t)

	// Default (nil) → per-tenant namespaced store.
	def := effectiveControlPlaneStoreRef(&c5c3v1alpha1.ControlPlane{})
	g.Expect(def.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(def.Name).To(Equal(esoTenantStoreName))

	// Explicit override wins.
	cp := &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
		SecretStoreRef: &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindCluster, Name: "shared"},
	}}
	g.Expect(effectiveControlPlaneStoreRef(cp).Name).To(Equal("shared"))
	g.Expect(effectiveControlPlaneStoreRef(cp).Kind).To(Equal(commonv1.SecretStoreKindCluster))

	// Empty-kind explicit ref normalises to the cluster kind.
	cpEmptyKind := &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
		SecretStoreRef: &commonv1.SecretStoreRefSpec{Name: "no-kind"},
	}}
	g.Expect(effectiveControlPlaneStoreRef(cpEmptyKind).Kind).To(Equal(commonv1.SecretStoreKindCluster))
}

// --- per-service namespaces (issue #646) ---

// TestReconcileESOTenantStore_ProvisionsAStorePerNamespace pins the secret-
// distribution mechanism: an ESO SecretStore and the Secrets it materialises are
// namespace-local, so a store in the ControlPlane's namespace cannot deliver
// anything into a service namespace. Each namespace hosting a service therefore
// gets its own tenant store trio — and the one in a service namespace carries the
// ownership labels rather than an owner reference.
func TestReconcileESOTenantStore_ProvisionsAStorePerNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "identity",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, ns := range []string{"openstack", "identity"} {
		store := &esov1.SecretStore{}
		g.Expect(r.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: esoTenantStoreName}, store)).To(Succeed(),
			"every namespace hosting a service needs its own tenant store")

		sa := &corev1.ServiceAccount{}
		g.Expect(r.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: esoTenantServiceAccountName}, sa)).To(Succeed())

		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		g.Expect(r.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: esoTenantClientCertName}, cert)).To(Succeed())
		commonName, _, _ := unstructured.NestedString(cert.Object, "spec", "commonName")
		g.Expect(commonName).To(Equal(esoTenantClientCertName+"."+ns+".svc"),
			"each store's client cert must identify its own namespace")
	}

	// Ownership: owner reference at home, labels abroad.
	home := &esov1.SecretStore{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: "openstack", Name: esoTenantStoreName}, home)).To(Succeed())
	g.Expect(metav1.GetControllerOf(home)).NotTo(BeNil())

	abroad := &esov1.SecretStore{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: "identity", Name: esoTenantStoreName}, abroad)).To(Succeed())
	g.Expect(abroad.OwnerReferences).To(BeEmpty())
	g.Expect(abroad.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "controlplane"))
	g.Expect(abroad.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "openstack"))
}

// TestReconcileESOTenantStore_GatesOnEveryNamespace verifies the readiness gate
// aggregates: a store still issuing its client cert in a SERVICE namespace holds
// the condition False, and the message names that namespace — otherwise the
// admin-password ExternalSecret would be projected through a store that cannot
// deliver it.
func TestReconcileESOTenantStore_GatesOnEveryNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"},
		},
	}

	// Ready at home, absent (hence not ready) in the service namespace.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, readyTenantSecretStore(esoTenantStoreName, "openstack", "", "")).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(esoTenantStoreRequeueAfter))

	cond := esoTenantCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
	g.Expect(cond.Message).To(ContainSubstring(`namespace "identity"`))
}

// TestReconcileESOTenantStore_RefusesForeignStoreInExternalNamespace a SecretStore
// that merely shares the operator's fixed name (openbao-tenant-store) in an
// External-lifecycle service namespace, but carries no ControlPlane ownership
// labels, must NOT be adopted: the operator never created it. Adopting it would
// overwrite its provider spec to point at our OpenBao and, via the labels the
// projection would stamp, make the teardown residue sweep DELETE it. The
// sub-reconciler fails loud (mirroring reconcileNamespaces' NamespaceNotOwned) and
// leaves the foreign object untouched.
func TestReconcileESOTenantStore_RefusesForeignStoreInExternalNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-tenant",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
	}
	// A store somebody else provisioned in the shared namespace, pointing at their
	// own OpenBao and carrying no ControlPlane ownership labels.
	foreign := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      esoTenantStoreName,
			Namespace: "shared-tenant",
			UID:       types.UID("foreign-store-uid"),
			Labels:    map[string]string{"owner": "someone-else"},
		},
		Spec: esov1.SecretStoreSpec{Provider: &esov1.SecretStoreProvider{Vault: &esov1.VaultProvider{
			Server: "https://not-ours.example.svc:8200",
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).To(HaveOccurred(), "must refuse to adopt a foreign store in an unowned namespace")
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt"))

	// Untouched: the foreign store still points where it did and never acquired the
	// ownership labels that would make the teardown sweep delete it.
	after := &esov1.SecretStore{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: "shared-tenant", Name: esoTenantStoreName}, after)).To(Succeed())
	g.Expect(after.Spec.Provider.Vault.Server).To(Equal("https://not-ours.example.svc:8200"),
		"foreign store spec must not be overwritten")
	g.Expect(after.Labels).NotTo(HaveKey(controlPlaneNameLabel))
	g.Expect(after.Labels).NotTo(HaveKey(controlPlaneNamespaceLabel))
	g.Expect(isControlPlaneChild(after, cp)).To(BeFalse(),
		"foreign store must not become a ControlPlane child, so the residue sweep leaves it alone")
}

// TestReconcileESOTenantStore_RefusesForeignCertInExternalNamespace is the
// Certificate twin of the store case: the eso-tenant-client-tls Certificate stays
// read-modify-write via CreateOrUpdate, so its ownership guard lives in
// refuseForeignAdoption. A same-named foreign Certificate in an External namespace
// must not be reshaped or stamped with our labels.
func TestReconcileESOTenantStore_RefusesForeignCertInExternalNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-tenant",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
	}
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(certificateGVK)
	foreign.SetName(esoTenantClientCertName)
	foreign.SetNamespace("shared-tenant")
	foreign.SetUID(types.UID("foreign-cert-uid"))
	foreign.SetLabels(map[string]string{"owner": "someone-else"})
	g.Expect(unstructured.SetNestedField(foreign.Object, "not-ours-issuer", "spec", "issuerRef", "name")).To(Succeed())
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).To(HaveOccurred(), "must refuse to adopt a foreign Certificate in an unowned namespace")
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt"))

	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: "shared-tenant", Name: esoTenantClientCertName}, after)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(after.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal("not-ours-issuer"), "foreign Certificate spec must not be overwritten")
	g.Expect(after.GetLabels()).NotTo(HaveKey(controlPlaneNameLabel))
	g.Expect(isControlPlaneChild(after, cp)).To(BeFalse())
}

// --- per-service target clusters: the tenant store follows the service ---

// TestReconcileESOTenantStore_PlacedTrioLandsOnBothClusters verifies the trio of a
// placed namespace that HOSTS A PROJECTED REGISTRATION is provisioned on both
// clusters that namespace exists on. The copy on the target is what the ESO there
// materialises the service's Secrets through: an ESO store is only usable by the
// ESO that reads it, and its client certificate is only issued by the cert-manager
// on the same cluster. The copy at home is what the registration runs through — a
// KeystoneService is reconciled on the cluster its CR lives on, which is the
// management cluster, and it resolves the store in its own namespace there. The
// ControlPlane's own namespace keeps its owner-referenced trio at home.
func TestReconcileESOTenantStore_PlacedTrioLandsOnBothClusters(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := korcTestScheme(t)
	cp := placedGlanceControlPlane("remote-a")

	target := fake.NewClientBuilder().WithScheme(s).Build()
	resolver := &childrenResolver{children: target}
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: resolver,
	}

	res, err := r.reconcileESOTenantStore(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(esoTenantStoreRequeueAfter), "no store is Ready yet")
	g.Expect(resolver.names).To(Equal([]mcruntime.ClusterName{"remote-a"}),
		"only the placed namespace costs a cluster lookup")

	// Every object of the placed namespace's trio is on the target, carrying the
	// whole label set and no owner reference.
	remoteSA := &corev1.ServiceAccount{}
	g.Expect(target.Get(ctx, types.NamespacedName{Namespace: "images", Name: esoTenantServiceAccountName},
		remoteSA)).To(Succeed())
	remoteCert := &unstructured.Unstructured{}
	remoteCert.SetGroupVersionKind(certificateGVK)
	g.Expect(target.Get(ctx, types.NamespacedName{Namespace: "images", Name: esoTenantClientCertName},
		remoteCert)).To(Succeed())
	remoteStore := &esov1.SecretStore{}
	g.Expect(target.Get(ctx, types.NamespacedName{Namespace: "images", Name: esoTenantStoreName},
		remoteStore)).To(Succeed())
	for _, obj := range []client.Object{remoteSA, remoteCert, remoteStore} {
		g.Expect(obj.GetLabels()).To(Equal(remoteChildLabels(cp)), "%T must carry the full remote claim", obj)
		g.Expect(obj.GetOwnerReferences()).To(BeEmpty(),
			"%T must carry no owner reference on the target cluster", obj)
	}

	// And on the management cluster as well, claimed by the cross-namespace labels
	// alone: the namespace is not the ControlPlane's own, so no owner reference
	// reaches it there either.
	homeSA := &corev1.ServiceAccount{}
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Namespace: "images", Name: esoTenantServiceAccountName},
		homeSA)).To(Succeed())
	homeCert := &unstructured.Unstructured{}
	homeCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Namespace: "images", Name: esoTenantClientCertName},
		homeCert)).To(Succeed())
	homeStore := &esov1.SecretStore{}
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Namespace: "images", Name: esoTenantStoreName},
		homeStore)).To(Succeed())
	for _, obj := range []client.Object{homeSA, homeCert, homeStore} {
		g.Expect(obj.GetLabels()).To(Equal(controlPlaneChildLabels(cp)),
			"%T must carry the cross-namespace claim at home, and only that", obj)
		g.Expect(obj.GetOwnerReferences()).To(BeEmpty(),
			"%T is in a namespace that is not the ControlPlane's, so it carries no owner reference", obj)
	}

	// The ControlPlane's own trio is unchanged.
	own := &esov1.SecretStore{}
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: esoTenantStoreName},
		own)).To(Succeed())
	g.Expect(metav1.GetControllerOf(own)).NotTo(BeNil())
	g.Expect(own.Labels).NotTo(HaveKey(commonmulticluster.OwnerKindLabel),
		"a local child is claimed by its owner reference, not by the remote labels")
}

// TestReconcileESOTenantStore_PlacedTrioWithoutAHomeConsumerStaysOnTheTarget is
// the other side of the both-clusters rule: a placed namespace gets a home copy
// only where something on the management cluster reads it. A placed KEYSTONE
// namespace has no such path — the admin password, the DB credentials and the
// service-account deliveries all resolve their store through the namespace's own
// cluster — so a home copy would be written, certificate-issued and ESO-validated
// against OpenBao for a store nothing reads, and, sitting in the pipeline's
// blocking prefix, would park DBCredentials, AdminPassword and Keystone behind its
// readiness.
func TestReconcileESOTenantStore_PlacedTrioWithoutAHomeConsumerStaysOnTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := korcTestScheme(t)
	cp := placedKeystoneControlPlane("remote-a")

	// The target's copy is already Ready, so the only thing that could still hold
	// the gate is a home copy this pass must not have written.
	placedOnTarget := readyTenantSecretStore(esoTenantStoreName, "identity", "", "")
	placedOnTarget.Labels = remoteChildLabels(cp)
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placedOnTarget).Build()
	r := &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).
			WithObjects(cp, readyTenantSecretStore(esoTenantStoreName, "openstack", "", "")).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target},
	}

	res, err := r.reconcileESOTenantStore(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	homeCert := &unstructured.Unstructured{}
	homeCert.SetGroupVersionKind(certificateGVK)
	for _, absent := range []struct {
		name string
		obj  client.Object
	}{
		{esoTenantServiceAccountName, &corev1.ServiceAccount{}},
		{esoTenantClientCertName, homeCert},
		{esoTenantStoreName, &esov1.SecretStore{}},
	} {
		err := r.Get(ctx, types.NamespacedName{Namespace: "identity", Name: absent.name}, absent.obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"%T must not be provisioned at home for a placed namespace nothing reads it in", absent.obj)
	}

	g.Expect(res.IsZero()).To(BeTrue(),
		"the target copy alone opens the gate; a home copy would hold it on a store nothing reads")
	cond := esoTenantCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("ESOTenantStoreReady"))
}

// TestReconcileESOTenantStore_AllowlistedPlacedNamespaceGetsTheHomeCopy covers
// the OTHER registration that reads its store at home: a third-party
// KeystoneService in a namespace the serviceRegistrations allowlist admits.
// Nothing forbids that allowlist from naming a namespace this ControlPlane also
// placed a service in — and reconcileRegistrationTenantStores skips every
// namespace the ControlPlane occupies, on the premise that the step here covers
// it. Without this arm the registration mints its Keystone account and then waits
// forever on a store nobody creates, silently: with no other allowlisted
// namespace, that sub-reconciler reports True/NoRegistrationNamespaces.
func TestReconcileESOTenantStore_AllowlistedPlacedNamespaceGetsTheHomeCopy(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := korcTestScheme(t)
	cp := placedKeystoneControlPlane("remote-a")
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
		AllowedNamespaces: []string{"identity"},
	}

	target := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target},
	}

	_, err := r.reconcileESOTenantStore(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	homeCert := &unstructured.Unstructured{}
	homeCert.SetGroupVersionKind(certificateGVK)
	for _, present := range []struct {
		name string
		obj  client.Object
	}{
		{esoTenantServiceAccountName, &corev1.ServiceAccount{}},
		{esoTenantClientCertName, homeCert},
		{esoTenantStoreName, &esov1.SecretStore{}},
	} {
		g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "identity", Name: present.name}, present.obj)).
			To(Succeed(), "%T must be provisioned at home: an allowlisted registration reads it there", present.obj)
	}

	// The target keeps its own copy: the service placed there still materialises
	// its Secrets through the ESO on that cluster.
	g.Expect(target.Get(ctx, types.NamespacedName{Namespace: "identity", Name: esoTenantStoreName},
		&esov1.SecretStore{})).To(Succeed())
}

// TestHostsHomeRegistration_Neutron pins the network service's arm of the
// home-registration question: a placed Neutron namespace hosts the KeystoneService
// registration projected for it, so it needs the tenant store at home as well as
// on its own cluster. A namespace the ControlPlane placed nothing in answers
// false, and so does the Neutron namespace of a ControlPlane that declares no
// network service, because an undeclared Neutron resolves to the ControlPlane's
// own namespace instead.
func TestHostsHomeRegistration_Neutron(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			Services: c5c3v1alpha1.ServicesSpec{
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
						Name: "network", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
					},
					TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge-a"},
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"},
					},
				},
			},
		},
	}

	g.Expect(hostsHomeRegistration(cp, "network")).To(BeTrue(),
		"the placed Neutron namespace hosts the registration projected for the network service")
	g.Expect(hostsHomeRegistration(cp, "storage")).To(BeFalse(),
		"a namespace this ControlPlane placed nothing in hosts no registration")

	undeclared := cp.DeepCopy()
	undeclared.Spec.Services.Neutron = nil
	g.Expect(hostsHomeRegistration(undeclared, "network")).To(BeFalse(),
		"without a neutron block the network namespace belongs to no service of this plane")
}

// TestReconcileESOTenantStore_ReadyGatesOnBothPlacedStores verifies the readiness
// gate covers BOTH copies of a placed REGISTRATION-HOSTING namespace's store, each
// read from the cluster it was written to. The two carry different delivery
// paths — the target copy the Secrets the ESO there materialises, the home copy
// the registration reconciled at home — so either one still issuing its client
// certificate holds the condition False, and the message names the cluster to
// look on.
func TestReconcileESOTenantStore_ReadyGatesOnBothPlacedStores(t *testing.T) {
	ctx := context.Background()

	for name, tc := range map[string]struct {
		homeReady, targetReady bool
		// The cluster the message has to name, empty when the pass must go True.
		wantCluster string
	}{
		"the target copy is still issuing": {homeReady: true, wantCluster: "target cluster"},
		"the home copy is still issuing":   {targetReady: true, wantCluster: "management cluster"},
		"both copies are Ready":            {homeReady: true, targetReady: true},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := korcTestScheme(t)
			cp := placedGlanceControlPlane("remote-a")

			// A seeded store is one the operator already wrote on an earlier pass, so
			// it carries the claim of the cluster it sits on: a store it did not
			// create is refused, not adopted, on either of them.
			atHome := []client.Object{cp, readyTenantSecretStore(esoTenantStoreName, "default", "", "")}
			if tc.homeReady {
				placedAtHome := readyTenantSecretStore(esoTenantStoreName, "images", "", "")
				placedAtHome.Labels = controlPlaneChildLabels(cp)
				atHome = append(atHome, placedAtHome)
			}
			var onTarget []client.Object
			if tc.targetReady {
				placedOnTarget := readyTenantSecretStore(esoTenantStoreName, "images", "", "")
				placedOnTarget.Labels = remoteChildLabels(cp)
				onTarget = append(onTarget, placedOnTarget)
			}

			r := &ControlPlaneReconciler{
				Client: fake.NewClientBuilder().WithScheme(s).WithObjects(atHome...).Build(),
				Scheme: s,
				Resolver: &childrenResolver{
					children: fake.NewClientBuilder().WithScheme(s).WithObjects(onTarget...).Build(),
				},
			}

			res, err := r.reconcileESOTenantStore(ctx, cp)
			g.Expect(err).NotTo(HaveOccurred())
			cond := esoTenantCondition(cp)

			if tc.wantCluster == "" {
				g.Expect(res.IsZero()).To(BeTrue(), "both stores are Ready, so the pass must not requeue")
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(cond.Reason).To(Equal("ESOTenantStoreReady"))
				return
			}

			g.Expect(res.RequeueAfter).To(Equal(esoTenantStoreRequeueAfter),
				"one unready store holds the gate, whichever cluster it is on")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
			g.Expect(cond.Message).To(ContainSubstring(`namespace "images"`))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantCluster))
		})
	}
}

// TestReconcileESOTenantStore_UnresolvableTargetProvisionsNothing covers the
// cluster that does not resolve. The ControlPlane's own namespace is first in the
// occupied set, so finding its trio absent is what shows the clusters are all
// resolved before the first write.
func TestReconcileESOTenantStore_UnresolvableTargetProvisionsNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := korcTestScheme(t)
	cp := placedKeystoneControlPlane("remote-a")

	target := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target, err: mcruntime.ErrClusterNotFound},
	}

	res, err := r.reconcileESOTenantStore(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred(), "an unregistered cluster is a state to wait out, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(esoTenantStoreRequeueAfter))

	cond := esoTenantCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	for name, c := range map[string]client.Client{"management": r.Client, "target": target} {
		var stores esov1.SecretStoreList
		g.Expect(c.List(ctx, &stores)).To(Succeed())
		g.Expect(stores.Items).To(BeEmpty(), "no tenant store may be provisioned on the %s cluster", name)

		var accounts corev1.ServiceAccountList
		g.Expect(c.List(ctx, &accounts)).To(Succeed())
		g.Expect(accounts.Items).To(BeEmpty(), "no tenant ServiceAccount may be provisioned on the %s cluster", name)
	}
}

// --- registration tenant stores (allowlisted foreign namespaces) ---

// registrationTenantStoreClient builds the fake client the registration
// tenant-store tests drive the reconciler with. The KeystoneService field index is
// registered because reconcileRegistrationTenantStores resolves its registrations
// through it, exactly as the manager does in production.
func registrationTenantStoreClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithObjects(objs...).
		WithIndex(&c5c3v1alpha1.KeystoneService{}, KeystoneServiceControlPlaneRefIndexKey,
			keystoneServiceControlPlaneRefExtractor).
		Build()
}

// allowlistingControlPlane returns a managed ControlPlane admitting service
// registrations from the given namespaces.
func allowlistingControlPlane(namespaces ...string) *c5c3v1alpha1.ControlPlane {
	cp := dbCredManagedControlPlane()
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
		AllowedNamespaces: namespaces,
	}
	return cp
}

// registrationIn returns a KeystoneService in namespace registered against cp.
func registrationIn(namespace, name string, cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	return &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: cp.Name, Namespace: cp.Namespace},
		},
	}
}

// registrationTenantStoresCondition returns the RegistrationTenantStoresReady
// condition off the CR.
func registrationTenantStoresCondition(cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeRegistrationTenantStoresReady)
}

// ownedTenantTrioIn returns the three objects of a tenant-store trio already
// provisioned by cp in a foreign namespace: label-owned, since a cross-namespace
// child carries no owner reference. The store is Ready so the readiness rollup
// passes without an ESO controller.
func ownedTenantTrioIn(namespace string, cp *c5c3v1alpha1.ControlPlane) (*esov1.SecretStore, *unstructured.Unstructured, *corev1.ServiceAccount) {
	store := readyTenantSecretStore(esoTenantStoreName, namespace, "", "")
	store.Labels = controlPlaneChildLabels(cp)

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(esoTenantClientCertName)
	cert.SetNamespace(namespace)
	cert.SetLabels(controlPlaneChildLabels(cp))

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantServiceAccountName, Namespace: namespace, Labels: controlPlaneChildLabels(cp),
	}}
	return store, cert, sa
}

// expectTenantTrio asserts the trio exists in namespace and carries cp's ownership
// labels — the only ownership mechanism a cross-namespace child can carry.
func expectTenantTrio(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane, namespace string) {
	t.Helper()
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := &esov1.SecretStore{}
	g.Expect(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: esoTenantStoreName}, store)).To(Succeed())
	g.Expect(store.Labels).To(Equal(controlPlaneChildLabels(cp)))
	g.Expect(metav1.GetControllerOf(store)).To(BeNil(), "a cross-namespace child cannot carry an owner reference")

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: esoTenantClientCertName}, cert)).To(Succeed())
	g.Expect(cert.GetLabels()).To(Equal(controlPlaneChildLabels(cp)))

	sa := &corev1.ServiceAccount{}
	g.Expect(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: esoTenantServiceAccountName}, sa)).To(Succeed())
	g.Expect(sa.Labels).To(Equal(controlPlaneChildLabels(cp)))
}

// expectNoTenantTrio asserts no part of the trio is left in namespace.
func expectNoTenantTrio(t *testing.T, c client.Client, namespace string) {
	t.Helper()
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	for name, obj := range map[string]client.Object{
		"SecretStore":    &esov1.SecretStore{},
		"Certificate":    cert,
		"ServiceAccount": &corev1.ServiceAccount{},
	} {
		objectName := esoTenantStoreName
		switch name {
		case "Certificate":
			objectName = esoTenantClientCertName
		case "ServiceAccount":
			objectName = esoTenantServiceAccountName
		}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: objectName}, obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%s must not exist in namespace %q", name, namespace)
	}
}

// TestReconcileRegistrationTenantStores_ProvisionsForAllowlistedRegistration an
// allowlisted namespace hosting a KeystoneService registered against this
// ControlPlane receives the full trio, label-owned and without an owner reference,
// and the condition reports the store Ready.
func TestReconcileRegistrationTenantStores_ProvisionsForAllowlistedRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	// The store the reconciler is about to create cannot report Ready by itself in
	// a fake client, so it is seeded Ready and the apply reasserts its spec.
	seeded, _, _ := ownedTenantTrioIn("tenant-a", cp)
	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore(), seeded,
		registrationIn("tenant-a", "billing", cp))
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t)}

	res, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a fully provisioned and Ready set requests no requeue")

	expectTenantTrio(t, c, cp, "tenant-a")

	cond := registrationTenantStoresCondition(cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("RegistrationTenantStoresReady"))
	g.Expect(cond.ObservedGeneration).To(Equal(cp.Generation))
}

// TestReconcileRegistrationTenantStores_IgnoresAnotherControlPlanesRegistration a
// KeystoneService in an allowlisted namespace that references a DIFFERENT
// ControlPlane must not put that namespace into the provisioning set: the field
// index resolves the reference namespace-qualified, so a same-named plane elsewhere
// is not this one.
func TestReconcileRegistrationTenantStores_IgnoresAnotherControlPlanesRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	other := &c5c3v1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: cp.Name, Namespace: "elsewhere"}}
	foreign := registrationIn("tenant-a", "foreign", other)

	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore(), foreign)
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t)}

	_, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	expectNoTenantTrio(t, c, "tenant-a")
	g.Expect(registrationTenantStoresCondition(cp).Reason).To(Equal("NoRegistrationNamespaces"))
}

// TestReconcileRegistrationTenantStores_AllowlistedButEmptyNamespaceGetsNothing the
// allowlist alone provisions nothing: a namespace is only worth a store once it
// actually hosts a registration.
func TestReconcileRegistrationTenantStores_AllowlistedButEmptyNamespaceGetsNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a", "tenant-b")
	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore())
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t)}

	res, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	expectNoTenantTrio(t, c, "tenant-a")
	expectNoTenantTrio(t, c, "tenant-b")
	g.Expect(registrationTenantStoresCondition(cp).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(registrationTenantStoresCondition(cp).Reason).To(Equal("NoRegistrationNamespaces"))
}

// TestReconcileRegistrationTenantStores_UnlistedNamespaceGetsNothing a registration
// in a namespace the allowlist does not carry is not provisioned for. Its own CR
// already reports NamespaceNotAllowed.
func TestReconcileRegistrationTenantStores_UnlistedNamespaceGetsNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore(),
		registrationIn("tenant-unlisted", "billing", cp))
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t)}

	_, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	expectNoTenantTrio(t, c, "tenant-unlisted")
	g.Expect(registrationTenantStoresCondition(cp).Reason).To(Equal("NoRegistrationNamespaces"))
}

// TestReconcileRegistrationTenantStores_NoRegistrationNamespacesShapes the three
// ways the provisioning set comes out empty all report True/NoRegistrationNamespaces,
// write nothing, and request no requeue.
func TestReconcileRegistrationTenantStores_NoRegistrationNamespacesShapes(t *testing.T) {
	ctx := context.Background()

	for name, build := range map[string]func() (*c5c3v1alpha1.ControlPlane, []client.Object){
		"nil block": func() (*c5c3v1alpha1.ControlPlane, []client.Object) {
			return dbCredManagedControlPlane(), nil
		},
		"empty list": func() (*c5c3v1alpha1.ControlPlane, []client.Object) {
			return allowlistingControlPlane(), nil
		},
		"no matching registrations": func() (*c5c3v1alpha1.ControlPlane, []client.Object) {
			cp := allowlistingControlPlane("tenant-a")
			return cp, []client.Object{registrationIn("tenant-unlisted", "billing", cp)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp, extra := build()
			objs := append([]client.Object{cp, readyClusterSecretStore()}, extra...)
			c := registrationTenantStoreClient(t, objs...)
			r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t)}

			res, err := r.reconcileRegistrationTenantStores(ctx, cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue(), "an empty provisioning set requests no requeue")

			var stores esov1.SecretStoreList
			g.Expect(c.List(ctx, &stores)).To(Succeed())
			g.Expect(stores.Items).To(BeEmpty(), "nothing may be provisioned")

			cond := registrationTenantStoresCondition(cp)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal("NoRegistrationNamespaces"))
		})
	}
}

// TestReconcileRegistrationTenantStores_StoreRefOverridden an explicit
// spec.secretStoreRef opts the whole plane out: nothing is provisioned and nothing
// is collected, even with an allowlisted namespace hosting a registration.
func TestReconcileRegistrationTenantStores_StoreRefOverridden(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindCluster, Name: "shared-store",
	}
	seeded, cert, sa := ownedTenantTrioIn("tenant-a", cp)
	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore(), seeded, cert, sa,
		registrationIn("tenant-a", "billing", cp))
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t)}

	res, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// Untouched in both directions: neither reasserted nor collected.
	expectTenantTrio(t, c, cp, "tenant-a")

	cond := registrationTenantStoresCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("StoreRefOverridden"))
	g.Expect(cond.Message).To(ContainSubstring("shared-store"))
}

// TestReconcileRegistrationTenantStores_WaitsForStoreReadiness a provisioned store
// that is not Ready yet holds the condition False and requeues, naming the
// namespace to look at.
func TestReconcileRegistrationTenantStores_WaitsForStoreReadiness(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore(),
		registrationIn("tenant-a", "billing", cp))
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t)}

	res, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(esoTenantStoreRequeueAfter))

	// The trio is written; only its readiness is outstanding.
	expectTenantTrio(t, c, cp, "tenant-a")

	cond := registrationTenantStoresCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
	g.Expect(cond.Message).To(ContainSubstring("tenant-a"))
}

// TestReconcileRegistrationTenantStores_ForeignStoreFailsOnlyItsNamespace a
// pre-existing store nobody labelled as ours is refused rather than adopted, and
// that refusal must not starve the other allowlisted namespace in the same pass.
func TestReconcileRegistrationTenantStores_ForeignStoreFailsOnlyItsNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a", "tenant-b")
	// Unlabelled: somebody else's store occupying the name in tenant-a.
	foreign := readyTenantSecretStore(esoTenantStoreName, "tenant-a", "", "")
	healthy, _, _ := ownedTenantTrioIn("tenant-b", cp)

	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore(), foreign, healthy,
		registrationIn("tenant-a", "billing", cp), registrationIn("tenant-b", "imaging", cp))
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t), APIReader: c}

	res, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred(),
		"a tenant-side name collision must not put the whole ControlPlane reconcile into backoff")
	g.Expect(res.RequeueAfter).To(Equal(esoTenantStoreRequeueAfter))

	// The healthy namespace still got its full trio in the very same pass.
	expectTenantTrio(t, c, cp, "tenant-b")

	// The foreign store was not adopted: no ownership labels were stamped on it.
	live := &esov1.SecretStore{}
	g.Expect(c.Get(ctx, types.NamespacedName{Namespace: "tenant-a", Name: esoTenantStoreName}, live)).To(Succeed())
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel))

	cond := registrationTenantStoresCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ProvisioningError"))
	g.Expect(cond.Message).To(ContainSubstring("tenant-a"))
	g.Expect(cond.Message).NotTo(ContainSubstring("tenant-b"))
}

// TestReconcileRegistrationTenantStores_RegistrationListFailure a failing
// KeystoneService List is an infrastructure fault on our side, so it surfaces as
// ProvisioningError AND as a returned error, which is what backs the workqueue off.
func TestReconcileRegistrationTenantStores_RegistrationListFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	c := fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithObjects(cp, readyClusterSecretStore()).
		WithIndex(&c5c3v1alpha1.KeystoneService{}, KeystoneServiceControlPlaneRefIndexKey,
			keystoneServiceControlPlaneRefExtractor).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*c5c3v1alpha1.KeystoneServiceList); ok {
					return errors.New("cache down")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t)}

	_, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("listing KeystoneServices for registration tenant stores"))
	g.Expect(err.Error()).To(ContainSubstring("cache down"))

	cond := registrationTenantStoresCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ProvisioningError"))
}

// TestReconcileRegistrationTenantStores_StoreListFailure the enumeration of what is
// already provisioned fails the same way: condition plus a returned error.
func TestReconcileRegistrationTenantStores_StoreListFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	c := fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithObjects(cp).
		WithIndex(&c5c3v1alpha1.KeystoneService{}, KeystoneServiceControlPlaneRefIndexKey,
			keystoneServiceControlPlaneRefExtractor).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*esov1.SecretStoreList); ok {
					return errors.New("cache down")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t)}

	_, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("listing the provisioned registration tenant stores"))

	g.Expect(registrationTenantStoresCondition(cp).Reason).To(Equal("ProvisioningError"))
}

// TestReconcileRegistrationTenantStores_StoreReadinessReadFailurePropagates a
// readiness read that fails is propagated as the wrapped error from
// secrets.IsSecretStoreReady rather than folded into a False condition, so the
// workqueue backs off instead of reporting a state nobody observed.
func TestReconcileRegistrationTenantStores_StoreReadinessReadFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	seeded, _, _ := ownedTenantTrioIn("tenant-a", cp)
	// The provisioning pass reads the store once for its adoption pre-check before
	// the rollup reads it again, so only the SECOND read is failed. Failing both
	// would exercise the provisioning path instead of the one under test.
	var storeReads atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithObjects(cp, readyClusterSecretStore(), seeded, registrationIn("tenant-a", "billing", cp)).
		WithIndex(&c5c3v1alpha1.KeystoneService{}, KeystoneServiceControlPlaneRefIndexKey,
			keystoneServiceControlPlaneRefExtractor).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*esov1.SecretStore); ok && key.Namespace == "tenant-a" {
					if storeReads.Add(1) > 1 {
						return apierrors.NewInternalError(errors.New("etcd unavailable"))
					}
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t), APIReader: c}

	_, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("getting SecretStore"))
	g.Expect(err.Error()).To(ContainSubstring("etcd unavailable"))
}

// TestReconcileRegistrationTenantStores_CollectsWhenTheLastRegistrationLeaves once
// a namespace holds no registration any more its trio is collected, all three
// objects of it.
func TestReconcileRegistrationTenantStores_CollectsWhenTheLastRegistrationLeaves(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	store, cert, sa := ownedTenantTrioIn("tenant-a", cp)
	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore(), store, cert, sa)
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t), APIReader: c}

	res, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	expectNoTenantTrio(t, c, "tenant-a")
	g.Expect(registrationTenantStoresCondition(cp).Reason).To(Equal("NoRegistrationNamespaces"))
}

// TestReconcileRegistrationTenantStores_DeListingFreezesInsteadOfCollecting per D9
// the allowlist is an admission gate, not a revocation tool: removing a namespace
// that still hosts registrations leaves its store standing, because collecting it
// would destroy credentials a running service depends on.
func TestReconcileRegistrationTenantStores_DeListingFreezesInsteadOfCollecting(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	// The namespace is no longer allowlisted, but its registration is still there.
	cp := allowlistingControlPlane()
	store, cert, sa := ownedTenantTrioIn("tenant-a", cp)
	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore(), store, cert, sa,
		registrationIn("tenant-a", "billing", cp))
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t), APIReader: c}

	_, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	expectTenantTrio(t, c, cp, "tenant-a")
	g.Expect(registrationTenantStoresCondition(cp).Reason).To(Equal("NoRegistrationNamespaces"),
		"a frozen namespace is not part of the provisioning set the rollup reports on")
}

// TestReconcileRegistrationTenantStores_LeavesTheOwnNamespacesAlone the collection
// walks only namespaces OUTSIDE the ones the control plane occupies: the blocking
// tenant-store step owns those, and collecting one here would delete the store the
// plane's own credential material rides on.
func TestReconcileRegistrationTenantStores_LeavesTheOwnNamespacesAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	// A label-owned trio in a dedicated service namespace, which controlPlaneNamespaces
	// carries and reconcileESOTenantStore owns.
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: "identity", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
	}
	store, cert, sa := ownedTenantTrioIn("identity", cp)
	c := registrationTenantStoreClient(t, cp, readyClusterSecretStore(), store, cert, sa)
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t), APIReader: c}

	_, err := r.reconcileRegistrationTenantStores(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	expectTenantTrio(t, c, cp, "identity")
}

// TestDeleteESOTenantStoreTrioIn_LeavesForeignObjectsAlone the collection is
// ownership-checked against live state, so a same-named object belonging to
// somebody else in a shared namespace survives.
func TestDeleteESOTenantStoreTrioIn_LeavesForeignObjectsAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	foreignStore := readyTenantSecretStore(esoTenantStoreName, "tenant-a", "", "")
	foreignSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantServiceAccountName, Namespace: "tenant-a",
	}}
	c := registrationTenantStoreClient(t, cp, foreignStore, foreignSA)
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t), APIReader: c}

	g.Expect(r.deleteESOTenantStoreTrioIn(ctx, cp, "tenant-a")).To(Succeed())
	expectPresent(t, c, foreignStore, foreignSA)
}

// TestDeleteESOTenantStoreTrioIn_RemovesTheStoreBeforeItsCredentials the order is
// load-bearing and the reverse of provisioning: the store authenticates with the
// ServiceAccount and the client certificate, so removing either of those first
// would leave a live store authenticating against material that is already gone.
func TestDeleteESOTenantStoreTrioIn_RemovesTheStoreBeforeItsCredentials(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := allowlistingControlPlane("tenant-a")
	store, cert, sa := ownedTenantTrioIn("tenant-a", cp)

	var order []string
	c := fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithObjects(cp, store, cert, sa).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				switch obj.(type) {
				case *esov1.SecretStore:
					order = append(order, "SecretStore")
				case *unstructured.Unstructured:
					order = append(order, "Certificate")
				case *corev1.ServiceAccount:
					order = append(order, "ServiceAccount")
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: korcTestScheme(t), APIReader: c}

	g.Expect(r.deleteESOTenantStoreTrioIn(ctx, cp, "tenant-a")).To(Succeed())
	g.Expect(order).To(Equal([]string{"SecretStore", "Certificate", "ServiceAccount"}))
}
