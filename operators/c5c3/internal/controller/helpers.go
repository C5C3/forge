// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The effective-* resolvers below answer the one question every consumer of a
// backing service asks: which INSTANCE does this service actually talk to? A
// service that opted into a dedicated instance
// (services.<svc>.dedicatedBackingServices.<class>) talks to that one; a service
// that did not — the default — shares the ControlPlane-wide instance in
// spec.infrastructure.
//
// Routing every consumer through these resolvers is what makes the opt-in carry
// the same lifecycle guarantees as the shared block with no per-class
// special-casing: the infrastructure sub-reconciler provisions and gates
// readiness on the effective instances, the service projections point the child
// CRs at them (which in turn carries the child operators' credential wiring and
// NetworkPolicy egress derivation, both pure functions of the projected
// spec.database / spec.cache), and the DB-credential sub-reconciler decides the
// credential shape from them.
//
// They return nil when nothing resolves — an External-mode ControlPlane (no
// backing services at all) or a webhook-bypassed CR that dropped
// spec.infrastructure. Callers fail closed on nil rather than dereferencing it.

// effectiveKeystoneDatabase resolves the database instance the Keystone service
// connects to.
func effectiveKeystoneDatabase(cp *c5c3v1alpha1.ControlPlane) *commonv1.DatabaseSpec {
	if db := cp.DedicatedKeystoneDatabase(); db != nil {
		return db
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Database
	}
	return nil
}

// effectiveKeystoneCache resolves the cache instance the Keystone service
// connects to.
func effectiveKeystoneCache(cp *c5c3v1alpha1.ControlPlane) *commonv1.CacheSpec {
	if cache := cp.DedicatedKeystoneCache(); cache != nil {
		return cache
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Cache
	}
	return nil
}

// effectiveHorizonCache resolves the cache instance the Horizon dashboard
// connects to.
func effectiveHorizonCache(cp *c5c3v1alpha1.ControlPlane) *commonv1.CacheSpec {
	if cache := cp.DedicatedHorizonCache(); cache != nil {
		return cache
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Cache
	}
	return nil
}

// effectiveGlanceDatabase resolves the database instance the Glance service
// connects to.
func effectiveGlanceDatabase(cp *c5c3v1alpha1.ControlPlane) *commonv1.DatabaseSpec {
	if db := cp.DedicatedGlanceDatabase(); db != nil {
		return db
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Database
	}
	return nil
}

// effectiveGlanceCache resolves the cache instance the Glance service connects
// to.
func effectiveGlanceCache(cp *c5c3v1alpha1.ControlPlane) *commonv1.CacheSpec {
	if cache := cp.DedicatedGlanceCache(); cache != nil {
		return cache
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Cache
	}
	return nil
}

// effectivePlacementDatabase resolves the database instance the Placement service
// connects to.
func effectivePlacementDatabase(cp *c5c3v1alpha1.ControlPlane) *commonv1.DatabaseSpec {
	if db := cp.DedicatedPlacementDatabase(); db != nil {
		return db
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Database
	}
	return nil
}

// effectivePlacementCache resolves the cache instance the Placement service
// connects to.
func effectivePlacementCache(cp *c5c3v1alpha1.ControlPlane) *commonv1.CacheSpec {
	if cache := cp.DedicatedPlacementCache(); cache != nil {
		return cache
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Cache
	}
	return nil
}

// effectiveBarbicanDatabase resolves the database instance the Barbican service
// connects to.
func effectiveBarbicanDatabase(cp *c5c3v1alpha1.ControlPlane) *commonv1.DatabaseSpec {
	if db := cp.DedicatedBarbicanDatabase(); db != nil {
		return db
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Database
	}
	return nil
}

// effectiveBarbicanCache resolves the cache instance the Barbican service
// connects to.
func effectiveBarbicanCache(cp *c5c3v1alpha1.ControlPlane) *commonv1.CacheSpec {
	if cache := cp.DedicatedBarbicanCache(); cache != nil {
		return cache
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Cache
	}
	return nil
}

