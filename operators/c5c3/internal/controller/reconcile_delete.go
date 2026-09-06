// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// korcFinalizerPrefix is the common prefix of the finalizers K-ORC adds to the
// CRs it manages (e.g. "openstack.k-orc.cloud/applicationcredential"). The
// group prefix is stable across every K-ORC kind, so prefix-matching is what the
// stall escape uses to strip K-ORC's finalizers without hard-coding a suffix per
// kind. Non-K-ORC finalizers on the same object are preserved.
const korcFinalizerPrefix = orcv1alpha1.GroupName + "/"

// orcChildObject is one K-ORC CR the ControlPlane owns and must tear down first.
// newObj returns a zeroed object for Get/Delete.
type orcChildObject struct {
	newObj func() client.Object
	name   string
}

// orcChildObjects is the spec-derived set of K-ORC CRs the ControlPlane owns.
// All the CRs live in childNamespace(cp). A name that never existed in the
// current mode is simply NotFound and is tolerated as already-gone.
//
// DELETION BLAST RADIUS. The sweep is correct for BOTH keystone modes, because
// what a Delete does to the external OpenStack installation is decided by each
// CR's ManagementPolicy, not by the ControlPlane's mode:
//
//   - ApplicationCredential — ManagementPolicyManaged. Its K-ORC finalizer revokes
//     the credential at the Keystone level BEFORE the CR delete returns, so
//     authenticating with it immediately afterwards yields 404 "Could not find
//     Application Credential" (not 401). This is the one identity object the
//     operator minted, so it is the one it destroys.
//   - User, Domain — ManagementPolicyUnmanaged imports (see ensureKORCAdminImports).
//     Deleting their CRs removes the Kubernetes objects and leaves the OpenStack
//     resources they imported untouched. K-ORC's deletion-guard finalizers also
//     enforce the teardown order: a User cannot go while an ApplicationCredential
//     still references it. Note that K-ORC cannot RUN those finalizers once the
//     credential the imports authenticate with is revoked (it re-fetches the
//     imported resource through an authenticated actuator before releasing any
//     finalizer), which is why reconcileDelete force-releases an unmanaged-only
//     remainder instead of waiting for K-ORC.
//   - Service, Endpoint — in Managed mode these are the managed catalog rows, so
//     the sweep deletes them from Keystone's catalog. In External mode the identity
//     Service and its per-interface Endpoints are ManagementPolicyUnmanaged imports
//     (see ensureExternalCatalogImports), so deleting them is a CR-only delete and
//     the external catalog is left bit-for-bit intact.
//
// That holds for a teardown K-ORC can complete. The stall escape in reconcileDelete
// is the deliberate exception: it strips the very finalizer that would have revoked
// the credential or removed the row, so every MANAGED CR it releases leaves its
// OpenStack resource behind with no Kubernetes object naming it. The alternative is
// a permanently wedged namespace, so the escape stays — and names what it orphaned.
//
// The OpenBao-backed Secrets are torn down by owner-reference GC, including the
// path behind the {name}-admin-app-credential-backup PushSecret: its
// DeletionPolicy is Delete (see adminAppCredentialPushSecret), so the credential
// this teardown revokes in Keystone does not outlive it in OpenBao. Nothing else
// is touched.
func orcChildObjects(cp *c5c3v1alpha1.ControlPlane) []orcChildObject {
	newService := func() client.Object { return &orcv1alpha1.Service{} }
	newEndpoint := func() client.Object { return &orcv1alpha1.Endpoint{} }
	newUser := func() client.Object { return &orcv1alpha1.User{} }
	newDomain := func() client.Object { return &orcv1alpha1.Domain{} }

	objs := []orcChildObject{
		{func() client.Object { return &orcv1alpha1.ApplicationCredential{} }, adminAppCredentialName(cp)},
	}

	// The managed-mode catalog children come from the same table that registers them
	// (managedCatalogRows), today the identity row alone: a built-in service's rows
	// belong to the KeystoneService child projected for it, and that child's own
	// finalizer takes them out of the catalog. In External mode these very names
	// belong to the unmanaged identity imports instead; either way a name that never
	// existed in the current mode is simply NotFound and tolerated as already-gone.
	for _, row := range managedCatalogRows(cp) {
		objs = append(objs, orcChildObject{newService, row.crName})
		for _, ep := range row.endpoints {
			objs = append(objs, orcChildObject{newEndpoint, ep.crName})
		}
	}

	objs = append(
		objs,
		orcChildObject{newUser, adminUserRef(cp)},
		orcChildObject{newDomain, adminDomainRef(cp)},
	)

	if !cp.IsExternalKeystone() {
		return objs
	}

	for _, iface := range externalCatalogInterfaces {
		objs = append(objs, orcChildObject{newEndpoint, keystoneEndpointImportName(cp, iface)})
	}
	return objs
}

