// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Neutron sub-reconciler.
package controller

import (
	"context"
	"errors"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// neutronBusSecretName is the brownfield Secret the fixtures declare the shared
// bus through, and neutronBusURL the transport URL it carries.
const (
	neutronBusSecretName = "bus-url"
	neutronBusURL        = "rabbit://u:p@bus:5672/"
)

// neutronTestScheme registers c5c3, client-go, neutron, ovn, and external-secrets
// types (the projection ensures a DB-credential ExternalSecret).
func neutronTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := neutronv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding neutron scheme: %v", err)
	}
	if err := ovnv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding ovn scheme: %v", err)
	}
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	if err := esgenv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets generators scheme: %v", err)
	}
	return s
}

// neutronControlPlane builds a ControlPlane with services.neutron set and both
// gates reconcileNeutron reads off the ControlPlane itself True: KeystoneReady and
// OVNReady. The third gate is the projected KeystoneService child, which
// newNeutronTestReconciler seeds Ready (see withReadyNeutronRegistration), and the
// fourth is the shared bus, declared brownfield here and seeded by
// withNeutronBusSecret.
func neutronControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "default",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
					SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
				},
				Cache: commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
					Backend:    "dogpile.cache.pymemcache",
					Replicas:   3,
				},
				Messaging: &commonv1.MessagingSpec{
					SecretRef: &commonv1.SecretRefSpec{Name: neutronBusSecretName},
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"},
					},
				},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				},
			},
		},
	}
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "KeystoneReady",
		Message:            "ready",
	})
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeOVNReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "OVNCentralReady",
		Message:            "ready",
	})
	return cp
}

// readyNeutronRegistration builds the KeystoneService child the Neutron
// projection gates on, converged: account provisioned, catalog registered,
// aggregate Ready. A child in a dedicated namespace carries the ownership labels,
// so the projection re-applies it instead of refusing to adopt a same-named
// foreign CR.
func readyNeutronRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := neutronRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceAccountProvisioned,
		Message: "account provisioned",
	})
	conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
		Type:    conditionTypeKeystoneServiceCatalogReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceCatalogRegistered,
		Message: "catalog registered",
	})
	conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
		Type:    conditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "All sub-conditions are ready",
	})
	return ks
}

// neutronRegistration builds the KeystoneService child at the projected
// name/namespace carrying the given conditions, for the tests that drive the gate
// and the readiness fold from a child that has not converged.
func neutronRegistration(cp *c5c3v1alpha1.ControlPlane, conds ...metav1.Condition) *c5c3v1alpha1.KeystoneService {
	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: neutronName(cp), Namespace: cp.NeutronNamespace()},
	}
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	for _, cond := range conds {
		conditions.SetCondition(&ks.Status.Conditions, cond)
	}
	return ks
}

// notReadyNeutronDBCredES builds the Neutron DB-credential ExternalSecret with NO
// Ready condition, so WaitForExternalSecret reports not-Ready and the Dynamic
// readiness gate engages. Seeding it explicitly is what keeps
// withReadyNeutronDBCred from substituting a Ready one.
func notReadyNeutronDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := dbCredentialGeneratorExternalSecret(neutronDBCredentialTarget(cp))
	// Stamped as this ControlPlane's child so the cross-namespace projection path
	// re-applies it instead of refusing to adopt a same-named foreign object.
	stampControlPlaneChildLabels(es, cp)
	return es
}

// readyNeutronDBCredES builds a Ready Neutron DB-credential ExternalSecret at the
// derived name/namespace (Dynamic default shape), so WaitForExternalSecret reports
// Ready and the projection clears its dynamic readiness gate.
func readyNeutronDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := notReadyNeutronDBCredES(cp)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// materialisedNeutronDBCredSecret builds the Secret an ESO sync of the
// generator-backed ExternalSecret would materialise: an ENGINE-ISSUED username
// (the OpenBao mysql-database-plugin prefix) plus its password. The Dynamic gate
// checks the username, not just the ExternalSecret's Ready condition, so a Secret
// carrying a static seed's username reads as "not yet issued".
func materialisedNeutronDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutronDBCredentialSecretName(cp),
			Namespace: cp.NeutronNamespace(),
		},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-neutron-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	}
}

// withReadyNeutronDBCred seeds a Ready Neutron DB-credential ExternalSecret AND
// the engine-issued Secret an ESO sync of it would materialise, for the
// ControlPlane in objs, unless an ExternalSecret was already seeded explicitly.
//
// The Dynamic-default projection gates the child on both, the ExternalSecret
// having synced and the Secret behind it carrying an engine-issued username, and a
// fake client never runs ESO, so without this every projection test would stall on
// the gate and assert against a child that was deliberately not projected.
func withReadyNeutronDBCred(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	// Only the Dynamic path has a readiness gate; a Static ControlPlane projects a
	// KV-backed ExternalSecret of a different shape and must be left to build it.
	if cp == nil || !neutronDBCredentialsDynamicEnabled(cp) {
		return objs
	}
	name, ns := neutronDBCredentialSecretName(cp), cp.NeutronNamespace()
	for _, o := range objs {
		if _, ok := o.(*esov1.ExternalSecret); ok && o.GetName() == name && o.GetNamespace() == ns {
			return objs
		}
	}
	return append(objs, readyNeutronDBCredES(cp), materialisedNeutronDBCredSecret(cp))
}

// withReadyNeutronRegistration seeds the converged KeystoneService child the
// projection gates on, unless the test seeded one of its own, which is what the
// gate and readiness-fold tests do.
//
// A fake client runs no KeystoneService controller, so without this every
// projection test would hold at the registration gate and assert against a Neutron
// that was deliberately not projected.
func withReadyNeutronRegistration(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	if cp == nil || cp.Spec.Services.Neutron == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*c5c3v1alpha1.KeystoneService); ok {
			return objs
		}
	}
	return append(objs, readyNeutronRegistration(cp))
}

// withNeutronBusSecret seeds the brownfield transport-URL Secret the fixture's
// spec.infrastructure.messaging names, in the ControlPlane's OWN namespace where
// the bus is declared and read. Without it every projection test would halt on the
// messaging leg before reaching the child.
func withNeutronBusSecret(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	if cp == nil || cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil {
		return objs
	}
	ref := cp.Spec.Infrastructure.Messaging.SecretRef
	if ref == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*corev1.Secret); ok && o.GetName() == ref.Name && o.GetNamespace() == cp.Namespace {
			return objs
		}
	}
	return append(objs, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: cp.Namespace},
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(neutronBusURL)},
	})
}

// withNeutronTenantStore seeds the per-tenant SecretStore in the network
// service's namespace when that service is PLACED, unless the test seeded one of
// its own. The credential mirror a placed service gets is gated on that store, so
// without it every placed test would hold at SecretStoreNotReady.
//
// The store lands on the local client because newNeutronTestReconciler wires no
// resolver, which resolves every namespace to the management cluster. The tests
// that exercise the two-cluster legs build their reconciler themselves.
func withNeutronTenantStore(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	if cp == nil || targetClusterRefForNamespace(cp, cp.NeutronNamespace()) == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*esov1.SecretStore); ok {
			return objs
		}
	}
	return append(objs, readyTenantSecretStore(esoTenantStoreName, cp.NeutronNamespace(), "", ""))
}

func newNeutronTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := neutronTestScheme(t)
	seeded := withNeutronTenantStore(withReadyNeutronRegistration(withNeutronBusSecret(withReadyNeutronDBCred(objs))))
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &neutronv1alpha1.Neutron{},
			&c5c3v1alpha1.KeystoneService{})
	return &ControlPlaneReconciler{Client: cb.Build(), Scheme: s}
}

