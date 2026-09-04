// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the OVN sub-reconciler, which reads the referenced OVNCentral and
// mirrors it into OVNReady.
package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// ovnTestScheme registers c5c3, client-go and the ovn types. The Neutron child is
// deliberately absent: reconcileOVN reads the OVNCentral and nothing else.
func ovnTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := ovnv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding ovn scheme: %v", err)
	}
	return s
}

// ovnControlPlane builds a ControlPlane whose network service references the
// OVNCentral "ovn-central" in the namespace "ovn-system". reconcileOVN reads no
// condition off the ControlPlane, so no gate has to be seeded.
//
// The network service is PLACED in "ovn-system" with lifecycle External, because
// that is the only shape in which a central outside the ControlPlane's own
// namespace is reachable at all: the reach rule reconcileOVN re-runs as its
// controller-side backstop admits the plane's own namespace and the ones it
// claims through a services.<service>.namespace assignment, and refuses a
// Managed claim because the teardown would take the central with it.
func ovnControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "default",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Services: c5c3v1alpha1.ServicesSpec{
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
						Name:      "ovn-system",
						Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
					},
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{
							Name:      "ovn-central",
							Namespace: "ovn-system",
						},
					},
				},
			},
		},
	}
}

// readyOVNCentral builds the converged central the happy path reads: Ready=True,
// both databases published on both address forms, and a client Secret.
func readyOVNCentral(name, namespace string) *ovnv1alpha1.OVNCentral {
	central := &ovnv1alpha1.OVNCentral{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: ovnv1alpha1.OVNCentralStatus{
			Northbound: ovnv1alpha1.OVNDatabaseStatus{
				InternalDbAddress: "ssl:10.0.0.1:6641",
				DbAddress:         "ssl:192.168.1.10:30641",
			},
			Southbound: ovnv1alpha1.OVNDatabaseStatus{
				InternalDbAddress: "ssl:10.0.0.2:6642",
				DbAddress:         "ssl:192.168.1.10:30651",
			},
			ClientSecretName: "ovn-central-client",
		},
	}
	conditions.SetCondition(&central.Status.Conditions, metav1.Condition{
		Type:    conditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "OVNCentralReady",
		Message: "the control plane serves",
	})
	return central
}

func newOVNTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := ovnTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &ovnv1alpha1.OVNCentral{}).Build()
	return &ControlPlaneReconciler{Client: c, Scheme: s}
}

// newOVNTestReconcilerWithGetError returns a reconciler whose every Get fails with
// getErr, the only way to drive the two read-failure arms through a fake client.
func newOVNTestReconcilerWithGetError(t *testing.T, getErr error, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := ovnTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return getErr
			},
		}).Build()
	return &ControlPlaneReconciler{Client: c, Scheme: s}
}

// ovnCondition returns the OVNReady condition, failing the test when it is absent:
// every arm of reconcileOVN must leave one behind.
func ovnCondition(t *testing.T, cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	t.Helper()
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeOVNReady)
	if cond == nil {
		t.Fatalf("reconcileOVN left no %s condition", conditionTypeOVNReady)
	}
	return cond
}

func TestReconcileOVN_NotManagedWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	cp.Spec.Services.Neutron = nil
	r := newOVNTestReconciler(t, cp)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a plane without a network service must not requeue on OVN")
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("OVNNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring("spec.services.neutron is unset"))
}

// TestReconcileOVN_RefusesACentralOutsideThePlanesReach covers the CR that never
// passed admission — an unregistered webhook during install, a GitOps or etcd
// restore replaying stored objects. The reach rule is a trust boundary, not a
// spelling check: reading a foreign plane's central relays its database addresses
// and status into this plane's condition, and the Neutron projection gated on
// OVNReady would go on to hand the child a pointer whose operator mirrors that
// central's client Secret — a full mTLS identity for its Northbound and Southbound
// databases — into this plane's namespace. So the read must not happen at all.
func TestReconcileOVN_RefusesACentralOutsideThePlanesReach(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lifecycle c5c3v1alpha1.ServiceNamespaceLifecycle
		claim     string
		message   string
	}{
		{
			name:    "namespace this plane neither owns nor claims",
			claim:   "",
			message: `namespace "ovn-system" is neither this ControlPlane's own nor one it claims`,
		},
		{
			// A Managed claim is deleted with the plane, so the cascade would take
			// the referenced central and the logical network model with it.
			name:      "namespace claimed with lifecycle Managed",
			claim:     "ovn-system",
			lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			message:   `namespace "ovn-system" is claimed by this ControlPlane with lifecycle Managed`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := ovnControlPlane()
			cp.Spec.Services.Neutron.Namespace = nil
			if tc.claim != "" {
				cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
					Name: tc.claim, Lifecycle: tc.lifecycle,
				}
			}
			// The central exists and is fully converged: only the reach check stands
			// between this ControlPlane and reading it.
			r := newOVNTestReconciler(t, cp, readyOVNCentral("ovn-central", "ovn-system"))

			res, err := r.reconcileOVN(context.Background(), cp)

			g.Expect(err).NotTo(HaveOccurred(), "a spec fault is a wait, not a reconcile failure")
			g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			cond := ovnCondition(t, cp)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("OVNCentralNamespaceForbidden"))
			g.Expect(cond.Message).To(ContainSubstring("spec.services.neutron.ovn.centralRef.namespace"))
			g.Expect(cond.Message).To(ContainSubstring(tc.message))
			g.Expect(cond.Message).NotTo(ContainSubstring("ssl:10.0.0.1:6641"),
				"a refused central's database addresses must never reach this plane's status")
		})
	}
}

