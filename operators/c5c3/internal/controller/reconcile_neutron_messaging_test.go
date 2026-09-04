// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Neutron messaging sub-reconciler, which resolves the shared bus
// in the ControlPlane's own namespace and delivers it as a brownfield Secret into
// the namespace (and onto the cluster) the network service runs in.
package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

const (
	// neutronMsgBusCluster is the RabbitmqCluster the managed fixtures reference,
	// neutronMsgUserSecret the default-user Secret its status names.
	neutronMsgBusCluster = "openstack-rabbitmq"
	neutronMsgUserSecret = "openstack-rabbitmq-default-user" //nolint:gosec // G101 false positive: Secret name, not a credential.
	// neutronMsgURL is the URL the managed fixtures assemble from
	// neutronMsgDefaultUserSecret's four keys.
	neutronMsgURL = "rabbit://default_user_abc:s3cr3t@openstack-rabbitmq.openstack.svc:5672/"
)

func neutronMessagingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	return s
}

// neutronMessagingControlPlane builds a ControlPlane in the namespace "openstack"
// that declares a MANAGED bus and a co-located network service.
func neutronMessagingControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "openstack",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Messaging: &commonv1.MessagingSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: neutronMsgBusCluster},
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn-central", Namespace: "ovn-system"},
					},
				},
			},
		},
	}
}

// placeNeutron moves the network service into a namespace of its own, and onto a
// target cluster when targetCluster is non-empty.
func placeNeutron(cp *c5c3v1alpha1.ControlPlane, namespace, targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: namespace, Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	if targetCluster != "" {
		cp.Spec.Services.Neutron.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	}
	return cp
}

// neutronMsgRabbitmqCluster builds the unstructured RabbitmqCluster the managed
// flow reads. The kind is addressed unstructured, so it needs no scheme entry.
func neutronMsgRabbitmqCluster() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	u.SetName(neutronMsgBusCluster)
	u.SetNamespace("openstack")
	if err := unstructured.SetNestedMap(u.Object,
		map[string]interface{}{"name": neutronMsgUserSecret}, "status", "defaultUser", "secretReference"); err != nil {
		panic(err)
	}
	return u
}

// neutronMsgDefaultUserSecret is the Secret the RabbitMQ Cluster Operator writes
// the default user's credentials and endpoint into.
func neutronMsgDefaultUserSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: neutronMsgUserSecret, Namespace: "openstack"},
		Data: map[string][]byte{
			"username": []byte("default_user_abc"),
			"password": []byte("s3cr3t"),
			"host":     []byte("openstack-rabbitmq.openstack.svc"),
			"port":     []byte("5672"),
		},
	}
}

// newNeutronMessagingReconciler builds a reconciler on the management cluster
// alone; the tests that place the service hand in a resolver of their own.
func newNeutronMessagingReconciler(t *testing.T, s *runtime.Scheme, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	return &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		Scheme: s,
	}
}

// neutronMessagingCondition returns the NeutronReady condition, failing the test
// when it is absent: every halting arm has to leave one behind.
func neutronMessagingCondition(t *testing.T, cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	t.Helper()
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	if cond == nil {
		t.Fatalf("reconcileNeutronMessaging left no %s condition", conditionTypeNeutronReady)
	}
	return cond
}

// getNeutronMsgSecret reads a Secret through c, returning nil when it is absent.
func getNeutronMsgSecret(t *testing.T, c client.Client, namespace, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil
	}
	return secret
}

// TestReconcileNeutronMessaging_ManagedBusWritesTheSecretWithAnOwnerReference
// covers the co-located default: the URL assembled from the RabbitmqCluster's
// default user lands beside the child under the ControlPlane's own name, owned by
// a controller reference so it is reaped with the plane.
func TestReconcileNeutronMessaging_ManagedBusWritesTheSecretWithAnOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())

	res, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse(), "a delivered bus must let the projection run")
	g.Expect(res).To(Equal(ctrl.Result{}))

	secret := getNeutronMsgSecret(t, r.Client, "openstack", neutronMessagingSecretName(cp))
	g.Expect(secret).NotTo(BeNil(), "the messaging Secret must exist beside the child")
	g.Expect(secret.Name).To(Equal("cp-neutron-messaging"))
	g.Expect(secret.Name).NotTo(Equal(messaging.TransportURLSecretName(neutronName(cp))),
		"the neutron operator claims its own derived name in this namespace")
	g.Expect(string(secret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(neutronMsgURL))
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Name).To(Equal(cp.Name))
	g.Expect(secret.OwnerReferences[0].Controller).NotTo(BeNil())
	g.Expect(*secret.OwnerReferences[0].Controller).To(BeTrue())

	g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)).To(BeNil(),
		"the delivery reports only failures; readiness belongs to the projection")
}