func getProjectedNeutron(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *neutronv1alpha1.Neutron {
	t.Helper()
	nn := &neutronv1alpha1.Neutron{}
	key := types.NamespacedName{Name: neutronName(cp), Namespace: cp.NeutronNamespace()}
	if err := c.Get(context.Background(), key, nn); err != nil {
		t.Fatalf("getting projected Neutron %s: %v", key, err)
	}
	return nn
}

// --- gates ---

func TestReconcileNeutron_NotManagedWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron = nil
	r := newNeutronTestReconciler(t, cp)

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NeutronNotManaged"))

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileNeutron_UnsetPreservesChildAndTearsDownDynamicGenerator covers the
// preserve-by-default branch: dropping spec.services.neutron keeps the child (an
// accidental block drop must not remove a running service) but must NOT keep the
// credential minter. A retained VaultDynamicSecret mints a fresh MySQL user with
// ALL PRIVILEGES every refresh interval, forever, for a service the operator was
// told it no longer manages, with no consumer, no revocation, and a
// NeutronReady=True condition that surfaces none of it.
func TestReconcileNeutron_UnsetPreservesChildAndTearsDownDynamicGenerator(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialSecretName(cp), Namespace: cp.NeutronNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).To(Succeed(), "the generator was projected alongside the child")

	// No opt-in annotation: the child is preserved.
	cp.Spec.Services.Neutron = nil
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedNeutron(t, r.Client, cp)).NotTo(BeNil(), "the child must still be preserved")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Reason).To(Equal("NeutronNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring(neutronDeletionAllowedAnnotation))

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialSecretName(cp), Namespace: cp.NeutronNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(),
		"the credential minter must be torn down even though the child is preserved")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialServiceAccountName, Namespace: cp.NeutronNamespace(),
	}, &corev1.ServiceAccount{})).NotTo(Succeed(), "the generator's ServiceAccount must be torn down too")
	orphanCert := &unstructured.Unstructured{}
	orphanCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialClientCertName(cp), Namespace: cp.NeutronNamespace(),
	}, orphanCert)).NotTo(Succeed(), "the generator's mTLS client Certificate must be torn down too")
}

// TestReconcileNeutron_UnsetDeletesChildWithOptIn verifies the opt-in deletion
// sweep removes the child AND every DB-credential object: the generator-backed
// ExternalSecret plus the Dynamic-mode VaultDynamicSecret, Certificate, and
// ServiceAccount.
func TestReconcileNeutron_UnsetDeletesChildWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	// The Dynamic-default DB-credential objects were projected alongside the child.
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialSecretName(cp), Namespace: cp.NeutronNamespace(),
	}, &esov1.ExternalSecret{})).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialSecretName(cp), Namespace: cp.NeutronNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).To(Succeed())

	cp.Spec.Services.Neutron = nil
	cp.Annotations = map[string]string{neutronDeletionAllowedAnnotation: "true"}

	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "opt-in annotation must delete the owned child")

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialSecretName(cp), Namespace: cp.NeutronNamespace(),
	}, &esov1.ExternalSecret{})).NotTo(Succeed(), "the DB-credential ExternalSecret must be swept too")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialSecretName(cp), Namespace: cp.NeutronNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(), "the VaultDynamicSecret generator must be swept too")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialServiceAccountName, Namespace: cp.NeutronNamespace(),
	}, &corev1.ServiceAccount{})).NotTo(Succeed(), "the generator's ServiceAccount must be swept too")
	sweptCert := &unstructured.Unstructured{}
	sweptCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialClientCertName(cp), Namespace: cp.NeutronNamespace(),
	}, sweptCert)).NotTo(Succeed(), "the mTLS client Certificate must be swept too")
}

// TestReconcileNeutron_UnsetDeletesMessagingSecretsWithOptIn covers the bus
// delivery on that same sweep: the transport-URL Secret and the CA mirror are the
// only broker material in the namespace, and nothing else reaps them. A same-named
// Secret this ControlPlane never wrote survives, because the name is derived and a
// shared namespace may already carry one.
func TestReconcileNeutron_UnsetDeletesMessagingSecretsWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("ca-bundle")},
	}
	r := newNeutronTestReconciler(t, cp, busCA)

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	for _, name := range []string{neutronMessagingSecretName(cp), neutronMessagingCASecretName(cp)} {
		g.Expect(r.Get(ctx, types.NamespacedName{Name: name, Namespace: cp.Namespace},
			&corev1.Secret{})).To(Succeed(), "the delivery was written alongside the child")
	}

	cp.Spec.Services.Neutron = nil
	cp.Annotations = map[string]string{neutronDeletionAllowedAnnotation: "true"}
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, name := range []string{neutronMessagingSecretName(cp), neutronMessagingCASecretName(cp)} {
		g.Expect(r.Get(ctx, types.NamespacedName{Name: name, Namespace: cp.Namespace},
			&corev1.Secret{})).NotTo(Succeed(), "the bus delivery must be swept with the child")
	}

	// A foreign Secret at the derived transport-URL name is never touched.
	other := neutronControlPlane()
	other.Spec.Services.Neutron = nil
	other.Annotations = map[string]string{neutronDeletionAllowedAnnotation: "true"}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: neutronMessagingSecretName(other), Namespace: other.Namespace,
	}}
	r2 := newNeutronTestReconciler(t, other, foreign)

	_, err = r2.reconcileNeutron(ctx, other)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r2.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{})).To(Succeed(),
		"a messaging Secret we do not own must never be deleted")
}

// TestReconcileNeutron_UnsetPreservesForeignObjects proves the deletion sweep is
// ownership-checked across every object it names: a Neutron child and, most
// importantly, the FIXED-name neutron-db-creds ServiceAccount that this
// ControlPlane does NOT own (no owner reference, no ownership labels) both survive
// an opt-in teardown. The ServiceAccount name is not CR-derived, so in a shared
// service namespace it is exactly the object a collision would hand to somebody
// else.
func TestReconcileNeutron_UnsetPreservesForeignObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron = nil
	cp.Annotations = map[string]string{neutronDeletionAllowedAnnotation: "true"}

	foreignChild := &neutronv1alpha1.Neutron{
		ObjectMeta: metav1.ObjectMeta{Name: neutronName(cp), Namespace: cp.NeutronNamespace()},
	}
	foreignSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: neutronDBCredentialServiceAccountName, Namespace: cp.NeutronNamespace(),
		},
	}
	r := newNeutronTestReconciler(t, cp, foreignChild, foreignSA)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronName(cp), Namespace: cp.NeutronNamespace(),
	}, &neutronv1alpha1.Neutron{})).To(Succeed(), "a Neutron child we do not own must never be deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronDBCredentialServiceAccountName, Namespace: cp.NeutronNamespace(),
	}, &corev1.ServiceAccount{})).To(Succeed(), "a foreign neutron-db-creds ServiceAccount must never be deleted")
}

// TestReconcileNeutron_UnsetDeletionToleratesAlreadyGoneObjects covers the
// partially-cleaned state a repeated teardown reaches: every object the sweep
// names may already be gone (a previous pass removed it, or it was never
// projected), and each delete tolerates NotFound so the reconcile converges
// instead of failing on the first missing object.
func TestReconcileNeutron_UnsetDeletionToleratesAlreadyGoneObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron = nil
	cp.Annotations = map[string]string{neutronDeletionAllowedAnnotation: "true"}
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	// Nothing was ever projected, so every named object is already absent.
	res, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// And a second pass over the same empty state stays clean.
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NeutronNotManaged"))
}