// reconcileDelete drives the ORC-teardown finalizer when the ControlPlane CR is
// being deleted. It is a no-op if the finalizer is absent.
//
// The ControlPlane owns K-ORC CRs whose finalizers revoke/delete against the
// Keystone API. If the owner-reference GC cascade ran unsequenced, Keystone (and
// in managed mode its MariaDB) would be torn down at the same time as those ORC
// CRs, so the K-ORC finalizers could never complete and the ControlPlane /
// namespace would hang indefinitely on Terminating ORC CRs. Holding the
// ControlPlane CR in etcd (via this finalizer) defers the GC cascade, keeping
// Keystone reachable while K-ORC revokes.
//
// Ahead of all of it, the KeystoneService registrations the ControlPlane projects
// for its built-in services are deleted and waited for: their K-ORC CRs belong to
// the registration, and its controller drives them through the very admin
// credential step 1 revokes (deleteRegistrationsBeforeTeardown). The flow:
//
//  1. Delete every owned K-ORC CR (idempotent; NotFound / CRD-absent tolerated)
//     and collect those still present (Terminating behind a K-ORC finalizer).
//     Also delete every owned PushSecret: their DeletionPolicy=Delete cleanup —
//     ESO removing the mirrored OpenBao data — needs the per-tenant SecretStore
//     and its ServiceAccount, which the post-release GC cascade reaps
//     unsequenced, so it must happen while the finalizer still holds them.
//  2. When no K-ORC CR and no owned PushSecret remain, force-release the children
//     of every projected registration still present
//     (releaseStalledRegistrationChildren, Warning
//     ServiceRegistrationResourcesOrphaned), then release the finalizer so GC tears
//     down the rest.
//  3. When every CR still present is an Unmanaged import, force-remove their
//     K-ORC finalizers right away: an import's deletion is CR-only, but K-ORC
//     builds an authenticated delete actuator and re-fetches the imported
//     resource by ID before releasing ANY finalizer — and the imports
//     authenticate with the admin application credential whose revocation
//     step 1 already triggered (the managed children ride the admin-password
//     cloud instead). Waiting on them is waiting on a dead-credential retry
//     loop only the stall breaker would cut, at orcTeardownDeadline. Nothing is
//     orphaned: an import never owned the OpenStack resource behind it.
//  4. While managed CRs remain and the bounded orcTeardownDeadline has not
//     elapsed, report KORCReady=False/FinalizingORC and requeue.
//  5. Once the stall timeout elapses (K-ORC cannot make progress — most likely
//     Keystone is already gone, so it cannot revoke), force-remove the
//     openstack.k-orc.cloud/* finalizers, emit a Warning event, and release the
//     ControlPlane finalizer so deletion can complete. Every MANAGED CR released
//     that way orphans the OpenStack resource behind it, so a second Warning names
//     them: they are the only teardown outcome an operator has to repair by hand.
//     The children of a projected registration still present are force-released on
//     this path too, under the same Warning as on the normal release.
func (r *ControlPlaneReconciler) reconcileDelete(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer) {
		// The remote-children finalizer outlives the ORC one on the stall path: the
		// escape below releases the ORC finalizer without ever reaching the namespace
		// sweep, so the placed namespaces are still standing. Finish that sweep here
		// — nothing else can — and release the remaining finalizer once it is done.
		// A ControlPlane that placed nothing carries no remote-children finalizer, so
		// this is the same no-op it always was.
		return r.reconcileDeleteRemoteChildren(ctx, cp)
	}

	// The projected KeystoneService registrations go FIRST, and the rest of the
	// teardown waits for them. Their K-ORC CRs are owned by the registration rather
	// than by this ControlPlane, so no enumeration here names them, and their
	// controller drives them through the admin credential deleteORCResources is
	// about to revoke. Released first, the KeystoneService controller would find
	// its ControlPlane NotFound, fail open, and leave those CRs Terminating behind
	// K-ORC finalizers nothing can complete — the namespace wedge this finalizer
	// exists to prevent.
	if res, halt, err := r.deleteRegistrationsBeforeTeardown(ctx, cp); halt {
		return res, err
	}

	remaining, hasLiveWork, err := r.deleteORCResources(ctx, cp)
	if err != nil {
		return ctrl.Result{}, err
	}

	// The owned PushSecrets carry DeletionPolicy=Delete: ESO removes the
	// mirrored OpenBao data when it processes their deletion — and that needs
	// the per-tenant SecretStore and its eso-tenant-auth ServiceAccount, both of
	// which are CP-owned and die in the unsequenced GC cascade the moment the
	// finalizer is released. Delete the PushSecrets HERE, while the store still
	// authenticates, and gate the release on their disappearance; otherwise the
	// credential this teardown revokes in Keystone outlives it in OpenBao behind
	// an Errored, Terminating PushSecret.
	pushRemaining, err := r.deleteOwnedPushSecrets(ctx, cp)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Announce the teardown once, on the first pass where a live (not-yet-
	// Terminating) ORC CR is observed. Later requeues see only Terminating CRs
	// and suppress the event, giving exactly-once semantics per deletion.
	if hasLiveWork {
		r.Recorder.Event(cp, "Normal", "FinalizingORC",
			"Deleting owned K-ORC CRs before releasing the ControlPlane so K-ORC can revoke against a reachable Keystone")
	}

	if len(remaining) == 0 && len(pushRemaining) == 0 {
		// Every owned K-ORC CR is gone (revoked and deleted, or never existed)
		// and ESO has finished the OpenBao cleanup behind the owned PushSecrets.
		//
		// The cross-namespace children are still standing, though: no GC cascade
		// reaches them, because they carry ownership labels rather than an owner
		// reference. Tear them down HERE, while the finalizer still holds the
		// ControlPlane — releasing first would strand every one of them, in a
		// namespace nothing points back from.
		done, err := r.sweepNamespacesBeforeRelease(ctx, cp)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		}

		// A projected registration still present here is one the whole registration
		// window was spent on. Its children have to be released before the
		// ControlPlane lets go, or they stay Terminating with nothing to collect them.
		// Not done means a PushSecret is still owed its OpenBao purge, so wait for it
		// the way the sweep above is waited for.
		released, err := r.releaseStalledRegistrationChildren(ctx, cp)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !released {
			// Name the wait on the CR. The K-ORC condition set above says "0 K-ORC
			// CR(s) and 0 PushSecret(s)" — this branch is reachable only once both are
			// empty — so without this an operator watching a ControlPlane sit in
			// Terminating reads a condition asserting nothing is outstanding.
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeKORCReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "FinalizingORC",
				Message: "waiting for ESO to purge the OpenBao data behind a stalled KeystoneService " +
					"registration's PushSecret(s) before releasing the ControlPlane",
			})
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}

		// The managed message bus goes by hand, with foreground propagation, and
		// the release waits for it: a GC-driven background delete opens a window
		// in which the RabbitMQ Cluster Operator re-creates the broker unowned
		// (see deleteManagedMessagingBeforeRelease).
		busGone, err := r.deleteManagedMessagingBeforeRelease(ctx, cp)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !busGone {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeInfrastructureReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "FinalizingMessaging",
				Message: fmt.Sprintf("waiting for the managed RabbitmqCluster %q to be deleted before releasing the ControlPlane",
					cp.Spec.Infrastructure.Messaging.ClusterRef.Name),
			})
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		}

		// Release the finalizers so GC tears down Keystone/MariaDB and the rest. The
		// remote-children one goes with it: the sweep above visited every placed
		// namespace, on the cluster it lives on or, past the abandon window, not at
		// all — either way nothing is left for it to hold the CR open for.
		r.Recorder.Event(cp, "Normal", "ORCTeardownComplete",
			"No remaining K-ORC CRs; releasing the ControlPlane finalizer")
		controllerutil.RemoveFinalizer(cp, controlPlaneORCFinalizer)
		controllerutil.RemoveFinalizer(cp, commonmulticluster.RemoteChildrenFinalizer)
		if err := r.Update(ctx, cp); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Once only Unmanaged imports remain, K-ORC can never finish them: its
	// delete path re-fetches the imported resource by ID through an
	// authenticated actuator before releasing any finalizer, and the imports
	// authenticate with the admin application credential this sweep just
	// revoked. Force-release their K-ORC finalizers instead of waiting out the
	// stall window on a dead-credential retry loop. This is a Normal event, not
	// a Warning: an import's deletion is CR-only by definition, so the external
	// installation is left bit-for-bit intact and nothing is orphaned.
	onlyUnmanagedLeft := len(remaining) > 0
	for _, obj := range remaining {
		if isManagedORCChild(obj) {
			onlyUnmanagedLeft = false
			break
		}
	}
	if onlyUnmanagedLeft {
		names := make([]string, 0, len(remaining))
		for _, obj := range remaining {
			names = append(names, obj.GetName())
		}
		if err := r.forceRemoveKORCFinalizers(ctx, remaining); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(cp, "Normal", "ORCImportsReleased", fmt.Sprintf(
			"released the K-ORC finalizers of the remaining unmanaged import CR(s) %v: an import's deletion is "+
				"CR-only, and its finalizer cannot authenticate once the admin application credential is revoked",
			names,
		))
		log.FromContext(ctx).Info("released the K-ORC finalizers of the remaining unmanaged imports",
			"imports", names)
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Managed K-ORC CRs are still Terminating behind a finalizer that revokes
	// against Keystone, and/or ESO is still deleting the OpenBao data behind the
	// owned PushSecrets. Within the stall window, wait and requeue; updateStatus
	// persists the FinalizingORC condition so the wait is operator-visible. The
	// reason matches the FinalizingORC event above.
	if time.Since(cp.DeletionTimestamp.Time) <= orcTeardownDeadline {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "FinalizingORC",
			Message: fmt.Sprintf(
				"waiting for %d K-ORC CR(s) and %d PushSecret(s) to finish their teardown before releasing the ControlPlane",
				len(remaining), len(pushRemaining),
			),
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Stall timeout elapsed: the K-ORC finalizers cannot complete (most likely
	// Keystone is already gone, so K-ORC cannot revoke). Force-remove the
	// openstack.k-orc.cloud/* finalizers so GC can reclaim the stuck CRs, warn so
	// the wedge is operator-visible, then release the ControlPlane finalizer.
	//
	// Classify BEFORE stripping. Releasing an Unmanaged import has zero blast radius:
	// deleting its CR never called OpenStack in the first place. A Managed CR is the
	// opposite — its K-ORC finalizer is what revokes the ApplicationCredential, or
	// takes a managed catalog row back out of Keystone's catalog. Stripping it
	// abandons that OpenStack resource, and once GC reclaims the CR nothing in
	// Kubernetes names it. A flat list of CR names reads as "K-ORC was slow"; the
	// operator has to be told which resources leaked, and where.
	names := make([]string, 0, len(remaining))
	var orphaned []string
	for _, obj := range remaining {
		names = append(names, obj.GetName())
		if isManagedORCChild(obj) {
			orphaned = append(orphaned, obj.GetName())
		}
	}

	// The stall can also be hit with ZERO K-ORC CRs left (only PushSecrets
	// stuck, handled below) — suppress the K-ORC Warning then, so it never
	// alarms about an empty list.
	if len(remaining) > 0 {
		if err := r.forceRemoveKORCFinalizers(ctx, remaining); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(cp, "Warning", "ORCTeardownStalled", fmt.Sprintf(
			"K-ORC CRs %v stayed Terminating longer than %s (K-ORC may be unable to reach Keystone to revoke); "+
				"force-removed their K-ORC finalizers and releasing the ControlPlane",
			names, orcTeardownDeadline,
		))
		if len(orphaned) > 0 {
			r.Recorder.Event(cp, "Warning", "ORCResourcesOrphaned", fmt.Sprintf(
				"the OpenStack resources behind the managed K-ORC CRs %v were NOT deleted: their finalizers were "+
					"force-removed before K-ORC could revoke the admin application credential or remove the "+
					"catalog rows and identities it registered. Nothing in "+
					"Kubernetes names them any more — remove them from Keystone by hand",
				orphaned,
			))
		}
		log.FromContext(ctx).Info("ORC teardown stalled; force-removed K-ORC finalizers",
			"remaining", names, "orphaned", orphaned, "deadline", orcTeardownDeadline)
	}

	// A PushSecret still present past the stall window means ESO cannot finish
	// the OpenBao cleanup (backend or store gone). Strip its finalizers so the
	// namespace cannot wedge on it, and name the OpenBao paths that keep their
	// data — like the orphaned managed CRs, they are repair-by-hand outcomes.
	if len(pushRemaining) > 0 {
		stuckKeys := make([]string, 0, len(pushRemaining))
		for _, pending := range pushRemaining {
			ps := pending.pushSecret
			for _, d := range ps.Spec.Data {
				stuckKeys = append(stuckKeys, d.Match.RemoteRef.RemoteKey)
			}
			ps.Finalizers = nil
			// Through the client of the cluster the PushSecret was found on: one in a
			// placed namespace exists on the target alone.
			if err := pending.cluster.Update(ctx, ps); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("force-removing finalizers from PushSecret %q: %w", ps.Name, err)
			}
		}
		r.Recorder.Event(cp, "Warning", "OpenBaoCleanupStalled", fmt.Sprintf(
			"ESO could not delete the mirrored OpenBao data behind %d PushSecret(s) within %s; the OpenBao "+
				"path(s) %v may still hold the revoked credential — delete them by hand",
			len(pushRemaining), orcTeardownDeadline, stuckKeys,
		))
	}

	// The escape gave up on the projected registrations too, back in
	// deleteRegistrationsBeforeTeardown. Release the children of the ones still
	// present before the ControlPlane lets go: nothing collects them afterwards, and
	// they would stay Terminating behind K-ORC and ESO finalizers nothing can run.
	released, err := r.releaseStalledRegistrationChildren(ctx, cp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !released {
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Only the ORC finalizer. The escape gave up on K-ORC, not on the children
	// this ControlPlane placed on a target cluster: the namespace sweep never ran
	// on this path, so the remote-children finalizer stays on and
	// reconcileDeleteRemoteChildren finishes the sweep from the next pass.
	controllerutil.RemoveFinalizer(cp, controlPlaneORCFinalizer)
	if err := r.Update(ctx, cp); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer after force-remove: %w", err)
	}
	return ctrl.Result{}, nil
}

// reconcileDeleteRemoteChildren finishes the teardown of a ControlPlane that has
// already released its ORC finalizer but still carries the remote-children one.
// Only the ORC stall escape leaves a CR in that state: it gives up on K-ORC
// without reaching the namespace sweep, so the children on the placed clusters
// are still standing and no cascade will ever collect them.
//
// It is a no-op for a CR without the finalizer, which is every ControlPlane that
// places no service on a target cluster.
func (r *ControlPlaneReconciler) reconcileDeleteRemoteChildren(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cp, commonmulticluster.RemoteChildrenFinalizer) {
		return ctrl.Result{}, nil
	}

	done, err := r.sweepNamespacesBeforeRelease(ctx, cp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
	}

	controllerutil.RemoveFinalizer(cp, commonmulticluster.RemoteChildrenFinalizer)
	if err := r.Update(ctx, cp); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing the remote-children finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// sweepNamespacesBeforeRelease is the cross-namespace teardown both release paths
// gate on: every namespace the ControlPlane placed a service in is swept, on the
// cluster it lives on, and the cluster-scoped auth-delegator binding no namespace
// sweep can reach is deleted afterwards. It reports whether the ControlPlane may
// be released.
//
// The auth-delegator binding is cluster-scoped, so the sweep reaches it only when
// Barbican runs in a namespace of its own. Co-located — the default — there is no
// dedicated namespace at all, teardownDedicatedNamespaces returns at once, and
// nothing else can collect it.
func (r *ControlPlaneReconciler) sweepNamespacesBeforeRelease(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (bool, error) {
	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	if err != nil || !done {
		return false, err
	}
	r.sweepRegistrationTenantStores(ctx, cp)
	if err := r.deleteBarbicanAuthDelegatorBinding(ctx, cp); err != nil {
		return false, err
	}
	return true, nil
}

// sweepRegistrationTenantStores deletes, best-effort, the tenant-store trios this
// ControlPlane provisioned in the allowlisted namespaces standalone registrations
// come from (see reconcileRegistrationTenantStores).
//
// Nothing else collects them. They sit outside every namespace
// teardownDedicatedNamespaces walks, and a cross-namespace child carries ownership
// labels rather than an owner reference, so no GC cascade reaches them either.
//
// Errors are logged rather than propagated, exactly like
// sweepExternalNamespaceResidue: this is one of the last steps before the
// ControlPlane is released, and a residual trio is a repairable leak, whereas an
// error here would wedge the CR on a finalizer that can never clear.
//
// Unlike the per-pass collection this does NOT wait for a namespace's last
// registration to leave. The plane is going away, so the store has nothing left to
// reach, and a KeystoneService still standing out there fails its own teardown open
// once the ControlPlane it references is gone. That is the posture for a foreign,
// standalone registration; the children of a PROJECTED one are force-released by
// the ControlPlane before it lets go (releaseStalledRegistrationChildren), so the
// fail-open path only ever meets finalizer-free children of one. A registration
// mid-purge can lose the store its OpenBao cleanup rides on, which is the same
// trade the teardown already takes past its stall windows: a leak somebody can
// collect beats a CR nobody can delete.
func (r *ControlPlaneReconciler) sweepRegistrationTenantStores(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) {
	logger := log.FromContext(ctx)

	namespaces, err := r.registrationTenantStoreNamespaces(ctx, cp)
	if err != nil {
		logger.V(1).Info("best-effort registration tenant-store sweep could not enumerate its namespaces",
			"error", err.Error())
		return
	}
	for _, namespace := range namespaces {
		logger.Info("sweeping the registration tenant store of a terminating ControlPlane", "namespace", namespace)
		if err := r.deleteESOTenantStoreTrioIn(ctx, cp, namespace); err != nil {
			logger.V(1).Info("best-effort registration tenant-store sweep could not delete a trio",
				"namespace", namespace, "error", err.Error())
		}
	}
}

// teardownDedicatedNamespaces deletes the children the ControlPlane placed in a
// namespace of its own, and reports whether the sweep is complete. It is the
// GARBAGE-COLLECTION MECHANISM that owner references cannot provide: a
// cross-namespace child carries no owner reference (Kubernetes forbids one), so
// nothing reaps it when the ControlPlane goes. This does, by hand, while the
// finalizer still holds the CR.
//
// The order is load-bearing:
//
//  1. The SERVICE CHILDREN (Keystone, Horizon) first, and the sweep waits for them
//     to disappear. Their own operators run a sequenced ESO cleanup on deletion —
//     the Keystone child's fernet/credential-key PushSecrets purge their OpenBao
//     paths — and that cleanup authenticates through the tenant store in the same
//     namespace. Removing the store first would leave the key material in OpenBao
//     with no Kubernetes object naming it. The service CRs live on the MANAGEMENT
//     cluster whatever cluster their service was placed on, so waiting for them
//     here is also what waits out their own operators' remote sweeps.
//  2. Then what the ControlPlane put in that namespace, in the order its
//     lifecycle decides. One half of it is the LABEL-SELECTED sweep of the cluster
//     the namespace was placed on (commonmulticluster.DeleteRemoteChildren over
//     controlPlaneRemoteChildKinds), which is what reaches the children no cascade
//     and no owner reference can, both being confined to one cluster. It is a
//     no-op for a namespace nothing was placed on.
//     - Managed: the sweep, then the namespace itself, deleted on BOTH clusters
//     because reconcileNamespaces created it on both. It is deleted ONLY when it
//     carries our ownership labels — reconcileNamespaces never adopts a namespace
//     it did not create, and neither does this. An unlabelled one is left standing
//     with a Warning, rather than destroying a namespace (and every workload in
//     it) the operator never owned.
//     - External: the namespace stays, so its residue is named and deleted
//     instead — the backing services, the credential material, and the
//     tenant-store trio LAST, for the reason above — and the sweep follows it,
//     reaching whatever the names did not enumerate. That way round because the
//     named sweep is the only one of the two with an order to give.
//
// A placed namespace whose cluster does not resolve is not swept: the pass
// reports NOT done, so the ControlPlane keeps its finalizers and tries again.
// Past commonmulticluster.AbandonAfter the cluster is given up on instead — a
// Warning records that its children were left running, and the sweep continues
// without it, because holding the CR in Terminating forever helps nobody.
//
// Past orcTeardownDeadline the sweep stops waiting: it emits a Warning naming
// what is stuck and reports done, so a wedged child can never make a namespace
// undeletable. That mirrors the ORC stall escape, and it is the same trade — a
// repairable leak beats a permanently wedged namespace.
func (r *ControlPlaneReconciler) teardownDedicatedNamespaces(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (bool, error) {
	assignments := cp.DedicatedServiceNamespaces()
	if len(assignments) == 0 {
		return true, nil
	}
	logger := log.FromContext(ctx)

	stalled := time.Since(cp.DeletionTimestamp.Time) > orcTeardownDeadline

	var stuck, unresolved []string
	for _, assignment := range assignments {
		remaining, err := r.deleteServiceChildrenIn(ctx, cp, assignment.Name)
		if err != nil {
			return false, err
		}
		if len(remaining) > 0 {
			stuck = append(stuck, remaining...)
			continue
		}

		// The cluster this namespace lives on, resolved through the DELETION
		// resolver: a target that was deregistered under a terminating CR must not
		// fail the pass, or the finalizers could never be released.
		ref := targetClusterRefForNamespace(cp, assignment.Name)
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
			ctx, r.Resolver, r.Client, ref, *cp.DeletionTimestamp)
		switch {
		case wait:
			unresolved = append(unresolved, ref.Name)
			continue
		case children == nil:
			// Past the abandon window. The children on that cluster are unreachable
			// either way, so they are left where they are and the teardown carries on.
			r.Recorder.Event(cp, "Warning", "RemoteChildrenAbandoned", fmt.Sprintf(
				"Target cluster %q is no longer registered; releasing the remote-children finalizer without "+
					"deleting the objects it holds in namespace %q labelled as owned by this ControlPlane",
				ref.Name, assignment.Name))
			// Abandoning the target's copy of the namespace does not license leaking
			// the management cluster's, which is reachable and ours: nothing comes
			// back for it once both finalizers are released (see clustersFor).
			if assignment.Lifecycle != c5c3v1alpha1.ServiceNamespaceLifecycleExternal {
				if err := r.deleteManagedNamespace(ctx, r.Client, cp, assignment.Name); err != nil {
					return false, err
				}
			}
			continue
		}

		// The External residue is swept by name, and BEFORE the label-selected sweep
		// below, because it is the only one of the two with an order to give: the
		// tenant-store trio goes last, after everything that authenticated through it.
		external := assignment.Lifecycle == c5c3v1alpha1.ServiceNamespaceLifecycleExternal
		if external {
			r.sweepExternalNamespaceResidue(ctx, children, cp, assignment.Name)
		}

		// Then everything else the ControlPlane left in this namespace on a target
		// cluster: what the named sweep does not enumerate, and what a namespace
		// deletion cannot cascade across a cluster boundary.
		if err := r.deleteRemoteNamespaceChildren(ctx, children, cp, ref, assignment.Name); err != nil {
			return false, err
		}
		if external {
			// Both sweeps above ran on the TARGET cluster, and an External namespace is
			// never deleted, so a namespace that also carries a tenant-store trio at
			// home (esoTenantStoreClusters) has nothing left to collect it. The Managed
			// arm below needs no equivalent: deleting the namespace on both clusters
			// takes each copy with it.
			if hostsHomeRegistration(cp, assignment.Name) {
				if err := r.deleteESOTenantStoreTrioIn(ctx, cp, assignment.Name); err != nil {
					logger.V(1).Info("best-effort collection of the home tenant-store trio failed",
						"namespace", assignment.Name, "error", err.Error())
				}
			}
			continue
		}

		// A placed namespace exists on the management cluster as well, because
		// reconcileNamespaces ensures it on both; deleting it on one alone would
		// leave the other standing with nothing left to reclaim it (see clustersFor).
		for _, c := range r.clustersFor(children) {
			if err := r.deleteManagedNamespace(ctx, c, cp, assignment.Name); err != nil {
				return false, err
			}
		}
	}

	// A cluster that may still be engaging holds the release whatever else the
	// pass found: its children are neither swept nor abandoned yet, and giving up
	// on them here would strand them on a cluster that is about to come back.
	if len(unresolved) > 0 {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNamespacesReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             commonmulticluster.TargetClusterUnavailable,
			Message: fmt.Sprintf("target cluster(s) %v do not resolve; waiting at least %s for them before "+
				"abandoning the children the ControlPlane placed on them", unresolved, commonmulticluster.AbandonAfter),
		})
		return false, nil
	}

	if len(stuck) == 0 {
		return true, nil
	}

	if !stalled {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNamespacesReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "FinalizingNamespaces",
			Message: fmt.Sprintf("waiting for %d cross-namespace child(ren) to finish their teardown before "+
				"releasing the ControlPlane: %v", len(stuck), stuck),
		})
		return false, nil
	}

	// Stalled. Proceed anyway rather than wedging the namespace forever, and name
	// what was left behind — like the orphaned K-ORC resources, it is a
	// repair-by-hand outcome.
	r.Recorder.Event(cp, "Warning", "NamespaceTeardownStalled", fmt.Sprintf(
		"cross-namespace child(ren) %v stayed present longer than %s; releasing the ControlPlane anyway. "+
			"They carry no owner reference, so nothing will garbage-collect them — remove them by hand",
		stuck, orcTeardownDeadline,
	))
	logger.Info("cross-namespace teardown stalled; releasing the ControlPlane anyway",
		"stuck", stuck, "deadline", orcTeardownDeadline)
	return true, nil
}

