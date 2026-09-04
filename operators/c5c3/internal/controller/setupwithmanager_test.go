// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the ControlPlane SetupWithManager wiring: the secret-name field
// indexer extractor, the Secret -> ControlPlane watch mapper, and the
// target-cluster gate every remote watch leg carries.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// newControlPlaneMapperClient returns a fake client pre-registered with the
// ControlPlaneSecretNameIndexKey field indexer so secretToControlPlaneMapper can
// resolve its MatchingFields lookups, mirroring keystone's
// newMapperFakeClientBuilder.
func newControlPlaneMapperClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(controllerTestScheme(t)).
		WithObjects(objs...).
		WithIndex(&c5c3v1alpha1.ControlPlane{}, ControlPlaneSecretNameIndexKey, controlPlaneSecretNameExtractor).
		Build()
}

// mapperControlPlane builds a minimal ControlPlane whose admin passwordSecretRef
// points at the named Secret.
func mapperControlPlane(name, namespace, secretName string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name + "-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: secretName, Key: "password"},
				},
			},
		},
	}
}

// --- controlPlaneSecretNameExtractor ---

func TestControlPlaneSecretNameExtractor_ReturnsPasswordSecretRefName(t *testing.T) {
	g := NewGomegaWithT(t)

	// mapperControlPlane sets no Database.ClusterRef, so this is the BROWNFIELD
	// case: the effective admin-password Secret is the user-supplied passwordSecretRef.
	cp := mapperControlPlane("cp", "default", "keystone-admin")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("keystone-admin"),
		"extractor must return the admin passwordSecretRef name")
}

func TestControlPlaneSecretNameExtractor_ManagedReturnsEffectiveName(t *testing.T) {
	g := NewGomegaWithT(t)

	// Managed mode (Database.ClusterRef != nil): the operator projects the admin
	// password into a per-ControlPlane Secret, so the indexed name must be the
	// operator-owned adminPasswordSecretName(cp), NOT the spec passwordSecretRef.
	cp := mapperControlPlane("cp", "default", "keystone-admin")
	cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{}
	cp.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf(adminPasswordSecretName(cp)),
		"in managed mode the extractor must index the operator-owned per-CP admin-password Secret name")
}

func TestControlPlaneSecretNameExtractor_EmptyWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(BeEmpty(),
		"extractor must return an empty slice when passwordSecretRef.name is unset")
}

// externalMapperControlPlane is the External-mode shape: the user-supplied admin
// password Secret plus an optional private-CA bundle Secret. Both must be indexed
// so a rotation of either wakes the owning ControlPlane.
func externalMapperControlPlane(name, namespace, secretName, caSecretName string) *c5c3v1alpha1.ControlPlane {
	cp := mapperControlPlane(name, namespace, secretName)
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Mode:     c5c3v1alpha1.KeystoneModeExternal,
		External: &c5c3v1alpha1.ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
	}
	if caSecretName != "" {
		cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{
			Name: caSecretName, Key: "ca.crt",
		}
	}
	return cp
}

func TestControlPlaneSecretNameExtractor_ExternalIncludesCABundleSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalMapperControlPlane("cp", "default", "external-admin", "keystone-ca")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("external-admin", "keystone-ca"),
		"External mode must index both the admin-password and the CA-bundle Secret")
}

// TestControlPlaneSecretNameExtractor_DeduplicatesSharedSecret covers the shape
// where one Secret carries both the admin password and the CA bundle: a duplicate
// index entry would enqueue the same ControlPlane twice per Secret event.
func TestControlPlaneSecretNameExtractor_DeduplicatesSharedSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalMapperControlPlane("cp", "default", "shared", "shared")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("shared"))
}

// TestControlPlaneSecretNameExtractor_ManagedIgnoresCABundleRef proves the mode
// discriminator gates the CA entry: a managed ControlPlane never dials a
// TLS-fronted endpoint, so a leftover external block indexes nothing extra.
func TestControlPlaneSecretNameExtractor_ManagedIgnoresCABundleRef(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalMapperControlPlane("cp", "default", "keystone-admin", "keystone-ca")
	cp.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeManaged
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("keystone-admin"))
}

