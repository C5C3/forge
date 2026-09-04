// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// childNamespace is the projection target for the CONTROL-PLANE-SCOPED children —
// the ones that belong to the ControlPlane as a whole rather than to one service:
// the K-ORC CRs, the clouds.yaml Secret, the service-account material. They live
// in the ControlPlane's own namespace and are owned by it through a controller
// owner reference, so the GC cascade reaps them.
//
// It is NOT the projection target for a SERVICE and the things that follow it. A
// service assigned a namespace of its own (spec.services.<svc>.namespace) is
// placed there, together with its backing services, its tenant store, and its
// credential material — see cp.KeystoneNamespace() / cp.HorizonNamespace(). Those
// children cannot carry an owner reference at all: Kubernetes garbage collection
// only cascades within one namespace, so the API server rejects a cross-namespace
// controller reference. They are stamped with the ownership labels instead
// (controlPlaneChildLabels) and deleted explicitly by the finalizer-driven
// teardown, because nothing collects them otherwise.
func childNamespace(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Namespace
}

// DECISION the managed-mode MariaDB CR is provisioned with
// a MINIMAL but VALID spec. The mariadb-operator's webhook requires
// Storage.Size (or a VolumeClaimTemplate) — see Storage.Validate in the
// vendored v0.38.1 types — so a size is always set. Both the replica topology
// and the storage size are DERIVED from the DECLARED instance — the shared
// spec.infrastructure.database or a per-service dedicated one, which is exactly
// what lets an operator size a busy service's database independently of the
// shared cluster: its replicas drive the topology (the default 3 yields a Galera
// HA cluster matching the production baseline, a single replica a single-instance
// non-Galera MariaDB so the fresh-create path schedules on a constrained cluster
// such as a single-node kind), and its storageSize drives the volume size
// (default 100Gi mirrors deploy/flux-system/infrastructure/mariadb.yaml; kind/CI
// pins a far smaller value). TLS / issuerRefs are deliberately NOT set here: the
// baseline wires those from cluster-specific ClusterIssuers that are an
// infrastructure concern outside the aggregate's knowledge, and the keystone
// DB-client baseline reads TLS from the projected database spec (the effective
// instance's .TLS) rather than from the MariaDB CR. The minimal spec keeps the CR admissible while leaving
// site-specific hardening to the platform team.
const (
	// infraMariaDBStorageSizeDefault is the zero-value fallback applied when
	// spec.infrastructure.database.storageSize is unset (""). The CRD default is
	// 100Gi, so this only fires when validation was bypassed (e.g. a fake-client
	// unit test that builds the CR directly); it keeps the projection admissible
	// (the mariadb-operator requires a non-empty size) and matches the production
	// baseline rather than requesting a zero-sized volume. It shares
	// commonv1.DatabaseStorageSizeDefault with the ControlPlane webhook's
	// migration normalization so the fallback and the webhook cannot disagree
	// on what "" means.
	infraMariaDBStorageSizeDefault = commonv1.DatabaseStorageSizeDefault
	// infraMariaDBReplicasDefault is the zero-value floor applied when
	// spec.infrastructure.database.replicas is unset (0). The CRD default is 3,
	// so this only fires when validation was bypassed; it keeps the projection
	// admissible (replicas >= 1) rather than creating a zero-replica MariaDB.
	infraMariaDBReplicasDefault = int32(3)
	// infraRabbitMQReplicasDefault is the zero-value floor applied when
	// spec.infrastructure.messaging.replicas is unset (0). The CRD default is 3
	// and its minimum is 1, so this only fires when validation was bypassed; it
	// keeps the projection admissible (replicas >= 1) rather than creating a
	// zero-replica broker.
	infraRabbitMQReplicasDefault = int32(3)
)

// memcachedGVK is the GroupVersionKind of the Memcached CR projected in managed
// cache mode. DECISION memcached.c5c3.io publishes NO Go
// module, so the Memcached child is built and applied as an
// unstructured.Unstructured rather than a typed client object. The fake client
// and the real apiserver both accept an unstructured object carrying this GVK;
// no scheme registration is required.
var memcachedGVK = schema.GroupVersionKind{
	Group:   "memcached.c5c3.io",
	Version: "v1beta1",
	Kind:    "Memcached",
}

// messagingRecreateAllowedAnnotation, when set to a truthy value on a
// ControlPlane, opts that ControlPlane in to the DESTRUCTIVE convergence of a
// managed messaging.replicas SCALE-DOWN. The RabbitMQ Cluster Operator refuses
// an in-place shrink, so the only path to a lowered count is deleting the owned
// RabbitmqCluster and creating it again — which discards its volumes and with
// them every durable queue and every unacked message on the bus. Without the
// annotation ensureRabbitMQ REFUSES the shrink and reports the divergence
// instead.
//
// The gate exists because the decrement needs no deliberate act to reach the
// reconciler: replicas carries a schema default of 3, so a GitOps commit that
// merely DROPS the line off a ControlPlane running 5 reads as a no-op in review,
// is defaulted back to 3 by the apiserver, and arrives here as desired=3 against
// a live broker at 5. Mirrors keystoneDeletionAllowedAnnotation: destroying
// irreplaceable state is opt-in, never a side effect of an ordinary spec edit.
const messagingRecreateAllowedAnnotation = "c5c3.io/allow-messaging-recreate"

