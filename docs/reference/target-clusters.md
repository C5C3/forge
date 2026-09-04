---
title: Target Clusters
quadrant: operator
---

# Target Clusters

Nine workload CRDs carry an optional `spec.targetClusterRef`:
[Keystone](./keystone/keystone-crd.md), [Barbican](./barbican/barbican-crd.md),
[Horizon](./horizon/horizon-crd.md), [Glance](./glance/glance-crd.md),
[Placement](./placement/placement-crd.md), `OVNCentral`, `OVNChassis`,
`Neutron`, and `NeutronMetadataAgent`. The field names a registered target
cluster that receives every child the CR projects: Deployments, ConfigMaps,
Secrets, and, for the services that have one, the database CRs. The CR itself
does not move. It is created, reconciled, and deleted on the management cluster,
and so are its status, its finalizers, and the webhooks that admit it.

Omitting the field selects the local cluster, the one the operator runs on. The
children are created there and the deployment behaves like a single-cluster one,
so an existing CR keeps its behavior without an edit.

The [ControlPlane](./c5c3/controlplane-crd.md) carries the ref per service
instead of once per CR: `services.keystone`, `services.horizon`,
`services.glance`, `services.placement`, `services.barbican`, and
`services.neutron` each take one, so one control plane can run its identity
service on one cluster and its dashboard on another. Everything on this page
applies to it, and what is specific to it is collected under
[ControlPlane placement](#controlplane-placement).

## The field

`name` is the only key. It must be a non-empty DNS-1123 subdomain
(`MinLength=1` plus the Kubernetes object-name pattern) and it must match the
name of a registration Secret.

```yaml
spec:
  targetClusterRef:
    name: edge-1
```

The ref is immutable once the CR exists. Adding it, removing it, or renaming it
is rejected by two spec-level CEL transition rules with the message
`targetClusterRef is immutable`, and again by the validating webhook, which
appends the reason to that same text. The rules are evaluated only on UPDATE, and
being schema-level they hold when the webhook is down. Each of those edits would
strand the children already created on the previously selected cluster. Moving a
service between clusters means deleting the CR and creating it anew.

## Registering a target cluster

A target cluster is registered by a kubeconfig Secret on the management cluster.
The Secret's name is the cluster name a CR references.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: edge-1
  namespace: c5c3-clusters
  labels:
    sigs.k8s.io/multicluster-runtime-kubeconfig: "true"
type: Opaque
data:
  kubeconfig: <base64-encoded kubeconfig>
  namespaces: <base64-encoded comma-separated namespace list>
```

Three parts of that Secret are contractual, and a fourth is optional:

| Part | Value |
| --- | --- |
| Namespace | The operator's `--clusters-namespace` flag, default `c5c3-clusters`. The Secret informer is widened to this one namespace, and clearing the flag switches target clusters off entirely: no cluster is engaged and no Secret is read outside the watched namespace |
| Label | `sigs.k8s.io/multicluster-runtime-kubeconfig`, with the string value `"true"`. The label being present is not enough; any other value leaves the Secret unregistered |
| Data key | `kubeconfig` |
| Data key `namespaces` | Optional. A comma-separated list of DNS-1123 namespace names. Present, it restricts the engaged cluster's cache to exactly those namespaces: every LIST and WATCH the operator issues there is namespaced, so namespace-scoped RBAC on the target is enough for every namespaced kind. Absent, the cluster is cached cluster-wide |

Any number of clusters may be registered at the same time, and each CR resolves
its own by name. Writing a new kubeconfig into the Secret reconnects: the
provider hashes the kubeconfig together with the `namespaces` value, sees the
hash change, drops the existing connection, and engages a fresh one. Editing
either one re-engages the cluster, which is also the only way its cache's
namespaces change, since they are fixed when the cache is built. Deleting the
Secret deregisters the cluster.

Engagement is asynchronous. A CR created moments before its cluster engages
reports `TargetClusterUnavailable` briefly and heals on the next requeue.

## Who may name a cluster

::: warning Registering a cluster hands it to every CR the operator watches
`spec.targetClusterRef` is validated for shape and immutability, and for nothing
else. Nothing binds a CR, its namespace, or its author to the set of clusters it
may name, so once `edge-1` is registered, anyone who can create one of these CRs
in a watched namespace can direct the operator's stored credentials for
`edge-1` at that cluster — with an image and a configuration of their choosing.
Treat write access to the clusters namespace, and create access to these CRDs
on the management cluster, as equally privileged until an authorization model
lands.
:::

Two grants therefore need locking down on the management cluster:

| Grant | Why |
| --- | --- |
| `create`/`update` on Secrets in `c5c3-clusters` | A labelled Secret here registers a cluster. Whoever can write one decides which clusters the operator holds credentials for |
| `create` on `keystones`, `barbicans`, `glances`, `horizons`, `placements`, `controlplanes` | A CR author picks the cluster its children land on, from every name registered. A ControlPlane picks one per service |

An install that needs no target clusters carries neither exposure: a
namespace-scoped install clears `--clusters-namespace` (see below), the operator
engages nothing, and a `targetClusterRef` naming any cluster reports
`TargetClusterUnavailable`.

## Registration does not validate credentials

A kubeconfig that does not parse never engages, and the cluster name stays
unresolvable. A kubeconfig that parses but carries credentials the target
rejects engages silently: nothing logs in at registration time. The failure
surfaces on the first child write, as the target API server's own error.

```text
the server has asked for the client to provide credentials
```

A wrong token therefore shows up on the first CR that uses the cluster, not when
the Secret is applied.

A kubeconfig whose user carries an `exec` or an `auth-provider` block is refused
outright, with the cluster left unresolvable. Both are credential *plugins*, and
honoring one would have client-go run the named binary inside the operator's own
pod. A target cluster authenticates with a bearer token or a client certificate.

## Rotating and revoking the target token

The token the registration kubeconfig carries is a long-lived ServiceAccount
token, minted by the chart's `Secret` of type
`kubernetes.io/service-account-token`. It does not expire, and nothing on the
management cluster renews it — a bound token would, and no controller there
issues one. It is therefore at rest in two clusters' etcd, and a copy that leaks
stays valid until it is invalidated on the target.

Rotate it by discarding the Secret it lives in and re-applying the release, which
re-creates the Secret for the token controller to fill in. Nothing re-creates it
on its own:

```bash
kubectl --context "$TARGET" -n c5c3-access delete secret target-cluster-access-token
helm upgrade --kube-context "$TARGET" --reuse-values \
  target-cluster-access deploy/target-cluster/target-cluster-access -n c5c3-access
```

Then rebuild the registration kubeconfig from the new token, exactly as
[Deploy to a Target Cluster](../guides/deploy-to-a-target-cluster.md) assembles
it the first time, and write it back over the registration Secret:

```bash
kubectl --context "$MGMT" -n c5c3-clusters create secret generic "$CLUSTER" \
  --from-file=kubeconfig=./registration.kubeconfig \
  --from-literal=namespaces=openstack \
  --dry-run=client -o yaml | kubectl --context "$MGMT" apply -f -
```

The new kubeconfig changes the registration's hash, so the operator disengages
the cluster and engages it again under the new credential, exactly as a scope
change does. The old token stops working the moment its Secret is gone, so run
the two halves back to back: between them, every placed CR reports
`TargetClusterUnavailable` or a credentials error.

Revoking is not the same operation. Deleting the token Secret alone invalidates
that token, but a ServiceAccount re-created under the same name mints tokens
that are valid again, because the binding is to the account's name and the old
token's UID no longer matches. To revoke the access itself, delete the
ServiceAccount — `helm uninstall target-cluster-access -n c5c3-access` does,
along with the Roles and the ClusterRole, and leaves the namespaces and every
child placed in them where they are.

## When the name does not resolve

An unregistered name, or one whose Secret was deleted while CRs still pointed at
it, surfaces on the CR's first gate condition:

| CR | Condition | Status | Reason | Message |
| --- | --- | --- | --- | --- |
| Keystone, Barbican, Horizon, Glance, Placement, Neutron | `SecretsReady` | `False` | `TargetClusterUnavailable` | The resolver's error, `cluster not found` for a name that was never registered |
| BarbicanSecretStore, GlanceBackend | `CredentialsReady` | `False` | `TargetClusterUnavailable` | Same |
| ControlPlane | `NamespacesReady` usually, since it runs first; otherwise whichever sub-reconciler reaches the cluster first, out of `InfrastructureReady`, `ESOTenantStoreReady`, `DBCredentialsReady`, `AdminPasswordReady`, `GlanceReady`, `PlacementReady`, `BarbicanReady`, `NeutronReady`, `ServiceAccountsReady`, and `KORCReady` | `False` | `TargetClusterUnavailable` | Same |

The pass ends there. The CR requeues after 15 seconds, on a flat poll rather than
a backoff, and nothing is created on any cluster. Resolution runs before any
finalizer is installed, so a CR naming an unresolvable cluster carries none:
nothing was created for it, and a finalizer would only block its deletion. A
ControlPlane ends the pass in the sub-reconciler that reached the cluster and
requeues on that one's own interval, 15 seconds on the namespace and
backing-service legs and 10 on the credential ones.

A cluster that is deregistered under a running CR flips the same condition, and
the children already written to it stay where they are. The reconciler never
reaches its sub-reconcilers without a client, so nothing deletes them.

Deleting such a CR still works, after a grace period. The deletion path resolves
ahead of everything else and tolerates a target that is gone, but not right
away: while the grace period runs, an unresolvable target only requeues the
pass, every 15 seconds, the finalizers stay on, and the CR reports
`SecretsReady=False` with reason `TargetClusterUnavailable` and a message naming
the cluster it is waiting for, so the hold is not just a log line.

Two five-minute windows have to run out before the operator gives up: five
minutes since the CR was marked for deletion, and five minutes since that
operator process first failed to resolve the cluster. Engaging a cluster is
asynchronous, and rotating a registration Secret makes the provider drop the
cluster and build it again, so a registered cluster is indistinguishable from a
deregistered one for a moment after an operator restart or a rotation. Measuring
only from the deletion timestamp would give up on the first pass after such an
event whenever the CR had already been terminating for longer — a blocked
cleanup makes that ordinary — and strand the children of a CR whose cluster is
in fact perfectly reachable. A restarted operator therefore starts its own
window over.

Once both windows are out, the operator releases the finalizers, emits a
`RemoteChildrenAbandoned` warning naming what it could not delete, and lets the
CR leave etcd. The MariaDB CRs and the workload on the deregistered cluster are
left behind — unreachable, so nothing else was ever possible — and have to be
removed on that cluster by hand.

`BarbicanSecretStore` and `GlanceBackend` carry no `targetClusterRef` of their
own. Each resolves the target of the parent named by `spec.barbicanRef` or
`spec.glanceRef`, so an attachment lands on the same cluster as its parent's
children. A parent that does not exist — a dangling ref, or a GitOps apply that
has not landed it yet — leaves the target unknown, and both hold their first
gate condition at `WaitingForParent` rather than falling back to the management
cluster.

## Prerequisites on the target cluster

For the nine workload CRDs, the CR's namespace must already exist on the target.
Their operators do not create it, and a child write into a missing namespace
fails. A ControlPlane is the exception: it ensures the namespaces it places
services in, on both clusters (see
[ControlPlane placement](#controlplane-placement)).

The access a target cluster grants is packaged as the
`deploy/target-cluster/target-cluster-access` chart: a ServiceAccount, a
long-lived token Secret the registration kubeconfig carries, one Role per entry
in `values.namespaces`, and a ClusterRole for the kinds that have no namespace.
`values.namespaces` has to equal the registration Secret's `namespaces` key,
because one grants what the other scopes; a namespace granted but not declared
is never watched, and one declared but not granted answers every read with
forbidden. `createNamespaces` decides whether the chart creates those namespaces
or expects them to exist; they are annotated `helm.sh/resource-policy: keep`, so
uninstalling the release or dropping an entry from `values.namespaces` takes the
access away and leaves the placed workloads and their volumes standing. Among
its grants is `create` on `serviceaccounts/token`, which is what admits the
token a Barbican secret store mints through the target's TokenRequest API.
[Deploy to a Target Cluster](../guides/deploy-to-a-target-cluster.md) walks the
install, the kubeconfig, and the registration on two kind clusters.

One grant is off by default. `authDelegatorBinding` adds `create`, `patch` and
`delete` on `ClusterRoleBindings`, which is what a ControlPlane needs to bind a
dedicated OpenBao instance to `system:auth-delegator` — the binding that lets
that instance run the `TokenReview` every Kubernetes-auth login is validated
with. The binding's name carries a hash of the instance's namespace and name, so
`resourceNames` cannot narrow the grants to it and they cover every
`ClusterRoleBinding` on the cluster, `cluster-admin` included. Turn them on only
for a cluster a ControlPlane places such a Barbican on; every other placement, a
secret store pointed at an OpenBao instance that already runs there included,
works without them. Turning them off again while such a ControlPlane still
exists leaves its binding behind: the teardown reads the binding, is denied the
delete, and releases the ControlPlane anyway rather than holding it in
`Terminating`, recording an `AuthDelegatorBindingNotReclaimed` event that names
the binding to remove by hand.

Read-only `get` on `ClusterRoleBindings` is granted either way. Every
ControlPlane teardown reads that binding by name before it releases its
finalizer, because it is the one child no namespace sweep and no owner-reference
cascade can reach. Without the grant the API server answers forbidden rather
than not-found, and the teardown retries the error instead of releasing, so a
placed ControlPlane would never leave `Terminating`.

The OVN chassis layer (issue #903) needs a namespace it can run a node-level
workload in, and `privilegedNamespaces` is the one value that provides it. Every
entry is a namespace from `values.namespaces`; each gets its Namespace labelled
`pod-security.kubernetes.io/enforce: privileged`, and its per-namespace Role is
the only one that carries `daemonsets` alongside `deployments`. OVS and
ovn-controller run with hostNetwork, hostPath and added capabilities, so on a
cluster whose PodSecurity admission defaults to `baseline` or `restricted` a
namespace missing from the list has its chassis pods rejected at admission and
the DaemonSet reports `FailedCreate` events. A namespace not on the list has no
`daemonsets` grant either, so a cluster that hosts only service APIs carries
neither half. `createNamespaces` has to be `true`, because the chart cannot label
a namespace it does not create; either violation fails the render.

Read the label as the one grant that leaves the namespace. PodSecurity admission
is what confines this account's workload grants to the namespace they are scoped
to; without it, `create` on `deployments` or `daemonsets` there is enough to
schedule a pod with `hostPath` and a privileged `securityContext` — one per node,
for a DaemonSet — which is root on every schedulable node, and from there the
kubelet client certificate and every other pod's projected token. Whoever holds
the registration kubeconfig can do that. No narrower grant projects a chassis: a
node-level workload needs exactly the posture PodSecurity refuses. So list a
namespace the chassis has to itself. The chart sees namespace names, not what is
placed in them, and cannot check this for you — a namespace that also holds
Keystone, Glance or Barbican loses enforcement for their Deployments, Jobs and
CronJobs too.

A `NeutronMetadataAgent`'s namespace needs the same entry. Its
`neutron-ovn-metadata-agent` DaemonSet runs as root on the host network with a
bidirectional `/run/netns` mount, which is why the kind is projected into the
namespace of the `OVNChassis` it attaches to: one entry covers both node-level
workloads, and the `Neutron` API needs none.

`patch` on `Nodes` is deliberately granted nowhere in this chart, for the same
reason. Kubernetes cannot narrow the verb to one annotation key: it carries
labels and taints with it, so it would let the same holder lift
`node-role.kubernetes.io/control-plane:NoSchedule` and land one of those pods on
the control-plane node, next to `/etc/kubernetes/pki/ca.key`. Per-node state an
operator has to persist goes into a namespaced object keyed by node name, which
the per-namespace Role already covers — see
[Adding a new operator](../contributing/adding-a-new-operator.md#per-node-values).

Taking the label away again has one supported path: drop the entry from
`privilegedNamespaces`, keep it in `values.namespaces`, keep `createNamespaces`
`true`, and `helm upgrade`. Helm's three-way merge then removes the label and the
`daemonsets` grant with it. The other three revocations do not, because each one
stops the chart from rendering the Namespace at all while
`helm.sh/resource-policy: keep` holds the live one in place: `helm uninstall`,
dropping the entry from `values.namespaces` itself, and setting
`createNamespaces` to `false`. The label then survives on a namespace no release
writes any more, and PodSecurity stays off in it for whoever reuses the name.
Under `createNamespaces: false` the per-namespace Role is still rendered, so
this account keeps `create` on `deployments`, `jobs` and `cronjobs` in that
unenforced namespace; `helm uninstall` and dropping the entry from
`values.namespaces` take the Role with them and leave only the label. In those
three cases remove it by hand:

```bash
kubectl label namespace <ns> pod-security.kubernetes.io/enforce-
```

Which is also why an empty `privilegedNamespaces` proves nothing while
`createNamespaces` is `false`. In that mode the chart never writes the Namespace,
so it neither sets the label nor clears one that is already there, and it does
not read the live one either — an empty list says only that the chart is not
asking for the label, not that PodSecurity is enforcing. That is the mode the
multi-cluster CI job installs in, and the mode a cluster whose namespaces the
platform team owns installs in. Read the posture off the cluster instead:

```bash
kubectl get namespace <ns> -o jsonpath='{.metadata.labels}'
```

Placing an `OVNCentral` needs the chart revision that grants `statefulsets` and
`persistentvolumeclaims`. Without the first, the write of the Northbound
StatefulSet comes back forbidden and `NorthboundReady` holds the API server's
message under reason `StatefulSetError`. Without the second, the write of the
`<name>-backup` claim comes back forbidden and `BackupReady` holds it.

A placed CR's API health probe does not resolve over Service DNS from the
management cluster, so it runs through the target's API server instead. The same
credentials therefore need `get` on `services/proxy` in every namespace a
service is placed in; without it the probe holds the CR's API-ready condition at
`APIUnhealthy` with the API server's forbidden message.

The proxy authorizes each request by its HTTP method, so the route needs more
than `get`. The federation-objects teardown of a `KeystoneIdentityBackend`
attached to a placed Keystone takes the same route: it authenticates with a
token request, a `POST` authorized as `create`, and then deletes the identity
provider, the mapping and the protocol, each a `DELETE` authorized as `delete`.
The chart grants all three, because without them the teardown is refused on its
first call and, since it retries an error rather than releasing the finalizer,
the backend never leaves `Terminating`.

A placed `BarbicanSecretStore` cannot take that route to its OpenBao. The
operator verifies the instance's server certificate against the instance's own
CA bundle, and the URL it dials is the SAN that certificate carries, so the
handshake has to reach the instance itself. Through the service proxy it would
terminate at the API server. The connection is tunnelled at the TCP level
instead, over `pods/portforward`, which leaves the URL and the certificate check
as they are for an unplaced store. Those credentials need `create` on
`pods/portforward` in the namespace the OpenBao instance runs in, plus `get` on
`services` and `list` on `endpointslices` there to find the pod behind the
Service. Without them the store holds `ProvisioningReady` at
`OpenBaoUnreachable`, carrying the API server's forbidden message. The same
condition and reason carry `no ready endpoints for service <namespace>/<name>`
when the grants are in place but nothing ready sits behind the Service, for
instance an OpenBao mid-rollout. A brownfield store whose
`spec.openBao.server.url` names a server outside the cluster dials it directly
and needs none of the three.

Both routes carry the operator's own calls and nothing else. Traffic between
workloads on different clusters, a service on one cluster reaching Keystone on
another, takes neither: it rides the public URLs the operator composes, and
providing a route to them is the deployment's job, a gateway or a load balancer
per target cluster.

How much of a target cluster an operator caches follows the registration
Secret's `namespaces` key. Declared, each operator's cache on that cluster covers
those namespaces and nothing else, since every LIST and WATCH it issues there is
namespaced. That is what the access chart's per-namespace Roles are enough for,
`secrets` included. What stays cluster-scoped is what has no namespace to be
scoped to: `namespaces` themselves, `clustersecretstores`, `nodes`, and the
auth-delegator `ClusterRoleBinding` a dedicated OpenBao instance is bound
through — read-only always, writable behind `authDelegatorBinding`. `nodes` are
read-only with no writable half at all. That set is the chart's ClusterRole.

Omit the key and the cluster is engaged with a cluster-wide cache, which is what
every registration written before the key existed still gets. Each operator then
watches the kinds it projects there and the inputs it reads (both enumerated in
the next section) across every namespace, so its credentials need cluster-wide
`list` and `watch` on all of them, `secrets`, `roles` and `rolebindings`
included, and each operator process holds every object of every watched kind on
that cluster resident for its lifetime. Two consequences come with that posture,
and a hardened cluster should not accept them:

- Anyone who obtains the operator's target-cluster credentials can read every
  Secret in every namespace of that cluster, and a heap dump or an exec into one
  operator pod yields the same material.
- The memory the operator needs scales with the whole cluster, not with the
  namespaces it projects into.

A `namespaces` key that is present is also checked. A value that is empty, or
that carries an entry which is not a DNS-1123 label, refuses the engagement: the
provider logs the reason and builds no cluster, so every CR naming it reports
`TargetClusterUnavailable` the way an unregistered name does.

A CR whose namespace is outside a declared set fails differently, on a cluster
that engaged perfectly well. Its first cached read on the target returns
controller-runtime's `unknown namespace for the cache`, which the credential
gate records on the CR's first gate condition, carrying that message. That is
`SecretsReady` on a Keystone, Barbican, Horizon, Glance, Placement or Neutron.
The other three start their pipeline on a different condition: `TLSReady` on an
OVNCentral, `CentralReady` on an OVNChassis, `ChassisReady` on a
NeutronMetadataAgent. Nothing is created on the target: the reconciler writes
nothing it could not first read. Neither retrying nor waiting changes a cache's
scope, so the condition holds until the registration or the CR moves.

## Prerequisites on the management cluster

Target clusters need a cluster-scoped operator install. The registration Secrets
live outside the operator's own namespace, and a namespace-scoped install
(`rbac.namespaceScoped: true`) renders a Role in the release namespace and
nothing anywhere else. Its chart therefore passes `--clusters-namespace=`,
switching target clusters off: the operator engages no cluster and reads no
Secret it has no grant for. Left on, the widened Secret informer would fail to
sync and the manager would not start.

Every service operator watches, on each registered cluster, the kinds it
projects there and the inputs it reads: the credential Secrets, the database CR
for the services that have one, the OpenBao instance a Barbican secret store
waits on, and the ESO SecretStores and ClusterSecretStores. A child event maps
back to the owning CR through the ownership labels, an input event through the
same mappers the management-side watches use. A Deployment deleted on a target,
or a database password rotated there, is corrected within watch latency,
whatever cluster the infrastructure runs on. Engagement is provider-level: a
registered cluster serves one informer per watched kind to each service
operator, whether or not a CR names it — see the target-cluster prerequisites
above for what that costs. A watch is set up on a cluster only when that cluster
serves the kind, and the check runs once, as the cluster is engaged. A CRD
installed on a target after that is watched from the next engagement on, after a
registration-Secret rotation or an operator restart.

A child is matched to its owner only in the namespace that owner lives in, which
is the namespace its children land in. The ownership labels are readable and
writable by anyone with access to the target namespace, so an object carrying
them anywhere else is not treated as a child at all.

Nor is an event on a cluster the CR does not name. Because a leg is engaged on
every registered cluster, an event only reaches a CR if that CR's
`targetClusterRef` names the cluster the event came from — read from the CR on
the management cluster, never from the object that raised the event. Both the
child watches and the input watches apply it: without it, anyone able to create
an object in one shared namespace on any registered cluster could name any CR in
the fleet and have the operator reconcile it on their timing.

The watches against the management cluster register at builder time, so a
controller whose children all live on a target still needs the child kinds
installed at home. The four third-party CRD sets are therefore required on the
management cluster whatever `targetClusterRef` says.

| CRD set | Installed in this repo from |
| --- | --- |
| mariadb-operator | The `mariadb-operator-crds` chart on `https://mariadb-operator.github.io/mariadb-operator` |
| external-secrets | The `external-secrets` chart on `https://charts.external-secrets.io`, which bundles its CRDs |
| openbao-operator | The digest-pinned chart artifact `oci://ghcr.io/dc-tec/charts/openbao-operator` |
| rabbitmq-cluster-operator | The upstream `rabbitmq/cluster-operator` Git repository at tag `v2.22.5`, applied as a Flux Kustomization over `config/installation` |

The RabbitMQ Cluster Operator is installed in full there, controller included.
The shared message bus a ControlPlane declares at
[`spec.infrastructure.messaging`](./c5c3/controlplane-crd.md#messagingspec) is
provisioned in the ControlPlane's own namespace, so the controller that turns
that `RabbitmqCluster` into a broker has to run on the management cluster. No
part of the bus is written to a target.

See [Infrastructure Manifests](./infrastructure/infrastructure-manifests.md) for
the Flux sources and the dependency order these four ride in.

## Ownership and teardown on the target

A child written to a target cluster carries no owner references at all. Nothing
on that cluster can resolve an owner into the management cluster, so ownership
is recorded in three labels the operator stamps on every remote child:

| Label | Value |
| --- | --- |
| `openstack.c5c3.io/owner-kind` | The owning CR's kind: `Keystone`, `Barbican`, `Horizon`, `Glance`, `Placement`, `OVNCentral`, `OVNChassis`, `Neutron`, `NeutronMetadataAgent`, or `ControlPlane` |
| `openstack.c5c3.io/owner-name` | The owning CR's name |
| `openstack.c5c3.io/owner-namespace` | The owning CR's namespace. For the nine workload CRDs that is also the namespace the child lands in. A ControlPlane's remote children land in the namespace of the service it placed, so for them the label names the ControlPlane's namespace and the child sits elsewhere |

The kind is part of the key because a Keystone and a Barbican of the same name
in the same namespace project into one target namespace, and each has to select
only its own. Installing the service CRDs on a target cluster is safe: no child
there points at an owner, so no garbage collector has one to resolve.

A label value may not exceed 63 characters, so the name of a CR that names a
target cluster may not either. A longer one is refused at the first child write,
before anything is created.

::: warning One target namespace belongs to one management cluster
The three labels name the owning CR and say nothing about where it lives. Two
management clusters that each run these operators, each hold a CR of the same
kind and name in the same namespace, and each name the same target cluster would
project into that namespace under one identity: either would take the other's
children for its own, overwrite their specs, and delete them when its own CR is
deleted. Give each management cluster its own namespace on a shared target, or
its own target cluster.
:::

Deleting a CR that names a target cluster tears its children down explicitly.
The finalizer `openstack.c5c3.io/remote-children` goes on whenever
`targetClusterRef` is set, and holds the CR in etcd until the sweep has run. The
sweep deletes every object of that operator's projected kinds the CR owns, in the
CR's namespace on the target — by the three labels, or by a controller owner
reference an older operator left on it. It runs after the cleanup
flows that delete objects by name, the MariaDB CRs and, for Keystone, the backup
PushSecrets. That PushSecret flow holds the CR across passes until ESO has
purged the kv-v2 paths, and a sweep running first would delete the PushSecrets
out from under it. The order also matches the local one, where the cascade
starts only once every finalizer has released.

The sweep does not wait for the deleted objects to leave etcd. A child holding a
finalizer of its own, the MariaDB CRs being the slow ones, finishes terminating
on the target after the CR is gone, the way it does under the local cascade.

Horizon needs no finalizer for a local deletion, since the cascade reclaims
every child it composes. A Horizon that names a target cluster carries the
remote-children finalizer and the same sweep.

A `BarbicanSecretStore` whose parent Barbican resolves to a target cluster
carries that finalizer too, plus the annotation
`openstack.c5c3.io/children-cluster` naming the cluster. Both are written before
the first credentials write. Its deletion deletes the AppRole credentials Secret
on the cluster its parent names and on the annotated one — the same cluster on
every ordinary teardown — so neither a parent deleted first nor a parent
re-created against a different target strands it. Only a Secret carrying this
store's ownership labels is taken. The AppRole on the OpenBao instance stays: it
is shared instance state, owned by the self-init contract rather than by one
store.

A cluster deregistered under a CR that is already being deleted holds the
deletion open. The pass requeues and the finalizer stays on until the two
five-minute windows run out. Then it is released without a sweep, under a
`RemoteChildrenAbandoned` warning event naming what went undeleted, and the
children stay on the unreachable cluster, to be removed there by hand.

::: warning Children written once keep an owner reference from an older operator
A child written by an operator predating this contract carries an owner
reference to the management-cluster CR. Children on the Server-Side Apply and
`CreateOrUpdate` paths shed it on the operator's first pass over them. Children
created once and never rewritten do not: the immutable config ConfigMaps and
Secrets, the derived db-connection Secret, the fernet and credential key
Secrets, the Certificates, and completed Jobs keep the stale reference for as
long as they exist. The sweep still reaches them — it recognizes the reference as
readily as the labels — so deleting the CR removes them. Until it is deleted the
reference is a hazard on a target cluster that has the service CRDs installed:
that cluster's garbage collector resolves it to a missing object and collects the
child as an orphan. Delete such a CR and create it anew before the CRDs go onto
its target cluster; everything it writes afterwards carries the labels alone.
:::

## ControlPlane placement

A ControlPlane names a cluster per service. Each of the five service blocks takes
its own ref, and a block without one keeps its service on the management cluster:

```yaml
spec:
  services:
    keystone:
      namespace:
        name: identity
      publicEndpoint: https://keystone.example.com/v3
      targetClusterRef:
        name: edge-1
```

Five rules apply at admission on top of the name-only shape. A placed service
needs a `namespace` block of its own, because a namespace exists on exactly one
cluster and the ControlPlane's own namespace stays where the ControlPlane is. A
placed catalog service (keystone, glance, placement, barbican, neutron) needs a
`publicEndpoint` or a `gateway`, since its catalog entry would otherwise
advertise an in-cluster Service DNS name that resolves nowhere else; the
dashboard is exempt, being reached by a browser rather than looked up in the
catalog. And services sharing a namespace must name the same cluster, an unplaced
one counting as naming the local one. An External-mode keystone may carry no ref
at all: no workload is deployed there, so there is nothing to place.

The remaining two are Keystone's, because every other service validates its
tokens against it. Keystone has to be published as soon as any other service is
placed away from it, whether or not Keystone itself carries a ref: that service
reaches Keystone over the public URL, and the rule above only demands publication
of a service carrying a ref of its own. And Keystone's `publicEndpoint` must use
`https` as soon as a cluster boundary separates it from anything — Keystone
placed, or a service placed away from an unplaced Keystone. Either way that URL
is the `auth_url` the operator renders the admin password and every
service-account password next to, and those credentials cross the boundary to
reach it on every mint, re-mint and delivery. Which of the two ends moved changes
nothing about that.

::: warning A placed Keystone behind a private CA needs its anchor, which reaches K-ORC alone
K-ORC stays on the management cluster and dials the placed Keystone's `https`
endpoint from there with nothing but the container's system trust store. A
target published with a cert-manager-issued private CA — the default posture of
this stack — therefore fails verification on every mint, and K-ORC swallows the
resulting list failures into imports that hang on "Waiting for OpenStack resource
to be created externally" rather than failing loud. Supply the anchor through
`services.keystone.caBundleSecretRef`, a Secret in the ControlPlane's **own**
namespace; the mint waits on `KORCReady=False/WaitingForCABundle` until it
exists.

That bundle reaches K-ORC and nothing else: it is projected as the inline
`cacert` key into the two K-ORC credentials Secrets, while a service that does
not share Keystone's cluster gets the same `https` URL as its
`spec.keystoneEndpoint` and renders it into `[keystone_authtoken]`, which carries
no option for a trust anchor. So admission forbids the bundle while any service
sits on another cluster than Keystone. Placing Keystone away from a service
requires a publicly trusted certificate on its endpoint; a Keystone every service
is co-located with may use a private CA, because only K-ORC then dials the
`https` URL at all.
:::

The ref is frozen once the service exists. Adding it, removing it, or renaming it
is rejected with `targetClusterRef is immutable`, here by the validating webhook
alone: no CEL transition rule backs it, which leaves room to turn a move between
clusters into a gated migration later. A service the previous revision did not
declare may appear placed, that being the service's creation rather than a move.
What the freeze prevents is a re-point leaving the workload, the database, the
tenant store, and the credential material behind on a cluster nothing points at
any more.

Whether the named cluster is registered is not checked at admission. An unknown
name surfaces per CR as a condition instead, with reason
`TargetClusterUnavailable` and the resolver's `cluster not found`.

What a placed service takes with it, and what stays behind:

| Object | Created on |
| --- | --- |
| The five projected service CRs, each carrying `spec.targetClusterRef` verbatim | The management cluster |
| The `BarbicanSecretStore` and `GlanceBackend` CRs, which carry no ref and follow their parent's | The management cluster |
| Every K-ORC CR: the admin `ApplicationCredential`, the catalog `Service` and `Endpoint` rows, and the service accounts' `User`, `Project`, `Domain`, `Role`, and `RoleAssignment` | The management cluster |
| The admin-credential chain: the minted application-credential Secret, its backup `PushSecret`, and the `clouds.yaml` `ExternalSecret` | The management cluster |
| The per-generation service-account password Secrets the K-ORC `User` CRs reference | The management cluster |
| The tenant-store trio of the ControlPlane's own namespace | The management cluster |
| `MariaDB` and `Memcached` | The service's cluster |
| The ESO tenant-store trio: `ServiceAccount`, `Certificate`, `SecretStore` | The service's cluster |
| The database-credential quartet: `ServiceAccount`, `Certificate`, `VaultDynamicSecret`, `ExternalSecret` | The service's cluster |
| The admin-password `ExternalSecret` | The service's cluster |
| Barbican's dedicated OpenBao ensemble, including the auth-delegator `ClusterRoleBinding` | The service's cluster |
| A registration's service-account delivery objects: source Secret, `PushSecret`, `ExternalSecret`, and the Secret ESO materializes from it | The management cluster |
| The mirrored `ExternalSecret` a placed built-in service reads, and the Secret ESO materializes from it | The service's cluster |
| The namespace a service is placed in | Both |

The namespace is on both because both sides need it: the projected CR lives in it
on the management cluster, and the objects that follow the service land in it on
the target. It is created under whichever lifecycle the block declares, and a
`Managed` one is labelled and owned on both.

A `Managed` namespace and a scoped registration do not combine. RBAC cannot
pre-scope writes to a namespace that does not exist yet, and a `Managed`
namespace is created at reconcile time, after the grant would have had to name
it; pre-creating it to get ahead of that parks the condition on
`NamespaceNotOwned`, because the UID mark that would make it this ControlPlane's
is absent. A service placed onto a cluster whose registration declares
namespaces therefore takes the `External` lifecycle, against namespaces the
target's owner created and granted. ControlPlane placement also reaches past the
access chart in one place: Barbican's dedicated OpenBao ensemble reads the
`kubernetes` EndpointSlices in the target's `default` namespace to compute the
API server endpoint IPs the instance is configured with, and `default` is not a
namespace a service is placed in.

Every remote child carries the three ownership labels above, with `owner-kind:
ControlPlane`, plus the `c5c3.io/controlplane-name` and
`c5c3.io/controlplane-namespace` pair the operator's own watches map a child back
by, and no owner reference. A namespace created on a target cluster carries one
mark more: the annotation `c5c3.io/controlplane-uid`, holding the owning CR's
UID. The labels name a ControlPlane by name and namespace, and a target cluster
is registerable from any number of management clusters — each able to run an
`openstack` ControlPlane in an `openstack` namespace, the quickstart defaults.
The UID is what tells those apart, and it is also the only part of the claim a
target cluster cannot produce on its own: both labels are derived from the CR's
name and namespace, so anyone holding `patch` on a namespace there can write
them, while the UID is minted by the management cluster's API server. A namespace
on a target whose mark names a **different** ControlPlane is neither adopted nor
deleted, whatever its labels say.

A namespace whose mark is **missing** is not adopted either. Nothing left on the
object says whether something stripped the mark off a namespace the operator
created — the annotation is an ordinary, mutable one on a cluster the operator
does not own, so a mutating admission policy or an annotation pruner can take it
off — or whether the labels were written onto a namespace it never created.
Adopting on the labels alone would let that single `patch` hand the operator a
foreign namespace to project into and, at teardown, to cascade away. So the
condition parks on `NamespaceNotOwned` and names both remedies: restore the
annotation to this ControlPlane's UID, or pick a free name.

Teardown draws that one line lower. A namespace carrying the labels whose mark is
**missing** is still deleted, because teardown is the last pass anything makes
over it and refusing there would leak the namespace — with the service's database,
PVC and tenant store in it — permanently. A mark naming a **different**
ControlPlane still refuses on both sides.

The dedicated OpenBao instance's unseal Secret is the one exception: it keeps
the `OpenBaoCluster`'s controller reference, and both objects sit in the same
namespace on the same cluster, so that reference resolves.

Where a URL points depends on where its two ends sit. `korcAuthURL` answers for
the cluster a credentials document is read on. K-ORC runs on the management
cluster, so the admin documents carry the Keystone public endpoint
(`services.keystone.publicEndpoint`, else `https://{gateway.hostname}/v3` derived
from the gateway) as soon as Keystone is placed. A registration's `clouds.yaml`
is rendered with no target-cluster ref at all, so it carries the
management-cluster answer: the in-cluster Keystone URL while Keystone runs
there, the public one once Keystone is placed, and the external `authURL`
verbatim in External mode. A placed built-in service reads only the `password`
key of that Secret, so the document's own `auth_url` never reaches it. The
Keystone URL a dependent service configures comes from its own placement
instead (`keystoneEndpointFor` with that service's `targetClusterRef`): the
in-cluster one only when it and Keystone resolve to the same cluster, the
public one otherwise. A placed catalog service registers its public URL
on every interface it has, so the `internal` rows of the image, placement, and
key-manager entries carry the public form too. Composing those URLs is all this
does. Whether they are reachable from the other cluster is a routing question the
deployment answers.

Deleting a ControlPlane that placed a service runs the same
`openstack.c5c3.io/remote-children` finalizer the workload CRDs carry. It goes on
once at least one service is placed and at least one cluster the spec names
resolves — the namespaces on a resolvable cluster are created on that very pass,
whatever a sibling ref does.
The teardown that releases it keeps a fixed order. The K-ORC CRs go first, then
the owned PushSecrets, on each placed cluster as well as at home, while the
tenant store their OpenBao purge authenticates through is still alive. Then, per
placed namespace: the service CRs, deleted on the management cluster and waited
for, which is also what waits out each service operator's own remote sweep; then
thirteen kinds selected by ownership label and deleted through the target's own
credentials (`MariaDB`, `Memcached`, `SecretStore`, `Certificate`,
`ServiceAccount`, `Role`, `RoleBinding`, `Secret`, `ExternalSecret`,
`PushSecret`, `VaultDynamicSecret`, `OpenBaoTenant`, `OpenBaoCluster`); then,
under the `Managed` lifecycle, the namespace itself, on the target as well as at
home. An `External` namespace survives on both, its residue deleted by name ahead
of the label sweep so that its tenant-store trio goes last. The Barbican
auth-delegator `ClusterRoleBinding` is deleted through Barbican's cluster, and
both finalizers are released in one update. Because the sweep runs on the
registered kubeconfig's credentials rather than on the management ClusterRole,
`Role` and `RoleBinding` are swept although the operator holds no `list` verb on
them at home.

A cluster that stops resolving while the ControlPlane is terminating holds the
deletion open for the abandon window described above. Past it the operator emits
the `RemoteChildrenAbandoned` warning naming the cluster and the namespace whose
objects stay behind, and releases anyway. Only that cluster's copy stays: the
`Managed` namespace on the management cluster is reachable and owned, so it is
deleted as it always is — abandoning the unreachable half does not license
leaking the other.

The operators that act on the kinds a placed service takes with it have to run on
that service's cluster: mariadb-operator, memcached-operator, external-secrets,
cert-manager, and, for a dedicated Barbican secret store, openbao-operator. The
service operators are not among them. The `Keystone`, `Horizon`, `Glance`,
`Placement`, `Barbican`, and `Neutron` CRs stay on the management cluster, and
their operators project onto the target from there. The `OVNCentral` a placed
network service references is placed the same way: the ovn-operator reconciles it
on the management cluster and projects its children onto the target its own
`targetClusterRef` names. When the central and the network service land on
different clusters, the central has to publish both databases with
`externallyReachable: true`, because the Neutron pods then reach them over the
node network rather than through cluster DNS.

The Secrets a placed service reads but does not create have to exist in its
namespace on its own cluster: the dashboard's `SECRET_KEY` Secret, every Glance
backend's S3 credentials Secret, and, on a brownfield database, the database
credential Secret and the admin-password Secret the Keystone bootstrap reads. The
admin-password one is needed on both clusters, because the ControlPlane reads it
in its own namespace at home to mint the K-ORC application credential.

## Per-cluster capabilities

Two of the kinds these operators project are optional: the Gateway API
`HTTPRoute`, and the cert-manager `Certificate` Keystone issues for a managed
database client keypair. Whether a kind can be written is a property of the
cluster the children land on, so that is the cluster asked. A CR without
`targetClusterRef` takes the answer from the latch its operator probed against
the management cluster's `RESTMapper` at setup, and a CRD installed there
afterwards is picked up on the next operator restart. A CR that names a target
cluster is probed against that cluster's `RESTMapper` on every pass, with
nothing memoized in between. Install Gateway API on the target and the next
reconcile writes the route.

The verdict decides what the pass does. A `spec.gateway` set against a cluster
that does not serve `HTTPRoute` holds `HTTPRouteReady=False` with reason
`GatewayAPINotInstalled`, under a message naming the cluster that lacks the
CRD. Keystone's Certificate delete, the one that runs when database TLS is
switched off or pointed at a brownfield database, is skipped on a target
without cert-manager, where no Certificate can exist. A probe that fails
instead of answering is its own outcome: a target API server that is
unreachable, or throttling the discovery request, sets the sub-reconciler's own
condition — `HTTPRouteReady` or `DatabaseTLSReady` — to `False` with reason
`CapabilityProbeFailed`, and the pass is retried with backoff. That condition is
what keeps an aborted pass honest, since the aggregate `Ready` is re-computed
and `status.observedGeneration` stamped on every exit path.

A watch leg is fixed when its cluster is engaged (see above); the probe is not.
A CRD installed on a target after that takes effect on the next reconcile,
while the drift watch for that kind stays absent until the registration Secret
is rotated: an `HTTPRoute` deleted by hand on that cluster is corrected on the
next periodic requeue instead of within watch latency.

Field indexes are registered on the management cluster only. Every index is on
a CR kind, and the CRs exist there alone; a remote event finds its CR through
the local cache, because the mappers pin every request they emit to the
management cluster. Registering the indexes on the fleet would break cluster
engagement outright, since the kubeconfig provider applies its stored indexes
while engaging a cluster.

The API Service selector latch reads the target too: it goes through that
cluster's own uncached API reader, so a lagging cache cannot re-widen the
selector.

## Interim constraints

- `KeystoneIdentityBackend` carries no `targetClusterRef`, and its reconciler stays management-side. A backend attached to a Keystone that names a target cluster looks for the parent's Deployment and projection Secret locally, where they do not exist, and holds `ConfigProjected=False` with reason `WaitingForProjection`.
