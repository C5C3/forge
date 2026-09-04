// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the ControlPlane ORC-teardown finalizer (reconcileDelete): the
// ControlPlane CR is held in etcd until the operator-owned K-ORC CRs are gone,
// with a bounded stall escape that force-removes their finalizers and releases
// the ControlPlane anyway.
package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// drainEvents returns every event currently buffered on the FakeRecorder. Each
// entry is "<type> <reason> <message>".
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		if len(rec.Events) > 0 {
			out = append(out, <-rec.Events)
		} else {
			return out
		}
	}
}

// ownedByCP hand-builds a controller OwnerReference to cp so metav1.IsControlledBy
// recognises a seeded child as swept by the prune sweep.
func ownedByCP(cp *c5c3v1alpha1.ControlPlane) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion:         c5c3v1alpha1.GroupVersion.String(),
		Kind:               "ControlPlane",
		Name:               cp.Name,
		UID:                cp.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
}

// deletingControlPlane returns a ControlPlane being deleted (DeletionTimestamp
// set deletionAge in the past) carrying the ORC-teardown finalizer, so
// reconcileDelete drives its teardown.
func deletingControlPlane(deletionAge time.Duration) *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	ts := metav1.NewTime(metav1.Now().Add(-deletionAge))
	cp.DeletionTimestamp = &ts
	cp.Finalizers = []string{controlPlaneORCFinalizer}
	return cp
}

// TestReconcile_AddsORCFinalizerOnFirstReconcile asserts that a fresh
// (non-deleting) ControlPlane gets the ORC-teardown finalizer installed and the
// reconcile requeues before any sub-reconciler runs.
func TestReconcile_AddsORCFinalizerOnFirstReconcile(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"first reconcile must requeue after installing the finalizer")

	got := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, got)).To(Succeed())
	g.Expect(controllerutil.ContainsFinalizer(got, controlPlaneORCFinalizer)).To(BeTrue(),
		"the ORC-teardown finalizer must be installed")
}

// placingControlPlane returns a fresh (not deleting) ControlPlane that places its
// Keystone in a namespace of its own on the named target cluster, which is what
// makes it a candidate for the remote-children finalizer.
func placingControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: placedTeardownNamespace, Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
		TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: targetCluster},
	}
	return cp
}

// TestReconcile_RemoteChildrenFinalizerInstall pins the gate on the finalizer that
// holds a ControlPlane in etcd until the namespaces it placed on a target cluster
// are swept: it goes on a ControlPlane that places a service on a cluster that
// resolves, and on no other. A cluster that does not resolve is not an error here
// — nothing has been written to it that the finalizer would have to reclaim, and
// reconcileNamespaces reports the failure — so the install is retried on a later
// pass instead.
func TestReconcile_RemoteChildrenFinalizerInstall(t *testing.T) {
	// reconcileTwice runs the passes that install the ORC finalizer and, when the
	// gate admits it, the remote-children one, and returns the persisted CR.
	reconcileTwice := func(g *WithT, r *ControlPlaneReconciler, c client.Client, cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.ControlPlane {
		key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
		for range 2 {
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred(), "installing a finalizer must not fail the pass")
		}
		got := &c5c3v1alpha1.ControlPlane{}
		g.Expect(c.Get(context.Background(), key, got)).To(Succeed())
		return got
	}

	t.Run("installs it for a placed ControlPlane whose cluster resolves", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := placingControlPlane(placedTeardownCluster)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{children: fake.NewClientBuilder().WithScheme(s).Build()},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"a ControlPlane that places a service on a resolvable cluster must carry the remote-children finalizer")
	})

	t.Run("never installs it for a ControlPlane that places nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := korcControlPlane()
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{children: fake.NewClientBuilder().WithScheme(s).Build()},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeFalse(),
			"a ControlPlane whose children all stay at home has nothing for the finalizer to hold")
	})

	t.Run("skips the install while the cluster does not resolve", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := placingControlPlane(placedTeardownCluster)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeFalse(),
			"an unwritten cluster leaves nothing to reclaim, so the install waits")
		cond := conditions.GetCondition(got.Status.Conditions, conditionTypeNamespacesReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable),
			"the unresolvable cluster is reported by the namespace step, not by the finalizer install")
	})

	// ANY, not ALL. reconcileNamespaces resolves and writes per namespace inside
	// its loop, so the resolvable cluster's namespaces are created on this very
	// pass whatever the sibling ref does. Demanding every cluster would leave that
	// written half without a finalizer, and the ORC stall escape — which releases
	// the ORC finalizer expecting this one to hold the CR open — would then let the
	// CR leave etcd with those namespaces standing.
	t.Run("installs it when only one of two clusters resolves", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := placingControlPlane(placedTeardownCluster)
		cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name: "dashboard", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
			TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "deregistered"},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{
				children: fake.NewClientBuilder().WithScheme(s).Build(),
				errNames: map[string]error{"deregistered": mcruntime.ErrClusterNotFound},
			},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"the namespaces written to the resolvable cluster need the finalizer that reclaims them")
	})

	t.Run("keeps it once installed, even when the cluster stops resolving", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := placingControlPlane(placedTeardownCluster)
		cp.Finalizers = []string{controlPlaneORCFinalizer, commonmulticluster.RemoteChildrenFinalizer}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"children already on a cluster that dropped out still have to be swept or abandoned")
	})
}

// TestReconcileDelete_NoFinalizer_NoOp asserts reconcileDelete is a no-op when
// the ControlPlane does not carry the ORC-teardown finalizer: it must not touch
// any K-ORC CR.
func TestReconcileDelete_NoFinalizer_NoOp(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:       adminAppCredentialName(cp),
			Namespace:  childNamespace(cp),
			Finalizers: []string{"openstack.k-orc.cloud/applicationcredential"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ac).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	// A deleting ControlPlane that carries only a foreign finalizer.
	del := metav1.Now()
	cp.DeletionTimestamp = &del
	cp.Finalizers = []string{"example.com/other"}

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "no-op delete must return a zero result")

	got := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ac), got)).To(Succeed())
	g.Expect(got.DeletionTimestamp.IsZero()).To(BeTrue(),
		"reconcileDelete must not delete K-ORC CRs when its finalizer is absent")
	g.Expect(drainEvents(rec)).To(BeEmpty(), "no-op delete must not emit events")
}

// TestReconcileDelete_NoORCResources_ReleasesFinalizer asserts that when no
// K-ORC CRs remain, the ControlPlane finalizer is released in one pass (and the
// CR is then garbage-collected).
func TestReconcileDelete_NoORCResources_ReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingControlPlane(0)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	// Refresh from the client so the Update carries the right resourceVersion.
	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "release must return a zero result")

	err = c.Get(context.Background(), key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"releasing the last finalizer must let the ControlPlane be garbage-collected")
	g.Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("ORCTeardownComplete")))
}

// TestReconcileDelete_WaitsWhileORCTerminating asserts that while an owned K-ORC
// CR is still present, reconcileDelete holds the ControlPlane finalizer, reports
// KORCReady=False/FinalizingORC, and requeues. Deleting the live CR marks it
// Terminating (it carries a K-ORC finalizer) and emits FinalizingORC once.
func TestReconcileDelete_WaitsWhileORCTerminating(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingControlPlane(0)
	// A live AC (no DeletionTimestamp) carrying a K-ORC finalizer: deleting it
	// transitions it to Terminating rather than removing it outright.
	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:       adminAppCredentialName(cp),
			Namespace:  childNamespace(cp),
			Finalizers: []string{"openstack.k-orc.cloud/applicationcredential"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ac).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"a still-Terminating K-ORC CR must requeue at the K-ORC cadence")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("FinalizingORC"))

	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue(),
		"the ControlPlane finalizer must be held while K-ORC CRs remain")

	gotAC := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ac), gotAC)).To(Succeed())
	g.Expect(gotAC.DeletionTimestamp.IsZero()).To(BeFalse(),
		"the owned K-ORC CR must have been marked for deletion")

	g.Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("FinalizingORC")))
}

// TestReconcileDelete_ForceRemovesORCFinalizersAfterStall asserts the stall
// escape: once the ControlPlane has been Terminating past orcTeardownStallTimeout
// with K-ORC CRs still stuck, reconcileDelete strips their K-ORC finalizers
// (preserving non-K-ORC finalizers), emits a Warning, and releases the
// ControlPlane finalizer.
func TestReconcileDelete_ForceRemovesORCFinalizersAfterStall(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingControlPlane(2 * orcTeardownStallTimeout)
	// An AC stuck Terminating behind a K-ORC finalizer AND a foreign finalizer
	// that must survive the force-remove.
	acDeletion := metav1.NewTime(metav1.Now().Add(-2 * orcTeardownStallTimeout))
	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:              adminAppCredentialName(cp),
			Namespace:         childNamespace(cp),
			Finalizers:        []string{"openstack.k-orc.cloud/applicationcredential", "example.com/keep"},
			DeletionTimestamp: &acDeletion,
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ac).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "the stall escape must release without requeue")

	// The K-ORC finalizer is stripped; the foreign one survives.
	gotAC := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ac), gotAC)).To(Succeed())
	g.Expect(gotAC.Finalizers).To(Equal([]string{"example.com/keep"}),
		"only the openstack.k-orc.cloud/* finalizer must be force-removed")

	// The ControlPlane finalizer is released, so the CR is garbage-collected.
	err = c.Get(context.Background(), key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the ControlPlane finalizer must be released after the stall escape")

	events := drainEvents(rec)
	g.Expect(events).To(ContainElement(SatisfyAll(
		ContainSubstring("Warning"),
		ContainSubstring("ORCTeardownStalled"),
		// The window the gate actually applies, not the K-ORC share of it: triage
		// reads this number off the event and computes deletionTimestamp + it, so a
		// stale one sends the operator hunting a controller stall that never happened.
		ContainSubstring(orcTeardownDeadline.String()),
	)), "the stall escape must emit a Warning ORCTeardownStalled event naming its own deadline")
}

// deletingExternalControlPlane returns an External-mode ControlPlane being
// deleted, carrying the ORC-teardown finalizer.
func deletingExternalControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := korcExternalControlPlane()
	ts := metav1.NewTime(metav1.Now().Add(-time.Second))
	cp.DeletionTimestamp = &ts
	cp.Finalizers = []string{controlPlaneORCFinalizer}
	return cp
}

// terminatingImportMeta builds the ObjectMeta of a Terminating K-ORC CR: a
// K-ORC finalizer holds it and its DeletionTimestamp is set — the state the
// identity imports sit in once the teardown has revoked the application
// credential their finalizers would authenticate with.
func terminatingImportMeta(name, ns, finalizer string) metav1.ObjectMeta {
	ts := metav1.NewTime(metav1.Now().Add(-30 * time.Second))
	return metav1.ObjectMeta{
		Name:              name,
		Namespace:         ns,
		Finalizers:        []string{finalizer},
		DeletionTimestamp: &ts,
	}
}

// TestReconcileDelete_ReleasesUnmanagedImportsWithoutStall is the regression
// guard for the teardown wedge the external-keystone suite exposed: after the
// managed children (application credential included) are gone, the only CRs
// left are Unmanaged imports whose K-ORC finalizers can never run again — the
// revoked credential is the one they authenticate with. reconcileDelete must
// release them immediately (Normal event, finalizer held, no Warning), NOT
// wait out the five-minute stall window and alarm with ORCTeardownStalled.
func TestReconcileDelete_ReleasesUnmanagedImportsWithoutStall(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane() // deleted 1s ago — well inside the stall window
	ns := childNamespace(cp)
	svc := &orcv1alpha1.Service{
		ObjectMeta: terminatingImportMeta(keystoneServiceName(cp), ns, "openstack.k-orc.cloud/service"),
		Spec:       orcv1alpha1.ServiceSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	domain := &orcv1alpha1.Domain{
		ObjectMeta: terminatingImportMeta(adminDomainRef(cp), ns, "openstack.k-orc.cloud/domain"),
		Spec:       orcv1alpha1.DomainSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, svc, domain).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"the release pass must requeue to confirm the imports are gone")

	// Stripping the only (K-ORC) finalizer completes the deletions.
	err = c.Get(context.Background(), client.ObjectKeyFromObject(svc), &orcv1alpha1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the released Service import must be gone")
	err = c.Get(context.Background(), client.ObjectKeyFromObject(domain), &orcv1alpha1.Domain{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the released Domain import must be gone")

	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue(),
		"the ControlPlane finalizer is held until the follow-up pass confirms emptiness")
	events := drainEvents(rec)
	g.Expect(events).To(ContainElement(ContainSubstring("ORCImportsReleased")))
	g.Expect(events).NotTo(ContainElement(ContainSubstring("Warning")),
		"releasing unmanaged imports orphans nothing and must not alarm")

	// The follow-up pass finds nothing remaining and releases the ControlPlane.
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())
	res, err = r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	err = c.Get(context.Background(), key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("ORCTeardownComplete")))
}

// TestReconcileDelete_WaitsForOwnedPushSecretCleanup is the regression guard
// for the OpenBao-orphan race the external-keystone suite exposed: the owned
// PushSecrets carry DeletionPolicy=Delete, and ESO can only delete the
// mirrored OpenBao data while the per-tenant store and its ServiceAccount are
// alive — both die in the GC cascade the moment the ControlPlane finalizer is
// released. reconcileDelete must therefore delete the PushSecrets itself and
// hold the finalizer until they are gone.
func TestReconcileDelete_WaitsForOwnedPushSecretCleanup(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	ns := childNamespace(cp)
	// An owned PushSecret still live (not yet Terminating), held by ESO's
	// finalizer once deleted — the state right after the CP delete lands.
	ps := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            adminAppCredentialPushSecretName(cp),
			Namespace:       ns,
			OwnerReferences: ownedByCP(cp),
			Finalizers:      []string{"pushsecret.externalsecrets.io/finalizer"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ps).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"the teardown must wait for ESO to finish the OpenBao cleanup")

	gotPS := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ps), gotPS)).To(Succeed())
	g.Expect(gotPS.DeletionTimestamp.IsZero()).To(BeFalse(),
		"the owned PushSecret must have been deleted by the teardown, not left to GC")
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue(),
		"the ControlPlane finalizer must be held while the PushSecret cleanup runs")
	g.Expect(drainEvents(rec)).NotTo(ContainElement(ContainSubstring("ORCTeardownComplete")))

	// ESO finishes: the remote data is deleted and the finalizer released.
	gotPS.Finalizers = nil
	g.Expect(c.Update(context.Background(), gotPS)).To(Succeed())

	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())
	res, err = r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	err = c.Get(context.Background(), key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("ORCTeardownComplete")))
}

