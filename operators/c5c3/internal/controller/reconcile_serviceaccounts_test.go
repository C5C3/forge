// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for reconcileServiceAccounts, which aggregates the readiness of the
// KeystoneService children the built-in service legs project into the
// ServiceAccountsReady condition.
package controller

import (
	"context"
	"errors"
	"regexp"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// runServiceAccounts runs reconcileServiceAccounts against cp with the given
// KeystoneService children seeded on the management cluster.
func runServiceAccounts(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, children ...client.Object,
) (ctrl.Result, error) {
	t.Helper()
	s := korcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append([]client.Object{cp}, children...)...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}
	return r.reconcileServiceAccounts(context.Background(), cp)
}

// serviceAccountsCondition returns the ServiceAccountsReady condition the pass
// left on cp.
func serviceAccountsCondition(t *testing.T, cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	t.Helper()
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeServiceAccountsReady)
	if cond == nil {
		t.Fatalf("no %s condition was set", conditionTypeServiceAccountsReady)
	}
	return cond
}

// TestReconcileServiceAccounts_NoRegistrationsProjected covers the ControlPlane
// that declares no built-in service, which is every External-mode one: the
// condition is set True with its own reason rather than left absent, so the
// status schema does not depend on the spec.
func TestReconcileServiceAccounts_NoRegistrationsProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()

	res, err := runServiceAccounts(t, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonNoServiceRegistrationsProjected))
	g.Expect(cond.Message).To(Equal("no built-in service registrations are projected"))
}

// TestReconcileServiceAccounts_WaitsForAMissingChild covers the enabled service
// whose registration child has not been applied yet: the Glance leg is gated on
// something upstream of its own apply, and until it writes the child there is
// nothing to aggregate. That is a bounded wait, not a failure.
func TestReconcileServiceAccounts_WaitsForAMissingChild(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}

	res, err := runServiceAccounts(t, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(glanceName(cp)))
}

// TestReconcileServiceAccounts_WaitsForAChildWithoutReady covers the child the
// registration controller has not reconciled yet: it carries no condition at
// all, so the aggregate names it under the same bounded wait.
func TestReconcileServiceAccounts_WaitsForAChildWithoutReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}

	res, err := runServiceAccounts(t, cp, glanceRegistration(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(glanceName(cp)))
}

// TestReconcileServiceAccounts_RelaysTheChildCollision pins what the aggregate is
// for: a collision on the service's Keystone user is readable from
// ServiceAccountsReady with the child's own reason and message, without opening
// the registration.
func TestReconcileServiceAccounts_RelaysTheChildCollision(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	const childMessage = `user "glance" already exists in Keystone`
	child := glanceRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  reasonServiceAccountCollision,
		Message: childMessage,
	})

	res, err := runServiceAccounts(t, cp, child)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring(childMessage))
}

// TestReconcileServiceAccounts_RelaysACatalogSideReason covers the aggregate's
// most surprising observable output: keystoneServiceSubConditionTypes puts
// CatalogReady BEFORE AccountReady, so a registration whose Keystone catalog row
// failed reports a catalog reason under a condition named for service accounts.
// That is deliberate — the first failing sub-condition's reason admits a fix,
// where the aggregate's would only say something is wrong — so it is pinned.
func TestReconcileServiceAccounts_RelaysACatalogSideReason(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	const childMessage = `service "image" already exists in the catalog`
	child := glanceRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceCatalogReady,
		Status:  metav1.ConditionFalse,
		Reason:  conditionReasonCatalogFailed,
		Message: childMessage,
	})

	res, err := runServiceAccounts(t, cp, child)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring(childMessage))
}

// TestReconcileServiceAccounts_NamesTheFailingRegistrationNotTheFirst covers the
// loop iterating PAST a healthy registration: with Glance ready and Placement
// failing, the relayed cause must name Placement.
func TestReconcileServiceAccounts_NamesTheFailingRegistrationNotTheFirst(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{}
	const childMessage = `user "placement" already exists in Keystone`
	failing := placementRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  reasonServiceAccountCollision,
		Message: childMessage,
	})

	res, err := runServiceAccounts(t, cp, readyGlanceRegistration(cp), failing)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring(placementName(cp)))
	g.Expect(cond.Message).NotTo(ContainSubstring(glanceName(cp)))
}

// TestReconcileServiceAccounts_ReadErrorIsWrapped covers a read that fails for
// any reason other than absence: it is returned so the group joins it, wrapped
// with which child was being read, and the same text stands in the condition.
func TestReconcileServiceAccounts_ReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	s := korcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return errors.New("boom")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileServiceAccounts(context.Background(), cp)

	g.Expect(err).To(MatchError("reading the glance KeystoneService child: boom"))
	g.Expect(res).To(Equal(ctrl.Result{}))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
	g.Expect(cond.Message).To(Equal("reading the glance KeystoneService child: boom"))
}