// deleteRemoteNamespaceChildren deletes everything of controlPlaneRemoteChildKinds
// the ControlPlane owns in namespace on the cluster children writes to. It is a
// no-op for an unplaced namespace, whose children the local cascade collects, so
// the caller runs it without branching on where the namespace lives.
//
// The list goes through the target cluster's UNCACHED reader rather than through
// the children client's cache: this sweep is what licenses the finalizer release,
// and a child the cache has not caught up on would be missed, leaving no CR behind
// to delete it.
func (r *ControlPlaneReconciler) deleteRemoteNamespaceChildren(
	ctx context.Context, children client.Client, cp *c5c3v1alpha1.ControlPlane,
	ref *commonv1.TargetClusterRefSpec, namespace string,
) error {
	reader, err := commonmulticluster.ResolveChildrenAPIReader(ctx, r.Resolver, children, ref)
	if err != nil {
		return err
	}
	return commonmulticluster.DeleteRemoteChildren(ctx, reader, children, r.Scheme, cp,
		namespace, controlPlaneRemoteChildKinds)
}

// teardownReader returns the reader the teardown decides ownership from on the
// cluster c writes to: that cluster's own uncached reader for a resolved target
// client, and the management cluster's for the local one. Both are uncached,
// because a teardown read is one-shot and several of the kinds it names are ones
// the operator never watches (see ControlPlaneReconciler.APIReader).
func (r *ControlPlaneReconciler) teardownReader(c client.Client) client.Reader {
	if commonmulticluster.IsRemote(c) {
		return commonmulticluster.LiveReader(c)
	}
	return r.apiReader()
}