// TestReconcileOVN_CentralNotFound covers the typo case: the ref names a central
// that was never deployed. The message has to carry the full key an operator can
// go look for, and the pass has to keep waiting rather than error out.
func TestReconcileOVN_CentralNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	r := newOVNTestReconciler(t, cp)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "a missing central is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("OVNCentralNotFound"))
	g.Expect(cond.Message).To(ContainSubstring("ovn-system/ovn-central"))
	g.Expect(cond.Message).To(ContainSubstring("spec.services.neutron.ovn.centralRef"))
}

// TestReconcileOVN_CRDNotServedIsNotAnError covers a ControlPlane declaring a
// network service on a cluster where the ovn-operator was never installed. The
// no-match must read as a wait with an install hint: retrying it through manager
// backoff would only crash-loop the condition with an opaque RESTMapper error.
func TestReconcileOVN_CRDNotServedIsNotAnError(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	noMatch := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "ovn.openstack.c5c3.io", Kind: "OVNCentral"},
		SearchedVersions: []string{"v1alpha1"},
	}
	r := newOVNTestReconcilerWithGetError(t, noMatch, cp)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "an absent CRD must not be retried through manager backoff")
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("OVNCentralReadError"))
	g.Expect(cond.Message).To(Equal("the OVNCentral CRD is not served on this cluster; install the ovn-operator"))
}

// TestReconcileOVN_ReadErrorSurfaces covers an apiserver failure that is neither a
// NotFound nor a no-match: it must reach the manager as an error so the read is
// retried with backoff, and it must still leave a condition behind.
func TestReconcileOVN_ReadErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	sentinel := errors.New("etcd leader changed")
	r := newOVNTestReconcilerWithGetError(t, sentinel, cp)

	_, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, sentinel)).To(BeTrue(), "the upstream error must be wrapped with %%w")
	g.Expect(err.Error()).To(HavePrefix("reading OVNCentral ovn-system/ovn-central: "))
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("OVNCentralReadError"))
	g.Expect(cond.Message).To(Equal(sentinel.Error()))
}

// TestReconcileOVN_NotExternallyReachableAcrossClusters covers the placement the
// rule exists for: the Neutron pods run on another cluster than the central, so
// they leave their own cluster network and can only reach databases published on
// the node network. One unpublished database is enough to block the projection.
func TestReconcileOVN_NotExternallyReachableAcrossClusters(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		northbound, southbound bool
	}{
		{name: "neither database published"},
		{name: "only the southbound database unpublished", northbound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := ovnControlPlane()
			cp.Spec.Services.Neutron.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
			central := readyOVNCentral("ovn-central", "ovn-system")
			central.Spec.Northbound.ExternallyReachable = tc.northbound
			central.Spec.Southbound.ExternallyReachable = tc.southbound
			r := newOVNTestReconciler(t, cp, central)

			res, err := r.reconcileOVN(context.Background(), cp)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			cond := ovnCondition(t, cp)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("OVNCentralNotExternallyReachable"))
			g.Expect(cond.Message).To(ContainSubstring("spec.northbound.externallyReachable"))
			g.Expect(cond.Message).To(ContainSubstring("spec.southbound.externallyReachable"))
			g.Expect(cond.Message).To(ContainSubstring("another cluster"))
		})
	}
}

// TestReconcileOVN_ResolvesEmptyRefNamespaceToTheControlPlaneNamespace pins where
// the read looks when the ref carries no namespace. The defaulting webhook fills
// it, but a CR written before that webhook admitted it (or through a bypassed
// apiserver) reaches the reconciler blank, and the co-located central must still
// be found rather than reported missing in the empty namespace "".
func TestReconcileOVN_ResolvesEmptyRefNamespaceToTheControlPlaneNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = ""
	central := readyOVNCentral("ovn-central", cp.Namespace)
	r := newOVNTestReconciler(t, cp, central)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("OVNCentralReady"))
	g.Expect(cond.Message).To(ContainSubstring("default/ovn-central"))
}

// TestReconcileOVN_WaitingForOVNCentralWithoutConditions covers a central the
// ovn-operator has only just admitted: it exists, but its controller has written
// no status at all yet.
func TestReconcileOVN_WaitingForOVNCentralWithoutConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	central := &ovnv1alpha1.OVNCentral{
		ObjectMeta: metav1.ObjectMeta{Name: "ovn-central", Namespace: "ovn-system"},
	}
	r := newOVNTestReconciler(t, cp, central)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForOVNCentral"))
	g.Expect(cond.Message).To(Equal("OVNCentral ovn-system/ovn-central has not reported Ready yet"))
}