// TestDeleteOwnedPushSecrets_SweepsLabelOwnedInDedicatedNamespace pins the widened
// teardown: a service-account credential PushSecret delivered into a dedicated
// service namespace carries the ownership LABELS, not a controller reference, and
// must still get its DeletionPolicy=Delete OpenBao purge while that namespace's
// tenant store is alive. deleteOwnedPushSecrets therefore sweeps every namespace
// the ControlPlane occupies and matches isControlPlaneChild — while a same-named
// foreign PushSecret in that shared namespace is left alone.
func TestDeleteOwnedPushSecrets_SweepsLabelOwnedInDedicatedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)

	cp := deletingNamespacedControlPlane(time.Second) // places Keystone in "identity"
	homeNS := childNamespace(cp)

	// A label-owned PushSecret in the dedicated namespace, held by ESO's finalizer.
	delivered := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cp-service-account-nova-backup", Namespace: "identity",
			Labels:     controlPlaneChildLabels(cp),
			Finalizers: []string{"pushsecret.externalsecrets.io/finalizer"},
		},
	}
	// A finalizer-less owned PushSecret at home — gone with the Delete.
	home := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name: adminAppCredentialPushSecretName(cp), Namespace: homeNS, OwnerReferences: ownedByCP(cp),
		},
	}
	// Somebody else's PushSecret of the same name in the shared namespace.
	foreign := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-service-account-nova-backup", Namespace: "dashboard"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, delivered, home, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	remaining, err := r.deleteOwnedPushSecrets(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The delivery-namespace PushSecret was Deleted and, held by its finalizer, is
	// still present — so it is reported as remaining and gates the finalizer release.
	gotDelivered := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(delivered), gotDelivered)).To(Succeed())
	g.Expect(gotDelivered.DeletionTimestamp.IsZero()).To(BeFalse(),
		"the label-owned delivery PushSecret must be deleted, not left to a GC cascade that never reaches it")
	names := make([]string, 0, len(remaining))
	for _, pending := range remaining {
		names = append(names, pending.pushSecret.Namespace+"/"+pending.pushSecret.Name)
	}
	g.Expect(names).To(ContainElement("identity/cp-service-account-nova-backup"))

	// The finalizer-less owned PushSecret at home is gone.
	err = c.Get(context.Background(), client.ObjectKeyFromObject(home), &esov1alpha1.PushSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the owned home PushSecret must be swept too")

	// The foreign PushSecret is untouched.
	gotForeign := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), gotForeign)).To(Succeed())
	g.Expect(gotForeign.DeletionTimestamp.IsZero()).To(BeTrue(),
		"a same-named PushSecret we do not own must be left alone")
}

// TestReconcileDelete_DeletesTheProjectedRegistrationsFirst pins the ordering the
// registrations depend on: their K-ORC CRs are owned by the KeystoneService, not
// by this ControlPlane, so no enumeration here names them and their controller
// drives them through the admin credential this teardown revokes. The teardown
// therefore deletes the registrations BEFORE it touches the admin
// ApplicationCredential, and holds its finalizer while one is still Terminating.
func TestReconcileDelete_DeletesTheProjectedRegistrationsFirst(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingControlPlane(0)
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}

	// The projected registration, owner-referenced because it is co-located, and
	// carrying the finalizer its own controller releases once its children are gone.
	registration := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       glanceName(cp),
			Namespace:  cp.Namespace,
			Finalizers: []string{keystoneServiceFinalizerName},
		},
	}
	g.Expect(controllerutil.SetControllerReference(cp, registration, s)).To(Succeed())
	// A live admin ApplicationCredential: if the pass reached the K-ORC sweep it
	// would be marked for deletion, which is what must NOT happen yet.
	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:       adminAppCredentialName(cp),
			Namespace:  childNamespace(cp),
			Finalizers: []string{"openstack.k-orc.cloud/applicationcredential"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, registration, ac).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(ctx, key, cp)).To(Succeed())

	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"a registration still finishing its own teardown must hold the pass")

	gotRegistration := &c5c3v1alpha1.KeystoneService{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(registration), gotRegistration)).To(Succeed())
	g.Expect(gotRegistration.DeletionTimestamp.IsZero()).To(BeFalse(),
		"the projected registration must be deleted by the teardown, not left to the post-release GC cascade")

	gotAC := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(ac), gotAC)).To(Succeed())
	g.Expect(gotAC.DeletionTimestamp.IsZero()).To(BeTrue(),
		"the admin credential must survive until the registrations are gone; revoking it first "+
			"leaves their K-ORC CRs unable to complete")

	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("FinalizingServiceRegistrations"))
}

// TestReconcileDelete_RegistrationStallLeavesTheKORCSweepItsOwnWindow guards the
// budget split. The registration wait runs BEFORE the K-ORC sweep and blocks it,
// so a single deadline measured from the deletion timestamp lets an unreachable
// Keystone spend the whole window on the registrations and drop the sweep
// straight into its force-remove escape — the admin ApplicationCredential would
// be orphaned in Keystone, still granting admin scope on the cloud, without one
// revocation attempt. That is the scenario both stalls exist for, so the two
// windows are separate: registrationTeardownStallTimeout, then the K-ORC one on
// top (orcTeardownDeadline).
func TestReconcileDelete_RegistrationStallLeavesTheKORCSweepItsOwnWindow(t *testing.T) {
	// A registration wedged behind the finalizer its own controller cannot
	// release, plus the live admin credential the sweep behind it has to revoke.
	setup := func(t *testing.T, deletionAge time.Duration) (
		*ControlPlaneReconciler, *c5c3v1alpha1.ControlPlane, *record.FakeRecorder, client.Client,
	) {
		t.Helper()
		s := korcTestScheme(t)
		cp := deletingControlPlane(deletionAge)
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
		wedged := &c5c3v1alpha1.KeystoneService{
			ObjectMeta: metav1.ObjectMeta{
				Name:       glanceName(cp),
				Namespace:  cp.Namespace,
				Labels:     controlPlaneChildLabels(cp),
				Finalizers: []string{keystoneServiceFinalizerName},
			},
		}
		ac := &orcv1alpha1.ApplicationCredential{
			ObjectMeta: metav1.ObjectMeta{
				Name:       adminAppCredentialName(cp),
				Namespace:  childNamespace(cp),
				Finalizers: []string{"openstack.k-orc.cloud/applicationcredential"},
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, wedged, ac).Build()
		rec := record.NewFakeRecorder(10)
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}
		if err := c.Get(context.Background(), types.NamespacedName{
			Name: cp.Name, Namespace: cp.Namespace,
		}, cp); err != nil {
			t.Fatalf("reading the ControlPlane back: %v", err)
		}
		return r, cp, rec, c
	}
	acKey := func(cp *c5c3v1alpha1.ControlPlane) types.NamespacedName {
		return types.NamespacedName{Name: adminAppCredentialName(cp), Namespace: childNamespace(cp)}
	}

	// Past the registration window the teardown gives up on the registration and
	// the K-ORC sweep begins — with its own window still ahead of it.
	t.Run("the K-ORC sweep starts once the registration window is spent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		r, cp, rec, c := setup(t, registrationTeardownStallTimeout+time.Minute)

		res, err := r.reconcileDelete(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

		events := strings.Join(drainEvents(rec), "\n")
		g.Expect(events).To(ContainSubstring("ServiceRegistrationTeardownStalled"))

		ac := &orcv1alpha1.ApplicationCredential{}
		g.Expect(c.Get(ctx, acKey(cp), ac)).To(Succeed())
		g.Expect(ac.DeletionTimestamp.IsZero()).To(BeFalse(),
			"the sweep must issue the revoking Delete once the registrations are given up on")

		cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Reason).To(Equal("FinalizingORC"))
	})

	// And it keeps that window: a pass past the K-ORC budget of the OLD shared
	// deadline must still be waiting on the revoke, not force-removing it.
	t.Run("the K-ORC finalizer survives the old shared deadline", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		r, cp, rec, c := setup(t, orcTeardownStallTimeout+time.Minute)

		res, err := r.reconcileDelete(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

		events := strings.Join(drainEvents(rec), "\n")
		g.Expect(events).NotTo(ContainSubstring("ORCTeardownStalled"))
		g.Expect(events).NotTo(ContainSubstring("ORCResourcesOrphaned"))

		ac := &orcv1alpha1.ApplicationCredential{}
		g.Expect(c.Get(ctx, acKey(cp), ac)).To(Succeed())
		g.Expect(ac.Finalizers).To(ContainElement("openstack.k-orc.cloud/applicationcredential"),
			"the admin credential must be given its own window to revoke before it is orphaned")
		g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue())
	})
}

// TestDeleteProjectedRegistrations_ReachesAnExternalNamespaceAndSkipsForeignCRs
// covers the registration nothing else can collect: one projected into a
// dedicated namespace carries the ownership labels rather than an owner reference
// (no cascade), and under the External lifecycle its namespace is never deleted
// either. A same-named KeystoneService this ControlPlane did not project is left
// standing.
func TestDeleteProjectedRegistrations_ReachesAnExternalNamespaceAndSkipsForeignCRs(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingControlPlane(0)
	cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name:      "placement",
			Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
		},
	}
	cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{}

	ours := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      placementName(cp),
			Namespace: "placement",
			Labels:    controlPlaneChildLabels(cp),
		},
	}
	// Same name, no claim: somebody else's registration in a namespace this
	// ControlPlane does not own.
	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanName(cp), Namespace: cp.Namespace},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ours, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	remaining, err := r.deleteProjectedRegistrations(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remaining).To(BeEmpty(), "a registration with no finalizer is gone with the Delete")

	err = c.Get(ctx, client.ObjectKeyFromObject(ours), &c5c3v1alpha1.KeystoneService{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the registration in an External namespace must be deleted: nothing else can reach it")
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(foreign), &c5c3v1alpha1.KeystoneService{})).To(Succeed(),
		"a KeystoneService this ControlPlane did not project must be left alone")
}

// TestDeleteProjectedRegistrations_EnumeratesAPreservedRegistration pins that the
// names are enumerated whether or not the spec still declares the service:
// dropping a services.<svc> block without the deletion opt-in preserves the
// registration, and a preserved one still has to come down with the plane.
func TestDeleteProjectedRegistrations_EnumeratesAPreservedRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingControlPlane(0) // no services.glance and no services.neutron at all
	preserved := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      glanceName(cp),
			Namespace: cp.Namespace,
			Labels:    controlPlaneChildLabels(cp),
		},
	}
	preservedNeutron := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutronName(cp),
			Namespace: cp.Namespace,
			Labels:    controlPlaneChildLabels(cp),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, preserved, preservedNeutron).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	_, err := r.deleteProjectedRegistrations(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	for _, ks := range []*c5c3v1alpha1.KeystoneService{preserved, preservedNeutron} {
		err = c.Get(ctx, client.ObjectKeyFromObject(ks), &c5c3v1alpha1.KeystoneService{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"a registration preserved past its service block must still be torn down with the ControlPlane")
	}
}

// TestReconcileDelete_StallEscapeReleasesTheStalledRegistrationsChildren pins the
// stall path past orcTeardownDeadline. A registration the whole window was spent
// on keeps children nothing else can collect: they carry its ownership labels
// rather than an owner reference, so no GC cascade reaches them, and its own
// controller strips no K-ORC and no ESO finalizer on either of its paths. The
// release strips those finalizers, deletes the children, and names in one Warning
// what the managed ones leave behind in Keystone and what the PushSecret leaves in
// OpenBao. The unmanaged Role import is released too but never named: deleting an
// import is CR-only, so it orphans nothing.
func TestReconcileDelete_StallEscapeReleasesTheStalledRegistrationsChildren(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingControlPlane(orcTeardownDeadline + time.Minute)
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}

	// The registration wedged behind the finalizer its own controller cannot
	// release, with the children it projected still live: no KeystoneService
	// controller runs on the fake client, so nothing has begun their teardown.
	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       glanceName(cp),
			Namespace:  cp.Namespace,
			Labels:     controlPlaneChildLabels(cp),
			Finalizers: []string{keystoneServiceFinalizerName},
		},
	}
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:       keystoneServiceUserRef(ks),
			Namespace:  childNamespace(cp),
			Labels:     keystoneServiceChildLabels(ks),
			Finalizers: []string{"openstack.k-orc.cloud/user"},
		},
	}
	catalog := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:       keystoneServiceCatalogServiceRef(ks),
			Namespace:  childNamespace(cp),
			Labels:     keystoneServiceChildLabels(ks),
			Finalizers: []string{"openstack.k-orc.cloud/service"},
		},
	}
	roleImport := &orcv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:       keystoneServiceRoleImportRef(ks, "service"),
			Namespace:  childNamespace(cp),
			Labels:     keystoneServiceChildLabels(ks),
			Finalizers: []string{"openstack.k-orc.cloud/role"},
		},
		Spec: orcv1alpha1.RoleSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	const remoteKey = "c5c3/cp/glance/backup"
	push := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:       keystoneServicePushSecretName(ks),
			Namespace:  cp.Namespace,
			Labels:     keystoneServiceChildLabels(ks),
			Finalizers: []string{"pushsecret.externalsecrets.io/finalizer"},
		},
		Spec: esov1alpha1.PushSecretSpec{
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{RemoteKey: remoteKey},
				},
			}},
		},
	}
	// The ControlPlane's own stuck child, so the pass takes the stall escape
	// rather than the release path.
	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:       adminAppCredentialName(cp),
			Namespace:  childNamespace(cp),
			Finalizers: []string{"openstack.k-orc.cloud/applicationcredential"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, ks, user, catalog, roleImport, push, ac).Build()
	rec := record.NewFakeRecorder(20)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(ctx, key, cp)).To(Succeed())

	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "the stall escape must release without requeue")

	err = c.Get(ctx, key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the ControlPlane finalizer must be released after the stall escape")

	released := []struct {
		key client.ObjectKey
		obj client.Object
	}{
		{client.ObjectKeyFromObject(user), &orcv1alpha1.User{}},
		{client.ObjectKeyFromObject(catalog), &orcv1alpha1.Service{}},
		{client.ObjectKeyFromObject(roleImport), &orcv1alpha1.Role{}},
		{client.ObjectKeyFromObject(push), &esov1alpha1.PushSecret{}},
	}
	for _, child := range released {
		err = c.Get(ctx, child.key, child.obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"the stalled registration's child %q must be released and deleted, not left standing", child.key.Name)
	}

	events := drainEvents(rec)
	g.Expect(strings.Join(events, "\n")).To(ContainSubstring("ORCTeardownStalled"),
		"the fixture must take the stall escape, not the normal release path")

	var orphanWarnings []string
	for _, e := range events {
		if strings.Contains(e, "ServiceRegistrationResourcesOrphaned") {
			orphanWarnings = append(orphanWarnings, e)
		}
	}
	g.Expect(orphanWarnings).To(HaveLen(1),
		"the force-release must report every registration in ONE Warning, not one per child")
	g.Expect(orphanWarnings[0]).To(SatisfyAll(
		ContainSubstring("Warning"),
		ContainSubstring(user.Name),
		ContainSubstring(catalog.Name),
		ContainSubstring(remoteKey),
	), "the Warning must name the managed K-ORC CRs and the OpenBao path left behind")
	g.Expect(orphanWarnings[0]).NotTo(ContainSubstring(roleImport.Name),
		"an unmanaged import orphans nothing, so naming it would send the operator after a resource that is fine")

	gotKS := &c5c3v1alpha1.KeystoneService{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(ks), gotKS)).To(Succeed(),
		"the registration itself must survive the release of its children")
	g.Expect(gotKS.Finalizers).To(ContainElement(keystoneServiceFinalizerName),
		"the registration's own finalizer is its controller's to release, not the ControlPlane's")
}

