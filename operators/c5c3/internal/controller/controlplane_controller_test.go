// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the ControlPlane controller skeleton.
package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// controllerTestScheme returns a runtime.Scheme with the c5c3 ControlPlane type
// registered alongside the core client-go types.
func controllerTestScheme(t *testing.T) *runtime.Scheme {
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

// trueCondition returns a metav1.Condition of the given type with status True.
func trueCondition(condType string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "ready",
		LastTransitionTime: metav1.Now(),
	}
}

func TestAggregateReady_AllTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	conds := make([]metav1.Condition, 0, len(subConditionTypes))
	for _, ct := range subConditionTypes {
		conds = append(conds, trueCondition(ct))
	}

	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeTrue(),
		"aggregate Ready must be true when all sub-conditions are True")
}

func TestAggregateReady_MissingCondition(t *testing.T) {
	g := NewGomegaWithT(t)

	// Drop the last sub-condition so one is missing entirely.
	conds := make([]metav1.Condition, 0, len(subConditionTypes)-1)
	for _, ct := range subConditionTypes[:len(subConditionTypes)-1] {
		conds = append(conds, trueCondition(ct))
	}

	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeFalse(),
		"aggregate Ready must be false when a sub-condition is missing")
}

func TestAggregateReady_OneFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	conds := make([]metav1.Condition, 0, len(subConditionTypes))
	for i, ct := range subConditionTypes {
		c := trueCondition(ct)
		if i == 0 {
			c.Status = metav1.ConditionFalse
		}
		conds = append(conds, c)
	}

	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeFalse(),
		"aggregate Ready must be false when any sub-condition is False")
}

// TestSetServicesStatus_WritesPhaseAndKeystoneReadiness verifies that
// setServicesStatus populates the previously-unwritten status.updatePhase (fixed
// at Idle) and the status.services entry named "keystone", deriving the service
// readiness from the KeystoneReady sub-condition and the release from spec (#476).
func TestSetServicesStatus_WritesPhaseAndKeystoneReadiness(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services:         c5c3v1alpha1.ServicesSpec{Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{}},
		},
	}

	// KeystoneReady absent => service reported not Ready.
	setServicesStatus(cp)
	g.Expect(cp.Status.UpdatePhase).To(Equal(c5c3v1alpha1.UpdatePhaseIdle))
	svc := findServiceStatus(cp.Status.Services, "keystone")
	g.Expect(svc).NotTo(BeNil(), "status.services must report the projected keystone service")
	g.Expect(svc.Name).To(Equal("keystone"))
	g.Expect(svc.Ready).To(BeFalse(), "keystone service must be not Ready while KeystoneReady is absent")
	g.Expect(svc.Release).To(Equal("2025.2"))

	// KeystoneReady True => service reported Ready.
	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeKeystoneReady))
	setServicesStatus(cp)
	svc = findServiceStatus(cp.Status.Services, "keystone")
	g.Expect(svc).NotTo(BeNil(), "status.services must still report the projected keystone service")
	g.Expect(svc.Ready).To(BeTrue(),
		"keystone service must be Ready once KeystoneReady is True")
}

// TestSetServicesStatus_GlanceEntry extends the status projection to the third
// service: services.glance produces a "glance" entry whose readiness tracks the
// GlanceReady sub-condition, and an unmanaged Glance is omitted.
func TestSetServicesStatus_GlanceEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:  &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:   &c5c3v1alpha1.ServiceGlanceSpec{},
			},
		},
	}

	setServicesStatus(cp)
	g.Expect(cp.Status.Services).To(HaveLen(3))
	// Stable order: keystone, horizon, glance.
	g.Expect(cp.Status.Services[2].Name).To(Equal("glance"))
	g.Expect(cp.Status.Services[2].Ready).To(BeFalse(),
		"glance is not Ready while GlanceReady is absent")
	g.Expect(cp.Status.Services[2].Release).To(Equal("2025.2"))

	// GlanceReady True flips only the glance entry.
	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeGlanceReady))
	setServicesStatus(cp)
	glance := findServiceStatus(cp.Status.Services, "glance")
	g.Expect(glance).NotTo(BeNil())
	g.Expect(glance.Ready).To(BeTrue())

	// An unmanaged glance is omitted rather than reported.
	cp.Spec.Services.Glance = nil
	setServicesStatus(cp)
	g.Expect(cp.Status.Services).To(HaveLen(2))
	g.Expect(findServiceStatus(cp.Status.Services, "glance")).To(BeNil())
}