// TestReconcileServiceAccounts_AllChildrenReady covers the converged control
// plane: three enabled services, three Ready children, one True condition
// counting them.
func TestReconcileServiceAccounts_AllChildrenReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{}
	cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{}

	res, err := runServiceAccounts(t, cp,
		readyGlanceRegistration(cp), readyPlacementRegistration(cp), readyBarbicanRegistration(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountsProvisioned))
	g.Expect(cond.Message).To(Equal("3 built-in service registration(s) ready"))
}

// TestReconcileServiceAccounts_CountsOnlyEnabledServices pins the count on the
// enabled services rather than on the children present: a control plane running
// Glance alone reports one registration, and the two services it does not
// declare are not waited on.
func TestReconcileServiceAccounts_CountsOnlyEnabledServices(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}

	res, err := runServiceAccounts(t, cp, readyGlanceRegistration(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountsProvisioned))
	g.Expect(cond.Message).To(Equal("1 built-in service registration(s) ready"))
}

// TestReconcileServiceAccounts_CountsNeutron extends the aggregate to the fourth
// built-in registration. The network service is waited on exactly like its peers:
// while its KeystoneService child is missing the aggregate holds and names it, and
// only with the child present does the count reach four.
func TestReconcileServiceAccounts_CountsNeutron(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{}
	cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{}
	cp.Spec.Services.Neutron = &c5c3v1alpha1.ServiceNeutronSpec{
		OVN: c5c3v1alpha1.NeutronOVNSpec{CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"}},
	}

	res, err := runServiceAccounts(t, cp,
		readyGlanceRegistration(cp), readyPlacementRegistration(cp), readyBarbicanRegistration(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(neutronName(cp)))

	res, err = runServiceAccounts(t, cp, readyGlanceRegistration(cp), readyPlacementRegistration(cp),
		readyBarbicanRegistration(cp), readyNeutronRegistration(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	cond = serviceAccountsCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Message).To(Equal("4 built-in service registration(s) ready"))
}

// TestServiceAccountRoleSlug covers the slug normalization and its case-sensitive
// collision resistance. The slug names the Role import and RoleAssignment CRs a
// KeystoneService registration projects per declared role
// (keystoneServiceRoleImportRef), and lives in registration_projection.go.
func TestServiceAccountRoleSlug(t *testing.T) {
	// The readable base plus an 8-hex suffix, at most 25 bytes; the base may be
	// empty for an all-non-alnum role, leaving just "-<hash>".
	shape := regexp.MustCompile(`^[a-z0-9-]{0,16}-[0-9a-f]{8}$`)

	cases := []struct {
		name       string
		role       string
		wantPrefix string
	}{
		{"mixed case lowercases", "Member", "member-"},
		{"unicode and spaces collapse to dashes", "Über Admin", "ber-admin-"},
		{"long base truncates to 16", "verylongrolenamethatexceeds", "verylongrolename-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			slug := serviceAccountRoleSlug(tc.role)
			g.Expect(len(slug)).To(BeNumerically("<=", 25))
			g.Expect(shape.MatchString(slug)).To(BeTrue(), "slug %q must be a name-safe segment", slug)
			g.Expect(slug).To(HavePrefix(tc.wantPrefix))
		})
	}

	g := NewGomegaWithT(t)
	// Two roles differing only by case must not collide: the hash is taken over the
	// ORIGINAL (case-sensitive) role string, so the suffixes differ.
	g.Expect(serviceAccountRoleSlug("Member")).NotTo(Equal(serviceAccountRoleSlug("member")),
		"case-only-different roles must hash to distinct slugs")
}

// TestBuildServiceAccountCloudsYAML_UsesAccountIdentity covers the clouds.yaml
// every account credential is published with (korc_cloudsyaml.go): the account's
// own identity, not the admin one.
func TestBuildServiceAccountCloudsYAML_UsesAccountIdentity(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()

	out := buildServiceAccountCloudsYAML(cp, "nova", "service", "Default", "s3cret", nil)
	g.Expect(out).To(ContainSubstring("username: \"nova\""))
	g.Expect(out).To(ContainSubstring("password: \"s3cret\""))
	g.Expect(out).To(ContainSubstring("project_name: \"service\""))
	g.Expect(out).To(ContainSubstring("user_domain_name: \"Default\""))
	g.Expect(out).To(ContainSubstring("project_domain_name: \"Default\""))
}