// TestReconcileNeutron_UnresolvableBackingServicesRequeue is the nil-safety
// fail-safe: a webhook-bypassed CR that dropped spec.infrastructure, or only the
// bus block inside it, has nothing to project and no bus to deliver, so the
// projection requeues instead of dereferencing nil, and writes no condition it
// would then have to retract.
func TestReconcileNeutron_UnresolvableBackingServicesRequeue(t *testing.T) {
	for _, tt := range []struct {
		name  string
		apply func(cp *c5c3v1alpha1.ControlPlane)
	}{
		{
			name:  "no infrastructure at all",
			apply: func(cp *c5c3v1alpha1.ControlPlane) { cp.Spec.Infrastructure = nil },
		},
		{
			name:  "infrastructure without a messaging block",
			apply: func(cp *c5c3v1alpha1.ControlPlane) { cp.Spec.Infrastructure.Messaging = nil },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := neutronControlPlane()
			tt.apply(cp)
			r := newNeutronTestReconciler(t, cp)

			res, err := r.reconcileNeutron(context.Background(), cp)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)).To(BeNil(),
				"the fail-safe must not write a condition it cannot substantiate")

			var list neutronv1alpha1.NeutronList
			g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
			g.Expect(list.Items).To(BeEmpty(), "nothing may be projected against unresolvable backing services")
		})
	}
}

func TestReconcileNeutron_GatedOnKeystoneReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             "WaitingForKeystone",
		Message:            "not ready",
	})
	r := newNeutronTestReconciler(t, cp)

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForKeystone"))

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileNeutron_GatedOnOVNReady is the gate Neutron carries beyond its
// peers: the ML2/OVN mechanism driver writes every network, subnet, and port into
// the referenced central's Northbound database, so a Neutron projected against a
// central that does not serve would park unready with nothing to fix on this side.
func TestReconcileNeutron_GatedOnOVNReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeOVNReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             "OVNCentralNotFound",
		Message:            "OVNCentral default/ovn does not exist",
	})
	r := newNeutronTestReconciler(t, cp)

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForOVN"))

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileNeutron_MessagingWaitHalts covers the managed bus whose
// RabbitmqCluster is not there yet: the delivery is a wait, and until it lands
// neither the registration nor the child may be written, because a Neutron with no
// transport URL cannot serve an RPC call.
func TestReconcileNeutron_MessagingWaitHalts(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "openstack-rabbitmq"},
	}
	// Built without the seeded registration the other tests get, so an empty
	// KeystoneService list is evidence the leg never ran rather than a fixture.
	s := neutronTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withReadyNeutronDBCred([]client.Object{cp})...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &neutronv1alpha1.Neutron{},
			&c5c3v1alpha1.KeystoneService{}).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "a bus that has not been created yet is a wait, not a failure")
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be projected without a transport URL")
	var registrations c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(context.Background(), &registrations)).To(Succeed())
	g.Expect(registrations.Items).To(BeEmpty(), "the bus gate runs before the registration is projected")
}

// TestReconcileNeutron_MessagingErrorSurfaces covers the bus block that named
// neither a cluster nor a Secret, which only a bypassed admission produces: it
// does not converge on its own, so it is returned as an error rather than waited
// out.
func TestReconcileNeutron_MessagingErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{}
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resolving the shared bus transport URL"))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NeutronMessagingError"))
}

// TestReconcileNeutron_GatedOnRegistrationAccountNotReady pins the registration
// gate: while the child's AccountReady is False no Neutron is projected, the
// child's own reason and message are relayed, and a Neutron projected by an
// earlier pass is left running on the credentials it already has.
func TestReconcileNeutron_GatedOnRegistrationAccountNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	ks := neutronRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  reasonServiceAccountCollision,
		Message: `user "neutron" already exists in Keystone`,
	})
	existing := &neutronv1alpha1.Neutron{
		ObjectMeta: metav1.ObjectMeta{Name: neutronName(cp), Namespace: cp.NeutronNamespace()},
		Spec:       neutronv1alpha1.NeutronSpec{Region: "RegionPrevious"},
	}
	r := newNeutronTestReconciler(t, cp, ks, existing)

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring(`user "neutron" already exists in Keystone`))

	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Region).To(Equal("RegionPrevious"),
		"the gate must write no Neutron at all, leaving a previously projected one untouched")
}

// TestReconcileNeutron_GatedOnRegistrationWithoutConditions covers the child that
// exists but has not been reconciled yet: the gate holds on a waiting message
// rather than reading a missing condition as ready.
func TestReconcileNeutron_GatedOnRegistrationWithoutConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp, neutronRegistration(cp))

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(conditionTypeKeystoneServiceAccountReady))

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the registration gate must block projection")
}

// TestReconcileNeutron_RegistrationNotFoundAfterEnsureHolds covers the read-back
// that misses: a child the API server has not made readable yet is a wait, not an
// error.
func TestReconcileNeutron_RegistrationNotFoundAfterEnsureHolds(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	s := neutronTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withNeutronBusSecret(withReadyNeutronDBCred([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &neutronv1alpha1.Neutron{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return apierrors.NewNotFound(
						schema.GroupResource{Group: "c5c3.io", Resource: "keystoneservices"}, key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "a child that is not readable yet is not a failure")
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileNeutron_RegistrationReadFailureSurfaces covers the other half: a
// read that fails for any reason OTHER than absence is an error, wrapped with what
// it was reading.
func TestReconcileNeutron_RegistrationReadFailureSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	s := neutronTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withNeutronBusSecret(withReadyNeutronDBCred([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &neutronv1alpha1.Neutron{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("reading the neutron KeystoneService child:"))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
}

// TestReconcileNeutron_NeverAdoptsForeignRegistration proves the registration
// write is refused rather than allowed to overwrite a same-named KeystoneService
// in a namespace the ControlPlane does not own: the refusal surfaces on
// NeutronReady and the foreign CR keeps its spec.
func TestReconcileNeutron_NeverAdoptsForeignRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "neutron", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: neutronName(cp), Namespace: "neutron"},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
		},
	}
	r := newNeutronTestReconciler(t, cp, foreign)

	_, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign registration must be refused")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
	g.Expect(cond.Message).To(ContainSubstring("refusing to adopt pre-existing"))

	var live c5c3v1alpha1.KeystoneService
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: neutronName(cp), Namespace: "neutron",
	}, &live)).To(Succeed())
	g.Expect(live.Spec.ControlPlaneRef.Name).To(Equal("someone-else"),
		"a foreign registration must never be overwritten")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel))
}

// TestReconcileNeutron_DBCredentialErrorSurfacesAndReturns pins the error leg of
// the credential ensure: in a service namespace the ControlPlane does not own, a
// pre-existing foreign ExternalSecret at the derived name is never adopted, and
// the refusal is both reported as NeutronDBCredentialError and returned to the
// pipeline rather than swallowed into a wait.
func TestReconcileNeutron_DBCredentialErrorSurfacesAndReturns(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "neutron", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}
	// Somebody else's ExternalSecret under the name our Static branch projects.
	foreign := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: neutronDBCredentialSecretName(cp), Namespace: "neutron",
	}}
	r := newNeutronTestReconciler(t, cp, foreign)

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign ExternalSecret must be refused")
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NeutronDBCredentialError"))

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be projected against a credential that was never ensured")
}

// TestReconcileNeutron_DynamicCredentialNotReady_DefersProjection is the gate that
// keeps the Dynamic default from failing OPEN. The engine role behind the
// generator is provisioned by a MANUAL onboarding step (setup-database-tenant.sh),
// while the operator rolls out on its own, so a ControlPlane can reach here with
// no role to mint against. Until the credential materialises no Neutron child may
// be projected at all.
func TestReconcileNeutron_DynamicCredentialNotReady_DefersProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp, notReadyNeutronDBCredES(cp))

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForNeutronDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(neutronDBDynamicCredsPathFor(cp)),
		"the condition must name the engine path an operator has to onboard")

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no Neutron child may be projected before the credential lands")
}

