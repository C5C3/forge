// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// The ControlPlane reconciler watches kinds owned by sibling service operators —
// Keystone, Horizon, Glance, Placement, Barbican, Neutron and OVNCentral — plus
// the two openbao.org kinds a dedicated Barbican secret store is built from and
// the rabbitmq.com RabbitmqCluster kind. controller-runtime installs a shared
// informer for each watched kind and blocks manager start until every informer
// has synced; on a cluster missing one of those CRDs the informer never syncs,
// the manager fails start after CacheSyncTimeout, and the leader crash-loops.
// The helpers here let SetupWithManager register the fragile watches only when their
// CRD is actually served, so a slimmed-down install (Keystone-only, no Glance) starts
// clean. The infrastructure hard dependencies — MariaDB, Memcached, the ESO kinds
// and the eight K-ORC kinds — are deliberately NOT guarded: every reconcile pass
// reads them unconditionally, so their absence must fail fast rather than defer to a
// wedged reconcile (see optionalWatchObjects).
//
// The RabbitmqCluster kind is guarded for a reason of its own rather than the
// sibling-service one: messaging is opt-in, so spec.infrastructure.messaging is
// never materialized by defaulting and a Keystone-only install on a cluster without
// the rabbitmq-cluster-operator starts clean. A ControlPlane that does declare
// managed messaging on such a cluster fails closed in ensureRabbitMQ, with
// InfrastructureReady False and reason RabbitMQError, so the guard hides nothing.
//
// Presence is decided by a discovery probe against the live API server rather than by
// checking the runtime scheme: the scheme is compiled in and always advertises every
// type the binary knows about, so it says nothing about whether the cluster has the
// CRD installed. Discovery is the only source of truth for what the API server will
// actually serve a watch for.
//
// A CRD installed after start cannot be picked up in place — controller-runtime cannot
// add a watch to a running manager — so the recovery mechanism is a process restart:
// crdWatchGate detects the newly served CRD and returns an error that tears down
// mgr.Start, letting the Deployment restart the leader with the watch now registered.

// serverResourcesLister is the narrow slice of the discovery API the presence probe
// needs. It is defined here, at the consumer, so the probe depends on one method
// rather than the whole discovery.DiscoveryInterface: *discovery.DiscoveryClient
// satisfies it in production and a lightweight stub satisfies it in unit tests.
type serverResourcesLister interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// optionalWatchObjects returns one instance of every kind whose CRD is owned by
// another operator and may therefore be absent when the c5c3 operator starts.
// These are exactly the watch legs SetupWithManager must guard behind a
// discovery probe; the mandatory kinds the c5c3 operator ships itself (ControlPlane)
// and the infrastructure kinds it hard-depends on — MariaDB, Memcached, the ESO
// kinds and the eight K-ORC kinds — are intentionally not listed. K-ORC in
// particular is read unconditionally by every reconcile pass (reconcileKORC, the
// registration legs), so a missing K-ORC CRD is a fail-fast startup error,
// not a slimmable state that a guarded watch could paper over.
//
// The two openbao.org kinds are listed for the same reason the sibling service
// kinds are, one step removed: the openbao-operator that serves them is installed
// for a Barbican taking a dedicated secret store, and reconcileBarbican reads them
// only on that path (ensureBarbicanOpenBao), so a ControlPlane without one runs
// perfectly well on a cluster that never served them.
//
// The Neutron and OVNCentral kinds are listed for the sibling reason too: the
// neutron-operator and the ovn-operator are installed only for a ControlPlane that
// runs a network service, so a plane without one runs on a cluster that never
// served them.
//
// The RabbitmqCluster kind is listed for a reason of its own: messaging is opt-in,
// so spec.infrastructure.messaging is never materialized by defaulting and a
// Keystone-only install on a cluster without the rabbitmq-cluster-operator has no
// broker to watch. A ControlPlane that does declare managed messaging there fails
// closed in ensureRabbitMQ (InfrastructureReady False, reason RabbitMQError), so
// the guard defers nothing that would otherwise be caught. It is carried as an
// *unstructured.Unstructured, because this repository takes no dependency on the
// RabbitMQ Cluster Operator's Go module; apiutil.GVKForObject reads an
// unstructured object's GVK off the object itself, so it needs no scheme entry.
func optionalWatchObjects() []client.Object {
	rabbitmq := &unstructured.Unstructured{}
	rabbitmq.SetGroupVersionKind(messaging.RabbitmqClusterGVK)

	return []client.Object{
		&keystonev1alpha1.Keystone{},
		&horizonv1alpha1.Horizon{},
		&glancev1alpha1.Glance{},
		&glancev1alpha1.GlanceBackend{},
		&keystonev1alpha1.KeystoneIdentityBackend{},
		&placementv1alpha1.Placement{},
		&barbicanv1alpha1.Barbican{},
		&barbicanv1alpha1.BarbicanSecretStore{},
		&openbaov1alpha1.OpenBaoCluster{},
		&openbaov1alpha1.OpenBaoTenant{},
		rabbitmq,
		&neutronv1alpha1.Neutron{},
		&ovnv1alpha1.OVNCentral{},
	}
}