// TestSetServicesStatus_PlacementEntry extends the status projection to the
// fourth service: services.placement produces a "placement" entry whose readiness
// tracks the PlacementReady sub-condition, and an unmanaged Placement is omitted.
func TestSetServicesStatus_PlacementEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:   &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
			},
		},
	}

	setServicesStatus(cp)
	g.Expect(cp.Status.Services).To(HaveLen(4))
	// Stable order: keystone, horizon, glance, placement.
	g.Expect(cp.Status.Services[3].Name).To(Equal("placement"))
	g.Expect(cp.Status.Services[3].Ready).To(BeFalse(),
		"placement is not Ready while PlacementReady is absent")
	g.Expect(cp.Status.Services[3].Release).To(Equal("2025.2"))

	// PlacementReady True flips only the placement entry.
	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypePlacementReady))
	setServicesStatus(cp)
	placement := findServiceStatus(cp.Status.Services, "placement")
	g.Expect(placement).NotTo(BeNil())
	g.Expect(placement.Ready).To(BeTrue())
	g.Expect(findServiceStatus(cp.Status.Services, "glance").Ready).To(BeFalse(),
		"PlacementReady must not flip a peer service entry")

	// An unmanaged placement is omitted rather than reported, and the remaining
	// entries keep their positions.
	cp.Spec.Services.Placement = nil
	setServicesStatus(cp)
	g.Expect(cp.Status.Services).To(HaveLen(3))
	g.Expect(findServiceStatus(cp.Status.Services, "placement")).To(BeNil())
	g.Expect(cp.Status.Services[2].Name).To(Equal("glance"))
}

// TestSetServicesStatus_BarbicanEntry extends the status projection to the fifth
// service: services.barbican produces a "barbican" entry whose readiness tracks the
// BarbicanReady sub-condition, and an unmanaged Barbican is omitted. The entry is
// appended LAST, so the four established positions keep their meaning.
func TestSetServicesStatus_BarbicanEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:   &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
				Barbican:  &c5c3v1alpha1.ServiceBarbicanSpec{},
			},
		},
	}

	setServicesStatus(cp)
	g.Expect(cp.Status.Services).To(HaveLen(5))
	// Stable order: keystone, horizon, glance, placement, barbican.
	g.Expect(cp.Status.Services[4].Name).To(Equal("barbican"))
	g.Expect(cp.Status.Services[4].Ready).To(BeFalse(),
		"barbican is not Ready while BarbicanReady is absent")
	g.Expect(cp.Status.Services[4].Release).To(Equal("2025.2"))

	// BarbicanReady True flips only the barbican entry.
	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeBarbicanReady))
	setServicesStatus(cp)
	barbican := findServiceStatus(cp.Status.Services, "barbican")
	g.Expect(barbican).NotTo(BeNil())
	g.Expect(barbican.Ready).To(BeTrue())
	g.Expect(findServiceStatus(cp.Status.Services, "placement").Ready).To(BeFalse(),
		"BarbicanReady must not flip a peer service entry")

	// An unmanaged barbican is omitted rather than reported, and the remaining
	// entries keep their positions. The webhook's shared/dedicated transition
	// freeze reads status.services[].name, so a dropped service disappearing from
	// the list is what eventually reopens its CREATE path (serviceDeclaredBefore).
	cp.Spec.Services.Barbican = nil
	setServicesStatus(cp)
	g.Expect(cp.Status.Services).To(HaveLen(4))
	g.Expect(findServiceStatus(cp.Status.Services, "barbican")).To(BeNil())
	g.Expect(cp.Status.Services[3].Name).To(Equal("placement"))
}