// staleStaticNeutronDBCredSecret builds the Secret a MIGRATED cluster is left with
// right after the Static->Dynamic flip: materialised by the last STATIC sync, so
// it still carries the retired bootstrap's username=neutron seed. That name is a
// syntactically valid username, so a gate that only checks for a non-empty
// username would wave it through, but no MySQL user was ever created under it (the
// static login is the Neutron CR name).
func staleStaticNeutronDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	secret := materialisedNeutronDBCredSecret(cp)
	secret.Data["username"] = []byte("neutron")
	return secret
}

// TestReconcileNeutron_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic
// is the regression guard for the failure an ExternalSecret-only gate lets
// through. A Static->Dynamic flip create-or-updates the ExternalSecret IN PLACE,
// so on a migrated cluster it keeps reporting Ready from its last Static sync
// while the Secret behind it still holds the retired static seed. Flipping the
// child on that Ready alone stops the neutron-operator asserting the static
// User/Grant Neutron was serving on and points it at a login that never existed,
// an outage behind NeutronReady=True.
func TestReconcileNeutron_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	// A Ready ExternalSecret over a Secret still holding the static seed: exactly
	// what a cluster upgraded from the Static path presents to the reconciler.
	r := newNeutronTestReconciler(t, cp, readyNeutronDBCredES(cp), staleStaticNeutronDBCredSecret(cp))

	// Static deployment: the child is projected and runs on the static credential.
	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic))

	// Flip to Dynamic. The ExternalSecret is Ready, but only from the Static sync.
	cp.Spec.Services.Neutron.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	res, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic),
			"a Ready ExternalSecret over a stale static username must not flip the running child to Dynamic")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForNeutronDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(`"neutron"`),
		"the condition must name the non-engine-issued username it found")
	g.Expect(cond.Message).To(ContainSubstring(neutronDBCredentialSecretName(cp)),
		"the condition must name the Secret an operator has to delete")
}

// --- projected child fields ---

// TestReconcileNeutron_ProjectedChildFields is the field-mapping lock for the
// projection: the release-derived image, the backing services (with the fixed
// neutron schema, the operator-owned secretRef, and the Dynamic mode of the
// managed shared database), the top-down Keystone endpoint, the region, the
// service user the registration child declares, the resolved store ref, both
// replica counts, the bus delivery, and the resolved OVN central ref.
func TestReconcileNeutron_ProjectedChildFields(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	// An exposed Keystone: its external URL must reach the child as the public
	// endpoint only, never as the token-validation endpoint.
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com:8443/v3"
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Name).To(Equal("cp-neutron"))
	g.Expect(nn.Spec.OpenStackRelease).To(Equal("2025.2"))
	g.Expect(nn.Spec.Image.Repository).To(Equal("ghcr.io/c5c3/neutron"))
	g.Expect(nn.Spec.Image.Tag).To(Equal("2025.2"), "the tag defaults to spec.openStackRelease")

	// Database: the shared cluster, the fixed neutron schema, the operator-owned
	// credential Secret, and the managed-shared Dynamic default.
	g.Expect(nn.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(nn.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(nn.Spec.Database.Database).To(Equal("neutron"),
		"the logical schema must be neutron, not the shared block's keystone")
	g.Expect(nn.Spec.Database.SecretRef.Name).To(Equal(neutronDBCredentialSecretName(cp)))
	g.Expect(nn.Spec.Database.SecretRef.Key).To(Equal("password"))
	g.Expect(nn.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic),
		"a managed shared neutron database defaults to Dynamic (engine-issued) credentials")
	// DeepCopy: the projected pointers must not alias the ControlPlane spec.
	g.Expect(nn.Spec.Database.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Database.ClusterRef))
	g.Expect(nn.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(nn.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	g.Expect(nn.Spec.Cache.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Cache.ClusterRef))

	// The Keystone endpoint is derived top-down, never from the external exposure.
	g.Expect(nn.Spec.KeystoneEndpoint).To(Equal("http://cp-keystone.default.svc:5000/v3"),
		"the token-validation endpoint must be the cluster-local Service URL")
	g.Expect(nn.Spec.KeystonePublicEndpoint).To(Equal("https://keystone.example.com:8443/v3"),
		"the public endpoint carries the browser/client-facing URL")

	g.Expect(nn.Spec.Region).To(Equal("RegionOne"))

	// The service user names the account the registration child declares, and reads
	// its password from the consumer Secret the registration delivers.
	g.Expect(nn.Spec.ServiceUser.Username).To(Equal("neutron"))
	g.Expect(nn.Spec.ServiceUser.ProjectName).To(Equal("service-neutron"))
	// Both domains resolve to the ControlPlane's effective admin domain, which is
	// what the registration resolves its own unset domainName to.
	g.Expect(nn.Spec.ServiceUser.UserDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(nn.Spec.ServiceUser.ProjectDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(nn.Spec.ServiceUser.SecretRef.Name).To(Equal("cp-neutron-credentials"))
	g.Expect(nn.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))

	// The resolved store selection, so the child never falls back to its own default.
	g.Expect(nn.Spec.SecretStoreRef).NotTo(BeNil())
	g.Expect(nn.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(nn.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))

	g.Expect(nn.Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))
	g.Expect(nn.Spec.Workers.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas),
		"the RPC workers take the same operator default as the API pods")

	// The bus reaches the child as a brownfield secretRef naming the delivery
	// written beside it, never as the ControlPlane's own clusterRef.
	g.Expect(nn.Spec.Messaging.ClusterRef).To(BeNil())
	g.Expect(nn.Spec.Messaging.SecretRef).NotTo(BeNil())
	g.Expect(nn.Spec.Messaging.SecretRef.Name).To(Equal("cp-neutron-messaging"))
	g.Expect(nn.Spec.Messaging.SecretRef.Key).To(Equal(commonv1.DefaultTransportURLSecretKey))
	g.Expect(nn.Spec.Messaging.TLS).To(BeNil(), "a plaintext bus projects no trust anchor")

	// The OVN control plane, with the namespace resolved by the ControlPlane.
	g.Expect(nn.Spec.OVN.CentralRef.Name).To(Equal("ovn"))
	g.Expect(nn.Spec.OVN.CentralRef.Namespace).To(Equal("default"))

	g.Expect(metav1.IsControlledBy(nn, cp)).To(BeTrue(),
		"the projected Neutron must carry the ControlPlane controller owner reference")
}

// TestReconcileNeutron_LeavesAPIServerAndOVNDBSyncUnset pins the Placement
// posture on the blocks the ControlPlane deliberately does not drive: the
// child-side defaults stay authoritative, and the OVN database synchronisation
// stays a standalone-CR decision because a repair run rewrites the whole logical
// model.
func TestReconcileNeutron_LeavesAPIServerAndOVNDBSyncUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.APIServer).To(BeNil(), "the child-side uWSGI defaults stay authoritative")
	g.Expect(nn.Spec.OVNDBSync).To(BeNil(), "scheduling the OVN db-sync stays a standalone-CR decision")
	g.Expect(nn.Spec.NetworkPolicy).To(BeNil())
	g.Expect(nn.Spec.Autoscaling).To(BeNil())
	g.Expect(nn.Spec.Logging).To(BeNil())
}

func TestReconcileNeutron_ImageOverrideWins(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/mirror/neutron",
		Tag:        "custom",
	}
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.Image.Repository).To(Equal("registry.example.com/mirror/neutron"))
	g.Expect(nn.Spec.Image.Tag).To(Equal("custom"))
}