// TestReconcileDelete_ReleasePathReleasesAGivenUpRegistration pins the second
// release point. Past registrationTeardownStallTimeout the teardown gives up on a
// registration and carries on, and with nothing of the ControlPlane's own left to
// revoke it reaches the NORMAL release in the same pass. The children of the
// given-up registration must be force-released there too, before the finalizer
// goes, and the three events must arrive in the order that reads as one story:
// gave up, orphaned, released.
func TestReconcileDelete_ReleasePathReleasesAGivenUpRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingControlPlane(registrationTeardownStallTimeout + time.Minute)
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}

	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       glanceName(cp),
			Namespace:  cp.Namespace,
			Labels:     controlPlaneChildLabels(cp),
			Finalizers: []string{keystoneServiceFinalizerName},
		},
	}
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:       keystoneServiceUserRef(ks),
			Namespace:  childNamespace(cp),
			Labels:     keystoneServiceChildLabels(ks),
			Finalizers: []string{"openstack.k-orc.cloud/user"},
		},
	}
	// No ApplicationCredential and no PushSecret of the ControlPlane's own, so
	// its K-ORC sweep finds nothing and the release path is reached in one pass.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks, user).Build()
	rec := record.NewFakeRecorder(20)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(ctx, key, cp)).To(Succeed())

	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "the release must return a zero result")

	events := drainEvents(rec)
	index := func(reason string) int {
		return slices.IndexFunc(events, func(e string) bool { return strings.Contains(e, reason) })
	}
	stalled := index("ServiceRegistrationTeardownStalled")
	orphaned := index("ServiceRegistrationResourcesOrphaned")
	complete := index("ORCTeardownComplete")
	g.Expect(stalled).To(BeNumerically(">=", 0), "the registration must be given up on, not waited for")
	g.Expect(orphaned).To(BeNumerically(">", stalled),
		"the children are released only after the registration is given up on")
	g.Expect(events[orphaned]).To(ContainSubstring(user.Name),
		"the Warning must name the managed User whose Keystone identity is left behind")
	g.Expect(complete).To(BeNumerically(">", orphaned),
		"the release must come last: after it the finalizer is gone and nothing can reach the children")

	err = c.Get(ctx, client.ObjectKeyFromObject(user), &orcv1alpha1.User{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the given-up registration's User must be released and deleted on the normal release path too")

	err = c.Get(ctx, key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the ControlPlane finalizer must be released once the children are gone")
}

// TestReconcileDelete_ReleasePathNamesTheESOPurgeWait guards the one wait on this
// path that used to requeue silently. It is reachable only once no K-ORC CR and no
// owned PushSecret remain, so the last KORCReady the sweep wrote says "0 K-ORC
// CR(s) and 0 PushSecret(s)" — an operator watching a ControlPlane sit in
// Terminating for the rest of orcTeardownDeadline would read a condition asserting
// nothing is outstanding, and no event names the registration PushSecret either.
func TestReconcileDelete_ReleasePathNamesTheESOPurgeWait(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingControlPlane(registrationTeardownStallTimeout + time.Minute)
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}

	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       glanceName(cp),
			Namespace:  cp.Namespace,
			Labels:     controlPlaneChildLabels(cp),
			Finalizers: []string{keystoneServiceFinalizerName},
		},
	}
	// The registration's PushSecret, still held by ESO, and the co-located tenant
	// store its DeletionPolicy=Delete purge authenticates through.
	push := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:       keystoneServicePushSecretName(ks),
			Namespace:  cp.Namespace,
			Labels:     keystoneServiceChildLabels(ks),
			Finalizers: []string{"pushsecret.externalsecrets.io/finalizer"},
		},
		Spec: esov1alpha1.PushSecretSpec{DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyDelete},
	}
	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: cp.Namespace},
	}
	// No ApplicationCredential and no PushSecret of the ControlPlane's own, so its
	// K-ORC sweep finds nothing and the release path is reached in one pass.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks, push, store).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(ctx, key, cp)).To(Succeed())

	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"a PushSecret still owed its OpenBao purge must hold the release for another pass")
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue(),
		"the ControlPlane must keep its finalizer while it waits")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil(),
		"a requeue with no condition leaves an operator no way to tell what the teardown is waiting for")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("FinalizingORC"))
	g.Expect(cond.Message).To(SatisfyAll(
		ContainSubstring("ESO"),
		ContainSubstring("KeystoneService"),
		ContainSubstring("PushSecret"),
	), "the condition must name the registration PushSecret purge the pass is waiting on")
	g.Expect(cond.Message).NotTo(ContainSubstring("0 K-ORC CR(s)"),
		"the sweep's own message asserts nothing is outstanding, which is exactly what misleads here")
}

// TestReleaseStalledRegistrationChildren_TouchesOnlyOwnedRegistrationsChildren
// pins the blast radius of the force-release. It strips finalizers that exist to
// revoke against Keystone, so every object it reaches has to be one THIS
// ControlPlane projected: a child of another registration, a same-named
// KeystoneService with no claim on it, and a namespace with no projected
// registration at all must all come through untouched and unreported.
func TestReleaseStalledRegistrationChildren_TouchesOnlyOwnedRegistrationsChildren(t *testing.T) {
	ctx := context.Background()

	// The labels of a registration this ControlPlane did not project, used to seed
	// a child that must survive every subtest.
	otherLabels := keystoneServiceChildLabels(&c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"},
	})
	otherUser := func(cp *c5c3v1alpha1.ControlPlane) *orcv1alpha1.User {
		return &orcv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "other-user",
				Namespace:  childNamespace(cp),
				Labels:     otherLabels,
				Finalizers: []string{"openstack.k-orc.cloud/user"},
			},
		}
	}
	ownedUser := func(ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane) *orcv1alpha1.User {
		return &orcv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{
				Name:       keystoneServiceUserRef(ks),
				Namespace:  childNamespace(cp),
				Labels:     keystoneServiceChildLabels(ks),
				Finalizers: []string{"openstack.k-orc.cloud/user"},
			},
		}
	}

	// The label selector is the only thing separating two registrations' children
	// in one namespace, so a miss here revokes a live service account.
	t.Run("a child labelled for another registration keeps its finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)

		s := korcTestScheme(t)
		cp := deletingControlPlane(orcTeardownDeadline)
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
		ks := &c5c3v1alpha1.KeystoneService{
			ObjectMeta: metav1.ObjectMeta{
				Name:       glanceName(cp),
				Namespace:  cp.Namespace,
				Labels:     controlPlaneChildLabels(cp),
				Finalizers: []string{keystoneServiceFinalizerName},
			},
		}
		ours, theirs := ownedUser(ks, cp), otherUser(cp)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks, ours, theirs).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue(), "no PushSecret is owed a purge, so the release must not ask for another pass")

		err = c.Get(ctx, client.ObjectKeyFromObject(ours), &orcv1alpha1.User{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the registration's own User must be released")

		got := &orcv1alpha1.User{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(theirs), got)).To(Succeed(),
			"a User labelled for another registration must be left standing")
		g.Expect(got.Finalizers).To(ContainElement("openstack.k-orc.cloud/user"),
			"another registration's User must keep the finalizer that revokes its identity")
	})

	// The registration name is derived from the ControlPlane's, so a foreign
	// KeystoneService in the same namespace can carry exactly the name this
	// ControlPlane projects. Ownership, not the name, decides.
	t.Run("a same-named registration without this ControlPlane's ownership is skipped", func(t *testing.T) {
		g := NewGomegaWithT(t)

		s := korcTestScheme(t)
		cp := deletingControlPlane(orcTeardownDeadline)
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
		foreign := &c5c3v1alpha1.KeystoneService{
			ObjectMeta: metav1.ObjectMeta{
				Name:       glanceName(cp),
				Namespace:  cp.Namespace,
				Finalizers: []string{keystoneServiceFinalizerName},
			},
		}
		child := ownedUser(foreign, cp)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign, child).Build()
		rec := record.NewFakeRecorder(10)
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue(), "no PushSecret is owed a purge, so the release must not ask for another pass")

		got := &orcv1alpha1.User{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(child), got)).To(Succeed(),
			"the child of a registration this ControlPlane did not project must be left standing")
		g.Expect(got.Finalizers).To(ContainElement("openstack.k-orc.cloud/user"))
		g.Expect(drainEvents(rec)).To(BeEmpty(),
			"nothing was released, so nothing must be reported as orphaned")
	})

	// The common teardown: every registration finished on its own, so the release
	// point has nothing to do and must stay silent about it.
	t.Run("no projected registration present", func(t *testing.T) {
		g := NewGomegaWithT(t)

		s := korcTestScheme(t)
		cp := deletingControlPlane(orcTeardownDeadline)
		theirs := otherUser(cp)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, theirs).Build()
		rec := record.NewFakeRecorder(10)
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue(), "no PushSecret is owed a purge, so the release must not ask for another pass")

		got := &orcv1alpha1.User{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(theirs), got)).To(Succeed())
		g.Expect(got.Finalizers).To(ContainElement("openstack.k-orc.cloud/user"),
			"a labelled child of a foreign registration must not be swept when no registration is present")
		g.Expect(drainEvents(rec)).To(BeEmpty(), "a teardown with nothing to release must emit no Warning")
	})

	// The ownership labels are plain labels whose values are published CR names, so
	// anything in the namespace can end up carrying them — a Kustomize `labels:`
	// block applied one directory too wide, a hand-copied import manifest. The name
	// test is the second half of the invariant sweepChildren enforces, and without
	// it a foreign object is force-stripped and deleted, its OpenStack resource left
	// behind with no Kubernetes object naming it.
	t.Run("a labelled object outside the registration's name prefix keeps its finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)

		s := korcTestScheme(t)
		cp := deletingControlPlane(orcTeardownDeadline)
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
		ks := &c5c3v1alpha1.KeystoneService{
			ObjectMeta: metav1.ObjectMeta{
				Name:       glanceName(cp),
				Namespace:  cp.Namespace,
				Labels:     controlPlaneChildLabels(cp),
				Finalizers: []string{keystoneServiceFinalizerName},
			},
		}
		// Correctly labelled for THIS registration, but named by neither
		// keystoneServiceChildPrefix nor the consumer-credentials name.
		strayUser := &orcv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "imported-by-hand",
				Namespace:  childNamespace(cp),
				Labels:     keystoneServiceChildLabels(ks),
				Finalizers: []string{"openstack.k-orc.cloud/user"},
			},
		}
		strayPush := &esov1alpha1.PushSecret{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "pushed-by-hand",
				Namespace:  cp.Namespace,
				Labels:     keystoneServiceChildLabels(ks),
				Finalizers: []string{"pushsecret.externalsecrets.io/finalizer"},
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks, strayUser, strayPush).Build()
		rec := record.NewFakeRecorder(10)
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())

		gotUser := &orcv1alpha1.User{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(strayUser), gotUser)).To(Succeed(),
			"an object outside the registration's name prefix must be left standing")
		g.Expect(gotUser.Finalizers).To(ContainElement("openstack.k-orc.cloud/user"),
			"a foreign User must keep the finalizer that revokes its Keystone identity")
		gotPush := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(strayPush), gotPush)).To(Succeed(),
			"the name guard must cover the PushSecrets too, not only the K-ORC CRs")
		g.Expect(gotPush.Finalizers).To(ContainElement("pushsecret.externalsecrets.io/finalizer"))
		g.Expect(drainEvents(rec)).To(BeEmpty(),
			"nothing was released, so nothing must be reported as orphaned")
	})
	// A read that fails is not a registration that is gone. Returning the error
	// keeps the ControlPlane's finalizer on for another pass, rather than
	// releasing it over children the release never got to look at.
	t.Run("a KeystoneService Get error is returned wrapped", func(t *testing.T) {
		g := NewGomegaWithT(t)

		s := korcTestScheme(t)
		cp := deletingControlPlane(orcTeardownDeadline)
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
		sentinel := errors.New("boom")
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
						return sentinel
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		_, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(strings.HasPrefix(err.Error(), "getting KeystoneService ")).To(BeTrue(),
			"the wrapped error must name the read that failed, got %q", err.Error())
		g.Expect(errors.Is(err, sentinel)).To(BeTrue(), "the cause must stay unwrappable")
	})
}

// stalledRegistrationFixture returns a ControlPlane past the registration stall
// window (deletionAge measured from its deletion timestamp) together with the
// projected Glance registration whose children the release reaches.
func stalledRegistrationFixture(
	deletionAge time.Duration,
) (*c5c3v1alpha1.ControlPlane, *c5c3v1alpha1.KeystoneService) {
	cp := deletingControlPlane(deletionAge)
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       glanceName(cp),
			Namespace:  cp.Namespace,
			Labels:     controlPlaneChildLabels(cp),
			Finalizers: []string{keystoneServiceFinalizerName},
		},
	}
	return cp, ks
}