// servedKindsForGroupVersion asks discovery which Kinds the API server serves under a
// single GroupVersion and returns them as a set. Querying once per GroupVersion — not
// once per Kind — is why callers group their GVKs first: the keystone and glance
// groups each carry two optional kinds, so a per-Kind probe would issue two identical
// GETs against /apis/keystone.openstack.c5c3.io/v1alpha1 where one suffices.
//
// A missing GroupVersion surfaces from discovery as a NotFound error; that is the
// expected "CRD not installed" signal, so it collapses to an empty set with a nil
// error rather than a hard error. Any other discovery error (RBAC forbidden, API
// server unreachable) is a genuine failure the caller must see, so it is wrapped and
// returned; the wrap string is mandated verbatim by the acceptance criteria. A kind a
// caller asks about but the returned set omits — for example the keystone group present
// but the KeystoneIdentityBackend CRD not installed — is simply absent from the set.
func servedKindsForGroupVersion(disco serverResourcesLister, gv schema.GroupVersion) (map[string]bool, error) {
	list, err := disco.ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("listing server resources for %s: %w", gv, err)
	}
	kinds := make(map[string]bool, len(list.APIResources))
	for _, res := range list.APIResources {
		kinds[res.Kind] = true
	}
	return kinds, nil
}

// groupByGroupVersion buckets GVKs by their GroupVersion, preserving first-seen order,
// so every caller probes each GroupVersion against discovery exactly once and then
// tests all of that group's kinds against the single returned resource list.
func groupByGroupVersion(gvks []schema.GroupVersionKind) ([]schema.GroupVersion, map[schema.GroupVersion][]schema.GroupVersionKind) {
	order := make([]schema.GroupVersion, 0, len(gvks))
	byGV := make(map[schema.GroupVersion][]schema.GroupVersionKind, len(gvks))
	for _, gvk := range gvks {
		gv := gvk.GroupVersion()
		if _, seen := byGV[gv]; !seen {
			order = append(order, gv)
		}
		byGV[gv] = append(byGV[gv], gvk)
	}
	return order, byGV
}

// discoveryProbeAttempts is how many times the startup probe queries one GroupVersion
// before giving up on a transient (non-NotFound) discovery error; discoveryProbeBackoff
// is the pause between tries. They are vars, not consts, only so tests can zero the
// backoff and exercise the retry path without real sleeps.
var (
	discoveryProbeAttempts = 3
	discoveryProbeBackoff  = 500 * time.Millisecond
)

// servedKindsWithRetry wraps servedKindsForGroupVersion in a bounded retry so a single
// transient discovery blip during operator (re)start — an apiserver rollout, an etcd
// defrag/compaction pause, a momentarily overloaded API server returning 5xx — does not
// abort SetupWithManager and crash-loop the pod. Before this probe existed, GVK→GVR
// resolution happened lazily during informer sync, which controller-runtime already
// retries within CacheSyncTimeout; the retry here restores that tolerance for the now
// eager probe. A NotFound never reaches the retry (servedKindsForGroupVersion folds it
// to an empty set), so only genuine transient failures are retried and a persistent one
// still surfaces after the last attempt, aborting setup rather than starting with
// watches silently dropped.
func servedKindsWithRetry(disco serverResourcesLister, gv schema.GroupVersion) (map[string]bool, error) {
	for attempt := 1; ; attempt++ {
		kinds, err := servedKindsForGroupVersion(disco, gv)
		if err == nil || attempt >= discoveryProbeAttempts {
			return kinds, err
		}
		time.Sleep(discoveryProbeBackoff)
	}
}

