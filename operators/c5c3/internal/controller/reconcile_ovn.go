// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// reconcileOVN mirrors the readiness of the OVNCentral named by
// spec.services.neutron.ovn.centralRef into the OVNReady condition.
//
// It is the one sub-reconciler that writes nothing at all. The OVN control plane
// is deployed outside the ControlPlane, the way the infrastructure clusters in
// spec.infrastructure are: the plane never projects the central, never updates
// it, and never deletes it. What it needs from the central is what the Neutron
// projection consumes — the two database addresses the ML2/OVN mechanism driver
// dials and the client Secret it presents there — so this pass reads the central,
// decides whether those are usable, and records the verdict.
//
// The arms, in order, each with its own reason so the condition says what to fix:
// OVNNotManaged (no network service), OVNCentralNamespaceForbidden,
// OVNCentralNotFound, OVNCentralReadError, OVNCentralNotExternallyReachable,
// WaitingForOVNCentral, OVNEndpointsPending, and OVNCentralReady. Every not-yet
// arm requeues after infraRequeueAfter rather than erroring: nothing this
// ControlPlane does can converge a central it does not own, so the pass waits for
// the ovn-operator instead of burning manager backoff on it.
func (r *ControlPlaneReconciler) reconcileOVN(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	failed := conditionFailer(cp, conditionTypeOVNReady)

	// spec.services.neutron is optional. Without it no OVN control plane is
	// consumed, so OVNReady reports not-managed True and the aggregate Ready is
	// not blocked (the same staged-adoption posture the service legs take).
	if cp.Spec.Services.Neutron == nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeOVNReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "OVNNotManaged",
			Message:            "spec.services.neutron is unset; no OVN control plane is consumed by this ControlPlane",
		})
		return ctrl.Result{}, nil
	}

	// The reach check the validating webhook enforces, re-run here as the
	// controller-side backstop keystoneServiceNamespaceAllowed already is for the
	// sibling cross-namespace trust decision. A ControlPlane can reach etcd without
	// ever passing through admission — an unregistered webhook during install, a
	// GitOps or etcd restore replaying stored objects — and a centralRef pointing
	// into another plane's namespace is not a read-only act: the arms below relay
	// that central's database addresses and status, and the Neutron projection
	// gated on OVNReady hands the child a pointer whose operator mirrors the
	// central's client Secret, a full mTLS identity for its Northbound and
	// Southbound databases, into this plane's namespace. So the read itself does
	// not happen: the condition carries the webhook's own message, which names the
	// field and why it is refused.
	if errs := c5c3v1alpha1.ValidateNeutronOVNCentralNamespace(cp); len(errs) > 0 {
		failed("OVNCentralNamespaceForbidden", truncateConditionMessage(errs.ToAggregate().Error()))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	name := cp.Spec.Services.Neutron.OVN.CentralRef.Name
	ns := cp.NeutronOVNCentralNamespace()

	// The read goes through the LOCAL client whatever cluster the network service
	// is placed on: the OVNCentral CR lives on the management cluster, which is
	// where the ovn-operator reconciles it, and only the children it projects land
	// on the target cluster its own spec.targetClusterRef selects.
	central := &ovnv1alpha1.OVNCentral{}
	switch err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, central); {
	case apierrors.IsNotFound(err):
		failed("OVNCentralNotFound", fmt.Sprintf(
			"OVNCentral %s/%s named by spec.services.neutron.ovn.centralRef does not exist", ns, name))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	case meta.IsNoMatchError(err):
		// The ovn-operator is optional infrastructure, so its CRD may simply be
		// absent. That is a deployment gap the operator has to fix, not a fault
		// this reconcile can retry out of, so it reads as a wait with an
		// actionable message rather than an error.
		failed("OVNCentralReadError", "the OVNCentral CRD is not served on this cluster; install the ovn-operator")
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	case err != nil:
		failed("OVNCentralReadError", err.Error())
		return ctrl.Result{}, fmt.Errorf("reading OVNCentral %s/%s: %w", ns, name, err)
	}

	// Which addresses the Neutron pods can dial depends on whether they share a
	// cluster with the central. On another cluster they leave their own cluster
	// network, so both databases have to be published on the node network.
	sameCluster := sameTargetCluster(cp.NeutronTargetClusterRef(), central.Spec.TargetClusterRef)
	if !sameCluster && (!central.Spec.Northbound.ExternallyReachable || !central.Spec.Southbound.ExternallyReachable) {
		failed("OVNCentralNotExternallyReachable", fmt.Sprintf(
			"the network service runs on another cluster than OVNCentral %s/%s, so the central must set both "+
				"spec.northbound.externallyReachable and spec.southbound.externallyReachable", ns, name))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// The central's own aggregate Ready is what says its databases serve. Its
	// reason and message are relayed verbatim, because "OVNCentral is not ready"
	// says nothing an operator can act on while the central's own reason does.
	ready := conditions.GetCondition(central.Status.Conditions, conditionTypeReady)
	if ready == nil {
		failed("WaitingForOVNCentral", fmt.Sprintf("OVNCentral %s/%s has not reported Ready yet", ns, name))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}
	if ready.Status != metav1.ConditionTrue {
		failed("WaitingForOVNCentral", fmt.Sprintf("OVNCentral %s/%s: Ready=%s/%s: %s",
			ns, name, ready.Status, ready.Reason, ready.Message))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// A Ready central can still be missing the three values the Neutron projection
	// needs, because the ovn-operator publishes them as its children converge.
	nb, sb := central.Status.Northbound.InternalDbAddress, central.Status.Southbound.InternalDbAddress
	addressField := "internalDbAddress"
	if !sameCluster {
		nb, sb = central.Status.Northbound.DbAddress, central.Status.Southbound.DbAddress
		addressField = "dbAddress"
	}
	var missing []string
	if nb == "" {
		missing = append(missing, "status.northbound."+addressField)
	}
	if sb == "" {
		missing = append(missing, "status.southbound."+addressField)
	}
	if central.Status.ClientSecretName == "" {
		missing = append(missing, "status.clientSecretName")
	}
	if len(missing) > 0 {
		failed("OVNEndpointsPending", fmt.Sprintf("OVNCentral %s/%s reports Ready but has not published %s yet",
			ns, name, strings.Join(missing, ", ")))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeOVNReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "OVNCentralReady",
		Message: truncateConditionMessage(fmt.Sprintf("OVNCentral %s/%s is ready: Northbound %s, Southbound %s",
			ns, name, nb, sb)),
	})
	return ctrl.Result{}, nil
}