// TestReconcileNeutronMessaging_BrownfieldBusCopiesTheURLVerbatim covers the bus
// an administrator runs outside the plane: the vhost, port and credentials the
// referenced Secret carries have to survive the hand-off untouched.
func TestReconcileNeutronMessaging_BrownfieldBusCopiesTheURLVerbatim(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "external-bus", Key: "transport_url"},
	}
	const external = "rabbit://neutron:pw@broker.example.com:5673/openstack"
	bus := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-bus", Namespace: "openstack"},
		Data:       map[string][]byte{"transport_url": []byte(external)},
	}
	r := newNeutronMessagingReconciler(t, s, cp, bus)

	_, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	secret := getNeutronMsgSecret(t, r.Client, "openstack", neutronMessagingSecretName(cp))
	g.Expect(secret).NotTo(BeNil())
	g.Expect(string(secret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(external))
}

// TestReconcileNeutronMessaging_WaitHaltsOnWaitingForMessagingCredentials covers
// the bus whose default-user Secret the RabbitMQ Cluster Operator has not written
// yet. Nothing may be delivered from half a credential, so the pass halts on the
// shared reason and the Neutron namespace stays empty.
func TestReconcileNeutronMessaging_WaitHaltsOnWaitingForMessagingCredentials(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster())

	res, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "an unwritten default-user Secret is a wait, not a reconcile failure")
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))
	g.Expect(cond.Message).To(ContainSubstring("default-user Secret openstack/" + neutronMsgUserSecret))

	g.Expect(getNeutronMsgSecret(t, r.Client, "openstack", neutronMessagingSecretName(cp))).To(BeNil(),
		"no Secret may be delivered while the credential is incomplete")
}

// TestReconcileNeutronMessaging_NilBusHalts covers the caller-contract guard:
// this pass is only entered for a ControlPlane that declares a bus, so a missing
// block is a programming error rather than a state to wait out. The caller checks
// the same thing today, which is why the guard is unreachable through
// reconcileNeutron — it is called directly here so the arm keeps its verdict if
// that pre-check is ever relaxed or reordered.
func TestReconcileNeutronMessaging_NilBusHalts(t *testing.T) {
	for name, drop := range map[string]func(cp *c5c3v1alpha1.ControlPlane){
		"nil infrastructure": func(cp *c5c3v1alpha1.ControlPlane) { cp.Spec.Infrastructure = nil },
		"nil messaging":      func(cp *c5c3v1alpha1.ControlPlane) { cp.Spec.Infrastructure.Messaging = nil },
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := neutronMessagingScheme(t)
			cp := neutronMessagingControlPlane()
			drop(cp)
			r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())

			res, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

			g.Expect(err).To(MatchError(ContainSubstring("spec.infrastructure.messaging is nil")))
			g.Expect(halt).To(BeTrue(), "nothing may be projected from a bus that was never declared")
			g.Expect(res).To(Equal(ctrl.Result{}), "an unreachable state does not converge on a requeue")

			cond := neutronMessagingCondition(t, cp)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("NeutronMessagingError"))

			g.Expect(getNeutronMsgSecret(t, r.Client, "openstack", neutronMessagingSecretName(cp))).To(BeNil(),
				"no Secret may be written from a bus that was never declared")
		})
	}
}

// TestReconcileNeutronMessaging_ResolveErrorSurfaces covers a bus block that named
// neither a cluster nor a Secret, which only a bypassed admission produces. It is
// not a state that converges on its own, so it surfaces as an error rather than a
// wait.
func TestReconcileNeutronMessaging_ResolveErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{}
	r := newNeutronMessagingReconciler(t, s, cp)

	_, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resolving the shared bus transport URL:"))
	g.Expect(halt).To(BeTrue())

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NeutronMessagingError"))
}