// TestReconcileOVN_WaitingForOVNCentralRelaysReason proves the central's own
// diagnosis reaches the ControlPlane verbatim. "OVNCentral is not ready" says
// nothing an operator can act on; the central's reason and message do.
func TestReconcileOVN_WaitingForOVNCentralRelaysReason(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	central := readyOVNCentral("ovn-central", "ovn-system")
	conditions.SetCondition(&central.Status.Conditions, metav1.Condition{
		Type:    conditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  "RaftClusterUnhealthy",
		Message: "the northbound cluster has no leader",
	})
	r := newOVNTestReconciler(t, cp, central)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForOVNCentral"))
	g.Expect(cond.Message).To(Equal(
		"OVNCentral ovn-system/ovn-central: Ready=False/RaftClusterUnhealthy: the northbound cluster has no leader"))
}

// TestReconcileOVN_EndpointsPendingOnEmptyAddress covers the window a Ready
// central passes through while its Services are still being published: readiness
// alone is not enough, the addresses the Neutron projection reads have to be there
// too, and the message must name the field that is empty.
func TestReconcileOVN_EndpointsPendingOnEmptyAddress(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	central := readyOVNCentral("ovn-central", "ovn-system")
	central.Status.Southbound.InternalDbAddress = ""
	r := newOVNTestReconciler(t, cp, central)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("OVNEndpointsPending"))
	g.Expect(cond.Message).To(ContainSubstring("status.southbound.internalDbAddress"))
	g.Expect(cond.Message).NotTo(ContainSubstring("status.northbound"),
		"only the field that is actually empty may be named")
}

// TestReconcileOVN_EndpointsPendingOnEmptyClientSecret covers the same window from
// the other side: both databases publish, but the client certificate every OVN
// client authenticates with has not been minted yet.
func TestReconcileOVN_EndpointsPendingOnEmptyClientSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	central := readyOVNCentral("ovn-central", "ovn-system")
	central.Status.ClientSecretName = ""
	r := newOVNTestReconciler(t, cp, central)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("OVNEndpointsPending"))
	g.Expect(cond.Message).To(ContainSubstring("status.clientSecretName"))
}

// TestReconcileOVN_ReadySelectsInternalAddressOnSameCluster pins the address form
// a co-located network service gets: the in-cluster one. The node-network form is
// reachable from a host, not from a pod that resolves through cluster DNS.
func TestReconcileOVN_ReadySelectsInternalAddressOnSameCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	central := readyOVNCentral("ovn-central", "ovn-system")
	r := newOVNTestReconciler(t, cp, central)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("OVNCentralReady"))
	g.Expect(cond.Message).To(Equal(
		"OVNCentral ovn-system/ovn-central is ready: Northbound ssl:10.0.0.1:6641, Southbound ssl:10.0.0.2:6642"))
}

// TestReconcileOVN_ReadySelectsDbAddressAcrossClusters is the counterpart: with
// the network service on another cluster and both databases published on the node
// network, the externally routable addresses are the ones that get mirrored.
func TestReconcileOVN_ReadySelectsDbAddressAcrossClusters(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	cp.Spec.Services.Neutron.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
	central := readyOVNCentral("ovn-central", "ovn-system")
	central.Spec.Northbound.ExternallyReachable = true
	central.Spec.Southbound.ExternallyReachable = true
	r := newOVNTestReconciler(t, cp, central)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := ovnCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Message).To(ContainSubstring("Northbound ssl:192.168.1.10:30641"))
	g.Expect(cond.Message).To(ContainSubstring("Southbound ssl:192.168.1.10:30651"))
}

// TestReconcileOVN_EndpointsPendingUsesTheSelectedAddressField proves the pending
// message names the field the cross-cluster read actually looked at. A central
// that publishes only in-cluster addresses is unusable from another cluster, and
// pointing the operator at status.northbound.internalDbAddress there would send
// them to a field that is populated.
func TestReconcileOVN_EndpointsPendingUsesTheSelectedAddressField(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ovnControlPlane()
	cp.Spec.Services.Neutron.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
	central := readyOVNCentral("ovn-central", "ovn-system")
	central.Spec.Northbound.ExternallyReachable = true
	central.Spec.Southbound.ExternallyReachable = true
	central.Status.Northbound.DbAddress = ""
	central.Status.Southbound.DbAddress = ""
	r := newOVNTestReconciler(t, cp, central)

	res, err := r.reconcileOVN(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := ovnCondition(t, cp)
	g.Expect(cond.Reason).To(Equal("OVNEndpointsPending"))
	g.Expect(cond.Message).To(ContainSubstring("status.northbound.dbAddress, status.southbound.dbAddress"))
	g.Expect(cond.Message).NotTo(ContainSubstring("internalDbAddress"))
}