// TestControlPlaneSecretNameExtractor_ExternalWithoutPasswordStillIndexesCA covers
// the empty-name edge: an unset passwordSecretRef must not leak an empty string
// into the index, but must not suppress the CA entry either.
func TestControlPlaneSecretNameExtractor_ExternalWithoutPasswordStillIndexesCA(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalMapperControlPlane("cp", "default", "", "keystone-ca")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("keystone-ca"))
}

func TestControlPlaneSecretNameExtractor_WrongTypeReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)

	// A non-ControlPlane object must not panic; a nil return is the contract.
	got := controlPlaneSecretNameExtractor(&corev1.Secret{})

	g.Expect(got).To(BeNil(),
		"extractor must return nil for a non-ControlPlane object")
}

// --- secretToControlPlaneMapper ---

func TestSecretToControlPlaneMapper_EnqueuesMatchingAdminSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "keystone-admin")
	c := newControlPlaneMapperClient(t, cp)
	mapper := secretToControlPlaneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
	}
	reqs := mapper(context.Background(), secret)

	g.Expect(reqs).To(HaveLen(1),
		"a Secret matching the admin passwordSecretRef must enqueue its ControlPlane")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: "default", Name: "cp"}))
}

func TestSecretToControlPlaneMapper_IgnoresNonMatchingSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "keystone-admin")
	c := newControlPlaneMapperClient(t, cp)
	mapper := secretToControlPlaneMapper(c)

	// A Secret whose name does not match the admin passwordSecretRef must yield
	// no reconcile requests.
	other := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-secret", Namespace: "default"},
	}
	reqs := mapper(context.Background(), other)

	g.Expect(reqs).To(BeEmpty(),
		"a Secret not referenced by any ControlPlane must enqueue nothing")
}

func TestSecretToControlPlaneMapper_ScopedToNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	// Two ControlPlanes in different namespaces referencing the same Secret name.
	// Only the one in the event's namespace must be enqueued.
	cpA := mapperControlPlane("cp-a", "ns-a", "shared-secret")
	cpB := mapperControlPlane("cp-b", "ns-b", "shared-secret")
	c := newControlPlaneMapperClient(t, cpA, cpB)
	mapper := secretToControlPlaneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "ns-a"},
	}
	reqs := mapper(context.Background(), secret)

	g.Expect(reqs).To(HaveLen(1),
		"only the ControlPlane in the Secret's namespace must be enqueued")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: "ns-a", Name: "cp-a"}))
}

// --- storeToControlPlaneMapper (#476, #605) ---

// TestStoreToControlPlaneMapper_ClusterKindEnqueuesExplicitControlPlanes verifies
// that a status change on the OpenBao-backed ClusterSecretStore enqueues every
// ControlPlane that EXPLICITLY selects it (across namespaces), but NOT a
// ControlPlane that omits spec.secretStoreRef — the default now resolves to the
// operator-provisioned per-tenant namespaced store, so a cluster-store change no
// longer concerns it.
func TestStoreToControlPlaneMapper_ClusterKindEnqueuesExplicitControlPlanes(t *testing.T) {
	g := NewGomegaWithT(t)

	cpA := mapperControlPlane("cp-a", "ns-a", "secret-a")
	cpA.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindCluster, Name: openBaoClusterStoreName,
	}
	cpB := mapperControlPlane("cp-b", "ns-b", "secret-b")
	cpB.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindCluster, Name: openBaoClusterStoreName,
	}
	// A nil-ref ControlPlane defaults to the per-tenant store, so a cluster-store
	// change must NOT enqueue it.
	cpDefault := mapperControlPlane("cp-default", "ns-c", "secret-c")
	c := newControlPlaneMapperClient(t, cpA, cpB, cpDefault)
	mapper := storeToControlPlaneMapper(c, commonv1.SecretStoreKindCluster)

	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName},
	}
	reqs := mapper(context.Background(), store)

	names := make([]types.NamespacedName, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, r.NamespacedName)
	}
	g.Expect(names).To(ConsistOf(
		types.NamespacedName{Namespace: "ns-a", Name: "cp-a"},
		types.NamespacedName{Namespace: "ns-b", Name: "cp-b"},
	), "a ClusterSecretStore change must enqueue only ControlPlanes that explicitly select it, not the nil-ref default")
}