// ovnCentralToControlPlaneMapper maps an OVNCentral event onto the ControlPlanes
// whose services.neutron.ovn.centralRef names it.
//
// A plain Owns() would never fire: the central is deployed outside the plane and
// only referenced, so it carries no ControlPlane owner reference. The List is
// cluster-wide, not namespace-scoped: the central may live in a namespace of its
// own, so the ControlPlanes referencing it live elsewhere. The match is on the
// resolved ref NAMESPACE and the name together, so a same-named central beside an
// unrelated ControlPlane never wakes it.
func (r *ControlPlaneReconciler) ovnCentralToControlPlaneMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)
	central, ok := obj.(*ovnv1alpha1.OVNCentral)
	if !ok {
		return nil
	}

	var list c5c3v1alpha1.ControlPlaneList
	if err := r.List(ctx, &list); err != nil {
		// Surface the failure rather than silently dropping the event: an
		// unhealthy informer cache would otherwise leave OVNReady stale until the
		// next periodic resync, with no operational signal.
		logger.Error(err, "listing ControlPlanes for OVNCentral event",
			"central", client.ObjectKeyFromObject(central))
		return nil
	}

	var requests []reconcile.Request
	for i := range list.Items {
		cp := &list.Items[i]
		if cp.Spec.Services.Neutron != nil &&
			cp.Spec.Services.Neutron.OVN.CentralRef.Name == central.Name &&
			cp.NeutronOVNCentralNamespace() == central.Namespace {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cp)})
		}
	}
	return requests
}