// deleteManagedMessagingBeforeRelease tears the managed message bus down by hand
// and reports whether it is gone, so the finalizer is released only once the
// RabbitmqCluster has left etcd. Owner-reference GC would delete it too, but with
// BACKGROUND propagation: the CR vanishes the instant the RabbitMQ Cluster
// Operator removes its finalizer, and that operator's deletion path (v2.13.0 and
// later, rabbitmq/cluster-operator#1864) removes the finalizer through
// controllerutil.CreateOrUpdate. A second reconcile of the deleting CR, queued by
// the first one's own pod-label and StatefulSet writes, reads NotFound from its
// cache and CREATES a fresh, unowned broker under the same name. Nothing collects
// that one: it outlives the ControlPlane, and a namespace deleted around its pod
// is revisited days later, after the pod's default preStop grace.
//
// Deleting with FOREGROUND propagation closes that window by construction. The
// API server keeps the CR, marked deletingDependents, until every
// blockOwnerDeletion child (the StatefulSet, Services, Secrets, ConfigMaps and
// RBAC the cluster-operator owns) is gone, so the second reconcile still finds
// the object without its finalizer and does nothing.
//
// The wait ends once the cluster-operator's finalizer is off the Terminating CR
// (or the CR is NotFound): the re-create lives inside that finalizer's handling,
// so nothing after it can bring the broker back, and the CR then lingers under
// foregroundDeletion only until GC has reaped its children. Waiting for NotFound
// instead would wait on a garbage collector envtest does not run.
//
// Only a bus this ControlPlane controls is touched: an externally-provisioned
// RabbitmqCluster adopted under the managed name (ensureRabbitMQ never claims
// one) is left standing, and a brownfield secretRef names nothing to delete. A
// cluster that does not serve the kind reads as nothing to wait for. Past
// messagingTeardownDeadline — a cluster-operator that is absent or wedged — the
// wait is abandoned with a Warning naming the broker, and the release falls back
// to the GC cascade this path replaces: a wedged broker must not make the
// ControlPlane undeletable. The escape paths (K-ORC stall, remote-children
// finish) release without this wait, as they give up on everything else.
func (r *ControlPlaneReconciler) deleteManagedMessagingBeforeRelease(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (bool, error) {
	if cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil ||
		cp.Spec.Infrastructure.Messaging.ClusterRef == nil {
		return true, nil
	}
	m := cp.Spec.Infrastructure.Messaging
	key := types.NamespacedName{Name: m.ClusterRef.Name, Namespace: cp.Namespace}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	switch err := r.Get(ctx, key, u); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("reading RabbitmqCluster %q before release: %w", key.Name, err)
	}
	if !metav1.IsControlledBy(u, cp) {
		return true, nil
	}
	if u.GetDeletionTimestamp() == nil {
		if err := r.Delete(ctx, u, client.PropagationPolicy(metav1.DeletePropagationForeground)); client.IgnoreNotFound(err) != nil {
			return false, fmt.Errorf("deleting RabbitmqCluster %q before release: %w", key.Name, err)
		}
		return false, nil
	}
	if !controllerutil.ContainsFinalizer(u, rabbitmqClusterDeletionFinalizer) {
		return true, nil
	}
	if time.Since(cp.DeletionTimestamp.Time) > messagingTeardownDeadline {
		r.Recorder.Event(cp, "Warning", "MessagingTeardownStalled", fmt.Sprintf(
			"RabbitmqCluster %q still carries the RabbitMQ Cluster Operator's finalizer %s after the deletion "+
				"started; releasing the ControlPlane and leaving the broker to the owner-reference cascade",
			key.Name, messagingTeardownDeadline))
		log.FromContext(ctx).Info("managed message bus teardown stalled; releasing the ControlPlane anyway",
			"rabbitmqCluster", key.Name, "deadline", messagingTeardownDeadline)
		return true, nil
	}
	return false, nil
}

// rabbitmqClusterDeletionFinalizer is the finalizer the RabbitMQ Cluster Operator
// holds on every RabbitmqCluster (internal/controller/reconcile_finalizer.go in
// rabbitmq/cluster-operator). Its presence on a Terminating CR is what
// deleteManagedMessagingBeforeRelease waits out: the operator's deletion path
// runs while it is set, and the re-create that path can cause is impossible once
// it is gone.
const rabbitmqClusterDeletionFinalizer = "deletion.finalizers.rabbitmqclusters.rabbitmq.com"

// crossNamespaceServiceChildren returns the service children the ControlPlane
// placed in namespace: the Keystone child when the Keystone service is assigned
// there, and the Horizon, Glance, Placement, Barbican, and Neutron children
// likewise. Each is matched by its deterministic name; ownership is re-checked
// against the live object before anything is deleted.
//
// The Barbican arm names three objects rather than one, because its secret store
// and the dedicated OpenBao instance behind it belong in the WAIT SET too. The
// namespace must not be deleted until the openbao-operator has finished the
// instance's finalizer, and that finalizer runs under the tenant RBAC living in
// this very namespace: a namespace deleted first reaps the RBAC out from under it,
// leaving the instance unfinalizable and the namespace stuck Terminating. An
// external secret store projects no instance, so its name is simply NotFound and
// tolerated as already-gone.
func crossNamespaceServiceChildren(cp *c5c3v1alpha1.ControlPlane, namespace string) []client.Object {
	var children []client.Object
	if cp.KeystoneNamespace() == namespace {
		children = append(children, &keystonev1alpha1.Keystone{
			ObjectMeta: metav1.ObjectMeta{Name: keystoneName(cp), Namespace: namespace},
		})
	}
	if cp.HorizonNamespace() == namespace {
		children = append(children, &horizonv1alpha1.Horizon{
			ObjectMeta: metav1.ObjectMeta{Name: horizonName(cp), Namespace: namespace},
		})
	}
	if cp.GlanceNamespace() == namespace {
		children = append(children, &glancev1alpha1.Glance{
			ObjectMeta: metav1.ObjectMeta{Name: glanceName(cp), Namespace: namespace},
		})
	}
	if cp.PlacementNamespace() == namespace {
		children = append(children, &placementv1alpha1.Placement{
			ObjectMeta: metav1.ObjectMeta{Name: placementName(cp), Namespace: namespace},
		})
	}
	if cp.BarbicanNamespace() == namespace {
		children = append(
			children,
			&barbicanv1alpha1.Barbican{
				ObjectMeta: metav1.ObjectMeta{Name: barbicanName(cp), Namespace: namespace},
			},
			&barbicanv1alpha1.BarbicanSecretStore{
				ObjectMeta: metav1.ObjectMeta{Name: barbicanSecretStoreName(cp), Namespace: namespace},
			},
			&openbaov1alpha1.OpenBaoCluster{
				ObjectMeta: metav1.ObjectMeta{Name: barbicanOpenBaoName(cp), Namespace: namespace},
			},
		)
	}
	// The OVNCentral the network service references is NOT in here, and is deleted
	// nowhere: it is deployed outside the plane and only read (see reconcileOVN).
	if cp.NeutronNamespace() == namespace {
		children = append(children, &neutronv1alpha1.Neutron{
			ObjectMeta: metav1.ObjectMeta{Name: neutronName(cp), Namespace: namespace},
		})
	}
	return children
}

// deleteServiceChildrenIn deletes the service children this ControlPlane placed in
// namespace and returns those still present afterwards (Terminating behind their
// own operator's cleanup finalizers). A child that is not ours — same name, no
// ownership labels — is never touched and never reported as stuck.
func (r *ControlPlaneReconciler) deleteServiceChildrenIn(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, namespace string,
) ([]string, error) {
	var remaining []string
	for _, child := range crossNamespaceServiceChildren(cp, namespace) {
		key := client.ObjectKeyFromObject(child)
		switch err := r.Get(ctx, key, child); {
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("getting %T %s for cross-namespace teardown: %w", child, key, err)
		}
		if !isControlPlaneChild(child, cp) {
			continue
		}
		if child.GetDeletionTimestamp().IsZero() {
			if err := client.IgnoreNotFound(
				r.Delete(ctx, child, client.PropagationPolicy(metav1.DeletePropagationBackground)),
			); err != nil {
				return nil, fmt.Errorf("deleting %T %s: %w", child, key, err)
			}
		}
		remaining = append(remaining, fmt.Sprintf("%s/%s", namespace, child.GetName()))
	}

	// Also sweep the projected GlanceBackend children the ControlPlane placed in this
	// namespace. They carry no external state, so deleting them alongside the Glance
	// child is safe. Only c5c3-owned children carrying the glance child's name prefix
	// are touched — a hand-created GlanceBackend that merely shares the namespace is
	// never deleted. Each still-present owned backend is reported as remaining, the
	// same way the service children are, so the sweep waits for it to disappear.
	if cp.GlanceNamespace() == namespace {
		var backends glancev1alpha1.GlanceBackendList
		if err := r.List(ctx, &backends, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("listing GlanceBackends in %q for cross-namespace teardown: %w", namespace, err)
		}
		prefix := glanceBackendNamePrefix(cp)
		for i := range backends.Items {
			b := &backends.Items[i]
			if !isControlPlaneChild(b, cp) || !strings.HasPrefix(b.Name, prefix) {
				continue
			}
			if b.GetDeletionTimestamp().IsZero() {
				if err := client.IgnoreNotFound(
					r.Delete(ctx, b, client.PropagationPolicy(metav1.DeletePropagationBackground)),
				); err != nil {
					return nil, fmt.Errorf("deleting GlanceBackend %s/%s: %w", namespace, b.Name, err)
				}
			}
			remaining = append(remaining, fmt.Sprintf("%s/%s", namespace, b.Name))
		}
	}

	// The Barbican namespace carries two more sets. First the projected
	// BarbicanSecretStores, swept on the GlanceBackend terms: only c5c3-owned stores
	// carrying the Barbican child's name prefix, so a hand-created store attached to
	// the same Barbican is never deleted. The store the projection names today is
	// already in the wait set above; this catches one the reconcile-time prune never
	// removed, because a spec edit landing moments before the delete leaves a store
	// nobody names. An absent CRD (meta.IsNoMatchError) reads as nothing to sweep,
	// the same way deleteORCResources reads an absent K-ORC stack.
	if cp.BarbicanNamespace() == namespace {
		var stores barbicanv1alpha1.BarbicanSecretStoreList
		switch err := r.List(ctx, &stores, client.InNamespace(namespace)); {
		case err == nil:
			prefix := barbicanName(cp) + "-"
			for i := range stores.Items {
				store := &stores.Items[i]
				if store.Name == barbicanSecretStoreName(cp) {
					continue
				}
				if !isControlPlaneChild(store, cp) || !strings.HasPrefix(store.Name, prefix) {
					continue
				}
				if store.GetDeletionTimestamp().IsZero() {
					if derr := client.IgnoreNotFound(
						r.Delete(ctx, store, client.PropagationPolicy(metav1.DeletePropagationBackground)),
					); derr != nil {
						return nil, fmt.Errorf("deleting BarbicanSecretStore %s/%s: %w", namespace, store.Name, derr)
					}
				}
				remaining = append(remaining, fmt.Sprintf("%s/%s", namespace, store.Name))
			}
		case meta.IsNoMatchError(err):
		default:
			return nil, fmt.Errorf("listing BarbicanSecretStores in %q for cross-namespace teardown: %w", namespace, err)
		}

		// Then the rest of the dedicated OpenBao ensemble.
		if err := r.deleteBarbicanEnsembleIn(ctx, cp, namespace); err != nil {
			return nil, err
		}
	}
	return remaining, nil
}