// TestStoreToControlPlaneMapper_ClusterKindIgnoresOtherStores verifies the
// mapper only reacts to the store a ControlPlane's effective ref resolves to,
// not to unrelated ClusterSecretStores.
func TestStoreToControlPlaneMapper_ClusterKindIgnoresOtherStores(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "secret")
	c := newControlPlaneMapperClient(t, cp)
	mapper := storeToControlPlaneMapper(c, commonv1.SecretStoreKindCluster)

	other := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "some-other-store"},
	}
	reqs := mapper(context.Background(), other)

	g.Expect(reqs).To(BeEmpty(),
		"a change to an unrelated ClusterSecretStore must enqueue nothing")
}

// TestStoreToControlPlaneMapper_NamespacedKindScopesToStoreNamespace verifies a
// namespaced SecretStore named openbao-tenant-store enqueues every ControlPlane
// in its OWN namespace that resolves to it — both a ControlPlane that pins it
// explicitly and a nil-ref ControlPlane that defaults to it — but NOT a same-name
// store in a foreign namespace.
func TestStoreToControlPlaneMapper_NamespacedKindScopesToStoreNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	pinned := mapperControlPlane("cp-pinned", "tenant-a", "secret-a")
	pinned.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	// A nil-ref ControlPlane in the same namespace defaults to the per-tenant
	// store named openbao-tenant-store, so it too must be enqueued.
	defaulted := mapperControlPlane("cp-default", "tenant-a", "secret-b")
	foreign := mapperControlPlane("cp-foreign", "tenant-b", "secret-c")
	foreign.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	c := newControlPlaneMapperClient(t, pinned, defaulted, foreign)
	mapper := storeToControlPlaneMapper(c, commonv1.SecretStoreKindNamespaced)

	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "tenant-a"},
	}
	reqs := mapper(context.Background(), store)

	names := make([]types.NamespacedName, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, r.NamespacedName)
	}
	g.Expect(names).To(ConsistOf(
		types.NamespacedName{Namespace: "tenant-a", Name: "cp-pinned"},
		types.NamespacedName{Namespace: "tenant-a", Name: "cp-default"},
	), "a namespaced SecretStore change must enqueue every ControlPlane in its own namespace that resolves to it")
}

// TestControlPlaneSecretNameExtractor_ExternalModeIndexesUserSecret asserts the
// field indexer follows effectiveAdminPasswordSecretRef into External mode: the
// indexed name is the USER-supplied Secret, so an edit to it wakes the
// ControlPlane and feeds the hash-driven application-credential re-mint. Indexing
// the operator-owned name instead would leave an out-of-band password rotation
// invisible until the next periodic resync.
func TestControlPlaneSecretNameExtractor_ExternalModeIndexesUserSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "external-admin")
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Mode:     c5c3v1alpha1.KeystoneModeExternal,
		External: &c5c3v1alpha1.ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
	}

	g.Expect(controlPlaneSecretNameExtractor(cp)).To(ConsistOf("external-admin"))

	// Even with a (webhook-impossible) managed database block, the mode
	// discriminator keeps the user-supplied Secret indexed.
	cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{}
	cp.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}
	g.Expect(controlPlaneSecretNameExtractor(cp)).To(ConsistOf("external-admin"))
}

// --- per-service namespaces (issue #646) ---

// TestSecretToControlPlaneMapper_ResolvesAcrossNamespaces verifies a Secret event
// in a SERVICE namespace wakes the ControlPlane that lives elsewhere — the
// admin-password Secret follows the Keystone service, so a namespace-scoped
// lookup would look for the ControlPlane where it does not live and swallow the
// rotation. A ControlPlane that merely references the same Secret NAME in a
// namespace it does not occupy must still be left alone.
func TestSecretToControlPlaneMapper_ResolvesAcrossNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)

	// cp-a lives in ns-a and places Keystone in "identity".
	cpA := mapperControlPlane("cp-a", "ns-a", "shared-secret")
	cpA.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"},
	}
	// cp-b lives in ns-b, references the same Secret NAME, and occupies no other
	// namespace — the "identity" Secret is none of its business.
	cpB := mapperControlPlane("cp-b", "ns-b", "shared-secret")

	c := newControlPlaneMapperClient(t, cpA, cpB)
	mapper := secretToControlPlaneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "identity"},
	}
	reqs := mapper(context.Background(), secret)

	g.Expect(reqs).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns-a", Name: "cp-a"}},
	), "only the ControlPlane that actually occupies the Secret's namespace may be enqueued")
}

