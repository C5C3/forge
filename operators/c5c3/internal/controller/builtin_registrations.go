// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The built-in services a ControlPlane manages register against its identity
// plane through a projected KeystoneService child: one CR per service carrying
// both the catalog entry and the service account, reconciled by the
// KeystoneService controller. The ControlPlane's own sub-reconciler projects
// that child, relays its conditions, and consumes the consumer Secret it
// delivers (keystoneServiceCredentialsSecretName, key "password").
//
// Everything here is service-agnostic: the registration differs per service only
// in the values desiredGlanceRegistration and its siblings hand to
// builtinRegistration, so a further built-in service adds those values and its
// wiring rather than another copy of the ensure, gate and mirror legs.

const (
	// reasonWaitingForServiceRegistration is the bounded wait while the projected
	// registration has not provisioned the service's Keystone account yet. The
	// service child is not projected while it holds: the service would
	// authenticate as a user that does not exist, with a password its consumer
	// Secret does not carry.
	reasonWaitingForServiceRegistration = "WaitingForServiceRegistration"
	// reasonServiceRegistrationError reports a Kubernetes-level failure writing or
	// reading the registration child — a refused adoption of a same-named foreign
	// CR among them.
	reasonServiceRegistrationError = "ServiceRegistrationError"
	// reasonServiceRegistrationFieldsReclaimed reports the pass that reset a spec
	// field another field manager wrote on the registration child. It names the same
	// thing as the Warning event, and outlives it: an event ages out of etcd on the
	// cluster's TTL, while the condition stands until the next pass reads a child
	// that is untampered again.
	reasonServiceRegistrationFieldsReclaimed = "ServiceRegistrationFieldsReclaimed"
)

// builtinRegistration builds the KeystoneService child registering one built-in
// service: its catalog entry (an internal and a public endpoint) and its service
// account, in the namespace the service is assigned to. PURE builder — the
// caller claims ownership and applies it.
//
// spec.controlPlaneRef names the ControlPlane's namespace EXPLICITLY. An empty
// namespace resolves to the CR's own, which for a service placed in a dedicated
// namespace is not the ControlPlane's: the reference would dangle and the
// registration would never reconcile.
//
// Neither adopt flag is set. Both default to the fail-loudly posture — a
// pre-existing Keystone user or catalog row of these names surfaces a collision
// condition instead of being taken over — and assigning false explicitly would
// serialize away under omitempty anyway, leaving the SSA intent free of the
// field either way. Assigning nothing keeps the intent and the semantics in
// agreement — and, because the field is unasserted whichever way it is written,
// the apply can never reclaim an adopt=true another field manager set:
// builtinRegistrationForeignFields is what names one, and
// reclaimBuiltinRegistrationFields is what resets it.
//
// spec.account.domainName stays unset, so the registration resolves the
// ControlPlane's effective admin domain itself. Naming it here would freeze the
// value on the child (it is immutable after creation) against a ControlPlane
// that can still be read for it.
func builtinRegistration(
	cp *c5c3v1alpha1.ControlPlane,
	name, namespace, serviceType, serviceName, internalURL, publicURL, userName, projectName string,
) *c5c3v1alpha1.KeystoneService {
	return &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{
				Name:      cp.Name,
				Namespace: cp.Namespace,
			},
			Catalog: &c5c3v1alpha1.KeystoneServiceCatalogSpec{
				ServiceType: serviceType,
				ServiceName: serviceName,
				Endpoints: []c5c3v1alpha1.KeystoneServiceEndpointSpec{
					{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: internalURL},
					{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: publicURL},
				},
			},
			Account: &c5c3v1alpha1.KeystoneServiceAccountSpec{
				UserName: userName,
				Project: c5c3v1alpha1.ServiceAccountProjectSpec{
					Name:   projectName,
					Create: true,
				},
				Roles: []string{"service"},
			},
		},
	}
}

// desiredGlanceRegistration builds the registration child for Glance: the image
// catalog entry and the "glance" service account.
func desiredGlanceRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	return builtinRegistration(cp, glanceName(cp), cp.GlanceNamespace(), "image", "glance",
		internalCatalogURL(cp.GlanceTargetClusterRef(), glanceEndpointURL(cp), glanceCatalogURL(cp)),
		glanceCatalogURL(cp),
		c5c3v1alpha1.GlanceServiceAccountName, c5c3v1alpha1.GlanceServiceProjectName)
}