// messagingRecreateAllowed reports whether cp opts in to the delete-and-recreate
// a managed messaging scale-down needs, via a truthy
// messagingRecreateAllowedAnnotation. A missing, malformed, or non-truthy value
// means "refuse" — the fail-safe default that protects the broker's queues.
func messagingRecreateAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[messagingRecreateAllowedAnnotation])
	return err == nil && allowed
}

// reconcileInfrastructure reconciles the backing services (MariaDB, Memcached,
// RabbitMQ) the ControlPlane provisions and drives the InfrastructureReady
// condition.
//
// That set is the instances the ControlPlane's services actually RESOLVE to
// (managedInfraInstances enumerates them): the SHARED instances in
// spec.infrastructure, and the per-service DEDICATED instances under
// services.<svc>.dedicatedBackingServices that a service opted into instead. A
// shared instance every service has opted out of has no consumer and is NOT
// provisioned. Managed mode (ClusterRef set) ensures an owned child CR per
// instance; brownfield mode (Host / Servers set) provisions nothing.
// InfrastructureReady is True once every managed child is ensured and reports
// Ready; while any child is still converging the sub-reconciler requeues with
// InfrastructureReady False. When the control plane uses only brownfield infra
// there is nothing to provision, so InfrastructureReady is True immediately.
//
// External keystone mode has NO infrastructure block at all, so the skip is
// keyed on the mode discriminator (cp.IsExternalKeystone) rather than on the
// database shape the brownfield short-circuits read.
func (r *ControlPlaneReconciler) reconcileInfrastructure(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// External-mode short-circuit: identity is managed against a pre-existing
	// Keystone, so there are no backing services to provision. Report the
	// condition True with the dedicated ExternallyManaged reason — the condition
	// SCHEMA is identical across modes, so subConditionTypes, setReadyCondition
	// and the condition_type drift guard need no mode awareness.
	//
	// The message embeds authURL, so it is bounded by truncateConditionMessage like
	// every assembled failure message: authURL's MaxLength keeps it far under the
	// apiserver's cap on the admission path, but a webhook- and CRD-bypassed CR
	// would otherwise make the WHOLE status.conditions write unpersistable — every
	// condition, not just this one — and wedge the reconciler in a backoff loop.
	if cp.IsExternalKeystone() {
		logger.Info("External keystone mode; no backing services are provisioned",
			"authURL", externalKeystoneAuthURL(cp))
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInfrastructureReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             conditionReasonExternallyManaged,
			Message: truncateConditionMessage(fmt.Sprintf("External keystone mode: identity is managed against %s; "+
				"no MariaDB/Memcached is provisioned", externalKeystoneAuthURL(cp))),
		})
		return ctrl.Result{}, nil
	}

	// Nil-safety fail-safe. spec.infrastructure is optional at the Go/CRD layer
	// because External mode omits it, but the validating webhook REQUIRES it
	// outside External mode — so this branch is unreachable on the admission path
	// and only fires for a webhook-bypassed CR (direct etcd write, admission
	// misconfigured). Fail closed with a named reason rather than dereferencing
	// the nil block below.
	if cp.Spec.Infrastructure == nil {
		logger.Info("spec.infrastructure is unset on a non-External ControlPlane; refusing to provision")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInfrastructureReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             conditionReasonInfrastructureNotConfigured,
			Message: "spec.infrastructure is unset but services.keystone.mode is not External; " +
				"the backing services cannot be provisioned",
		})
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Ensure every managed instance FIRST, in a single pass, so a half-provisioned
	// control plane (e.g. DB created but Memcached missing) never occurs: every
	// child is created/updated before readiness is gated on. Readiness is then
	// evaluated collectively across ALL of them — the shared instances and every
	// per-service dedicated one alike, so a service's dedicated database is as
	// load-bearing for InfrastructureReady (and therefore for the projection gate
	// on the consuming service) as the shared cluster is.
	instances := r.managedInfraInstances(cp)

	// A backing service is provisioned on the cluster of the service it belongs
	// to, so each instance is written with its own namespace's children client.
	// They are all resolved BEFORE the first ensure — the pass writes nothing at
	// all when one of them does not resolve, rather than leaving a control plane
	// with its database on one cluster and its cache nowhere.
	namespaces := make([]string, 0, len(instances))
	for _, inst := range instances {
		namespaces = append(namespaces, inst.namespace)
	}
	children, cerr := r.childrenClientsFor(ctx, cp, namespaces...)
	if cerr != nil {
		conditionFailer(cp, conditionTypeInfrastructureReady)(commonmulticluster.TargetClusterUnavailable, cerr.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	var notReady *infraInstance
	for _, inst := range instances {
		ready, err := inst.ensure(ctx, children[inst.namespace])
		if err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeInfrastructureReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             inst.errorReason,
				Message: fmt.Sprintf("ensuring %s %q in namespace %q (%s): %v",
					inst.kind, inst.name, inst.namespace, inst.declaredAt, err),
			})
			return ctrl.Result{}, err
		}
		if !ready && notReady == nil {
			notReady = &inst
		}
	}

	if notReady != nil {
		// Report the first instance still converging. The reason stays the
		// class-level WaitingForDatabase / WaitingForCache (unchanged for the shared
		// block), and the message names the instance so an operator can tell a
		// pending dedicated database from a pending shared one.
		inst := notReady
		logger.Info("managed backing service not ready, requeuing",
			"kind", inst.kind, "cluster", inst.name, "namespace", inst.namespace, "declaredAt", inst.declaredAt)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInfrastructureReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             inst.waitReason,
			Message: fmt.Sprintf("%s %q in namespace %q (%s) is not ready",
				inst.kind, inst.name, inst.namespace, inst.declaredAt),
		})
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeInfrastructureReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "InfrastructureReady",
		Message:            "All managed backing services are ensured and ready",
	})
	return ctrl.Result{}, nil
}

