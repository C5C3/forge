// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// ServiceAccountsReady reasons. Like the other condition-reason blocks these are
// the single source of truth for the status contract; call sites MUST reference
// the constants so a rename is caught by the compiler. Most of them are written
// by the KeystoneService controller onto a registration child, and relayed
// verbatim onto the ControlPlane's conditions from here and from the service
// legs.
const (
	// reasonNoServiceRegistrationsProjected is the True reason when no built-in
	// service block is declared, which is every External-mode ControlPlane. It is
	// still set True rather than omitted, so the condition schema is identical
	// either way.
	reasonNoServiceRegistrationsProjected = "NoServiceRegistrationsProjected"
	// reasonServiceAccountsProvisioned is the True reason when every projected
	// built-in registration is Ready.
	reasonServiceAccountsProvisioned = "ServiceAccountsProvisioned"
	// reasonWaitingForServiceAccountAdmin defers projection until the admin
	// credential is minted (K-ORC cannot talk to Keystone before then).
	reasonWaitingForServiceAccountAdmin = "WaitingForAdminCredential"
	// reasonServiceAccountStoreNotReady reports the OpenBao-backed
	// ClusterSecretStore is unavailable, so no password can round-trip.
	reasonServiceAccountStoreNotReady = "SecretStoreNotReady"
	// reasonProbingForCollision reports that a fail-loudly collision probe has not
	// yet resolved either way.
	reasonProbingForCollision = "ProbingForCollision"
	// reasonServiceAccountCollision reports the fail-loudly default: a declared
	// user or managed project already exists in Keystone and adopt was not set.
	reasonServiceAccountCollision = "ServiceAccountCollision"
	// reasonWaitingForServiceAccounts is the bounded wait while the K-ORC User /
	// Project / password round-trip converges.
	reasonWaitingForServiceAccounts = "WaitingForServiceAccounts"
	// reasonServiceAccountsFailed reports a terminal K-ORC failure on a
	// service-account child CR.
	reasonServiceAccountsFailed = "ServiceAccountsFailed"
	// reasonServiceAccountError reports a Kubernetes-level failure reconciling a
	// service-account child (not a K-ORC/OpenStack failure).
	reasonServiceAccountError = "ServiceAccountError"
)

// projectedBuiltinRegistration names one KeystoneService child a built-in
// service leg projects: the display name used in messages and the desired child
// whose name/namespace the aggregation reads.
type projectedBuiltinRegistration struct {
	display string
	desired *c5c3v1alpha1.KeystoneService
}

// projectedBuiltinRegistrations returns one entry per enabled built-in service (a
// non-nil spec.services.glance / .placement / .barbican / .neutron), in that
// order.
func projectedBuiltinRegistrations(cp *c5c3v1alpha1.ControlPlane) []projectedBuiltinRegistration {
	var entries []projectedBuiltinRegistration
	if cp.Spec.Services.Glance != nil {
		entries = append(entries, projectedBuiltinRegistration{
			display: "glance", desired: desiredGlanceRegistration(cp),
		})
	}
	if cp.Spec.Services.Placement != nil {
		entries = append(entries, projectedBuiltinRegistration{
			display: "placement", desired: desiredPlacementRegistration(cp),
		})
	}
	if cp.Spec.Services.Barbican != nil {
		entries = append(entries, projectedBuiltinRegistration{
			display: "barbican", desired: desiredBarbicanRegistration(cp),
		})
	}
	if cp.Spec.Services.Neutron != nil {
		entries = append(entries, projectedBuiltinRegistration{
			display: "neutron", desired: desiredNeutronRegistration(cp),
		})
	}
	return entries
}

// reconcileServiceAccounts aggregates the readiness of the KeystoneService
// children the Glance, Placement, Barbican and Neutron legs applied earlier in
// the same pass into the ServiceAccountsReady condition.
//
// The double reporting is intended: a failing child already fails its own
// service condition, and the aggregate names the same cause under the condition
// type operators alert on.
//
// The member carries no condition gate, because it only reads the children the
// service legs wrote: there is no projection it could defer.
func (r *ControlPlaneReconciler) reconcileServiceAccounts(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, error) {
	fail := conditionFailer(cp, conditionTypeServiceAccountsReady)

	entries := projectedBuiltinRegistrations(cp)
	if len(entries) == 0 {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeServiceAccountsReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             reasonNoServiceRegistrationsProjected,
			Message:            "no built-in service registrations are projected",
		})
		return ctrl.Result{}, nil
	}

	for _, entry := range entries {
		// Read through the LOCAL client: a KeystoneService is reconciled on the
		// management cluster beside the ControlPlane it registers through, whatever
		// cluster its service runs on.
		child := &c5c3v1alpha1.KeystoneService{}
		err := r.Get(ctx, client.ObjectKeyFromObject(entry.desired), child)
		switch {
		case apierrors.IsNotFound(err):
			// The service leg is gated on something upstream of its own apply, so the
			// child it projects is not there yet. That is a bounded wait, not a failure.
			fail(reasonWaitingForServiceRegistration, fmt.Sprintf(
				"KeystoneService child %q has not been projected yet", entry.desired.Name))
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		case err != nil:
			err = fmt.Errorf("reading the %s KeystoneService child: %w", entry.display, err)
			fail(reasonServiceRegistrationError, err.Error())
			return ctrl.Result{}, err
		}

		if res, halt := foldBuiltinRegistrationReady(cp, child, conditionTypeServiceAccountsReady); halt {
			return res, nil
		}
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeServiceAccountsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             reasonServiceAccountsProvisioned,
		Message:            fmt.Sprintf("%d built-in service registration(s) ready", len(entries)),
	})
	return ctrl.Result{}, nil
}