// desiredPlacementRegistration builds the registration child for Placement: the
// placement catalog entry and the "placement" service account.
func desiredPlacementRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	return builtinRegistration(cp, placementName(cp), cp.PlacementNamespace(), "placement", "placement",
		internalCatalogURL(cp.PlacementTargetClusterRef(), placementEndpointURL(cp), placementCatalogURL(cp)),
		placementCatalogURL(cp),
		c5c3v1alpha1.PlacementServiceAccountName, c5c3v1alpha1.PlacementServiceProjectName)
}

// desiredBarbicanRegistration builds the registration child for Barbican: the
// key-manager catalog entry and the "barbican" service account.
func desiredBarbicanRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	return builtinRegistration(cp, barbicanName(cp), cp.BarbicanNamespace(), "key-manager", "barbican",
		internalCatalogURL(cp.BarbicanTargetClusterRef(), barbicanEndpointURL(cp), barbicanCatalogURL(cp)),
		barbicanCatalogURL(cp),
		c5c3v1alpha1.BarbicanServiceAccountName, c5c3v1alpha1.BarbicanServiceProjectName)
}

// desiredNeutronRegistration builds the registration child for Neutron: the
// network catalog entry and the "neutron" service account.
func desiredNeutronRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	return builtinRegistration(cp, neutronName(cp), cp.NeutronNamespace(), "network", "neutron",
		internalCatalogURL(cp.NeutronTargetClusterRef(), neutronEndpointURL(cp), neutronCatalogURL(cp)),
		neutronCatalogURL(cp),
		c5c3v1alpha1.NeutronServiceAccountName, c5c3v1alpha1.NeutronServiceProjectName)
}