// effectiveNeutronDatabase resolves the database instance the Neutron service
// connects to.
func effectiveNeutronDatabase(cp *c5c3v1alpha1.ControlPlane) *commonv1.DatabaseSpec {
	if db := cp.DedicatedNeutronDatabase(); db != nil {
		return db
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Database
	}
	return nil
}

// effectiveNeutronCache resolves the cache instance the Neutron service connects
// to.
func effectiveNeutronCache(cp *c5c3v1alpha1.ControlPlane) *commonv1.CacheSpec {
	if cache := cp.DedicatedNeutronCache(); cache != nil {
		return cache
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Cache
	}
	return nil
}

// targetClusterRefForNamespace resolves the target cluster the namespace named
// namespace lives on: nil — the local cluster the operator runs on — for the
// ControlPlane's own namespace, and otherwise the ref of a service that declares
// namespace as its dedicated one.
//
// Which of several co-located services answers does not matter. The webhook
// rejects a ControlPlane whose services share a namespace and disagree on the
// ref, so every service in one namespace names one cluster, and a namespace maps
// to exactly one. A namespace no service declares resolves to nil as well, which
// keeps a caller that walks namespaces from somewhere other than
// DedicatedServiceNamespaces on the local cluster.
//
// The ControlPlane's own namespace is answered before the services are walked,
// so a webhook-bypassed CR that names a target cluster without a namespace block
// of its own — the one case where a service resolves to that namespace — still
// stays local. It holds the ControlPlane CR itself, which never moves.
func targetClusterRefForNamespace(cp *c5c3v1alpha1.ControlPlane, namespace string) *commonv1.TargetClusterRefSpec {
	if namespace == cp.Namespace {
		return nil
	}
	for _, svc := range []struct {
		namespace string
		ref       *commonv1.TargetClusterRefSpec
	}{
		{cp.KeystoneNamespace(), cp.KeystoneTargetClusterRef()},
		{cp.HorizonNamespace(), cp.HorizonTargetClusterRef()},
		{cp.GlanceNamespace(), cp.GlanceTargetClusterRef()},
		{cp.PlacementNamespace(), cp.PlacementTargetClusterRef()},
		{cp.BarbicanNamespace(), cp.BarbicanTargetClusterRef()},
		{cp.NeutronNamespace(), cp.NeutronTargetClusterRef()},
	} {
		if svc.namespace == namespace {
			return svc.ref
		}
	}
	return nil
}

// childrenClientFor resolves the client the children the ControlPlane places in
// namespace are read and written with. A namespace no service placed on a target
// cluster — the ControlPlane's own among them — resolves to the local client, and
// a placed one to that cluster's client. Every sub-reconciler that projects into
// a service namespace resolves through it, so the step from a namespace to the
// cluster it lives on is taken in one place.
func (r *ControlPlaneReconciler) childrenClientFor(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, namespace string,
) (client.Client, error) {
	return commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client,
		targetClusterRefForNamespace(cp, namespace))
}

// anyTargetClusterResolves reports whether the ControlPlane places a service on a
// target cluster and AT LEAST ONE cluster it names resolves right now. It is the
// gate on installing the remote-children finalizer, so the finalizer only ever
// goes on a CR that can actually have children on a cluster to sweep.
//
// ANY, not ALL, because reconcileNamespaces resolves and writes PER NAMESPACE,
// inside its loop: a ControlPlane spanning two clusters, one of them
// unresolvable, still has the namespaces of the resolvable one created before the
// pass aborts. Demanding all of them would leave that written half without a
// finalizer, and the ORC stall escape — which releases the ORC finalizer on the
// explicit assumption that this one is still holding the CR open — would then let
// the CR leave etcd with those namespaces standing and nothing left to reclaim
// them.
//
// A nil Resolver resolves nothing: the operator runs without a multicluster
// manager, so every child stays on the management cluster whatever the spec names
// — which is what keeps a single-cluster deployment's deletion path byte-identical
// to what it was.
func (r *ControlPlaneReconciler) anyTargetClusterResolves(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) bool {
	if r.Resolver == nil {
		return false
	}
	for _, name := range cp.TargetClusterNames() {
		if _, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client,
			&commonv1.TargetClusterRefSpec{Name: name}); err == nil {
			return true
		}
	}
	return false
}