// TestReconcileNeutron_DatabaseBrownfieldLeavesCredentialsModeUntouched is the
// other half of the credentials-mode contract: a database with no ClusterRef
// carries a user-supplied credential, so the mode and the secretRef are left as
// declared and no DB-credential ExternalSecret is projected.
func TestReconcileNeutron_DatabaseBrownfieldLeavesCredentialsModeUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "brownfield-db"},
	}
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.Database.ClusterRef).To(BeNil())
	g.Expect(nn.Spec.Database.Database).To(Equal("neutron"),
		"the logical schema is always overridden to neutron, even for a brownfield database")
	g.Expect(nn.Spec.Database.CredentialsMode).To(BeEmpty(),
		"a brownfield database must keep its credentialsMode untouched")
	g.Expect(nn.Spec.Database.SecretRef.Name).To(Equal("brownfield-db"),
		"a brownfield database keeps its user-supplied secretRef")

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: neutronDBCredentialSecretName(cp), Namespace: cp.NeutronNamespace(),
	}, &esov1.ExternalSecret{})).NotTo(Succeed(),
		"no DB-credential ExternalSecret is projected in brownfield mode")
}

// TestReconcileNeutron_ProjectsBrownfieldMessagingSecretRef pins the delivery
// contract: the neutron operator resolves spec.messaging in the Neutron's own
// namespace on the Neutron's own cluster, so a managed bus declared on the
// ControlPlane reaches the child as a brownfield secretRef naming the Secret this
// pass wrote there.
func TestReconcileNeutron_ProjectsBrownfieldMessagingSecretRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.Messaging.ClusterRef).To(BeNil(),
		"the child never resolves the ControlPlane's own bus reference")
	g.Expect(nn.Spec.Messaging.SecretRef).To(Equal(&commonv1.SecretRefSpec{
		Name: "cp-neutron-messaging", Key: commonv1.DefaultTransportURLSecretKey,
	}))
	g.Expect(nn.Spec.Messaging.TLS).To(BeNil())

	delivered := &corev1.Secret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronMessagingSecretName(cp), Namespace: cp.NeutronNamespace(),
	}, delivered)).To(Succeed())
	g.Expect(delivered.Data).To(HaveKeyWithValue(commonv1.DefaultTransportURLSecretKey, []byte(neutronBusURL)))
}

// TestReconcileNeutron_ProjectsTLSMirrorWhenSharedBusHasTLS covers the trust
// anchor: a bus that declares TLS gets its CA bundle mirrored beside the child,
// and the child's messaging block names the mirror rather than the bundle Secret
// in the ControlPlane's namespace, which its own cluster may not carry. Dropping
// the tls block again has to revert BOTH halves on the same pass: the mirror is
// deleted, so a child left pointing at it would wedge its pods on a volume source
// that no longer exists.
func TestReconcileNeutron_ProjectsTLSMirrorWhenSharedBusHasTLS(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	bundle := []byte("-----BEGIN CERTIFICATE-----\nbus\n-----END CERTIFICATE-----\n")
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": bundle},
	}
	r := newNeutronTestReconciler(t, cp, busCA)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.Messaging.TLS).NotTo(BeNil())
	g.Expect(nn.Spec.Messaging.TLS.CABundleSecretRef).To(Equal(commonv1.SecretRefSpec{
		Name: "cp-neutron-messaging-ca", Key: neutronMessagingCAKey,
	}))

	mirror := &corev1.Secret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronMessagingCASecretName(cp), Namespace: cp.NeutronNamespace(),
	}, mirror)).To(Succeed())
	g.Expect(mirror.Data).To(HaveKeyWithValue(neutronMessagingCAKey, bundle))

	// Drop the tls block: the child must revert instead of pinning the last value,
	// and the mirror comes down behind it. The child is converged first because the
	// reap waits for that verdict (see
	// TestReconcileNeutron_KeepsTheCAMirrorUntilTheChildHasConvergedOnTheDrop).
	convergeNeutronChild(t, r, cp, getProjectedNeutron(t, r.Client, cp).Generation)
	cp.Spec.Infrastructure.Messaging.TLS = nil
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nn = getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.Messaging.TLS).To(BeNil(),
		"the child must not keep a trust anchor whose mirror this same pass deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronMessagingCASecretName(cp), Namespace: cp.NeutronNamespace(),
	}, &corev1.Secret{})).NotTo(Succeed())
}

// convergeNeutronChild stands in for the neutron-operator: it stamps
// status.observedGeneration on the projected Neutron child and marks it Ready, the
// two halves of the verdict reconcileNeutron reads before it reaps the CA mirror.
func convergeNeutronChild(
	t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane, observedGeneration int64,
) {
	t.Helper()
	nn := getProjectedNeutron(t, r.Client, cp)
	nn.Status.ObservedGeneration = observedGeneration
	conditions.SetCondition(&nn.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: observedGeneration,
		Reason:             "AllReady",
		Message:            "ready",
	})
	if err := r.Client.Status().Update(context.Background(), nn); err != nil {
		t.Fatalf("converging the projected Neutron: %v", err)
	}
}

// TestReconcileNeutron_KeepsTheCAMirrorUntilTheChildHasConvergedOnTheDrop covers
// the far side of the apply that removes the pointer. Removing spec.messaging.tls
// from the CR does not remove the volume from the workload: the neutron-operator
// renders the Deployment on a pass of its own, and until it has, the live pod
// template still names the mirror as a REQUIRED Secret volume source. Reaping it
// in that window leaves every pod created in it — an eviction, a node drain, a
// rollout the operator triggers for an unrelated digest — stuck on FailedMount,
// and a neutron-operator that is down or backing off never closes the window at
// all. So the reap waits for the child to report the generation the apply
// produced.
func TestReconcileNeutron_KeepsTheCAMirrorUntilTheChildHasConvergedOnTheDrop(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("bus bundle")},
	}
	r := newNeutronTestReconciler(t, cp, busCA)
	mirrorKey := types.NamespacedName{
		Name: neutronMessagingCASecretName(cp), Namespace: cp.NeutronNamespace(),
	}

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, mirrorKey, &corev1.Secret{})).To(Succeed())

	// Pin the child one generation behind: Ready and converged on the spec that
	// still carried the pointer, which is exactly the status the API server returns
	// to the apply that drops it.
	nn := getProjectedNeutron(t, r.Client, cp)
	nn.Generation = 2
	g.Expect(r.Client.Update(ctx, nn)).To(Succeed())
	convergeNeutronChild(t, r, cp, 1)

	cp.Spec.Infrastructure.Messaging.TLS = nil
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Messaging.TLS).To(BeNil(),
		"the apply must drop the pointer from the CR")
	g.Expect(r.Get(ctx, mirrorKey, &corev1.Secret{})).To(Succeed(),
		"the mirror must outlive the pointer until the child has re-rendered without it")

	// The neutron-operator catches up: the child reports the generation the apply
	// produced, so the volume source is gone from the workload too.
	convergeNeutronChild(t, r, cp, 2)
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, mirrorKey, &corev1.Secret{}))).To(BeTrue(),
		"a converged child leaves no trust anchor behind in the Neutron namespace")
}

// TestReconcileNeutron_KeepsTheCAMirrorWhileTheChildStillNamesIt covers the
// window the same-pass revert above does not: the gates between the messaging leg
// and the projection can halt the pass with the child's spec.messaging.tls still
// naming the mirror. Reaping the mirror on the messaging leg would leave the LIVE
// child pointing at a volume source that no longer exists, so every Neutron pod
// that restarts during a service-account or DB-credential rotation wedges on
// CreateContainerConfigError, with nothing in the condition set naming the cause.
// The referent must outlive its last reference, not the other way round.
func TestReconcileNeutron_KeepsTheCAMirrorWhileTheChildStillNamesIt(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("bus bundle")},
	}
	r := newNeutronTestReconciler(t, cp, busCA)

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Messaging.TLS).NotTo(BeNil())

	// Drop the tls block, and halt this pass after the messaging leg by putting the
	// registration back into a rotation: the child is never re-applied, so its
	// pointer at the mirror stays live.
	cp.Spec.Infrastructure.Messaging.TLS = nil
	rotating := &c5c3v1alpha1.KeystoneService{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronName(cp), Namespace: cp.NeutronNamespace(),
	}, rotating)).To(Succeed())
	conditions.SetCondition(&rotating.Status.Conditions, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  "RotatingPassword",
		Message: "the service account password is being rotated",
	})
	g.Expect(r.Status().Update(ctx, rotating)).To(Succeed())

	res, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).NotTo(BeZero(), "the pass has to halt on the registration gate")
	g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady).Reason).
		To(Equal(reasonWaitingForServiceRegistration))

	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Messaging.TLS).NotTo(BeNil(),
		"the halted pass left the child's pointer at the mirror in place")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: neutronMessagingCASecretName(cp), Namespace: cp.NeutronNamespace(),
	}, &corev1.Secret{})).To(Succeed(),
		"the mirror must not be deleted while the live child still names it as a volume source")
}