// reconcileBuiltinRegistration drives the registration leg every built-in
// service shares: it applies the registration child, mirrors the credentials the
// child delivers onto the cluster a placed service runs on, and gates on the
// service account having been provisioned. display is the service's name as it
// reads in a condition ("Glance"), conditionType the condition the caller drives.
//
// It returns the live child plus the caller's own (result, halt, error) triple:
// while halt is true the caller returns res and err verbatim and projects
// nothing, and once it is false the child is non-nil with its account ready.
//
// The write always goes through the LOCAL client, whatever cluster the service
// runs on: a KeystoneService is reconciled on the management cluster, beside the
// ControlPlane whose admin credential it registers through. Only the credentials
// it produces follow the service (see ensureBuiltinRegistrationMirror).
func (r *ControlPlaneReconciler) reconcileBuiltinRegistration(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, desired *c5c3v1alpha1.KeystoneService,
	display, conditionType string,
) (*c5c3v1alpha1.KeystoneService, ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	fail := func(reason, message string) {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
	}

	// The projection's own intent, captured BEFORE the apply: the apply populates
	// desired with the API server's response, so afterwards it carries the live
	// merged spec — foreign fields and all — and comparing the child against it
	// would compare the tampered state with itself.
	projected := desired.Spec.DeepCopy()

	// The ensure errors are relayed verbatim: the refusal to adopt a same-named
	// CR this ControlPlane did not create spells out what it refuses and why, and
	// that text goes straight into the condition.
	if err := r.ensureUnownedOrOwned(ctx, r.Client, cp, desired); err != nil {
		fail(reasonServiceRegistrationError,
			fmt.Sprintf("projecting the %s KeystoneService child: %v", display, err))
		return nil, ctrl.Result{}, true, err
	}

	// Read the live CR back with a fresh Get rather than reusing the apply
	// response, so the conditions gated on below are the ones the registration
	// controller last wrote. A CR that is not readable yet leaves child nil, which
	// is "not registered yet" rather than a failure.
	child := &c5c3v1alpha1.KeystoneService{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), child); err != nil {
		if !apierrors.IsNotFound(err) {
			err = fmt.Errorf("reading the %s KeystoneService child: %w", strings.ToLower(display), err)
			fail(reasonServiceRegistrationError,
				fmt.Sprintf("projecting the %s KeystoneService child: %v", display, err))
			return nil, ctrl.Result{}, true, err
		}
		child = nil
	}

	// Reclaim any assertion this ControlPlane never projected. They are exactly the
	// ones the apply above cannot take back (see builtinRegistrationForeignFields),
	// and leaving them standing is not an option: an undeclared endpoint row is
	// PUBLISHED in the Keystone catalog by the registration's own controller, which
	// this ControlPlane halting does not touch, so every admin-scoped client that
	// resolves it sends its token wherever the row points. An ordinary Update is
	// what reclaims them — field ownership constrains apply requests, not writes —
	// so the operator remediates itself instead of waiting for somebody to read a
	// condition. The pass stops afterwards and picks the reclaimed child up on the
	// next one.
	//
	// The condition is set as well as the event: an event ages out of etcd on the
	// cluster's TTL (an hour by default), and without the condition a tampering
	// remediated at 02:00 leaves the ControlPlane reporting Ready=True with no
	// durable trace of it anywhere in the API. The pass halts either way, so saying
	// so costs no convergence.
	if child != nil {
		if foreign := builtinRegistrationForeignFields(projected, child); len(foreign) > 0 {
			logger.Info("reclaiming foreign spec fields on the "+display+" KeystoneService child",
				"child", child.Name, "fields", foreign)
			if err := r.reclaimBuiltinRegistrationFields(ctx, projected, child); err != nil {
				fail(reasonServiceRegistrationError,
					fmt.Sprintf("reclaiming the %s KeystoneService child: %v", display, err))
				return nil, ctrl.Result{}, true, err
			}
			// After the write, so neither the event nor the condition claims a reset
			// that did not land.
			message := fmt.Sprintf(
				"the %s KeystoneService child %q carried spec field(s) this ControlPlane did not project (%s); "+
					"they were reset to the projected values — a catalog endpoint or an adopt consent written "+
					"there takes effect in Keystone, so it is never left standing",
				display, child.Name, strings.Join(foreign, ", "))
			r.Recorder.Event(cp, "Warning", "ServiceRegistrationFieldsReclaimed", message)
			fail(reasonServiceRegistrationFieldsReclaimed, message)
			return nil, ctrl.Result{RequeueAfter: korcRequeueAfter}, true, nil
		}
	}

	// Mirror the registration's credentials onto the cluster a placed service runs
	// on. The desired object stands in for the live one: both name the same CR,
	// which is all the mirror derives its Secret name and OpenBao path from. A
	// co-located service is a no-op here.
	ok, reason, message, err := r.ensureBuiltinRegistrationMirror(ctx, cp, desired)
	if err != nil {
		fail(reasonServiceRegistrationError,
			fmt.Sprintf("mirroring the %s registration credentials: %v", display, err))
		return nil, ctrl.Result{}, true, err
	}
	if !ok {
		logger.Info("registration credentials not deliverable, deferring "+display+" projection", "reason", reason)
		fail(reason, message)
		return nil, ctrl.Result{RequeueAfter: korcRequeueAfter}, true, nil
	}

	// Gate on the registered account. Until the Keystone user exists and its
	// password is materialized, projecting the service would point it at
	// credentials that do not resolve — so no service child is written at all, and
	// a previously projected one keeps running on the credentials it already has.
	if accountReady, waitMessage := builtinRegistrationAccountReady(desired, child); !accountReady {
		logger.Info(display + " service registration not ready, deferring " + display + " projection")
		fail(reasonWaitingForServiceRegistration, waitMessage)
		return nil, ctrl.Result{RequeueAfter: korcRequeueAfter}, true, nil
	}
	return child, ctrl.Result{}, false, nil
}