// TestReleaseStalledRegistrationChildren_DeletesBeforeStripping pins the order of
// the two writes the release makes against a LIVE child — one whose controller
// never reached its own teardown, which is the case this function exists for.
// Stripping first is a write K-ORC sees as an ordinary update, so it re-adds its
// finalizer and the Delete behind it wedges the CR Terminating against a
// credential this teardown already revoked. The Delete has to land first: no
// controller adds a finalizer back to an object that already carries a
// deletionTimestamp.
func TestReleaseStalledRegistrationChildren_DeletesBeforeStripping(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp, ks := stalledRegistrationFixture(registrationTeardownStallTimeout + time.Minute)
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:       keystoneServiceUserRef(ks),
			Namespace:  childNamespace(cp),
			Labels:     keystoneServiceChildLabels(ks),
			Finalizers: []string{"openstack.k-orc.cloud/user"},
		},
	}

	var ops []string
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks, user).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.DeleteOption,
			) error {
				if _, ok := obj.(*orcv1alpha1.User); ok {
					ops = append(ops, "delete")
				}
				return c.Delete(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object,
				patch client.Patch, opts ...client.PatchOption,
			) error {
				if _, ok := obj.(*orcv1alpha1.User); ok {
					ops = append(ops, "strip")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.UpdateOption,
			) error {
				if _, ok := obj.(*orcv1alpha1.User); ok {
					ops = append(ops, "strip")
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	done, err := r.releaseStalledRegistrationChildren(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	g.Expect(ops).To(Equal([]string{"delete", "strip"}),
		"the Delete must land before the finalizer strip, or the owning controller re-adds it")
	err = c.Get(ctx, client.ObjectKeyFromObject(user), &orcv1alpha1.User{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the live child must be gone once its K-ORC finalizer is stripped")
}

// TestReleaseStalledRegistrationChildren_GivesESOItsPurgeWindow pins the posture
// the ControlPlane's own deleteOwnedPushSecrets takes, for the registration's
// PushSecret. It carries DeletionPolicy=Delete, so ESO purges the mirrored
// OpenBao data while processing the deletion — and the budget that brought the
// release here bounds the REGISTRATION's teardown, not ESO's. Within
// orcTeardownDeadline the PushSecret is deleted and left to ESO; past it the
// give-up posture applies and the finalizer goes. The wait is gated on the tenant
// store the purge authenticates through, so the fixture stands one up: co-located
// — the default — it lives in the ControlPlane's own namespace, which no sweep
// touches before the release.
func TestReleaseStalledRegistrationChildren_GivesESOItsPurgeWindow(t *testing.T) {
	ctx := context.Background()

	const remoteKey = "openstack/keystone/openstack/glance/service-accounts/credentials"
	fixture := func(deletionAge time.Duration, ifs interceptor.Funcs) (
		*c5c3v1alpha1.ControlPlane, *esov1alpha1.PushSecret, client.Client, *record.FakeRecorder,
		*ControlPlaneReconciler,
	) {
		s := korcTestScheme(t)
		cp, ks := stalledRegistrationFixture(deletionAge)
		store := &esov1.SecretStore{
			ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: cp.Namespace},
		}
		push := &esov1alpha1.PushSecret{
			ObjectMeta: metav1.ObjectMeta{
				Name:       keystoneServicePushSecretName(ks),
				Namespace:  cp.Namespace,
				Labels:     keystoneServiceChildLabels(ks),
				Finalizers: []string{"pushsecret.externalsecrets.io/finalizer"},
			},
			Spec: esov1alpha1.PushSecretSpec{
				DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyDelete,
				Data: []esov1alpha1.PushSecretData{{
					Match: esov1alpha1.PushSecretMatch{
						RemoteRef: esov1alpha1.PushSecretRemoteRef{RemoteKey: remoteKey},
					},
				}},
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks, push, store).
			WithInterceptorFuncs(ifs).Build()
		rec := record.NewFakeRecorder(10)
		return cp, push, c, rec, &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}
	}

	// The registration's controller never reached its teardown, so the PushSecret
	// is still LIVE. Stripping it here would forfeit the purge outright.
	t.Run("a live PushSecret is deleted and left to ESO", func(t *testing.T) {
		g := NewGomegaWithT(t)

		cp, push, c, rec, r := fixture(registrationTeardownStallTimeout+time.Minute, interceptor.Funcs{})

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeFalse(),
			"a PushSecret still owed its OpenBao purge must hold the release for another pass")

		got := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(push), got)).To(Succeed())
		g.Expect(got.DeletionTimestamp.IsZero()).To(BeFalse(),
			"the PushSecret must be deleted so ESO starts the DeletionPolicy=Delete purge")
		g.Expect(got.Finalizers).To(ContainElement("pushsecret.externalsecrets.io/finalizer"),
			"ESO must keep the finalizer it needs to finish the purge")
		g.Expect(drainEvents(rec)).To(BeEmpty(),
			"nothing is abandoned yet, so the release must not report an orphan")
	})

	// Past the shared deadline the trade every stall escape in this teardown makes
	// applies: a repairable leak beats a ControlPlane nobody can delete.
	t.Run("past orcTeardownDeadline the finalizer is stripped and the path named", func(t *testing.T) {
		g := NewGomegaWithT(t)

		cp, push, c, rec, r := fixture(orcTeardownDeadline+time.Minute, interceptor.Funcs{})

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue(), "past the deadline the release must not ask for another pass")

		err = c.Get(ctx, client.ObjectKeyFromObject(push), &esov1alpha1.PushSecret{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"the PushSecret must be released rather than left Terminating behind ESO")
		g.Expect(strings.Join(drainEvents(rec), "\n")).To(SatisfyAll(
			ContainSubstring("Warning"),
			ContainSubstring("ServiceRegistrationResourcesOrphaned"),
			ContainSubstring(remoteKey),
		), "the Warning must name the OpenBao path that keeps the data")
	})

	// A registration in a DEDICATED namespace reaches this release point only after
	// sweepNamespacesBeforeRelease deleted the tenant store the purge rides on
	// (Managed: the whole namespace, which the PushSecret's own finalizer then holds
	// Terminating). Waiting out the rest of orcTeardownDeadline there produces the
	// identical give-up outcome minutes later, so the wait must not be entered at
	// all.
	t.Run("a swept tenant store is not waited on", func(t *testing.T) {
		g := NewGomegaWithT(t)

		cp, push, c, rec, r := fixture(registrationTeardownStallTimeout+time.Minute, interceptor.Funcs{})
		g.Expect(c.Delete(ctx, &esov1.SecretStore{
			ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: cp.Namespace},
		})).To(Succeed(), "the namespace sweep deletes the tenant store before the release point")

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue(),
			"ESO cannot authenticate through a store that is gone, so the release must not wait for the purge")

		err = c.Get(ctx, client.ObjectKeyFromObject(push), &esov1alpha1.PushSecret{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"the PushSecret must be released rather than left holding its namespace Terminating")
		g.Expect(strings.Join(drainEvents(rec), "\n")).To(SatisfyAll(
			ContainSubstring("Warning"),
			ContainSubstring("ServiceRegistrationResourcesOrphaned"),
			ContainSubstring(remoteKey),
		), "the Warning must name the OpenBao path that keeps the data")
	})

	// The probe is a decision about whether to keep WAITING, and this release point
	// sits in the one branch of reconcileDelete with no deadline escape of its own.
	// A store read that cannot answer — Forbidden after a Helm upgrade narrowed the
	// ClusterRole, a SecretStore conversion webhook down while the ESO stack is
	// being upgraded — must therefore not become the pass's error: that would abort
	// the release for every other registration too and push the give-up out past
	// orcTeardownDeadline on reconcile backoff. It reads as alive instead, which is
	// exactly what this path did before the probe existed.
	t.Run("an unreadable tenant store is waited on rather than erroring the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)

		cp, push, c, rec, r := fixture(registrationTeardownStallTimeout+time.Minute, interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*esov1.SecretStore); ok {
					return apierrors.NewForbidden(
						schema.GroupResource{Group: "external-secrets.io", Resource: "secretstores"},
						key.Name, errors.New("RBAC: access denied"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		})

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred(),
			"a probe that cannot answer must not error the give-up path, which has no deadline escape")
		g.Expect(done).To(BeFalse(),
			"an unreadable store must read as standing, so the purge keeps its window")

		got := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(push), got)).To(Succeed())
		g.Expect(got.Finalizers).To(ContainElement("pushsecret.externalsecrets.io/finalizer"),
			"failing open must keep waiting, not give up early on a store it could not read")
		g.Expect(drainEvents(rec)).To(BeEmpty(),
			"nothing is abandoned yet, so the release must not report an orphan")
	})
}

// TestReleaseStalledRegistrationChildren_ReportsWhatItReleased pins the report
// against the two ways it used to mislead: it stayed silent about a release that
// only ever met children K-ORC had not finalized yet, and it named the hashed CR
// names of the ones it did strip — strings that appear nowhere in Keystone, on
// CRs deleted in the same pass.
func TestReleaseStalledRegistrationChildren_ReportsWhatItReleased(t *testing.T) {
	ctx := context.Background()

	// The 'controller never ran its own teardown' case in full: live children, no
	// K-ORC finalizer on any of them. Every object is deleted, so the release is
	// not a no-op and has to leave a record — but it abandons nothing, so naming
	// two empty sets in a Warning would send an operator after a leak that is not
	// there.
	t.Run("a release that abandons nothing reports it as Normal", func(t *testing.T) {
		g := NewGomegaWithT(t)

		s := korcTestScheme(t)
		cp, ks := stalledRegistrationFixture(registrationTeardownStallTimeout + time.Minute)
		user := &orcv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{
				Name:      keystoneServiceUserRef(ks),
				Namespace: childNamespace(cp),
				Labels:    keystoneServiceChildLabels(ks),
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks, user).Build()
		rec := record.NewFakeRecorder(10)
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())

		err = c.Get(ctx, client.ObjectKeyFromObject(user), &orcv1alpha1.User{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a live child must still be deleted")

		events := drainEvents(rec)
		g.Expect(events).To(HaveLen(1),
			"a release that deleted a child must leave a record of it, not return silently")
		g.Expect(events[0]).To(SatisfyAll(
			ContainSubstring("Normal"),
			ContainSubstring("ServiceRegistrationChildrenReleased"),
			ContainSubstring(client.ObjectKeyFromObject(ks).String()),
		))
		g.Expect(events[0]).NotTo(ContainSubstring("[]"),
			"nothing was abandoned, so the message must carry no empty list")
	})

	// The CR names are keystoneServiceChildPrefix-derived, so the sha256 segment in
	// them matches nothing in Keystone; the OpenStack names are what an operator
	// has to search for, and the CRs carrying them are gone a few lines later.
	t.Run("the Warning names the OpenStack resources, not the CRs", func(t *testing.T) {
		g := NewGomegaWithT(t)

		s := korcTestScheme(t)
		cp, ks := stalledRegistrationFixture(registrationTeardownStallTimeout + time.Minute)
		user := &orcv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{
				Name:       keystoneServiceUserRef(ks),
				Namespace:  childNamespace(cp),
				Labels:     keystoneServiceChildLabels(ks),
				Finalizers: []string{"openstack.k-orc.cloud/user"},
			},
			Spec: orcv1alpha1.UserSpec{
				Resource: &orcv1alpha1.UserResourceSpec{Name: ptr.To(orcv1alpha1.OpenStackName("glance"))},
			},
		}
		catalog := &orcv1alpha1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:       keystoneServiceCatalogServiceRef(ks),
				Namespace:  childNamespace(cp),
				Labels:     keystoneServiceChildLabels(ks),
				Finalizers: []string{"openstack.k-orc.cloud/service"},
			},
			Spec: orcv1alpha1.ServiceSpec{
				Resource: &orcv1alpha1.ServiceResourceSpec{
					Type: "image",
					Name: ptr.To(orcv1alpha1.OpenStackName("glance-catalog")),
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks, user, catalog).Build()
		rec := record.NewFakeRecorder(10)
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

		done, err := r.releaseStalledRegistrationChildren(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())

		events := drainEvents(rec)
		g.Expect(events).To(HaveLen(1))
		g.Expect(events[0]).To(SatisfyAll(
			ContainSubstring("Warning"),
			ContainSubstring("ServiceRegistrationResourcesOrphaned"),
			ContainSubstring("user glance"),
			ContainSubstring("catalog service glance-catalog"),
		), "the Warning must name what to repair in Keystone")
		g.Expect(events[0]).NotTo(ContainSubstring(user.Name),
			"the hashed CR name matches nothing in Keystone, and the CR is deleted in this pass")
		g.Expect(events[0]).NotTo(ContainSubstring("OpenBao path"),
			"no PushSecret was released, so the OpenBao clause must be left out entirely")
	})
}

// TestReconcileDelete_MixedRemainderStillWaitsForManaged asserts the release
// shortcut stays gated on the managed children: while a managed CR (here the
// application credential, whose revocation is real OpenStack work) is still
// Terminating, the unmanaged imports keep their K-ORC finalizers and the
// teardown waits at the K-ORC cadence.
func TestReconcileDelete_MixedRemainderStillWaitsForManaged(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	ns := childNamespace(cp)
	ac := &orcv1alpha1.ApplicationCredential{
		// ManagementPolicy unset counts as managed (fail-loud default).
		ObjectMeta: terminatingImportMeta(adminAppCredentialName(cp), ns, "openstack.k-orc.cloud/applicationcredential"),
	}
	svc := &orcv1alpha1.Service{
		ObjectMeta: terminatingImportMeta(keystoneServiceName(cp), ns, "openstack.k-orc.cloud/service"),
		Spec:       orcv1alpha1.ServiceSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ac, svc).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	gotSvc := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(svc), gotSvc)).To(Succeed())
	g.Expect(gotSvc.Finalizers).To(ContainElement("openstack.k-orc.cloud/service"),
		"unmanaged imports must NOT be released while a managed CR still needs K-ORC")

	g.Expect(drainEvents(rec)).NotTo(ContainElement(ContainSubstring("ORCImportsReleased")))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("FinalizingORC"))
}

// externalModeORCChildren returns the owned K-ORC CRs an External-mode
// ControlPlane projects, with the ManagementPolicy each really carries: the
// ApplicationCredential is Managed (its finalizer revokes at the Keystone level),
// while the admin User/Domain and the whole identity catalog — the Service plus
// one Endpoint per interface — are Unmanaged imports whose CR deletion cannot
// touch the external Keystone.
func externalModeORCChildren(cp *c5c3v1alpha1.ControlPlane) []client.Object {
	ns := childNamespace(cp)
	objs := []client.Object{
		&orcv1alpha1.ApplicationCredential{
			ObjectMeta: metav1.ObjectMeta{Name: adminAppCredentialName(cp), Namespace: ns},
			Spec:       orcv1alpha1.ApplicationCredentialSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
		},
		&orcv1alpha1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceName(cp), Namespace: ns},
			Spec: orcv1alpha1.ServiceSpec{
				ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
				Import:           &orcv1alpha1.ServiceImport{Filter: &orcv1alpha1.ServiceFilter{}},
			},
		},
		&orcv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: adminUserRef(cp), Namespace: ns},
			Spec: orcv1alpha1.UserSpec{
				ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
				Import:           &orcv1alpha1.UserImport{Filter: &orcv1alpha1.UserFilter{}},
			},
		},
		&orcv1alpha1.Domain{
			ObjectMeta: metav1.ObjectMeta{Name: adminDomainRef(cp), Namespace: ns},
			Spec: orcv1alpha1.DomainSpec{
				ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
				Import:           &orcv1alpha1.DomainImport{Filter: &orcv1alpha1.DomainFilter{}},
			},
		},
	}
	for _, iface := range externalCatalogInterfaces {
		objs = append(objs, &orcv1alpha1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{Name: keystoneEndpointImportName(cp, iface), Namespace: ns},
			Spec: orcv1alpha1.EndpointSpec{
				ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
				Import:           &orcv1alpha1.EndpointImport{Filter: &orcv1alpha1.EndpointFilter{}},
			},
		})
	}
	return objs
}

// TestReconcileDelete_ExternalMode_TearsDownOnlyOwnedORCCRs is the AC-4 guard:
// deleting an External-mode ControlPlane removes exactly the K-ORC CRs the
// operator owns — and provably nothing else. A same-namespace K-ORC User that the
// ControlPlane never created (another tenant's import) must survive.
func TestReconcileDelete_ExternalMode_TearsDownOnlyOwnedORCCRs(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	foreign := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-user", Namespace: childNamespace(cp)},
		Spec:       orcv1alpha1.UserSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	// A same-namespace Endpoint import that looks like a catalog import of a
	// DIFFERENT ControlPlane: only the cp.Name-scoped names keep it safe.
	foreignEndpoint := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "other-cp-identity-endpoint-public", Namespace: childNamespace(cp)},
	}
	objs := append([]client.Object{cp, foreign, foreignEndpoint}, externalModeORCChildren(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	// No K-ORC finalizers are seeded, so the CRs vanish on Delete and the sweep
	// releases the ControlPlane finalizer in one pass.
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeFalse(),
		"the ControlPlane finalizer must be released once every owned K-ORC CR is gone")

	// Every owned K-ORC CR is gone — including the three per-interface identity
	// Endpoint imports.
	children := orcChildObjects(cp)
	g.Expect(children).To(HaveLen(5+len(externalCatalogInterfaces)),
		"the sweep must enumerate the identity/admin CRs and the catalog imports")
	for _, child := range children {
		obj := child.newObj()
		key := types.NamespacedName{Name: child.name, Namespace: childNamespace(cp)}
		g.Expect(apierrors.IsNotFound(c.Get(ctx, key, obj))).To(BeTrue(),
			"owned K-ORC CR %s must be deleted", key.Name)
	}

	// ... and provably nothing else. The unrelated imports survive untouched.
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "someone-elses-user", Namespace: childNamespace(cp)},
		&orcv1alpha1.User{})).To(Succeed(), "a K-ORC CR the ControlPlane does not own must never be swept")
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(foreignEndpoint), &orcv1alpha1.Endpoint{})).
		To(Succeed(), "another ControlPlane's catalog import must never be swept")
}