// infraInstance is one MANAGED backing-service instance the ControlPlane
// provisions and owns: an instance some service EFFECTIVELY resolves to — the
// shared database/cache from spec.infrastructure, or a per-service dedicated one.
// Brownfield instances are not represented — there is nothing to provision or
// gate readiness on.
type infraInstance struct {
	// kind, name and namespace identify the child CR; declaredAt is the spec path
	// the instance was declared at, so a condition message tells a pending
	// dedicated database from a pending shared one — and, now that services can be
	// placed apart, which namespace the pending one is in.
	kind       string
	name       string
	namespace  string
	declaredAt string
	// errorReason / waitReason are the condition reasons for a failed ensure and
	// for a child still converging. They are per CLASS, not per instance, so the
	// reason vocabulary is unchanged by the dedicated opt-in.
	errorReason string
	waitReason  string
	// ensure provisions the instance with the client its namespace's children are
	// written with, which reconcileInfrastructure resolves once per namespace. The
	// enumeration below stays a pure derivation of the spec: it names the namespace
	// each instance belongs to and leaves resolving that namespace's cluster — the
	// one step that can fail, and that must fail before anything is written — to
	// the caller.
	ensure func(context.Context, client.Client) (bool, error)
}

// managedInfraInstances enumerates the managed backing-service instances of cp.
// It is the single place the set is derived, so adding a backing-service class or
// a service extends the enumeration rather than the reconcile flow around it.
//
// The set is the instances the ControlPlane's services actually RESOLVE to (the
// effective-* resolvers), NOT the set of declared blocks. That distinction is
// load-bearing once a service opts out: a ControlPlane whose declared database
// consumers all took dedicated databases leaves the shared
// spec.infrastructure.database with no consumer at all — and the webhook
// materializes that block (3 Galera replicas, 100Gi) whenever it is omitted.
// Enumerating declarations would provision that cluster anyway and then gate
// InfrastructureReady on it, so an instance nothing talks to could hold the whole
// control plane back. The same holds for the shared cache once every service has
// taken one of its own.
//
// BACKING SERVICES FOLLOW THE SERVICE. Each instance is placed in the namespace
// of the service that resolves to it, so a namespace hosting at least one service
// of the ControlPlane gets its own set of backing-service instances. That falls
// out of the enumeration rather than being a special case: the effective database
// and cache of each service are added AT that service's namespace, so the shared
// spec.infrastructure block materializes once per namespace that consumes it —
// two MariaDBs and two Memcacheds when Keystone and Horizon are placed apart, one
// of each when they are co-located, exactly one of each (today's behavior) when
// neither is assigned a namespace.
//
// MESSAGING DOES NOT. It is the one class enumerated at the ControlPlane's own
// namespace regardless of consumers. A message bus is shared across services by
// nature (Nova and Neutron RPC have to meet on one broker), so a declared managed
// spec.infrastructure.messaging is wanted even on a ControlPlane with
// services: {}. childrenClientsFor resolves cp.Namespace to the local client, so
// the bus is always written at home.
//
// Entries are deduplicated on (kind, namespace, name) — the identity of the child
// CR they resolve to. The namespace is part of that identity now: the same shared
// clusterRef name in two namespaces is two distinct child CRs, and must be
// provisioned twice. Within one namespace the dedup does what it always did: it
// collapses co-located services onto one instance, and it fails closed on a
// webhook-bypassed CR whose dedicated clusterRef collides with the shared one
// (validateDedicatedBackingServices rejects the duplicate at admission). Without
// it, two entries would run ensure against the SAME child CR in one pass, each
// projecting a different desired topology, and each write would re-enqueue the
// ControlPlane into a self-sustaining loop of conflicting writes. First
// resolution wins.
func (r *ControlPlaneReconciler) managedInfraInstances(cp *c5c3v1alpha1.ControlPlane) []infraInstance {
	var instances []infraInstance

	seen := map[string]struct{}{}
	claim := func(kind, namespace, name string) bool {
		key := kind + "/" + namespace + "/" + name
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
		return true
	}

	addDatabase := func(db *commonv1.DatabaseSpec, namespace, declaredAt string) {
		if db == nil || db.ClusterRef == nil {
			return // absent, or brownfield: nothing to provision.
		}
		if !claim("MariaDB", namespace, db.ClusterRef.Name) {
			return
		}
		instances = append(instances, infraInstance{
			kind:        "MariaDB",
			name:        db.ClusterRef.Name,
			namespace:   namespace,
			declaredAt:  declaredAt,
			errorReason: "MariaDBError",
			waitReason:  "WaitingForDatabase",
			ensure: func(ctx context.Context, c client.Client) (bool, error) {
				return r.ensureMariaDB(ctx, c, cp, db, namespace)
			},
		})
	}
	addCache := func(cache *commonv1.CacheSpec, namespace, declaredAt string) {
		if cache == nil || cache.ClusterRef == nil {
			return
		}
		if !claim("Memcached", namespace, cache.ClusterRef.Name) {
			return
		}
		instances = append(instances, infraInstance{
			kind:        "Memcached",
			name:        cache.ClusterRef.Name,
			namespace:   namespace,
			declaredAt:  declaredAt,
			errorReason: "MemcachedError",
			waitReason:  "WaitingForCache",
			ensure: func(ctx context.Context, c client.Client) (bool, error) {
				return r.ensureMemcached(ctx, c, cp, cache, namespace)
			},
		})
	}
	addMessaging := func(m *commonv1.MessagingSpec, namespace, declaredAt string) {
		if m == nil || m.ClusterRef == nil {
			return // absent, or brownfield: nothing to provision.
		}
		if !claim("RabbitmqCluster", namespace, m.ClusterRef.Name) {
			return
		}
		instances = append(instances, infraInstance{
			kind:        "RabbitmqCluster",
			name:        m.ClusterRef.Name,
			namespace:   namespace,
			declaredAt:  declaredAt,
			errorReason: "RabbitMQError",
			waitReason:  "WaitingForMessaging",
			ensure: func(ctx context.Context, c client.Client) (bool, error) {
				return r.ensureRabbitMQ(ctx, c, cp, m, namespace)
			},
		})
	}

	keystoneNS := cp.KeystoneNamespace()

	addDatabase(effectiveKeystoneDatabase(cp), keystoneNS, keystoneDatabaseDeclaredAt(cp))
	addCache(effectiveKeystoneCache(cp), keystoneNS, keystoneCacheDeclaredAt(cp))

	// The dashboard's cache is enumerated only when the dashboard is DECLARED.
	// While every service shared the ControlPlane's namespace this gate made no
	// difference — an undeclared Horizon resolved to the same shared cache in the
	// same namespace as Keystone, so the entry deduplicated away. Once the services
	// can be placed apart it does: without the gate, a ControlPlane that declares
	// only Keystone and places it elsewhere would provision a SECOND cache back in
	// its own namespace, for a dashboard that does not exist — and then gate
	// InfrastructureReady on that cache reaching Ready, holding the whole control
	// plane behind an instance nothing talks to. That is precisely the
	// no-consumer-no-instance rule this enumeration already applies to a shared
	// block every service opted out of.
	if cp.Spec.Services.Horizon != nil {
		addCache(effectiveHorizonCache(cp), cp.HorizonNamespace(), horizonCacheDeclaredAt(cp))
	}

	// Glance's database and cache are enumerated only when the service is
	// DECLARED — the same no-consumer-no-instance rationale the Horizon cache
	// above applies: an undeclared Glance would otherwise provision a database and
	// a cache in the ControlPlane's namespace for a service that does not exist and
	// then hold InfrastructureReady behind them.
	if cp.Spec.Services.Glance != nil {
		glanceNS := cp.GlanceNamespace()
		addDatabase(effectiveGlanceDatabase(cp), glanceNS, glanceDatabaseDeclaredAt(cp))
		addCache(effectiveGlanceCache(cp), glanceNS, glanceCacheDeclaredAt(cp))
	}

	// Placement's database and cache are gated on the DECLARATION for the same
	// no-consumer-no-instance reason as Glance's above.
	if cp.Spec.Services.Placement != nil {
		placementNS := cp.PlacementNamespace()
		addDatabase(effectivePlacementDatabase(cp), placementNS, placementDatabaseDeclaredAt(cp))
		addCache(effectivePlacementCache(cp), placementNS, placementCacheDeclaredAt(cp))
	}

	// Barbican's database and cache are gated on the DECLARATION for the same
	// no-consumer-no-instance reason as Placement's above.
	if cp.Spec.Services.Barbican != nil {
		barbicanNS := cp.BarbicanNamespace()
		addDatabase(effectiveBarbicanDatabase(cp), barbicanNS, barbicanDatabaseDeclaredAt(cp))
		addCache(effectiveBarbicanCache(cp), barbicanNS, barbicanCacheDeclaredAt(cp))
	}

	// Neutron's database and cache are gated on the DECLARATION for the same
	// no-consumer-no-instance reason as Barbican's above.
	if cp.Spec.Services.Neutron != nil {
		neutronNS := cp.NeutronNamespace()
		addDatabase(effectiveNeutronDatabase(cp), neutronNS, neutronDatabaseDeclaredAt(cp))
		addCache(effectiveNeutronCache(cp), neutronNS, neutronCacheDeclaredAt(cp))
	}

	// The shared message bus is the one class enumerated at the ControlPlane's
	// own namespace regardless of consumers: see the doc comment above. The nil
	// check on the block mirrors the effective-* resolvers, so a webhook-bypassed
	// CR without spec.infrastructure enumerates nothing instead of panicking
	// (reconcileInfrastructure fails such a CR closed before it gets here).
	if cp.Spec.Infrastructure != nil {
		addMessaging(cp.Spec.Infrastructure.Messaging, childNamespace(cp), "spec.infrastructure.messaging")
	}

	return instances
}