// TestReconcileNeutronMessaging_UnresolvableClusterHaltsOnTargetClusterUnavailable
// covers the network service placed on a cluster that is not registered (yet).
// The Secret belongs on that cluster, so nothing is written anywhere and the pass
// parks on the reason every placed sub-reconciler shares.
func TestReconcileNeutronMessaging_UnresolvableClusterHaltsOnTargetClusterUnavailable(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "remote-a")
	remote := fake.NewClientBuilder().WithScheme(s).Build()
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())
	r.Resolver = &childrenResolver{children: remote, err: mcruntime.ErrClusterNotFound}

	res, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "an unregistered cluster is a state to wait out, not a reconcile failure")
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	for name, c := range map[string]client.Client{"management": r.Client, "target": remote} {
		g.Expect(getNeutronMsgSecret(t, c, "networking", neutronMessagingSecretName(cp))).To(BeNil(),
			"no Secret may be delivered on the %s cluster", name)
	}
}

// TestReconcileNeutronMessaging_DedicatedNamespaceCarriesTheOwnershipLabels covers
// the service in a namespace of its own on the local cluster: Kubernetes forbids a
// cross-namespace controller reference, so the ownership labels are what mark the
// Secret as this plane's child.
func TestReconcileNeutronMessaging_DedicatedNamespaceCarriesTheOwnershipLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "")
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())

	_, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	secret := getNeutronMsgSecret(t, r.Client, "networking", neutronMessagingSecretName(cp))
	g.Expect(secret).NotTo(BeNil())
	g.Expect(secret.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, cp.Name))
	g.Expect(secret.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, cp.Namespace))
	g.Expect(secret.OwnerReferences).To(BeEmpty(),
		"a controller reference across namespaces is rejected by the API server")
}

// TestReconcileNeutronMessaging_TargetClusterCarriesTheOwnerTriple covers the
// placed service: the Secret is written on the cluster the Neutron pods run on,
// with the owner triple the shared teardown selects on, and nothing is left at
// home for a cluster that will never read it.
func TestReconcileNeutronMessaging_TargetClusterCarriesTheOwnerTriple(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "remote-a")
	remote := fake.NewClientBuilder().WithScheme(s).Build()
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())
	r.Resolver = &childrenResolver{children: remote}

	_, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	secret := getNeutronMsgSecret(t, remote, "networking", neutronMessagingSecretName(cp))
	g.Expect(secret).NotTo(BeNil(), "the bus must be delivered on the network service's own cluster")
	g.Expect(string(secret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(neutronMsgURL))
	g.Expect(secret.Labels).To(Equal(remoteChildLabels(cp)))
	g.Expect(secret.OwnerReferences).To(BeEmpty(),
		"an owner reference on the target cluster names a UID that cluster cannot resolve")

	g.Expect(getNeutronMsgSecret(t, r.Client, "networking", neutronMessagingSecretName(cp))).To(BeNil(),
		"nothing may be left behind at home")
}

// TestReconcileNeutronMessaging_RefusesAForeignSameNamedSecret covers the name
// taken by somebody else in a namespace the ControlPlane does not own. Adopting it
// would overwrite its data and get it deleted at teardown, so the write is refused
// and the refusal reaches the condition.
func TestReconcileNeutronMessaging_RefusesAForeignSameNamedSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "")
	// The UID is what refuseForeignAdoption reads as "this already exists".
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutronMessagingSecretName(cp),
			Namespace: "networking",
			UID:       types.UID("foreign-secret-uid"),
		},
		Data: map[string][]byte{"transport_url": []byte("rabbit://someone:else@broker:5672/")},
	}
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), foreign)

	_, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt"))
	g.Expect(halt).To(BeTrue())

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NeutronMessagingError"))
	g.Expect(cond.Message).To(ContainSubstring("refusing to adopt"))

	live := getNeutronMsgSecret(t, r.Client, "networking", neutronMessagingSecretName(cp))
	g.Expect(live).NotTo(BeNil())
	g.Expect(string(live.Data["transport_url"])).To(Equal("rabbit://someone:else@broker:5672/"),
		"the foreign Secret's data must survive untouched")
}

// TestReconcileNeutronMessaging_TLSWritesTheCAMirror covers a bus the consumer
// verifies: the bundle lives beside the bus, the Neutron pods may be somewhere
// else entirely, so it is mirrored into their namespace under the key the
// projected messaging block names.
func TestReconcileNeutronMessaging_TLSWritesTheCAMirror(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	bundle := []byte("-----BEGIN CERTIFICATE-----\nbus\n-----END CERTIFICATE-----\n")
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: "openstack"},
		Data:       map[string][]byte{"ca.crt": bundle},
	}
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), busCA)

	_, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	mirror := getNeutronMsgSecret(t, r.Client, "openstack", neutronMessagingCASecretName(cp))
	g.Expect(mirror).NotTo(BeNil())
	g.Expect(mirror.Name).To(Equal("cp-neutron-messaging-ca"))
	g.Expect(mirror.Data).To(HaveLen(1))
	g.Expect(mirror.Data).To(HaveKeyWithValue(neutronMessagingCAKey, bundle))
}