// TestDeleteORCResources_ExternalMode_LeavesUnmanagedImportsUntouched pins WHY the
// sweep has zero blast radius on the external installation: the admin User/Domain
// AND the whole identity catalog the sweep deletes are Unmanaged imports, so
// removing their CRs cannot delete the OpenStack resources behind them — the
// external catalog is left bit-for-bit intact. Only the ApplicationCredential is
// Managed — its K-ORC finalizer revokes at the Keystone level before the CR delete
// returns, so authenticating with the revoked credential afterwards yields 404
// "Could not find Application Credential" (not 401).
func TestDeleteORCResources_ExternalMode_LeavesUnmanagedImportsUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	objs := append([]client.Object{cp}, externalModeORCChildren(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	// Read the management policies the sweep is about to act on, BEFORE the sweep.
	user := &orcv1alpha1.User{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: adminUserRef(cp), Namespace: childNamespace(cp)}, user)).To(Succeed())
	g.Expect(user.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(user.Spec.Import).NotTo(BeNil(), "the admin User is an import, not an owned resource")

	domain := &orcv1alpha1.Domain{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: adminDomainRef(cp), Namespace: childNamespace(cp)}, domain)).To(Succeed())
	g.Expect(domain.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(domain.Spec.Import).NotTo(BeNil())

	// The catalog itself: the identity Service and every endpoint interface are
	// imports, so teardown never removes a row from the external catalog.
	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneServiceName(cp), Namespace: childNamespace(cp)}, svc)).To(Succeed())
	g.Expect(svc.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
		"the identity Service is an import, so its CR delete cannot touch the external catalog")
	g.Expect(svc.Spec.Import).NotTo(BeNil())
	for _, iface := range externalCatalogInterfaces {
		ep := &orcv1alpha1.Endpoint{}
		g.Expect(c.Get(ctx, types.NamespacedName{
			Name: keystoneEndpointImportName(cp, iface), Namespace: childNamespace(cp),
		}, ep)).To(Succeed())
		g.Expect(ep.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
			"the %q endpoint is an import, so its CR delete cannot touch the external catalog", iface)
		g.Expect(ep.Spec.Import).NotTo(BeNil())
	}

	ac := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: adminAppCredentialName(cp), Namespace: childNamespace(cp)}, ac)).To(Succeed())
	g.Expect(ac.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged),
		"the app credential is the only identity object the operator minted, so the only one it revokes")

	remaining, hasLiveWork, err := r.deleteORCResources(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(hasLiveWork).To(BeTrue(), "live (not-yet-Terminating) CRs must announce the teardown once")
	g.Expect(remaining).To(BeEmpty())
}

// TestOrcChildObjects_ManagedModeUnchanged is the golden-behavior guard on the
// sweep: a Managed ControlPlane enumerates exactly the five identity/admin CRs it
// always did and nothing more, whether or not it declares an image, a placement or
// a key-manager service. Their catalog rows belong to the KeystoneService child
// projected for each of them, which tears them down under its own finalizer, so
// neither the External-mode nor the per-service additions widen the managed blast
// radius.
func TestOrcChildObjects_ManagedModeUnchanged(t *testing.T) {
	assertIdentityAndAdminOnly := func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) {
		t.Helper()
		g := NewGomegaWithT(t)

		children := orcChildObjects(cp)
		g.Expect(children).To(HaveLen(5))
		names := make([]string, 0, len(children))
		for _, child := range children {
			names = append(names, child.name)
		}
		g.Expect(names).To(ConsistOf(
			adminAppCredentialName(cp),
			keystoneServiceName(cp),
			keystoneEndpointName(cp),
			adminUserRef(cp),
			adminDomainRef(cp),
		))
	}

	t.Run("no built-in service declared", func(t *testing.T) {
		assertIdentityAndAdminOnly(t, korcControlPlane()) // services.glance, .placement, and .barbican unset
	})

	t.Run("every built-in service declared", func(t *testing.T) {
		cp := korcControlPlane()
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
		cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{}
		cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{
			SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
				Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
			},
		}
		assertIdentityAndAdminOnly(t, cp)
	})
}

// stalledExternalORCChildren returns every owned K-ORC CR of an External-mode
// ControlPlane, each stuck Terminating behind a K-ORC finalizer — the state the stall
// escape releases. The management policies are the ones the reconcilers really set,
// so a test can tell apart the CRs whose release leaks an OpenStack resource from the
// ones whose release costs nothing.
func stalledExternalORCChildren(cp *c5c3v1alpha1.ControlPlane) []client.Object {
	deletion := metav1.NewTime(metav1.Now().Add(-2 * orcTeardownStallTimeout))
	objs := externalModeORCChildren(cp)
	for _, obj := range objs {
		obj.SetFinalizers([]string{korcFinalizerPrefix + "stuck"})
		obj.SetDeletionTimestamp(&deletion)
	}
	return objs
}

// TestReconcileDelete_StallEscapeNamesOrphanedManagedResources is the guard on the
// blast radius of the stall escape. The escape strips openstack.k-orc.cloud/*
// finalizers with no ManagementPolicy check, so it releases a Managed CR by removing
// the very finalizer that would have revoked the application credential this
// ControlPlane minted in a Keystone it does not own. The credential survives with no
// Kubernetes object naming it. That is unavoidable (the alternative is a permanently
// wedged namespace), but a flat list of CR names under "unable to reach Keystone to
// revoke" never says an OpenStack resource leaked.
//
// The escape must therefore name exactly the Managed CRs it orphaned, and never the
// Unmanaged imports, whose CR deletion could not have touched OpenStack anyway.
func TestReconcileDelete_StallEscapeNamesOrphanedManagedResources(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	stalled := metav1.NewTime(metav1.Now().Add(-2 * orcTeardownStallTimeout))
	cp.DeletionTimestamp = &stalled

	objs := append([]client.Object{cp}, stalledExternalORCChildren(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	rec := record.NewFakeRecorder(20)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "the stall escape must release without requeue")

	var orphanEvent string
	for _, event := range drainEvents(rec) {
		if strings.Contains(event, "ORCResourcesOrphaned") {
			orphanEvent = event
		}
	}
	g.Expect(orphanEvent).NotTo(BeEmpty(),
		"releasing a Managed K-ORC CR abandons its OpenStack resource and must be reported as such")
	g.Expect(orphanEvent).To(HavePrefix("Warning"))

	// The one Managed CR: the application credential this ControlPlane minted in a
	// Keystone it does not own.
	g.Expect(orphanEvent).To(ContainSubstring(adminAppCredentialName(cp)))

	// The Unmanaged imports: their CR delete never called OpenStack, so nothing leaked.
	g.Expect(orphanEvent).NotTo(ContainSubstring(keystoneServiceName(cp)))
	g.Expect(orphanEvent).NotTo(ContainSubstring(adminUserRef(cp)))
	g.Expect(orphanEvent).NotTo(ContainSubstring(adminDomainRef(cp)))
	for _, iface := range externalCatalogInterfaces {
		g.Expect(orphanEvent).NotTo(ContainSubstring(keystoneEndpointImportName(cp, iface)))
	}
}

// TestIsManagedORCChild_UnsetPolicyCountsAsManaged pins the fail-loud default: K-ORC
// defaults managementPolicy to `managed`, so a CR whose policy the reconciler never
// stamped must be reported as orphaned rather than silently omitted from the warning.
func TestIsManagedORCChild_UnsetPolicyCountsAsManaged(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(isManagedORCChild(&orcv1alpha1.Service{})).To(BeTrue())
	g.Expect(isManagedORCChild(&orcv1alpha1.Service{
		Spec: orcv1alpha1.ServiceSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	})).To(BeFalse())
}

// TestReconcileDelete_ExternalMode_NoORCResources_ReleasesFinalizer covers the
// edge path where the K-ORC chain never converged: an External-mode ControlPlane
// deleted before any K-ORC CR was projected must still release its finalizer
// rather than wedge on Terminating.
func TestReconcileDelete_ExternalMode_NoORCResources_ReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeFalse())
}

// --- Service-account teardown ---

func TestIsManagedORCChild_ClassifiesProject(t *testing.T) {
	g := NewGomegaWithT(t)
	managed := &orcv1alpha1.Project{Spec: orcv1alpha1.ProjectSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged}}
	unmanaged := &orcv1alpha1.Project{Spec: orcv1alpha1.ProjectSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged}}
	g.Expect(isManagedORCChild(managed)).To(BeTrue(), "a managed Project leaks on force-remove")
	g.Expect(isManagedORCChild(unmanaged)).To(BeFalse(), "an unmanaged reference Project is a CR-only delete")
}

// TestIsManagedORCChild_ClassifiesRoleChildren pins the two role kinds: the managed
// RoleAssignment leaks on force-remove (its finalizer revokes the assignment in
// Keystone), while the unmanaged Role import is a CR-only delete that must be
// force-releasable without a false orphan warning.
func TestIsManagedORCChild_ClassifiesRoleChildren(t *testing.T) {
	g := NewGomegaWithT(t)
	managedAssignment := &orcv1alpha1.RoleAssignment{
		Spec: orcv1alpha1.RoleAssignmentSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}
	unmanagedRole := &orcv1alpha1.Role{
		Spec: orcv1alpha1.RoleSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	g.Expect(isManagedORCChild(managedAssignment)).To(BeTrue(), "a managed RoleAssignment leaks on force-remove")
	g.Expect(isManagedORCChild(unmanagedRole)).To(BeFalse(), "an unmanaged Role import is a CR-only delete")
}

// --- cross-namespace teardown (issue #646) ---

// namespaceTeardownScheme extends the K-ORC test scheme with the service-child
// and backing-service types the cross-namespace teardown deletes.
func namespaceTeardownScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := korcTestScheme(t)
	if err := keystonev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding keystone scheme: %v", err)
	}
	if err := horizonv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding horizon scheme: %v", err)
	}
	if err := glancev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding glance scheme: %v", err)
	}
	if err := placementv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding placement scheme: %v", err)
	}
	if err := barbicanv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding barbican scheme: %v", err)
	}
	if err := openbaov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding openbao scheme: %v", err)
	}
	if err := mariadbv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding mariadb scheme: %v", err)
	}
	return s
}

// deletingNamespacedControlPlane returns a deleting ControlPlane that placed
// Keystone in an operator-owned namespace and Horizon in a pre-existing one.
func deletingNamespacedControlPlane(deletionAge time.Duration) *c5c3v1alpha1.ControlPlane {
	cp := deletingControlPlane(deletionAge)
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "identity",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
		},
		Horizon: &c5c3v1alpha1.ServiceHorizonSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "dashboard",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
	}
	return cp
}

// TestCrossNamespaceServiceChildren_IncludesGlance verifies the Glance child is
// enumerated for the namespace it was assigned to, and excluded from any other —
// so a Glance placed in a namespace of its own is torn down by the finalizer sweep
// (which carries no owner reference to garbage-collect it), while a namespace it
// was never placed in never names it.
func TestCrossNamespaceServiceChildren_IncludesGlance(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
	}

	hasGlance := func(namespace string) bool {
		for _, child := range crossNamespaceServiceChildren(cp, namespace) {
			if _, ok := child.(*glancev1alpha1.Glance); ok && child.GetName() == glanceName(cp) {
				return true
			}
		}
		return false
	}

	g.Expect(hasGlance("images")).To(BeTrue(), "the Glance child is enumerated for its assigned namespace")
	g.Expect(hasGlance("unrelated")).To(BeFalse(), "a namespace Glance was not placed in must not name it")
}

// TestCrossNamespaceServiceChildren_IncludesPlacement is the same guard for the
// Placement child: it is enumerated for the namespace it was assigned to and
// excluded from any other, so a Placement placed in a namespace of its own is torn
// down by the finalizer sweep (it carries no owner reference to garbage-collect
// it).
func TestCrossNamespaceServiceChildren_IncludesPlacement(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
	}

	hasPlacement := func(namespace string) bool {
		for _, child := range crossNamespaceServiceChildren(cp, namespace) {
			if _, ok := child.(*placementv1alpha1.Placement); ok && child.GetName() == placementName(cp) {
				return true
			}
		}
		return false
	}

	g.Expect(hasPlacement("compute")).To(BeTrue(), "the Placement child is enumerated for its assigned namespace")
	g.Expect(hasPlacement("unrelated")).To(BeFalse(), "a namespace Placement was not placed in must not name it")
}

// TestCrossNamespaceServiceChildren_IncludesNeutron is the same guard for the
// Neutron child: it is enumerated for the namespace it was assigned to and
// excluded from any other, so a Neutron placed in a namespace of its own is torn
// down by the finalizer sweep (it carries no owner reference to garbage-collect
// it). The OVNCentral it references is enumerated nowhere, on purpose: the plane
// only reads it.
func TestCrossNamespaceServiceChildren_IncludesNeutron(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Neutron = &c5c3v1alpha1.ServiceNeutronSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: "network", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
		OVN: c5c3v1alpha1.NeutronOVNSpec{
			CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn", Namespace: "ovn-system"},
		},
	}

	hasNeutron := func(namespace string) bool {
		for _, child := range crossNamespaceServiceChildren(cp, namespace) {
			if _, ok := child.(*neutronv1alpha1.Neutron); ok && child.GetName() == neutronName(cp) {
				return true
			}
		}
		return false
	}

	g.Expect(hasNeutron("network")).To(BeTrue(), "the Neutron child is enumerated for its assigned namespace")
	g.Expect(hasNeutron("unrelated")).To(BeFalse(), "a namespace Neutron was not placed in must not name it")
	g.Expect(crossNamespaceServiceChildren(cp, "ovn-system")).To(BeEmpty(),
		"the referenced OVNCentral is never enumerated for deletion")
}

// TestDeleteServiceChildrenIn_SweepsOwnedGlanceBackends verifies the cross-namespace
// teardown reaps the projected GlanceBackend children the ControlPlane placed in a
// dedicated namespace: a c5c3-owned backend carrying the glance child's name prefix
// is deleted and reported as remaining (so the sweep waits for it), while a
// hand-created backend that merely shares the namespace is left untouched.
func TestDeleteServiceChildrenIn_SweepsOwnedGlanceBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)

	cp := deletingControlPlane(time.Minute)
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Glance: &c5c3v1alpha1.ServiceGlanceSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
		},
	}

	// A projected, label-owned backend carrying the glance child's name prefix (a
	// cross-namespace child cannot carry an owner reference).
	owned := &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name: glanceBackendName(cp, "primary"), Namespace: "images",
			Labels: controlPlaneChildLabels(cp),
		},
	}
	// A hand-created backend sharing the namespace and the prefix but owned by nobody.
	foreign := &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{Name: glanceBackendName(cp, "byo"), Namespace: "images"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, owned, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	remaining, err := r.deleteServiceChildrenIn(context.Background(), cp, "images")
	g.Expect(err).NotTo(HaveOccurred())

	// The owned backend was deleted and reported as remaining so the sweep waits.
	g.Expect(remaining).To(ContainElement("images/" + glanceBackendName(cp, "primary")))
	err = c.Get(context.Background(), client.ObjectKeyFromObject(owned), &glancev1alpha1.GlanceBackend{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the owned projected backend must be swept")

	// The foreign backend is untouched and never reported as remaining.
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &glancev1alpha1.GlanceBackend{})).
		To(Succeed(), "a hand-created backend we do not own must never be swept")
	g.Expect(remaining).NotTo(ContainElement("images/" + glanceBackendName(cp, "byo")))
}

// TestTeardownDedicatedNamespaces_NoAssignments verifies the default costs
// nothing: a ControlPlane with no service namespaces reports done at once.
func TestTeardownDedicatedNamespaces_NoAssignments(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingControlPlane(time.Minute)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())
}