// TestSetServicesStatus_NeutronEntry extends the status projection to the sixth
// service: services.neutron produces a "neutron" entry whose readiness tracks the
// NeutronReady sub-condition, and an unmanaged Neutron is omitted. The entry is
// appended LAST, so the five established positions keep their meaning.
func TestSetServicesStatus_NeutronEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:   &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
				Barbican:  &c5c3v1alpha1.ServiceBarbicanSpec{},
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"},
					},
				},
			},
		},
	}

	setServicesStatus(cp)
	g.Expect(cp.Status.Services).To(HaveLen(6))
	// Stable order: keystone, horizon, glance, placement, barbican, neutron.
	g.Expect(cp.Status.Services[5].Name).To(Equal("neutron"))
	g.Expect(cp.Status.Services[5].Ready).To(BeFalse(),
		"neutron is not Ready while NeutronReady is absent")
	g.Expect(cp.Status.Services[5].Release).To(Equal("2025.2"))

	// A False NeutronReady keeps the entry not-ready; only True flips it, and only
	// the neutron entry.
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:    conditionTypeNeutronReady,
		Status:  metav1.ConditionFalse,
		Reason:  "WaitingForOVN",
		Message: "OVNReady is not True; Neutron projection deferred",
	})
	setServicesStatus(cp)
	g.Expect(findServiceStatus(cp.Status.Services, "neutron").Ready).To(BeFalse())

	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeNeutronReady))
	setServicesStatus(cp)
	neutron := findServiceStatus(cp.Status.Services, "neutron")
	g.Expect(neutron).NotTo(BeNil())
	g.Expect(neutron.Ready).To(BeTrue())
	g.Expect(findServiceStatus(cp.Status.Services, "barbican").Ready).To(BeFalse(),
		"NeutronReady must not flip a peer service entry")

	// An unmanaged neutron is omitted rather than reported, and the remaining
	// entries keep their positions.
	cp.Spec.Services.Neutron = nil
	setServicesStatus(cp)
	g.Expect(cp.Status.Services).To(HaveLen(5))
	g.Expect(findServiceStatus(cp.Status.Services, "neutron")).To(BeNil())
	g.Expect(cp.Status.Services[4].Name).To(Equal("barbican"))
}

// TestAggregateReady_OVNAndNeutronBlockIt pins the two condition types the network
// service contributes to the aggregate vocabulary: with every sub-condition True
// the plane reports Ready, and either of the two turning False takes it back to
// False/NotAllReady.
func TestAggregateReady_OVNAndNeutronBlockIt(t *testing.T) {
	for _, blocked := range []string{conditionTypeOVNReady, conditionTypeNeutronReady} {
		t.Run(blocked, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := &c5c3v1alpha1.ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			}
			for _, ct := range subConditionTypes {
				conditions.SetCondition(&cp.Status.Conditions, trueCondition(ct))
			}
			setReadyCondition(cp)
			ready := conditions.GetCondition(cp.Status.Conditions, conditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

			blocking := trueCondition(blocked)
			blocking.Status = metav1.ConditionFalse
			blocking.Reason = "NotReady"
			conditions.SetCondition(&cp.Status.Conditions, blocking)
			setReadyCondition(cp)

			ready = conditions.GetCondition(cp.Status.Conditions, conditionTypeReady)
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("NotAllReady"))
		})
	}
}

// findServiceStatus returns a pointer to the ServiceStatus entry with the given
// name, or nil when the listType=map status.services list has no such entry.
func findServiceStatus(services []c5c3v1alpha1.ServiceStatus, name string) *c5c3v1alpha1.ServiceStatus {
	for i := range services {
		if services[i].Name == name {
			return &services[i]
		}
	}
	return nil
}

// TestUpdateStatus_SkipsWriteWhenUnchanged locks in the no-op status-write
// skip the ControlPlane controller gained by adopting the shared status
// writer: a converged steady-state pass must not issue Status().Update (no
// write → no watch event → no resourceVersion churn), while a changed status
// still writes. The reconciler's Status().Update is wired to always fail, so
// a skipped write is observable as a nil error return.
func TestUpdateStatus_SkipsWriteWhenUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default", Generation: 3},
	}
	statusErr := fmt.Errorf("status update must not be called on an unchanged status")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return statusErr
			},
		}).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// Bring cp.Status into the exact state updateStatus would compute (Ready
	// aggregated + services projected + ObservedGeneration stamped), then
	// snapshot it — a converged steady-state pass.
	setReadyCondition(cp)
	setServicesStatus(cp)
	cp.Status.ObservedGeneration = cp.Generation
	snapshot := cp.Status.DeepCopy()

	_, err := r.updateStatus(context.Background(), cp, snapshot, ctrl.Result{}, nil)
	g.Expect(err).NotTo(HaveOccurred(),
		"an unchanged status must skip the write; the failing Status().Update proves it was not called")

	// An empty snapshot differs from the computed status, so the (failing)
	// write must be attempted.
	_, err = r.updateStatus(context.Background(), cp, &c5c3v1alpha1.ControlPlaneStatus{}, ctrl.Result{}, nil)
	g.Expect(err).To(HaveOccurred(), "a changed status must attempt the write")
	g.Expect(err.Error()).To(ContainSubstring("updating status:"))
}