// The declaredAt-* helpers name the spec path the instance a service resolves to
// was declared at, so a condition message tells a pending dedicated instance from
// a pending shared one.
func keystoneDatabaseDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedKeystoneDatabase() != nil {
		return "spec.services.keystone.dedicatedBackingServices.database"
	}
	return "spec.infrastructure.database"
}

func keystoneCacheDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedKeystoneCache() != nil {
		return "spec.services.keystone.dedicatedBackingServices.cache"
	}
	return "spec.infrastructure.cache"
}

func horizonCacheDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedHorizonCache() != nil {
		return "spec.services.horizon.dedicatedBackingServices.cache"
	}
	return "spec.infrastructure.cache"
}

func glanceDatabaseDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedGlanceDatabase() != nil {
		return "spec.services.glance.dedicatedBackingServices.database"
	}
	return "spec.infrastructure.database"
}

func glanceCacheDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedGlanceCache() != nil {
		return "spec.services.glance.dedicatedBackingServices.cache"
	}
	return "spec.infrastructure.cache"
}

func placementDatabaseDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedPlacementDatabase() != nil {
		return "spec.services.placement.dedicatedBackingServices.database"
	}
	return "spec.infrastructure.database"
}

func placementCacheDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedPlacementCache() != nil {
		return "spec.services.placement.dedicatedBackingServices.cache"
	}
	return "spec.infrastructure.cache"
}

func barbicanDatabaseDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedBarbicanDatabase() != nil {
		return "spec.services.barbican.dedicatedBackingServices.database"
	}
	return "spec.infrastructure.database"
}

func barbicanCacheDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedBarbicanCache() != nil {
		return "spec.services.barbican.dedicatedBackingServices.cache"
	}
	return "spec.infrastructure.cache"
}

func neutronDatabaseDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedNeutronDatabase() != nil {
		return "spec.services.neutron.dedicatedBackingServices.database"
	}
	return "spec.infrastructure.database"
}

func neutronCacheDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedNeutronCache() != nil {
		return "spec.services.neutron.dedicatedBackingServices.cache"
	}
	return "spec.infrastructure.cache"
}

// ensureMariaDB create-or-updates the owned MariaDB CR named after db.clusterRef
// in namespace and reports whether it is Ready. db is the declared instance — the
// shared spec.infrastructure.database or a per-service dedicated one; both are
// provisioned, owned, and torn down identically, which is what makes a dedicated
// instance carry the shared block's lifecycle guarantees rather than a parallel
// set of its own.
//
// namespace is the namespace of the SERVICE that resolves to this instance, so a
// service placed apart gets its backing services beside it, and c is the client
// that namespace's children are written with — the local one, or the target
// cluster's when the service was placed on one. The instance has to land there
// and not at home: it is the mariadb-operator on the target cluster that has to
// see the CR, and the service's own pods that have to reach the database it
// brings up. In the ControlPlane's own namespace the child takes a controller
// owner reference and the GC cascade reaps it; anywhere else no owner reference
// is possible (Kubernetes forbids a cross-namespace one, and a target cluster
// cannot resolve the owner's UID), so the child is stamped with the ownership
// labels and the finalizer-driven teardown deletes it explicitly.
//
// It stays read-modify-write (not Server-Side Apply): the write is gated on the
// LIVE object's ownership — an owned CR has its topology re-projected, while an
// externally-provisioned CR sharing the name is adopted read-only and never has
// ownership claimed. That adoption-vs-projection decision reads live state, so it
// cannot be expressed as a pure projection of cp.Spec.
func (r *ControlPlaneReconciler) ensureMariaDB(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, db *commonv1.DatabaseSpec, namespace string) (bool, error) {
	key := types.NamespacedName{
		Name:      db.ClusterRef.Name,
		Namespace: namespace,
	}
	// Derive the projected topology from the ControlPlane spec. A single replica
	// yields a single-instance MariaDB with Galera off, so a single-node kind can
	// schedule the fresh-create path; any multi-replica count (the default is 3)
	// enables the Galera clustering the production baseline uses. Floor a
	// zero/negative value (only reachable when CRD validation was bypassed) to
	// the default.
	replicas := db.Replicas
	if replicas < 1 {
		replicas = infraMariaDBReplicasDefault
	}
	galeraEnabled := replicas > 1

	// Derive the projected volume size from the spec, falling back to the
	// production baseline when the field is empty (only reachable when the CRD
	// default was bypassed). Storage is immutable on the mariadb-operator CR, so
	// this value is honoured on fresh create only and never re-projected below.
	storageSize := db.StorageSize
	if storageSize == "" {
		storageSize = infraMariaDBStorageSizeDefault
	}

	mariadb := &mariadbv1alpha1.MariaDB{}
	err := c.Get(ctx, key, mariadb)
	switch {
	case apierrors.IsNotFound(err):
		// Create fresh with the projected, spec-derived topology.
		size, perr := resource.ParseQuantity(storageSize)
		if perr != nil {
			return false, fmt.Errorf("parsing MariaDB storage size %q: %w", storageSize, perr)
		}
		mariadb.Name = key.Name
		mariadb.Namespace = key.Namespace
		mariadb.Spec.Replicas = replicas
		mariadb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: galeraEnabled}
		mariadb.Spec.Storage = mariadbv1alpha1.Storage{Size: &size}
		if serr := claimChildOwnership(c, cp, mariadb, r.Scheme); serr != nil {
			return false, fmt.Errorf("claiming ownership of MariaDB %q: %w", key.Name, serr)
		}
		if cerr := c.Create(ctx, mariadb); cerr != nil {
			return false, fmt.Errorf("creating MariaDB %q: %w", key.Name, cerr)
		}
	case err != nil:
		return false, fmt.Errorf("getting MariaDB %q: %w", key.Name, err)
	default:
		// A MariaDB with this clusterRef name already exists. Two sub-cases:
		//
		//  1. It is OWNED by this ControlPlane (we created it on an earlier pass):
		//     re-assert the spec-derived projection — spec.replicas and the derived
		//     Galera topology — so external drift on the owned cluster is corrected
		//     back to the declared topology. spec.infrastructure.database.replicas
		//     is itself immutable after creation (the ControlPlane validating
		//     webhook rejects a change), so this never scales down or toggles Galera
		//     in response to a user edit — only in response to drift. spec.storage
		//     is deliberately NOT re-projected even when owned: the mariadb-operator
		//     webhook rejects changing spec.storage.* on an existing CR, so storage
		//     stays as first created.
		//
		//  2. It is NOT owned (e.g. the infrastructure stack provisions
		//     "openstack-db" under the same name): adopt it as-is and reconcile only
		//     against its status. Re-projecting our defaults would be rejected
		//     (immutable storage) or needlessly reshape a running database, and we
		//     never claim ownership of a resource we did not create, so deleting
		//     the ControlPlane never cascades into shared infra.
		//
		// Ownership is isControlPlaneChild, not IsControlledBy: a child in a service
		// namespace carries the ownership labels instead of an owner reference.
		if isControlPlaneChild(mariadb, cp) {
			currentGalera := mariadb.Spec.Galera != nil && mariadb.Spec.Galera.Enabled
			if mariadb.Spec.Replicas != replicas || currentGalera != galeraEnabled {
				mariadb.Spec.Replicas = replicas
				mariadb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: galeraEnabled}
				if uerr := c.Update(ctx, mariadb); uerr != nil {
					return false, fmt.Errorf("updating owned MariaDB %q topology: %w", key.Name, uerr)
				}
			}
		}
	}

	return conditions.IsReady(mariadb.Status.Conditions), nil
}