// TestNamespacedStoreToControlPlaneMapper_MatchesServiceNamespaces verifies the
// per-tenant store watch reaches into the service namespaces: a store flipping
// unready in a namespace the ControlPlane placed a service in must wake it, while
// an identically-named store in an unrelated namespace must not.
func TestNamespacedStoreToControlPlaneMapper_MatchesServiceNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "openstack", "admin-secret")
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"},
	}
	c := newControlPlaneMapperClient(t, cp)
	mapper := namespacedStoreToControlPlaneMapper(c)

	inServiceNS := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: "identity"},
	}
	g.Expect(mapper(context.Background(), inServiceNS)).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "cp"}},
	), "a tenant store in a service namespace must wake its ControlPlane")

	inOwnNS := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: "openstack"},
	}
	g.Expect(mapper(context.Background(), inOwnNS)).To(HaveLen(1))

	unrelated := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: "some-other-tenant"},
	}
	g.Expect(mapper(context.Background(), unrelated)).To(BeEmpty(),
		"an identically-named store in a namespace the ControlPlane does not occupy must wake nobody")
}

// --- controlPlaneTargetClusters ---

// TestControlPlaneTargetClusters_ReadsThePlacementFromTheCR pins where the gate
// on every target-cluster watch leg gets its answer: the ControlPlane on the
// management cluster, not the object the event carried. A CR placing two
// services names both clusters; one placing none names nothing, so no engaged
// cluster matches it.
func TestControlPlaneTargetClusters_ReadsThePlacementFromTheCR(t *testing.T) {
	g := NewGomegaWithT(t)

	placed := mapperControlPlane("placed", "openstack", "admin-secret")
	placed.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge-a"},
	}
	placed.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{
		TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge-b"},
	}
	localOnly := mapperControlPlane("local-only", "openstack", "admin-secret")

	targets := controlPlaneTargetClusters(newControlPlaneMapperClient(t, placed, localOnly))

	names, err := targets(context.Background(), types.NamespacedName{Namespace: "openstack", Name: "placed"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(names).To(Equal([]string{"edge-a", "edge-b"}))

	names, err = targets(context.Background(), types.NamespacedName{Namespace: "openstack", Name: "local-only"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(names).To(BeEmpty(),
		"a ControlPlane that places nothing must match no engaged cluster")
}

// TestControlPlaneTargetClusters_PropagatesTheReadError covers the event whose CR
// is already gone — a child outliving the ControlPlane that placed it. The error
// travels to commonmulticluster.RemoteRequestsAmong, which drops the event rather
// than reconciling on the strength of the object alone. Swallowing it here would
// make a deleted CR indistinguishable from a live local-only one.
func TestControlPlaneTargetClusters_PropagatesTheReadError(t *testing.T) {
	g := NewGomegaWithT(t)

	targets := controlPlaneTargetClusters(newControlPlaneMapperClient(t))

	names, err := targets(context.Background(), types.NamespacedName{Namespace: "openstack", Name: "gone"})
	g.Expect(names).To(BeEmpty())
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"a missing ControlPlane must surface as NotFound, the answer the handler silences")
}

// --- keystoneServiceToControlPlaneMapper ---

// TestKeystoneServiceToControlPlaneMapper_ExplicitReferenceNamespace a
// registration naming the ControlPlane's namespace explicitly maps to exactly that
// ControlPlane.
func TestKeystoneServiceToControlPlaneMapper_ExplicitReferenceNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "tenant-a"},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "openstack"},
		},
	}

	g.Expect(keystoneServiceToControlPlaneMapper(context.Background(), ks)).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "cp"}},
	))
}