// TestReconcileNeutronMessaging_TLSHaltsWhenTheCABundleSecretIsMissing covers the
// referenced bundle that has not been created yet. Delivering the URL without the
// trust anchor would leave the pods failing TLS on every connection, so the pass
// halts and names both halves of the reference.
func TestReconcileNeutronMessaging_TLSHaltsWhenTheCABundleSecretIsMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())

	res, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForMessagingCABundle"))
	g.Expect(cond.Message).To(ContainSubstring("openstack/bus-ca"))
	g.Expect(cond.Message).To(ContainSubstring(`"ca.crt"`))

	g.Expect(getNeutronMsgSecret(t, r.Client, "openstack", neutronMessagingCASecretName(cp))).To(BeNil(),
		"no mirror may be written from an absent bundle")
}

// TestReconcileNeutronMessaging_TLSHaltsWhenTheCABundleKeyIsMissing covers the
// Secret that exists but does not carry the key, the ordinary transient of a
// two-step create-then-populate flow. The ref leaves the key empty, so the pass
// also has to fall back on the default the webhooks materialize.
func TestReconcileNeutronMessaging_TLSHaltsWhenTheCABundleKeyIsMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: "openstack"},
		Data:       map[string][]byte{"tls.crt": []byte("not the bundle")},
	}
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), busCA)

	res, halt, err := r.reconcileNeutronMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForMessagingCABundle"))
	g.Expect(cond.Message).To(ContainSubstring("openstack/bus-ca"))
	g.Expect(cond.Message).To(ContainSubstring(`"` + c5c3v1alpha1.DefaultCABundleSecretKey + `"`))

	g.Expect(getNeutronMsgSecret(t, r.Client, "openstack", neutronMessagingCASecretName(cp))).To(BeNil())
}

// TestPruneNeutronMessagingCA_DeletesAStaleCAMirror covers dropping the tls block
// from a bus that had one: the mirror the earlier pass wrote is trust nobody reads
// any more, so it comes down instead of lingering in the Neutron namespace.
//
// The prune is its own entry point, not part of the messaging leg, because the
// live child still names the mirror until the projection several gates later
// rewrites it. reconcileNeutron calls this only on the far side of that apply.
func TestPruneNeutronMessagingCA_DeletesAStaleCAMirror(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "")
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutronMessagingCASecretName(cp),
			Namespace: "networking",
			Labels: map[string]string{
				controlPlaneNameLabel:      cp.Name,
				controlPlaneNamespaceLabel: cp.Namespace,
			},
		},
		Data: map[string][]byte{neutronMessagingCAKey: []byte("stale bundle")},
	}
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), stale)

	_, halt, err := r.pruneNeutronMessagingCA(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	g.Expect(getNeutronMsgSecret(t, r.Client, "networking", neutronMessagingCASecretName(cp))).To(BeNil(),
		"a mirror this ControlPlane wrote must not outlive the tls block")
}

// TestPruneNeutronMessagingCA_LeavesAForeignCAMirror covers the same name held by
// somebody else. The cleanup deletes children, not neighbours, so an unowned
// Secret survives the pass untouched.
func TestPruneNeutronMessagingCA_LeavesAForeignCAMirror(t *testing.T) {
	g := NewGomegaWithT(t)
	s := neutronMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "")
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutronMessagingCASecretName(cp),
			Namespace: "networking",
			UID:       types.UID("foreign-ca-uid"),
		},
		Data: map[string][]byte{neutronMessagingCAKey: []byte("somebody else's bundle")},
	}
	r := newNeutronMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), foreign)

	_, halt, err := r.pruneNeutronMessagingCA(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	live := getNeutronMsgSecret(t, r.Client, "networking", neutronMessagingCASecretName(cp))
	g.Expect(live).NotTo(BeNil(), "a Secret this ControlPlane never wrote must not be deleted")
	g.Expect(string(live.Data[neutronMessagingCAKey])).To(Equal("somebody else's bundle"))
}