// TestTeardownDedicatedNamespaces_WaitsForServiceChildren pins the ordering: the
// service children are deleted and WAITED on before anything else, because their
// own operators run a sequenced ESO cleanup through the tenant store in the same
// namespace — removing the store first would strand their key material in OpenBao.
func TestTeardownDedicatedNamespaces_WaitsForServiceChildren(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingNamespacedControlPlane(time.Minute)

	// A Keystone child held by its own cleanup finalizer, so the Delete leaves it
	// Terminating rather than gone.
	keystone := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneName(cp), Namespace: "identity",
			Finalizers: []string{"keystone.openstack.c5c3.io/cleanup"},
		},
	}
	stampControlPlaneChildLabels(keystone, cp)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "identity", Labels: controlPlaneChildLabels(cp),
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, keystone, ns).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse(), "the sweep must wait for the service child")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNamespacesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("FinalizingNamespaces"))

	// The Keystone child was deleted (Terminating), and the namespace still stands:
	// deleting it now would cascade the child out from under its own cleanup.
	live := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneName(cp), Namespace: "identity",
	}, live)).To(Succeed())
	g.Expect(live.DeletionTimestamp).NotTo(BeNil())
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "identity"}, &corev1.Namespace{})).To(Succeed())
}

// TestTeardownDedicatedNamespaces_DeletesTheManagedNamespace verifies a Managed
// namespace is deleted once its children are gone — that is the whole point of
// the Managed lifecycle, and the namespace delete cascades whatever is left in it.
func TestTeardownDedicatedNamespaces_DeletesTheManagedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingNamespacedControlPlane(time.Minute)
	cp.Spec.Services.Horizon = nil

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "identity", Labels: controlPlaneChildLabels(cp),
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ns).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	err = c.Get(context.Background(), types.NamespacedName{Name: "identity"}, &corev1.Namespace{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a Managed namespace must be deleted with the ControlPlane")
}

// TestTeardownDedicatedNamespaces_RefusesToDeleteAnUnownedNamespace is the guard
// that matters most on the way out: a namespace carrying no ownership labels was
// not created by us, so deleting it would destroy every workload in it. It is left
// standing and the operator is warned.
func TestTeardownDedicatedNamespaces_RefusesToDeleteAnUnownedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingNamespacedControlPlane(time.Minute)
	cp.Spec.Services.Horizon = nil

	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "identity", Labels: map[string]string{"team": "platform"},
	}}
	rec := record.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "identity"}, &corev1.Namespace{})).
		To(Succeed(), "a namespace we did not create must never be deleted")
	g.Expect(strings.Join(drainEvents(rec), "\n")).To(ContainSubstring("NamespaceNotOwned"))
}

// TestTeardownDedicatedNamespaces_SweepsExternalNamespaceResidue verifies the
// External lifecycle: the namespace survives, so nothing cascades and every object
// the ControlPlane placed there has to be named and deleted — while a same-named
// object belonging to somebody else in that shared namespace is left alone.
func TestTeardownDedicatedNamespaces_SweepsExternalNamespaceResidue(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)

	// Keystone, Glance, and Placement in the External namespace, so their credential
	// material lands there.
	cp := deletingControlPlane(time.Minute)
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-ns",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
		Glance: &c5c3v1alpha1.ServiceGlanceSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-ns",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
		Placement: &c5c3v1alpha1.ServicePlacementSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-ns",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
	}
	ours := &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantStoreName, Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	adminPw := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	glanceDB := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: glanceDBCredentialSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	// The Dynamic-mode Glance DB-credential objects: the generator, its mTLS client
	// Certificate, and the generator's ServiceAccount are label-owned residue too.
	glanceDBVDS := &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
		Name: glanceDBCredentialSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	glanceDBCert := &unstructured.Unstructured{}
	glanceDBCert.SetGroupVersionKind(certificateGVK)
	glanceDBCert.SetName(glanceDBCredentialClientCertName(cp))
	glanceDBCert.SetNamespace("shared-ns")
	glanceDBCert.SetLabels(controlPlaneChildLabels(cp))
	glanceDBSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: glanceDBCredentialServiceAccountName, Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	// The Placement credential material in the same four shapes.
	placementDB := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: placementDBCredentialSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	placementDBVDS := &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
		Name: placementDBCredentialSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	placementDBCert := &unstructured.Unstructured{}
	placementDBCert.SetGroupVersionKind(certificateGVK)
	placementDBCert.SetName(placementDBCredentialClientCertName(cp))
	placementDBCert.SetNamespace("shared-ns")
	placementDBCert.SetLabels(controlPlaneChildLabels(cp))
	placementDBSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: placementDBCredentialServiceAccountName, Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	// Somebody else's ServiceAccount of the same fixed name in the shared namespace.
	foreignSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantServiceAccountName, Namespace: "shared-ns",
	}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shared-ns"}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		cp, ns, ours, adminPw, glanceDB, glanceDBVDS, glanceDBCert, glanceDBSA,
		placementDB, placementDBVDS, placementDBCert, placementDBSA, foreignSA,
	).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "shared-ns"}, &corev1.Namespace{})).
		To(Succeed(), "an External namespace must survive the ControlPlane")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: esoTenantStoreName, Namespace: "shared-ns",
	}, &esov1.SecretStore{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "our tenant store must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordSecretName(cp), Namespace: "shared-ns",
	}, &esov1.ExternalSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "our credential material must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: "shared-ns",
	}, &esov1.ExternalSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Glance DB-credential ExternalSecret must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: "shared-ns",
	}, &esgenv1alpha1.VaultDynamicSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Glance VaultDynamicSecret generator must be swept")

	sweptGlanceCert := &unstructured.Unstructured{}
	sweptGlanceCert.SetGroupVersionKind(certificateGVK)
	err = c.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialClientCertName(cp), Namespace: "shared-ns",
	}, sweptGlanceCert)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Glance DB-credential Certificate must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialServiceAccountName, Namespace: "shared-ns",
	}, &corev1.ServiceAccount{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Glance DB-credential ServiceAccount must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: "shared-ns",
	}, &esov1.ExternalSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Placement DB-credential ExternalSecret must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: "shared-ns",
	}, &esgenv1alpha1.VaultDynamicSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Placement VaultDynamicSecret generator must be swept")

	sweptPlacementCert := &unstructured.Unstructured{}
	sweptPlacementCert.SetGroupVersionKind(certificateGVK)
	err = c.Get(context.Background(), types.NamespacedName{
		Name: placementDBCredentialClientCertName(cp), Namespace: "shared-ns",
	}, sweptPlacementCert)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Placement DB-credential Certificate must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: placementDBCredentialServiceAccountName, Namespace: "shared-ns",
	}, &corev1.ServiceAccount{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Placement DB-credential ServiceAccount must be swept")

	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: esoTenantServiceAccountName, Namespace: "shared-ns",
	}, &corev1.ServiceAccount{})).To(Succeed(),
		"an object we do not own must survive, even under a name we also use")
}

// TestTeardownDedicatedNamespaces_StallEscape verifies the bounded escape: past the
// stall window a child that will not go must not make the namespace undeletable
// forever. The sweep warns, names what it left behind, and releases.
func TestTeardownDedicatedNamespaces_StallEscape(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingNamespacedControlPlane(orcTeardownDeadline + time.Minute)
	cp.Spec.Services.Horizon = nil

	wedged := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneName(cp), Namespace: "identity",
			Finalizers: []string{"keystone.openstack.c5c3.io/cleanup"},
		},
	}
	stampControlPlaneChildLabels(wedged, cp)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "identity", Labels: controlPlaneChildLabels(cp),
	}}
	rec := record.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, wedged, ns).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue(), "the stall escape must release rather than wedge forever")

	events := strings.Join(drainEvents(rec), "\n")
	g.Expect(events).To(ContainSubstring("NamespaceTeardownStalled"))
	g.Expect(events).To(ContainSubstring("identity/" + keystoneName(cp)))
	g.Expect(events).To(ContainSubstring(orcTeardownDeadline.String()),
		"the Warning must name the deadline this sweep is gated on")
}

// --- barbican ensemble teardown ---

// barbicanTeardownNamespace is the namespace the fixtures below assign to the
// Barbican service, so its child, its secret store, and the dedicated OpenBao
// ensemble all land outside the ControlPlane's own namespace.
const barbicanTeardownNamespace = "keymanager"

// deletingBarbicanControlPlane returns a deleting ControlPlane whose Barbican
// service takes a dedicated secret store in a namespace of its own, under the
// given lifecycle.
func deletingBarbicanControlPlane(
	deletionAge time.Duration, lifecycle c5c3v1alpha1.ServiceNamespaceLifecycle,
) *c5c3v1alpha1.ControlPlane {
	cp := deletingControlPlane(deletionAge)
	cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: barbicanTeardownNamespace, Lifecycle: lifecycle,
		},
		SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
			Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
		},
	}
	return cp
}

// ownedBarbicanCertificate builds one of the ensemble's cert-manager Certificates
// the way the projection leaves it: unstructured (no Go type ships for them) and
// carrying the ownership labels a cross-namespace child takes instead of an owner
// reference.
func ownedBarbicanCertificate(cp *c5c3v1alpha1.ControlPlane, name string) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(name)
	cert.SetNamespace(cp.BarbicanNamespace())
	cert.SetLabels(controlPlaneChildLabels(cp))
	return cert
}

// ownedBarbicanEnsemble returns the label-owned ensemble objects that outlive the
// dedicated OpenBao instance: the tenant admitting the namespace, both transport
// Certificates, the provisioner account, the TokenRequest grant, the static-seal
// Secret, and the cluster-scoped auth-delegator binding.
func ownedBarbicanEnsemble(cp *c5c3v1alpha1.ControlPlane) []client.Object {
	ns, name := cp.BarbicanNamespace(), barbicanOpenBaoName(cp)
	return []client.Object{
		&openbaov1alpha1.OpenBaoTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name: name + barbicanOpenBaoTenantSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
			},
			Spec: openbaov1alpha1.OpenBaoTenantSpec{TargetNamespace: ns},
		},
		ownedBarbicanCertificate(cp, name+barbicanOpenBaoServerCertSuffix),
		ownedBarbicanCertificate(cp, name+barbicanOpenBaoCACertSuffix),
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: name + barbicanOpenBaoProvisionerSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
		}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
			Name: name + barbicanOpenBaoTokenGrantSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
		}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name: name + barbicanOpenBaoTokenGrantSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name + barbicanOpenBaoUnsealSecretSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
		}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name: barbicanOpenBaoAuthDelegatorName(name, ns), Labels: controlPlaneChildLabels(cp),
		}},
	}
}

// expectSwept asserts every named object is gone from the cluster.
func expectSwept(t *testing.T, c client.Client, objs ...client.Object) {
	t.Helper()
	g := NewGomegaWithT(t)
	for _, obj := range objs {
		fresh := obj.DeepCopyObject().(client.Object)
		key := client.ObjectKeyFromObject(obj)
		g.Expect(apierrors.IsNotFound(c.Get(context.Background(), key, fresh))).
			To(BeTrue(), "%T %s must be swept", obj, key)
	}
}

// expectPresent asserts every named object is still in the cluster.
func expectPresent(t *testing.T, c client.Client, objs ...client.Object) {
	t.Helper()
	g := NewGomegaWithT(t)
	for _, obj := range objs {
		fresh := obj.DeepCopyObject().(client.Object)
		key := client.ObjectKeyFromObject(obj)
		g.Expect(c.Get(context.Background(), key, fresh)).
			To(Succeed(), "%T %s must be left alone", obj, key)
	}
}

// TestCrossNamespaceServiceChildren_IncludesBarbicanEnsemble pins the WAIT SET the
// Barbican namespace contributes: the child, its secret store, and the dedicated
// OpenBao instance. The instance has to be in there. The namespace must not be
// deleted until the openbao-operator has run the instance's finalizer, and that
// finalizer works through the tenant RBAC in this very namespace — deleting the
// namespace first reaps the RBAC mid-run and wedges it in Terminating.
func TestCrossNamespaceServiceChildren_IncludesBarbicanEnsemble(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	// Keystone in a namespace of its own, so the per-namespace split is provable.
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: "identity", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
	}

	children := crossNamespaceServiceChildren(cp, barbicanTeardownNamespace)
	g.Expect(children).To(HaveLen(3))
	g.Expect(children[0]).To(BeAssignableToTypeOf(&barbicanv1alpha1.Barbican{}))
	g.Expect(children[0].GetName()).To(Equal(barbicanName(cp)))
	g.Expect(children[1]).To(BeAssignableToTypeOf(&barbicanv1alpha1.BarbicanSecretStore{}))
	g.Expect(children[1].GetName()).To(Equal(barbicanSecretStoreName(cp)))
	g.Expect(children[2]).To(BeAssignableToTypeOf(&openbaov1alpha1.OpenBaoCluster{}))
	g.Expect(children[2].GetName()).To(Equal(barbicanOpenBaoName(cp)))

	// The other namespaces are unaffected: Keystone's names its own child alone, and
	// a namespace Barbican was never placed in names nothing.
	identity := crossNamespaceServiceChildren(cp, "identity")
	g.Expect(identity).To(HaveLen(1))
	g.Expect(identity[0]).To(BeAssignableToTypeOf(&keystonev1alpha1.Keystone{}))
	g.Expect(crossNamespaceServiceChildren(cp, "unrelated")).To(BeEmpty())
}

// TestDeleteServiceChildrenIn_BarbicanEnsembleOrdersTheTenantAfterTheInstance is
// the ordering guard on the ensemble sweep. While the instance is still finalizing
// the tenant that admitted the namespace stays put — deleting it first strips the
// RBAC the openbao-operator needs to finish, so the instance never goes and the
// namespace never becomes deletable. Everything the instance does not depend on
// comes down right away, including the cluster-scoped auth-delegator binding no
// namespace deletion would ever reclaim.
func TestDeleteServiceChildrenIn_BarbicanEnsembleOrdersTheTenantAfterTheInstance(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ns := barbicanTeardownNamespace

	child := &barbicanv1alpha1.Barbican{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	store := &barbicanv1alpha1.BarbicanSecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanSecretStoreName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	// The instance is held by the openbao-operator's finalizer, so the Delete leaves
	// it Terminating rather than gone — the state the tenant must outlive.
	instance := &openbaov1alpha1.OpenBaoCluster{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanOpenBaoName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
		Finalizers: []string{openbaov1alpha1.OpenBaoClusterFinalizer},
	}}
	ensemble := ownedBarbicanEnsemble(cp)
	tenant, rest := ensemble[0], ensemble[1:]
	// The POSITIVE case of the undeclared-store sweep: an owned, prefix-matching
	// store nobody names any more, which a spec edit landing moments before the
	// delete leaves behind. Without it the sweep's Delete never runs in this suite.
	staleStore := &barbicanv1alpha1.BarbicanSecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanName(cp) + "-stale", Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}

	objs := append([]client.Object{cp, child, store, instance, staleStore}, ensemble...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	remaining, err := r.deleteServiceChildrenIn(ctx, cp, ns)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remaining).To(ConsistOf(
		ns+"/"+barbicanName(cp),
		ns+"/"+barbicanSecretStoreName(cp),
		ns+"/"+barbicanOpenBaoName(cp),
		ns+"/"+barbicanName(cp)+"-stale",
	), "the child, the store, the instance, and the swept undeclared store gate the namespace deletion")
	expectSwept(t, c, staleStore)

	expectPresent(t, c, tenant)
	expectSwept(t, c, rest...)

	// The openbao-operator finishes: its finalizer goes and the instance leaves etcd.
	live := &openbaov1alpha1.OpenBaoCluster{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(instance), live)).To(Succeed())
	live.Finalizers = nil
	g.Expect(c.Update(ctx, live)).To(Succeed())

	remaining, err = r.deleteServiceChildrenIn(ctx, cp, ns)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remaining).To(BeEmpty(), "nothing gates the namespace deletion once the instance is gone")
	expectSwept(t, c, tenant)
}