// builtinRegistrationForeignFields names the spec fields the live child carries
// that projected — the spec this ControlPlane applied — does not assert. They are
// the fields Server-Side Apply cannot take back, however forcefully it is applied
// — force only settles CONFLICTS on the fields the apply body carries, so
// reclaiming these takes the ordinary Update in
// reclaimBuiltinRegistrationFields:
//
//   - adopt (both blocks) is a bool with omitempty, so the builder's implicit
//     false serializes away and the apply owns nothing to force. Another field
//     manager's adopt=true therefore survives every pass — and it is the consent
//     that turns a pre-existing Keystone user into a password takeover, or a
//     pre-existing catalog row into one this registration DELETES at teardown.
//   - rotation is a nil pointer, unasserted for the same reason.
//   - endpoints is a listType=map keyed on interface, and a map-list apply
//     reconciles only the keys it carries. An admin endpoint row added by another
//     manager is published in the catalog and never reclaimed.
//
// Everything else the builder names — the URLs, the service and user names, the
// project, the roles, the controlPlaneRef — IS in the apply body, so
// ForceOwnership takes it back on the next pass and needs no check here.
func builtinRegistrationForeignFields(
	projected *c5c3v1alpha1.KeystoneServiceSpec, child *c5c3v1alpha1.KeystoneService,
) []string {
	var foreign []string

	if live := child.Spec.Catalog; live != nil {
		asserted := projected.Catalog
		if live.Adopt && (asserted == nil || !asserted.Adopt) {
			foreign = append(foreign, "spec.catalog.adopt")
		}
		declared := make(map[c5c3v1alpha1.ExternalEndpointType]struct{})
		if asserted != nil {
			for _, ep := range asserted.Endpoints {
				declared[ep.Interface] = struct{}{}
			}
		}
		for _, ep := range live.Endpoints {
			if _, ok := declared[ep.Interface]; !ok {
				foreign = append(foreign, fmt.Sprintf("spec.catalog.endpoints[interface=%s]", ep.Interface))
			}
		}
	}

	if live := child.Spec.Account; live != nil {
		asserted := projected.Account
		if live.Adopt && (asserted == nil || !asserted.Adopt) {
			foreign = append(foreign, "spec.account.adopt")
		}
		if live.Rotation != nil && (asserted == nil || asserted.Rotation == nil) {
			foreign = append(foreign, "spec.account.rotation")
		}
	}

	return foreign
}

// reclaimBuiltinRegistrationFields resets the four spec fields Server-Side Apply
// cannot reclaim — both adopt flags, the account rotation block, and the catalog
// endpoint list — to what this ControlPlane projected, through an ordinary
// Update.
//
// Update is the right verb precisely because apply is not: field ownership is a
// property of apply requests, so a manager that owns adopt or an extra endpoint
// row blocks nothing here. The whole object is written from the read the caller
// just made, so its resourceVersion carries the optimistic-concurrency check and
// a racing write loses to a conflict rather than being clobbered.
//
// Only the four fields are touched, and the endpoint list is REPLACED rather than
// merged: an endpoint row keyed on an interface the projection does not declare
// is exactly what must not survive.
func (r *ControlPlaneReconciler) reclaimBuiltinRegistrationFields(
	ctx context.Context, projected *c5c3v1alpha1.KeystoneServiceSpec, child *c5c3v1alpha1.KeystoneService,
) error {
	reclaimed := child.DeepCopy()
	if live, asserted := reclaimed.Spec.Catalog, projected.Catalog; live != nil && asserted != nil {
		live.Adopt = asserted.Adopt
		live.Endpoints = asserted.Endpoints
	}
	if live, asserted := reclaimed.Spec.Account, projected.Account; live != nil && asserted != nil {
		live.Adopt = asserted.Adopt
		live.Rotation = asserted.Rotation
	}
	if err := r.Update(ctx, reclaimed); err != nil {
		return fmt.Errorf("resetting the foreign spec fields on KeystoneService %q: %w", child.Name, err)
	}
	return nil
}

// builtinRegistrationAccountReady reports whether the registration has
// provisioned the service's Keystone account, and otherwise the message the
// caller relays. The child's own reason and message are carried verbatim, so a
// collision on the user name or an unreachable secret backend is readable from
// the ControlPlane without opening the registration. A nil child — one applied
// but not readable back yet — is the same bounded wait, named after the desired
// object.
func builtinRegistrationAccountReady(desired, child *c5c3v1alpha1.KeystoneService) (bool, string) {
	if child == nil {
		return false, fmt.Sprintf("KeystoneService child %q is not readable yet", desired.Name)
	}
	cond := conditions.GetCondition(child.Status.Conditions, conditionTypeKeystoneServiceAccountReady)
	switch {
	case cond == nil:
		// A child created moments ago, before its controller reconciled it.
		return false, fmt.Sprintf("KeystoneService child %q has not reported %s yet",
			child.Name, conditionTypeKeystoneServiceAccountReady)
	case cond.Status == metav1.ConditionTrue:
		return true, ""
	default:
		return false, builtinRegistrationConditionMessage(child, cond)
	}
}