// TestReconcileNeutron_ExtraConfigMerge proves the projected child's
// spec.extraConfig is the key-by-key merge of globalExtraConfig and the
// per-service block: the per-service value wins on an overlapping key, a
// global-only key in the same section survives, and a global-only section is
// carried over.
func TestReconcileNeutron_ExtraConfigMerge(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{
		"database": {
			"connection_recycle_time": "280",
			"max_pool_size":           "5",
		},
		"DEFAULT": {"debug": "true"},
	}
	cp.Spec.Services.Neutron.ExtraConfig = map[string]map[string]string{
		"database": {"connection_recycle_time": "600"}, // overrides global
	}
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.ExtraConfig).To(Equal(map[string]map[string]string{
		"database": {
			"connection_recycle_time": "600", // per-service wins
			"max_pool_size":           "5",   // global-only key in the same section
		},
		"DEFAULT": {"debug": "true"}, // global-only section
	}), "per-service extraConfig must win, global keys/sections merged in")
}

// TestReconcileNeutron_ExtraConfigClearedProjectsNil proves the field is assigned
// unconditionally: clearing both extraConfig blocks reverts the child to an absent
// spec.extraConfig rather than leaving the previously-projected value pinned.
func TestReconcileNeutron_ExtraConfigClearedProjectsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": "true"}}
	cp.Spec.Services.Neutron.ExtraConfig = map[string]map[string]string{
		"database": {"connection_recycle_time": "600"},
	}
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.ExtraConfig).NotTo(BeEmpty())

	cp.Spec.GlobalExtraConfig = nil
	cp.Spec.Services.Neutron.ExtraConfig = nil
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.ExtraConfig).To(BeNil(),
		"clearing both extraConfig blocks must revert the child")
}

func TestReconcileNeutron_GatewayNilClears(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "neutron.example.com",
	}
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.Gateway).NotTo(BeNil())
	g.Expect(nn.Spec.Gateway.Hostname).To(Equal("neutron.example.com"))

	// Clearing the gateway reverts the child rather than pinning the old value.
	cp.Spec.Services.Neutron.Gateway = nil
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	nn = getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.Gateway).To(BeNil(), "clearing the gateway must tear the HTTPRoute down")
}

func TestReconcileNeutron_ReplicasOverrideAndRevert(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.Replicas = ptr.To(int32(5))
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Deployment.Replicas).To(Equal(int32(5)))

	cp.Spec.Services.Neutron.Replicas = nil
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas),
		"clearing the override must revert the child to the operator default")
}

// TestReconcileNeutron_ProjectsWorkerReplicasOverrideAndRevert covers the second
// replica knob: the RPC workers run their own Deployment, and the override exists
// because a single-node devstack cannot carry six idle worker pods beside the rest
// of the control plane.
func TestReconcileNeutron_ProjectsWorkerReplicasOverrideAndRevert(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.WorkerReplicas = ptr.To(int32(1))
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Workers.Deployment.Replicas).To(Equal(int32(1)))

	cp.Spec.Services.Neutron.WorkerReplicas = nil
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.Workers.Deployment.Replicas).
		To(Equal(commonv1.DefaultReplicas), "clearing the override must revert the child to the operator default")
}

// TestReconcileNeutron_ProjectsOVNCentralRef pins the namespace resolution the
// ControlPlane performs rather than delegating: an empty ref namespace means the
// ControlPlane's own namespace, which is not what the child would default it to
// once the network service is placed in a namespace of its own.
func TestReconcileNeutron_ProjectsOVNCentralRef(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	// An empty ref namespace resolves to the ControlPlane's namespace, even though
	// the child itself lives elsewhere.
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "neutron", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newNeutronTestReconciler(t, cp)
	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.OVN.CentralRef).
		To(Equal(neutronv1alpha1.OVNCentralRef{Name: "ovn", Namespace: "default"}))

	// An explicit namespace passes through untouched.
	placed := neutronControlPlane()
	placed.Spec.Services.Neutron.OVN.CentralRef.Namespace = "ovn-system"
	r2 := newNeutronTestReconciler(t, placed)
	_, err = r2.reconcileNeutron(ctx, placed)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r2.Client, placed).Spec.OVN.CentralRef).
		To(Equal(neutronv1alpha1.OVNCentralRef{Name: "ovn", Namespace: "ovn-system"}))
}

// TestReconcileNeutron_MirrorsChildReady exercises the readiness mirror: a fresh
// child is not ready (WaitingForNeutron + requeue), a Ready child flips
// NeutronReady True.
func TestReconcileNeutron_MirrorsChildReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	res, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForNeutron"))

	nn := getProjectedNeutron(t, r.Client, cp)
	conditions.SetCondition(&nn.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nn.Generation,
		Reason:             "AllReady",
		Message:            "ready",
	})
	g.Expect(r.Client.Status().Update(ctx, nn)).To(Succeed())

	res, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond = conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NeutronReady"))
}