// TestDeleteServiceChildrenIn_BarbicanEnsembleToleratesAlreadyGoneObjects covers
// the edge path a re-run always takes: most of the ensemble was reclaimed by an
// earlier pass (or never projected, because the service took an external secret
// store), so every Get is a NotFound the sweep must swallow while it still reaps
// what IS there.
func TestDeleteServiceChildrenIn_BarbicanEnsembleToleratesAlreadyGoneObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ensemble := ownedBarbicanEnsemble(cp)
	// Only the static-seal Secret and the cluster-scoped binding survived.
	sealSecret, binding := ensemble[len(ensemble)-2], ensemble[len(ensemble)-1]

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, sealSecret, binding).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	remaining, err := r.deleteServiceChildrenIn(context.Background(), cp, barbicanTeardownNamespace)
	g.Expect(err).NotTo(HaveOccurred(), "an already-gone ensemble object must not fail the teardown")
	g.Expect(remaining).To(BeEmpty())
	expectSwept(t, c, sealSecret, binding)
}

// TestBarbicanTeardown_LeavesForeignEnsembleObjectsAlone is the blast-radius guard
// on the two objects the sweep could destroy for somebody else. The OpenBaoTenant
// admitting the namespace may predate this ControlPlane (in the kind stack the
// proving instance's tenant already admits it), and the auth-delegator binding is
// cluster-scoped, so a name collision reaches across the whole cluster. Neither is
// touched without the ownership labels — on either lifecycle path.
func TestBarbicanTeardown_LeavesForeignEnsembleObjectsAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleExternal)
	ns, name := barbicanTeardownNamespace, barbicanOpenBaoName(cp)

	foreignTenant := &openbaov1alpha1.OpenBaoTenant{
		ObjectMeta: metav1.ObjectMeta{Name: name + barbicanOpenBaoTenantSuffix, Namespace: ns},
		Spec:       openbaov1alpha1.OpenBaoTenantSpec{TargetNamespace: ns},
	}
	foreignBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanOpenBaoAuthDelegatorName(name, ns)},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreignTenant, foreignBinding).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	_, err := r.deleteServiceChildrenIn(ctx, cp, ns)
	g.Expect(err).NotTo(HaveOccurred())
	expectPresent(t, c, foreignTenant, foreignBinding)

	r.sweepExternalNamespaceResidue(ctx, c, cp, ns)
	expectPresent(t, c, foreignTenant, foreignBinding)
}

// TestReconcileDelete_RemovesTheColocatedAuthDelegatorBinding closes the hole no
// namespace sweep can reach. With Barbican co-located in the ControlPlane's own
// namespace there is no dedicated namespace, so teardownDedicatedNamespaces returns
// at once and the ensemble sweep never runs; the binding is cluster-scoped, so the
// GC cascade behind the released finalizer cannot collect it either. Deleting the
// ControlPlane has to remove it by name, and a same-named binding belonging to
// somebody else has to survive that: the name is cluster-wide, so a collision is
// not confined to one namespace.
func TestReconcileDelete_RemovesTheColocatedAuthDelegatorBinding(t *testing.T) {
	// colocatedBarbicanControlPlane returns a deleting ControlPlane whose Barbican
	// service declares no namespace block, so it shares the ControlPlane's namespace.
	colocatedBarbicanControlPlane := func(g *WithT) *c5c3v1alpha1.ControlPlane {
		cp := deletingControlPlane(time.Minute)
		cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{
			SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
				Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
			},
		}
		g.Expect(cp.DedicatedServiceNamespaces()).To(BeEmpty(),
			"the fixture must be co-located, or the per-namespace sweep would cover the binding")
		return cp
	}

	t.Run("deletes the binding it owns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		s := namespaceTeardownScheme(t)

		cp := colocatedBarbicanControlPlane(g)
		binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name:   barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace()),
			Labels: controlPlaneChildLabels(cp),
		}}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, binding).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
		g.Expect(c.Get(ctx, key, cp)).To(Succeed())

		res, err := r.reconcileDelete(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}), "nothing gates this teardown, so it releases in one pass")
		g.Expect(apierrors.IsNotFound(c.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue())

		expectSwept(t, c, binding)
	})

	t.Run("leaves a foreign binding of the same name alone", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		s := namespaceTeardownScheme(t)

		cp := colocatedBarbicanControlPlane(g)
		foreign := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name: barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace()),
		}}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		g.Expect(c.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, cp)).To(Succeed())

		_, err := r.reconcileDelete(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())

		expectPresent(t, c, foreign)
	})
}

// TestSweepExternalNamespaceResidue_RemovesTheBarbicanResidue covers the External
// lifecycle, where the namespace survives the ControlPlane so nothing cascades and
// every object has to be named: the Barbican child and its secret store, the whole
// dedicated OpenBao ensemble (the cluster-scoped auth-delegator binding included),
// and the DB-credential material.
func TestSweepExternalNamespaceResidue_RemovesTheBarbicanResidue(t *testing.T) {
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleExternal)
	ns := barbicanTeardownNamespace

	child := &barbicanv1alpha1.Barbican{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	store := &barbicanv1alpha1.BarbicanSecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanSecretStoreName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	instance := &openbaov1alpha1.OpenBaoCluster{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanOpenBaoName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	// The DB-credential material, in the same four shapes as Glance's and
	// Placement's: the ExternalSecret, the Dynamic-mode generator, its mTLS client
	// Certificate, and the ServiceAccount whose token it authenticates with.
	dbES := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanDBCredentialSecretName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	dbVDS := &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanDBCredentialSecretName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	dbCert := ownedBarbicanCertificate(cp, barbicanDBCredentialClientCertName(cp))
	dbSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanDBCredentialServiceAccountName, Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}

	residue := append([]client.Object{child, store, instance, dbES, dbVDS, dbCert, dbSA},
		ownedBarbicanEnsemble(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append([]client.Object{cp}, residue...)...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	r.sweepExternalNamespaceResidue(ctx, c, cp, ns)
	expectSwept(t, c, residue...)
}

// neutronTeardownNamespace is the namespace the Neutron fixtures below place the
// network service in.
const neutronTeardownNamespace = "network"

// deletingNeutronControlPlane returns a deleting ControlPlane whose network
// service lives in a namespace of its own, under the given lifecycle.
func deletingNeutronControlPlane(
	deletionAge time.Duration, lifecycle c5c3v1alpha1.ServiceNamespaceLifecycle,
) *c5c3v1alpha1.ControlPlane {
	cp := deletingControlPlane(deletionAge)
	cp.Spec.Services.Neutron = &c5c3v1alpha1.ServiceNeutronSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: neutronTeardownNamespace, Lifecycle: lifecycle,
		},
		OVN: c5c3v1alpha1.NeutronOVNSpec{
			CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn", Namespace: "ovn-system"},
		},
	}
	return cp
}

// TestSweepExternalNamespaceResidue_RemovesTheNeutronResidue covers the External
// lifecycle for the network service, where the namespace survives the ControlPlane
// so nothing cascades and every object has to be named: the DB-credential material
// in the same four shapes as Glance's, plus the bus delivery beside it, the
// transport-URL Secret and the CA mirror. A same-named Secret this ControlPlane
// never wrote is left alone.
func TestSweepExternalNamespaceResidue_RemovesTheNeutronResidue(t *testing.T) {
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingNeutronControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleExternal)
	ns := neutronTeardownNamespace

	dbES := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: neutronDBCredentialSecretName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	dbVDS := &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
		Name: neutronDBCredentialSecretName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	dbCert := &unstructured.Unstructured{}
	dbCert.SetGroupVersionKind(certificateGVK)
	dbCert.SetName(neutronDBCredentialClientCertName(cp))
	dbCert.SetNamespace(ns)
	dbCert.SetLabels(controlPlaneChildLabels(cp))
	dbSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: neutronDBCredentialServiceAccountName, Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	bus := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: neutronMessagingSecretName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	busCA := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: neutronMessagingCASecretName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}

	residue := []client.Object{dbES, dbVDS, dbCert, dbSA, bus, busCA}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append([]client.Object{cp}, residue...)...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	r.sweepExternalNamespaceResidue(ctx, c, cp, ns)
	expectSwept(t, c, residue...)

	// A Secret at the derived transport-URL name that carries none of our labels
	// belongs to somebody else in this shared namespace and survives.
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: neutronMessagingSecretName(cp), Namespace: ns,
	}}
	c2 := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r2 := &ControlPlaneReconciler{Client: c2, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	r2.sweepExternalNamespaceResidue(ctx, c2, cp, ns)
	expectPresent(t, c2, foreign)
}

// --- placed-namespace teardown ---

const (
	// placedTeardownNamespace is the namespace the fixtures below place Keystone
	// in, and placedTeardownCluster the target cluster that namespace lives on.
	placedTeardownNamespace = "identity"
	placedTeardownCluster   = "remote-a"
)

// deletingPlacedControlPlane returns a deleting ControlPlane that places its
// Keystone — and with it the namespace, the backing services, and the credential
// material scoped to that namespace — on a target cluster, under the given
// lifecycle.
func deletingPlacedControlPlane(
	deletionAge time.Duration, lifecycle c5c3v1alpha1.ServiceNamespaceLifecycle,
) *c5c3v1alpha1.ControlPlane {
	cp := deletingControlPlane(deletionAge)
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: placedTeardownNamespace, Lifecycle: lifecycle,
		},
		TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: placedTeardownCluster},
	}
	cp.Finalizers = []string{controlPlaneORCFinalizer, commonmulticluster.RemoteChildrenFinalizer}
	return cp
}

// onTarget stamps obj with the labels a child written to a TARGET cluster carries
// — the owner triple the shared sweep selects on plus this operator's
// cross-namespace pair — the way claimChildOwnership leaves it. A remote child has
// no owner reference, so its labels are the whole of its identity.
func onTarget(cp *c5c3v1alpha1.ControlPlane, obj client.Object) client.Object {
	obj.SetLabels(remoteChildLabels(cp))
	return obj
}

// abandonImmediately compresses the abandon window to nothing for the duration of
// one test, so an unresolvable cluster is given up on in the first pass instead of
// after five minutes of wall clock. It is the package-level knob
// internal/common/multicluster documents for exactly this.
func abandonImmediately(t *testing.T) {
	t.Helper()
	previous := commonmulticluster.AbandonAfter
	commonmulticluster.AbandonAfter = 0
	t.Cleanup(func() { commonmulticluster.AbandonAfter = previous })
}

// placedNamespaceOnTarget builds the namespace reconcileNamespaces creates on a
// TARGET cluster: the ownership labels every remote child carries, plus the UID
// annotation that is the only mark distinguishing this ControlPlane from a
// same-named one owned by another management cluster.
func placedNamespaceOnTarget(cp *c5c3v1alpha1.ControlPlane, name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Labels:      remoteChildLabels(cp),
		Annotations: map[string]string{controlPlaneUIDAnnotation: string(cp.UID)},
	}}
}

// TestTeardownDedicatedNamespaces_SweepsThePlacedNamespaceOnItsTarget is the
// central guard on the placed teardown: nothing on a target cluster collects what
// the ControlPlane wrote there — no owner reference and no garbage collection
// cascade crosses a cluster boundary — so every kind of
// controlPlaneRemoteChildKinds the ControlPlane owns in that namespace is deleted
// by name, on that cluster, and the Managed namespace goes with it on BOTH
// clusters, because reconcileNamespaces created it on both. An object in the same
// namespace that carries none of our labels is nobody's child and survives.
func TestTeardownDedicatedNamespaces_SweepsThePlacedNamespaceOnItsTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ns := placedTeardownNamespace

	ours := []client.Object{
		onTarget(cp, &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: ns}}),
		onTarget(cp, &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: ns}}),
		onTarget(cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
			Name: adminPasswordSecretName(cp), Namespace: ns,
		}}),
		onTarget(cp, &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
			Name: dbCredentialSecretName(cp), Namespace: ns,
		}}),
		onTarget(cp, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: esoTenantServiceAccountName, Namespace: ns,
		}}),
		onTarget(cp, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: ns}}),
	}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: ns}}
	remoteNS := placedNamespaceOnTarget(cp, ns)
	// The same namespace at home, where it carries the two cross-namespace labels a
	// local child is stamped with.
	localNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: controlPlaneChildLabels(cp)}}

	target := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append(append([]client.Object{}, ours...), foreign, remoteNS)...).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, localNS).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue(), "a resolvable cluster is swept in one pass")

	expectSwept(t, target, ours...)
	expectPresent(t, target, foreign)
	expectSwept(t, target, remoteNS)
	expectSwept(t, local, localNS)
}

// TestTeardownDedicatedNamespaces_PlacedExternalNamespaceKeepsTheTrioLast covers
// the other lifecycle: an External namespace survives the ControlPlane on both
// clusters, so its residue is named and deleted on the cluster it lives on — and
// the ORDER is what the named sweep is for, with the tenant store trio going after
// everything that authenticated through it.
func TestTeardownDedicatedNamespaces_PlacedExternalNamespaceKeepsTheTrioLast(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleExternal)
	ns := placedTeardownNamespace

	var deletes []string
	residue := []client.Object{
		onTarget(cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
			Name: adminPasswordSecretName(cp), Namespace: ns,
		}}),
		onTarget(cp, &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: ns}}),
		onTarget(cp, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: esoTenantServiceAccountName, Namespace: ns,
		}}),
	}
	remoteNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	target := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append(append([]client.Object{}, residue...), remoteNS)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes = append(deletes, obj.GetName())
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	expectSwept(t, target, residue...)
	expectPresent(t, target, remoteNS)
	g.Expect(deletes).To(ContainElements(adminPasswordSecretName(cp), esoTenantStoreName))
	g.Expect(slices.Index(deletes, adminPasswordSecretName(cp))).
		To(BeNumerically("<", slices.Index(deletes, esoTenantStoreName)),
			"the credential material must be deleted before the store it authenticates through")
}

// TestReconcileDelete_SweepsPlacedPushSecretsBeforeThatClustersStore is the
// per-cluster half of the OpenBao-orphan guard: a PushSecret carries
// DeletionPolicy=Delete, and ESO can only purge its OpenBao path while the tenant
// store IN ITS OWN NAMESPACE — on its own cluster — is alive. The teardown
// therefore deletes the placed cluster's PushSecrets and holds the ControlPlane
// until they are gone, so the store is still standing while ESO works.
func TestReconcileDelete_SweepsPlacedPushSecretsBeforeThatClustersStore(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ns := placedTeardownNamespace

	// Held by ESO's finalizer, so the Delete leaves it Terminating rather than gone.
	push := onTarget(cp, &esov1alpha1.PushSecret{ObjectMeta: metav1.ObjectMeta{
		Name: "cp-service-account-nova-backup", Namespace: ns,
		Finalizers: []string{"pushsecret.externalsecrets.io/finalizer"},
	}})
	store := onTarget(cp, &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantStoreName, Namespace: ns,
	}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(push, store).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"the teardown must wait for ESO to finish the OpenBao cleanup on the target")

	live := &esov1alpha1.PushSecret{}
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(push), live)).To(Succeed())
	g.Expect(live.DeletionTimestamp.IsZero()).To(BeFalse(),
		"the placed PushSecret must be deleted by the teardown, not left to a cascade that never reaches it")
	expectPresent(t, target, store)

	// ESO finishes and releases the PushSecret; the store may go now.
	live.Finalizers = nil
	g.Expect(target.Update(ctx, live)).To(Succeed())
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err = r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	expectSwept(t, target, store)
	g.Expect(apierrors.IsNotFound(local.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue(),
		"both finalizers must be released once the placed namespace is swept")
}