// TestKeystoneServiceToControlPlaneMapper_DefaultsToItsOwnNamespace an omitted
// reference namespace means the CR's own, exactly as resolveControlPlane resolves
// it — so the mapper and the reconciler cannot disagree on which plane is meant.
func TestKeystoneServiceToControlPlaneMapper_DefaultsToItsOwnNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "tenant-a"},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp"},
		},
	}

	g.Expect(keystoneServiceToControlPlaneMapper(context.Background(), ks)).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "cp"}},
	))
}

// TestKeystoneServiceToControlPlaneMapper_IgnoresUnusableObjects a CR that
// bypassed admission and carries no reference name maps to nothing, rather than to
// a request for an unnamed ControlPlane; so does an object of another kind.
func TestKeystoneServiceToControlPlaneMapper_IgnoresUnusableObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	nameless := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "tenant-a"},
	}
	g.Expect(keystoneServiceToControlPlaneMapper(context.Background(), nameless)).To(BeEmpty())

	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "tenant-a"}}
	g.Expect(keystoneServiceToControlPlaneMapper(context.Background(), other)).To(BeEmpty())
}

// --- namespacedStoreToControlPlaneMapper: the registration-namespace arm ---

// allowlistingMapperControlPlane returns a mapper fixture admitting registrations
// from the given namespaces.
func allowlistingMapperControlPlane(name, namespace string, allowed ...string) *c5c3v1alpha1.ControlPlane {
	cp := mapperControlPlane(name, namespace, "admin-secret")
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{AllowedNamespaces: allowed}
	return cp
}

// TestNamespacedStoreToControlPlaneMapper_WakesOnAnAllowlistedRegistrationStore the
// per-tenant store the ControlPlane provisions in an allowlisted namespace is one
// it owns the lifecycle of, so its deletion or readiness flip must re-drive the
// plane even though the namespace is not one the plane occupies.
func TestNamespacedStoreToControlPlaneMapper_WakesOnAnAllowlistedRegistrationStore(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := allowlistingMapperControlPlane("cp", "openstack", "tenant-a")
	c := newControlPlaneMapperClient(t, cp)
	mapper := namespacedStoreToControlPlaneMapper(c)

	store := &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantStoreName, Namespace: "tenant-a",
	}}

	g.Expect(mapper(context.Background(), store)).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "cp"}},
	))
}

// TestNamespacedStoreToControlPlaneMapper_IgnoresUnrelatedRegistrationStores the
// arm is narrow on purpose: only the operator-provisioned store name, only an
// allowlisted namespace, and only while the plane has not overridden its store
// reference — an override means the operator provisions no registration store at all.
func TestNamespacedStoreToControlPlaneMapper_IgnoresUnrelatedRegistrationStores(t *testing.T) {
	ctx := context.Background()

	for name, build := range map[string]func() (*c5c3v1alpha1.ControlPlane, *esov1.SecretStore){
		"another store in an allowlisted namespace": func() (*c5c3v1alpha1.ControlPlane, *esov1.SecretStore) {
			return allowlistingMapperControlPlane("cp", "openstack", "tenant-a"),
				&esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: "somebody-elses-store", Namespace: "tenant-a"}}
		},
		"the tenant store in an unlisted namespace": func() (*c5c3v1alpha1.ControlPlane, *esov1.SecretStore) {
			return allowlistingMapperControlPlane("cp", "openstack", "tenant-a"),
				&esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: "tenant-z"}}
		},
		"an overridden store reference": func() (*c5c3v1alpha1.ControlPlane, *esov1.SecretStore) {
			cp := allowlistingMapperControlPlane("cp", "openstack", "tenant-a")
			cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
				Kind: commonv1.SecretStoreKindCluster, Name: "shared-store",
			}
			return cp, &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{
				Name: esoTenantStoreName, Namespace: "tenant-a",
			}}
		},
		"no allowlist at all": func() (*c5c3v1alpha1.ControlPlane, *esov1.SecretStore) {
			return mapperControlPlane("cp", "openstack", "admin-secret"),
				&esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: "tenant-a"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp, store := build()
			mapper := namespacedStoreToControlPlaneMapper(newControlPlaneMapperClient(t, cp))
			g.Expect(mapper(ctx, store)).To(BeEmpty())
		})
	}
}

