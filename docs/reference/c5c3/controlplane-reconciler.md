---
title: ControlPlane Reconciler Architecture
quadrant: operator
---

# ControlPlane Reconciler Architecture

Reference documentation for the `ControlPlaneReconciler`, the
`CredentialRotationReconciler`, and their sub-reconciler contracts.
The `ControlPlaneReconciler` implements the control loop that drives a
`ControlPlane` CR from desired state to a fully operational Keystone control
plane: backing infrastructure, the projected Keystone service, the K-ORC admin
application credential, and the OpenStack service-catalog entries.

For CRD type definitions and webhooks, see
[ControlPlane CRD API Reference](./controlplane-crd.md). For the shared
controller-manager bootstrap pattern (`internal/common/bootstrap`) the c5c3
operator reuses verbatim, see the
[Keystone Reconciler — Controller Registration](../keystone/keystone-reconciler.md#controller-registration)
section. For the library functions used by the sub-reconcilers, see
[Kubernetes-Interacting Packages](../backend/kubernetes-packages.md). For the
infrastructure stack (MariaDB, Memcached, K-ORC, OpenBao) the operator targets,
see [Infrastructure Manifests](../infrastructure/infrastructure-manifests.md).

The c5c3 operator is intentionally a *thin orchestrator*: it provisions and
owns child CRs (MariaDB, Memcached, Keystone, Horizon, K-ORC
`ApplicationCredential` / `Service` / `Endpoint`) and aggregates their readiness. It does **not**
re-implement the per-service logic those child operators already own. As a
consequence the c5c3 API surface is deliberately smaller than the
[Keystone reconciler](../keystone/keystone-reconciler.md)'s: its reconcile chain
is a blocking prefix plus a sequential, non-short-circuiting tail group
(`RunSequentialGroup`) — not the keystone reconciler's goroutine-backed parallel
groups (`RunParallelGroup`) — and it keeps no per-CR metric cardinality. It does
install a single finalizer to sequence K-ORC teardown ahead of
Keystone/infrastructure teardown on deletion — see
[Owner-ref / GC model](#owner-ref--gc-model).

## Controller Registration

The c5c3 operator registers **two** reconcilers and an optional webhook with the
controller manager in `operators/c5c3/main.go` via the shared bootstrap package
(`github.com/c5c3/cobaltcore/internal/common/bootstrap`). The bootstrap helper builds
the manager, wires leader election, and invokes the operator's `SetupFunc`; the
same pattern is documented in detail under
[Keystone Reconciler — Controller Registration](../keystone/keystone-reconciler.md#controller-registration).

```go
import (
    "github.com/c5c3/cobaltcore/internal/common/bootstrap"
    c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
    "github.com/c5c3/cobaltcore/operators/c5c3/internal/controller"
)

const leaderElectionID = "c5c3.openstack.c5c3.io"

bootstrap.Run(bootstrap.ManagerConfig{
    Scheme:           scheme,
    LeaderElectionID: leaderElectionID,
    TargetClusters:   true,
    SetupFunc: func(mcMgr mcmanager.Manager, webhooks bool) error {
        mgr := mcMgr.GetLocalManager()
        if err := (&controller.ControlPlaneReconciler{
            Client:   mgr.GetClient(),
            Scheme:   mgr.GetScheme(),
            Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
            Resolver: mcMgr,
        }).SetupWithManager(mcMgr); err != nil {
            return err
        }
        if err := (&controller.CredentialRotationReconciler{
            Client:   mgr.GetClient(),
            Scheme:   mgr.GetScheme(),
            Recorder: mgr.GetEventRecorderFor("credentialrotation-controller"),
        }).SetupWithManager(mgr); err != nil {
            return err
        }
        if webhooks {
            return (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetClient()}).
                SetupWebhookWithManager(mgr)
        }
        return nil
    },
})
```

| Element | Value |
| --- | --- |
| `LeaderElectionID` | `c5c3.openstack.c5c3.io` (a package-level constant in `main.go`; referenced by the deploy-stack RBAC and asserted by `main_test.go` so a rename cannot silently break leader election) |
| Primary reconciler | `ControlPlaneReconciler` (event recorder `controlplane-controller`), completed through the **multicluster** builder: it takes `mcMgr`, and its `Resolver` turns a service's [`targetClusterRef`](../target-clusters.md) into the client that service's children are written with |
| Secondary reconciler | `CredentialRotationReconciler` (event recorder `credentialrotation-controller`), on the local manager: it only ever touches the management cluster |
| `TargetClusters` | `true`: the binary engages the clusters registered in `--clusters-namespace`. The provider engages nothing while that namespace holds no registration Secret, which is the single-cluster default every existing install keeps |
| Webhook | `ControlPlaneWebhook`, registered **only** when `bootstrap.Run` passes `webhooks == true` to `SetupFunc` (the bool is resolved once by the bootstrap layer from the manager environment) |

### Scheme Registration

The operator registers these schemes in `main.go`'s `init()` so the reconcilers
can interact with the typed child CRDs:

| Module | Scheme | Types Used |
| --- | --- | --- |
| `k8s.io/client-go/kubernetes/scheme` | `clientgoscheme.AddToScheme` | core Kubernetes types (`Secret`, `Event`) |
| `github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1` | `c5c3v1alpha1.AddToScheme` | `ControlPlane`, `CredentialRotation`, `SecretAggregate` (own API) |
| `github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1` | `keystonev1alpha1.AddToScheme` | `Keystone` (projected and owned child) |
| `github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1` | `placementv1alpha1.AddToScheme` | `Placement` (projected and owned child) |
| `github.com/mariadb-operator/mariadb-operator` | `mariadbv1alpha1.AddToScheme` | `MariaDB` (projected and owned child) |
| `github.com/external-secrets/external-secrets` | `esov1alpha1.SchemeBuilder` | `PushSecret` (admin-credential mirror) |
| `github.com/external-secrets/external-secrets` | `esov1.SchemeBuilder` | `ExternalSecret`, `ClusterSecretStore`, `SecretStore` (K-ORC clouds.yaml gate + per-ControlPlane store selection) |
| `github.com/k-orc/openstack-resource-controller/v2` | `orcv1alpha1.AddToScheme` | `ApplicationCredential`, `Service`, `Endpoint` |

> **Note (Memcached is unstructured):** `memcached.c5c3.io` ships **no Go
> module**, so the `Memcached` child is **deliberately not** registered in the
> scheme. `reconcileInfrastructure` builds and applies it as an
> `*unstructured.Unstructured` carrying the shared `memcachedGVK`
> (`memcached.c5c3.io/v1beta1`, kind `Memcached`), and `SetupWithManager`
> `Owns` the same unstructured GVK. The GVK is resolved against the cluster
> `RESTMapper` at runtime, so no scheme entry is required.

### Watches

The controller watches the primary `ControlPlane` CR, every child CR the
sub-reconcilers project (including the owned ESO `ExternalSecret` and
`PushSecret`), the admin-password `Secret`, and both OpenBao-backed store
kinds (`ClusterSecretStore` and `SecretStore`):

| Resource | Watch Type | Effect |
| --- | --- | --- |
| `ControlPlane` | `For()` | Triggers reconciliation on CR changes |
| `MariaDB` | `Owns()` | Re-reconciles the owning ControlPlane when the managed MariaDB child status changes |
| `Memcached` (unstructured `memcachedGVK`) | `Owns()` | Re-reconciles when the managed Memcached child status changes; owned as `*unstructured.Unstructured` because the kind has no Go module |
| `Keystone` | `Owns()` | Re-reconciles when the projected Keystone child status changes |
| `Horizon` | `Owns()` | Re-reconciles when the projected Horizon dashboard child status changes |
| `Glance` | `Owns()` | Re-reconciles when the projected Glance image-service child status changes |
| `GlanceBackend` | `Owns()` | Re-reconciles when a projected GlanceBackend child status changes |
| `Placement` | `Owns()` | Re-reconciles when the projected Placement child status changes |
| `Barbican` | `Owns()` | Re-reconciles when the projected Barbican key-manager child status changes |
| `BarbicanSecretStore` | `Owns()` | Re-reconciles when the projected BarbicanSecretStore status changes |
| `Neutron` | `Owns()` + cross-namespace `Watches()` | Re-reconciles when the projected Neutron network-service child status changes. The neutron-operator is installed only for a ControlPlane that runs the network service, so both legs sit behind the discovery probe with the other sibling-operator kinds |
| `OpenBaoCluster`, `OpenBaoTenant` | `Owns()` | Re-reconciles when the OpenBao instance provisioned for a dedicated Barbican secret store, or the tenant admitting its namespace, changes. The openbao-operator is installed only for that mode, so a ControlPlane without one runs on a cluster that never serves these kinds; both legs sit behind the discovery probe with the other sibling-operator kinds (`probeOptionalWatches`, which skips the leg and registers a leader-gated re-check that restarts the operator once the CRD appears) |
| `RabbitmqCluster` (unstructured `rabbitmqClusterGVK`) | `Owns()` + cross-namespace `Watches()` | Re-reconciles when the managed message-bus child status changes, so `InfrastructureReady` follows `AllReplicasReady` instead of waiting for the periodic requeue. Watched as `*unstructured.Unstructured`, since the c5c3 operator takes no dependency on the RabbitMQ Cluster Operator's Go module. Messaging is opt-in, so both legs sit behind the discovery probe with the openbao kinds: a cluster that does not serve `rabbitmqclusters.rabbitmq.com` starts without them, and `crdWatchGate` restarts the operator once the CRD appears |
| `OVNCentral` | `Watches()` | Per-CR fan-out via `ovnCentralToControlPlaneMapper`. The central is deployed outside the plane and only named by `spec.services.neutron.ovn.centralRef`, so it carries no owner reference an `Owns()` could match; the leg re-runs `reconcileOVN` when the central's status moves instead of waiting for the periodic requeue. The ovn-operator is installed only for a plane that runs a network service, so the leg sits behind the discovery probe with the other sibling-operator kinds |
| K-ORC `ApplicationCredential` | `Owns()` | Re-reconciles when the minted admin credential's `Available` condition or `status.id` changes |
| K-ORC `Service` | `Owns()` | Re-reconciles when the identity catalog Service changes |
| K-ORC `Endpoint` | `Owns()` | Re-reconciles when the public identity Endpoint changes |
| K-ORC `User`, `Domain` | `Owns()` | Re-reconciles when the admin identity imports change. These, the `ApplicationCredential` and the identity `Service`/`Endpoint` are the K-ORC CRs the ControlPlane itself owns (`orcChildObjects`) |
| K-ORC `Project`, `Role`, `RoleAssignment` | `Owns()` | Registered, but no CR the ControlPlane projects carries them any more. A registration's project, role imports and role assignments are claimed by its own `KeystoneService` (`claimKeystoneServiceChild`: a controller owner reference to the **registration** when co-located with it, ownership labels otherwise), never by the ControlPlane, so an `Owns()` leg keyed on a ControlPlane owner reference does not match them. Such a change wakes the registration's controller, and the ControlPlane sees it one step later when the registration's status moves, through the legs below |
| `ExternalSecret` | `Owns()` | Re-reconciles when an owned ESO ExternalSecret (DB credential, admin password, K-ORC clouds.yaml) syncs or fails, so the credential conditions track ESO promptly |
| `PushSecret` | `Owns()` | Re-reconciles when the owned admin-credential PushSecret status changes |
| `KeystoneService` (co-located child) | `Owns()` | Re-reconciles when a registration child projected for a built-in service changes. Both gates the service legs apply are status-only reads, and the standalone mapper leg below drops exactly those writes, so without this leg a child going Ready would only reach the ControlPlane at the next periodic requeue |
| `KeystoneService` (placed child) | `Watches()` | The label-predicate twin, for a registration child of a service placed in a namespace of its own, which can carry no owner reference |
| `KeystoneService` (standalone) | `Watches()` | Per-CR fan-out via `keystoneServiceToControlPlaneMapper`, behind `watch.CRUpdatePredicate()`: which allowlisted namespaces host a registration is what decides where `reconcileRegistrationTenantStores` provisions a store, so a CR appearing or being deleted has to re-drive the plane. Such a CR is no child of the ControlPlane, so it is mapped by its own `controlPlaneRef`, and the predicate drops the KeystoneService controller's status-only writes, which cannot move the provisioning set |
| `Secret` | `Watches()` | Maps Secret events to referencing ControlPlane CRs via the `ControlPlaneSecretNameIndexKey` field indexer (`secretToControlPlaneMapper`) |
| `ClusterSecretStore` | `Watches()` | Per-ref fan-out via `storeToControlPlaneMapper` (bound to the shared `watch.StoreRefFanOut` for the cluster kind): a status change on a cluster-scoped store enqueues only the ControlPlanes whose effective `spec.secretStoreRef` resolves to it |
| `SecretStore` | `Watches()` | The namespaced twin, scoped to the store's own namespace, so a ControlPlane pinned to a per-tenant `SecretStore` reacts to its backend health (`storeToControlPlaneMapper` for the namespaced kind) |
| projected service children + `Namespace` (cross-namespace) | `Watches()` | Label-predicate twin of the `Owns()` rows for a service placed in a namespace of its own: a cross-namespace child carries no owner reference (Kubernetes forbids one), so `Keystone` / `Horizon` / `Glance` / `GlanceBackend` / `Placement` / `Barbican` / `BarbicanSecretStore` / `Neutron` / `OpenBaoCluster` / `OpenBaoTenant` / `RabbitmqCluster` / `MariaDB` / `Memcached` / `ExternalSecret` / `KeystoneService` — and the `Namespace` itself — are watched a second time through the ownership labels the projections stamp (`crossNamespaceChildHandler` gated by `crossNamespaceChildPredicate`), so same-namespace children keep flowing through `Owns()` alone and neither leg double-enqueues the other's objects |

The `Secret` watch uses `Watches()` with a `MapFunc` rather than `Owns()`
because the admin-password Secret
(`spec.korc.adminCredential.passwordSecretRef`) is typically **ESO-managed** —
it is owned by the ExternalSecret controller, not by the ControlPlane CR — so an
owner-reference filter would never match it. The index-backed namespace List is
exactly what wakes the ControlPlane when its admin password rotates, so the
re-mint chain (see [K-ORC admin credential chain](#k-orc-admin-credential-chain))
converges on watch delivery instead of waiting for the next periodic requeue.

Both store watches are per-ref fan-outs rather than blanket enqueues: a status
transition (for example ESO losing the backend connection) wakes only the
ControlPlanes whose effective `spec.secretStoreRef` resolves to the changed
store — the cluster watch lists every namespace, the namespaced watch only the
store's own. A ControlPlane pinned to a different store stays untouched. This is
why the DB-credential, admin-password, and admin-credential sub-reconcilers can
flip their conditions to `SecretStoreNotReady` the moment the ControlPlane's own
backend becomes unreachable instead of waiting up to a full ESO refresh interval
(default 1h) for the next per-secret re-sync. The `ExternalSecret`/`PushSecret`
children are owned (controller reference), so `Owns()` wires them directly.

#### Target-cluster watch legs

Every leg above is pinned to the management cluster, with
`EngageLocalCluster` and `EngageNoProviderClusters` on each one. The
`ControlPlane` CR, the K-ORC CRs, and the service CRs live there whatever a
service names, so a provider-cluster leg would only watch objects this operator
never wrote.

The kinds the ControlPlane writes into the namespace of a service placed on a
[target cluster](../target-clusters.md) take the opposite pinning: no local
engagement, provider clusters only, and a per-cluster filter
(`ClusterServesKind`) that skips the leg on a cluster not serving the kind.
Eleven kinds are watched that way: `MariaDB`, `Memcached`, `Certificate`,
`ExternalSecret`, `PushSecret`, `VaultDynamicSecret`, `SecretStore`, `Secret`,
`OpenBaoCluster`, `OpenBaoTenant`, and `Namespace`. `Memcached` and
`Certificate` are watched as `*unstructured.Unstructured` under the same
`memcachedGVK` / `certificateGVK` values the local legs use, neither kind having
a Go module the operator can type against. `Secret` and `SecretStore` carry two
legs each: one maps a child back through its ownership labels, the other maps an
input through the same mapper the management-side watch uses. The workqueue
deduplicates what the two produce.

What keeps a registered cluster from reaching a ControlPlane that never named it
is `RemoteRequestsAmong`. For every request a mapper produces it reads that
ControlPlane on the management cluster and compares the cluster the event arrived
from against `TargetClusterNames()`, the deduplicated set of the five per-service
refs; a cluster outside the set drops the event. The set comes from the CR, never
from the object that raised the event, so an object planted in a shared namespace
on any registered cluster cannot name a ControlPlane it does not belong to. A CR
already deleted answers `NotFound` and the event is dropped in silence; any other
read error is logged and the event dropped too. Surviving requests carry no
cluster name, so the reconcile they trigger runs against the management cluster,
where the CR is.

#### Secret Field Indexer

The controller registers a controller-runtime field indexer on the
`ControlPlane` kind so that a Secret event resolves to the referencing
ControlPlane CR(s) via an O(1) cache lookup instead of an unfiltered
namespace-scoped List, mirroring the keystone operator's
`KeystoneSecretNameIndexKey`.

| Aspect | Value |
| --- | --- |
| Index key | `ControlPlaneSecretNameIndexKey = "spec.korc.adminCredential.passwordSecretRef.name"` (exported package-level constant in `operators/c5c3/internal/controller/controlplane_controller.go`) |
| Indexed fields | `spec.korc.adminCredential.passwordSecretRef.name` — currently the only Secret a ControlPlane references. The extractor (`controlPlaneSecretNameExtractor`) returns an empty slice when the name is unset so an unset field does not pollute the index, and returns `nil` if invoked with the wrong type rather than panicking. |
| Registration site | `SetupWithManager` → `registerControlPlaneSecretNameIndex(ctx, local.GetFieldIndexer())`, invoked **before** the `Watches(Secret, …)` chain. It goes on the **local** field indexer rather than the multicluster manager's: with a provider configured, that one registers its indexes against the target clusters, which hold no `ControlPlane` CR, and applying it while engaging a cluster would fail that engagement. Every request the legs emit is pinned to the management cluster, so a remote event still resolves its CR through this index. Any error from `IndexField` is wrapped with the index key and propagated, so manager startup aborts loudly if registration fails. |
| Lookup site | `secretToControlPlaneMapper(local.GetClient())` — performs a namespace-scoped `client.List` with `client.MatchingFields{ControlPlaneSecretNameIndexKey: secret.Name}`. On List error the error is logged via `log.FromContext` and the mapper returns `nil` per the `handler.MapFunc` contract (it must not return errors). |
| Result | Each matching ControlPlane in the Secret's namespace is enqueued as a `reconcile.Request`; an event matching no ControlPlane returns `nil`. |

> **Why no owner-ref fallback?** Unlike the keystone operator, the c5c3 Secret
> mapper has a **pure index-backed** lookup with no owner-reference fallback
> branch — the ControlPlane projects no rotation-staging Secrets that are
> owned-but-unreferenced, so the union/owner-ref complexity of
> `secretToKeystoneMapper` is not needed here.

---

## Reconciler Struct

```go
type ControlPlaneReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder
}
```

| Field | Type | Purpose |
| --- | --- | --- |
| `Client` | `client.Client` | Kubernetes API client for CRUD operations (embedded) |
| `Scheme` | `*runtime.Scheme` | Runtime scheme for owner-reference resolution |
| `Recorder` | `record.EventRecorder` | Records Kubernetes events for state transitions |

The `CredentialRotationReconciler` has the identical three-field shape
(`client.Client` embedded, `Scheme`, `Recorder`).

---

## RBAC Permissions

RBAC markers on the two reconcilers generate the required ClusterRole. The
`ControlPlaneReconciler` markers (in `controlplane_controller.go`):

| API Group | Resources | Verbs |
| --- | --- | --- |
| `c5c3.io` | `controlplanes` | get, list, watch, create, update, patch, delete |
| `c5c3.io` | `controlplanes/status` | get, update, patch |
| `c5c3.io` | `controlplanes/finalizers` | update |
| `c5c3.io` | `credentialrotations` | get, list, watch, create, update, patch, delete |
| `c5c3.io` | `credentialrotations/status` | get, update, patch |
| `c5c3.io` | `secretaggregates` | get, list, watch |
| `k8s.mariadb.com` | `mariadbs` | get, list, watch, create, update, patch, delete |
| `memcached.c5c3.io` | `memcacheds` | get, list, watch, create, update, patch, delete |
| `rabbitmq.com` | `rabbitmqclusters` | get, list, watch, create, update, patch, delete |
| `keystone.openstack.c5c3.io` | `keystones` | get, list, watch, create, update, patch, delete |
| `placement.openstack.c5c3.io` | `placements` | get, list, watch, create, update, patch, delete |
| `barbican.openstack.c5c3.io` | `barbicans`, `barbicansecretstores` | get, list, watch, create, update, patch, delete |
| `neutron.openstack.c5c3.io` | `neutrons` | get, list, watch, create, update, patch, delete |
| `ovn.openstack.c5c3.io` | `ovncentrals` | get, list, watch |
| `openbao.org` | `openbaoclusters`, `openbaotenants` | get, list, watch, create, update, patch, delete |
| `rbac.authorization.k8s.io` | `roles`, `rolebindings`, `clusterrolebindings` | get, create, patch, delete |
| `rbac.authorization.k8s.io` | `clusterroles` (`resourceNames: system:auth-delegator`) | bind |
| `openstack.k-orc.cloud` | `applicationcredentials`, `services`, `endpoints` | get, list, watch, create, update, patch, delete |
| `external-secrets.io` | `externalsecrets`, `pushsecrets` | get, list, watch, create, update, patch, delete |
| `external-secrets.io` | `clustersecretstores`, `secretstores` | get, list, watch |
| `core` | `secrets` | get, list, watch, create, update, patch, delete |
| `core` | `events` | create, patch |

The `CredentialRotationReconciler` markers (in
`reconcile_credentialrotation.go`) are scoped tighter — it never mints, so it
holds only `update`/`patch` (not `create`/`delete`) on K-ORC
`applicationcredentials` and read-only access to `controlplanes`:

| API Group | Resources | Verbs |
| --- | --- | --- |
| `c5c3.io` | `credentialrotations` | get, list, watch, create, update, patch, delete |
| `c5c3.io` | `credentialrotations/status` | get, update, patch |
| `c5c3.io` | `controlplanes` | get, list, watch |
| `openstack.k-orc.cloud` | `applicationcredentials` | get, list, watch, update, patch |
| `core` | `secrets` | get, list, watch |
| `core` | `events` | create, patch |

### Blast radius and namespace scoping

By default the chart binds these markers to a cluster-wide `ClusterRole`, so the
`secrets` rule lets a compromised operator pod read and write every `Secret` in
every namespace; the
[Multi-Tenant Deployment → Security trade-off](../../guides/multi-tenant-deployment.md#security-trade-off-the-cluster-wide-rbac-default)
details that privilege-escalation path. Two specifics apply to this operator:

- It amplifies the exposure itself: `reconcileAdminPassword` and `reconcileKORC`
  project the OpenStack admin password **in cleartext** into a `clouds.yaml`
  `Secret`, so cluster-wide read access exposes every projected admin password.
- It writes RBAC. The dedicated Barbican secret store needs `get`, `create`,
  `patch`, and `delete` on `roles`, `rolebindings`, and `clusterrolebindings` to
  give the OpenBao instance its TokenReview and the barbican operator a bound
  provisioner token. What a forged binding can grant is still capped by the API
  server's privilege-escalation check: the operator holds `escalate` nowhere and
  `bind` only on `system:auth-delegator` (narrowed by `resourceNames`), so it can
  hand out no permission it does not already hold. The cluster-wide Secret read
  therefore remains the dominant risk.

A single-namespace deployment — one where no service is placed in a namespace of
its own — co-locates every projected resource in the ControlPlane's own
namespace, so it can run the operator namespace-scoped
(`rbac.namespaceScoped: true`), bounding both the RBAC grant and the informer
cache to that namespace. Keep the default only when
[cluster-wide RBAC is still required](../../guides/multi-tenant-deployment.md#when-cluster-wide-rbac-is-still-required).

[Dedicated service namespaces](./controlplane-crd.md#service-namespaces) are
**incompatible with namespace-scoped mode**: placing a service in a namespace of
its own needs cluster-scoped `namespaces` verbs (`create`, `delete`) and
cross-namespace access to the children, which only the default ClusterRole mode
grants. The markers therefore add `core/namespaces` with
`get;list;watch;create;delete`.

---

## Reconciliation Flow

```text
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                    CONTROLPLANE RECONCILIATION FLOW                                 │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│  ControlPlane CR changed (or requeue timer fires)                                   │
│         │                                                                           │
│         ▼                                                                           │
│  Fetch ControlPlane CR (return empty result if NotFound)                            │
│         │                                                                           │
│         ▼                                                                           │
│  Duplicate guard — park all but the oldest ControlPlane in the namespace            │
│  (Ready=False / DuplicateControlPlane, requeue 30s; see Multi-instance)             │
│         │                                                                           │
│         ▼                                                                           │
│  ┌──────────────────────────┐                                                       │
│  │ reconcileNamespaces      │  Ensure the namespaces services are placed in         │
│  │  (gate: none)            │  Sets: NamespacesReady                                │
│  └────────┬─────────────────┘  Requeue: 15s while a namespace is unusable           │
│           │  (True immediately when no service declares a namespace)                │
│           ▼                                                                         │
│  ┌──────────────────────────┐                                                       │
│  │ reconcileInfrastructure  │  Ensure managed MariaDB + Memcached children          │
│  │  (gate: none)            │  Sets: InfrastructureReady                            │
│  └────────┬─────────────────┘  Requeue: 15s while a child is not Ready              │
│           │  early-return if !result.IsZero() || err                                │
│           ▼                                                                         │
│  ┌──────────────────────────┐                                                       │
│  │ reconcileESOTenantStore  │  Provision the per-tenant SecretStore + SA +          │
│  │  (gate: none)            │  mTLS cert. Sets: ESOTenantStoreReady                 │
│  └────────┬─────────────────┘  Requeue: 10s while the store is not Ready            │
│           │  (skipped when spec.secretStoreRef overrides the default)               │
│           ▼                                                                         │
│  ┌──────────────────────────┐                                                       │
│  │ reconcileDBCredentials   │  Project per-CP DB-credential ExternalSecret          │
│  │  (gate: none)            │  Sets: DBCredentialsReady                             │
│  └────────┬─────────────────┘  Requeue: 10s while the ES is not yet synced          │
│           │                                                                         │
│           ▼                                                                         │
│  ┌──────────────────────────┐                                                       │
│  │ reconcileAdminPassword   │  Project per-CP admin-password ExternalSecret         │
│  │  (gate: none)            │  Sets: AdminPasswordReady                             │
│  └────────┬─────────────────┘  Requeue: 10s while the ES is not yet synced          │
│           │                                                                         │
│           ▼                                                                         │
│  ┌──────────────────────────┐                                                       │
│  │ reconcileKeystone        │  Project the Keystone child CR                        │
│  │  (gate: InfraReady)      │  Sets: KeystoneReady                                  │
│  └────────┬─────────────────┘  Requeue: 5s gated / 15s child not Ready              │
│           │                                                                         │
│           ▼                                                                         │
│  ╔════════════════════════════════════════════════════════════════════════════════╗ │
│  ║  RunSequentialGroup — tail group · non-short-circuiting                        ║ │
│  ║                                                                                ║ │
│  ║  every member runs each pass · each member condition always persists           ║ │
│  ║                                                                                ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcileHorizon         │  Project the Horizon dashboard child CR          ║ │
│  ║  │  (gate: KeystoneReady)   │  Sets: HorizonReady (not-managed when unset)     ║ │
│  ║  └────────┬─────────────────┘  Requeue: 5s gated / 15s child not Ready         ║ │
│  ║           │                                                                    ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcileKORC            │  Mint the admin ApplicationCredential            ║ │
│  ║  │  (gate: none*)           │  Sets: KORCReady                                 ║ │
│  ║  └────────┬─────────────────┘  Requeue: 10s while AC not Available             ║ │
│  ║           │                                                                    ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcileAdminCredential │  Commit minted Secret + PushSecret to OpenBao    ║ │
│  ║  │  (gate: KORCReady)       │  Sets: AdminCredentialReady                      ║ │
│  ║  └────────┬─────────────────┘  Requeue: 10s gated / clouds.yaml not Ready      ║ │
│  ║           │                                                                    ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcileCatalog         │  Register identity Service + public Endpoint     ║ │
│  ║  │  (gate: AdminCredReady)  │  Sets: CatalogReady (Service+Endpoint Available) ║ │
│  ║  └────────┬─────────────────┘  Requeue: 10s gated / not Available / terminal   ║ │
│  ║           │                                                                    ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcileGlance          │  Project the Glance image-service child CR       ║ │
│  ║  │ (gate: KS + its registr.)│  Sets: GlanceReady (not-managed when unset)      ║ │
│  ║  └────────┬─────────────────┘  Requeue: 5s gated / 15s child not Ready         ║ │
│  ║           │                                                                    ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcilePlacement       │  Project the Placement child CR                  ║ │
│  ║  │ (gate: KS + its registr.)│  Sets: PlacementReady (not-managed when unset)   ║ │
│  ║  └────────┬─────────────────┘  Requeue: 5s gated / 15s child not Ready         ║ │
│  ║           │                                                                    ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcileBarbican        │  Project the Barbican child, its secret store,   ║ │
│  ║  │ (gate: KS + its registr.)│  and a dedicated OpenBao instance                ║ │
│  ║  └────────┬─────────────────┘  Sets: BarbicanReady (not-managed when unset)    ║ │
│  ║           │                    Requeue: 5s gated / 15s instance or child       ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcileOVN             │  Mirror the referenced OVNCentral's readiness    ║ │
│  ║  │  (gate: none)            │  Sets: OVNReady (not-managed when unset)         ║ │
│  ║  └────────┬─────────────────┘  Requeue: 15s while the central is not usable    ║ │
│  ║           │                                                                    ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcileNeutron         │  Deliver the shared bus, then project the        ║ │
│  ║  │ (gate: KS + OVN + its    │  Neutron network-service child CR                ║ │
│  ║  │  registration)           │  Sets: NeutronReady (not-managed when unset)     ║ │
│  ║  └────────┬─────────────────┘  Requeue: 5s gated / 15s child or bus / 10s reg. ║ │
│  ║           │                                                                    ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────┐                                                  ║ │
│  ║  │ reconcileServiceAccounts │  Fold the four registration children's           ║ │
│  ║  │  (gate: none)            │  readiness. Sets: ServiceAccountsReady           ║ │
│  ║  └────────┬─────────────────┘  Requeue: 10s while one is not Ready             ║ │
│  ║           │                                                                    ║ │
│  ║           ▼                                                                    ║ │
│  ║  ┌──────────────────────────────┐                                              ║ │
│  ║  │ reconcileRegistrationTenant- │  Tenant-store trio in each allowlisted       ║ │
│  ║  │ Stores    (gate: none)       │  registration namespace                      ║ │
│  ║  └──────────────────────────────┘  Sets: RegistrationTenantStoresReady         ║ │
│  ║                                    Requeue: 10s while a store is not Ready     ║ │
│  ║                                                                                ║ │
│  ║  member requeues → ShortestRequeue · member errors → errors.Join               ║ │
│  ╚═══════════╤════════════════════════════════════════════════════════════════════╝ │
│              │                                                                      │
│              ▼                                                                      │
│  setReadyCondition()  — aggregate Ready = AllTrue(subConditionTypes)                │
│  updateStatus()       — stamp status.observedGeneration, persist                    │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘

  * reconcileKORC has no condition gate, but it defers (KORCReady=False,
    requeue) until the admin-password Secret can be read.
```

### Execution Model

The chain runs in **two phases** over the shared scaffolding in
`internal/common/reconcile` (the same building blocks the keystone controller
uses). Every step is a `commonreconcile.Step`, and the whole table is driven by
one `commonreconcile.RunPipeline` call:

**Phase 1 — the blocking prefix.** Namespaces → Infrastructure → ESOTenantStore
→ DBCredentials → AdminPassword → Keystone run as six named, short-circuiting
`Step` entries. `RunPipeline` returns the pass at the **first non-zero result or
error**, because each step genuinely feeds the next — a later step applying
before its predecessor converged would fail or wedge. Each named step is wrapped
in `instrumenter.Instrument` (see
[Metrics Instrumentation](#metrics-instrumentation)) so a duration sample and an
error counter are emitted under a stable `sub_reconciler` label.

**Phase 2 — the tail group.** Horizon, KORC, AdminCredential, Catalog, Glance,
Placement, Barbican, OVN, Neutron, ServiceAccounts and RegistrationTenantStores
are the eleven named members of one `commonreconcile.RunSequentialGroup`,
embedded as the pipeline's final **bare (unnamed)** `Step`. The group members
self-instrument through `instrumenter.Instrument` — following the keystone
self-instrumenting-group convention — which is why the enclosing group step
carries no `sub_reconciler` name of its own. `RunSequentialGroup` attempts
**every** member on **every** pass and never short-circuits: each member
self-gates on the conditions it needs (Horizon on `KeystoneReady`; KORC until the
admin-password Secret is readable; AdminCredential on `KORCReady`; Catalog on
`AdminCredentialReady`; Glance, Placement, Barbican and Neutron on `KeystoneReady`
and on the `AccountReady` of the `KeystoneService` registration each projects for
itself, see [Built-in service registrations](#built-in-service-registrations)), so
running all of them each pass is safe. Barbican carries one gate the others do
not: on a dedicated secret store it holds the projection until the OpenBao
instance it provisions serves requests. Neutron carries two: `OVNReady`, and the
shared message bus having been delivered into its namespace. OVN itself carries
none, because the `OVNCentral` it mirrors is deployed outside the plane and
nothing this chain produces can converge it.

The last two members carry **no** gate, and their order in the group is what
makes that safe. ServiceAccounts only folds the registration children the four
service legs wrote earlier in the same pass, so it must run after them and has
nothing to defer. RegistrationTenantStores consumes no condition this chain
produces — the trio it writes depends on cert-manager and OpenBao alone, exactly
like its blocking-prefix twin — and sits in the group rather than in that prefix
so a namespace the control plane does not own can never park DBCredentials,
AdminPassword and Keystone behind it.

```go
pipeline := []commonreconcile.Step{
    {Name: "Namespaces", Fn: func(ctx context.Context) (ctrl.Result, error) {
        return r.reconcileNamespaces(ctx, &cp)
    }},
    // ... Infrastructure, ESOTenantStore, DBCredentials, AdminPassword, Keystone
    //     — the blocking prefix, each short-circuiting via RunPipeline.
    //
    // The independent tail projections run as one self-instrumenting,
    // non-short-circuiting group embedded as the final bare Step:
    {Fn: func(ctx context.Context) (ctrl.Result, error) {
        return commonreconcile.RunSequentialGroup(ctx, instrumenter.Instrument,
            []commonreconcile.Step{
                {Name: "Horizon", Fn: /* ... */},
                // KORC, AdminCredential, Catalog, Glance, Placement,
                // Barbican, OVN, Neutron, ServiceAccounts,
                // RegistrationTenantStores
            })
    }},
}
result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, pipeline)
return r.updateStatus(ctx, &cp, statusBefore, result, err)
```

This guarantees:

1. **Prefix (phase 1) — unchanged early-return semantics.** A prefix
   sub-reconciler error **propagates immediately** and a non-zero result
   (`RequeueAfter > 0`) causes an **early return**; either way the subsequent
   prefix steps and the whole tail group are skipped for that pass.
2. **Group (phase 2) — every member runs each pass.** No member's non-zero
   result or error prevents a later member from running, so a still-converging
   or failing Horizon no longer parks KORC, the AdminCredential/Catalog
   identity bootstrap, Glance, Placement, Barbican, OVN, Neutron, or the two
   ungated members that close the group. Each member's condition therefore
   always persists.
3. **Group result aggregation.** When no member errors, the group result is the
   **shortest** member requeue (`commonreconcile.ShortestRequeue`) and the error
   is nil. When one or more members error, the group returns `ctrl.Result{}`
   and the `errors.Join` of every member error — member requeues are
   **discarded**, and controller-runtime's error backoff drives the retry.
4. **Status always persisted.** Whichever phase ends the pass, its outcome
   funnels through `updateStatus()`, so the conditions and the `(result, error)`
   pair are persisted by construction on every exit path.

**Onboarding rule.** A future service whose projection is **independent** of the
others — it self-gates on the conditions it needs rather than being a hard
prerequisite for a later step — joins the **tail group**, not the blocking
prefix. Only a projection that genuinely feeds the next step belongs in the
prefix.

### Status Update Pattern

`updateStatus()` delegates to the shared `commonreconcile.UpdateStatus`: it
stamps `cp.Status.ObservedGeneration = cp.Generation`, records the per-service
status (`setServicesStatus()`, see below), persists all condition changes via
`r.Status().Update()`, and returns the provided `(result, error)` pair.

`Reconcile` snapshots `cp.Status` immediately after the initial Get and
threads that snapshot into `updateStatus`, which compares the computed status
against it with `equality.Semantic.DeepEqual` and **skips** the write when a
pass left status unchanged — no write means no watch event and no
`resourceVersion` churn on a converged steady-state pass. Together with the
`watch.CRUpdatePredicate` on the controller's `For(...)` watch (which filters
the CR's own status-only updates), this closes the self-wake loop the previous
always-write `updateStatus` and bare `For()` allowed.

When both a reconcile error and the status update fail, both errors are
preserved via `errors.Join` so the original reconcile failure remains visible in
controller-runtime logs:

| reconcileErr | `Status().Update()` | Returned error |
| --- | --- | --- |
| nil | succeeds | nil |
| non-nil | succeeds | reconcileErr (unchanged) |
| nil | fails | `errors.Join(nil, fmt.Errorf("updating status: %w", statusErr))` |
| non-nil | fails | `errors.Join(reconcileErr, fmt.Errorf("updating status: %w", statusErr))` |

Because `ObservedGeneration` is stamped on **every** `updateStatus` call (early
return or final), a stale status is always distinguishable from a current one.

### Ready Condition Aggregation

After all sub-reconcilers succeed, `setReadyCondition()` evaluates whether every
sub-condition type is `True` using `aggregateReady()`, which delegates to
`conditions.AllTrue(conds, subConditionTypes...)`:

| All Sub-Conditions True | Ready Condition | Reason | Message |
| --- | --- | --- | --- |
| Yes | `Status: True` | `AllReady` | `All sub-conditions are ready` |
| No (any missing or False) | `Status: False` | `NotAllReady` | `One or more sub-conditions are not ready` |

The aggregated sub-condition types (the source-of-truth `subConditionTypes`
slice in `controlplane_controller.go`) are:

```text
NamespacesReady, InfrastructureReady, ESOTenantStoreReady, DBCredentialsReady, KeystoneReady, HorizonReady, GlanceReady, PlacementReady, BarbicanReady, OVNReady, NeutronReady, KORCReady, AdminCredentialReady, AdminPasswordReady, CatalogReady, ServiceAccountsReady, RegistrationTenantStoresReady
```

The `Ready` condition carries `ObservedGeneration = cp.Generation` so clients can
detect a stale aggregate.

One path bypasses the aggregation entirely: a ControlPlane parked by the
duplicate guard (see [Multi-instance](#multi-instance)) gets `Ready=False` with
reason `DuplicateControlPlane` written directly — `setReadyCondition()` would
otherwise overwrite the reason with `NotAllReady` on the next status update.

### Services and Update Phase

`setServicesStatus()` runs on every `updateStatus` call and populates two status
fields that the schema declared but the reconciler previously never wrote:

| Field | Value |
| --- | --- |
| `status.updatePhase` | Fixed at `Idle` — the release-update state machine is not implemented and the other `UpdatePhase` values are reserved, so "no update in progress" is the current state |
| `status.services` | one entry per managed service, in a stable order: `keystone` (present when `spec.services.keystone` is set), then `horizon` (present when `spec.services.horizon` is set), then `glance` (present when `spec.services.glance` is set), then `placement` (present when `spec.services.placement` is set), then `barbican` (present when `spec.services.barbican` is set), then `neutron` (present when `spec.services.neutron` is set). Each entry's `ready` mirrors the matching `KeystoneReady` / `HorizonReady` / `GlanceReady` / `PlacementReady` / `BarbicanReady` / `NeutronReady` sub-condition (via `conditions.AllTrue`) and `release` is `spec.openStackRelease`; an unmanaged service is omitted rather than reported |

---

## Sub-Reconciler Contracts

Each sub-reconciler owns exactly one Ready sub-condition. The tables below give
each one's gate, what it projects/owns, and the condition reasons it sets on the
`True`, requeue, and error paths. All condition constants are the exported
source-of-truth strings in `controlplane_controller.go`; sub-reconcilers
reference the constants (never inline literals) so a rename is a compile error
and is caught by the no-inline-literals drift guard.

Every condition is stamped with `ObservedGeneration = cp.Generation` on every
path.

**Which cluster a sub-reconciler writes to.** A sub-reconciler that projects into
a service namespace does not reach for the reconciler's own client. It resolves
one per namespace through `childrenClientFor`, which maps the namespace to the
[target cluster](../target-clusters.md) of the service placed there and hands
back that cluster's client. The ControlPlane's own namespace answers with the
local client, and so does any namespace no placed service declares. Co-located
services cannot disagree about the answer, because admission rejects a shared
namespace whose services name different clusters, so the namespace alone decides.
An ensemble that spans several namespaces resolves all of them through
`childrenClientsFor` before its first write: a cluster that does not resolve then
leaves the whole ensemble unwritten rather than half of it, under
`TargetClusterUnavailable` on the sub-reconciler's own condition, carrying the
resolver's message verbatim (`cluster not found` for a name that was never
registered).

A nil `Resolver` resolves nothing. The reconciler is then running without a
multicluster manager, which is how the unit tests and the single-cluster envtest
fixtures construct it, and every namespace answers with the local client whatever
the spec names.

### External keystone mode and the chain

When `spec.services.keystone.mode` is `External`, the ControlPlane manages
identity against a pre-existing, externally-operated Keystone. The chain keeps
its order; External mode changes what each link does. Four sub-reconcilers
short-circuit — `reconcileInfrastructure`, `reconcileDBCredentials`,
`reconcileAdminPassword` and `reconcileKeystone` — each reporting its own
condition with `Status=True` and reason `ExternallyManaged`, and a message naming
`spec.services.keystone.external.authURL`.

Skipped sub-reconcilers **keep their condition types**. The condition schema is
therefore identical across modes, so `subConditionTypes`, `setReadyCondition` and
the `condition_type` drift guard need no mode awareness.

Every skip is keyed on the mode discriminator `cp.IsExternalKeystone()`, never on
the database shape: an External-mode ControlPlane has no `spec.infrastructure`
block at all, so "no *managed* database" and "no database" are different states.
The vocabulary keeps three "nothing was projected" reasons deliberately apart:

| Reason | Meaning |
| --- | --- |
| `ExternallyManaged` | identity is managed against a pre-existing Keystone; this sub-reconciler has nothing to project |
| `KeystoneNotManaged` | `spec.services.keystone` is unset: there is no identity plane at all (staged adoption) |
| `BrownfieldUserSuppliedCredential` | the ControlPlane owns a Keystone, but the *database* is brownfield, so the user supplies its credential Secret |

`services.horizon` is forbidden in External mode, so `reconcileHorizon` always
takes its `HorizonNotManaged` early-exit. The duplicate-ControlPlane parking
guard is mode-agnostic and applies unchanged — External-mode ControlPlanes count
towards the one-per-namespace contract.

`reconcileCatalog` neither skips nor behaves as it does in Managed mode: it
forks. The catalog belongs to the external installation, so External mode is
**import-first** — the existing identity service and its endpoints are imported
read-only, zero catalog entries are created by default, and an import that
resolves to nothing fails loud rather than waiting forever. See
[reconcileCatalog](#reconcilecatalog).

#### Egress and TLS posture

K-ORC — not the c5c3-operator — is what dials the external Keystone. It is
installed by the Flux Kustomization `deploy/flux-system/releases/k-orc.yaml` into
the `orc-system` namespace, and the operator never opens an OpenStack connection
itself: everything stays K-ORC-mediated.

- **Egress.** No `NetworkPolicy` exists anywhere under `deploy/`, and none scopes
  `orc-system`, so nothing restricts K-ORC's egress on the shipped stack —
  out-of-cluster Keystone worked first-pass in the phase-1 spike. A cluster that
  applies a **default-deny egress** policy to `orc-system` must add an explicit
  allow rule to the external endpoint's host and port, or every mint, import and
  catalog call fails as `EndpointUnreachable`.
- **TLS.** An IP-based `authURL` requires an **IP SAN** in the external Keystone's
  server certificate — a CN or DNS SAN alone will not verify. Hostnames resolve
  through cluster DNS via the upstream forwarder, so no extra DNS wiring is
  needed. A privately-signed certificate needs
  `spec.services.keystone.external.caBundleSecretRef`; without it K-ORC reports
  an `x509` failure, classified onto `KORCReady=False/TLSVerificationFailed`.
- **CA-cache aliasing.** K-ORC's provider-client cache keys on the parsed cloud
  struct only — `cacert` is **not** part of the key (`internal/scope/provider.go`)
  — and the entry lives for the token lifetime / 2 (≈30 min at Keystone defaults).
  A rotated or removed CA bundle therefore converges the Secrets immediately but
  the trust store only after cache expiry. Nothing in this operator can shorten
  that window; an upstream fix would have to fold `cacert` into the cache key.

### Reaching a placed service

An in-cluster Service DNS name resolves only inside the cluster the Service runs
on. Every URL the ControlPlane composes therefore depends on where the two ends
sit, and the rule is the same everywhere: the in-cluster URL while both resolve
to the same cluster (`sameTargetCluster` compares the two
[`targetClusterRef`](../target-clusters.md)s, with "no ref" meaning the
management cluster), the public URL as soon as they do not.

- **The four dependents of Keystone.** `horizonKeystoneEndpoint`,
  `glanceKeystoneEndpoint`, `placementKeystoneEndpoint` and
  `barbicanKeystoneEndpoint` compare their own service's ref against Keystone's.
  A service that shares Keystone's cluster keeps the conventional
  `http://{controlplane.Name}-keystone.<keystone-namespace>.svc:5000/v3`; one on
  another cluster gets `keystonePublicEndpoint` — `services.keystone.publicEndpoint`
  when set, else the gateway-derived `https://{gateway.hostname}/v3`.
- **K-ORC and the service accounts.** `korcAuthURL` takes the cluster the
  document is CONSUMED on and answers by the same rule. The two admin documents
  are read by K-ORC, which always runs on the management cluster wherever the
  ControlPlane places its services, so they render the public URL as soon as
  Keystone names a cluster — a placed Keystone's Service DNS name is an address
  K-ORC cannot dial. A service account's `clouds.yaml` is read on the cluster its
  delivery namespace lives on, so that namespace's ref is what it resolves
  against. External mode is untouched, and a co-located Keystone renders the
  in-cluster URL byte for byte as before.
- **The catalog.** A placed service registers its public URL on its `internal`
  interface as well as its `public` one (`internalCatalogURL`), because that
  entry is what K-ORC and every consumer outside the service's cluster resolve.
  The identity row is unaffected: it registers a public interface only.

Admission is what keeps those public URLs from being empty. A placed catalog
service (keystone, glance, placement, barbican) must declare a `publicEndpoint`
or a `gateway`, and Keystone must declare one as soon as ANY other service is
placed away from it — the per-service rule only reaches a service carrying a ref
of its own, so an unplaced Keystone would otherwise leave every dependent child
with an empty `spec.keystoneEndpoint` its own CRD refuses. A placed Keystone's
endpoint must additionally use `https`, because placement promotes it to the
`auth_url` the credential documents render passwords next to. Composing those
URLs is where this ends: making them routable between clusters is the
deployment's job, a gateway or a load balancer per target cluster.

### reconcileNamespaces

| Aspect | Value |
| --- | --- |
| File | `reconcile_namespaces.go` |
| Condition | `NamespacesReady` |
| Gate | none (runs first) |
| Projects / Owns | `Namespace` objects for every service placed in a namespace of its own under the `Managed` lifecycle, on the management cluster and on the target cluster of a service that names one |
| Requeue | `namespaceRequeueAfter` = **15s** while a namespace is unusable |

`reconcileNamespaces` ensures the namespaces the ControlPlane's services are
placed in outside its own (see
[Service Namespaces](./controlplane-crd.md#service-namespaces)), and runs
**first** because every later sub-reconciler projects into one of them — applying
into a namespace that does not exist fails with an error naming neither the
ControlPlane nor the assignment behind it. A ControlPlane with no assignments (the
default) has nothing to ensure and reports `NamespacesReady=True` immediately, so
the step costs nothing on the common path.

The two lifecycles are asymmetric. Under **`Managed`** the operator creates the
namespace and stamps it with the ownership labels plus
`app.kubernetes.io/managed-by`, and on a target cluster the annotation
`c5c3.io/controlplane-uid` as well; a namespace that already exists without the
labels is **never adopted** — the condition fails loud rather than taking over a
namespace it did not create. On a target cluster the mark is required on top of
them, because the labels alone are forgeable there: both are derived from the CR's
name and namespace, so anyone holding `patch` on a namespace of that cluster can
write them, while the UID is minted by the management cluster's API server. A mark
naming a **different** ControlPlane disowns the namespace — and the condition
message names the recorded UID, since such a namespace *was* created by a
ControlPlane of this name, on a cluster several management clusters can register.
A **missing** mark refuses adoption too, since nothing on the object separates a
stripped mark from labels somebody else wrote; that message names both remedies,
restoring the annotation or picking a free name. The read behind the verdict goes
through the target cluster's live reader, as the
[teardown](#owner-ref--gc-model) side does — which draws the missing-mark line one
notch lower, because it is the last pass anything makes over that namespace. Under
**`External`** the operator only verifies the namespace exists; a missing one parks
the condition and requeues.

A namespace whose services carry a [`targetClusterRef`](../target-clusters.md)
is ensured on that cluster as well as on the management cluster, under whichever
lifecycle it declares. Both copies are needed: the workload CR stays at home, in
the namespace the service is assigned to, and the service operator that picks it
up writes its own children into the namespace of the same name on the target.
One reason vocabulary covers both sides, so a namespace missing on the target
reports `NamespaceNotFound` as one missing at home does. The cluster is resolved
per namespace before anything is written, and a name that does not resolve
creates nothing on either side.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `NoDedicatedNamespaces` | No service declares a namespace of its own. |
| `True` | `NamespacesReady` | Every declared service namespace is present (and, for `Managed`, owned). |
| `False` | `NamespaceNotFound` | An `External` namespace does not exist; requeue. |
| `False` | `NamespaceNotOwned` | A `Managed` namespace exists but does not carry the operator's ownership labels, or — on a target cluster — its `c5c3.io/controlplane-uid` annotation is missing or names a different ControlPlane, so it is never adopted. The message distinguishes the three, because the remedies differ. |
| `False` | `NamespaceTerminating` | The namespace is being deleted; wait and requeue. |
| `False` | `TargetClusterUnavailable` | A service placed the namespace on a target cluster that does not resolve; the resolver's own message, `cluster not found` for a name that was never registered. On deletion the same reason marks a placed namespace whose cluster has not answered yet: the teardown waits for it, and gives up on its children only past the abandon window (see [Owner-ref / GC model](#owner-ref--gc-model)). |
| `False` | `FinalizingNamespaces` | On deletion, waiting for cross-namespace children to be torn down (see [Owner-ref / GC model](#owner-ref--gc-model)). |
| `False` | `NamespaceError` | A create/get against the namespace failed. |

### reconcileInfrastructure

| Aspect | Value |
| --- | --- |
| File | `reconcile_infrastructure.go` |
| Condition | `InfrastructureReady` |
| Gate | none |
| Projects / Owns | Managed-mode `MariaDB` (`k8s.mariadb.com`) and `Memcached` (unstructured `memcached.c5c3.io/v1beta1`) children, each named after its `clusterRef` and created in **the namespace of the service that resolves to it** (`cp.KeystoneNamespace()` / `cp.HorizonNamespace()`, the ControlPlane's own unless a `namespace` assignment places the service elsewhere), **on the cluster that namespace lives on**; plus the managed-mode `RabbitmqCluster` (unstructured `rabbitmq.com/v1beta1`), always in **the ControlPlane's own namespace** on the local cluster |
| Requeue | `infraRequeueAfter` = **15s** while a managed child is not yet Ready |

**Backing services follow the service.** `managedInfraInstances` adds each
service's effective database and cache **at that service's namespace** and
deduplicates on `(kind, namespace, name)`, so the one shared `spec.infrastructure`
block materializes once **per namespace** that consumes it — two instances when
Keystone and Horizon are placed apart, one when they are co-located, exactly one
(today's behavior) when neither is assigned a namespace. A child in a service
namespace carries no owner reference (Kubernetes forbids a cross-namespace one) —
it is stamped with the ownership labels and cleaned up by the finalizer instead;
a same-namespace child keeps its controller owner reference. The dashboard's cache
is enumerated only when the dashboard is **declared**, so a ControlPlane that
places Keystone apart never provisions a phantom cache for an absent Horizon.

**The message bus stays home.** `addMessaging` is called once, at
`childNamespace(cp)` with the declared-at path `spec.infrastructure.messaging`,
and it is called whether or not a service consumes the bus. That makes messaging
the single class enumerated at the ControlPlane's own namespace regardless of
consumers: a bus is shared across services by nature, so declaring the block is
what asks for the broker. Brownfield messaging (`secretRef`) enumerates nothing,
the same way a brownfield database does. Consumers reach the managed bus at
`<clusterRef.name>.<cp.Namespace>.svc`. A service placed on a target cluster
cannot reach it from there; that path is open until the first consumer settles it
(issue #906).

A service that names a [`targetClusterRef`](../target-clusters.md) takes its
backing services with it: the instance is created on that cluster, where the
mariadb-operator that acts on the CR runs and where the service's own pods
connect to it. The cluster of every enumerated instance is resolved before the
first one is written, so an unresolvable name leaves the whole set unprovisioned
rather than the database on one cluster and the cache nowhere.

`reconcileInfrastructure` provisions the backing services the ControlPlane owns.
That set is the instances its services actually **resolve to**, not the set of
declared blocks: the **shared** instances in `spec.infrastructure`, and the
per-service **dedicated** instances under
`services.<svc>.dedicatedBackingServices` (see
[DedicatedBackingServices](./controlplane-crd.md#dedicatedbackingservices)) that
a service opted into instead. `managedInfraInstances` enumerates them by walking
the effective-instance resolvers per service and deduplicating on the identity of
the child CR they resolve to — so several services on one shared instance ensure
it exactly once, and a **shared instance every service has opted out of has no
consumer and is not provisioned at all**. The defaulting webhook materializes
`spec.infrastructure.database` whenever it is omitted, so once every declared
database consumer — Keystone, Glance, Placement, Barbican — has taken a
dedicated instance, provisioning the declared set instead would leave a full
Galera cluster nothing talks to — with `InfrastructureReady` blocked on it
coming up. A backing service is **managed**
when its `clusterRef` is set and **brownfield** (provisions nothing) when
`host`/`servers` are set instead.

Every managed child — shared or dedicated — is ensured in a single pass *before*
readiness is gated, so a half-provisioned control plane (DB created but cache
missing) never occurs; readiness is then evaluated **collectively** across the
whole set. A service whose dedicated database is still converging therefore holds
`InfrastructureReady` `False` even when every other instance is Ready, so that
service's projection stays gated on the database it actually talks to. The
condition message names the pending instance and the spec path it was declared
at; the *reasons* stay per-class (`WaitingForDatabase` / `WaitingForCache`), so
the reason vocabulary is unchanged by the dedicated opt-in.

`ensureMariaDB` / `ensureMemcached` take the **declared instance** rather than
reading `spec.infrastructure` directly, which is what makes a dedicated instance
carry the shared block's lifecycle rather than a parallel one of its own: it is
created with a controller owner reference (so it is garbage-collected with the
ControlPlane), sized from **its** `replicas` / `storageSize`, re-projected on
drift while owned, and **adopted read-only** — never reshaped, never GC-claimed —
when a CR under that name already exists.

`ensureRabbitMQ` is the twin of `ensureMemcached`: read-modify-write on an
`*unstructured.Unstructured` carrying `rabbitmqClusterGVK`. On
`NotFound` it creates the CR with `spec.replicas` and a controller owner
reference (`claimChildOwnership`); on a CR it already owns it re-projects
`spec.replicas` and nothing else; a CR of that name owned by someone else is
adopted read-only. The re-projection is asymmetric: growing an owned cluster is
an in-place `Update`, while **shrinking** it is a delete-and-recreate — the
RabbitMQ Cluster Operator refuses an in-place scale-down, so a lowered count
written onto the CR would sit there ignored while `AllReplicasReady` (and with it
`InfrastructureReady`) stayed `True`. The delete reports the bus not ready, so
the next pass takes the `NotFound` branch and creates the cluster at the declared
size. It is destructive by construction — the broker and its volumes come back
empty, and with them every durable queue and unacked message — so it is **gated
on an annotation**: without `c5c3.io/allow-messaging-recreate: "true"` on the
ControlPlane the shrink is refused, the broker keeps running at its current size,
and the divergence surfaces as `InfrastructureReady=False` / `RabbitMQError` with
a message naming the annotation. The gate exists because `replicas` carries a
schema default of `3`: a commit that merely drops the line off a ControlPlane
running five pods reads as a no-op in review and arrives at the reconciler as a
scale-down nobody typed. With the annotation set, the recreate is the only path
the operator offers for a declared count the running cluster exceeds.

On ControlPlane deletion the bus is not left to the owner-reference cascade:
the finalizer deletes it with foreground propagation and waits for it to go, for
the cluster-operator race described under
[Owner-ref / GC model](#owner-ref--gc-model).

A child that is mid-teardown is never reported ready. The RabbitMQ Cluster
Operator holds a finalizer on its CRs, so a deleted `RabbitmqCluster` lingers in
`Terminating` with its spec and its `status.conditions` intact — `AllReplicasReady`
still reads `True` on a broker whose StatefulSet and Secrets are going away. A
non-nil `metadata.deletionTimestamp` therefore short-circuits the pass to
not-ready before the replica comparison, which would otherwise fall through to
the condition read whenever the declared count happens to match the dying
cluster's.

Image, resources, persistence, and `spec.tls` stay at the
RabbitMQ Cluster Operator's defaults, or at whatever the platform set on the
adopted CR, the posture `ensureMariaDB` takes on TLS and issuer refs. A bypassed
`replicas: 0` is floored to `infraRabbitMQReplicasDefault` (3) so the broker
never comes up with no pods.

Its readiness reads `AllReplicasReady` through
`unstructuredConditionTrue(u, "AllReplicasReady")`. The RabbitMQ Cluster Operator
sets no `Ready` condition at all, so the `Ready` specialisation would never see a
healthy broker. A cluster that does not serve the `rabbitmq.com` CRD answers the
`Get` with a `meta.NoKindMatchError`, which `apierrors.IsNotFound` does not
match, so a ControlPlane that declares managed messaging there fails closed with
`InfrastructureReady=False` / `RabbitMQError` instead of quietly provisioning
nothing.

`spec.infrastructure` is optional: an **External**-mode Keystone ControlPlane
omits it (the validating webhook forbids it in External mode and requires it
otherwise). In External mode the sub-reconciler provisions nothing and reports
`InfrastructureReady=True` / `ExternallyManaged` immediately. A **non**-External
ControlPlane that nevertheless reaches this point with the block unset is a
webhook-bypass shape (direct etcd write, admission misconfigured); it fails
closed with `InfrastructureNotConfigured` rather than dereferencing the nil
pointer.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| External keystone mode | True | `ExternallyManaged` | no MariaDB/Memcached is provisioned; the message names `external.authURL` |
| MariaDB create/update fails | False | `MariaDBError` | returns the error (controller-runtime backoff) |
| Memcached create/update fails | False | `MemcachedError` | returns the error |
| MariaDB not yet Ready | False | `WaitingForDatabase` | requeue 15s |
| Memcached not yet Ready | False | `WaitingForCache` | requeue 15s |
| RabbitmqCluster create/update fails | False | `RabbitMQError` | returns the error; a cluster that does not serve `rabbitmqclusters.rabbitmq.com` lands here with the `RESTMapper`'s `no matches for kind` |
| Declared `messaging.replicas` below the owned cluster's, `c5c3.io/allow-messaging-recreate` unset | False | `RabbitMQError` | returns the error; the destructive delete-and-recreate is refused and the broker keeps running at its current size |
| RabbitmqCluster not yet `AllReplicasReady` | False | `WaitingForMessaging` | requeue 15s |
| `spec.infrastructure` unset, not External | False | `InfrastructureNotConfigured` | requeue 15s; unreachable on the admission path — fails closed for a webhook-bypassed CR |
| A service placed an instance on a target cluster that does not resolve | False | `TargetClusterUnavailable` | requeue 15s; the resolver's own message, `cluster not found` for a name that was never registered. Nothing is provisioned, on either cluster |
| All managed children Ready (or pure brownfield) | True | `InfrastructureReady` | — |

> The managed MariaDB child is provisioned with a minimal-but-valid spec —
> `replicas: 3`, `galera.enabled: true`, `storage.size: 100Gi`
> (`infraMariaDBReplicas` / `infraMariaDBStorageSize`) — mirroring the production
> baseline; the mariadb-operator webhook rejects a CR without a storage size.
> Both values come from the **declared instance**, so a dedicated database can be
> sized (and, with `replicas: 1`, taken off Galera) independently of the shared
> cluster. The Memcached child's `spec.replicas` is likewise taken from the
> declared instance's `cache.replicas` (widened to `int64` for unstructured
> nested-field storage). MariaDB readiness is read via
> `conditions.IsReady(mariadb.Status.Conditions)`; Memcached readiness is read
> from the unstructured `status.conditions[type=Ready].status == "True"`
> (`unstructuredReady`), where a missing/malformed list is treated as not-ready
> rather than an error. `unstructuredReady` is the `Ready` specialisation of
> `unstructuredConditionTrue`, which takes the condition type as an argument;
> the RabbitmqCluster child is read with `AllReplicasReady` through the same
> helper, and its `spec.replicas` comes from `messaging.replicas`.

### reconcileESOTenantStore

| Aspect | Value |
| --- | --- |
| File | `reconcile_esotenant.go` |
| Condition | `ESOTenantStoreReady` |
| Gate | none — runs after Infrastructure and before every store-consuming sub-reconciler so the per-tenant store exists before they gate on it |
| Projects / Owns | when `spec.secretStoreRef` is omitted: a `ServiceAccount` (`eso-tenant-auth`), a cert-manager mTLS `Certificate` (`eso-tenant-client-tls`), and a namespaced `SecretStore` (`openbao-tenant-store`), one trio per namespace the ControlPlane occupies, each on the cluster that namespace lives on; when `spec.secretStoreRef` is set the sub-reconciler provisions nothing |
| Requeue | `esoTenantStoreRequeueAfter` = **10s** while the `SecretStore` is not yet Ready |

`reconcileESOTenantStore` provisions the in-cluster half of the ControlPlane's
per-tenant OpenBao identity and makes it the **enforced default**: every
ControlPlane that omits `spec.secretStoreRef` routes its (and its children's)
secret traffic through the per-tenant `openbao-tenant-store`, so OpenBao's
templated `eso-tenant` policy — not a naming convention — isolates one control
plane's key material from another. The OpenBao server and Kubernetes-auth mount
are read from the **shared** cluster store (the per-tenant store cannot describe
its own bootstrap). An explicit `spec.secretStoreRef` is an override: the
sub-reconciler provisions nothing and reports `ESOTenantStoreReady=True` with
reason `StoreRefOverridden`, and the store-consuming sub-reconcilers gate on the
selected store's own readiness.

An ESO store and the Secrets it materialises are namespace-local and
cluster-local, so each namespace the ControlPlane occupies gets its own trio,
written to the cluster that namespace lives on and read back from there for the
readiness gate. The condition is True only once every store is Ready, and it
names the namespace of the first one that is not.

| Scenario | Status | Reason |
| --- | --- | --- |
| `spec.secretStoreRef` set (override) | True | `StoreRefOverridden` |
| every per-tenant `SecretStore` Ready | True | `ESOTenantStoreReady` |
| a per-tenant `SecretStore` not yet Ready | False | `SecretStoreNotReady` |
| a namespace's target cluster does not resolve | False | `TargetClusterUnavailable` |
| provisioning the objects failed | False | `ProvisioningError` |

### reconcileDBCredentials

| Aspect | Value |
| --- | --- |
| File | `reconcile_dbcredentials.go` |
| Condition | `DBCredentialsReady` |
| Gate | none — runs unconditionally, positioned after Infrastructure and before Keystone so the Keystone CR is never projected before the DB-credential Secret exists |
| Projects / Owns | Managed-mode (`spec.infrastructure.database.clusterRef != nil`), effective **Dynamic** unless `credentialsMode: Static`: a `VaultDynamicSecret` generator, `ServiceAccount` (`keystone-db-creds`), mTLS client `Certificate`, and an `ExternalSecret` named `{controlplane.Name}-keystone-db-credentials` (`dbCredentialSecretName`) drawing from the generator, all in `cp.KeystoneNamespace()` and on the cluster that namespace lives on; brownfield projects nothing |
| Requeue | `dbCredentialsRequeueAfter` = **10s** while the ExternalSecret is not yet Ready |

`reconcileDBCredentials` projects the per-ControlPlane service database
credential so the projected Keystone CR consumes a DB credential scoped to its
own ControlPlane. It mirrors `reconcileAdminCredential`'s wait/condition
handling.

Every decision below is made on the **effective** database
(`effectiveKeystoneDatabase`) — the Keystone service's
[dedicated](./controlplane-crd.md#dedicatedbackingservices) database when it
opted into one, the shared `spec.infrastructure.database` otherwise — so the
credential follows the instance the service actually connects to. A **dedicated**
managed database always takes the **Static** branch: the OpenBao database engine
carries one connection and one role per *namespace*, bootstrapped against the
shared cluster, so no engine role can issue credentials for a dedicated instance.
The validating webhook rejects an explicit `credentialsMode: Dynamic` there, and
keying the reconciler's decision on the dedicated *declaration* (not only on the
stored mode) makes a webhook-bypassed CR fail closed onto Static rather than
project a generator that could never sync.

The database is **managed** when the effective `clusterRef` is set and
**brownfield** when the user supplies a `host`-based connection:

- **Brownfield is a pure no-op.** When `clusterRef == nil` the user owns the DB
  credential Secret out-of-band, so the operator projects **no** ExternalSecret
  and never references OpenBao or the selected secret store; `DBCredentialsReady`
  is reported `True` immediately so the chain proceeds to Keystone.
- **Managed defaults to Dynamic (engine-issued).** After gating (via
  `secrets.IsStoreRefReady`) on the store the ControlPlane selected through
  `spec.secretStoreRef` — a `ClusterSecretStore` (default `openbao-cluster-store`)
  or a namespaced `SecretStore` resolved in `childNamespace(cp)` — the operator
  projects (all owner-referenced): a `keystone-db-creds` `ServiceAccount`, an mTLS client
  `Certificate` from the cluster-scoped `openbao-ca-issuer`, a
  `generators.external-secrets.io/v1alpha1` `VaultDynamicSecret` reading
  `database/mariadb/creds/keystone-{cp.Namespace}`
  (`dbDynamicCredsPathFor`, keyed on the namespace alone), and an `ExternalSecret`
  (`RefreshInterval` 24h, `Target.CreationPolicy: Owner`) drawing from that generator via
  `dataFrom.sourceRef.generatorRef` — **no** static `Data` refs and **no**
  `SecretStoreRef`. The generator's OpenBao server URL and Kubernetes-auth mount
  are copied from the selected store's Vault provider by `openBaoConnection`
  (falling back to the documented defaults when unreadable), so the generator
  cannot drift from the store the rest of the stack uses. All Secret references
  are same-namespace (the generator is Namespaced), satisfying the OpenBao
  listener's require-and-verify-client-cert gate. The materialised Secret carries
  an engine-issued username and password with a finite lease, so no long-lived
  static DB password remains at rest.
- **Managed Static is the opt-out** — and the only mode a **dedicated** managed
  database has. The operator projects the stage-(a) KV-backed `ExternalSecret`
  (`SecretStoreRef` the selected store — default `openbao-cluster-store`, built via
  `secrets.ESOSecretStoreRef` — with `username`/`password` `Data` reading
  `openstack/keystone/{cp.Namespace}/{cp.Name}/db`) and tears down any leftover
  dynamic-mode objects.

  > **That KV path is seeded by neither the operator nor the bootstrap.** The
  > per-ControlPlane static seed was retired when managed mode moved to
  > engine-issued credentials, so a Static ControlPlane — the explicit opt-out on
  > the shared database, and *every dedicated managed database* — reaches Ready
  > only once the path has been seeded (`username`, `password`) out-of-band; see
  > [Migrate the Keystone DB to dynamic credentials](../../guides/keystone/migrate-keystone-db-to-dynamic-credentials.md).
  > Until then `DBCredentialsReady` stays `False` with reason
  > `WaitingForDBCredentialSecret`, and the message names the exact path to seed.

`reconcileKeystone` projects the effective mode onto the Keystone CR's
`spec.database.credentialsMode`, so the Keystone operator consumes the matching
credential shape.

The credential material follows the Keystone service onto its
[target cluster](../target-clusters.md), because the ESO that runs the generator
and the cert-manager that issues its client certificate are the ones on that
cluster, and the Secret they produce has to be mountable by the Keystone pods.
The gates in front of it (the store's readiness and the ExternalSecret's) are
read from the same cluster. `ensureServiceDBCredential`, the shared helper behind
the Glance, Placement, and Barbican credentials, does the same for their
namespaces and reports an unresolvable cluster on the calling service's own
condition rather than on `DBCredentialsReady`.

In **External** keystone mode the ControlPlane manages no database at all —
neither a managed one to issue credentials for, nor a brownfield connection to
reference — so neither OpenBao nor any secret store is consulted.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| External keystone mode | True | `ExternallyManaged` | no database is managed; nothing is projected and no secret store is read |
| Brownfield (`clusterRef == nil`) | True | `BrownfieldUserSuppliedCredential` | no ExternalSecret projected; user supplies the DB credential Secret out-of-band |
| Keystone placed on a target cluster that does not resolve | False | `TargetClusterUnavailable` | requeue 10s; resolved before the store gate, so nothing is projected and no store is read. The resolver's own message |
| Selected secret store not Ready | False | `SecretStoreNotReady` | requeue 10s; managed mode only, checked before projection so an OpenBao/ESO outage surfaces promptly. The message names the store's kind and name |
| Dynamic generator/SA/Certificate/ExternalSecret create/update fails | False | `GeneratorError` | returns the error |
| Static ExternalSecret create/update fails | False | `ExternalSecretError` | returns the error |
| ExternalSecret create/update or read fails | False | `ExternalSecretError` | returns the error |
| ExternalSecret not yet synced | False | `WaitingForDBCredentialSecret` | requeue 10s |
| ExternalSecret Ready | True | `DBCredentialsReady` | — |

### reconcileAdminPassword

| Aspect | Value |
| --- | --- |
| File | `reconcile_adminpassword.go` |
| Condition | `AdminPasswordReady` |
| Gate | none — runs unconditionally, positioned after DBCredentials and before Keystone so the Keystone CR is never projected before the admin-password Secret exists |
| Projects / Owns | Managed-mode (`spec.infrastructure.database.clusterRef != nil`) one `external-secrets.io/v1` `ExternalSecret` named `{controlplane.Name}-keystone-admin-credentials` (`adminPasswordSecretName`) in `cp.KeystoneNamespace()`, on the cluster that namespace lives on; brownfield projects nothing |
| Requeue | `adminPasswordRequeueAfter` = **10s** while the ExternalSecret is not yet Ready |

`reconcileAdminPassword` projects the per-ControlPlane Keystone admin password
as an OpenBao-backed `ExternalSecret`, so the projected Keystone CR's bootstrap
admin-password ref consumes a credential scoped to its own ControlPlane. It
mirrors `reconcileDBCredentials`'s wait/condition handling. The database is
**managed** when `spec.infrastructure.database.clusterRef` is set and
**brownfield** when the user supplies a `host`-based connection:

- **Brownfield is a pure no-op.** When `clusterRef == nil` the user owns the
  admin-password Secret out-of-band, so the operator projects **no**
  ExternalSecret and never references OpenBao or the selected secret store;
  `AdminPasswordReady` is reported `True` immediately so the chain proceeds to
  Keystone.
- **Managed projects the ExternalSecret.** The owned ExternalSecret has
  `RefreshInterval` 1h, its `SecretStoreRef` built from the ControlPlane's
  `spec.secretStoreRef` via `secrets.ESOSecretStoreRef` (default
  `Kind: ClusterSecretStore, Name: openbao-cluster-store`; a namespaced
  `SecretStore` when selected),
  and `Target.CreationPolicy: Owner` (so ESO owns the materialised Secret of the
  same name). Its single `password` `Data` key reads from the per-CP remote key
  `bootstrap/{cp.Namespace}/{cp.Name}-keystone/admin`
  (`adminPasswordRemoteKeyFor`) with `Property: password`. Unlike the
  DB-credential path this key is **Keystone-name-scoped**
  (`{cp.Name}-keystone`, not `{cp.Name}`) so it matches the bootstrap seeder and
  the keystone-operator's Model-B rotation `PushSecret` at
  `bootstrap/{keystone.Namespace}/{keystone.Name}/admin`; the
  `{namespace}/{keystone-name}` scoping still keeps two ControlPlanes from
  colliding on the cluster-global OpenBao backend. The builder
  `adminPasswordExternalSecret(cp)` sets **no** owner reference; the reconciler
  applies the ExternalSecret via Server-Side Apply under the shared field manager
  (`cobaltcore-operator`), which stamps the ControlPlane controller reference for GC.
  In a dedicated Keystone namespace, and on a
  [target cluster](../target-clusters.md), no owner reference is possible: the
  ExternalSecret carries the ControlPlane's ownership labels instead and the
  finalizer-driven teardown deletes it. A placed Keystone takes the ExternalSecret
  with it, since only the ESO on that cluster can sync it into a Secret the
  Keystone pods can mount.

The managed-mode effective admin-password ref (`effectiveAdminPasswordSecretRef`)
points the projected Keystone child's `spec.bootstrap.adminPasswordSecretRef` at
this materialised Secret's `password` key
(`{controlplane.Name}-keystone-admin-credentials`); in brownfield mode it stays
the user-declared `spec.korc.adminCredential.passwordSecretRef` verbatim. The
cp-level spec default for `passwordSecretRef` remains `keystone-admin`.

`effectiveAdminPasswordSecretRef` keys its External branch on
`spec.services.keystone.mode`, not on the database shape, and returns
`spec.korc.adminCredential.passwordSecretRef` verbatim. That Secret is the admin
password source: the operator only ever **reads** it, because the external
Keystone's admin password is owned out-of-band. Its SHA-256 feeds
`adminPasswordHashAnnotation`, so **updating that Secret is what drives a
hash-driven re-mint** of the admin application credential — the only supported
rotation path in External mode. The field indexer
(`controlPlaneSecretNameExtractor`) follows the same ref, so an edit to the
user's Secret wakes the ControlPlane immediately.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| External keystone mode | True | `ExternallyManaged` | the admin password is read from the user-supplied `passwordSecretRef` Secret; no ExternalSecret is projected and no OpenBao bootstrap path is seeded |
| Brownfield (`clusterRef == nil`) | True | `BrownfieldUserSuppliedCredential` | no ExternalSecret projected; user supplies the admin-password Secret out-of-band |
| Keystone placed on a target cluster that does not resolve | False | `TargetClusterUnavailable` | requeue 10s; resolved before the store gate, so no ExternalSecret is projected and no store is read. The resolver's own message |
| Selected secret store not Ready | False | `SecretStoreNotReady` | requeue 10s; managed mode only, checked before the ExternalSecret is projected. The message names the store's kind and name |
| ExternalSecret create/update or read fails | False | `ExternalSecretError` | returns the error |
| ExternalSecret not yet synced | False | `WaitingForAdminPasswordSecret` | requeue 10s |
| ExternalSecret Ready | True | `AdminPasswordReady` | — |

### Identity-backend watch

`KeystoneIdentityBackend` CRs are authored by the operator, not projected by the
ControlPlane, so they carry no ControlPlane owner reference an `Owns()` could
match. `SetupWithManager` therefore registers a
`Watches(&KeystoneIdentityBackend{}, ...)` bound to
`identityBackendToControlPlaneMapper`, which enqueues the ControlPlane whose
`keystoneName(cp)` equals the backend's `keystoneRef.name`. A backend attached
to a hand-rolled Keystone beside a ControlPlane therefore never wakes it.

`listIdentityBackends` resolves a ControlPlane's backends with a cache-backed
`List` in `childNamespace(cp)` and the same `keystoneRef.name` filter applied in
memory — the set holds one backend per identity provider plus one per LDAP
domain. Backends carrying a `deletionTimestamp` are dropped: a backend's own
`reconcileDelete` never demotes `Ready` while it waits for de-projection, so a
Terminating backend would otherwise keep offering an SSO choice whose
Keystone-side federation objects are being torn down — and would collide with the
same-named replacement its webhook admits during teardown.

RBAC is **read-only** (`get;list;watch`) in both the kubebuilder marker and the
shared Helm rules helper: the reconciler never writes a backend.

Attaching, detaching, or a backend reaching `Ready` re-projects the Horizon
websso choices and the Keystone `trusted_dashboard` immediately, without waiting
for a periodic resync.

### reconcileKeystone

| Aspect | Value |
| --- | --- |
| File | `reconcile_keystone.go` |
| Condition | `KeystoneReady` |
| Gate | `InfrastructureReady == True` |
| Projects / Owns | one `Keystone` child named `{controlplane.Name}-keystone` (`keystoneNameSuffix`) in `childNamespace(cp)` |
| Requeue | `keystoneInfraGateRequeueAfter` = **5s** while gated; `infraRequeueAfter` = **15s** while the child is not Ready |

`reconcileKeystone` projects `spec.services.keystone` into an owned `Keystone`
CR. The projection is deliberately *thin* — it reuses the ControlPlane's own
infrastructure specs verbatim so Keystone points at the same backing services
the ControlPlane provisioned:

- **Image:** repository defaults to `ghcr.io/c5c3/keystone` with the tag derived
  from `spec.openStackRelease`; `spec.services.keystone.image` overrides the
  whole image reference when set.
- **Database / Cache:** `keystone.Spec.Database` and `keystone.Spec.Cache` are
  DeepCopies of the **effective** instances — the service's
  [dedicated](./controlplane-crd.md#dedicatedbackingservices) database/cache when
  it opted into one (`effectiveKeystoneDatabase` / `effectiveKeystoneCache`), the
  shared `spec.infrastructure` instance otherwise (the default). Projecting the
  effective spec is what carries the opt-in through the rest of the chain with no
  per-class special-casing: the keystone-operator derives its logical database,
  its MariaDB `User`/`Grant` CRs, and its NetworkPolicy database/cache egress
  rules from `spec.database` / `spec.cache`, so all of them follow the instance
  the service actually talks to.
- **Bootstrap:** the admin-password Secret ref is the effective ref
  (`effectiveAdminPasswordSecretRef`) — in managed mode the operator-projected
  per-CP Secret `{controlplane.Name}-keystone-admin-credentials` (see
  [reconcileAdminPassword](#reconcileadminpassword)), in brownfield mode the
  user-declared `cp.Spec.KORC.AdminCredential.PasswordSecretRef` verbatim (so
  Keystone and K-ORC agree on the admin-password source) — and the region is
  `cp.Spec.Region`.
- **Replicas:** copied from `spec.services.keystone.replicas` when set.
- **Federation:** `spec.federation.proxyImage` is the
  `spec.services.keystone.federationProxyImage` override when set, else
  `ghcr.io/c5c3/keystone-federation-proxy:latest`;
  `spec.federation.trustedDashboards` is the ControlPlane's own dashboard origin
  (`cp.Spec.Services.Horizon.DerivedPublicEndpoint() + "/auth/websso/"`), or `nil` when no dashboard is
  externally reachable. Both are assigned unconditionally, so clearing the
  override or the horizon block reverts the child. The origin is derived
  **top-down from `cp.Spec`**, never from the Horizon child's status, so this
  projection carries no ordering dependency on `reconcileHorizon` (which is
  gated on `KeystoneReady` and therefore runs strictly after it). Both fields
  are inert until a federation backend attaches.
- **Policy:** `policy.MergePolicies(cp.Spec.GlobalPolicyOverrides, cp.Spec.Services.Keystone.PolicyOverrides)`
  (the shared `internal/common/policy` helper) merges the global base with
  per-service overrides (per-service wins on conflict).
- **Rotation:** when `spec.services.keystone.rotationInterval` is set,
  `intervalToCron` converts it to a cron schedule applied to **both**
  `Fernet.RotationSchedule` and `CredentialKeys.RotationSchedule`. Only `168h`
  (weekly, `0 0 * * 0`) and positive whole-day multiples (daily, `0 0 * * *`)
  are supported.
- **Target cluster:** `spec.targetClusterRef` is a DeepCopy of
  `services.keystone.targetClusterRef`, and a nil source projects no field at
  all. The Keystone CR itself is applied through the **local** client whichever
  cluster it names, so that one field is the whole hand-over: the
  keystone-operator reads the CR on the management cluster and owns everything
  it creates on the target. The same holds for the Horizon, Glance, Placement,
  and Barbican children. The value is frozen on both sides — the ControlPlane
  webhook rejects an edit, and each child CRD carries its own CEL transition
  rule — so re-projecting it never trips the child's freeze.

In **External** keystone mode no child is projected — and none is deleted. A
`Managed -> External` flip is rejected at admission (adopting an existing
installation must be a fresh External-mode ControlPlane), so no child can exist;
were one to appear anyway, the fail-safe that preserves a child unless
`c5c3.io/allow-keystone-deletion: "true"` opts in is the only sanctioned teardown
path, because the child's credential/fernet keys are irreplaceable.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.services.keystone` unset | True | `KeystoneNotManaged` | no identity plane at all; a previously-projected child is preserved by default |
| External keystone mode | True | `ExternallyManaged` | identity is managed against `external.authURL`; no child is projected and none is deleted |
| `InfrastructureReady` not True | False | `WaitingForInfrastructure` | requeue 5s; no Keystone CR is created while infra is unready |
| Invalid `rotationInterval` | False | `InvalidRotationInterval` | **returns the error** so the reconcile chain stops at Keystone and the manager requeues with backoff (the validating webhook already rejects unrepresentable intervals at admission, so this is defense-in-depth) |
| Keystone create/update fails | False | `KeystoneError` | returns the error |
| Keystone child not yet Ready | False | `WaitingForKeystone` | requeue 15s |
| Keystone child Ready | True | `KeystoneReady` | — |

### reconcileHorizon

| Aspect | Value |
| --- | --- |
| File | `reconcile_horizon.go` |
| Condition | `HorizonReady` |
| Gate | `KeystoneReady == True` |
| Projects / Owns | one `Horizon` child named `{controlplane.Name}-horizon` (`horizonNameSuffix`) in `childNamespace(cp)` — only when `spec.services.horizon` is set |
| Requeue | `keystoneInfraGateRequeueAfter` = **5s** while gated; `infraRequeueAfter` = **15s** while the child is not Ready |

`reconcileHorizon` is optional: `spec.services.horizon` unset means this
ControlPlane manages no dashboard, and the sub-reconciler reports
`HorizonReady=True` / `HorizonNotManaged` so the aggregate is not blocked (staged
adoption). A previously-projected child is **preserved** unless the ControlPlane
opts in with `c5c3.io/allow-horizon-deletion: "true"` (then the orphan is
deleted) — the same annotation UX as Keystone, though the dashboard is stateless.

When managed, the projection mirrors the Keystone one's *thin* discipline,
reusing the ControlPlane's own specs so the dashboard points at the same backing
services:

- **Image:** repository defaults to `ghcr.io/c5c3/horizon` with the tag derived
  from `spec.openStackRelease`; `spec.services.horizon.image` overrides the whole
  image reference when set.
- **Cache:** a DeepCopy of the **effective** cache (`effectiveHorizonCache`) —
  the dashboard's [dedicated](./controlplane-crd.md#dedicatedbackingservices)
  cache when it opted into one, the shared `spec.infrastructure.cache` otherwise
  — with the same `clusterRef` / servers / replicas, **except** that `Backend` is
  overridden to the Horizon Django default
  `django.core.cache.backends.memcached.PyMemcacheCache`. The shared
  `CacheSpec.Backend` carries the oslo.cache dogpile path Keystone consumes
  (`dogpile.cache.pymemcache`), which Django renders verbatim as a `CACHES`
  backend and rejects with `InvalidCacheBackendError`, so the dashboard would
  never go Ready — only the endpoint-bearing fields are reused unchanged.
- **Keystone endpoint:** derived top-down via `horizonKeystoneEndpoint(cp)` from
  the Keystone child's naming convention, not read from the Keystone child's
  status (no machine consumer reads status endpoints, per the settled
  convention). The cluster-local Service URL (the same URL K-ORC authenticates
  against) as long as the dashboard and Keystone resolve to the **same** cluster
  — never the external `publicEndpoint` or gateway hostname, which the dashboard
  pods may not be able to reach: the dashboard's Django backend connects to this
  URL server-side, the browser never does. A dashboard on another cluster than
  Keystone cannot resolve that Service DNS name at all and gets
  `keystonePublicEndpoint` instead (see
  [Reaching a placed service](#reaching-a-placed-service)).
- **SecretKeyRef:** defaults to the kind shim Secret `horizon-secret-key` (key
  `secret-key`), which is pinned to the **default** ControlPlane identity;
  `spec.services.horizon.secretKeyRef` overrides it, and a second ControlPlane
  **must** set its own so each dashboard reads distinct `SECRET_KEY` material.
- **Gateway:** a DeepCopy of `spec.services.horizon.gateway`; a nil source clears
  the projected gateway so removing the block tears the HTTPRoute down.
- **Replicas:** `commonv1.DefaultReplicas`, overridden by
  `spec.services.horizon.replicas` when set (assigned unconditionally so clearing
  the field reverts the child to the default instead of pinning a lost update).
- **WebSSO:** projected from the **Ready** OIDC `KeystoneIdentityBackend` CRs
  attached to the Keystone child (see
  [Identity-backend watch](#identity-backend-watch)). One choice per Ready
  backend, keyed `{identityProvider}_{protocol}` (truncated to a digest-suffixed
  64 characters when the two names together exceed the Horizon CRD's bound on
  `choices[].id`), with the local-credentials fallback leading the list and
  preselected; `keystoneURL` is `keystonePublicEndpoint(cp)` — the
  **browser-facing** endpoint, because the browser follows the SSO redirect.
  At most 16 federated choices are projected (`maxProjectedFederationChoices`);
  the excess is dropped and logged rather than rejected by the API server as a
  `choices`/`idpMapping` overflow that would wedge every later Horizon change.
  `nil` when no OIDC backend is **attached**, so a choice never appears for a
  backend whose federation objects are not provisioned yet — and `nil` too when
  the hand-off could not complete anyway: no trusted dashboard origin
  (`trustedDashboards(cp)`) means Keystone bounces the browser *after* the user
  has entered their corporate credentials, and no `keystonePublicEndpoint(cp)`
  means the redirect targets a cluster-local DNS name the browser cannot
  resolve. Both are logged.
- **MultiDomain:** `enabled` with `defaultDomain: Default` once any LDAP backend
  is Ready, so the login form gains a domain field. `domainChoices` /
  `domainDropdown` are deliberately **not** projected: upstream `openstack_auth`
  turns the domain field into a select bounded by
  `OPENSTACK_KEYSTONE_DOMAIN_CHOICES`, and the operator only ever sees the
  LDAP-backed domains — a dropdown built from them would lock out every user of
  a domain it cannot enumerate (a SQL-backed domain populated out-of-band, or
  the domain an OIDC backend targets). `nil` when no LDAP backend is attached.
- **Detached vs. unhealthy.** Detaching the last backend of a type clears its
  block, so the login page reverts to local credentials. A backend that is
  attached but **not Ready** retains the previously-projected block instead
  (`projectWebSSO` / `projectMultiDomain`): a backend's aggregate `Ready` can
  drop on a failed observation while the Keystone-side federation objects it
  provisioned are untouched, so the SSO button keeps working. Rebuilding the
  block from that view would re-render `local_settings.py`, roll the dashboard
  Deployment, and roll it back on recovery — twice, for a login page that was
  never broken. The retention is logged.

A failure to list the backends surfaces as `HorizonReady=False` with reason
`IdentityBackendsUnavailable` and returns the error — joined into the tail
group's error, with controller-runtime's backoff driving the retry — never
an empty websso block, which would silently remove a working SSO button.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.services.horizon` unset | True | `HorizonNotManaged` | staged adoption — does not block the aggregate; a previously-projected child is preserved unless `c5c3.io/allow-horizon-deletion: "true"` is set |
| `KeystoneReady` not True | False | `WaitingForKeystone` | requeue 5s; no Horizon CR is projected while Keystone is unready |
| Horizon create/update fails | False | `HorizonError` | returns the error |
| Projected spec rejected (HTTP 422 Invalid) | False | `HorizonProjectionRejected` | returns the error; the projection violates a Horizon CRD/webhook rule — reconcile the ControlPlane spec to a valid projection to recover |
| Horizon child not yet Ready | False | `WaitingForHorizon` | requeue 15s |
| Horizon child Ready | True | `HorizonReady` | — |

### Built-in service registrations

Glance, Placement, Barbican and Neutron each need a Keystone catalog row and a
Keystone service user. None of the four registers either itself: every one of them
projects a [`KeystoneService`](./keystoneservice-crd.md) child and lets that CR's
controller do the work. `builtin_registrations.go` is the leg all four share, so
it is described once here rather than four times below, and a fifth built-in
service adds the values it registers with rather than another copy of the leg.

| Aspect | Value |
| --- | --- |
| File | `builtin_registrations.go` |
| Conditions | `GlanceReady`, `PlacementReady`, `BarbicanReady`, `NeutronReady` — the leg writes the caller's condition, never one of its own |
| Projects / Owns | one `KeystoneService` named `{controlplane.Name}-{service}` in the namespace that service is placed in, applied through the **local** client whatever cluster the service runs on |
| Requeue | `korcRequeueAfter` = **10s** while the registration is not usable yet |

**What the child asserts.** `spec.controlPlaneRef` names the ControlPlane's
namespace explicitly rather than relying on the default, so the child resolves the
same plane wherever it is placed. `spec.catalog` carries the service's type and
name with **two** endpoints, `internal` and `public`, from the start, so no later
catalog migration is needed. `spec.account` carries the service's user with a
project of its own and `create: true`, plus the single role `service`. Each
service creates its own project (`service-placement`, `service-barbican`,
`service-neutron`), because two registrations creating one project would each
adopt the other's Keystone row.

**The URLs each row advertises.** The `internal` endpoint advertises the
in-cluster API Service URL — `http://{controlplane.Name}-glance.<glance-namespace>.svc:9292`
(`glanceEndpointURL`), `…-placement.<placement-namespace>.svc:8778`
(`placementEndpointURL`), `…-barbican.<barbican-namespace>.svc:9311`
(`barbicanEndpointURL`), `…-neutron.<neutron-namespace>.svc:9696`
(`neutronEndpointURL`). None of the four carries a `/v3` path suffix; unlike
identity, these APIs are served at the root. The `public` endpoint resolves
through one preference order (`glanceCatalogURL` and its siblings): an explicit
`services.<svc>.publicEndpoint`, advertised verbatim, then the externally routable
gateway hostname `https://{gateway.hostname}`, then the same in-cluster URL when
the service is not exposed through a Gateway.

A service carrying a [`targetClusterRef`](../target-clusters.md) advertises its
public URL on the `internal` interface too (`internalCatalogURL`): the in-cluster
Service URL resolves only on the cluster that service runs on, so K-ORC — and
every other consumer reading the catalog from elsewhere — would otherwise get an
address it cannot connect to. Admission requires a placed catalog service to carry
a `publicEndpoint` or a gateway, so that URL is never empty. The K-ORC children the
registration projects stay on the management cluster in the ControlPlane's
namespace whatever the rows advertise, under the naming convention the
[KeystoneService Reconciler](./keystoneservice-reconciler.md) documents.

**The gate is two-staged.** The service child is not projected until the
registration reports `AccountReady` (`builtinRegistrationAccountReady`), because a
service pointed at a Keystone user that does not exist, with a password its
consumer Secret does not carry, cannot start. A previously projected child keeps
running on the credentials it already has. The service's own condition then stays
`False` until the registration reports its aggregate `Ready`
(`foldBuiltinRegistrationReady`), which covers the catalog row: a running service
whose catalog entry never landed is reachable by nothing that discovers it through
the catalog.

**The relayed reason is the child's first failing sub-condition**, walked in
`keystoneServiceSubConditionTypes` order, not the aggregate's. `NotAllReady` says
only that something is wrong, while `ServiceCollision` on the registration's
`CatalogReady` says what and admits a fix — so a catalog-side reason can and does
surface on `GlanceReady`. The message names which child it came from.

**Foreign spec fields are reclaimed, not tolerated.** A field manager other than
this operator can write assertions the projecting apply cannot take back:
`spec.catalog.adopt`, `spec.account.adopt`, `spec.account.rotation`, and endpoint
interfaces the ControlPlane never declared. Leaving them standing is not an
option, because an undeclared endpoint row is **published** in the Keystone catalog
by the registration's own controller, and every admin-scoped client that resolves
it sends its token wherever the row points. An ordinary Update reclaims them, the
pass halts, and the next one picks the reclaimed child up. Both a Warning event and
the `ServiceRegistrationFieldsReclaimed` condition record it: an event ages out of
etcd on the cluster's TTL, and without the condition a tampering remediated at
02:00 would leave the ControlPlane reporting `Ready=True` with no durable trace.

**Credentials follow a placed service.** For a service on a target cluster the leg
mirrors the registration's consumer credentials there
(`ensureBuiltinRegistrationMirror`): it resolves that cluster, gates on the store
being ready **on that cluster**, and writes an `ExternalSecret` drawing the same
OpenBao path. A co-located service is a no-op. For the credentials' shape and the
aggregate condition over all four registrations, see
[reconcileServiceAccounts](#reconcileserviceaccounts).

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| projecting, reading, reclaiming or mirroring the child fails | False | `ServiceRegistrationError` | returns the error. The refusal to adopt a same-named CR this ControlPlane did not create is relayed verbatim, since it spells out what it refuses and why |
| the child carries spec fields this ControlPlane never projected | False | `ServiceRegistrationFieldsReclaimed` | the fields are reset first, so neither the event nor the condition claims a reset that did not land; requeue 10s |
| the target cluster of a placed service does not resolve | False | `TargetClusterUnavailable` | the resolver's own message; a wait, not a failed reconcile; requeue 10s |
| the store on that target cluster is not ready | False | `SecretStoreNotReady` | an ExternalSecret written against it would never sync, and the service would wait on a Secret whose absence nothing explains; requeue 10s |
| the registration has not provisioned the account yet | False | `WaitingForServiceRegistration` | no service child is written at all; requeue 10s |
| the registration reports a `False` sub-condition | False | *(the child's own reason)* | relayed with the child's message and which child it came from; requeue 10s |
| the registration has not reported `Ready` at all yet | False | `WaitingForServiceRegistration` | a child created moments ago, before its controller reconciled it; requeue 10s |

### reconcileGlance

| Aspect | Value |
| --- | --- |
| File | `reconcile_glance.go` |
| Condition | `GlanceReady` |
| Gate | `KeystoneReady == True` (Glance validates every token against the Keystone child) **and** the `AccountReady` of the `KeystoneService` registration it projects (see [Built-in service registrations](#built-in-service-registrations)) |
| Projects / Owns | one `Glance` child named `{controlplane.Name}-glance` (`glanceNameSuffix`) in `cp.GlanceNamespace()`; one `GlanceBackend` child per `services.glance.backends` entry, named `{controlplane.Name}-glance-{entry}`; and — managed database only — the per-ControlPlane DB-credential objects in the Glance service namespace: in **Dynamic** mode (the managed-shared default) a ServiceAccount `glance-db-creds`, an mTLS client Certificate `{controlplane.Name}-glance-db-openbao-client`, a `VaultDynamicSecret` generator reading `database/mariadb/creds/glance-{glance-namespace}` (auth role `glance-db`), and a generator-backed `ExternalSecret` `{controlplane.Name}-glance-db-credentials`; in the **Static** opt-out a KV-backed `ExternalSecret` of the same name reading `openstack/glance/{glance-namespace}/{controlplane.Name}/db` (properties `username`, `password`). Only when `spec.services.glance` is set |
| Requeue | `keystoneInfraGateRequeueAfter` = **5s** while gated on Keystone; `korcRequeueAfter` = **10s** while the `glance` service account is not yet Ready; `infraRequeueAfter` = **15s** while the child is not Ready |

`reconcileGlance` runs **last** in the pipeline (after `reconcileServiceAccounts`),
because it gates on the per-account readiness that stage computes into status in
the same pass. It is optional: `spec.services.glance` unset means this ControlPlane
manages no image service, and the sub-reconciler reports `GlanceReady=True` /
`GlanceNotManaged` so the aggregate is not blocked (staged adoption). A
previously-projected child — and its `GlanceBackend` children, DB-credential
ExternalSecret, and (from a prior Dynamic deployment) the `VaultDynamicSecret`
generator, its client Certificate, and the `glance-db-creds` ServiceAccount — is
**preserved** unless the ControlPlane opts in with
`c5c3.io/allow-glance-deletion: "true"` (then the orphans, plus the image catalog
K-ORC CRs, are deleted). Cross-namespace children are ownership-checked, so a
hand-created `GlanceBackend` sharing the namespace is never touched.

When managed, the projection mirrors the Keystone/Horizon *thin* discipline,
reusing the ControlPlane's own specs so Glance points at the same backing services:

- **Image:** repository defaults to `ghcr.io/c5c3/glance` with the tag derived
  from `spec.openStackRelease`; `spec.services.glance.image` overrides the whole
  image reference when set.
- **Database:** a DeepCopy of the **effective** database (`effectiveGlanceDatabase`
  — Glance's [dedicated](./controlplane-crd.md#dedicatedbackingservices) database
  when it opted into one, the shared `spec.infrastructure.database` otherwise) with
  its logical database name forced to `glance` so Glance's schema stays isolated
  from Keystone's on a shared cluster. In managed mode (`clusterRef` set) the
  `secretRef` is repointed at the operator-owned
  `{controlplane.Name}-glance-db-credentials` Secret, and the projected
  `credentialsMode` is the **effective** mode: `Dynamic` (engine-issued) by
  default on the managed shared database — Glance now has its own engine role
  `glance-{glance-ns}`, provisioned by `setup-database-tenant.sh` — flipped to
  `Static` by the shared-block opt-out or the per-service
  `services.glance.databaseCredentialsMode` override, and always `Static` for a
  dedicated glance database (fail-closed even when the stored mode says
  otherwise). In `Dynamic` mode the generator objects above (the
  `glance-db-creds` ServiceAccount, the client Certificate, the
  `VaultDynamicSecret`, and the generator-backed ExternalSecret) are ensured
  before the child; in `Static` mode they are torn down and the KV-backed
  ExternalSecret is projected, reading a path seeded **out-of-band** (see
  [Migrate the Glance DB to dynamic credentials](../../guides/glance/migrate-glance-db-to-dynamic-credentials.md)).
  A brownfield database keeps the user-supplied `secretRef` and `credentialsMode`.
- **Cache:** a DeepCopy of the **effective** cache (`effectiveGlanceCache`).
- **Keystone endpoint:** `keystoneEndpoint` is derived top-down via
  `glanceKeystoneEndpoint(cp)` — the cluster-local `{controlplane.Name}-keystone`
  Service URL (the same URL K-ORC authenticates against) while Glance and Keystone
  resolve to the same cluster, never the external
  `publicEndpoint` or gateway hostname, because Glance validates tokens
  server-side and the pods must reach it in-cluster. Placed apart, Glance gets the
  public URL, the only one that leaves Keystone's cluster (see
  [Reaching a placed service](#reaching-a-placed-service)). `keystonePublicEndpoint` is a
  pass-through of the Keystone service's own public endpoint (empty when Keystone is
  not externally exposed, in which case the child falls back to the internal URL).
- **Service user:** derived from the `glance` account the projected
  `KeystoneService` registration provisions — its `username`, project, and
  user/project domains — with the password read from the consumer Secret that
  registration delivers, `{controlplane.Name}-glance-credentials`.
- **Backends:** each `services.glance.backends` entry projects one `GlanceBackend`
  child (the CP-side S3 `endpoint` maps to the child's `spec.s3.host`; an unset
  `bucketURLFormat` serializes away so the child's own `path` default applies), and
  previously-projected children whose entry was removed are **pruned** — matched by
  the c5c3 ownership and the `{controlplane.Name}-glance-` name prefix. A prune
  **de-registers a store whose id existing image location rows still reference**:
  the backend's name is its store id, so dropping the entry removes the
  `[<name>]` section from `backends.conf` and the `<name>:s3` entry from
  `enabled_backends`, and Glance no longer resolves that store. Images stored
  there stay listed in the catalog but `GET /v2/images/{id}/file` fails for each
  of them, so the breakage is invisible until download. Recovery is re-creating
  the entry under the *identical* name. Unlike unsetting `spec.services.glance`,
  which is gated behind `c5c3.io/allow-glance-deletion`, removing one
  `backends[]` line takes effect immediately and is not gated.
- **Gateway / Replicas / SecretStoreRef / Region:** `gateway` is a DeepCopy of
  `spec.services.glance.gateway` (a nil source clears it, tearing the HTTPRoute
  down); `replicas` defaults to `commonv1.DefaultReplicas`, overridden by
  `spec.services.glance.replicas`; the resolved store selection and `spec.region`
  are projected through. `spec.apiServer` is deliberately **not** set.

A child placed outside the ControlPlane's namespace (`services.glance.namespace`)
carries no owner reference — it is stamped with the ownership labels and applied
unowned, and the finalizer sweeps it by those labels.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.services.glance` unset | True | `GlanceNotManaged` | staged adoption; a previously-projected child is preserved unless `c5c3.io/allow-glance-deletion: "true"` is set — but its dynamic DB-credential generator, ServiceAccount, and client Certificate are torn down either way, so no credential minter outlives the service |
| `KeystoneReady` not True | False | `WaitingForKeystone` | requeue 5s; no Glance CR is projected while Keystone is unready |
| the projected registration has not provisioned the account yet | False | `WaitingForServiceRegistration` | requeue 10s; no Glance child is written until the Keystone user and its password exist. See [Built-in service registrations](#built-in-service-registrations) |
| projecting, reading or mirroring the registration child fails | False | `ServiceRegistrationError` | returns the error |
| the registration child carries foreign spec fields | False | `ServiceRegistrationFieldsReclaimed` | they are reset and the pass halts; requeue 10s |
| Glance placed on a target cluster that does not resolve | False | `TargetClusterUnavailable` | requeue 10s; the DB credential resolves its namespace's cluster before it writes anything (`ensureServiceDBCredential`). The resolver's own message |
| DB-credential ensure fails (Dynamic generator objects or Static ExternalSecret) | False | `GlanceDBCredentialError` | returns the error (managed database only) |
| Dynamic DB credential not yet materialised | False | `WaitingForGlanceDBCredential` | requeue 10s; no Glance CR is projected and an existing child keeps its current mode until the generator-backed ExternalSecret reports Ready **and** the Secret it targets carries an engine-issued username. The message names the `database/mariadb/creds/glance-<namespace>` path, which only exists after `setup-database-tenant.sh` has onboarded the tenant, or — on a Static→Dynamic migration, where the in-place ExternalSecret update leaves the previous Static sync's `Ready` in place over a stale Secret — the non-engine-issued username it found |
| projected `GlanceBackend` rejected (HTTP 422 Invalid) | False | `GlanceBackendProjectionRejected` | returns the error; reconcile the `services.glance.backends` entries to a valid projection to recover |
| `GlanceBackend` project/prune fails | False | `GlanceBackendError` | returns the error |
| Glance child not yet Ready | False | `WaitingForGlance` | requeue 15s |
| projected Glance spec rejected (HTTP 422 Invalid) | False | `GlanceProjectionRejected` | returns the error; the projection violates a Glance CRD/webhook rule — reconcile the ControlPlane spec to a valid projection to recover |
| Glance create/update fails | False | `GlanceError` | returns the error |
| Glance child Ready | True | `GlanceReady` | — |

### reconcilePlacement

| Aspect | Value |
| --- | --- |
| File | `reconcile_placement.go`, `reconcile_placement_dbcredentials.go` |
| Condition | `PlacementReady` |
| Gate | `KeystoneReady == True` (Placement validates every token against the Keystone child) **and** the `AccountReady` of the `KeystoneService` registration it projects (see [Built-in service registrations](#built-in-service-registrations)) |
| Projects / Owns | one `Placement` child named `{controlplane.Name}-placement` (`placementNameSuffix`) in `cp.PlacementNamespace()`; and, on a managed database only, the per-ControlPlane DB-credential objects in the Placement service namespace: in **Dynamic** mode (the managed-shared default) a ServiceAccount `placement-db-creds`, an mTLS client Certificate `{controlplane.Name}-placement-db-openbao-client`, a `VaultDynamicSecret` generator reading `database/mariadb/creds/placement-{placement-namespace}` (auth role `placement-db`), and a generator-backed `ExternalSecret` `{controlplane.Name}-placement-db-credentials`; in the **Static** opt-out a KV-backed `ExternalSecret` of the same name reading `openstack/placement/{placement-namespace}/{controlplane.Name}/db` (properties `username`, `password`). Only when `spec.services.placement` is set |
| Requeue | `keystoneInfraGateRequeueAfter` = **5s** while gated on Keystone; `korcRequeueAfter` = **10s** while the `placement` service account is not yet Ready; `dbCredentialsRequeueAfter` = **10s** while the Dynamic DB credential has not landed; `infraRequeueAfter` = **15s** while the child is not Ready |

`reconcilePlacement` runs after `reconcileServiceAccounts` (and after
`reconcileGlance`, last in the pipeline) because it gates on the per-account
readiness that stage computes into status in the same pass. It is optional:
`spec.services.placement` unset means this ControlPlane manages no placement
service, and the sub-reconciler reports `PlacementReady=True` /
`PlacementNotManaged` so the aggregate is not blocked (staged adoption). A
previously-projected child is **preserved** unless the ControlPlane opts in with
`c5c3.io/allow-placement-deletion: "true"`; with the annotation set, the child,
its DB-credential ExternalSecret, the Dynamic-mode generator objects, and the
placement catalog K-ORC CRs are deleted. Each object is only removed when this
ControlPlane still owns it, so a foreign object colliding on a name is left
alone.

The credential minter comes down either way. On the preserve branch the
`VaultDynamicSecret`, its client Certificate, and the `placement-db-creds`
ServiceAccount are torn down before the condition is written: a live generator
keeps issuing a fresh MySQL user with all privileges on the `placement` schema at
every refresh interval, for a service this ControlPlane has been told it no
longer manages, behind a `PlacementReady=True` condition that surfaces none of
it.

When managed, the projection follows the same thin discipline as its Keystone,
Horizon, and Glance siblings, reusing the ControlPlane's own specs so Placement
points at the same backing services:

- **Image:** repository defaults to `ghcr.io/c5c3/placement` with the tag derived
  from `spec.openStackRelease`; `spec.services.placement.image` overrides the
  whole image reference when set.
- **Database:** a DeepCopy of the **effective** database
  (`effectivePlacementDatabase`: Placement's
  [dedicated](./controlplane-crd.md#dedicatedbackingservices) database when it
  opted into one, the shared `spec.infrastructure.database` otherwise) with its
  logical database name forced to `placement`, so its schema stays isolated from
  Keystone's on a shared cluster. In managed mode (`clusterRef` set) the
  `secretRef` is repointed at the operator-owned
  `{controlplane.Name}-placement-db-credentials` Secret (key `password`), and the
  projected `credentialsMode` is the **effective** mode: `Dynamic`
  (engine-issued) by default on the managed shared database, drawn from the
  engine role `placement-{placement-ns}` that `setup-database-tenant.sh`
  provisions, flipped to `Static` by the shared-block opt-out or the per-service
  `services.placement.databaseCredentialsMode` override, and always `Static` for
  a dedicated placement database (fail-closed even when the stored mode says
  otherwise). A brownfield database keeps the user-supplied `secretRef` and
  `credentialsMode`.
- **Cache:** a DeepCopy of the **effective** cache (`effectivePlacementCache`).
- **Keystone endpoint:** `keystoneEndpoint` is derived top-down via
  `placementKeystoneEndpoint(cp)`, the cluster-local
  `{controlplane.Name}-keystone` Service URL (the same URL K-ORC authenticates
  against) while Placement and Keystone resolve to the same cluster, and never
  the external `publicEndpoint` or gateway hostname, because
  Placement validates tokens server-side and the pods have to reach it
  in-cluster. Placed apart, Placement gets the public URL (see
  [Reaching a placed service](#reaching-a-placed-service)).
  `keystonePublicEndpoint` is a pass-through of the Keystone
  service's own public endpoint (empty when Keystone is not externally exposed,
  in which case the child falls back to the internal URL).
- **Service user:** derived from the `placement` account the projected
  `KeystoneService` registration provisions, its `username`, project, and
  user/project domains, with the password read from the consumer Secret that
  registration delivers, `{controlplane.Name}-placement-credentials`.
- **ExtraConfig:** `spec.globalExtraConfig` merged with
  `services.placement.extraConfig` (the per-service value winning key by key),
  assigned unconditionally so clearing the ControlPlane block reverts the child
  instead of pinning the last projected value.
- **Gateway / Replicas / SecretStoreRef / Region:** `gateway` is a DeepCopy of
  `spec.services.placement.gateway` (a nil source clears it, tearing the
  HTTPRoute down); `replicas` defaults to `commonv1.DefaultReplicas`, overridden
  by `spec.services.placement.replicas`; the resolved store selection and
  `spec.region` are projected through. `spec.apiServer` is deliberately **not**
  set, so the child-side uWSGI defaults stay authoritative.

The DB-credential objects are ensured **before** the child, so the Secret it
references exists when the placement operator resolves it. They are ensured for a
managed database only: a brownfield one carries a user-supplied credential
out-of-band, leaving nothing to project. In `Dynamic` mode the projection is held
(`WaitingForPlacementDBCredential`) until the generator-backed ExternalSecret
reports Ready **and** the Secret it targets carries an engine-issued username, so
an existing child keeps its current mode rather than being handed a DSN for a
login the engine never issued.

The placement catalog entry, a `placement`-type K-ORC `Service` with an internal
and a public `Endpoint`, is registered by
[`reconcileCatalog`](#reconcilecatalog) rather than here; unsetting
`spec.services.placement` with the deletion annotation set sweeps those CRs along
with the child.

A child placed outside the ControlPlane's namespace
(`services.placement.namespace`) carries no owner reference: it is stamped with
the ownership labels and applied unowned, and the finalizer sweeps it by those
labels.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.services.placement` unset | True | `PlacementNotManaged` | staged adoption; a previously-projected child is preserved unless `c5c3.io/allow-placement-deletion: "true"` is set, but its dynamic DB-credential generator, ServiceAccount, and client Certificate are torn down either way, so no credential minter outlives the service |
| `KeystoneReady` not True | False | `WaitingForKeystone` | requeue 5s; no Placement CR is projected while Keystone is unready |
| the projected registration has not provisioned the account yet | False | `WaitingForServiceRegistration` | requeue 10s; no Placement child is written until the Keystone user and its password exist. See [Built-in service registrations](#built-in-service-registrations) |
| projecting, reading or mirroring the registration child fails | False | `ServiceRegistrationError` | returns the error |
| the registration child carries foreign spec fields | False | `ServiceRegistrationFieldsReclaimed` | they are reset and the pass halts; requeue 10s |
| Placement placed on a target cluster that does not resolve | False | `TargetClusterUnavailable` | requeue 10s; the DB credential resolves its namespace's cluster before it writes anything (`ensureServiceDBCredential`). The resolver's own message |
| DB-credential ensure fails (Dynamic generator objects or Static ExternalSecret) | False | `PlacementDBCredentialError` | returns the error (managed database only) |
| Dynamic DB credential not yet materialised | False | `WaitingForPlacementDBCredential` | requeue 10s; the message names the `database/mariadb/creds/placement-<namespace>` path, which only exists once `setup-database-tenant.sh` has onboarded the tenant, or the non-engine-issued username it found in the target Secret |
| Placement child not yet Ready | False | `WaitingForPlacement` | requeue 15s |
| projected Placement spec rejected (HTTP 422 Invalid) | False | `PlacementProjectionRejected` | returns the error; the projection violates a Placement CRD/webhook rule, so reconcile the ControlPlane spec to a valid projection to recover |
| Placement create/update fails | False | `PlacementError` | returns the error |
| Placement child Ready | True | `PlacementReady` | — |

### reconcileBarbican

| Aspect | Value |
| --- | --- |
| File | `reconcile_barbican.go`, `reconcile_barbican_openbao.go`, `reconcile_barbican_dbcredentials.go` |
| Condition | `BarbicanReady` |
| Gate | `KeystoneReady == True` (Barbican validates every token against the Keystone child) **and** the `AccountReady` of the `KeystoneService` registration it projects (see [Built-in service registrations](#built-in-service-registrations)) |
| Projects / Owns | a trio in `cp.BarbicanNamespace()`: one `Barbican` child `{controlplane.Name}-barbican` (`barbicanNameSuffix`), one `BarbicanSecretStore` `{controlplane.Name}-barbican-store` (`barbicanSecretStoreNameSuffix`), and — on a dedicated secret store only — one `OpenBaoCluster` `{controlplane.Name}-barbican-bao` (`barbicanOpenBaoNameSuffix`) with the ensemble below. Plus, on a managed database only, the per-ControlPlane DB-credential objects in the same namespace: in **Dynamic** mode (the managed-shared default) a ServiceAccount `barbican-db-creds`, an mTLS client Certificate `{controlplane.Name}-barbican-db-openbao-client`, a `VaultDynamicSecret` generator reading `database/mariadb/creds/barbican-{barbican-namespace}` (auth role `barbican-db`), and a generator-backed `ExternalSecret` `{controlplane.Name}-barbican-db-credentials`; in the **Static** opt-out a KV-backed `ExternalSecret` of the same name reading `openstack/barbican/{barbican-namespace}/{controlplane.Name}/db` (properties `username`, `password`). Only when `spec.services.barbican` is set |
| Requeue | `keystoneInfraGateRequeueAfter` = **5s** while gated on Keystone; `korcRequeueAfter` = **10s** while the `barbican` service account is not yet Ready; `dbCredentialsRequeueAfter` = **10s** while the Dynamic DB credential has not landed; `infraRequeueAfter` = **15s** while the dedicated ensemble's target cluster does not resolve, while the OpenBao instance is not Available, while the secret store is being recreated, and while the child is not Ready |

`reconcileBarbican` runs after `reconcileGlance` and `reconcilePlacement` in the
tail group, with `reconcileOVN` and `reconcileNeutron` behind it. It gates on the
`AccountReady` of the `KeystoneService` registration it projects for itself,
which the same pass writes. It is optional: `spec.services.barbican` unset means
this ControlPlane manages no key manager, and the sub-reconciler reports
`BarbicanReady=True` / `BarbicanNotManaged` so the aggregate is not blocked
(staged adoption).

Unsetting the block deletes nothing on its own.
`c5c3.io/allow-barbican-deletion: "true"` opts in to releasing the child, its
secret store, its DB-credential ExternalSecret, and the key-manager catalog
K-ORC CRs; destroying a dedicated OpenBao instance takes a second annotation on
top of that one. Each object is only removed while this ControlPlane still owns
it, so a foreign object colliding on a name is left alone. See
[Barbican secret-store teardown](#barbican-secret-store-teardown) for the order
and for what each annotation authorises.

The credential minter comes down either way. On the preserve branch the
`VaultDynamicSecret`, its client Certificate, and the `barbican-db-creds`
ServiceAccount are torn down before the condition is written: a live generator
keeps issuing a fresh MySQL user with all privileges on the `barbican` schema at
every refresh interval, for a service this ControlPlane no longer manages, behind
a `BarbicanReady=True` condition that surfaces none of it.

When managed, the projection follows the same thin discipline as its Keystone,
Horizon, Glance, and Placement siblings:

- **Image:** repository defaults to `ghcr.io/c5c3/barbican` with the tag derived
  from `spec.openStackRelease`; `spec.services.barbican.image` overrides the
  whole image reference when set.
- **Database:** a DeepCopy of the **effective** database
  (`effectiveBarbicanDatabase`: Barbican's
  [dedicated](./controlplane-crd.md#barbicandedicatedbackingservicesspec)
  database when it opted into one, the shared `spec.infrastructure.database`
  otherwise) with its logical database name forced to `barbican`. The name is
  fixed: the pre-wired OpenBao bootstrap grants the dynamic credential access to
  that schema alone. In managed mode (`clusterRef` set) the
  `secretRef` is repointed at the operator-owned
  `{controlplane.Name}-barbican-db-credentials` Secret (key `password`), and the
  projected `credentialsMode` is the **effective** mode: `Dynamic`
  (engine-issued) by default on the managed shared database, drawn from the
  engine role `barbican-{barbican-ns}` that `setup-database-tenant.sh`
  provisions, flipped to `Static` by the shared-block opt-out or the per-service
  `services.barbican.databaseCredentialsMode` override, and always `Static` for a
  dedicated barbican database (fail-closed even when the stored mode says
  otherwise). A brownfield database keeps the user-supplied `secretRef` and
  `credentialsMode`.
- **Cache:** a DeepCopy of the **effective** cache (`effectiveBarbicanCache`).
- **Keystone endpoint:** `keystoneEndpoint` is derived top-down via
  `barbicanKeystoneEndpoint(cp)`, the cluster-local
  `{controlplane.Name}-keystone` Service URL (the same URL K-ORC authenticates
  against) while Barbican and Keystone resolve to the same cluster, and never the
  external `publicEndpoint` or gateway hostname, because
  Barbican validates tokens server-side and the pods have to reach it
  in-cluster. Placed apart, Barbican gets the public URL (see
  [Reaching a placed service](#reaching-a-placed-service)).
  `keystonePublicEndpoint` is a pass-through of the Keystone
  service's own public endpoint, the URL Barbican advertises on a 401 (empty when
  Keystone is not externally exposed, in which case the child falls back to the
  internal endpoint).
- **Service user:** derived from the `barbican` account the projected
  `KeystoneService` registration provisions, its `username`, project, and
  user/project domains, with the password read from the consumer Secret that
  registration delivers, `{controlplane.Name}-barbican-credentials`.
- **ExtraConfig:** `spec.globalExtraConfig` merged with
  `services.barbican.extraConfig` (the per-service value winning key by key),
  assigned unconditionally so clearing the ControlPlane block reverts the child
  instead of pinning the last projected value.
- **Gateway / Replicas / SecretStoreRef / Region:** `gateway` is a DeepCopy of
  `spec.services.barbican.gateway` (a nil source clears it, tearing the HTTPRoute
  down); `replicas` defaults to `commonv1.DefaultReplicas`, overridden by
  `spec.services.barbican.replicas`; the resolved store selection and
  `spec.region` are projected through. `spec.apiServer` and `spec.dbClean` are
  not set, so the child-side uWSGI parameters and clean-up schedule keep tracking
  the barbican operator's defaults.

The DB-credential objects are ensured **before** the child, so the Secret it
references exists when the barbican operator resolves it. In `Dynamic` mode the
projection is held (`WaitingForBarbicanDBCredential`) until the generator-backed
ExternalSecret reports Ready **and** the Secret it targets carries an
engine-issued username.

#### The dedicated OpenBao ensemble

A `secretStore.dedicated` Barbican gets an OpenBao instance of its own in the
Barbican service namespace, projected by `ensureBarbicanOpenBao` in dependency
order before the store that points at it. Every object is named after the
instance `{controlplane.Name}-barbican-bao`, because the openbao-operator derives
several of its own object names from that name:

| Object | Name | Purpose |
| --- | --- | --- |
| `OpenBaoTenant` | `{instance}-tenant` | Admits the service namespace to the openbao-operator |
| `Secret` | `{instance}-unseal-key` | The static-seal key, under data key `key`. Written before the CR exists, or the operator blind-creates an immutable Secret of its own and the generated key is lost |
| `Certificate` (cert-manager) | `{instance}-tls-server`, `{instance}-tls-ca` | The two fixed-name Secrets the instance's `External` TLS mode consumes |
| `ServiceAccount` | `{instance}-provisioner` | The identity the Kubernetes-auth role `provisioner` binds to, which the barbican operator logs in with |
| `Role` + `RoleBinding` | `{instance}-provisioner-token` | Let the barbican operator mint a bound token for that account |
| `ClusterRoleBinding` | `{instance}-{hash}-auth-delegator` | Grants the TokenReview every Kubernetes-auth login is validated with. Cluster-scoped, so the name folds in eight bytes of `sha256("{namespace}/{instance}")`: two ControlPlanes of the same name in different namespaces derive the same instance name and would otherwise collide on one binding |

The instance runs the `Development` profile at a single replica with a static
seal (see
[BarbicanDedicatedSecretStoreSpec](./controlplane-crd.md#barbicandedicatedsecretstorespec)
for what that posture costs) and `deletionPolicy: DeletePVCs`. The deletion policy
is not optional: the instance name is derived from the ControlPlane name, so a
re-created ControlPlane of the same name would meet the previous instance's
`data-{instance}-0` PVC, raft storage initialised under a seal key that no longer
exists, and never unseal. Its self-init requests (the KV mount, the AppRole
identity, the Kubernetes auth method, and the two policies scoping them) run once
against freshly initialised storage, so changing them requires recreating the
instance and its PVC.

The whole ensemble is written to the cluster the Barbican service was placed on
(its [`targetClusterRef`](../target-clusters.md)), because everything that acts on
it runs there: the openbao-operator that reconciles the instance, the cert-manager
that issues its transport certificates, the TokenReview its Kubernetes-auth logins
are validated with, and the Barbican pods that read their key material from it.
The cluster-scoped `ClusterRoleBinding` is created on the target for the same
reason — the account it names is a target-cluster account. On the target every
object is claimed by the ownership labels alone. The one owner reference in the
ensemble is target-local and stays: the unseal Secret takes the instance's, and
the two live in the same namespace on the same cluster. The cluster is resolved
before the first object is written, so an unresolvable name parks
`BarbicanReady=False/TargetClusterUnavailable` and leaves the ensemble unwritten
on both clusters.

The instance's `spec.network` carries two allowlists. `trustedIngressPeers` names
the barbican operator's pods and the Barbican API pods, the only sources admitted
to the API port. `apiServerEndpointIPs` is the egress half, and
`resolveAPIServerEndpointIPs` resolves it per pass from the EndpointSlice
`kubernetes` in `default` **on the cluster the instance runs on**, deduplicated
and sorted. The policy is enforced by the CNI there, over pods that reach their
own API server, so for a placed Barbican the management cluster's addresses would
allow the instance nothing it needs. Without it the operator-rendered
NetworkPolicy allows the API server only at the in-cluster service VIP on port 443,
which a CNI enforcing egress against the post-DNAT destination never matches, and
the instance loses the API server: raft auto-join times out, self-init cannot
complete, and the partial raft state wedges every later initialization attempt.
Sorting keeps the desired-versus-live comparison from reading endpoint reordering as
drift, and the read goes through that cluster's uncached reader so no cluster-wide
EndpointSlice informer starts for one well-known object.

That resolution fails closed. An instance created without the egress rules is
recoverable only by deleting it together with its PVC, so a pass that cannot resolve
the addresses writes no `OpenBaoCluster` at all and reports
`BarbicanReady=False/BarbicanOpenBaoError`. Under `rbac.namespaceScoped`, where the
operator's Role cannot read across into `default`, every dedicated store takes that
path.

The store, and with it the child, waits until the instance is `Available`
(`BarbicanReady=False/WaitingForOpenBaoInstance`): a store attached to an
instance that is still initialising reports `ProvisioningDenied` and would have to
be re-driven from a failure state. That gate is read from the cluster the instance
was provisioned on. The `BarbicanSecretStore` and the `Barbican` child themselves
do not move: they are the CRs the barbican operator reconciles from the management
cluster, and it is that operator which then projects its own children onto the
target.

#### Projecting the secret store

`reconcileBarbicanSecretStore` projects the one
[`BarbicanSecretStore`](../barbican/barbican-secret-store-crd.md) the service
attaches to. It references its Barbican by name (inverted attachment), so it can
be applied before the child exists.

A store whose frozen fields moved cannot be updated: the CRD's transition rules
reject the write, and the loop would re-attempt it on every requeue with no
self-heal. `barbicanSecretStoreImmutableDrift` compares the four fields the CRD
freezes (the `barbicanRef`, the `type`, the store mode `instanceRef` vs
`server`, and the KV mountpoint); when one moved, the live store is deleted and
recreated on the
next pass, with `BarbicanReady=False/RecreatingBarbicanSecretStore` reported in
between. The AppRole credentials the barbican operator minted for it carry its
owner reference, so they are collected with it rather than left pointing at the
retired mount.

Every write routes through `ensureUnownedOrOwned`, and the prune sweep deletes
only c5c3-owned stores carrying the Barbican child's name prefix, so a
hand-created store attached to the same Barbican is never pruned or overwritten.

The key-manager catalog entry, a `key-manager`-type K-ORC `Service` with an
internal and a public `Endpoint`, is registered by
[`reconcileCatalog`](#reconcilecatalog) rather than here. A child placed outside
the ControlPlane's namespace (`services.barbican.namespace`) carries no owner
reference: it is stamped with the ownership labels and applied unowned, and the
finalizer sweeps it by those labels.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.services.barbican` unset | True | `BarbicanNotManaged` | staged adoption; the child, its store, and the catalog CRs are preserved unless `c5c3.io/allow-barbican-deletion: "true"` is set, and the dedicated OpenBao instance until `c5c3.io/allow-barbican-secret-store-data-deletion: "true"` is set on top of it. The dynamic DB-credential generator, ServiceAccount, and client Certificate are torn down either way. The message names whichever annotation is still missing |
| `KeystoneReady` not True | False | `WaitingForKeystone` | requeue 5s; no Barbican CR is projected while Keystone is unready |
| the projected registration has not provisioned the account yet | False | `WaitingForServiceRegistration` | requeue 10s; no Barbican child is written until the Keystone user and its password exist. See [Built-in service registrations](#built-in-service-registrations) |
| projecting, reading or mirroring the registration child fails | False | `ServiceRegistrationError` | returns the error |
| the registration child carries foreign spec fields | False | `ServiceRegistrationFieldsReclaimed` | they are reset and the pass halts; requeue 10s |
| Barbican placed on a target cluster that does not resolve | False | `TargetClusterUnavailable` | requeue 10s from the DB credential, which resolves its namespace's cluster before it writes anything (`ensureServiceDBCredential`); requeue 15s from the dedicated OpenBao ensemble, which is where a brownfield database reaches the resolver first. Nothing is written, on either cluster. The resolver's own message |
| DB-credential ensure or read fails (Dynamic generator objects or Static ExternalSecret) | False | `BarbicanDBCredentialError` | returns the error (managed database only) |
| Dynamic DB credential not yet materialised | False | `WaitingForBarbicanDBCredential` | requeue 10s; the message names the `database/mariadb/creds/barbican-<namespace>` path, which only exists once `setup-database-tenant.sh` has onboarded the tenant, or the non-engine-issued username it found in the target Secret |
| provisioning the dedicated OpenBao ensemble fails | False | `BarbicanOpenBaoError` | returns the error |
| dedicated OpenBao instance not yet `Available` | False | `WaitingForOpenBaoInstance` | requeue 15s; the store and the child are projected once it serves requests |
| `BarbicanSecretStore` project or prune fails | False | `BarbicanSecretStoreError` | returns the error |
| projected `BarbicanSecretStore` rejected (HTTP 422 Invalid) | False | `BarbicanSecretStoreProjectionRejected` | returns the error; reconcile `services.barbican.secretStore` to a valid projection to recover |
| live store's frozen fields moved, so it was deleted | False | `RecreatingBarbicanSecretStore` | requeue 15s; recreated on the next pass |
| Barbican child not yet Ready | False | `WaitingForBarbican` | requeue 15s |
| projected Barbican spec rejected (HTTP 422 Invalid) | False | `BarbicanProjectionRejected` | returns the error; the projection violates a Barbican CRD/webhook rule, so reconcile the ControlPlane spec to a valid projection to recover |
| Barbican create/update fails | False | `BarbicanError` | returns the error |
| Barbican child Ready | True | `BarbicanReady` | — |

### reconcileOVN

| Aspect | Value |
| --- | --- |
| File | `reconcile_ovn.go` |
| Condition | `OVNReady` |
| Gate | none. The OVNCentral is deployed outside the plane, so nothing this chain produces can make it ready |
| Projects / Owns | nothing. The sub-reconciler only READS the `OVNCentral` named by `spec.services.neutron.ovn.centralRef`, in `cp.NeutronOVNCentralNamespace()` (the ref's namespace, or the ControlPlane's own when it is empty) |
| Requeue | `infraRequeueAfter` = **15s** on every not-yet arm |

`reconcileOVN` is the one sub-reconciler that writes nothing. The OVN control
plane is referenced the way the infrastructure clusters in `spec.infrastructure`
are: the ControlPlane never projects the central, never updates it, never deletes
it. What it takes from the central is what the Neutron projection consumes, the
two database addresses the ML2/OVN mechanism driver dials and the client Secret
it presents there. The pass reads the central, decides whether those are usable,
and records the verdict in `OVNReady`.

The read goes through the **local** client whatever cluster the network service
is placed on: the `OVNCentral` CR lives on the management cluster, where the
ovn-operator reconciles it, and only the children it projects land on the target
cluster its own `spec.targetClusterRef` selects.

Which address form is mirrored depends on placement. A network service sharing a
cluster with the central reads `status.northbound.internalDbAddress` /
`status.southbound.internalDbAddress`; one on another cluster leaves its own
cluster network and reads `status.northbound.dbAddress` /
`status.southbound.dbAddress`, which exist only while the central publishes both
databases on the node network. A cross-cluster placement therefore also requires
`spec.northbound.externallyReachable` and `spec.southbound.externallyReachable`
on the central.

Before the read, the pass re-runs the reach rule the validating webhook enforces
on `centralRef.namespace` (`ValidateNeutronOVNCentralNamespace`, exported for this
caller). It is the controller-side backstop `keystoneServiceNamespaceAllowed` is
for the sibling cross-namespace trust decision: a ControlPlane can reach etcd
without ever passing through admission — an unregistered webhook during install, a
GitOps or etcd restore replaying stored objects — and naming a foreign plane's
central is not a read-only act. The arms below would relay that central's database
addresses and status, and the Neutron projection gated on `OVNReady` would hand
the child a pointer whose operator mirrors the central's client `Secret`, a full
mTLS identity for its Northbound and Southbound databases, into this plane's
namespace. So the read does not happen at all, and the condition carries the
webhook's own message.

No arm returns an error except the read failure: nothing the ControlPlane does
can converge a central it does not own, so every other arm requeues after 15s and
waits for the ovn-operator.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.services.neutron` unset | True | `OVNNotManaged` | staged adoption; no OVN control plane is consumed, so the aggregate is not blocked |
| the resolved `centralRef` namespace is outside the plane's reach | False | `OVNCentralNamespaceForbidden` | requeue 15s; the central is NOT read, so none of its addresses or status reaches this plane. The message is the webhook's own, naming the field and the direction of the refusal |
| the referenced `OVNCentral` does not exist | False | `OVNCentralNotFound` | requeue 15s; the message carries the full `<namespace>/<name>` the ref resolved to |
| the `OVNCentral` CRD is not served | False | `OVNCentralReadError` | requeue 15s; the message says to install the ovn-operator. A no-match is a deployment gap, not a fault manager backoff can retry out of |
| reading the `OVNCentral` fails otherwise | False | `OVNCentralReadError` | returns the error wrapped as `reading OVNCentral <namespace>/<name>` |
| the network service runs on another cluster and the central does not publish both databases | False | `OVNCentralNotExternallyReachable` | requeue 15s; the message names `spec.northbound.externallyReachable` and `spec.southbound.externallyReachable` |
| the central has not reported `Ready`, or reports it not True | False | `WaitingForOVNCentral` | requeue 15s; the central's own reason and message are relayed verbatim, because "not ready" alone says nothing an operator can act on |
| the central reports `Ready` but has not published both addresses and `status.clientSecretName` | False | `OVNEndpointsPending` | requeue 15s; the message names each status field that is still empty, in the address form the placement selected |
| the central is ready and complete | True | `OVNCentralReady` | the message carries the Northbound and Southbound addresses that were selected |

### reconcileNeutron

| Aspect | Value |
| --- | --- |
| File | `reconcile_neutron.go`, `reconcile_neutron_messaging.go`, `reconcile_neutron_dbcredentials.go` |
| Condition | `NeutronReady` |
| Gate | `KeystoneReady == True` (Neutron validates every token against the Keystone child), `OVNReady == True` (the ML2/OVN mechanism driver writes every network into the referenced central's Northbound database) **and** the `AccountReady` of the `KeystoneService` registration it projects (see [Built-in service registrations](#built-in-service-registrations)) |
| Projects / Owns | one `Neutron` child named `{controlplane.Name}-neutron` (`neutronNameSuffix`) in `cp.NeutronNamespace()`; the bus delivery beside it, a `{controlplane.Name}-neutron-messaging` Secret (key `transport_url`) and, only while the shared bus declares `tls`, a `{controlplane.Name}-neutron-messaging-ca` Secret (key `ca.crt`), both written on the client that namespace resolves to and claimed by a controller owner reference at home or by the ownership labels in a service namespace or on a target cluster; and, on a managed database only, the per-ControlPlane DB-credential objects in the same namespace: in **Dynamic** mode (the managed-shared default) a ServiceAccount `neutron-db-creds`, an mTLS client Certificate `{controlplane.Name}-neutron-db-openbao-client`, a `VaultDynamicSecret` generator reading `database/mariadb/creds/neutron-{neutron-namespace}` (auth role `neutron-db`), and a generator-backed `ExternalSecret` `{controlplane.Name}-neutron-db-credentials`; in the **Static** opt-out a KV-backed `ExternalSecret` of the same name reading `openstack/neutron/{neutron-namespace}/{controlplane.Name}/db` (properties `username`, `password`). Only when `spec.services.neutron` is set |
| Requeue | `keystoneInfraGateRequeueAfter` = **5s** while gated on `KeystoneReady` or `OVNReady`; `infraRequeueAfter` = **15s** while the bus material has not landed, while the namespace's cluster does not resolve, and while the child is not Ready; `korcRequeueAfter` = **10s** while the `neutron` service account is not yet Ready; `dbCredentialsRequeueAfter` = **10s** while the Dynamic DB credential has not landed |

`reconcileNeutron` gates in order: `KeystoneReady`, `OVNReady`, the shared message
bus delivered into the network service's namespace, the `KeystoneService`
registration whose account Neutron authenticates as, and the DB credential the
child references. The bus runs ahead of the registration because it reads nothing
the registration produces and writes into the namespace on the cluster the child
is applied to, so an unresolvable target cluster surfaces before the plane
projects the registration that mints the Keystone user.
The transport URL's digest is **not** projected onto the child: the neutron
operator rolls its pods off the Secret it derives itself, so a second digest on
the child would only add a redundant rollout trigger.

It is optional: `spec.services.neutron` unset means this ControlPlane manages no
network service, and the sub-reconciler reports `NeutronReady=True` /
`NeutronNotManaged` so the aggregate is not blocked (staged adoption).

When managed, the projection follows the same thin discipline as its Placement
and Barbican siblings, reusing the ControlPlane's own specs so Neutron points at
the same backing services:

- **Image:** repository defaults to `ghcr.io/c5c3/neutron` with the tag derived
  from `spec.openStackRelease`; `spec.services.neutron.image` overrides the whole
  image reference when set.
- **Database:** a DeepCopy of the **effective** database
  (`effectiveNeutronDatabase`: Neutron's
  [dedicated](./controlplane-crd.md#neutrondedicatedbackingservicesspec) database
  when it opted into one, the shared `spec.infrastructure.database` otherwise)
  with its logical database name forced to `neutron`. In managed mode
  (`clusterRef` set) the `secretRef` is repointed at the operator-owned
  `{controlplane.Name}-neutron-db-credentials` Secret (key `password`), and the
  projected `credentialsMode` is the **effective** mode: `Dynamic` (engine-issued)
  by default on the managed shared database, drawn from the engine role
  `neutron-{neutron-ns}` that `setup-database-tenant.sh` provisions, flipped to
  `Static` by the shared-block opt-out or the per-service
  `services.neutron.databaseCredentialsMode` override, and always `Static` for a
  dedicated neutron database. A brownfield database keeps the user-supplied
  `secretRef` and `credentialsMode`.
- **Cache:** a DeepCopy of the **effective** cache (`effectiveNeutronCache`).
- **Messaging:** a **brownfield** `secretRef` naming the
  `{controlplane.Name}-neutron-messaging` Secret the pass wrote beside the child,
  under the key `transport_url`. `messaging.tls.caBundleSecretRef` is set only
  while `spec.infrastructure.messaging.tls` is declared, and names the
  `{controlplane.Name}-neutron-messaging-ca` mirror under `ca.crt`. Both resolve
  in the Neutron's own namespace on the Neutron's own cluster, which is where the
  neutron operator looks for them. Dropping the `tls` block reverts both halves in
  order: the projection removes the child's pointer first, and only once the child
  reports having converged on that spec — Ready, with a
  `status.observedGeneration` that has caught up with the generation the apply
  produced — does `pruneNeutronMessagingCA` reap the mirror. Both steps of the
  order matter, because the pointer and the volume are removed on different
  passes: the prune deliberately does not live on the messaging leg, which runs
  ahead of every gate that can halt the pass — a service-account rotation, a DB
  credential mid-rotation, a transient API error — and the neutron operator
  re-renders the Deployment that mounts the mirror only after the pointer is gone
  from the CR. Reaping ahead of either step leaves a live pod template naming a
  volume source that no longer exists, wedging every restarting pod on
  `CreateContainerConfigError`.
- **OVN:** `spec.ovn.centralRef` carries the referenced central's name and its
  namespace **resolved here** rather than passed through: an empty ref namespace
  means the ControlPlane's own namespace, which is not the namespace the child
  would default it to once it is placed elsewhere.
- **Keystone endpoint:** `keystoneEndpoint` is derived top-down via
  `neutronKeystoneEndpoint(cp)`, the cluster-local `{controlplane.Name}-keystone`
  Service URL while Neutron and Keystone resolve to the same cluster, and the
  public URL when they are placed apart (see
  [Reaching a placed service](#reaching-a-placed-service)).
  `keystonePublicEndpoint` is a pass-through of the Keystone service's own public
  endpoint.
- **Service user:** derived from the `neutron` account the projected
  `KeystoneService` registration provisions, its `username`, the
  `service-neutron` project, and both domains from the ControlPlane's effective
  admin domain, with the password read from the consumer Secret that registration
  delivers.
- **ExtraConfig:** `spec.globalExtraConfig` merged with
  `services.neutron.extraConfig` (the per-service value winning key by key),
  assigned unconditionally so clearing the ControlPlane block reverts the child
  instead of pinning the last projected value.
- **Gateway / Replicas / SecretStoreRef / Region:** `gateway` is a DeepCopy of
  `spec.services.neutron.gateway` (a nil source clears it, tearing the HTTPRoute
  down); `deployment.replicas` and `workers.deployment.replicas` both default to
  `commonv1.DefaultReplicas` and are overridden by `services.neutron.replicas`
  and `services.neutron.workerReplicas`; the resolved store selection and
  `spec.region` are projected through. `spec.apiServer`, `spec.ovnDBSync`,
  `spec.networkPolicy`, `spec.autoscaling` and `spec.logging` are **not** set, so
  the child-side defaults stay authoritative.

Unsetting `spec.services.neutron` deletes nothing on its own. With
`c5c3.io/allow-neutron-deletion: "true"`, `deleteOrphanedNeutron` releases, in
order, the `Neutron` child, the DB-credential `ExternalSecret`, the Dynamic-mode
`VaultDynamicSecret`, its client Certificate and the `neutron-db-creds`
ServiceAccount, the two messaging Secrets, and finally the `KeystoneService`
registration, whose finalizer is what tears the network catalog rows, the service
user and its project down. Each object is only removed while this ControlPlane
still owns it, so a foreign object colliding on a name is left alone. The
referenced `OVNCentral` is **never** deleted: it is deployed outside the plane and
only read.

The credential minter comes down either way. On the preserve branch the
`VaultDynamicSecret`, its client Certificate, and the `neutron-db-creds`
ServiceAccount are torn down before the condition is written: a live generator
keeps issuing a fresh MySQL user with all privileges on the `neutron` schema at
every refresh interval, for a service this ControlPlane has been told it no longer
manages, behind a `NeutronReady=True` condition that surfaces none of it.

A child placed outside the ControlPlane's namespace
(`services.neutron.namespace`) carries no owner reference: it is stamped with the
ownership labels and applied unowned, and the finalizer sweeps it by those labels.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.services.neutron` unset | True | `NeutronNotManaged` | staged adoption; a previously-projected child is preserved unless `c5c3.io/allow-neutron-deletion: "true"` is set, but its dynamic DB-credential generator, ServiceAccount, and client Certificate are torn down either way |
| `KeystoneReady` not True | False | `WaitingForKeystone` | requeue 5s; no Neutron CR is projected while Keystone is unready |
| `OVNReady` not True | False | `WaitingForOVN` | requeue 5s; a Neutron pointed at a central whose databases do not serve cannot program a single network |
| the shared bus has not delivered its transport URL yet | False | `WaitingForMessagingCredentials` | requeue 15s; nothing is written, so the child never sees a partial URL |
| the messaging CA bundle Secret is absent or carries no data under its key | False | `WaitingForMessagingCABundle` | requeue 15s; an empty key is the ordinary transient of a two-step create-then-populate flow, so it waits rather than mirroring an empty trust anchor |
| resolving the URL, writing either messaging Secret, or reaping the stale CA mirror after the child stopped naming it fails | False | `NeutronMessagingError` | returns the error |
| the cluster the Neutron namespace resolves to is unavailable | False | `TargetClusterUnavailable` | the resolver's own message; a wait, not a failed reconcile. Requeue 15s from the bus delivery, 10s from the DB credential |
| the projected registration has not provisioned the account yet | False | `WaitingForServiceRegistration` | requeue 10s; no Neutron child is written until the Keystone user and its password exist. See [Built-in service registrations](#built-in-service-registrations) |
| projecting, reading or mirroring the registration child fails | False | `ServiceRegistrationError` | returns the error |
| the registration child carries foreign spec fields | False | `ServiceRegistrationFieldsReclaimed` | they are reset and the pass halts; requeue 10s |
| DB-credential ensure fails (Dynamic generator objects or Static ExternalSecret) | False | `NeutronDBCredentialError` | returns the error (managed database only) |
| Dynamic DB credential not yet materialised | False | `WaitingForNeutronDBCredential` | requeue 10s; the message names the `database/mariadb/creds/neutron-<namespace>` path, which only exists once `setup-database-tenant.sh` has onboarded the tenant, or the non-engine-issued username it found in the target Secret |
| Neutron child not yet Ready | False | `WaitingForNeutron` | requeue 15s |
| projected Neutron spec rejected (HTTP 422 Invalid) | False | `NeutronProjectionRejected` | returns the error; the projection violates a Neutron CRD/webhook rule, so reconcile the ControlPlane spec to a valid projection to recover |
| Neutron create/update fails | False | `NeutronError` | returns the error |
| Neutron child Ready and its registration Ready | True | `NeutronReady` | — |

### reconcileKORC

| Aspect | Value |
| --- | --- |
| File | `reconcile_korc.go` |
| Condition | `KORCReady` |
| Gate | none (but defers until the admin-password Secret is readable) |
| Projects / Owns | one K-ORC `ApplicationCredential` named `{controlplane.Name}-admin-app-credential` and the password-based clouds.yaml Secret `{controlplane.Name}-admin-password-cloud`, both in `childNamespace(cp)` |
| Requeue | `korcRequeueAfter` = **10s** while deferring, while the CRD is missing, while a re-mint is in progress, or while the AC is not yet Available |

`reconcileKORC` create-or-updates an **owned** K-ORC `ApplicationCredential` CR
that instructs K-ORC to mint the admin application credential, and drives re-mint. Key behaviours:

- **Restricted → Unrestricted inversion (CRITICAL).** Our
  `ApplicationCredentialSpec.restricted` is the inverse of K-ORC's
  `spec.resource.unrestricted`: `restricted=true ⇒ Unrestricted=false`
  (`ptr.To(!restricted)`). `restricted` defaults to `true` (least-privilege)
  when unset, matching the defaulting webhook.
- **Password-cloud (breaks the self-referential deadlock).** The AC authenticates
  via an operator-owned, password-based clouds.yaml Secret
  `{controlplane.Name}-admin-password-cloud` (`ensureAdminPasswordCloud`), **not**
  `k-orc-clouds-yaml`. That matters because `k-orc-clouds-yaml` *is* the minted
  application credential itself, so deleting the AC to re-mint would invalidate the
  very clouds.yaml needed to re-authenticate; a restricted application credential
  also cannot mint a new application credential. The password-cloud is re-rendered
  from the live admin password on every pass (so a rotation flows through to it)
  and is not churned when the password is unchanged. The Domain/User imports and
  the catalog Service/Endpoint keep using the spec's `CloudCredentialsRef`
  (`k-orc-clouds-yaml`) and tolerate the brief auth gap during a re-mint by
  requeueing.
- **Mode-aware `clouds.yaml` (`korc_cloudsyaml.go`).** Both builders render
  `auth_url`, `endpoint_type` and `region_name` through three resolvers.
  `korcAuthURL` takes the cluster the document is consumed on and returns the
  in-cluster Keystone Service DNS in managed mode — `keystonePublicEndpoint`
  whenever that cluster is not Keystone's (see
  [Reaching a placed service](#reaching-a-placed-service)) — and
  `spec.services.keystone.external.authURL` in External mode; `korcEndpointType`
  returns `internal` in managed mode (K-ORC runs in-cluster, so `public` would
  resolve to an unreachable Gateway host) and the configured `endpointType`
  (default `public`) in External mode; `korcRegion` returns `spec.region`
  (default `RegionOne`). Managed-mode output is byte-identical to before, pinned
  by golden tests, so no upgraded ControlPlane churns its Secrets. The key must
  be `endpoint_type`, never `interface` — K-ORC drops gophercloud's `Interface`
  field (the authoritative note lives on `buildAppCredCloudsYAML`).
- **Admin identities.** `buildPasswordCloudsYAML` renders `username`,
  `project_name` and both domain keys from
  `spec.korc.adminCredential.userName`/`.projectName`/`.domainName`, and
  `ensureKORCAdminImports` uses the same `userName`/`domainName` as the `User`
  and `Domain` import filters. **Same-user constraint:** Keystone's default policy
  mints an application credential only for the token's own user, so the
  `clouds.yaml` `username` and the imported `User` (the AC's `UserRef`) must be the
  same user — both derive from `adminUserName`.
- **UserRef.** The required K-ORC `UserRef` points at the deterministic,
  `cp.Name`-scoped `User` CR `{controlplane.Name}-user-admin`, imported as
  unmanaged by `ensureKORCAdminImports`. The CR name is a stable handle; the
  OpenStack user it resolves to comes from the import filter above.
- **CA bundle (`cacert`).** When `spec.services.keystone.external.caBundleSecretRef`
  is set, the referenced bundle is read from the ControlPlane namespace and
  projected **verbatim** as the inline `cacert` key into **both** operator-owned
  credentials Secrets (`setCACertKey`). K-ORC reads that key natively from the
  same Secret as `clouds.yaml`, so there is no mount and no upstream change. The
  password-cloud is what the AC authenticates with directly; the app-credential
  Secret is the PushSecret's whole-Secret source, so the bundle also reaches
  OpenBao and is read back by the `cacert` entry
  `ensureKORCCloudsYAMLExternalSecret` adds — gated on the **resolved bundle**, the
  same predicate `setCACertKey` writes the source key under, so the read-back can
  never point at a property the PushSecret did not push. Clearing the ref deletes
  the key and drops the read-back entry. A missing Secret/key — or a present-but-
  empty key, the transient of a two-step "create then populate" flow — defers the
  mint (`WaitingForCABundle`); see the [CA-cache aliasing
  caveat](#egress-and-tls-posture).
- **Access rules.** `projectAccessRules` maps our `{service, method, path}` list
  onto K-ORC's rule shape: `service` becomes a `serviceRef` (Kubernetes name ref
  to an ORC `Service` CR, e.g. `identity`), `method` becomes the typed
  `HTTPMethod` enum, and `path` becomes a string pointer.
- **Re-mint trigger (delete + recreate).** K-ORC's AC actuator implements only
  Create + Delete, so a rotated admin password cannot re-mint in place. The SHA-256
  of the admin password is stamped onto the AC CR under the
  `cobaltcore.c5c3.io/admin-password-hash` annotation (`adminPasswordHashAnnotation`);
  on a later pass a mismatch (the hash moved, or the CredentialRotation reconciler
  zeroed the annotation to nudge) drives `reconcileKORC` to **delete** the AC — the
  finalizer revokes the old Keystone credential, authenticating via the
  password-cloud — and **regenerate** the secret `value`, so the next pass recreates
  the AC for a fresh mint. The hash is computed by the package-level
  `computeAdminPasswordHash`, shared with the CredentialRotation reconciler so both
  agree on one derivation. The annotation is (re-)stamped **only on a fresh mint or
  when it is absent**, never overwriting a present-but-empty value — that empty value
  is the CredentialRotation reconciler's nudge marker, so preserving it keeps a
  concurrently-cleared nudge from being silently lost (`shouldStampPasswordHash`).
- **Re-mint on immutable resource-block drift.** K-ORC declares the AC's whole
  `spec.resource` block immutable via CEL (`self == oldSelf`), so a legal, webhook-
  admitted change to `restricted` or `accessRules` cannot be reconciled by an
  in-place update — it would be rejected on every pass. `reconcileKORC` detects drift
  on the operator-managed fields (`Unrestricted`, `UserRef`, `SecretRef`,
  `AccessRules` — never the whole struct, so a K-ORC/CRD-defaulted sub-field can never
  read as permanent drift) and routes it through the **same** delete+recreate re-mint
  (`adminACResourceDrifted`).
- **Re-mint progress / stall.** While the old AC is `Terminating` the condition is
  `KORCReady=False/ReMinting`; if it stays terminating longer than
  `remintStallTimeout` (**5m**) — a finalizer K-ORC cannot clear, e.g. it cannot
  reach Keystone to revoke — it escalates to `KORCReady=False/ReMintStalled`.
- **Status reflection.** `updateAdminApplicationCredentialStatus` reflects the
  observed AC into `cp.Status.AdminApplicationCredential` (`ID`, the inverted
  `Restricted`, and a `LastRotation` re-stamped whenever the credential ID changes —
  i.e. advanced by a completed re-mint).
- **Missing-CRD safety.** If the K-ORC CRD is absent the apiserver/RESTMapper
  returns a no-match error, detected via `meta.IsNoMatchError` and surfaced as a
  clean condition **without** crash-looping the operator.
- **The admin password is read where it lives.** In managed mode the operator
  materialises it beside the Keystone child
  ([reconcileAdminPassword](#reconcileadminpassword)), so a placed Keystone takes
  it to its target cluster and the read goes through that cluster's client. The
  client is resolved for `effectiveAdminPasswordSecretNamespace(cp)`, which is the
  ControlPlane's own namespace in External and brownfield mode — the Secret is the
  user's there, and that namespace never leaves the management cluster. It is the
  only cross-cluster read in this sub-reconciler: everything it writes stays local,
  because K-ORC runs on the management cluster.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| Keystone placed on a target cluster that does not resolve | False | `TargetClusterUnavailable` | requeue 10s; the resolver's own message. Nothing is minted, seeded or written, on either cluster |
| Admin password Secret/key missing | False | `WaitingForAdminPassword` | requeue 10s (via `secrets.IsMissingSecretOrKey`) |
| Admin password read fails otherwise | False | `AdminPasswordError` | returns the error |
| Keystone CA bundle Secret/key missing or empty | False | `WaitingForCABundle` | requeue 10s; no credentials Secret is written before the endpoint can be verified. The message names the field the bundle was read off — `external.caBundleSecretRef` in External mode, `caBundleSecretRef` on a placed Keystone |
| External CA bundle read fails otherwise | False | `CABundleError` | returns the error |
| Password-cloud ensure fails | False | `PasswordCloudError` | returns the error |
| Hash mismatch → AC deleted for re-mint | False | `ReMinting` | requeue 10s; AC deleted + `value` regenerated, recreated next pass |
| `spec.resource` drift (`restricted`/`accessRules`) → AC deleted for re-mint | False | `ReMinting` | requeue 10s; the block is CEL-immutable, so the change forces a delete+recreate |
| Re-mint stuck `Terminating` past `remintStallTimeout` (5m) | False | `ReMintStalled` | requeue 10s; finalizer cannot revoke the old credential |
| AC create/update/delete/read fails otherwise | False | `ApplicationCredentialError` | returns the error |
| `value` regeneration fails | False | `SecretError` | returns the error |
| AC reports a terminal K-ORC error | False | `ApplicationCredentialFailed` | requeue 10s; gated on `orcv1alpha1.GetTerminalError(ac)` (an unrecoverable/invalid-config Progressing reason, e.g. K-ORC cannot authenticate with the clouds.yaml) so a credential that will never converge is not reported as an eternal wait |
| AC not yet `Available` | False | `WaitingForApplicationCredential` | requeue 10s; gated on `orcv1alpha1.IsAvailable(ac)` (K-ORC uses `Available`, not `Ready`) |
| AC minted and Available | True | `ApplicationCredentialMinted` | — |

Both the `ApplicationCredentialFailed` and `WaitingForApplicationCredential`
messages fold in the admin Domain/User import status (`ensureKORCAdminImports`
returns the admin Domain/User imports, and `korcAdminImports.statusFragment()`
names the first that is terminally failed or not yet Available), so the
documented endpoint/clouds.yaml failure class — where K-ORC swallows a list error
and an import hangs on "created externally" — names the stuck dependency instead of
surfacing as an opaque wait.

#### External-mode failure classification

K-ORC collapses **every** hard failure against a pre-existing Keystone — a wrong
admin password (401), an unresolvable `authURL`, an untrusted private CA, a
region/`endpointType` absent from the catalog — into the same **non-terminal**
`Progressing` condition with `reason=TransientError`. Nothing in the observed
inventory is terminal, so neither `GetTerminalError` nor the reason
discriminates: the failure class survives only in the free-text message.

In External mode the ControlPlane therefore classifies on message substrings and
relays K-ORC's message **verbatim** alongside the reason. Classification walks
the admin Domain, then the admin User, then the ApplicationCredential, so the
*root* stuck dependency is reported rather than the resource that merely blocked
on it. Precedence, most specific first: catalog mismatch, credential drift, TLS,
authentication, reachability.

It is gated on External mode — a managed ControlPlane's `KORCReady` reasons are
byte-identical to before — and never runs against an `Available` credential: K-ORC
leaves the message of the last transient attempt on the `Progressing` condition,
and re-classifying it would flip a converged ControlPlane back to
`AuthenticationFailed` on a failure it has already recovered from.

| Path (External mode only) | Status | Reason | Notes |
| --- | --- | --- | --- |
| message contains `401` / `Unauthorized` | False | `AuthenticationFailed` | the external Keystone rejected the admin credential — typically the password was rotated out-of-band and `passwordSecretRef` is stale |
| message contains `no such host` / `connection refused` / `dial tcp` / `i/o timeout` | False | `EndpointUnreachable` | `external.authURL` could not be dialled |
| message contains `x509` | False | `TLSVerificationFailed` | supply the private CA via `external.caBundleSecretRef` |
| message contains `No suitable endpoint could be found in the service catalog` | False | `CatalogEndpointMismatch` | a wrong `external.endpointType` or `spec.region`; fails loud rather than silently importing nothing. gophercloud never names the interface or region it looked for, so the message appends the **effective** `endpointType` and `spec.region` as the values the external catalog must publish |
| message names `identity:create_application_credential` (403) | False | `CredentialDrift` | an application-credential create against a **stale** resolve-once import id; see [drift](#drift-is-surfaced-never-fought) |
| admin import stuck on "Waiting for OpenStack resource to be created externally" beyond `externalImportStallGrace` | False | `ImportStalled` | the silent-empty detector |

`externalImportStallGrace` is **2 minutes**. In External mode every import target
pre-exists by definition, so an import that keeps waiting to be "created
externally" never resolves on its own — K-ORC is looking in the wrong place. The
message names the stuck import and points at `external.endpointType` and
`spec.region`. The window is deliberately shorter than `remintStallTimeout` /
`orcTeardownStallTimeout` (both 5m): those wait on work that is genuinely in
flight, whereas a resolvable import has nothing to wait for.

#### Drift is surfaced, never fought

The external installation can change under the CR: the admin password is rotated
without updating the referenced Secret, or the admin user is deleted and
recreated. K-ORC imports are **resolve-once** — a resolved `status.id` is never
re-resolved — so after a recreate the import stays `Available=True` with a stale
id while an application-credential create against that id yields a Keystone
**403** (`identity:create_application_credential`; Keystone's default policy
allows creating an application credential only for the token's own user).

The operator **never remediates the external installation**. It signals drift on
the existing sub-conditions with the documented `CredentialDrift` reason, and
`reconcileKORC` emits a `Warning` `CredentialDrift` event on the **transition**
into the drifted state (not on every 10s requeue). The remedy is to update the
Secret `spec.korc.adminCredential.passwordSecretRef` names, which changes its
digest and drives a hash-driven re-mint.

> **Hard CRD dependency.** K-ORC (like Memcached, ESO, MariaDB, and Keystone) is
> a hard dependency: `SetupWithManager` `Owns`/`Watches` its kinds, so the
> manager fails fast at startup if any CRD is absent. A missing K-ORC CRD never
> reaches the reconcile path, so there is no dedicated CRD-not-installed
> condition; a no-match error that could only occur if a CRD were deleted after
> startup propagates as a hard error (`ApplicationCredentialError` /
> `ServiceError`) and the manager requeues with backoff.

### reconcileAdminCredential

| Aspect | Value |
| --- | --- |
| File | `reconcile_admincredential.go` |
| Condition | `AdminCredentialReady` |
| Gate | `KORCReady == True`, the store selected via `spec.secretStoreRef` (default the OpenBao-backed cluster store `openbao-cluster-store`) is Ready, the K-ORC clouds.yaml `ExternalSecret` (`{childNamespace(cp)}/{CloudCredentialsRef.SecretName}`, co-located with the K-ORC CRs per C1) is Ready, the admin app-credential `PushSecret` has actually synced to OpenBao (its `Ready` condition is True), **and** the materialised clouds.yaml Secret semantically matches (parsed application-credential id+secret) the freshly assembled credential |
| Owns | the operator-owned `Secret` `{controlplane.Name}-admin-app-credential` and the `PushSecret` `{controlplane.Name}-admin-app-credential-backup`, both in `childNamespace(cp)` |
| Requeue | `korcRequeueAfter` = **10s** while any gate is unmet (including a stale/absent materialised clouds.yaml) |

`reconcileAdminCredential` commits the minted credential and mirrors it to
OpenBao:

- **Clobber-safe operator Secret.** The Secret K-ORC writes the minted
  credential into is ensured by the operator, but the `CreateOrUpdate` mutate
  closure **never touches `secret.Data`** — only the owner reference. K-ORC owns
  the data, so a reconcile can never overwrite a freshly minted credential.
- **clouds.yaml gate.** Readiness is checked via
  `secrets.WaitForExternalSecret(childNamespace(cp)/CloudCredentialsRef.SecretName)`
  so the credential is never published before K-ORC can actually authenticate.
  The Secret is co-located with the K-ORC CRs (C1) because K-ORC resolves
  `CloudCredentialsRef` in the resource's own namespace; on a fresh cluster
  `reconcileKORC` itself seeds a password-based bootstrap clouds.yaml into the
  `{controlplane.Name}-admin-app-credential` Secret (`seedBootstrapCloudsYAML`,
  write-if-empty) and the PushSecret mirrors it to the per-ControlPlane
  OpenBao path, so the operator-created per-CR ExternalSecret can materialise
  before any credential is minted — once the AC is minted the PushSecret carries
  the minted credential-based clouds.yaml instead.
- **PushSecret to OpenBao.** `secrets.EnsurePushSecret` (applied via server-side
  apply under a fixed field manager that owns only the fields the operator sets,
  so repeated applies of an unchanged desired spec are no-ops at the API server)
  builds the PushSecret to the selected store (default `openbao-cluster-store`;
  its store ref comes from `spec.secretStoreRef` via `secrets.PushSecretStoreRefs`,
  and switching the ref moves the push in place — unchanged name and remote key) at
  the per-ControlPlane remote
  key `openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential`
  (`adminAppCredentialRemoteKeyFor`) with **`DeletionPolicy: None`** — the
  admin credential is a per-ControlPlane persistent bootstrap secret, so deleting
  the PushSecret on ControlPlane teardown (or when rotation is disabled) leaves the
  last-pushed credential intact in OpenBao at that CR's own path, so re-adoption
  works and the admin is never locked out.
- **Forced re-push on credential change.** ESO's PushSecret controller does
  **not** watch its source Secret: its refresh gate reacts only to the PushSecret
  object's own label/annotation hash, so a source-Secret update — e.g. the
  fresh-create handoff from the password-based bootstrap clouds.yaml to the
  minted credential — would otherwise not reach OpenBao until the hourly
  `refreshInterval`, leaving `AdminCredentialReady` stuck False for up to an
  hour. `forceRepushAdminAppCredential` therefore stamps the assembled
  clouds.yaml content hash onto the PushSecret (`c5c3.io/push-content-hash`,
  read-modify-write under `RetryOnConflict`), changing its metadata hash and
  forcing an immediate re-push. The annotation is keyed by the content hash, so
  it fires exactly once per credential change and a steady-state pass is a
  no-op.
- **PushSecret sync gate.** `AdminCredentialReady` is gated on the PushSecret's
  `Ready` condition — not merely on the CR existing — so a backend permission
  failure (e.g. the ESO role missing the push policy) surfaces as
  `WaitingForPushSecret` instead of a false-positive Ready while OpenBao still
  serves the password-based bootstrap clouds.yaml.
- **Live clouds.yaml gate (stale-credential window).** A re-mint revokes the old
  credential immediately, but the `k-orc-clouds-yaml` Secret only refreshes from
  OpenBao at the ExternalSecret's hourly `refreshInterval`, so the PushSecret-Ready
  check above can pass while the materialised Secret K-ORC actually authenticates
  with still holds the revoked credential. After assembling the clouds.yaml,
  `reconcileAdminCredential` stamps the `external-secrets.io/force-sync` annotation
  to nudge ESO to re-materialise immediately. The trigger value is
  `contentHash + "/" + PushSecret.status.syncedResourceVersion`, **not** the
  content hash alone: both nudges (the PushSecret re-push above and this
  force-sync) are stamped in the same reconcile pass, but ESO processes the two
  objects independently — keyed on the content hash alone, the ExternalSecret
  refresh can read OpenBao *before* the re-push has written it, re-materialise
  the stale bootstrap document, and (with the annotation already at its final
  value) never be nudged again, wedging `AdminCredentialReady` at
  `WaitingForCloudsYamlSync` until the hourly refresh. ESO bumps
  `syncedResourceVersion` only *after* a completed push, so folding it in
  re-nudges the ExternalSecret exactly once more as soon as the re-push lands;
  both inputs are stable once converged, so a steady-state pass still leaves the
  ExternalSecret untouched. It then **compares the
  materialised Secret semantically** — by the parsed application-credential id and
  secret, not byte-for-byte, so a benign ESO/OpenBao re-serialisation (a stripped
  trailing newline, reordered keys, requoting) cannot wedge the gate permanently —
  and only reports `AdminCredentialReady=True` when they match. The semantic
  compare — not the best-effort force-sync — is the correctness guarantee: the
  condition never reads True against a stale credential. A sync that never converges
  is bounded: once the materialised Secret has failed to match for longer than
  `cloudsYamlSyncStuckTimeout` (measured from the credential's `LastRotation`), the
  reason escalates from the transient `WaitingForCloudsYamlSync` to the alertable
  `CloudsYamlSyncStuck`, so a permanently broken sync is distinguishable from a
  2-second transient miss.
- **Drift escalation (External mode).** When the `KORCReady` gate is closed
  *because* the external Keystone reports drift, the gate reports
  `CredentialDrift` rather than the opaque `WaitingForKORC`. Note that the **live**
  drift signal is `KORCReady` itself: `reconcileKORC` writes the drifted
  `KORCReady` earlier in the pass, and because both run inside the
  non-short-circuiting tail group, `reconcileAdminCredential` runs later in the
  **same** pass and observes it directly — so this `CredentialDrift` escalation is
  the live same-pass path that keeps `AdminCredentialReady` honest, not merely a
  defense-in-depth fallback. An unreachable endpoint, a TLS failure, a catalog mismatch
  or a stalled import are **not** drift and keep `WaitingForKORC` — drift is a
  statement about the credential.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `KORCReady` not True | False | `WaitingForKORC` | requeue 10s |
| `KORCReady` reports drift (`AuthenticationFailed` / `CredentialDrift`), External mode | False | `CredentialDrift` | requeue 10s; names the Secret the operator reads and states that the external installation is never remediated |
| Selected secret store not Ready | False | `SecretStoreNotReady` | requeue 10s; checked after the `KORCReady` gate so an OpenBao/ESO outage surfaces before the clouds.yaml wait. The message names the store's kind and name |
| Selected secret store check errors | False | `SecretStoreError` | returns the error; stamped rather than returned bare so the peers gating on `AdminCredentialReady` later in the tail group cannot read the previous pass's `True` |
| clouds.yaml ES check errors | False | `CloudsYamlError` | returns the error (also covers a force-sync/materialised-Secret read error) |
| clouds.yaml ES not Ready | False | `WaitingForCloudsYaml` | requeue 10s |
| operator Secret ensure fails | False | `SecretError` | returns the error |
| PushSecret ensure, re-push stamp, or readback fails | False | `PushSecretError` | returns the error |
| PushSecret not yet synced to OpenBao | False | `WaitingForPushSecret` | requeue 10s; the PushSecret's `Ready` condition is not True — the assembled credential has not landed in OpenBao yet |
| materialised clouds.yaml absent or semantically stale | False | `WaitingForCloudsYamlSync` | requeue 10s; force-sync annotation stamped, semantic compare (parsed id+secret) against the assembled document not yet satisfied |
| materialised clouds.yaml stuck stale past `cloudsYamlSyncStuckTimeout` | False | `CloudsYamlSyncStuck` | requeue 10s; the sync has not converged since `LastRotation` — alertable, distinguishable from a transient miss |
| committed, mirrored, and materialised | True | `AdminCredentialReady` | — |

### reconcileCatalog

| Aspect | Value |
| --- | --- |
| File | `reconcile_catalog.go` (Managed), `reconcile_catalog_external.go` (External) |
| Condition | `CatalogReady` |
| Gate | `AdminCredentialReady == True`, **and** every catalog child reports `Available` |
| Owns | Managed mode: a K-ORC identity `Service` (`{controlplane.Name}-identity-service`) and its public `Endpoint` (`{controlplane.Name}-identity-endpoint`). That is the whole managed table: a built-in service's catalog row belongs to the `KeystoneService` registration projected for it, not here. External mode: the same identity `Service` plus one `Endpoint` per interface (`{controlplane.Name}-identity-endpoint-{interface}`), all unmanaged imports, and nothing managed. All in `childNamespace(cp)` |
| Requeue | `korcRequeueAfter` = **10s** while gated, while a child is not yet Available, or on a terminal K-ORC failure |

`reconcileCatalog` drives `CatalogReady`. Everything up to and including the
`AdminCredentialReady` gate and the admin `CloudCredentialsReference` is
mode-agnostic; below it the two postures are opposites, so the reconciler forks
on `cp.IsExternalKeystone()`. K-ORC is a hard CRD dependency (see the note
above), so a missing Service/Endpoint CRD never reaches this path and there is no
CRD-not-installed condition.

#### Managed mode — the control plane owns the catalog

`reconcileCatalog` registers the OpenStack service-catalog entries the control
plane itself owns as owned K-ORC CRs, driven from a per-service table
(`managedCatalogRows`). That table holds exactly **one** row, the identity
(Keystone) service: an `identity`-type `Service` named `keystone`, plus a
`public` `Endpoint` whose URL defaults to the conventional in-cluster identity URL
`http://keystone.<namespace>.svc:5000/v3` and whose `serviceRef` points at the
identity Service. The child is projected idempotently via Server-Side Apply under
the shared field manager (`cobaltcore-operator`).

The rows for the built-in services are **not** here. Glance's `image` row,
Placement's `placement` row and Barbican's `key-manager` row each belong to the
`KeystoneService` registration their service leg projects, which owns the row's
K-ORC children and their teardown; see
[Built-in service registrations](#built-in-service-registrations) for what each
one advertises. The identity row stays ControlPlane-owned in both modes because
nothing registers it on the plane's behalf: it is the entry K-ORC authenticates
through.

The identity row registers a `public` interface only, so it needs none of the
target-cluster switching a placed service's row does. Its `Service`/`Endpoint` CRs
stay in `childNamespace(cp)` on the management cluster: they are K-ORC's to
reconcile, and K-ORC runs there. See
[Reaching a placed service](#reaching-a-placed-service).

No managed row carries a region: `managedCatalogService` and
`managedCatalogEndpoint` set the management policy, the credentials ref, and the
resource block (type/name/enabled, and interface/URL/serviceRef), and nothing
else. The region every client filters on comes from the `clouds.yaml`
`region_name` the admin credential renders from `spec.region`. The registration
children carry no region either, for the same upstream reason: K-ORC's
`EndpointResourceSpec` has no region field.

Registering the child CRs only instructs K-ORC to create the catalog entries — it
does not mean they exist in Keystone — so `CatalogReady` is gated on both children
reporting `Available` for their current generation (`korcAvailableUpToDate`, which
refuses a stale `Available` condition whose `ObservedGeneration` lags the object —
the same generation gate `GetTerminalError` already applies via its `Progressing`
check — so an endpoint/region edit that moves the catalog URL cannot flip
`CatalogReady` True before K-ORC re-reconciles the new value), and a terminal K-ORC
failure (`GetTerminalError`, the documented wrong-endpoint / import-stuck class) is
surfaced as the distinct `CatalogFailed` reason instead of a false-positive Ready.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `AdminCredentialReady` not True | False | `WaitingForAdminCredential` | requeue 10s |
| Service create/update fails | False | `ServiceError` | returns the error |
| Endpoint create/update fails | False | `EndpointError` | returns the error |
| a catalog entry's Service/Endpoint reports a terminal K-ORC error | False | `CatalogFailed` | requeue 10s (Service before its Endpoints, so the root stuck dependency surfaces) |
| a catalog entry's Service/Endpoint registered but not yet Available | False | `WaitingForCatalog` | requeue 10s |
| every catalog entry registered and Available | True | `CatalogRegistered` | the message counts the registered entries, which is the identity row alone today; the table is what a future ControlPlane-owned row would be added to |

#### External mode — import-first

The catalog belongs to the pre-existing installation. Keystone enforces no
uniqueness on service names, so registering an identity `Service` against a
populated catalog would silently duplicate rows. `reconcileCatalogExternal`
therefore **imports** instead: the identity `Service` and each of its three
endpoint interfaces become K-ORC CRs with `managementPolicy: unmanaged`, an
import filter, and no desired resource. K-ORC resolves them read-only and writes
nothing; deleting their CRs later removes only the Kubernetes objects.

The ControlPlane creates **zero** catalog entries in External mode: this branch
of `reconcileCatalog` is import-only. A genuinely new row is declared with a `KeystoneService` CR, whose
own reconciler creates it, gates its own `CatalogReady` on it, and deletes it at
that CR's teardown — so nothing the ControlPlane writes can duplicate or outlive
a row in a catalog it does not own.

`status.catalog.imports` is rebuilt before any failure return, so an unresolved
import is visible as `resolved: false` rather than omitted.

**Import-first inverts the failure modes, and detecting them is the point.** A
K-ORC import that matches **nothing** does not error: it waits indefinitely on
`Available=False`, `reason=Progressing`, *"Waiting for OpenStack resource to be
created externally"* — by conditions indistinguishable from a resource that is
about to appear. For a **gating** import the target pre-exists **by definition**,
so past `externalImportStallGrace` (2m) that wait is a misconfiguration signal,
not a wait. An import that matches **several** entries is terminal in K-ORC
itself, which refuses to guess and stops retrying.

Only two of the four imports gate `CatalogReady`: the identity `Service`, and the
`Endpoint` of the interface `external.endpointType` selects. The control plane
already authenticates through that interface, so a catalog that does not publish
it is not the catalog K-ORC was pointed at. The other two interfaces are imported
for visibility, projected into `status.catalog.imports`, and may stall forever —
or resolve ambiguously — without failing the condition. An external installation
is free not to publish an interface, and free to publish it once per region (which
K-ORC's region-less `EndpointFilter` cannot select among, so no spec edit repairs
it); both are precisely the brownfield posture External mode adopts.

The precedence below reports the most specific cause first, and the `Service`
before the `Endpoint`s so the **root** stuck dependency is named rather than an
endpoint merely blocked on the service it references:

| # | Path | Status | Reason | Notes |
| --- | --- | --- | --- | --- |
| — | `AdminCredentialReady` not True | False | `WaitingForAdminCredential` | requeue 10s; no import CR is reconciled |
| — | import create/update fails | False | `ImportError` | returns the error |
| 1 | an **unresolved** import carries a classifiable K-ORC message | False | `AuthenticationFailed` \| `EndpointUnreachable` \| `TLSVerificationFailed` \| `CatalogEndpointMismatch` \| `CredentialDrift` | requeue 10s; K-ORC's message is relayed verbatim. A resolved import is never re-classified — K-ORC leaves the last transient attempt's message on `Progressing`, and classifying it would flip a converged catalog to a failure it has recovered from |
| 2 | an import reports a terminal K-ORC error | False | `CatalogFailed` | requeue 10s; gating or not — K-ORC has given up on it. On the **>1-match** message the hint names `external.catalog.identityServiceName` for the `Service` import, or the region limitation for an `Endpoint` import (K-ORC's `EndpointFilter` carries no region, so no spec field can select among per-region rows). **One exception:** an `InvalidConfiguration` on a **non-gating** import does not fail the condition. A non-gating import has no user-supplied configuration to fix — its filter is entirely operator-derived — so it has no remediation and nothing depends on it; it is tolerated exactly like the 0-match of row 3 and reported as `resolved: false`. The exception is keyed on K-ORC's machine-readable reason, never on the >1-match message text: keying it on the text would turn a K-ORC rewording into a permanent `CatalogReady=False`. An `UnrecoverableError` gates on every import, and so does any terminal error on a gating one |
| 3 | a **gating** import stalled past `externalImportStallGrace` | False | `ImportStalled` | requeue 10s; the **0-match** case. The message names the stuck import, the `authURL`, and `external.endpointType` / `spec.region` — plus, for an `Endpoint` import, that the external catalog may publish no such interface |
| 4 | a **gating** import is unresolved | False | `WaitingForCatalog` | requeue 10s; the bounded, legitimate wait |
| 5 | every gating import resolved | True | `CatalogImported` | the message reports how many of the three endpoint interfaces resolved |

`publicEndpoint` is forbidden in External mode, so `keystoneCatalogURL` — the URL
the Managed branch registers — is never consulted here: advertisement visibility
is owned by the imports.

> **Promote-to-managed is reserved, not implemented.** Turning an import into a
> managed row (to edit its endpoint URL declaratively) is a later phase.
> K-ORC's `managementPolicy` is CEL-immutable, so it will have to be a
> delete-and-recreate of the import CR. Nothing in the deterministic CR names or
> the spec-derived filters chosen here precludes that.

### reconcileServiceAccounts

| Aspect | Value |
| --- | --- |
| File | `reconcile_serviceaccounts.go` |
| Condition | `ServiceAccountsReady` |
| Gate | none — the member only reads the `KeystoneService` children the built-in service legs wrote earlier in the same pass, so there is no projection it could defer |
| Requeue | `korcRequeueAfter` = **10s** while a registration is not yet Ready |

`reconcileServiceAccounts` **aggregates**: it reads the `KeystoneService`
registration child each enabled built-in service leg projects
(`services.glance`, `services.placement`, `services.barbican`,
`services.neutron`, in that order) and folds their readiness into the one
condition operators alert on. It projects no OpenStack
resource itself; the registration CR owns the Keystone user, its project, its role
assignments, the generation-scoped password, and the OpenBao round-trip that
delivers the credentials.

The double reporting is intended. A failing registration already fails its own
service condition (`GlanceReady`, `PlacementReady`, `BarbicanReady`,
`NeutronReady`); the
aggregate names the same cause under the condition type that does not depend on
knowing which service broke.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| no built-in service block is declared | True | `NoServiceRegistrationsProjected` | every External-mode ControlPlane; set rather than omitted so the condition schema does not depend on the spec |
| a registration child is absent | False | `WaitingForServiceRegistration` | its service leg is gated on something upstream of its own apply; requeue 10s |
| reading a registration child fails | False | `ServiceRegistrationError` | returns the error, wrapped with which child was being read |
| a registration reports a `False` sub-condition | False | *(the child's own reason)* | relayed verbatim with the child's message, prefixed by which child it came from; requeue 10s |
| a registration has not reported `Ready` yet | False | `WaitingForServiceRegistration` | requeue 10s |
| every registration reports `Ready` | True | `ServiceAccountsProvisioned` | the message counts them |

**The relayed reason is the child's first failing sub-condition, not the
aggregate's.** `keystoneServiceSubConditionTypes` orders `CatalogReady` before
`AccountReady`, so a registration whose Keystone catalog row failed reports a
catalog reason (`CatalogFailed`, `ServiceCollision`) under a condition
named for service accounts. That is deliberate: `NotAllReady` says only that
something is wrong, while the sub-condition's own reason says what and admits a
fix.

**Consumption contract.** Consumers read the credentials from the Secret the
registration materializes in its own namespace — `{keystoneservice.Name}-credentials`,
keys `password` and a ready-to-use `clouds.yaml` — named in the registration's
`status.account.secretName`. For which OpenBao paths exist in each Keystone mode,
see [OpenBao paths per ControlPlane
mode](../infrastructure/openbao-bootstrap.md#openbao-paths-per-controlplane-mode).

**Deletion.** The registrations go first at teardown: `reconcileDelete` deletes
the projected `KeystoneService` children and waits for them, because their K-ORC
CRs belong to the registration and its controller tears them down through the
admin credential the next step revokes.

### reconcileRegistrationTenantStores

| Aspect | Value |
| --- | --- |
| File | `reconcile_esotenant.go` |
| Condition | `RegistrationTenantStoresReady` |
| Gate | none — it consumes no condition this chain produces; the trio it writes depends on cert-manager and OpenBao alone |
| Projects / Owns | the same trio [`reconcileESOTenantStore`](#reconcileesotenantstore) writes (a `ServiceAccount` `eso-tenant-auth`, a cert-manager mTLS `Certificate` `eso-tenant-client-tls`, and a namespaced `SecretStore` `openbao-tenant-store`), one per **allowlisted** namespace hosting at least one `KeystoneService`; nothing when `spec.secretStoreRef` overrides the default |
| Requeue | `esoTenantStoreRequeueAfter` = **10s** while a store is not yet Ready, and on a provisioning failure |

Credential delivery is namespace-local. A registration's consumer Secret is
materialized through the tenant store in the CR's **own** namespace, and
`reconcileESOTenantStore` provisions one only in the namespaces the ControlPlane
occupies. Without this member an allowlisted foreign registration would mint its
Keystone account and then wait forever on a store nobody creates.

It shares `ensureESOTenantStoreTrioIn` with its blocking-prefix twin, so the
ownership mechanism, the adoption refusal and the error wording are written once.
The OpenBao side needs nothing: the `eso-tenant` role binds the ServiceAccount
name in **any** namespace and its templated policy confines each token to its own
paths, so a trio in a foreign namespace authenticates as that namespace's tenant
identity and reaches only its own paths.

**Why it is a member of its own, at the end of the group.** The tenant-store step
runs in the chain's blocking prefix, where a non-zero result parks DBCredentials,
AdminPassword and Keystone behind it. These namespaces are not the operator's — a
foreign object can occupy the store's name, a certificate can fail to issue there —
and none of that may reach the plane's core reconciliation. A failure here
surfaces in this condition, and through it in the aggregate `Ready`, and stops.
For the same reason a provisioning failure **requeues** rather than returning an
error: the cause is usually a tenant-side one no backoff resolves, and returning
an error would put the whole ControlPlane reconcile into exponential backoff for
it. Every namespace is attempted even after one fails, so a single broken
namespace cannot starve its peers, and the failures are reported together.

**Collecting, and the one namespace it will not collect.** The namespaces already
provisioned are enumerated by ownership label **before** the allowlist is
consulted, so emptying the allowlist still collects the trios whose registrations
have left. A namespace that no longer appears in the allowlist but still holds
registrations is **frozen, not collected**: de-listing is an admission gate, and
revoking the store under a running service would destroy credentials it depends
on. See [ServiceRegistrationsSpec](./controlplane-crd.md#serviceregistrationsspec).

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.secretStoreRef` set (override) | True | `StoreRefOverridden` | nothing to provision and nothing this member created standing to collect; a registration resolves that store in its own namespace itself |
| no allowlisted namespace hosts a registration | True | `NoRegistrationNamespaces` | both arms share one message: nothing admitted at all, and an allowlist no registration sits in. Returns before the registration List, so a ControlPlane that never uses the feature never depends on the KeystoneService field index |
| provisioning or collecting a trio failed | False | `ProvisioningError` | requeue 10s, no error returned; the message names the failing namespaces and the joined error. A failed List while enumerating the provisioned namespaces or counting registrations sets the same reason **and** returns the error |
| a store is written but not yet Ready | False | `SecretStoreNotReady` | requeue 10s, naming the namespace; the delivery leg gates on exactly this, so reporting True early would claim a path that does not carry yet. A failed readiness read returns the error with no condition written, leaving the previous value standing |
| every store Ready | True | `RegistrationTenantStoresReady` | the message counts the namespaces |

### CredentialRotation reconciler

| Aspect | Value |
| --- | --- |
| File | `reconcile_credentialrotation.go` |
| `For()` | `CredentialRotation` |
| Condition | `Ready` (`conditionTypeRotationReady`) |
| Owns / mints | **nothing** — it never mints |
| Requeue | `credentialRotationWaitInterval` = **10s** while waiting for the ControlPlane reconciler or for a dependency to appear |

The `CredentialRotationReconciler` drives one-shot rotations of a control-plane
credential by **nudging** the owning reconciler rather than duplicating any
mint logic. It **dispatches on `spec.target`**: `adminApplicationCredential`
nudges the admin AC (clearing the AC's password-hash annotation), and
`serviceAccountPassword` nudges the managed account of the referenced
`KeystoneService` (clearing the managed `User`'s
`cobaltcore.c5c3.io/password-generation` annotation, which the KeystoneService
controller's `ensureManagedAccountUser` consumes by bumping the password
generation on its next pass). Its model:

- **Nudge, never mint or delete.** For the admin target it **clears** (zeroes)
  the `cobaltcore.c5c3.io/admin-password-hash` annotation on the owned AC CR via
  `clearPasswordHashAnnotation` (a no-op `Update` when already empty). On its next
  pass `reconcileKORC` observes the mismatch and performs the delete+recreate
  re-mint, re-stamping the fresh hash. The `serviceAccountPassword` target is the
  same discipline against the managed `User`: it resolves the `KeystoneService`
  named by `spec.keystoneService` in the CredentialRotation's **own** namespace
  (`KeystoneServiceNotFound` when it is absent), requires that CR's `spec.account`
  block (`NoAccountDeclared` otherwise), and resolves the ControlPlane through the
  registration's `controlPlaneRef` (`ControlPlaneNotFound` when that dangles).
  Each of the three is a `Ready=False` wait with the 10s requeue. Because that
  reference crosses namespaces and the caller controls it, the path then repeats
  **all three** pre-write gates the KeystoneService reconciler applies before it
  touches the account: the plane's registration consent for the CR's namespace
  (`NamespaceNotAllowed` — de-listing a namespace *freezes* its registrations, so
  the `User` is still there to nudge while the reconciler that would act on the
  nudge no longer runs), the plane's `AdminCredentialReady` condition
  (`WaitingForAdminCredential` — K-ORC cannot reach Keystone before the admin
  credential is minted, which freezes the registration the same way), and the
  ownership labels on the `User` itself (`ForeignServiceAccount` — a
  cross-namespace child carries no owner reference, so the derived name is the
  only thing tying it to the registration). Each is refused **before** the nudge,
  so the generation stays unlatched and the rotation still fires once the gate
  opens. A
  `CredentialRotation` that predates `spec.keystoneService` decodes with an empty
  name and finishes `Ready=False` reason `MissingKeystoneService` with **no**
  requeue, naming the removed `spec.serviceAccount` field rather than reporting a
  dangling reference to a `KeystoneService` nobody wrote. Because there is
  **no external password source to observe**, the target has no auto-detect path,
  so a rotation fires only on an explicit `reMint` (latched to the spec
  generation). Keeping the resource lifecycle owned solely by the reconciler that
  owns the object (the ControlPlane reconciler for the admin AC, the
  KeystoneService reconciler for the managed account) avoids two controllers
  racing on the same object.
- **`reMint` is one-shot per spec generation.** An explicit `spec.reMint` is
  **latched** on `status.lastTriggeredGeneration`: the reconciler nudges only while
  it differs from `metadata.generation`, then records the generation. A `reMint:
  true` left in the spec therefore fires the nudge **once per edit**, not on every
  cache resync (~10 min via the shared `SyncPeriod`) or operator restart — without
  the latch it would revoke + re-mint the admin credential indefinitely, re-opening
  the stale-credential window each cycle. A pass over an already-latched generation
  reports `NoRotationNeeded`. The auto-detect (password-hash change) path is **not**
  latched: it is self-limiting (it stops once the hash matches) and relies on resync
  to observe an out-of-band password rotation.
- **ControlPlane resolution (one-per-namespace).** On the
  `adminApplicationCredential` target a `CredentialRotation` carries no explicit
  ControlPlane reference, so `resolveControlPlane` lists ControlPlanes in the
  CredentialRotation's **own** namespace and requires exactly one. Zero →
  `Ready=False` reason `NoControlPlane` with a short requeue;
  multiple → `Ready=False` reason `AmbiguousControlPlane` with **no** requeue (an
  arbitrary pick could rotate the wrong credential). The one-ControlPlane-per-namespace
  contract is now enforced at admission by the ControlPlane validating webhook
  (`validateUniqueInNamespace`), so the `AmbiguousControlPlane` branch is
  **defense-in-depth**: it is retained as a safety fallback but is unreachable while
  the webhook is active. The `serviceAccountPassword` target does not use this
  lookup. It reaches its ControlPlane through the referenced KeystoneService's
  `controlPlaneRef`, so it works from a namespace that holds no ControlPlane.
- **Bootstrap is idempotent.** With `spec.bootstrap`, an already-existing AC is a
  no-op success (`BootstrapComplete`); a missing AC waits (`WaitingForBootstrap`)
  for the ControlPlane reconciler to mint it.
- **Scheduled fields are read-but-ignored.** `intervalDays` / `preRotationDays`
  / `gracePeriodDays` are accepted but deferred to a later level; when set, an
  informational `ScheduledRotationDeferred` event is emitted but **no** loop runs
  and **no** error is raised.
- **Target enum.** `adminApplicationCredential` and `serviceAccountPassword` are
  supported; any other target finishes `Ready=False` reason
  `UnsupportedTarget`.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| target neither `adminApplicationCredential` nor `serviceAccountPassword` | False | `UnsupportedTarget` | no requeue |
| no ControlPlane in namespace | False | `NoControlPlane` | requeue 10s |
| multiple ControlPlanes | False | `AmbiguousControlPlane` | no requeue; defense-in-depth — unreachable while the one-per-namespace webhook is active |
| ControlPlane List errors | False | `ControlPlaneListError` | no requeue |
| bootstrap, AC exists | True | `BootstrapComplete` | no-op success |
| bootstrap, AC absent | False | `WaitingForBootstrap` | requeue 10s |
| rotation, AC absent | False | `WaitingForApplicationCredential` | requeue 10s |
| admin password not yet readable | False | `WaitingForAdminPassword` | requeue 10s |
| hash unchanged, no pending `reMint` (incl. a `reMint` already latched for this generation) | True | `NoRotationNeeded` | nothing to do |
| nudge performed | True | `RotationTriggered` | emits `RotationNudged` event; an explicit `reMint` latches `status.lastTriggeredGeneration` |

The CredentialRotation reconciler is registered with the manager via a plain
`For(&CredentialRotation{})` — it owns no children and registers no watches or
field indexers.

---

## K-ORC admin credential chain

The end-to-end path that delivers the admin application credential to the K-ORC
controller spans three sub-reconcilers and the ESO/OpenBao backend:

```text
OpenBao kv  bootstrap/{cp.Namespace}/{cp.Name}-keystone/admin   (admin password)
        │  (managed mode; reconcileAdminPassword, owner-ref'd to the ControlPlane)
        ▼
ExternalSecret  →  {control-plane ns}/{controlplane.Name}-keystone-admin-credentials
        │            (ESO owns the materialised Secret; CreationPolicy: Owner)
        ▼
admin-password Secret (the effective admin-password ref; read by c5c3-operator)
        │  SHA-256 → cobaltcore.c5c3.io/admin-password-hash annotation
        ▼
c5c3-operator mints a RESTRICTED ApplicationCredential        (reconcileKORC)
   restricted:true  ⇒  K-ORC spec.resource.unrestricted=false  (INVERSION)
        │
        ▼
K-ORC writes the minted credential into the operator-owned Secret
   {controlplane.Name}-admin-app-credential   (Resource.SecretRef target)
        │
        ▼  (reconcileAdminCredential, gated on KORCReady + clouds.yaml ES)
PushSecret  →  OpenBao kv  openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential
   (DeletionPolicy: None — per-ControlPlane bootstrap secret survives teardown;
    ESO does not watch the source Secret, so a content-hash annotation nudge
    forces the re-push on credential change, and the clouds.yaml force-sync
    below is keyed on that hash + the completed push's syncedResourceVersion)
        │
        ▼
ExternalSecret  →  {control-plane ns}/k-orc-clouds-yaml  (the clouds.yaml gate;
        │            operator-created per-CR by reconcileKORC, owner-ref'd to
        │            the ControlPlane; the orc-system copy is the retained
        │            STATIC manifest for K-ORC's global mount)
        ▼
K-ORC controller authenticates with the admin clouds.yaml and reconciles
   the catalog Service + Endpoint                              (reconcileCatalog)
```

**Re-mint trigger.** A rotation is signalled by comparing
`SHA-256(admin password)` against the `cobaltcore.c5c3.io/admin-password-hash`
annotation last stamped on the AC CR. `reconcileKORC` re-stamps and re-mints when
they differ; the CredentialRotation reconciler forces the same path by clearing
the annotation (which guarantees a mismatch). The admin-password Secret watch
(see [Secret Field Indexer](#secret-field-indexer)) wakes the ControlPlane the
moment the password rotates so the chain converges without waiting for the next
periodic requeue.

---

## Multi-instance

The ControlPlane reconciler scopes every admin / K-ORC credential it owns to
the individual ControlPlane CR, so multiple control planes can coexist in a single
cluster without sharing OpenBao state.

- **One ControlPlane per namespace (admission-enforced).** The ControlPlane
  validating webhook's `validateUniqueInNamespace` check runs in
  `ValidateCreate` **only** (not `ValidateUpdate`): it lists ControlPlanes in the
  object's namespace through the uncached API reader and returns
  `field.Forbidden` naming the incumbent if one already exists. The cluster
  therefore admits **exactly one** ControlPlane per namespace. This is what
  makes the CredentialRotation reconciler's `AmbiguousControlPlane` branch
  defense-in-depth (see
  [CredentialRotation reconciler](#credentialrotation-reconciler)).
- **Duplicate guard (reconciler-enforced).** As defense-in-depth for CRs that
  predate the webhook guard, raced through the API server, or were written with
  the webhook bypassed, `Reconcile` runs `duplicateControlPlaneIncumbent`
  before the sub-reconciler chain: it lists ControlPlanes in the CR's namespace
  and parks every CR except the oldest (by `creationTimestamp`, lexically
  smallest name breaking ties). A parked duplicate gets `Ready=False` with
  reason `DuplicateControlPlane` naming the incumbent, runs **no**
  sub-reconcilers, and requeues every 30s — so it takes over automatically once
  the incumbent is fully deleted (no watch event fires on the duplicate's
  behalf when that happens).
- **Per-CR OpenBao path for the admin AC.** The admin application credential is
  pushed to the per-ControlPlane key
  `openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential`
  (`adminAppCredentialRemoteKeyFor`), so two ControlPlanes never write to the same
  OpenBao object. The K-ORC admin `User` CR is named `{cp.Name}-user-admin` (its
  Kubernetes `metadata.name`), while the **OpenStack** username it imports stays
  `admin` (set via the import `Filter.Name`) — the K8s-name and the OpenStack-name
  are deliberately split so the per-CR Kubernetes object is unique while the
  OpenStack identity is unchanged.
- **Per-CR OpenBao path for the service DB credential.** The managed-mode service
  database credential is read from the per-ControlPlane key
  `openstack/keystone/{cp.Namespace}/{cp.Name}/db`
  (`dbCredentialRemoteKeyFor`), scoped by **both** namespace and name (mirroring
  `adminAppCredentialRemoteKeyFor`) so two ControlPlanes never collide on the
  cluster-global OpenBao backend. `reconcileDBCredentials`
  projects an owned ExternalSecret reading `username`/`password` from this key.
- **Running multiple control planes.** Because the webhook caps a namespace at one
  ControlPlane and the OpenBao paths are keyed by `{namespace}/{name}`, operating
  several control planes means deploying each into its **own** namespace. Each gets
  a disjoint OpenBao prefix
  (`openstack/keystone/{namespace}/{name}/admin/app-credential`), disjoint child CRs
  in its own namespace, and an independent rotation lifecycle — no two control
  planes can clobber one another's credentials.

See [Migration: legacy flat paths → per-ControlPlane paths](#migration-legacy-flat-paths--per-controlplane-paths)
for moving an existing single-instance cluster onto the per-CR layout.

---

## Owner-ref / GC model

A child CR created in **the ControlPlane's own namespace** carries an owner
reference to the ControlPlane CR via `controllerutil.SetControllerReference()`.
This enables both **automatic garbage collection** (deleting the ControlPlane
cascades to its children) and **watch-based reconciliation** (a child change
re-reconciles the owner).

A child created in a **service namespace** — one a
[`namespace` assignment](./controlplane-crd.md#service-namespaces) places
elsewhere — can carry **no owner reference**: Kubernetes garbage collection only
cascades within one namespace, so the API server rejects a cross-namespace
controller reference. Such a child is stamped with two **ownership labels**
instead — `c5c3.io/controlplane-name` and `c5c3.io/controlplane-namespace`,
which together name the owning ControlPlane. They carry the two jobs an owner
reference would have done: `isControlPlaneChild` recognizes the child (so a
colliding object owned by nobody is never reshaped or deleted), and a
label-mapped `Watches()` leg resolves an event on it back to a reconcile request.
Because nothing garbage-collects such a child, the finalizer tears it down
explicitly (see below).

A child written to a **target cluster** carries no owner reference either, for a
stronger reason: that cluster's garbage collector cannot resolve an owner living
on the management cluster at all. Such a child takes the two labels above plus
the three [shared ownership
labels](../target-clusters.md#ownership-and-teardown-on-the-target)
(`openstack.c5c3.io/owner-kind`, `-owner-name`, `-owner-namespace`), which name
the owning CR across operators, so a Keystone and a ControlPlane projecting into
one target namespace each select only their own. Nothing on that cluster collects
them, and no cascade crosses the boundary, so a second finalizer holds the
ControlPlane in etcd until they are swept.

### Deletion ordering — the `c5c3.io/orc-teardown` finalizer

Owner-reference GC alone is **unordered**: deleting the ControlPlane would
garbage-collect every child at once. That is unsafe for the K-ORC CRs the
operator owns (`ApplicationCredential`, `Service`, `Endpoint`, `User`,
`Domain`). Those CRs carry K-ORC finalizers that call the **Keystone API** to
revoke/delete the credentials and catalog entries they minted; if Keystone (and
in managed mode its MariaDB) were torn down concurrently, the K-ORC finalizers
could never complete and the ControlPlane — and its namespace — would hang
indefinitely on Terminating ORC CRs.

The ControlPlane reconciler therefore installs a single finalizer,
`c5c3.io/orc-teardown`, added on the first reconcile before any K-ORC CR is
projected. On deletion it:

1. **Deletes the projected `KeystoneService` registrations first** and waits for
   them to go. Their K-ORC CRs belong to the registration, not to the
   ControlPlane, so no enumeration in the sweep below names them, and their
   controller tears them down through the very admin credential step 2 revokes.
   While one is still Terminating the reconciler reports `KORCReady=False` with
   reason `FinalizingServiceRegistrations` and requeues; past
   `registrationTeardownStallTimeout` (2 minutes) it continues anyway and a
   **Warning** `ServiceRegistrationTeardownStalled` names what was left behind.
   That window is a budget of its own rather than a share of the K-ORC one: this
   wait blocks the sweep below entirely, so a shared deadline would let an
   unreachable Keystone spend it all here and leave the admin credential's
   revocation with no time at all. A registration still standing when the
   ControlPlane is about to be released, on either path, has the children it
   owns — matched by both their ownership labels and their name prefix — deleted
   first, and the `openstack.k-orc.cloud/*` finalizers of the K-ORC ones stripped
   afterwards, so the owning controller cannot re-add what it just lost. Its
   PushSecrets carry `DeletionPolicy=Delete`, so their deletion is left with ESO
   to purge from OpenBao until `orcTeardownDeadline` — reported as
   `KORCReady=False` with reason `FinalizingORC` while the wait runs — and only
   past it are their finalizers stripped too. That wait is entered only while the
   tenant `SecretStore` the purge authenticates through is still standing: for a
   registration in a dedicated namespace the namespace sweep above already took
   it (`Managed`: the whole namespace), so the finalizers are stripped at once
   rather than holding the namespace `Terminating` for a purge ESO can no longer
   run. A **Warning**
   `ServiceRegistrationResourcesOrphaned` then names the OpenStack resources and
   the OpenBao paths left behind (unmanaged imports are released without being
   listed, the same line step 9 draws); a release that abandoned neither — every
   child was still live, or held nothing yet — reports a **Normal**
   `ServiceRegistrationChildrenReleased` instead of a Warning naming empty sets.
   With its children gone the registration's own controller releases the
   `KeystoneService` CR.
2. **Deletes the owned K-ORC CRs** and holds the ControlPlane CR in etcd.
   Holding the CR defers the owner-reference GC cascade, so Keystone stays
   reachable while K-ORC revokes. While ORC CRs are still Terminating the
   reconciler reports `KORCReady=False` with reason `FinalizingORC` and requeues
   at the K-ORC cadence.
3. **Deletes the owned PushSecrets alongside them.** Their
   `deletionPolicy: Delete` cleanup — ESO deleting the mirrored OpenBao data —
   needs the per-tenant `SecretStore` and its `eso-tenant-auth` ServiceAccount,
   both of which the post-release GC cascade reaps unsequenced. Deleting the
   PushSecrets while the finalizer still holds that infrastructure is what
   keeps the revoked credential from outliving the ControlPlane in OpenBao. A
   PushSecret still stuck past the stall window has its finalizers
   force-removed, and a **Warning** `OpenBaoCleanupStalled` names the OpenBao
   paths that may retain data. A placed namespace is swept on its target cluster
   as well as at home, because both the PushSecrets and the store their purge
   authenticates through live there; the force-remove goes to the cluster the
   PushSecret was found on.
4. **Tears down the cross-namespace children before releasing.** A service
   placed in a namespace of its own — and the backing services, tenant store,
   and credential material that follow it — carries no owner reference, so no GC
   cascade reaches it; releasing the finalizer first would strand every one of
   them. `teardownDedicatedNamespaces` deletes them by hand, in order: the
   service children (`Keystone`/`Horizon`, and for a Barbican placed apart its
   `Barbican`, `BarbicanSecretStore` and `OpenBaoCluster` together) first,
   waiting for them (their operators run a sequenced ESO cleanup through the
   tenant store in the same namespace), then the namespace per its lifecycle. The
   OpenBao instance belongs in that wait set because its own finalizer runs under
   the tenant RBAC living in the same namespace: deleting the namespace first
   reaps that RBAC out from under it and leaves the instance unfinalizable, with
   the namespace stuck `Terminating`. The service CRs live on the management
   cluster wherever their service is placed, so waiting for them here is also
   what waits out the service operators' own remote sweeps. A **`Managed`**
   namespace is
   deleted, which cascades everything left in it — but only when it carries the
   ownership marks, read through that cluster's **uncached** reader because the
   verdict authorises a whole-namespace cascade; one that does not is left
   standing with a **Warning** `NamespaceNotOwned`, because the operator never
   destroys a namespace it did not create. On a target cluster the
   `c5c3.io/controlplane-uid` annotation is read with them, so a namespace whose
   mark names another management cluster's ControlPlane is refused rather than
   cascaded away — while one whose mark was stripped is still reaped, unlike at
   adoption, since nothing else ever comes back for it. A placed one is
   deleted on both clusters, since `reconcileNamespaces` created it on both. An **`External`** namespace survives,
   so its residue (backing
   services, credential material, tenant-store trio last) is swept by name, each
   object ownership-checked so a same-named object belonging to somebody else in
   that shared namespace is left alone. On a placed namespace both of those run
   against that cluster's client, and the
   [label-selected sweep](#the-placed-namespaces--openstackc5c3ioremote-children)
   follows them. While children remain the condition
   reports `NamespacesReady=False/FinalizingNamespaces`; past the
   `orcTeardownDeadline` the sweep stops waiting, emits a **Warning**
   `NamespaceTeardownStalled` naming what is stuck, and releases anyway — a wedged
   child must not make a namespace undeletable forever.
5. **Removes the auth-delegator `ClusterRoleBinding` by hand.** A dedicated
   Barbican secret store places one cluster-scoped object,
   `{instance}-{hash}-auth-delegator`. A namespaced owner cannot own it, so no GC
   cascade collects it, and the per-namespace sweep in step 3 reaches it only
   when Barbican was placed in a namespace of its own. `deleteBarbicanAuthDelegatorBinding`
   therefore runs on every ControlPlane teardown, Barbican or not, reading through
   the uncached API reader (a cached read would install a cluster-wide
   `ClusterRoleBinding` informer for one object) and deleting only a binding
   carrying this ControlPlane's ownership labels. It reads and deletes on the
   cluster Barbican was placed on, where the ensemble wrote the binding. A delete
   that cluster denies — its access chart grants the write verbs only behind
   `authDelegatorBinding` — leaves the binding standing with a **Warning**
   `AuthDelegatorBindingNotReclaimed` naming it, rather than holding the
   ControlPlane in `Terminating` for a grant the target withdrew.
6. **Deletes the managed message bus by hand, then releases the finalizers once
   the ORC CRs, PushSecrets, cross-namespace children and the bus are gone**,
   letting GC cascade-delete the same-namespace Keystone, the infrastructure,
   and the remaining children. The `RabbitmqCluster` is the one same-namespace
   child the cascade is not trusted with: GC deletes with background
   propagation, so the CR vanishes the instant the RabbitMQ Cluster Operator
   removes its finalizer, and that operator's deletion path (v2.13.0 and later,
   rabbitmq/cluster-operator#1864) removes the finalizer through
   `controllerutil.CreateOrUpdate`. A second reconcile of the deleting CR, queued
   by the first one's own pod-label and StatefulSet writes, reads NotFound from
   its cache and creates a fresh, unowned broker under the same name, which
   nothing collects. `deleteManagedMessagingBeforeRelease` therefore deletes the
   owned bus itself with **foreground** propagation, which keeps the CR visible
   until every child the cluster-operator owns is gone, and holds the finalizer
   until the cluster-operator's own finalizer is off the Terminating CR (or the
   CR is gone), reported as `InfrastructureReady=False/FinalizingMessaging`. The
   CR then lingers under `foregroundDeletion` only until GC has reaped its
   children; waiting for NotFound instead would wait on a garbage collector
   envtest does not run. A bus this ControlPlane does not control (an adopted,
   externally-provisioned one) is left standing, and a brownfield `secretRef`
   names nothing to delete. Past
   `messagingTeardownDeadline` (3 minutes from the deletion timestamp) a
   **Warning** `MessagingTeardownStalled` names the broker and the release
   proceeds on the cascade, so a wedged cluster-operator cannot make the
   ControlPlane undeletable. The escape paths below release without this wait.
   The remote-children finalizer goes in the same update, because by then every
   placed namespace has been swept or its cluster abandoned.
7. **Releases an unmanaged-only remainder immediately.** K-ORC re-fetches the
   imported resource through an *authenticated* actuator before releasing any
   finalizer, and the unmanaged imports authenticate with the admin application
   credential whose revocation step 1 already triggered — so once every CR
   still present is an `Unmanaged` import, waiting on K-ORC is waiting on a
   dead-credential retry loop. The reconciler force-removes their
   `openstack.k-orc.cloud/*` finalizers right away and emits a **Normal**
   `ORCImportsReleased` event. An import's deletion is CR-only, so the external
   installation is untouched and nothing is orphaned.
8. **Bounds the wait.** If managed ORC CRs stay Terminating past
   `orcTeardownDeadline` (the 2-minute registration window plus
   `orcTeardownStallTimeout`, 5 minutes) — typically because Keystone is already
   gone and K-ORC cannot revoke — the reconciler force-removes the stuck
   `openstack.k-orc.cloud/*` finalizers (preserving any non-K-ORC finalizers),
   emits a **Warning** `ORCTeardownStalled` event, and releases the ControlPlane
   finalizer so deletion completes rather than wedging forever. A projected
   registration still present here has its children force-released before that
   finalizer goes, under the same **Warning**
   `ServiceRegistrationResourcesOrphaned` as on the normal release.
9. **Names what the escape orphaned.** The escape strips the very finalizer that
   would have revoked the credential or removed the catalog row, so every
   `Managed` CR it releases leaves its OpenStack resource behind with no
   Kubernetes object naming it. A second **Warning**, `ORCResourcesOrphaned`,
   lists exactly those CRs — the admin `ApplicationCredential` among them — and
   tells the operator to remove them from Keystone by hand. `Unmanaged` imports are never listed: their CR delete could not have
   touched OpenStack. The classification is by `ManagementPolicy` and fails loud
   (anything not explicitly `Unmanaged` is reported), because under-reporting a
   leak is worse than over-reporting one.

::: warning `kubectl delete namespace` makes the leak deterministic
Children live in the ControlPlane's own namespace, so the namespace controller
reaps `{name}-admin-password-cloud` — the Secret the managed catalog entries
authenticate with, chosen precisely so they *can* still authenticate during
teardown — concurrently with the entry CRs themselves. K-ORC then has no
credential to delete the rows with, the CRs stall, and after 5 minutes the escape
releases them. Delete the **ControlPlane CR** and let it converge before deleting
its namespace.
:::

This mirrors the Keystone reconciler's sequenced-finalizer discipline (MariaDB
then OpenBao cleanup); see
[Keystone reconciler — finalizer](../keystone/keystone-reconciler.md#finalizer).
The `{name}-admin-app-credential-backup` PushSecret is the one child kept on
`DeletionPolicy: None` so its OpenBao path is not purged on teardown.

#### The placed namespaces — `openstack.c5c3.io/remote-children`

A ControlPlane that places a service on a [target
cluster](../target-clusters.md) carries a second finalizer, the shared
`openstack.c5c3.io/remote-children`. It goes on once ANY cluster the spec names
resolves, because `reconcileNamespaces` resolves and writes per namespace inside
its loop, so that cluster's namespaces are created on the very same pass whatever
a sibling ref does. Demanding every cluster would leave the written half
unreclaimable when the ORC stall escape releases the other finalizer. While no
cluster resolves, nothing has been written to any of them for it to reclaim, and
`reconcileNamespaces` is what reports the unresolvable name. Once installed it
stays for the CR's life: a cluster that stops resolving later still holds
children.

What it holds the ControlPlane open for is the label-selected sweep in step 3.
Per placed namespace, `controlPlaneRemoteChildKinds` names the thirteen kinds the
ControlPlane writes there: `MariaDB`, `Memcached`, `SecretStore`, `Certificate`,
`ServiceAccount`, `Role`, `RoleBinding`, `Secret`, `ExternalSecret`,
`PushSecret`, `VaultDynamicSecret`, `OpenBaoTenant`, `OpenBaoCluster`. Every
object of them the ControlPlane owns is deleted through that cluster's client,
listed through its uncached reader and paged so a shared namespace cannot arrive
in one response. The list holds namespaced kinds only. The auth-delegator
`ClusterRoleBinding` and the namespace itself are cluster-scoped and deleted by
name (steps 3 and 4), while the service CRs and the K-ORC CRs never leave the
management cluster.

The sweep runs through the credentials of the registered cluster's kubeconfig,
never through the management `ClusterRole`. That is why `Role` and `RoleBinding`
are swept although the markers grant them no `list` verb at home, and it means
the [blast radius](#blast-radius-and-namespace-scoping) of a placed ControlPlane
is whatever that kubeconfig grants on its cluster.

A cluster that does not resolve while the ControlPlane is terminating is waited
for: the pass requeues with both finalizers on and reports
`NamespacesReady=False/TargetClusterUnavailable`. Engagement is asynchronous, so
an operator restart is indistinguishable from a deregistration. After
`AbandonAfter` (5 minutes, measured from both the first failed resolve in this
process and the deletion timestamp) the cluster is abandoned: a **Warning**
`RemoteChildrenAbandoned` names the cluster and the namespace whose objects stay
behind, and the teardown continues without it — the `Managed` namespace's copy on
the management cluster is still deleted, since abandoning the unreachable half
does not license leaking the reachable one. The ORC stall escape (step 7)
releases `c5c3.io/orc-teardown` alone. It never reaches this sweep, so the
remote-children finalizer stays on and a later pass runs the sweep and releases
it.

> **ControlPlane-scoped children live in the owner's namespace; service children
> follow their service.** The K-ORC CRs, the `clouds.yaml` Secret, the
> PushSecret, and the service-account material belong to the ControlPlane as a
> whole and are created in `childNamespace(cp) = cp.Namespace`, owner-referenced.
> A **service** and the things that follow it (its `MariaDB`, `Memcached`,
> `Keystone`/`Horizon`, tenant store, and credential material) are placed in
> `cp.KeystoneNamespace()` / `cp.HorizonNamespace()` — the ControlPlane's own
> namespace by default, or the one a [`namespace`
> assignment](./controlplane-crd.md#service-namespaces) gives it. A
> cross-namespace owner reference is rejected at admission because Kubernetes GC
> only cascades within a single namespace, so a service child in another
> namespace carries the ownership labels and is torn down by the finalizer
> instead of the GC cascade.

| Resource | Name | Owner | Notes |
| --- | --- | --- | --- |
| `MariaDB` | `{spec.infrastructure.database.clusterRef.name}` | ControlPlane CR | managed mode only |
| `Memcached` (unstructured) | `{spec.infrastructure.cache.clusterRef.name}` | ControlPlane CR | managed mode only |
| `ExternalSecret` (DB credential) | `{name}-keystone-db-credentials` | ControlPlane CR | managed mode only; ESO owns the materialised Secret of the same name |
| `ExternalSecret` (admin password) | `{name}-keystone-admin-credentials` | ControlPlane CR | managed mode only; ESO owns the materialised Secret of the same name |
| `Keystone` | `{name}-keystone` | ControlPlane CR | managed mode only |
| `ApplicationCredential` | `{name}-admin-app-credential` | ControlPlane CR | both modes; carries `cobaltcore.c5c3.io/admin-password-hash` |
| `Secret` | `{name}-admin-app-credential` | ControlPlane CR | both modes; data written by K-ORC, not the operator |
| `PushSecret` | `{name}-admin-app-credential-backup` | ControlPlane CR | both modes; `DeletionPolicy: None` |
| `User` (K-ORC) | `{name}-user-admin` | ControlPlane CR | both modes; unmanaged import |
| `Domain` (K-ORC) | `{name}-domain-default` | ControlPlane CR | both modes; unmanaged import |
| `Service` (K-ORC) | `{name}-identity-service` | ControlPlane CR | both modes; managed catalog entry in Managed mode, unmanaged import in External mode |
| `Endpoint` (K-ORC) | `{name}-identity-endpoint` | ControlPlane CR | managed mode only; public interface |
| `Endpoint` (K-ORC) | `{name}-identity-endpoint-{interface}` | ControlPlane CR | External mode only; one unmanaged import per interface (`public`, `internal`, `admin`) |

#### Barbican secret-store teardown

Unsetting `spec.services.barbican` takes a different path from deleting the
ControlPlane, and `deleteOrphanedBarbican` gates it on two annotations rather
than one. `c5c3.io/allow-barbican-deletion: "true"` releases the
`BarbicanSecretStore` (first, so its controller can still reach the server while
it finalizes), then the `Barbican` child, the DB-credential objects, and the
key-manager catalog CRs. Reaching the dedicated OpenBao instance takes
`c5c3.io/allow-barbican-secret-store-data-deletion: "true"` on top: the instance
carries `deletionPolicy: DeletePVCs`, so the delete wipes the raft volume, and
the teardown removes the static-seal Secret with it, leaving even a recovered
volume unreadable. Without the second annotation the instance, its PVC, its seal
key and the rest of the ensemble stand, and `BarbicanReady` says so, so
re-declaring the service reattaches to the same instance.

It does not reattach to the same secrets. The first annotation already deletes
the `Barbican` child, whose finalizer deletes the MariaDB `Database` CR, and
`database.BuildDatabase` sets no `cleanupPolicy`, so mariadb-operator applies its
`Delete` default and drops the schema. The secret and container rows,
`secret_store_metadata`, the ACLs and the quotas go with it, and db-sync recreates
the schema empty on the next projection, leaving the surviving OpenBao payloads
unreferenced. A dump of the `barbican` schema taken before the teardown is the
only thing that makes the preserve branch a round trip.

With both set the order is store, child, credentials, catalog rows, then the
`OpenBaoCluster`. The sweep then **waits** for the instance to leave etcd before
deleting the `OpenBaoTenant` and the rest of the ensemble. The tenant is what
admits the namespace to the openbao-operator, and the instance's own finalizer
runs under that admission, so removing it first would leave the instance
unfinalizable. Each object is ownership-checked, and an object already gone is
tolerated.

Both annotations are read from the live CR on every reconcile of that sweep, so
the decision is not latched. Removing one while the instance is still finalizing
resumes on the preserve branch after the `OpenBaoCluster` is already deleted, and
the ensemble behind it is never swept. The cluster-scoped auth-delegator binding
is the object that then has nothing left to collect it.

Deleting the **ControlPlane** takes the ensemble down either way, with no second
opt-in: tearing down a whole control plane is already an explicit destructive
act.

#### Neutron teardown residue

The network service leaves objects in three shapes, and both teardown paths name
them. `deleteOrphanedNeutron` runs when `spec.services.neutron` is unset with
`c5c3.io/allow-neutron-deletion: "true"`; the ControlPlane teardown reaches the
same set through `sweepExternalNamespaceResidue` for a namespace it does not own,
through `crossNamespaceServiceChildren` for the `Neutron` child in a dedicated
namespace, and through `projectedRegistrationKeys` for the registration.

- The four DB-credential shapes: the `{controlplane.Name}-neutron-db-credentials`
  `ExternalSecret` and the `VaultDynamicSecret` generator of the same name, the
  `{controlplane.Name}-neutron-db-openbao-client` `Certificate`, and the
  `neutron-db-creds` `ServiceAccount`.
- The bus delivery: the `{controlplane.Name}-neutron-messaging` Secret and the
  `{controlplane.Name}-neutron-messaging-ca` mirror. Nothing else writes them, so
  an unmanaged service leaves no broker credential behind in the namespace.
- The `Neutron` child and the `{controlplane.Name}-neutron` `KeystoneService`
  registration, whose finalizer removes the network catalog rows, the service
  user, and its project.

Each object is ownership-checked against its live state, so a same-named object
belonging to somebody else in a shared namespace is left alone. The `OVNCentral`
the child references appears in neither path and is deleted nowhere: it is
deployed outside the plane and only read.

#### External-mode deletion resource set

`orcChildObjects(cp)` derives the swept CR names from the ControlPlane spec, so
Managed mode enumerates exactly the five CRs it always did and External mode adds
the per-interface identity `Endpoint` imports. A name that never existed in the
current mode is simply `NotFound` and is tolerated as already-gone.

The enumeration is purely spec-derived, and it can be: every name it produces
follows from the ControlPlane's identity and its keystone mode, neither of which a
spec edit can drop while leaving a CR behind. Rows a `KeystoneService` registers
are not in this set at all — that CR owns them, and `reconcileDelete` deletes the
projected registrations first and waits for their own teardown.

What a `Delete` does to the external OpenStack installation is decided by each
K-ORC CR's `ManagementPolicy`, not by the ControlPlane's mode:

- **`ApplicationCredential`** — `Managed`. Its K-ORC finalizer revokes the
  credential at the Keystone level *before* the CR delete returns, so
  authenticating with it immediately afterwards yields **404** `Could not find
  Application Credential` (not 401). This is the one identity object the operator
  minted, so it is the one it destroys.
- **`User`, `Domain`** — `Unmanaged` imports. Deleting their CRs removes the
  Kubernetes objects and leaves the OpenStack resources they imported untouched.
  K-ORC's deletion-guard finalizers also enforce the teardown order: a `User`
  cannot go while an `ApplicationCredential` still references it.
- **`Service`, `Endpoint`** — in Managed mode these are the managed catalog
  entries, so the sweep deletes them from Keystone's catalog. In **External** mode
  the identity `Service` and its per-interface `Endpoint`s are `Unmanaged`
  imports, so deleting them is a CR-only delete and the external catalog is left
  bit-for-bit intact.
That holds for a teardown K-ORC can complete. The **stall escape is the deliberate
exception**: past `orcTeardownDeadline` it releases every stuck CR by stripping
the finalizer that would have done the revoke or the `DELETE`, so each `Managed` CR
it releases orphans its OpenStack resource. Those are the CRs the
`ORCResourcesOrphaned` Warning names.

The OpenBao-backed Secrets are torn down by owner-reference GC, **except** the
path behind the `{name}-admin-app-credential-backup` PushSecret: its
`DeletionPolicy` is deliberately `None`, so the last-pushed credential survives
at its OpenBao path. Nothing else is touched — a K-ORC CR the ControlPlane does
not own is never swept.

### Security invariant

The admin password and the minted application-credential Secret are read **only**
by the c5c3-operator and the K-ORC controller pods — they are **never** mounted
into Keystone or any OpenStack service workload. Keystone
receives the admin password solely through its own bootstrap Secret ref for the
one-time `keystone-manage bootstrap`; the long-lived application credential lives
exclusively on the c5c3↔K-ORC↔OpenBao path. `restricted: true` (the default)
further bounds the blast radius by scoping the minted credential. These
invariants are enforced by the `credential_invariant_test.go` checks
(`TestCredentialInvariant_MintedACIsRestricted`,
`TestCredentialInvariant_AppCredentialSecretAbsentFromKeystoneSpec`,
`TestCredentialInvariant_AppCredentialSecretReferencedOnlyByPushSecretAndAC`,
`TestCredentialInvariant_NoWorkloadReferencesAppCredentialSecret`).

The `PushSecret`'s `DeletionPolicy: None` is the one deliberate exception to the
GC cascade: tearing down a ControlPlane removes the PushSecret CR but leaves the
last-pushed credential in OpenBao at this ControlPlane's own per-CR path
(`openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential`), so a
re-created control plane in the same namespace re-adopts that per-ControlPlane
bootstrap secret rather than being locked out mid-rotation.

---

## Metrics Instrumentation

Every sub-reconciler invocation is instrumented for Prometheus via a single
package-scope `instrumenter`, declared in
`operators/c5c3/internal/controller/instrumentation.go`. Its `Instrument` method
comes from the shared `internal/common/instrumentation` package — the
duration/error metric pair and the wrapper logic are identical across all
CobaltCore operators and live there; the c5c3 file supplies only the
`c5c3_operator` prefix
and the `subReconcilerConditionTypes` map. `Reconcile` wraps every
sub-reconciler call with it; a direct call that bypasses it is a contract
violation.

```go
func (i *Instrumenter) Instrument(
    ctx  context.Context,
    name string,
    fn   func(context.Context) (ctrl.Result, error),
) (ctrl.Result, error)
```

Behavioural contract:

- **Always** records one observation in
  `c5c3_operator_reconcile_duration_seconds{sub_reconciler=name}` via `defer` —
  on the success path, the error path, and even when `fn` panics (the deferred
  call runs before the stack unwinds).
- **Only** increments
  `c5c3_operator_reconcile_errors_total{sub_reconciler=name, condition_type=…}`
  when `fn` returns a non-nil error.
- Does **not** recover from panics — they propagate to the caller.
- Carries **no per-CR labels** (no `controlplane` / `namespace`). The two label
  dimensions (`sub_reconciler`, and `condition_type` on the error counter) are
  bounded by the number of sub-reconcilers, keeping the series count
  fleet-independent. Per-CR collectors are intentionally out of scope.

Both vectors are registered exactly once on the controller-runtime registry via
`sync.Once`; the histogram buckets are a fixed contract
(`0.01 … 30s`).

### Name → `condition_type` lookup and the drift guard

The `condition_type` label is resolved from the package-private
`subReconcilerConditionTypes` map in `instrumentation.go`:

| `sub_reconciler` | `condition_type` |
| --- | --- |
| `Namespaces` | `NamespacesReady` |
| `Infrastructure` | `InfrastructureReady` |
| `ESOTenantStore` | `ESOTenantStoreReady` |
| `DBCredentials` | `DBCredentialsReady` |
| `Keystone` | `KeystoneReady` |
| `Horizon` | `HorizonReady` |
| `Glance` | `GlanceReady` |
| `Placement` | `PlacementReady` |
| `Barbican` | `BarbicanReady` |
| `KORC` | `KORCReady` |
| `AdminCredential` | `AdminCredentialReady` |
| `AdminPassword` | `AdminPasswordReady` |
| `Catalog` | `CatalogReady` |
| `ServiceAccounts` | `ServiceAccountsReady` |
| `RegistrationTenantStores` | `RegistrationTenantStoresReady` |

The map carries two further entries, `KeystoneServiceCatalog` and
`KeystoneServiceAccount`, which belong to the `KeystoneService` controller rather
than to this one; see the
[KeystoneService Reconciler](./keystoneservice-reconciler.md). The
`RegistrationTenantStores` series is kept apart from `ESOTenantStore`'s because
the two carry different blast radii and one alert should not read as the other.

If `instrumenter.Instrument` is ever called with a name absent from the map, the
helper emits the sentinel `condition_type=UNKNOWN`
(`instrumentation.ConditionTypeUnknown`) rather than an empty label, so any drift is
visible in dashboards/alerts. One static drift guard keeps the map honest:
`TestSubReconcilerConditionTypesCoversAllNames` asserts that every mapped
`condition_type` is a member of `subConditionTypes`. Adding a new
sub-reconciler therefore requires updating `subConditionTypes` **and**
`subReconcilerConditionTypes`; an unmapped name is not caught by CI and
surfaces only as `condition_type=UNKNOWN` in the metrics.

---

## Testing

The reconcilers have comprehensive unit tests using the controller-runtime fake
client with `gomega` (`NewGomegaWithT(t)`), plus envtest integration tests that
drive the full chain in a real manager against a live API server. They cover the
full reconcile to `Ready=True` and two deletion scenarios,
`TestIntegration_ControlPlaneDeletion_SweepsProjectedRegistrationsFirst` and
`TestIntegration_DedicatedNamespaces`.

### Running Tests

| Scope | Command |
| --- | --- |
| All controller unit tests | `go test ./operators/c5c3/internal/controller/...` |
| Integration (envtest) | `go test -tags integration -run TestIntegration_FullReconcile_ManagedToReady ./operators/c5c3/internal/controller/` |

### Integration test

`TestIntegration_FullReconcile_ManagedToReady` (`integration_test.go`, build tag
`integration`) registers the real controller wiring (the inline
builder is kept byte-for-byte in step with `SetupWithManager`) and drives a
managed-mode ControlPlane through every sub-reconciler to the aggregate
`Ready=True`. It simulates each external dependency's readiness **in dependency
order** — MariaDB and Memcached Ready → the operator-created admin-password
`ExternalSecret` synced → Keystone child Ready → K-ORC
`ApplicationCredential` `Available` with a `status.id` → the
`{control-plane ns}/k-orc-clouds-yaml` `ExternalSecret` synced — and asserts that
every sub-condition and the aggregate `Ready` (reason `AllReady`) reach `True`,
that `status.observedGeneration` and every condition's `ObservedGeneration` match
the CR generation, and that `status.adminApplicationCredential` mirrors the
simulated AC. Beyond the aggregate condition it also asserts the **intermediate
projected specs** so a projection regression is caught: the Keystone
image tag derived from `openStackRelease`, the database/cache `clusterRef`s wired
to the infra CRs, the merged `policyOverrides`, the `restricted→Unrestricted=false`
inversion on the AC, and the identity `Service`/`Endpoint` shape.

A phase between Infrastructure and Keystone exercises the new admin-password
projection: it waits for the operator-created per-CP admin-password
`ExternalSecret`, asserts its `RemoteRef.Key` equals `adminPasswordRemoteKeyFor(cp)`
and that it is controller-owned by the ControlPlane, then simulates the ESO sync —
`SimulateExternalSecretSync` patches **only** the ExternalSecret's status, so the
renamed plain Secret (pre-created under the same name) stays the cleartext source
the operator reads — and waits for `AdminPasswordReady` to reach `True` before the
Keystone child is projected.

Two deletion scenarios run on the same harness with both controllers on one
manager. `TestIntegration_ControlPlaneDeletion_SweepsProjectedRegistrationsFirst`
pins the order for a co-located Placement registration: it goes before the admin
`ApplicationCredential`, `KORCReady=False/FinalizingServiceRegistrations` persists
while a K-ORC-shaped finalizer holds the registration's `User`, and the K-ORC
sweep starts only once the registration, its K-ORC children, PushSecret and
source Secret are gone. `TestIntegration_DedicatedNamespaces` asserts that order
for a Barbican registration placed in a namespace of its own, ahead of the
cross-namespace teardown assertions.

### Test Files

| File | Coverage |
| --- | --- |
| `controlplane_controller_test.go` | `Reconcile` orchestration, sequential early-return, Ready aggregation, `updateStatus` error-join, idempotency |
| `reconcile_infrastructure_test.go` | Managed/brownfield MariaDB + Memcached, unstructured readiness, condition contract, `ObservedGeneration` |
| `reconcile_dbcredentials_test.go` | Managed ExternalSecret projection (name/store/data/owner-ref), brownfield no-op `Ready=True`, not-ready requeue + condition contract, distinct per-CP remote key/secret name |
| `reconcile_adminpassword_test.go` | Managed ExternalSecret projection (name/store/data/owner-ref), brownfield no-op `Ready=True`, not-ready requeue + condition contract, distinct per-CP remote key/secret name |
| `reconcile_keystone_test.go` | Keystone projection, infra gate, image/rotation/policy projection, condition contract, `ObservedGeneration` |
| `reconcile_korc_test.go` | AC mint, restricted↔unrestricted inversion, hash annotation/re-mint, missing-CRD safety, admin-credential push, catalog, condition contract |
| `reconcile_credentialrotation_test.go` | Nudge model, one-per-namespace resolution, bootstrap, deferred scheduled fields, target enum |
| `credential_invariant_test.go` | Security invariants (restricted mint, app-credential Secret not on any workload) |
| `instrumentation_test.go` | Wiring smoke test (records through the instrumenter), condition_type drift guard |
| `setupwithmanager_test.go` | `For`/`Owns`/`Watches` wiring, field-indexer registration |
| `helpers_test.go` | `intervalToCron` |
| `integration_test.go` | Full envtest reconciliation to `Ready=True`; the deletion scenarios `TestIntegration_ControlPlaneDeletion_SweepsProjectedRegistrationsFirst` (registration-first teardown order, co-located) and `TestIntegration_DedicatedNamespaces` (the same in a dedicated namespace, with the cross-namespace sweep) (build tag `integration`) |

---

## File Layout

```text
operators/c5c3/
├── main.go                                     Scheme registration + bootstrap wiring, leaderElectionID
├── api/v1alpha1/
│   ├── controlplane_types.go                   ControlPlane CRD types
│   ├── credentialrotation_types.go             CredentialRotation CRD types
│   ├── secretaggregate_types.go                SecretAggregate CRD types
│   ├── controlplane_webhook.go                 ControlPlaneWebhook (validating + defaulting)
│   ├── keystoneservice_types.go                KeystoneService CRD types (see the KeystoneService pages)
│   ├── keystoneservice_webhook.go              KeystoneServiceWebhook (see the KeystoneService pages)
│   └── ...
└── internal/
    ├── controller/
    │   ├── controlplane_controller.go          Reconciler struct, Reconcile(), setReadyCondition,
    │   │                                        aggregateReady, setServicesStatus, updateStatus, secret
    │   │                                        field indexer, SetupWithManager
    │   ├── external_mode.go                    IsExternalKeystone predicate + K-ORC external-failure
    │   │                                        classification and import-stall markers
    │   ├── helpers.go                          effective backing-service resolvers, intervalToCron
    │   ├── identity_backends.go                KeystoneIdentityBackend listing + WebSSO/MultiDomain
    │   │                                        projection helpers
    │   ├── instrumentation.go                  instrumenter + drift-guard map
    │   ├── korc_cloudsyaml.go                  clouds.yaml document builders (app-credential + password bootstrap)
    │   ├── korc_eso.go                         PushSecret + clouds.yaml ExternalSecret builders/ensure
    │   ├── korc_imports.go                     admin Domain/User import projection
    │   ├── korc_secrets.go                     app-credential Secret seeding, computeAdminPasswordHash
    │   ├── reconcile_infrastructure.go         reconcileInfrastructure (MariaDB + Memcached),
    │   │                                        childNamespace, memcachedGVK
    │   ├── reconcile_namespaces.go             reconcileNamespaces (dedicated service namespaces),
    │   │                                        child ownership labels + claim
    │   ├── reconcile_esotenant.go              reconcileESOTenantStore (per-tenant SecretStore + SA +
    │   │                                        mTLS cert), effectiveControlPlaneStoreRef
    │   ├── reconcile_dbcredentials.go          reconcileDBCredentials (per-CP DB-credential ExternalSecret)
    │   ├── reconcile_adminpassword.go          reconcileAdminPassword (per-CP admin-password ExternalSecret),
    │   │                                        effectiveAdminPasswordSecretRef
    │   ├── reconcile_keystone.go               reconcileKeystone projection
    │   ├── reconcile_horizon.go                reconcileHorizon projection (dashboard + WebSSO/MultiDomain)
    │   ├── reconcile_glance.go                 reconcileGlance projection (Glance + GlanceBackend
    │   │                                        children, DB-credential ExternalSecret)
    │   ├── reconcile_placement.go              reconcilePlacement projection (Placement child)
    │   ├── reconcile_placement_dbcredentials.go Placement DB-credential names, OpenBao paths, mode
    │   ├── reconcile_barbican.go               reconcileBarbican projection (Barbican +
    │   │                                        BarbicanSecretStore children, orphan teardown)
    │   ├── reconcile_barbican_openbao.go       Dedicated OpenBao instance and its ensemble
    │   ├── reconcile_barbican_dbcredentials.go Barbican DB-credential names, OpenBao paths, mode
    │   ├── reconcile_ovn.go                    reconcileOVN (mirror the referenced OVNCentral),
    │   │                                        ovnCentralToControlPlaneMapper
    │   ├── reconcile_neutron.go                reconcileNeutron projection (Neutron child, orphan
    │   │                                        teardown)
    │   ├── reconcile_neutron_messaging.go      Shared-bus delivery into the Neutron namespace
    │   │                                        (transport-URL Secret + CA mirror)
    │   ├── reconcile_neutron_dbcredentials.go  Neutron DB-credential names, OpenBao paths, mode
    │   ├── reconcile_korc.go                   reconcileKORC (AC mint/re-mint, drift detection)
    │   ├── reconcile_admincredential.go        reconcileAdminCredential (assemble + push + re-push
    │   │                                        nudges, semantic clouds.yaml gate)
    │   ├── reconcile_catalog.go                reconcileCatalog (mode fork; the managed identity
    │   │                                        Service/Endpoint), korcAvailableUpToDate
    │   ├── reconcile_catalog_external.go       reconcileCatalogExternal (import-first: unmanaged
    │   │                                        identity imports, opt-in entries, stall detection)
    │   ├── reconcile_serviceaccounts.go        reconcileServiceAccounts (folds the built-in
    │   │                                        registrations into ServiceAccountsReady)
    │   ├── builtin_registrations.go            The leg Glance/Placement/Barbican/Neutron share:
    │   │                                        project the KeystoneService child, gate, mirror, reclaim
    │   ├── registration_projection.go          K-ORC child + ESO builders shared with the
    │   │                                        KeystoneService controller
    │   ├── keystoneservice_controller.go       KeystoneServiceReconciler (see the KeystoneService pages)
    │   ├── keystoneservice_catalog.go          Its catalog block (see the KeystoneService pages)
    │   ├── keystoneservice_account.go          Its account block (see the KeystoneService pages)
    │   ├── reconcile_delete.go                 reconcileDelete (ORC-teardown finalizer sequencing)
    │   ├── reconcile_credentialrotation.go     CredentialRotationReconciler (nudge model)
    │   ├── requeue_intervals.go                infra/dbCredentials/adminPassword/keystone/korc/credentialRotation backoffs
    │   ├── controlplane_controller_test.go     Orchestration tests
    │   ├── external_mode_test.go               External-mode helper tests
    │   ├── helpers_test.go                     helper-function tests
    │   ├── identity_backends_test.go           Identity-backend projection tests
    │   ├── instrumentation_test.go             Metrics instrumentation + drift guards
    │   ├── korc_cloudsyaml_test.go             clouds.yaml builder tests
    │   ├── reconcile_infrastructure_test.go    Infrastructure tests
    │   ├── reconcile_namespaces_test.go        Namespaces tests
    │   ├── reconcile_esotenant_test.go         ESO-tenant-store tests
    │   ├── reconcile_dbcredentials_test.go     DBCredentials tests
    │   ├── reconcile_adminpassword_test.go     AdminPassword tests
    │   ├── reconcile_keystone_test.go          Keystone projection tests
    │   ├── reconcile_horizon_test.go           Horizon projection tests
    │   ├── reconcile_glance_test.go            Glance projection tests
    │   ├── reconcile_placement_test.go         Placement projection tests
    │   ├── reconcile_placement_dbcredentials_test.go Placement DB-credential tests
    │   ├── reconcile_barbican_test.go          Barbican projection tests
    │   ├── reconcile_barbican_openbao_test.go  Dedicated OpenBao ensemble tests
    │   ├── reconcile_barbican_dbcredentials_test.go Barbican DB-credential tests
    │   ├── reconcile_ovn_test.go               OVNCentral mirroring tests
    │   ├── reconcile_neutron_test.go           Neutron projection tests
    │   ├── reconcile_neutron_messaging_test.go Bus-delivery tests
    │   ├── reconcile_neutron_dbcredentials_test.go Neutron DB-credential tests
    │   ├── reconcile_korc_test.go              K-ORC mint/re-mint tests
    │   ├── reconcile_admincredential_test.go   AdminCredential tests
    │   ├── reconcile_catalog_test.go           Catalog (managed-mode) tests
    │   ├── reconcile_catalog_external_test.go  External-catalog tests
    │   ├── reconcile_serviceaccounts_test.go   ServiceAccounts tests
    │   ├── reconcile_delete_test.go            Deletion-sequencing (finalizer) tests
    │   ├── reconcile_credentialrotation_test.go CredentialRotation tests
    │   ├── credential_invariant_test.go        Security-invariant tests
    │   ├── setupwithmanager_test.go            Watch/Owns/indexer wiring tests
    │   ├── setupwithmanager_integration_test.go SetupWithManager envtest wiring (tag: integration)
    │   └── integration_test.go                 Envtest integration test (tag: integration)
    └── testutil/                                c5c3 envtest setup helpers
```

The `c5c3_operator_*` duration/error metric vectors are registered by the shared
`internal/common/instrumentation` package (the former per-operator
`internal/metrics` package was folded into it); `instrumentation.go` supplies
only the `c5c3_operator` prefix and the name → `condition_type` map.

## Migration: legacy flat paths → per-ControlPlane paths

Earlier releases wrote the admin / K-ORC credentials to cluster-global,
flat OpenBao paths that assumed a single control plane per cluster. The operator
now writes every credential family onto a per-CR path keyed by the owning
ControlPlane's (or projected Keystone CR's) `{namespace}/{name}`, so multiple
control planes (one per
namespace; see [Multi-instance](#multi-instance)) never collide in OpenBao. This is
a one-time operator runbook to migrate an **existing** cluster; new clusters need
no migration.

> **Switching a ControlPlane's secret store** (from the default shared cluster
> store to a namespaced per-tenant `SecretStore`, or between stores) is a separate
> operation from this path migration and is documented in the
> [Multi-Tenant Deployment guide → Per-ControlPlane secret stores and OpenBao identities](../../guides/multi-tenant-deployment.md#per-controlplane-secret-stores-and-openbao-identities).
> Switching the ref moves each PushSecret in place — unchanged name and remote key —
> so the irreplaceable key material is relocated, never re-created.

The new `RemoteKey` lands the moment the operator is upgraded — the next reconcile
of each CR emits the per-CR path — so re-apply the OpenBao ACLs **first or
concurrently** with the operator upgrade. Without the updated policies ESO returns
`403` on the backup/push and the corresponding Ready conditions flip `False`
(`AdminCredentialReady` for the admin AC; `PasswordRotationReady` for the Model-B
admin password; `FernetKeysReady` / `CredentialKeysReady` for the signing keys).

**Path mapping (legacy → per-CR):**

| Credential family | Legacy flat path | Per-CR path |
| --- | --- | --- |
| Admin application credential (K-ORC) | `openstack/keystone/admin/app-credential` | `openstack/keystone/{namespace}/{name}/admin/app-credential` |
| Admin bootstrap password (Model B) | `bootstrap/keystone-admin` | `bootstrap/{namespace}/{name}/admin` |
| Fernet / credential keys (boundary-4) | `openstack/keystone/{name}/{fernet,credential}-keys` | `openstack/keystone/{namespace}/{name}/{fernet,credential}-keys` |
| Service-account passwords | — (new) | `openstack/keystone/{namespace}/{name}/service-accounts/credentials`, where `{namespace}/{name}` is the **KeystoneService** CR's |

For the admin AC the `{namespace}/{name}` is the **ControlPlane** CR's
(`adminAppCredentialRemoteKeyFor`); for the admin password and the Fernet /
credential keys it is the projected **Keystone** CR's (`{cp.Name}-keystone`). The
Fernet / credential move adds the namespace segment **on top of** the prior
flat→per-name migration (see the keystone reconciler's
[Migration note: legacy flat paths](../keystone/keystone-reconciler.md#migration-note-legacy-flat-paths));
this change only adds the leading `{namespace}/` segment.

**One-time copy (preserve the last-pushed value so nothing is locked out):**

```sh
# admin application credential (per ControlPlane <ns>/<cp>)
bao kv get kv-v2/openstack/keystone/admin/app-credential
bao kv put kv-v2/openstack/keystone/<ns>/<cp>/admin/app-credential clouds.yaml=@-
# admin bootstrap password (per Keystone CR <ns>/<name>, name = <cp>-keystone)
bao kv get kv-v2/bootstrap/keystone-admin
bao kv put kv-v2/bootstrap/<ns>/<name>/admin password=<value>
# fernet / credential keys (per Keystone CR <ns>/<name>)
bao kv get kv-v2/openstack/keystone/<name>/fernet-keys
bao kv put kv-v2/openstack/keystone/<ns>/<name>/fernet-keys value=<value>
bao kv get kv-v2/openstack/keystone/<name>/credential-keys
bao kv put kv-v2/openstack/keystone/<ns>/<name>/credential-keys value=<value>
```

**Re-apply the OpenBao ACLs.** Re-run
`deploy/openbao/bootstrap/setup-policies.sh` (the kind/dev path; also invoked by
`hack/deploy-infra.sh`), or for production clusters managed outside the bootstrap
flow apply the updated policy files directly with `bao policy write …`:

| Policy file | Grants write to |
| --- | --- |
| `eso-tenant.hcl` | the per-tenant admin AC, admin-password, fernet-keys, credential-keys, and service-account paths, each templated to the caller's own namespace (`…/keystone/{namespace}/+/…` and `bootstrap/{namespace}/+/admin`) |

The three former wildcard write policies (`push-keystone-keys.hcl`,
`push-keystone-admin.hcl`, `push-app-credentials.hcl`) were retired: they matched
every tenant's paths behind a `+/+` glob, so a leaked shared-identity token could
overwrite any tenant's key material. Per-tenant secret traffic now authenticates
as the `eso-tenant` role through the operator-provisioned per-tenant
`openbao-tenant-store`, and `eso-tenant.hcl` scopes every writable path to the
caller's own namespace.

Until the `eso-tenant` policy is re-applied, ESO's push to the new path returns
`403` and the credential's Ready condition stays `False`.

**Orphaned but harmless.** After migration the legacy flat paths are **orphaned but
harmless**: the live control plane no longer reads or refreshes them, no live
PushSecret references them, and they are otherwise inert. Operators who want a clean
OpenBao state can purge them once the per-CR paths are confirmed populated and Ready:

```sh
bao kv metadata delete kv-v2/openstack/keystone/admin/app-credential
bao kv metadata delete kv-v2/bootstrap/keystone-admin
bao kv metadata delete kv-v2/openstack/keystone/<name>/fernet-keys
bao kv metadata delete kv-v2/openstack/keystone/<name>/credential-keys
```

`metadata delete` removes the current version and all historical versions at the
path — the canonical KV-v2 purge and the right inverse of the now-superseded write.
(The Fernet / credential families were previously migrated flat→per-name in an
earlier release, and the boundary-4 change layers the namespace segment on top.)