// TestDeleteBarbicanAuthDelegatorBinding_DeletesItOnTheServicesCluster pins the
// one child that lives outside every namespace. It is cluster-scoped, so neither
// the label-selected sweep (which lists one namespace) nor a namespace deletion
// reclaims it — and it was written on the cluster Barbican was placed on, so that
// is where it has to be deleted. A binding of the same name on the management
// cluster is a different object and must survive, even carrying our labels.
func TestDeleteBarbicanAuthDelegatorBinding_DeletesItOnTheServicesCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	cp.Spec.Services.Barbican.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: placedTeardownCluster}
	name := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace())

	placed := onTarget(cp, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
	atHome := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: controlPlaneChildLabels(cp),
	}}
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placed).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, atHome).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	g.Expect(r.deleteBarbicanAuthDelegatorBinding(ctx, cp)).To(Succeed())
	expectSwept(t, target, placed)
	expectPresent(t, local, atHome)
}

// TestDeleteBarbicanAuthDelegatorBinding_ReleasesWhenTheTargetDeniesTheDelete
// closes the second half of the same wedge the unconditional get closes. The
// target's access chart grants get on ClusterRoleBindings always but create,
// patch and delete only behind authDelegatorBinding, so a cluster that had the
// flag on when the binding was written and has it off now answers the read with
// the binding and the delete with a 403 — every pass, with no stall breaker left
// on this path. Returning that error would hold the ControlPlane in Terminating
// forever, so the binding is left standing and named instead.
func TestDeleteBarbicanAuthDelegatorBinding_ReleasesWhenTheTargetDeniesTheDelete(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	cp.Spec.Services.Barbican.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: placedTeardownCluster}
	name := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace())

	placed := onTarget(cp, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placed).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(rbacv1.Resource("clusterrolebindings"), obj.GetName(), nil)
			},
		}).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme: s, Recorder: rec, Resolver: &childrenResolver{children: target},
	}

	g.Expect(r.deleteBarbicanAuthDelegatorBinding(ctx, cp)).To(Succeed(),
		"a denied delete must release the ControlPlane rather than wedge it in Terminating")
	expectPresent(t, target, placed)

	events := strings.Join(drainEvents(rec), "\n")
	g.Expect(events).To(ContainSubstring("AuthDelegatorBindingNotReclaimed"))
	g.Expect(events).To(ContainSubstring(name), "the event must name the binding left behind")
	g.Expect(events).To(ContainSubstring("authDelegatorBinding=true"),
		"and the grant that would have let the teardown reclaim it")
}

// Any other denial is a real failure the teardown may retry: only the cluster
// withholding the grant is unrecoverable, and swallowing the rest would release
// the ControlPlane over a conflict or an outage that the next pass would clear.
func TestDeleteBarbicanAuthDelegatorBinding_KeepsHoldingOnOtherDeleteErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	cp.Spec.Services.Barbican.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: placedTeardownCluster}
	name := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace())

	placed := onTarget(cp, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placed).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewConflict(rbacv1.Resource("clusterrolebindings"), obj.GetName(), nil)
			},
		}).Build()
	r := &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	g.Expect(r.deleteBarbicanAuthDelegatorBinding(ctx, cp)).NotTo(Succeed())
}

// TestReconcileDelete_HoldsBothFinalizersUntilThePlacedNamespaceIsSwept pins the
// release order: while a service child of the placed namespace is still
// Terminating behind its own operator's cleanup, neither finalizer may go — the
// service operator's own remote sweep is what that wait is for — and once it is
// gone both are released in the same pass.
func TestReconcileDelete_HoldsBothFinalizersUntilThePlacedNamespaceIsSwept(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ns := placedTeardownNamespace

	// The Keystone CR lives on the MANAGEMENT cluster whatever cluster it places
	// its own children on, held by its cleanup finalizer.
	keystone := &keystonev1alpha1.Keystone{ObjectMeta: metav1.ObjectMeta{
		Name: keystoneName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
		Finalizers: []string{"keystone.openstack.c5c3.io/cleanup"},
	}}
	placedChild := onTarget(cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordSecretName(cp), Namespace: ns,
	}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placedChild).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, keystone).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))
	expectPresent(t, target, placedChild)

	held := &c5c3v1alpha1.ControlPlane{}
	g.Expect(local.Get(ctx, key, held)).To(Succeed())
	g.Expect(held.Finalizers).To(ConsistOf(controlPlaneORCFinalizer, commonmulticluster.RemoteChildrenFinalizer))

	// The keystone-operator finishes its own teardown, remote sweep included.
	live := &keystonev1alpha1.Keystone{}
	g.Expect(local.Get(ctx, client.ObjectKeyFromObject(keystone), live)).To(Succeed())
	live.Finalizers = nil
	g.Expect(local.Update(ctx, live)).To(Succeed())

	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err = r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	expectSwept(t, target, placedChild)
	g.Expect(apierrors.IsNotFound(local.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue())
}

// TestTeardownDedicatedNamespaces_WaitsForAnUnresolvedTargetCluster covers the
// first of the two answers an unresolvable cluster gets. Engagement is
// asynchronous, so right after an operator restart a registered cluster looks
// exactly like a deregistered one: within the abandon window the teardown waits,
// keeping the finalizers, rather than releasing a ControlPlane whose children are
// running on a cluster that is about to answer.
func TestTeardownDedicatedNamespaces_WaitsForAnUnresolvedTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: rec,
		Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred(), "an unresolvable cluster must never fail the deletion pass")
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	held := &c5c3v1alpha1.ControlPlane{}
	g.Expect(local.Get(ctx, key, held)).To(Succeed())
	g.Expect(held.Finalizers).To(ConsistOf(controlPlaneORCFinalizer, commonmulticluster.RemoteChildrenFinalizer))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNamespacesReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(drainEvents(rec)).NotTo(ContainElement(ContainSubstring("RemoteChildrenAbandoned")),
		"a cluster that may still be engaging must not be given up on")
}

// TestTeardownDedicatedNamespaces_AbandonsAnUnresolvedTargetClusterPastTheWindow
// covers the other answer. A cluster that has not resolved for the whole abandon
// window is deregistered as far as this operator can tell: its children are
// unreachable either way, so they are left running, a Warning records that they
// were, and the finalizers are released — holding the ControlPlane in Terminating
// forever would help nobody.
func TestTeardownDedicatedNamespaces_AbandonsAnUnresolvedTargetClusterPastTheWindow(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)
	abandonImmediately(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: rec,
		Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	g.Expect(apierrors.IsNotFound(local.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue(),
		"an abandoned cluster must not strand the ControlPlane in Terminating")
	events := strings.Join(drainEvents(rec), "\n")
	g.Expect(events).To(ContainSubstring("RemoteChildrenAbandoned"))
	g.Expect(events).To(ContainSubstring(placedTeardownCluster))
	g.Expect(events).To(ContainSubstring(placedTeardownNamespace))
}

// TestTeardownDedicatedNamespaces_AbandonStillReapsTheNamespaceAtHome pins the
// half of the abandon path that IS reachable. A placed Managed namespace exists
// on both clusters, and the target's copy is unreclaimable once its cluster is
// given up on — but the management cluster's is reachable, ours, and nothing
// comes back for it after both finalizers are released.
func TestTeardownDedicatedNamespaces_AbandonStillReapsTheNamespaceAtHome(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)
	abandonImmediately(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	localNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: placedTeardownNamespace, Labels: controlPlaneChildLabels(cp),
	}}
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, localNS).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue(), "an abandoned cluster must not hold the release")
	expectSwept(t, local, localNS)
}

// TestDeleteManagedNamespace_RefusesAForeignManagementClustersNamespace is the
// guard on the one delete this operator makes on a cluster it does not own. The
// ownership labels name a ControlPlane by name and namespace only, and a target
// cluster may be registered by any number of management clusters — each able to
// run a ControlPlane called "openstack" in namespace "openstack", the quickstart
// defaults, and to place a service in a namespace called "identity". The UID
// stamped at creation is the one mark that tells them apart, and what it stops is
// one teardown cascading the other's database, PVC and tenant store away.
func TestDeleteManagedNamespace_RefusesAForeignManagementClustersNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	// Same name, same namespace, same labels — a different management cluster's CR.
	theirs := placedNamespaceOnTarget(cp, placedTeardownNamespace)
	theirs.Annotations[controlPlaneUIDAnnotation] = "another-management-clusters-cp-uid"

	target := fake.NewClientBuilder().WithScheme(s).WithObjects(theirs).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: rec,
		Resolver: &childrenResolver{children: target},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue(), "refusing the namespace must not wedge the release")
	expectPresent(t, target, theirs)
	g.Expect(strings.Join(drainEvents(rec), "\n")).To(ContainSubstring("NamespaceNotOwned"))
}

// TestDeleteManagedNamespace_StillReapsANamespaceWhoseMarkWasStripped is the
// counterpart of that guard. What proves the namespace is somebody else's is a
// mark naming somebody else — the annotation is an ordinary, mutable annotation
// on a cluster this operator does not own, so a mutating policy or an annotation
// pruner can take it off. Reading that as "not ours" would leak the namespace,
// and everything in it, permanently: nothing else ever comes back for it once the
// finalizers are released.
func TestDeleteManagedNamespace_StillReapsANamespaceWhoseMarkWasStripped(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	stripped := placedNamespaceOnTarget(cp, placedTeardownNamespace)
	stripped.Annotations = nil

	target := fake.NewClientBuilder().WithScheme(s).WithObjects(stripped).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())
	expectSwept(t, target, stripped)
}

// TestDeleteManagedNamespace_DecidesFromTheUncachedReader pins which of the target
// cluster's two readers answers the ownership question. The verdict authorises
// deleting a whole namespace — the service's database, its PVC, its tenant store
// — so it may not be read from an informer that trails the API server: here the
// cache still holds the pre-adoption, unlabelled copy while the live cluster has
// the one the operator created and owns.
func TestDeleteManagedNamespace_DecidesFromTheUncachedReader(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	stale := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: placedTeardownNamespace}}
	live := placedNamespaceOnTarget(cp, placedTeardownNamespace)

	target := fake.NewClientBuilder().WithScheme(s).WithObjects(stale).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{
			children: target,
			reader:   fake.NewClientBuilder().WithScheme(s).WithObjects(live).Build(),
		},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())
	expectSwept(t, target, stale)
}

// TestReconcileDelete_StallEscapeKeepsTheRemoteChildrenFinalizer guards the one
// release the escape may not make. It gives up on K-ORC without ever reaching the
// namespace sweep, so the children on the placed cluster are still standing: the
// remote-children finalizer stays on, and the next pass finishes the sweep and
// releases it.
func TestReconcileDelete_StallEscapeKeepsTheRemoteChildrenFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(orcTeardownDeadline+time.Minute,
		c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	// A managed K-ORC CR wedged behind a finalizer K-ORC can no longer run.
	wedged := &orcv1alpha1.ApplicationCredential{ObjectMeta: terminatingImportMeta(
		adminAppCredentialName(cp), childNamespace(cp), "openstack.k-orc.cloud/applicationcredential")}
	placedChild := onTarget(cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordSecretName(cp), Namespace: placedTeardownNamespace,
	}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placedChild).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, wedged).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: rec,
		Resolver: &childrenResolver{children: target},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	_, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(strings.Join(drainEvents(rec), "\n")).To(ContainSubstring("ORCTeardownStalled"))

	escaped := &c5c3v1alpha1.ControlPlane{}
	g.Expect(local.Get(ctx, key, escaped)).To(Succeed())
	g.Expect(escaped.Finalizers).To(ConsistOf(commonmulticluster.RemoteChildrenFinalizer),
		"the escape gave up on K-ORC, not on the children the ControlPlane placed")
	expectPresent(t, target, placedChild)

	// The next pass runs the sweep the escape never reached.
	res, err := r.reconcileDelete(ctx, escaped)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	expectSwept(t, target, placedChild)
	g.Expect(apierrors.IsNotFound(local.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue())
}

// TestSweepRegistrationTenantStores_CollectsTheForeignTrio a tenant-store trio the
// ControlPlane provisioned in an allowlisted registration namespace is collected
// before the finalizer is released. Nothing else can reach it: it sits outside
// every namespace the dedicated-namespace teardown walks, and a cross-namespace
// child carries no owner reference for a GC cascade to follow.
func TestSweepRegistrationTenantStores_CollectsTheForeignTrio(t *testing.T) {
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := allowlistingControlPlane("tenant-a")
	store, cert, sa := ownedTenantTrioIn("tenant-a", cp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, store, cert, sa).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, APIReader: c, Recorder: record.NewFakeRecorder(10)}

	r.sweepRegistrationTenantStores(ctx, cp)

	expectSwept(t, c, store, cert, sa)
}

// TestSweepRegistrationTenantStores_LeavesForeignObjectsAlone the sweep is
// ownership-checked against live state, so a same-named store somebody else runs in
// a namespace we happen to share survives the teardown.
func TestSweepRegistrationTenantStores_LeavesForeignObjectsAlone(t *testing.T) {
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := allowlistingControlPlane("tenant-a")
	// Same names, no ownership labels: not ours.
	foreignStore := readyTenantSecretStore(esoTenantStoreName, "tenant-a", "", "")
	foreignSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantServiceAccountName, Namespace: "tenant-a",
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreignStore, foreignSA).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, APIReader: c, Recorder: record.NewFakeRecorder(10)}

	r.sweepRegistrationTenantStores(ctx, cp)

	expectPresent(t, c, foreignStore, foreignSA)
}

// TestSweepRegistrationTenantStores_LeavesTheOwnNamespacesAlone the sweep walks
// only namespaces OUTSIDE the ones the ControlPlane occupies. The store in its own
// namespace is owner-referenced and reaped by the GC cascade, and a dedicated
// service namespace is the dedicated-namespace teardown's to sweep, in its own
// order — collecting either here would race that order.
func TestSweepRegistrationTenantStores_LeavesTheOwnNamespacesAlone(t *testing.T) {
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := allowlistingControlPlane("tenant-a")
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: "identity", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
	}
	store, cert, sa := ownedTenantTrioIn("identity", cp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, store, cert, sa).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, APIReader: c, Recorder: record.NewFakeRecorder(10)}

	r.sweepRegistrationTenantStores(ctx, cp)

	expectPresent(t, c, store, cert, sa)
}

// TestSweepNamespacesBeforeRelease_CollectsRegistrationTenantStores pins the
// WIRING, not just the sweep: the release gate every teardown path funnels through
// has to reach the registration tenant stores, or the trios stay behind in
// namespaces nothing points back from once the ControlPlane leaves etcd.
func TestSweepNamespacesBeforeRelease_CollectsRegistrationTenantStores(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingControlPlane(time.Minute)
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
		AllowedNamespaces: []string{"tenant-a"},
	}
	store, cert, sa := ownedTenantTrioIn("tenant-a", cp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, store, cert, sa).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, APIReader: c, Recorder: record.NewFakeRecorder(10)}

	done, err := r.sweepNamespacesBeforeRelease(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	expectSwept(t, c, store, cert, sa)
}