// --- ovnCentralToControlPlaneMapper ---

// ovnMapperControlPlane returns a mapper fixture whose network service references
// the named OVNCentral. An empty refNamespace leaves the ref blank, which
// NeutronOVNCentralNamespace resolves to the ControlPlane's own namespace.
func ovnMapperControlPlane(name, namespace, centralName, refNamespace string) *c5c3v1alpha1.ControlPlane {
	cp := mapperControlPlane(name, namespace, "admin-secret")
	cp.Spec.Services.Neutron = &c5c3v1alpha1.ServiceNeutronSpec{
		OVN: c5c3v1alpha1.NeutronOVNSpec{
			CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: centralName, Namespace: refNamespace},
		},
	}
	return cp
}

// TestOVNCentralToControlPlaneMapper covers the watch leg that replaces the Owns()
// a referenced central can never have: which ControlPlanes an OVNCentral event
// wakes, and which it must leave asleep. The match is on the RESOLVED ref
// namespace and the name together, so a same-named central beside an unrelated
// plane triggers nothing.
func TestOVNCentralToControlPlaneMapper(t *testing.T) {
	ctx := context.Background()
	central := func(name, namespace string) *ovnv1alpha1.OVNCentral {
		return &ovnv1alpha1.OVNCentral{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	}

	for name, tc := range map[string]struct {
		cp       *c5c3v1alpha1.ControlPlane
		obj      client.Object
		wantWake bool
	}{
		"the referenced central in the referenced namespace": {
			cp:       ovnMapperControlPlane("cp", "openstack", "ovn-central", "ovn-system"),
			obj:      central("ovn-central", "ovn-system"),
			wantWake: true,
		},
		"an empty ref namespace resolves to the ControlPlane's own": {
			cp:       ovnMapperControlPlane("cp", "openstack", "ovn-central", ""),
			obj:      central("ovn-central", "openstack"),
			wantWake: true,
		},
		"the same name in another namespace": {
			cp:  ovnMapperControlPlane("cp", "openstack", "ovn-central", "ovn-system"),
			obj: central("ovn-central", "somewhere-else"),
		},
		"a ControlPlane without a network service": {
			cp:  mapperControlPlane("cp", "openstack", "admin-secret"),
			obj: central("ovn-central", "openstack"),
		},
		"an object of another kind": {
			cp:  ovnMapperControlPlane("cp", "openstack", "ovn-central", "ovn-system"),
			obj: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ovn-central", Namespace: "ovn-system"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			r := &ControlPlaneReconciler{Client: newControlPlaneMapperClient(t, tc.cp)}

			reqs := r.ovnCentralToControlPlaneMapper(ctx, tc.obj)

			if !tc.wantWake {
				g.Expect(reqs).To(BeEmpty())
				return
			}
			g.Expect(reqs).To(ConsistOf(reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: tc.cp.Namespace, Name: tc.cp.Name},
			}))
		})
	}
}

// TestOVNCentralToControlPlaneMapper_WakesEveryReferencingControlPlane proves the
// List is cluster-wide rather than scoped to the central's namespace: several
// planes may share one OVN control plane, and each of them mirrors its readiness.
func TestOVNCentralToControlPlaneMapper_WakesEveryReferencingControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	first := ovnMapperControlPlane("first", "openstack", "ovn-central", "ovn-system")
	second := ovnMapperControlPlane("second", "tenant-a", "ovn-central", "ovn-system")
	unrelated := ovnMapperControlPlane("third", "tenant-b", "other-central", "ovn-system")
	r := &ControlPlaneReconciler{Client: newControlPlaneMapperClient(t, first, second, unrelated)}

	reqs := r.ovnCentralToControlPlaneMapper(context.Background(),
		&ovnv1alpha1.OVNCentral{ObjectMeta: metav1.ObjectMeta{Name: "ovn-central", Namespace: "ovn-system"}})

	g.Expect(reqs).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "first"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "second"}},
	))
}