func TestReconcile_NotFound_EarlyReturn(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "absent"},
	})

	g.Expect(err).NotTo(HaveOccurred(),
		"Reconcile on a missing ControlPlane must not error")
	g.Expect(res).To(Equal(ctrl.Result{}),
		"Reconcile on a missing ControlPlane must return a zero result")
}

// --- Duplicate-ControlPlane guard tests (defense-in-depth) ---

// duplicateGuardControlPlane returns a minimal ControlPlane with the given
// identity and creation time for the duplicate-guard tests. The guard runs
// before any spec interpretation, so an empty spec is sufficient.
func duplicateGuardControlPlane(name, namespace, uid string, created time.Time) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			UID:               types.UID(uid),
			CreationTimestamp: metav1.NewTime(created),
		},
	}
}

func TestReconcile_DuplicateControlPlane_ParksYounger(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	now := time.Now()
	older := duplicateGuardControlPlane("alpha", "default", "uid-alpha", now.Add(-time.Hour))
	younger := duplicateGuardControlPlane("bravo", "default", "uid-bravo", now)
	// The scheme deliberately registers no child types (MariaDB, Keystone, ...):
	// if the guard ever failed to park the duplicate, the sub-reconciler chain
	// would error on the unregistered kinds and fail this test.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(older, younger).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).
		Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "bravo"},
	})

	g.Expect(err).NotTo(HaveOccurred(), "parking a duplicate must not error")
	g.Expect(res.RequeueAfter).To(Equal(duplicateControlPlaneRequeueAfter),
		"a parked duplicate must requeue so it can take over once the incumbent is gone")

	var parked c5c3v1alpha1.ControlPlane
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "bravo"}, &parked)).To(Succeed())
	ready := conditions.GetCondition(parked.Status.Conditions, conditionTypeReady)
	g.Expect(ready).NotTo(BeNil(), "the parked duplicate must carry a Ready condition")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse),
		"a parked duplicate must report Ready=False")
	g.Expect(ready.Reason).To(Equal("DuplicateControlPlane"),
		"a parked duplicate must report reason DuplicateControlPlane")
	g.Expect(ready.Message).To(ContainSubstring(`"alpha"`),
		"the Ready message must name the incumbent")
	g.Expect(parked.Status.ObservedGeneration).To(Equal(parked.Generation),
		"the parked status must stamp ObservedGeneration")
}

func TestReconcile_DuplicateControlPlane_TieBreakByName(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	created := metav1.Now().Time
	// Identical creation timestamps: the lexically smallest name wins.
	first := duplicateGuardControlPlane("alpha", "default", "uid-alpha", created)
	second := duplicateGuardControlPlane("bravo", "default", "uid-bravo", created)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(first, second).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).
		Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "bravo"},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var parked c5c3v1alpha1.ControlPlane
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "bravo"}, &parked)).To(Succeed())
	ready := conditions.GetCondition(parked.Status.Conditions, conditionTypeReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("DuplicateControlPlane"),
		"on a creation-time tie the lexically larger name must be parked")
	g.Expect(ready.Message).To(ContainSubstring(`"alpha"`),
		"the tie-break incumbent must be the lexically smallest name")
}

func TestDuplicateControlPlaneIncumbent_OldestProceeds(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	now := time.Now()
	older := duplicateGuardControlPlane("alpha", "default", "uid-alpha", now.Add(-time.Hour))
	younger := duplicateGuardControlPlane("bravo", "default", "uid-bravo", now)
	// A ControlPlane in another namespace must not influence the guard.
	unrelated := duplicateGuardControlPlane("older-elsewhere", "other", "uid-other", now.Add(-2*time.Hour))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(older, younger, unrelated).
		Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	incumbent, err := r.duplicateControlPlaneIncumbent(context.Background(), older)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(incumbent).To(BeEmpty(),
		"the oldest ControlPlane in the namespace must proceed")

	incumbent, err = r.duplicateControlPlaneIncumbent(context.Background(), younger)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(incumbent).To(Equal("alpha"),
		"a younger ControlPlane must be parked behind the namespace's oldest")
}

func TestDuplicateControlPlaneIncumbent_SingleControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	only := duplicateGuardControlPlane("solo", "default", "uid-solo", time.Now())
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(only).Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	incumbent, err := r.duplicateControlPlaneIncumbent(context.Background(), only)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(incumbent).To(BeEmpty(),
		"a namespace's only ControlPlane must proceed")
}