// TestReconcileNeutron_CrossNamespaceChildIsLabelledNotOwned verifies the
// ownership substitute for a Neutron placed in a namespace of its own: the child
// carries the ControlPlane's ownership labels and NO owner reference (Kubernetes
// forbids a cross-namespace one).
func TestReconcileNeutron_CrossNamespaceChildIsLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "neutron", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Namespace).To(Equal("neutron"))
	g.Expect(nn.OwnerReferences).To(BeEmpty(), "a cross-namespace child cannot carry an owner reference")
	g.Expect(nn.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(nn.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
}

// --- the projected KeystoneService registration ---

func getProjectedNeutronRegistration(
	t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) *c5c3v1alpha1.KeystoneService {
	t.Helper()
	ks := &c5c3v1alpha1.KeystoneService{}
	key := types.NamespacedName{Name: neutronName(cp), Namespace: cp.NeutronNamespace()}
	if err := c.Get(context.Background(), key, ks); err != nil {
		t.Fatalf("getting projected KeystoneService %s: %v", key, err)
	}
	return ks
}

// TestReconcileNeutron_ProjectsTheRegistration pins the registration's content:
// the network catalog entry with both endpoint rows, the service account in its
// own per-service project, and the explicit controlPlaneRef a child in a dedicated
// namespace needs to resolve the ControlPlane at all. The public row's three-step
// fallback is covered with it, because it is the URL every client resolves to
// create its networks.
func TestReconcileNeutron_ProjectsTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedNeutronRegistration(t, r.Client, cp)
	g.Expect(ks.Name).To(Equal("cp-neutron"))
	g.Expect(ks.Namespace).To(Equal("default"))
	g.Expect(ks.Spec.ControlPlaneRef.Name).To(Equal("cp"))
	g.Expect(ks.Spec.ControlPlaneRef.Namespace).To(Equal("default"),
		"the namespace is explicit so a child in a dedicated namespace resolves the right ControlPlane")

	g.Expect(ks.Spec.Catalog).NotTo(BeNil())
	g.Expect(ks.Spec.Catalog.ServiceType).To(Equal("network"))
	g.Expect(ks.Spec.Catalog.ServiceName).To(Equal("neutron"))
	g.Expect(ks.Spec.Catalog.Adopt).To(BeFalse(), "a colliding catalog row must fail loud, never be adopted")
	g.Expect(ks.Spec.Catalog.Endpoints).To(HaveLen(2))
	g.Expect(ks.Spec.Catalog.Endpoints[0].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypeInternal))
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal("http://cp-neutron.default.svc:9696"))
	g.Expect(ks.Spec.Catalog.Endpoints[1].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypePublic))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal("http://cp-neutron.default.svc:9696"),
		"an unexposed Neutron advertises the in-cluster URL on both rows")

	g.Expect(ks.Spec.Account).NotTo(BeNil())
	g.Expect(ks.Spec.Account.UserName).To(Equal("neutron"))
	g.Expect(ks.Spec.Account.DomainName).To(BeEmpty(),
		"an unset domain lets the registration resolve the ControlPlane's admin domain")
	g.Expect(ks.Spec.Account.Adopt).To(BeFalse(), "a colliding user must fail loud, never be taken over")
	g.Expect(ks.Spec.Account.Project.Name).To(Equal("service-neutron"))
	g.Expect(ks.Spec.Account.Project.Create).To(BeTrue())
	g.Expect(ks.Spec.Account.Roles).To(Equal([]string{"service"}))

	g.Expect(metav1.IsControlledBy(ks, cp)).To(BeTrue(),
		"a co-located registration carries the ControlPlane controller owner reference")

	// A gateway alone yields the default-443 form.
	gated := neutronControlPlane()
	gated.Spec.Services.Neutron.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "neutron.example.com",
	}
	rGated := newNeutronTestReconciler(t, gated)
	_, err = rGated.reconcileNeutron(context.Background(), gated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutronRegistration(t, rGated.Client, gated).Spec.Catalog.Endpoints[1].URL).
		To(Equal("https://neutron.example.com"))

	// An explicit publicEndpoint wins over it, the only way to advertise a
	// non-443 external port.
	explicit := neutronControlPlane()
	explicit.Spec.Services.Neutron.Gateway = gated.Spec.Services.Neutron.Gateway
	explicit.Spec.Services.Neutron.PublicEndpoint = "https://neutron.example.com:8443"
	rExplicit := newNeutronTestReconciler(t, explicit)
	_, err = rExplicit.reconcileNeutron(context.Background(), explicit)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutronRegistration(t, rExplicit.Client, explicit).Spec.Catalog.Endpoints[1].URL).
		To(Equal("https://neutron.example.com:8443"))
}

// TestReconcileNeutron_PlacedRegistrationEndpointsFollowTheNeutron covers the
// internal row of a placed service: the in-cluster Service URL resolves nowhere
// outside its cluster, so the placed entry advertises the public URL on both
// interfaces.
func TestReconcileNeutron_PlacedRegistrationEndpointsFollowTheNeutron(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedNeutronControlPlane("remote-a")
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedNeutronRegistration(t, r.Client, cp)
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal("https://neutron.example.com"))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal("https://neutron.example.com"))
}

// TestReconcileNeutron_CrossNamespaceRegistrationIsLabelledNotOwned verifies the
// ownership substitute for a registration in a namespace of its own: the two
// ownership labels and no owner reference.
func TestReconcileNeutron_CrossNamespaceRegistrationIsLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "neutron", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedNeutronRegistration(t, r.Client, cp)
	g.Expect(ks.Namespace).To(Equal("neutron"))
	g.Expect(ks.OwnerReferences).To(BeEmpty(), "a cross-namespace child cannot carry an owner reference")
	g.Expect(ks.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(ks.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	g.Expect(ks.Spec.ControlPlaneRef.Namespace).To(Equal("default"))
}

// TestReconcileNeutron_ReadyFoldsInTheRegistration proves NeutronReady is the
// conjunction of both children: a Ready Neutron whose registration collided on the
// catalog row keeps NeutronReady False, naming the failing child condition.
func TestReconcileNeutron_ReadyFoldsInTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	ks := neutronRegistration(cp,
		metav1.Condition{
			Type:    conditionTypeKeystoneServiceAccountReady,
			Status:  metav1.ConditionTrue,
			Reason:  reasonKeystoneServiceAccountProvisioned,
			Message: "account provisioned",
		},
		metav1.Condition{
			Type:    conditionTypeKeystoneServiceCatalogReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonKeystoneServiceCatalogCollision,
			Message: `a service row of type "network" named "neutron" already exists`,
		},
		metav1.Condition{
			Type:    conditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NotAllReady",
			Message: "One or more sub-conditions are not ready",
		},
	)
	r := newNeutronTestReconciler(t, cp, ks)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The Neutron child itself reaches Ready.
	nn := getProjectedNeutron(t, r.Client, cp)
	conditions.SetCondition(&nn.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nn.Generation,
		Reason:             "AllReady",
		Message:            "ready",
	})
	g.Expect(r.Client.Status().Update(ctx, nn)).To(Succeed())

	res, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"a Neutron nothing can discover through the catalog is not ready")
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogCollision),
		"the failing sub-condition's reason is relayed, not the aggregate's")
	g.Expect(cond.Message).To(ContainSubstring(conditionTypeKeystoneServiceCatalogReady))
	g.Expect(cond.Message).To(ContainSubstring("cp-neutron"))
}

// TestReconcileNeutron_UnsetDeletesRegistrationWithOptIn verifies the opt-in
// teardown removes the registration too, which is what unregisters Neutron from
// the catalog and the identity plane.
func TestReconcileNeutron_UnsetDeletesRegistrationWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	getProjectedNeutronRegistration(t, r.Client, cp)

	cp.Spec.Services.Neutron = nil
	cp.Annotations = map[string]string{neutronDeletionAllowedAnnotation: "true"}
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the opt-in annotation must delete the owned registration")
}

// TestReconcileNeutron_UnsetPreservesForeignRegistration is the ownership guard on
// that sweep: a same-named KeystoneService the ControlPlane does not own survives.
func TestReconcileNeutron_UnsetPreservesForeignRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron = nil
	cp.Annotations = map[string]string{neutronDeletionAllowedAnnotation: "true"}

	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: neutronName(cp), Namespace: cp.NeutronNamespace()},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
		},
	}
	r := newNeutronTestReconciler(t, cp, foreign)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: neutronName(cp), Namespace: cp.NeutronNamespace(),
	}, &c5c3v1alpha1.KeystoneService{})).To(Succeed(),
		"a KeystoneService we do not own must never be deleted")
}

// TestReconcileNeutron_UnsetPreservesRegistrationByDefault pins the preserve
// default: without the opt-in annotation a previously projected registration
// stays, so an accidental block drop never unregisters a running service.
func TestReconcileNeutron_UnsetPreservesRegistrationByDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Neutron = nil
	_, err = r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	getProjectedNeutronRegistration(t, r.Client, cp)
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NeutronNotManaged"))
}

// TestReconcileNeutron_NilBlockProjectsNoRegistration covers the staged-adoption
// path: a ControlPlane that manages no network service registers none either.
func TestReconcileNeutron_NilBlockProjectsNoRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron = nil
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestNeutronEndpointURL pins the in-cluster Neutron API endpoint convention the
// catalog registers against: http://{name}.{ns}.svc:9696.
func TestNeutronEndpointURL(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	g.Expect(neutronEndpointURL(cp)).To(Equal("http://cp-neutron.default.svc:9696"))
}

// --- per-service target clusters ---

// placedNeutronControlPlane places the network service in a namespace of its own
// on a target cluster. Its database is brownfield, so the DB-credential leg, whose
// own placement is covered in reconcile_neutron_dbcredentials_test.go, projects
// nothing and the pass reaches the child projection over the local client alone.
func placedNeutronControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      "neutron",
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Neutron.PublicEndpoint = "https://neutron.example.com"
	cp.Spec.Services.Neutron.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	return cp
}