// deleteBarbicanEnsembleIn deletes what the dedicated OpenBao ensemble leaves
// behind once its instance is on the way out: the tenant that admitted the
// namespace, the two transport Certificates, the provisioner ServiceAccount, the
// TokenRequest Role and RoleBinding, the static-seal Secret, and the cluster-scoped
// auth-delegator binding. The instance itself and the secret store in front of it
// are in the wait set (crossNamespaceServiceChildren), so nothing here can outrun
// them.
//
// The TENANT is the one gated object. It is what admits the namespace to the
// openbao-operator, and the instance's own finalizer runs under that admission, so
// it is held back while an instance this ControlPlane owns is still present. No
// requeue is needed for that: the instance is in the wait set, so the surrounding
// flow re-runs until it is gone and a later pass takes the tenant. A tenant this
// ControlPlane did not create is never touched — in the kind stack a proving tenant
// already admits the namespace, and deleting it would revoke an admission the
// operator does not own.
//
// The auth-delegator ClusterRoleBinding is cluster-scoped, so neither a namespace
// deletion nor an owner-reference cascade ever reclaims it. It is deleted here for
// the Managed lifecycle, by sweepExternalNamespaceResidue for the External one, and
// by deleteBarbicanAuthDelegatorBinding for the co-located default that reaches
// neither. Naming it more than once costs nothing: the second Get is a NotFound.
func (r *ControlPlaneReconciler) deleteBarbicanEnsembleIn(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, namespace string,
) error {
	name := barbicanOpenBaoName(cp)

	instanceHoldsTenant := false
	instance := &openbaov1alpha1.OpenBaoCluster{}
	switch err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
	case err != nil:
		return fmt.Errorf("checking whether OpenBaoCluster %q is gone before releasing its OpenBaoTenant: %w", name, err)
	default:
		instanceHoldsTenant = isControlPlaneChild(instance, cp)
	}

	// The tenant leads the ensemble, so holding it back is dropping that entry.
	objs := barbicanOpenBaoEnsembleObjects(name, namespace)
	if instanceHoldsTenant {
		objs = objs[1:]
	}

	for _, obj := range objs {
		if err := r.deleteOwnedEnsembleObject(ctx, cp, obj); err != nil {
			return err
		}
	}
	return nil
}

// deleteBarbicanAuthDelegatorBinding removes the cluster-scoped ClusterRoleBinding
// that lets a dedicated OpenBao instance run its TokenReviews. It is the one child
// the ControlPlane places outside every namespace, and the per-namespace sweeps
// reach it only when Barbican was assigned a namespace of its own. Co-located in
// the ControlPlane's own namespace it has no sweep at all: an owner reference
// cannot cross from a namespaced owner into cluster scope, so the GC cascade skips
// it too, and the binding would outlive the ControlPlane under a name a
// same-named ControlPlane later adopts.
//
// Only a binding carrying this ControlPlane's ownership labels is deleted. The name
// is cluster-wide, so a collision is not confined to one namespace.
//
// It is deleted on the cluster Barbican was placed on, where the ensemble wrote it,
// and on the management cluster for a Barbican that names none. A cluster that does
// not resolve leaves the binding where it is: teardownDedicatedNamespaces has
// already decided whether that cluster is being waited for or was abandoned, and
// this is the same unreachable cluster. A cluster that denies the delete leaves it
// too, reported as an event — the write grants are opt-in on the target's access
// chart, and no retry turns a withdrawn grant into a delete this ControlPlane may
// wait for.
func (r *ControlPlaneReconciler) deleteBarbicanAuthDelegatorBinding(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) error {
	children, _ := commonmulticluster.ResolveChildrenClientForDeletion(ctx, r.Resolver, r.Client,
		targetClusterRefForNamespace(cp, cp.BarbicanNamespace()), *cp.DeletionTimestamp)
	if children == nil {
		return nil
	}

	name := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace())
	binding := &rbacv1.ClusterRoleBinding{}
	// Uncached (see ControlPlaneReconciler.APIReader): this runs on EVERY
	// ControlPlane teardown, Barbican or not, and a cached read would install a
	// cluster-wide ClusterRoleBinding informer for one object.
	switch err := r.teardownReader(children).Get(ctx, types.NamespacedName{Name: name}, binding); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("getting ClusterRoleBinding %q for teardown: %w", name, err)
	}
	if !isControlPlaneChild(binding, cp) {
		return nil
	}
	if err := client.IgnoreNotFound(children.Delete(ctx, binding)); err != nil {
		// Forbidden is the read's failure one step further along, and the target
		// cluster's access chart makes it reachable: it grants get on
		// ClusterRoleBindings unconditionally, but create, patch and delete only
		// behind authDelegatorBinding. A cluster that had the flag on when this
		// binding was written and has it off now — a GitOps overlay, a decommission,
		// an upgrade that drops the override — answers the read with the binding and
		// the delete with a 403, on this pass and on every pass after it. Nothing
		// breaks that loop: the NamespaceTeardownStalled escape hatch lives inside
		// teardownDedicatedNamespaces, which has already returned done by the time
		// this runs. Wedging the ControlPlane in Terminating over a grant the target
		// cluster withdrew is the worse outcome, so it is released and what stays
		// behind is named — like the orphaned K-ORC resources, a repair-by-hand
		// outcome, and one that matters because a same-named ControlPlane would
		// otherwise adopt the binding left standing.
		if apierrors.IsForbidden(err) {
			r.Recorder.Event(cp, "Warning", "AuthDelegatorBindingNotReclaimed", fmt.Sprintf(
				"no permission to delete ClusterRoleBinding %q on the cluster Barbican was placed on; "+
					"releasing the ControlPlane anyway. It is cluster-scoped, so nothing will "+
					"garbage-collect it — remove it by hand, or set authDelegatorBinding=true on that "+
					"cluster's target-cluster-access release: %v", name, err))
			log.FromContext(ctx).Info("auth-delegator binding could not be reclaimed; releasing the "+
				"ControlPlane anyway", "clusterRoleBinding", name, "error", err.Error())
			return nil
		}
		return fmt.Errorf("deleting ClusterRoleBinding %q: %w", name, err)
	}
	return nil
}

// deleteManagedNamespace deletes a namespace the operator created, which cascades
// everything left in it. It refuses to delete one that does not carry our
// ownership labels: reconcileNamespaces never adopts a foreign namespace, and
// deleting one here would destroy every workload in it. That case can only arise
// from a webhook-bypassed CR or a namespace re-created out-of-band under the same
// name, so it is reported as a Warning rather than acted on.
//
// The delete is fire-and-observe: the namespace's own termination (Kubernetes
// reaping every object in it) can take a while, and holding the ControlPlane
// finalizer for it would gain nothing — the children the ControlPlane is
// responsible for are already gone by the time this runs.
//
// c selects the cluster: a placed namespace was created on the management cluster
// and on the target, so it is deleted once per cluster, each read and each
// ownership check answered by the cluster it is about to delete on.
//
// The read goes through that cluster's UNCACHED reader (teardownReader), as the
// adoption verdict on the same object does (ensureServiceNamespace). It is the
// read that decides whether a whole namespace — with the service's database, its
// PVC, and its tenant store in it — is deleted or leaked, and the marks it decides
// from are written on the target cluster by this very operator: a cache one resync
// behind would answer with a namespace that had not been stamped yet.
func (r *ControlPlaneReconciler) deleteManagedNamespace(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, name string,
) error {
	ns := &corev1.Namespace{}
	switch err := r.teardownReader(c).Get(ctx, types.NamespacedName{Name: name}, ns); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("getting managed service namespace %q for teardown: %w", name, err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		return nil
	}
	// On a target cluster the ownership labels are not proof enough: they name a
	// ControlPlane by name and namespace, and a cluster registered by two
	// management clusters can carry two of those. reapableManagedNamespace refuses
	// one whose UID mark names the other ControlPlane there — and, unlike the
	// adoption verdict, still reaps one whose mark was stripped, because this is the
	// last pass anything makes over it.
	if !reapableManagedNamespace(c, ns, cp) {
		r.Recorder.Event(cp, "Warning", "NamespaceNotOwned", fmt.Sprintf(
			"namespace %q does not carry this ControlPlane's ownership marks, so it was NOT deleted even though "+
				"its lifecycle is Managed; the operator never destroys a namespace it did not create", name,
		))
		return nil
	}
	if err := client.IgnoreNotFound(c.Delete(ctx, ns)); err != nil {
		return fmt.Errorf("deleting managed service namespace %q: %w", name, err)
	}
	log.FromContext(ctx).Info("deleted managed service namespace", "namespace", name)
	return nil
}