// probeOptionalWatches resolves every optional watch object to its GroupVersionKind
// and probes discovery for it, querying each distinct GroupVersion exactly once. It
// returns a served map with an entry for all optional kinds (true when the CRD is
// installed, false when it is not) so callers can gate each watch leg individually,
// and a missing slice listing the unserved kinds so the crdWatchGate knows which GVKs
// to keep polling for. A discovery error other than NotFound aborts the whole probe
// after a bounded retry (servedKindsWithRetry): at start-up a persistently unreachable
// API server is fatal, not a signal to silently drop watches.
func probeOptionalWatches(disco serverResourcesLister, scheme *runtime.Scheme) (map[schema.GroupVersionKind]bool, []schema.GroupVersionKind, error) {
	objs := optionalWatchObjects()
	gvks := make([]schema.GroupVersionKind, 0, len(objs))
	for _, obj := range objs {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving GVK for optional watch object %T: %w", obj, err)
		}
		gvks = append(gvks, gvk)
	}

	order, byGV := groupByGroupVersion(gvks)
	served := make(map[schema.GroupVersionKind]bool, len(gvks))
	var missing []schema.GroupVersionKind
	for _, gv := range order {
		kinds, err := servedKindsWithRetry(disco, gv)
		if err != nil {
			return nil, nil, err
		}
		for _, gvk := range byGV[gv] {
			ok := kinds[gvk.Kind]
			served[gvk] = ok
			if !ok {
				missing = append(missing, gvk)
			}
		}
	}
	return served, missing, nil
}

// crdWatchGate is a manager.Runnable that watches for an optional CRD to appear after
// the operator has started without it. controller-runtime cannot add a watch to a
// running manager, so once a missing CRD is installed the only way to register its
// watch is to restart the process: Start returns an error, mgr.Start unwinds, main
// exits non-zero, and the Deployment restarts the leader — which re-runs
// SetupWithManager with the CRD now served. The re-check interval is a field so
// production can pass a steady constant and unit tests can inject a short one.
type crdWatchGate struct {
	disco    serverResourcesLister
	missing  []schema.GroupVersionKind
	interval time.Duration
}

// Start blocks until one of the missing CRDs becomes available, the context is
// cancelled, or forever if none ever appears. When there is nothing to wait for it
// returns immediately. A CRD becoming served returns a non-nil error whose message is
// mandated verbatim by the acceptance criteria; that error is the intended trigger for
// the process restart, not a fault. Context cancellation — lost leadership or manager
// shutdown — returns nil, because standing down is not an error. A transient discovery
// failure on a single tick is logged at V(1) and skipped: a blip in API-server
// reachability must never be mistaken for "the CRD appeared" and must never abort the
// gate, so the loop simply retries on the next tick.
func (g *crdWatchGate) Start(ctx context.Context) error {
	if len(g.missing) == 0 {
		return nil
	}
	// Group once up front so each tick queries every distinct GroupVersion exactly
	// once instead of re-fetching it per kind — the keystone and glance groups each
	// carry two optional kinds.
	order, byGV := groupByGroupVersion(g.missing)
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for _, gv := range order {
				kinds, err := servedKindsForGroupVersion(g.disco, gv)
				if err != nil {
					log.FromContext(ctx).V(1).Info(
						"discovery probe for optional CRD failed; will retry",
						"groupVersion", gv, "err", err.Error(),
					)
					continue
				}
				for _, gvk := range byGV[gv] {
					if kinds[gvk.Kind] {
						return fmt.Errorf("optional CRD %s became available after startup; restarting to register its watch", gvk)
					}
				}
			}
		}
	}
}

// NeedLeaderElection ties the gate to leadership: only the elected leader owns the
// watches, so only it should restart itself to pick up a new one. A non-leader replica
// would restart pointlessly without ever having registered the watch.
func (g *crdWatchGate) NeedLeaderElection() bool {
	return true
}

var (
	_ manager.Runnable               = (*crdWatchGate)(nil)
	_ manager.LeaderElectionRunnable = (*crdWatchGate)(nil)
)