// ensureMemcached create-or-updates the owned Memcached CR named after
// cache.clusterRef and reports whether it is Ready. cache is the declared
// instance — the shared spec.infrastructure.cache or a per-service dedicated one
// (see ensureMariaDB). The Memcached CR is handled as an
// unstructured.Unstructured because memcached.c5c3.io ships no Go module (see
// memcachedGVK).
//
// Like ensureMariaDB it stays read-modify-write: it reads the live object's
// owner references to project only onto an owned CR and adopt an externally
// provisioned one read-only (never claiming GC ownership), and it is
// unstructured, which apply.EnsureObject's typed-struct path does not cover.
func (r *ControlPlaneReconciler) ensureMemcached(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, cache *commonv1.CacheSpec, namespace string) (bool, error) {
	key := types.NamespacedName{
		Name:      cache.ClusterRef.Name,
		Namespace: namespace,
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	err := c.Get(ctx, key, u)
	switch {
	case apierrors.IsNotFound(err):
		u.SetName(key.Name)
		u.SetNamespace(key.Namespace)
		// int32 must be widened to int64 for unstructured nested-field storage.
		if serr := unstructured.SetNestedField(u.Object, int64(cache.Replicas), "spec", "replicas"); serr != nil {
			return false, fmt.Errorf("setting spec.replicas: %w", serr)
		}
		if serr := claimChildOwnership(c, cp, u, r.Scheme); serr != nil {
			return false, fmt.Errorf("claiming ownership of Memcached %q: %w", key.Name, serr)
		}
		if cerr := c.Create(ctx, u); cerr != nil {
			return false, fmt.Errorf("creating Memcached %q: %w", key.Name, cerr)
		}
	case err != nil:
		return false, fmt.Errorf("getting Memcached %q: %w", key.Name, err)
	default:
		// An existing Memcached. If this ControlPlane OWNS it (we created it on an
		// earlier pass), reconcile spec.replicas so a ControlPlane spec change
		// (the declared instance's cache.replicas) actually scales the cache we own
		// instead of being ignored after first creation. If it is a pre-existing /
		// externally-provisioned instance (NOT owned) we adopt it as-is and never
		// reshape it — same rationale as ensureMariaDB — nor claim GC ownership of
		// shared infra.
		if metav1.IsControlledBy(u, cp) {
			desired := int64(cache.Replicas)
			current, found, gerr := unstructured.NestedInt64(u.Object, "spec", "replicas")
			if gerr != nil {
				return false, fmt.Errorf("reading Memcached %q spec.replicas: %w", key.Name, gerr)
			}
			if !found || current != desired {
				if serr := unstructured.SetNestedField(u.Object, desired, "spec", "replicas"); serr != nil {
					return false, fmt.Errorf("setting Memcached %q spec.replicas: %w", key.Name, serr)
				}
				if uerr := c.Update(ctx, u); uerr != nil {
					return false, fmt.Errorf("updating owned Memcached %q replicas: %w", key.Name, uerr)
				}
			}
		}
	}

	return unstructuredReady(u), nil
}

// ensureRabbitMQ create-or-updates the owned RabbitmqCluster CR named after
// m.clusterRef in namespace and reports whether it is ready. m is the declared
// shared spec.infrastructure.messaging block; the bus has no per-service
// dedicated variant, because it is shared across services by nature.
//
// Only spec.replicas is projected. Image, resources, persistence and tls stay at
// the RabbitMQ Cluster Operator's defaults, or at whatever the platform set on an
// adopted CR: they are site-specific hardening outside the aggregate's knowledge,
// the posture ensureMariaDB already takes on TLS and issuerRefs.
//
// Readiness follows the operator's AllReplicasReady condition. The RabbitMQ
// Cluster Operator sets no Ready condition at all, so unstructuredReady would
// never see a healthy broker; unstructuredConditionTrue reads the condition the
// operator does set.
//
// A cluster that does not serve the RabbitmqCluster kind surfaces on the Get as a
// meta.NoKindMatchError, which is not NotFound. A ControlPlane declaring managed
// messaging on such a cluster therefore fails closed with InfrastructureReady
// False and reason RabbitMQError instead of quietly provisioning nothing.
//
// Like ensureMemcached it is unstructured (this repository takes no dependency
// on the RabbitMQ Cluster Operator's Go module, see
// messaging.RabbitmqClusterGVK) and read-modify-write: the write is gated on the
// LIVE object's ownership, so an owned CR has its replica count re-projected
// while an externally-provisioned CR sharing the name is adopted read-only and
// never has ownership claimed.
//
// Unlike ensureMemcached the re-projection is not symmetric. Growing an owned
// cluster is an in-place Update; SHRINKING one is a delete-and-recreate, because
// the RabbitMQ Cluster Operator refuses an in-place scale-down and an Update
// carrying the lowered count would be silently ignored. That is destructive by
// construction — the broker and its volumes are recreated empty — and it is the
// only path the operator offers for a declared count the running cluster
// exceeds, so it is GATED on messagingRecreateAllowedAnnotation: an unauthorised
// shrink is refused with an error naming the annotation, and the broker keeps
// running at its current size.
func (r *ControlPlaneReconciler) ensureRabbitMQ(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, m *commonv1.MessagingSpec, namespace string) (bool, error) {
	// Floor a zero/negative count (only reachable when CRD validation was
	// bypassed) to the default rather than creating a broker with no pods.
	replicas := m.Replicas
	if replicas < 1 {
		replicas = infraRabbitMQReplicasDefault
	}

	key := types.NamespacedName{
		Name:      m.ClusterRef.Name,
		Namespace: namespace,
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	err := c.Get(ctx, key, u)
	switch {
	case apierrors.IsNotFound(err):
		u.SetName(key.Name)
		u.SetNamespace(key.Namespace)
		// int32 must be widened to int64 for unstructured nested-field storage.
		if serr := unstructured.SetNestedField(u.Object, int64(replicas), "spec", "replicas"); serr != nil {
			return false, fmt.Errorf("setting spec.replicas: %w", serr)
		}
		if serr := claimChildOwnership(c, cp, u, r.Scheme); serr != nil {
			return false, fmt.Errorf("claiming ownership of RabbitmqCluster %q: %w", key.Name, serr)
		}
		if cerr := c.Create(ctx, u); cerr != nil {
			return false, fmt.Errorf("creating RabbitmqCluster %q: %w", key.Name, cerr)
		}
	case err != nil:
		return false, fmt.Errorf("getting RabbitmqCluster %q: %w", key.Name, err)
	default:
		// A child mid-teardown — including the one an earlier pass's scale-down
		// deleted — keeps its spec AND its status.conditions until the RabbitMQ
		// Cluster Operator's finalizer clears, so AllReplicasReady still reads True
		// on a broker that is being destroyed. Report not ready and write nothing:
		// readiness must never be gated on a bus that is going away, and neither
		// the re-projection below nor a second Delete would change its fate.
		if u.GetDeletionTimestamp() != nil {
			return false, nil
		}

		// An existing RabbitmqCluster. If this ControlPlane OWNS it (we created it
		// on an earlier pass), reconcile spec.replicas so a change to the declared
		// messaging.replicas scales the broker we own instead of being ignored after
		// first creation. A pre-existing / externally-provisioned one (NOT owned) is
		// adopted as-is, never reshaped and never claimed for GC, on the rationale
		// ensureMemcached states.
		if metav1.IsControlledBy(u, cp) {
			desired := int64(replicas)
			current, found, gerr := unstructured.NestedInt64(u.Object, "spec", "replicas")
			if gerr != nil {
				return false, fmt.Errorf("reading RabbitmqCluster %q spec.replicas: %w", key.Name, gerr)
			}
			// The RabbitMQ Cluster Operator refuses an in-place shrink (it logs an
			// UnsupportedOperation event, sets ReconcileSuccess=False and leaves the
			// StatefulSet at its old size), so writing a lowered count onto the CR
			// would leave the declared size and the running broker permanently
			// divergent while AllReplicasReady — and with it InfrastructureReady —
			// stayed True. A scale-down is therefore a delete-and-recreate: the next
			// pass takes the NotFound branch above and creates the cluster at the
			// declared size. Reported not ready, so readiness is not gated on a
			// cluster that is going away.
			//
			// That recreate destroys every queue and message on the bus, and an
			// unintended decrement is cheap to write — replicas defaults to 3, so
			// dropping the line off a ControlPlane running 5 arrives here as a
			// scale-down nobody typed. So it is REFUSED unless the ControlPlane
			// carries messagingRecreateAllowedAnnotation: the broker keeps running at
			// its current size and the divergence surfaces as InfrastructureReady
			// False with reason RabbitMQError, naming the annotation that authorises
			// the recreate.
			if found && desired < current {
				if !messagingRecreateAllowed(cp) {
					return false, fmt.Errorf(
						"declared messaging.replicas %d is below owned RabbitmqCluster %q's %d and the RabbitMQ "+
							"Cluster Operator cannot shrink in place; converging requires deleting and recreating "+
							"the broker, which loses every queue and message on it (set annotation %s=true on the "+
							"ControlPlane to authorise the recreate)",
						desired, key.Name, current, messagingRecreateAllowedAnnotation)
				}
				if derr := c.Delete(ctx, u); derr != nil && !apierrors.IsNotFound(derr) {
					return false, fmt.Errorf("deleting owned RabbitmqCluster %q for scale-down: %w", key.Name, derr)
				}
				return false, nil
			}
			if !found || current != desired {
				if serr := unstructured.SetNestedField(u.Object, desired, "spec", "replicas"); serr != nil {
					return false, fmt.Errorf("setting RabbitmqCluster %q spec.replicas: %w", key.Name, serr)
				}
				if uerr := c.Update(ctx, u); uerr != nil {
					return false, fmt.Errorf("updating owned RabbitmqCluster %q replicas: %w", key.Name, uerr)
				}
			}
		}
	}

	return unstructuredConditionTrue(u, "AllReplicasReady"), nil
}

// unstructuredReady reports whether an unstructured object carries a
// status.conditions entry of type "Ready" with status "True". It is the "Ready"
// specialisation of unstructuredConditionTrue, so a missing or malformed
// conditions list is treated as not-ready rather than an error and a
// freshly-created child simply requeues.
func unstructuredReady(u *unstructured.Unstructured) bool {
	return unstructuredConditionTrue(u, "Ready")
}

// unstructuredConditionTrue reports whether an unstructured object carries a
// status.conditions entry of type conditionType with status "True". Operators
// disagree on which condition means healthy (the memcached-operator sets Ready,
// the RabbitMQ Cluster Operator sets AllReplicasReady and no Ready at all), so
// the condition type is the caller's choice. A missing or malformed conditions
// list reads as false rather than an error, so a freshly-created child simply
// requeues.
func unstructuredConditionTrue(u *unstructured.Unstructured, conditionType string) bool {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == conditionType && cond["status"] == string(metav1.ConditionTrue) {
			return true
		}
	}
	return false
}