// sweepExternalNamespaceResidue deletes, best-effort, the objects the ControlPlane
// placed in a namespace it does NOT own — the namespace itself must survive, so
// nothing cascades and every object has to be named. The set is deterministic
// (every name is derived from the ControlPlane), so nothing has to be discovered:
// the backing services, the admin-password and Keystone DB-credential material, the
// Glance, Placement, Barbican, and Neutron DB-credential material, the Barbican
// secret store with the dedicated OpenBao ensemble behind it, the bus delivery the
// network service reads, and the tenant-store trio.
//
// The tenant-store trio goes LAST: the service children deleted before this ran
// their own ESO cleanup through that store, and an ESO PushSecret cannot purge its
// OpenBao path once the store it authenticates with is gone.
//
// Each object is ownership-checked against its live state, so a same-named object
// belonging to somebody else in this shared namespace is left alone. Errors are
// logged rather than propagated: this is the last step before the ControlPlane is
// released, and a residual object is a repairable leak, whereas an error here
// would wedge the namespace on a finalizer that can never clear.
//
// c is the client of the cluster the namespace lives on, so a placed namespace's
// residue is read and deleted there. It runs before the label-selected sweep of
// the same namespace, which reaches everything this one does not enumerate but
// has no order to give.
func (r *ControlPlaneReconciler) sweepExternalNamespaceResidue(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, namespace string,
) {
	logger := log.FromContext(ctx)

	unstructuredIn := func(gvk schema.GroupVersionKind, name string) client.Object {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetName(name)
		u.SetNamespace(namespace)
		return u
	}

	var objs []client.Object
	// The backing services this namespace's services resolved to.
	for _, inst := range r.managedInfraInstances(cp) {
		if inst.namespace != namespace {
			continue
		}
		switch inst.kind {
		case "MariaDB":
			objs = append(objs, &mariadbv1alpha1.MariaDB{
				ObjectMeta: metav1.ObjectMeta{Name: inst.name, Namespace: namespace},
			})
		case "Memcached":
			objs = append(objs, unstructuredIn(memcachedGVK, inst.name))
		}
	}
	// The credential material, which follows the Keystone service.
	if cp.KeystoneNamespace() == namespace {
		objs = append(
			objs,
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: adminPasswordSecretName(cp), Namespace: namespace,
			}},
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: dbCredentialSecretName(cp), Namespace: namespace,
			}},
			&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
				Name: dbCredentialSecretName(cp), Namespace: namespace,
			}},
			unstructuredIn(certificateGVK, dbCredentialClientCertName(cp)),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: dbCredentialServiceAccountName, Namespace: namespace,
			}},
		)
	}
	// The Glance credential material, which follows the Glance service: the
	// DB-credential ExternalSecret plus, in Dynamic mode, the VaultDynamicSecret
	// generator, its mTLS client Certificate, and the ServiceAccount whose token it
	// authenticates with.
	if cp.GlanceNamespace() == namespace {
		objs = append(
			objs,
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: glanceDBCredentialSecretName(cp), Namespace: namespace,
			}},
			&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
				Name: glanceDBCredentialSecretName(cp), Namespace: namespace,
			}},
			unstructuredIn(certificateGVK, glanceDBCredentialClientCertName(cp)),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: glanceDBCredentialServiceAccountName, Namespace: namespace,
			}},
		)
	}
	// The Placement credential material, which follows the Placement service, in the
	// same four shapes as Glance's above.
	if cp.PlacementNamespace() == namespace {
		objs = append(
			objs,
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: placementDBCredentialSecretName(cp), Namespace: namespace,
			}},
			&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
				Name: placementDBCredentialSecretName(cp), Namespace: namespace,
			}},
			unstructuredIn(certificateGVK, placementDBCredentialClientCertName(cp)),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: placementDBCredentialServiceAccountName, Namespace: namespace,
			}},
		)
	}
	// Everything that follows the Barbican service: the child and its secret store,
	// the dedicated OpenBao ensemble behind them (instance, tenant, transport
	// Certificates, provisioner account, TokenRequest grant, static-seal Secret, and
	// the cluster-scoped auth-delegator binding no namespace deletion reclaims), and
	// the DB-credential material in the same four shapes as Glance's above.
	if cp.BarbicanNamespace() == namespace {
		instance := barbicanOpenBaoName(cp)
		objs = append(
			objs,
			&barbicanv1alpha1.Barbican{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanName(cp), Namespace: namespace,
			}},
			&barbicanv1alpha1.BarbicanSecretStore{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanSecretStoreName(cp), Namespace: namespace,
			}},
			&openbaov1alpha1.OpenBaoCluster{ObjectMeta: metav1.ObjectMeta{
				Name: instance, Namespace: namespace,
			}},
		)
		objs = append(objs, barbicanOpenBaoEnsembleObjects(instance, namespace)...)
		objs = append(
			objs,
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanDBCredentialSecretName(cp), Namespace: namespace,
			}},
			&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanDBCredentialSecretName(cp), Namespace: namespace,
			}},
			unstructuredIn(certificateGVK, barbicanDBCredentialClientCertName(cp)),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanDBCredentialServiceAccountName, Namespace: namespace,
			}},
		)
	}
	// The Neutron credential material, which follows the network service, in the
	// same four shapes as Glance's above, plus the bus delivery the ControlPlane
	// wrote beside the child: the brownfield transport-URL Secret and the CA
	// mirror. The OVNCentral the child references is NOT in here, and is deleted
	// nowhere: it is deployed outside the plane and only read (see reconcileOVN).
	if cp.NeutronNamespace() == namespace {
		objs = append(
			objs,
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: neutronDBCredentialSecretName(cp), Namespace: namespace,
			}},
			&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
				Name: neutronDBCredentialSecretName(cp), Namespace: namespace,
			}},
			unstructuredIn(certificateGVK, neutronDBCredentialClientCertName(cp)),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: neutronDBCredentialServiceAccountName, Namespace: namespace,
			}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: neutronMessagingSecretName(cp), Namespace: namespace,
			}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: neutronMessagingCASecretName(cp), Namespace: namespace,
			}},
		)
	}
	// The tenant store LAST: everything above authenticated through it.
	objs = append(
		objs,
		&esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: namespace}},
		unstructuredIn(certificateGVK, esoTenantClientCertName),
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: esoTenantServiceAccountName, Namespace: namespace,
		}},
	)

	reader := r.teardownReader(c)
	for _, obj := range objs {
		key := client.ObjectKeyFromObject(obj)
		// Uncached: the Barbican arm above adds the ensemble's three RBAC kinds
		// to this list (see ControlPlaneReconciler.APIReader), and a one-shot
		// teardown read has nothing to gain from an informer anyway.
		switch err := reader.Get(ctx, key, obj); {
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			continue
		case err != nil:
			logger.V(1).Info("best-effort residue sweep could not read an object",
				"object", key, "error", err.Error())
			continue
		}
		if !isControlPlaneChild(obj, cp) {
			continue
		}
		if err := client.IgnoreNotFound(c.Delete(ctx, obj)); err != nil {
			logger.V(1).Info("best-effort residue sweep could not delete an object",
				"object", key, "error", err.Error())
		}
	}
}

// deleteRegistrationsBeforeTeardown deletes the projected KeystoneService
// registrations and reports whether reconcileDelete must stop on them this pass,
// with the (result, error) it returns when it does.
//
// It runs before deleteORCResources for the reason deleteOwnedPushSecrets runs
// before the release: the cleanup it triggers only works while what it
// authenticates through is still alive. Here that is the whole identity plane —
// the KeystoneService controller drives the registration's own K-ORC CRs, and
// they resolve the admin credential the very next step revokes.
//
// The wait is bounded by registrationTeardownStallTimeout, a budget of its OWN
// rather than a share of the one the K-ORC and namespace sweeps hold: it blocks
// both of them, so a single deadline measured from the deletion timestamp would
// let an unreachable Keystone spend the whole window here and leave the admin
// credential's revocation with no time at all — orcTeardownDeadline adds this
// window on top for exactly that reason. Past it the trade is the one every stall
// escape in this teardown makes: a registration that cannot finish leaves a
// repairable leak, whereas waiting forever leaves a ControlPlane nobody can
// delete. The release force-strips the children of a registration given up on here
// (releaseStalledRegistrationChildren), so none of them is left Terminating, and
// the Warning names what stayed so the operator knows which Keystone rows to
// repair.
func (r *ControlPlaneReconciler) deleteRegistrationsBeforeTeardown(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, bool, error) {
	remaining, err := r.deleteProjectedRegistrations(ctx, cp)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if len(remaining) == 0 {
		return ctrl.Result{}, false, nil
	}

	if time.Since(cp.DeletionTimestamp.Time) <= registrationTeardownStallTimeout {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "FinalizingServiceRegistrations",
			Message: fmt.Sprintf(
				"waiting for %d projected KeystoneService registration(s) to finish their teardown before "+
					"revoking the admin credential: %v", len(remaining), remaining),
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, true, nil
	}

	r.Recorder.Event(cp, "Warning", "ServiceRegistrationTeardownStalled", fmt.Sprintf(
		"KeystoneService registration(s) %v stayed present longer than %s; continuing the ControlPlane teardown "+
			"anyway. Their K-ORC CRs still hold the catalog rows and service users they registered, and once this "+
			"ControlPlane is gone nothing can remove them — delete them from Keystone by hand",
		remaining, registrationTeardownStallTimeout))
	log.FromContext(ctx).Info("KeystoneService registration teardown stalled; continuing the ControlPlane teardown",
		"registrations", remaining, "stallTimeout", registrationTeardownStallTimeout)
	return ctrl.Result{}, false, nil
}

// projectedRegistrationKeys returns the keys of the KeystoneService registrations
// this ControlPlane projects for its built-in services. The teardown sweep and the
// force-release of a stalled registration's children walk the same list.
//
// The names are enumerated whether or not the spec still declares the service:
// dropping a services.<svc> block without the deletion opt-in PRESERVES the
// registration (the same reserve the reconcile-time sweeps make for the service
// children), and a preserved registration still has to come down with the plane.
// <Svc>Namespace() resolves correctly in both cases — for a service placed in a
// dedicated namespace the block cannot be dropped at all
// (validateServiceNamespacesImmutable), so the fallback to the ControlPlane's own
// namespace is only ever taken for a co-located one.
func projectedRegistrationKeys(cp *c5c3v1alpha1.ControlPlane) []client.ObjectKey {
	return []client.ObjectKey{
		{Name: glanceName(cp), Namespace: cp.GlanceNamespace()},
		{Name: placementName(cp), Namespace: cp.PlacementNamespace()},
		{Name: barbicanName(cp), Namespace: cp.BarbicanNamespace()},
		{Name: neutronName(cp), Namespace: cp.NeutronNamespace()},
	}
}

// presentProjectedRegistrations returns the projected registrations
// (projectedRegistrationKeys) still present on the cluster — the list both the
// teardown sweep and the force-release of a stalled registration's children walk.
//
// The registration always lives on the MANAGEMENT cluster, beside the
// ControlPlane, whatever cluster its service runs on — so one client reaches all
// of them. Ownership is re-checked against the live object, so a same-named
// KeystoneService this ControlPlane did not project is never returned, and neither
// walker ever touches one; that also covers an External-lifecycle namespace, whose
// registration nothing else reaches (no owner reference to cascade from, and the
// namespace itself is never deleted).
func (r *ControlPlaneReconciler) presentProjectedRegistrations(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]*c5c3v1alpha1.KeystoneService, error) {
	var present []*c5c3v1alpha1.KeystoneService
	for _, key := range projectedRegistrationKeys(cp) {
		ks := &c5c3v1alpha1.KeystoneService{}
		switch err := r.Get(ctx, key, ks); {
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("getting KeystoneService %s for teardown: %w", key, err)
		}
		if !isControlPlaneChild(ks, cp) {
			continue
		}
		present = append(present, ks)
	}
	return present, nil
}

// deleteProjectedRegistrations issues an idempotent Delete on every projected
// KeystoneService registration still present (presentProjectedRegistrations) and
// returns those that survive it, as "namespace/name".
func (r *ControlPlaneReconciler) deleteProjectedRegistrations(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]string, error) {
	present, err := r.presentProjectedRegistrations(ctx, cp)
	if err != nil {
		return nil, err
	}

	var remaining []string
	for _, ks := range present {
		key := client.ObjectKeyFromObject(ks)
		if ks.DeletionTimestamp.IsZero() {
			if err := client.IgnoreNotFound(r.Delete(ctx, ks)); err != nil {
				return nil, fmt.Errorf("deleting KeystoneService %s: %w", key, err)
			}
		}
		// Re-Get: a registration that had nothing to tear down carries no finalizer
		// and is gone with the Delete, so waiting a whole requeue cycle on it would
		// delay the teardown for nothing.
		switch err := r.Get(ctx, key, &c5c3v1alpha1.KeystoneService{}); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, fmt.Errorf("re-checking KeystoneService %s: %w", key, err)
		default:
			remaining = append(remaining, key.String())
		}
	}
	return remaining, nil
}