// childrenClientsFor resolves the children client of every namespace given,
// once per DISTINCT name, keyed by namespace. A sub-reconciler whose ensemble
// spans several namespaces resolves all of them BEFORE its first write, so a
// cluster that does not resolve leaves the whole ensemble unwritten rather than
// half of it — and a namespace that hosts several children costs one lookup.
func (r *ControlPlaneReconciler) childrenClientsFor(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, namespaces ...string,
) (map[string]client.Client, error) {
	clients := make(map[string]client.Client, len(namespaces))
	for _, namespace := range namespaces {
		if _, resolved := clients[namespace]; resolved {
			continue
		}
		c, err := r.childrenClientFor(ctx, cp, namespace)
		if err != nil {
			return nil, err
		}
		clients[namespace] = c
	}
	return clients, nil
}

// sameTargetCluster reports whether a and b name the same target cluster, with
// nil — no ref, so the local cluster — equal to nil. A ref carries nothing but a
// name, so the comparison is on the name alone.
func sameTargetCluster(a, b *commonv1.TargetClusterRefSpec) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Name == b.Name
}

// keystoneEndpointFor returns the Keystone URL a service placed behind ref
// validates its tokens against. Every service's spec.keystoneEndpoint resolves
// through it, because which Keystone URL a server-side token validator can reach
// is one rule, and a refinement of it has to land once rather than per service.
//
// On the cluster Keystone itself runs on that is the cluster-local convention URL
// of the projected Keystone child (keystoneEndpointURL, the same URL K-ORC
// authenticates against): an externally routable URL that resolves to a host-only
// address (a kind port-mapping, an external LB) is unreachable from inside the
// cluster. A service on ANOTHER cluster cannot resolve that Service DNS name at
// all, so it gets the public URL (keystonePublicEndpoint) — the only address that
// leaves Keystone's cluster. Derived top-down from the naming convention, never
// read from the child's status.
func keystoneEndpointFor(cp *c5c3v1alpha1.ControlPlane, ref *commonv1.TargetClusterRefSpec) string {
	if sameTargetCluster(ref, cp.KeystoneTargetClusterRef()) {
		return keystoneEndpointURL(cp)
	}
	return keystonePublicEndpoint(cp.Spec.Services.Keystone)
}

// intervalToCron converts a rotation interval into a cron expression suitable
// for a Kubernetes CronJob schedule.
//
// Supported intervals:
//   - 168h (7 days)                -> "0 0 * * 0" (weekly, Sunday midnight)
//   - any positive multiple of 24h -> "0 0 * * *" (daily midnight)
//
// The weekly case is matched first so the canonical 7-day interval yields the
// weekly schedule rather than a daily one. Any interval that is not a positive
// whole number of days returns an error naming the unsupported value.
//
// FIDELITY NOTE every multi-day interval that is NOT exactly 168h
// (e.g. 48h, 72h, 336h) INTENTIONALLY collapses to the same daily schedule —
// the cron form models only "daily" and "weekly", so the interval magnitude
// beyond {24h, 168h} is not preserved. This is a deliberate, documented
// limitation; a future level that needs other cadences must extend this
// mapping. The ControlPlane admission webhook (controlplane_webhook.go) rejects
// any rotationInterval that is not a positive whole number of days, so only the
// daily/weekly-representable set reaches this function in production.
func intervalToCron(d time.Duration) (string, error) {
	const (
		day  = 24 * time.Hour
		week = 7 * day
	)

	switch {
	case d == week:
		return "0 0 * * 0", nil
	case d > 0 && d%day == 0:
		return "0 0 * * *", nil
	default:
		return "", fmt.Errorf("unsupported rotation interval %q: must be 168h (weekly) or a positive multiple of 24h (daily)", d)
	}
}