// TestReconcileNeutron_ProjectsTheTargetClusterRef verifies the placement reaches
// the child verbatim, the neutron-operator owning everything on the target, and
// that an unplaced service projects no ref.
func TestReconcileNeutron_ProjectsTheTargetClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedNeutronControlPlane("remote-a")
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedNeutron(t, r.Client, cp).Spec.TargetClusterRef).
		To(Equal(&commonv1.TargetClusterRefSpec{Name: "remote-a"}))

	unplaced := neutronControlPlane()
	r2 := newNeutronTestReconciler(t, unplaced)
	_, err = r2.reconcileNeutron(context.Background(), unplaced)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNeutron(t, r2.Client, unplaced).Spec.TargetClusterRef).To(BeNil(),
		"a service that names no cluster must project no ref at all")
}

// TestNeutronKeystoneEndpoint_FollowsTheNeutron pins the endpoint policy: Neutron
// validates tokens against Keystone itself, so it gets the in-cluster Service DNS
// name exactly while the two services share a cluster, and the public URL as soon
// as they do not, because that name resolves nowhere else.
func TestNeutronKeystoneEndpoint_FollowsTheNeutron(t *testing.T) {
	const (
		inCluster = "http://cp-keystone.identity.svc:5000/v3"
		public    = "https://keystone.example.com/v3"
	)
	for _, tc := range []struct {
		name              string
		neutron, keystone *commonv1.TargetClusterRefSpec
		want              string
	}{
		{name: "both co-located", want: inCluster},
		{
			name:     "both on the same cluster",
			neutron:  &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     inCluster,
		},
		{
			name:    "Neutron placed, Keystone at home",
			neutron: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:    public,
		},
		{
			name:     "Keystone placed, Neutron at home",
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     public,
		},
		{
			name:     "different clusters",
			neutron:  &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-b"},
			want:     public,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := neutronControlPlane()
			cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"}
			cp.Spec.Services.Keystone.PublicEndpoint = public
			cp.Spec.Services.Keystone.TargetClusterRef = tc.keystone
			cp.Spec.Services.Neutron.TargetClusterRef = tc.neutron

			g.Expect(neutronKeystoneEndpoint(cp)).To(Equal(tc.want))
		})
	}
}

// --- the credential mirror of a placed service ---

// newPlacedNeutronReconciler wires a ControlPlane whose network service is placed
// on a target cluster: the CR, its registration and the shared bus live on the
// management cluster, the objects in onTarget on the other one.
func newPlacedNeutronReconciler(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, resolver *childrenResolver, onTarget ...client.Object,
) *ControlPlaneReconciler {
	t.Helper()
	s := neutronTestScheme(t)
	resolver.children = fake.NewClientBuilder().WithScheme(s).WithObjects(onTarget...).Build()
	local := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withNeutronBusSecret(withReadyNeutronRegistration([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &neutronv1alpha1.Neutron{},
			&c5c3v1alpha1.KeystoneService{}).
		Build()
	return &ControlPlaneReconciler{Client: local, Scheme: s, Resolver: resolver}
}

// TestReconcileNeutron_MirrorsRegistrationCredentialsToTheTarget covers the reason
// the mirror exists: the registration delivers its consumer Secret at home, and a
// Neutron running on another cluster reads it there, from an ExternalSecret of the
// same name, over the same OpenBao path.
func TestReconcileNeutron_MirrorsRegistrationCredentialsToTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedNeutronControlPlane("remote-a")
	resolver := &childrenResolver{}
	r := newPlacedNeutronReconciler(t, cp, resolver,
		readyTenantSecretStore(esoTenantStoreName, "neutron", "", ""))

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var mirror esov1.ExternalSecret
	g.Expect(resolver.children.Get(context.Background(), types.NamespacedName{
		Name: "cp-neutron-credentials", Namespace: "neutron",
	}, &mirror)).To(Succeed())
	g.Expect(mirror.Spec.SecretStoreRef.Name).To(Equal(esoTenantStoreName))
	g.Expect(mirror.Spec.SecretStoreRef.Kind).To(Equal(string(commonv1.SecretStoreKindNamespaced)))
	g.Expect(mirror.Spec.Target.Name).To(Equal("cp-neutron-credentials"))
	for _, d := range mirror.Spec.Data {
		g.Expect(d.RemoteRef.Key).To(Equal("openstack/keystone/neutron/cp-neutron/service-accounts/credentials"))
	}
	// No owner reference crosses a cluster boundary, so the labels are the whole of
	// the mirror's identity, and what the teardown sweep selects on.
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	g.Expect(mirror.OwnerReferences).To(BeEmpty())
}

// TestReconcileNeutron_NoMirrorForACoLocatedService is the other half: the
// registration's own delivery already lands in a co-located service's namespace,
// so no second ExternalSecret is written for it.
func TestReconcileNeutron_NoMirrorForACoLocatedService(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)

	_, err := r.reconcileNeutron(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: "cp-neutron-credentials", Namespace: "default",
	}, &esov1.ExternalSecret{})).NotTo(Succeed(),
		"a co-located service must get no mirror at all")
}

// TestReconcileNeutron_MirrorHoldsOnAnUnresolvableCluster covers the cluster that
// does not resolve: the resolver's own text reaches the condition, and nothing is
// projected. The bus delivery reaches that cluster first, so the halt is the
// messaging leg's rather than the mirror's, on the same reason.
func TestReconcileNeutron_MirrorHoldsOnAnUnresolvableCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedNeutronControlPlane("remote-a")
	resolver := &childrenResolver{err: errors.New("cluster not found")}
	r := newPlacedNeutronReconciler(t, cp, resolver)

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	var list neutronv1alpha1.NeutronList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileNeutron_MirrorHoldsOnANotReadyTargetStore covers the store gate on
// the target cluster: an ExternalSecret written against a store that is not ready
// never syncs, so the projection waits and names the store, the namespace, and the
// cluster it is missing on.
func TestReconcileNeutron_MirrorHoldsOnANotReadyTargetStore(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedNeutronControlPlane("remote-a")
	resolver := &childrenResolver{}
	// The store exists at home but not on the target, which is the cluster the
	// mirror is materialized on.
	r := newPlacedNeutronReconciler(t, cp, resolver)

	res, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
	g.Expect(cond.Message).To(ContainSubstring(esoTenantStoreName))
	g.Expect(cond.Message).To(ContainSubstring(`namespace "neutron"`))
	g.Expect(cond.Message).To(ContainSubstring("target cluster"))

	g.Expect(resolver.children.Get(context.Background(), types.NamespacedName{
		Name: "cp-neutron-credentials", Namespace: "neutron",
	}, &esov1.ExternalSecret{})).NotTo(Succeed(), "nothing may be written against a store that is not ready")
}

// TestReconcileNeutron_MirrorStoreLookupFailurePropagates covers the store read
// that fails outright, as opposed to reporting not-ready: it is wrapped with what
// was being checked and returned, so the reconcile retries with backoff.
func TestReconcileNeutron_MirrorStoreLookupFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedNeutronControlPlane("remote-a")
	s := neutronTestScheme(t)
	target := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*esov1.SecretStore); ok {
					return apierrors.NewInternalError(errors.New("the target apiserver is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	local := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withNeutronBusSecret(withReadyNeutronRegistration([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &neutronv1alpha1.Neutron{},
			&c5c3v1alpha1.KeystoneService{}).
		Build()
	r := &ControlPlaneReconciler{Client: local, Scheme: s, Resolver: &childrenResolver{children: target}}

	_, err := r.reconcileNeutron(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`in namespace "neutron"`))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
}