// releaseStalledRegistrationChildren force-releases the children of every projected
// KeystoneService registration still present at a release point, and reports whether
// it is done — false while a PushSecret still owes ESO the OpenBao purge its
// DeletionPolicy asks for, which the caller waits out with a requeue.
//
// A registration still standing here is one the whole registrationTeardownStallTimeout
// window was spent on. Its own controller strips no K-ORC and no ESO finalizer, on
// either of its paths, so on a teardown where Keystone is already gone its label-owned
// K-ORC CRs and its PushSecrets stay Terminating behind finalizers that can never
// complete, with nothing left to collect them once the ControlPlane is released. This
// deletes those children, strips the finalizers that cannot complete, and names in a
// Warning what that abandons in Keystone and in OpenBao.
//
// The registration's own c5c3.io/keystoneservice-teardown finalizer is deliberately
// left on: with its children gone the KeystoneService controller releases it itself,
// on the patient path while the ControlPlane is still present, or on the fail-open
// path afterwards.
func (r *ControlPlaneReconciler) releaseStalledRegistrationChildren(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (bool, error) {
	present, err := r.presentProjectedRegistrations(ctx, cp)
	if err != nil {
		return false, err
	}

	var registrations, orphaned, openBaoPaths []string
	done := true

	for _, ks := range present {
		key := client.ObjectKeyFromObject(ks)

		// The kinds sweepChildren lists, in its order. A List that fails because the
		// CRD is absent reads as nothing to release on that kind, the posture
		// deleteORCResources and deleteOwnedPushSecretsIn already take.
		inChildNS := client.InNamespace(keystoneServiceChildNamespace(cp))
		owned := client.MatchingLabels(keystoneServiceChildLabels(ks))
		korcLists := []client.ObjectList{
			&orcv1alpha1.RoleAssignmentList{},
			&orcv1alpha1.RoleList{},
			&orcv1alpha1.EndpointList{},
			&orcv1alpha1.ServiceList{},
			&orcv1alpha1.UserList{},
			&orcv1alpha1.ProjectList{},
			&orcv1alpha1.DomainList{},
		}

		// ownsKeystoneServiceChild, not the label selector alone: the labels are plain
		// labels whose values derive from published CR names, so anything in the
		// namespace can carry them (a Kustomize `labels:` block applied one directory
		// too wide, a hand-copied import manifest). BOTH the ownership test and the
		// name test have to pass before a strip reaches an object — the invariant every
		// other sweep over these children enforces.
		var korcObjs []client.Object
		for _, list := range korcLists {
			if err := r.List(ctx, list, inChildNS, owned); err != nil && !meta.IsNoMatchError(err) {
				return false, fmt.Errorf("listing the %T children of KeystoneService %s for release: %w",
					list, key, err)
			}
			items, err := meta.ExtractList(list)
			if err != nil {
				return false, fmt.Errorf("reading the %T children of KeystoneService %s for release: %w",
					list, key, err)
			}
			for _, item := range items {
				obj, ok := item.(client.Object)
				if !ok || !ownsKeystoneServiceChild(ks, obj) {
					continue
				}
				korcObjs = append(korcObjs, obj)
			}
		}

		// Delete FIRST, and strip only afterwards. This is the one strip in the
		// teardown that can land on a LIVE object: a registration whose controller
		// never ran its own teardown still has children with no deletionTimestamp,
		// and stripping one of those is a write K-ORC sees as an ordinary update, so
		// it re-adds its finalizer and the Delete behind it wedges the CR Terminating
		// against a credential this teardown already revoked — the very wedge this
		// function exists to prevent. A deletionTimestamp is what closes that window:
		// no controller adds a finalizer back to an object already being deleted. A
		// child that was already Terminating is usually gone by now, and its Delete
		// is a no-op.
		for _, obj := range korcObjs {
			if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
				return false, fmt.Errorf("deleting %T %s of KeystoneService %s: %w",
					obj, client.ObjectKeyFromObject(obj), key, err)
			}
		}

		// The registration's PushSecret carries DeletionPolicy=Delete, so ESO purges
		// the mirrored OpenBao data while it processes the deletion. Delete and then
		// WAIT for it, the way deleteOwnedPushSecrets waits for the ControlPlane's
		// own: the budget that brought this release here bounds the REGISTRATION's
		// teardown — dominated by K-ORC revoking against Keystone — and says nothing
		// about ESO, so stripping the finalizer now would forfeit a purge that may
		// well be in flight, and of a credential K-ORC could not revoke either.
		pushStuck, err := r.deleteRegistrationPushSecrets(ctx, ks)
		if err != nil {
			return false, err
		}
		if len(pushStuck) > 0 && time.Since(cp.DeletionTimestamp.Time) <= orcTeardownDeadline {
			// Past the deadline the give-up posture applies and the strip below runs;
			// until then let ESO finish, and report this registration on the pass that
			// actually releases it rather than once per pass.
			//
			// Only while the store the purge authenticates through is still standing,
			// though. A registration in a DEDICATED namespace reaches this point only
			// after sweepNamespacesBeforeRelease deleted that store (Managed: the whole
			// namespace, which the PushSecret's own finalizer then holds Terminating),
			// so waiting buys nothing and only defers the identical give-up outcome by
			// the rest of the deadline. Co-located — the default — the store sits in the
			// ControlPlane's own namespace, which no sweep touches before the release,
			// so it is standing here and the wait is real.
			if r.registrationStoreAlive(ctx, cp, ks.Namespace) {
				done = false
				continue
			}
		}

		// Classify BEFORE the strip, which clears the very finalizers this reads. A
		// child counts as released when this pass changed it: one carrying a K-ORC
		// finalizer the strip removes, or a live one whose Delete above started a
		// teardown its own controller never ran. A child already Terminating with
		// nothing left to strip is on its way out on its own, and reporting it would
		// alarm about a teardown that is working.
		//
		// A MANAGED K-ORC CR whose finalizer is stripped leaves its OpenStack resource
		// behind with no Kubernetes object naming it, while an unmanaged import (a Role
		// import, a collision probe) is CR-only and orphans nothing: the same line the
		// ControlPlane's own ORCResourcesOrphaned draws.
		released := false
		for _, obj := range korcObjs {
			_, stripped := withoutKORCFinalizers(obj.GetFinalizers())
			if !stripped && !obj.GetDeletionTimestamp().IsZero() {
				continue
			}
			released = true
			if stripped && isManagedORCChild(obj) {
				orphaned = append(orphaned, orcChildOpenStackRef(obj))
			}
		}
		if err := r.patchAwayKORCFinalizers(ctx, korcObjs); err != nil {
			return false, err
		}

		for _, ps := range pushStuck {
			if len(ps.Finalizers) == 0 {
				// Terminating with nothing holding it: ESO finished the purge and the
				// API server is about to reap it. Nothing to strip, nothing left behind.
				continue
			}
			released = true
			for _, d := range ps.Spec.Data {
				openBaoPaths = append(openBaoPaths, d.Match.RemoteRef.RemoteKey)
			}
			// Already Terminating, so clearing the finalizers completes the deletion.
			ps.Finalizers = nil
			if err := r.Update(ctx, ps); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("force-removing finalizers from PushSecret %q: %w", ps.Name, err)
			}
		}

		if released {
			registrations = append(registrations, key.String())
		}
	}

	// Either no registration was left, or every child of the ones left was already
	// on its way out. There is nothing to report.
	if len(registrations) == 0 {
		return done, nil
	}

	log.FromContext(ctx).Info("released the children of stalled KeystoneService registrations",
		"registrations", registrations, "orphaned", orphaned, "openBaoPaths", openBaoPaths)

	// Name only what was actually abandoned. A release that reached nothing but live
	// children and unmanaged imports abandons neither a Keystone row nor an OpenBao
	// path, and a Warning listing two empty sets would send an operator looking for
	// a leak that is not there — the same reserve the ControlPlane's own escape takes
	// with its ORCImportsReleased.
	var abandoned []string
	if len(orphaned) > 0 {
		abandoned = append(abandoned, fmt.Sprintf(
			"the OpenStack resource(s) %v behind the released managed K-ORC CRs were NOT deleted", orphaned))
	}
	if len(openBaoPaths) > 0 {
		abandoned = append(abandoned, fmt.Sprintf(
			"the OpenBao path(s) %v keep the data of the released PushSecret(s)", openBaoPaths))
	}
	if len(abandoned) == 0 {
		r.Recorder.Event(cp, "Normal", "ServiceRegistrationChildrenReleased", fmt.Sprintf(
			"released the children of the KeystoneService registration(s) %v so the ControlPlane can be deleted: "+
				"nothing was abandoned — no managed K-ORC CR held an OpenStack resource this release had to "+
				"strip a finalizer from, and no PushSecret had data left in OpenBao",
			registrations))
		return done, nil
	}
	r.Recorder.Event(cp, "Warning", "ServiceRegistrationResourcesOrphaned", fmt.Sprintf(
		"force-released the children of the KeystoneService registration(s) %v so the ControlPlane can be "+
			"deleted: %s. Nothing in Kubernetes names them any more, so remove them by hand",
		registrations, strings.Join(abandoned, ", and ")))
	return done, nil
}

// registrationStoreAlive reports whether the SecretStore a registration's
// PushSecret purges OpenBao through is still standing in namespace ns. It is the
// gate on waiting for that purge: ESO cannot authenticate against a store that is
// gone or already Terminating, so a wait past it is a wait nothing can end.
//
// A ClusterSecretStore is cluster-scoped and outlives every namespace sweep, so it
// always reads as alive. An absent PushSecret/SecretStore CRD reads as no store, the
// same posture the sweeps around this take.
//
// A read that cannot answer at all — Forbidden, a conversion webhook that is down,
// a 503 from the API path serving external-secrets.io/v1 — reads as ALIVE. This is
// only ever a decision about whether to keep waiting, and the pre-probe behaviour on
// this path was to wait; failing open reproduces it and leaves orcTeardownDeadline
// to end the wait. Returning the error instead would abort the release pass for
// every remaining registration and push the give-up out past that deadline on
// reconcile backoff — the posture every other read on this give-up path already
// avoids (see sweepRegistrationTenantStores, deleteESOTenantStoreTrioIn).
func (r *ControlPlaneReconciler) registrationStoreAlive(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, ns string,
) bool {
	ref := effectiveControlPlaneStoreRef(cp)
	if ref.Kind != commonv1.SecretStoreKindNamespaced {
		return true
	}
	var store esov1.SecretStore
	switch err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ns}, &store); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return false
	case err != nil:
		log.FromContext(ctx).V(1).Info("could not read the tenant store before waiting for an ESO purge; "+
			"assuming it stands and letting orcTeardownDeadline bound the wait",
			"namespace", ns, "store", ref.Name, "error", err.Error())
		return true
	}
	return store.DeletionTimestamp.IsZero()
}

// deleteRegistrationPushSecrets deletes the PushSecret children ks owns and returns
// those still present afterwards, re-read so their resourceVersion survives a later
// strip. A PushSecret ESO has finished with is gone with the Delete; one it still
// holds stays present until the remote delete is confirmed. An absent PushSecret CRD
// reads as nothing to release, the posture deleteOwnedPushSecretsIn already takes.
func (r *ControlPlaneReconciler) deleteRegistrationPushSecrets(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService,
) ([]*esov1alpha1.PushSecret, error) {
	var list esov1alpha1.PushSecretList
	if err := r.List(ctx, &list, client.InNamespace(ks.Namespace),
		client.MatchingLabels(keystoneServiceChildLabels(ks))); err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("listing the PushSecret children of KeystoneService %s for release: %w",
			client.ObjectKeyFromObject(ks), err)
	}

	var stuck []*esov1alpha1.PushSecret
	for i := range list.Items {
		ps := &list.Items[i]
		// The name guard the K-ORC children get, for the same reason.
		if !ownsKeystoneServiceChild(ks, ps) {
			continue
		}
		if ps.DeletionTimestamp.IsZero() {
			if err := client.IgnoreNotFound(r.Delete(ctx, ps)); err != nil {
				return nil, fmt.Errorf("deleting PushSecret %q of KeystoneService %s: %w",
					ps.Name, client.ObjectKeyFromObject(ks), err)
			}
		}
		current := &esov1alpha1.PushSecret{}
		switch err := r.Get(ctx, client.ObjectKeyFromObject(ps), current); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, fmt.Errorf("re-checking PushSecret %q of KeystoneService %s: %w",
				ps.Name, client.ObjectKeyFromObject(ks), err)
		default:
			stuck = append(stuck, current)
		}
	}
	return stuck, nil
}