// foldBuiltinRegistrationReady folds the registration's readiness — account
// provisioned AND catalog entry registered — into the service's own condition,
// and reports whether the caller must halt on it: a running service whose
// catalog entry never landed is reachable by nothing that discovers it through
// the catalog, and the ControlPlane must not report the service as ready for it.
//
// The relayed reason is the FIRST failing sub-condition's, not the aggregate's:
// "NotAllReady" says only that something is wrong, while ServiceCollision on
// CatalogReady says what and admits a fix. The aggregate answers only when no
// sub-condition is False, which is what a registration reports while its
// controller has not written them yet.
func foldBuiltinRegistrationReady(
	cp *c5c3v1alpha1.ControlPlane, child *c5c3v1alpha1.KeystoneService, conditionType string,
) (ctrl.Result, bool) {
	ready := conditions.GetCondition(child.Status.Conditions, conditionTypeReady)
	if ready != nil && ready.Status == metav1.ConditionTrue {
		return ctrl.Result{}, false
	}
	fail := func(reason, message string) (ctrl.Result, bool) {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, true
	}
	for _, subConditionType := range keystoneServiceSubConditionTypes {
		cond := conditions.GetCondition(child.Status.Conditions, subConditionType)
		if cond != nil && cond.Status != metav1.ConditionTrue {
			return fail(cond.Reason, builtinRegistrationConditionMessage(child, cond))
		}
	}
	if ready == nil {
		return fail(reasonWaitingForServiceRegistration, fmt.Sprintf(
			"KeystoneService child %q has not reported %s yet", child.Name, conditionTypeReady))
	}
	return fail(ready.Reason, builtinRegistrationConditionMessage(child, ready))
}

// builtinRegistrationConditionMessage renders one of the child's conditions into
// the message the ControlPlane relays: which child, which condition, and the
// child's own reason and message.
func builtinRegistrationConditionMessage(child *c5c3v1alpha1.KeystoneService, cond *metav1.Condition) string {
	return fmt.Sprintf("KeystoneService child %s: %s=%s/%s: %s",
		child.Name, cond.Type, cond.Status, cond.Reason, cond.Message)
}

// ensureBuiltinRegistrationMirror materializes the registration's consumer Secret
// on the cluster a PLACED service runs on, and reports whether the service may
// proceed.
//
// The registration controller delivers the credentials at home only. A service
// placed on a target cluster reads them there, so the same OpenBao path is
// materialized a second time by the ESO on that cluster, from an ExternalSecret
// of the same name. A co-located service needs no mirror at all: the
// registration's own delivery already lands in its namespace.
//
// The ExternalSecret carries this ControlPlane's ownership labels, so the
// teardown that sweeps a placed namespace's label-owned children reaps it with
// the rest.
func (r *ControlPlaneReconciler) ensureBuiltinRegistrationMirror(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, ks *c5c3v1alpha1.KeystoneService,
) (bool, string, string, error) {
	namespace := ks.Namespace
	if targetClusterRefForNamespace(cp, namespace) == nil {
		return true, "", "", nil
	}
	delivery, err := r.childrenClientFor(ctx, cp, namespace)
	if err != nil {
		// The resolver's own text is what an operator reading the condition needs
		// ("cluster not found" for an unregistered name), so it is relayed rather
		// than returned: an unresolvable cluster is a wait, not a failed reconcile.
		return false, commonmulticluster.TargetClusterUnavailable, fmt.Sprintf("%v", err), nil
	}

	// Gate on the store on THAT cluster: an ExternalSecret written against a store
	// that is not ready never syncs, and the service would wait on a Secret whose
	// absence nothing explains.
	storeRef := effectiveControlPlaneStoreRef(cp)
	storeReady, err := secrets.IsStoreRefReady(ctx, delivery, storeRef, namespace)
	if err != nil {
		return false, "", "", fmt.Errorf("checking %s %q in namespace %q: %w",
			storeRef.Kind, storeRef.Name, namespace, err)
	}
	if !storeReady {
		return false, reasonServiceAccountStoreNotReady, fmt.Sprintf(
			"%s %q in namespace %q is not ready on the target cluster; the registration's credentials "+
				"cannot be materialized there", storeRef.Kind, storeRef.Name, namespace), nil
	}

	es := accountExternalSecretSpec(keystoneServiceCredentialsSecretName(ks), namespace,
		keystoneServiceRemoteKeyFor(ks), storeRef)
	if err := r.ensureUnownedOrOwned(ctx, delivery, cp, es); err != nil {
		return false, "", "", err
	}
	return true, "", "", nil
}