// orcChildOpenStackRef names the OpenStack resource behind a registration's K-ORC
// CR, which is what an operator has to find in Keystone to repair a leak. The CR
// name cannot serve: a registration's children are named by
// keystoneServiceChildPrefix, whose sha256 segment appears nowhere in OpenStack,
// and the CR itself is deleted in the same pass that reports it.
//
// An Endpoint and a RoleAssignment have no name of their own — the first is
// identified by its service and interface, the second by the binding it makes —
// so both fall back to the CR name, as does any kind whose resource name is
// unset, which is the name K-ORC defaults such a resource to.
func orcChildOpenStackRef(obj client.Object) string {
	var kind, name string
	switch o := obj.(type) {
	case *orcv1alpha1.User:
		kind = "user"
		if o.Spec.Resource != nil && o.Spec.Resource.Name != nil {
			name = string(*o.Spec.Resource.Name)
		}
	case *orcv1alpha1.Project:
		kind = "project"
		if o.Spec.Resource != nil && o.Spec.Resource.Name != nil {
			name = string(*o.Spec.Resource.Name)
		}
	case *orcv1alpha1.Domain:
		kind = "domain"
		if o.Spec.Resource != nil && o.Spec.Resource.Name != nil {
			name = string(*o.Spec.Resource.Name)
		}
	case *orcv1alpha1.Service:
		kind = "catalog service"
		if o.Spec.Resource != nil && o.Spec.Resource.Name != nil {
			name = string(*o.Spec.Resource.Name)
		}
	case *orcv1alpha1.Role:
		kind = "role"
		if o.Spec.Resource != nil && o.Spec.Resource.Name != nil {
			name = string(*o.Spec.Resource.Name)
		}
	case *orcv1alpha1.Endpoint:
		kind = "catalog endpoint"
	case *orcv1alpha1.RoleAssignment:
		kind = "role assignment"
	default:
		kind = fmt.Sprintf("%T", obj)
	}
	return kind + " " + cmp.Or(name, obj.GetName())
}

// deleteOwnedPushSecrets issues an idempotent Delete on every PushSecret this
// ControlPlane owns and returns those still present after the sweep. The owned
// PushSecrets carry DeletionPolicy=Delete, so ESO deletes the mirrored OpenBao
// data while processing their deletion — which only works while the per-tenant
// SecretStore and its eso-tenant-auth ServiceAccount are still alive, i.e.
// BEFORE the ControlPlane finalizer is released and the GC cascade reaps them.
// An absent PushSecret CRD (meta.IsNoMatchError) reads as nothing-to-clean so
// the finalizer can still release when the ESO stack is gone.
//
// It sweeps every namespace the ControlPlane occupies and matches
// isControlPlaneChild, not a bare controller reference: a service-account
// credential delivered into a dedicated service namespace carries the ownership
// labels rather than an owner reference, and its DeletionPolicy=Delete OpenBao
// purge must run while that namespace's tenant store is still alive — which the
// existing ordering guarantees, since this runs before teardownDedicatedNamespaces
// (and its per-namespace tenant-store sweep).
//
// A placed namespace is swept on its target cluster as well as at home: the
// PushSecrets are written there, and so is the tenant store their purge
// authenticates through, so the ordering has to hold on that cluster too. The
// cluster is resolved through the DELETION resolver, which never fails the pass —
// one that does not resolve is left to teardownDedicatedNamespaces, which is where
// waiting for it and abandoning it are decided.
func (r *ControlPlaneReconciler) deleteOwnedPushSecrets(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]pendingPushSecret, error) {
	var remaining []pendingPushSecret
	for _, namespace := range controlPlaneNamespaces(cp) {
		children, _ := commonmulticluster.ResolveChildrenClientForDeletion(ctx, r.Resolver, r.Client,
			targetClusterRefForNamespace(cp, namespace), *cp.DeletionTimestamp)
		for _, c := range r.clustersFor(children) {
			left, err := r.deleteOwnedPushSecretsIn(ctx, c, cp, namespace)
			if err != nil {
				return nil, err
			}
			remaining = append(remaining, left...)
		}
	}
	return remaining, nil
}

// pendingPushSecret is one PushSecret still present after the teardown sweep,
// paired with the client of the cluster it lives on. The stall escape strips its
// finalizers through that client: a PushSecret in a placed namespace exists on
// the target cluster alone, so an Update issued at home would reach no object.
type pendingPushSecret struct {
	pushSecret *esov1alpha1.PushSecret
	cluster    client.Client
}

// deleteOwnedPushSecretsIn is deleteOwnedPushSecrets for one namespace on the
// cluster c reads and writes, and returns the PushSecrets still present there
// afterwards. An absent PushSecret CRD (meta.IsNoMatchError) reads as nothing to
// clean on THAT cluster: a target that does not serve ESO can hold no PushSecret,
// and the management cluster's sweep is unaffected either way.
func (r *ControlPlaneReconciler) deleteOwnedPushSecretsIn(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, namespace string,
) ([]pendingPushSecret, error) {
	var list esov1alpha1.PushSecretList
	switch err := c.List(ctx, &list, client.InNamespace(namespace)); {
	case err == nil:
	case meta.IsNoMatchError(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("listing owned PushSecrets in namespace %q for teardown: %w", namespace, err)
	}

	var remaining []pendingPushSecret
	for i := range list.Items {
		ps := &list.Items[i]
		if !isControlPlaneChild(ps, cp) {
			continue
		}
		if ps.DeletionTimestamp.IsZero() {
			if err := client.IgnoreNotFound(c.Delete(ctx, ps)); err != nil {
				return nil, fmt.Errorf("deleting PushSecret %q: %w", ps.Name, err)
			}
		}
		// Re-Get: a finalizer-less PushSecret is gone with the Delete, while one
		// held by ESO stays present until the remote delete is confirmed.
		current := &esov1alpha1.PushSecret{}
		switch err := c.Get(ctx, client.ObjectKeyFromObject(ps), current); {
		case err == nil:
			remaining = append(remaining, pendingPushSecret{pushSecret: current, cluster: c})
		case apierrors.IsNotFound(err):
		default:
			return nil, fmt.Errorf("re-checking PushSecret %q: %w", ps.Name, err)
		}
	}
	return remaining, nil
}

// deleteORCResources issues an idempotent Delete on every owned K-ORC CR
// (orcChildObjects) and returns those still present after the sweep.
// hasLiveWork reports whether any CR was observed live (present,
// DeletionTimestamp unset) — that is the one-shot signal for the FinalizingORC
// event. NotFound and "CRD not installed" (meta.IsNoMatchError) are tolerated as
// already-gone so the finalizer can still release when the K-ORC stack is absent.
func (r *ControlPlaneReconciler) deleteORCResources(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (remaining []client.Object, hasLiveWork bool, err error) {
	logger := log.FromContext(ctx)
	ns := childNamespace(cp)
	children := orcChildObjects(cp)

	// Pass 1: classify and delete. A present CR with no DeletionTimestamp is the
	// live work whose Delete starts the teardown.
	for _, child := range children {
		obj := child.newObj()
		key := client.ObjectKey{Name: child.name, Namespace: ns}
		getErr := r.Get(ctx, key, obj)
		if apierrors.IsNotFound(getErr) || meta.IsNoMatchError(getErr) {
			continue
		}
		if getErr != nil {
			return nil, false, fmt.Errorf("getting %T %s: %w", obj, key, getErr)
		}
		if obj.GetDeletionTimestamp().IsZero() {
			hasLiveWork = true
		}
		if delErr := r.Delete(ctx, obj); delErr != nil {
			if apierrors.IsNotFound(delErr) || meta.IsNoMatchError(delErr) {
				continue
			}
			return nil, false, fmt.Errorf("deleting %T %s: %w", obj, key, delErr)
		}
	}

	// Pass 2: collect the CRs still present after the deletes. Re-Get so the
	// returned objects carry current finalizers for a possible force-remove.
	for _, child := range children {
		obj := child.newObj()
		key := client.ObjectKey{Name: child.name, Namespace: ns}
		getErr := r.Get(ctx, key, obj)
		if apierrors.IsNotFound(getErr) || meta.IsNoMatchError(getErr) {
			continue
		}
		if getErr != nil {
			return nil, false, fmt.Errorf("re-checking %T %s: %w", obj, key, getErr)
		}
		remaining = append(remaining, obj)
		logger.V(1).Info("K-ORC CR still present during ControlPlane teardown",
			"resource", fmt.Sprintf("%T", obj), "name", key.Name)
	}

	return remaining, hasLiveWork, nil
}

// isManagedORCChild reports whether force-removing obj's K-ORC finalizer abandons
// the OpenStack resource behind it. It answers by ManagementPolicy — the field that
// decides what a Delete does to the external installation, see orcChildObjects — and
// it fails LOUD: anything not explicitly Unmanaged counts as managed, so an unset
// policy (K-ORC defaults it to `managed`) or a kind added later is reported as a leak
// rather than silently omitted from the warning.
func isManagedORCChild(obj client.Object) bool {
	switch o := obj.(type) {
	case *orcv1alpha1.ApplicationCredential:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.Service:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.Endpoint:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.User:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.Domain:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.Project:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.RoleAssignment:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.Role:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	default:
		return true
	}
}

// forceRemoveKORCFinalizers strips every openstack.k-orc.cloud/* finalizer from
// the given objects, preserving any non-K-ORC finalizers, and persists the
// change. Removing the last finalizer on an already-Terminating CR lets the API
// server complete its deletion. NotFound is tolerated (GC won the race).
func (r *ControlPlaneReconciler) forceRemoveKORCFinalizers(ctx context.Context, remaining []client.Object) error {
	for _, obj := range remaining {
		kept, removed := withoutKORCFinalizers(obj.GetFinalizers())
		if !removed {
			continue
		}
		obj.SetFinalizers(kept)
		if err := r.Update(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("force-removing K-ORC finalizers from %T %s: %w",
				obj, client.ObjectKeyFromObject(obj), err)
		}
	}
	return nil
}

// patchAwayKORCFinalizers is forceRemoveKORCFinalizers for objects whose
// resourceVersion the caller can no longer trust, because it issued a Delete
// against them first: it strips through a merge patch, which carries no
// resourceVersion, so the deletionTimestamp the API server just stamped does not
// make the listed object conflict. releaseStalledRegistrationChildren needs that
// order — see the delete-first reasoning there.
func (r *ControlPlaneReconciler) patchAwayKORCFinalizers(ctx context.Context, objs []client.Object) error {
	for _, obj := range objs {
		kept, removed := withoutKORCFinalizers(obj.GetFinalizers())
		if !removed {
			continue
		}
		patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
		obj.SetFinalizers(kept)
		if err := r.Patch(ctx, obj, patch); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("force-removing K-ORC finalizers from %T %s: %w",
				obj, client.ObjectKeyFromObject(obj), err)
		}
	}
	return nil
}

// withoutKORCFinalizers returns finalizers minus every openstack.k-orc.cloud/* one,
// and reports whether any was dropped.
func withoutKORCFinalizers(finalizers []string) (kept []string, removed bool) {
	kept = make([]string, 0, len(finalizers))
	for _, f := range finalizers {
		if strings.HasPrefix(f, korcFinalizerPrefix) {
			removed = true
			continue
		}
		kept = append(kept, f)
	}
	return kept, removed
}
