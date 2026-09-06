---
title: ControlPlane CRD API Reference
quadrant: operator
---

# ControlPlane CRD API Reference

Reference documentation for the c5c3-operator ControlPlane Custom Resource
Definition. The ControlPlane CRD is the top-level aggregate that
projects an OpenStack control plane: it owns the shared infrastructure
references (database, cache), a curated set of per-service specs (today:
Keystone, Horizon, Glance, Placement, and Barbican), and the K-ORC (OpenStack
Resource Controller) integration that
bootstraps and rotates the admin application credential. The reconciler (L2)
materializes this aggregate into the individual per-service CRs — see the
[ControlPlane Reconciler reference](./controlplane-reconciler.md) for the
reconciliation flow.

The c5c3 API group also ships two companion kinds: `CredentialRotation`
(a one-shot credential-rotation request) and `SecretAggregate` (types-only at
this level; the reconciler is deferred). All three are documented here.

The API surface is intentionally **smaller** than the
[Keystone CRD](../keystone/keystone-crd.md): the ControlPlane curates a subset
of each service's knobs and derives the rest from operator policy, rather than
re-exposing every service field through the aggregate.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Scope | Namespaced |

| Kind | List Kind |
| --- | --- |
| `ControlPlane` | `ControlPlaneList` |
| `CredentialRotation` | `CredentialRotationList` |
| `SecretAggregate` | `SecretAggregateList` |

**Import path:**

```go
import c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
```

**Scheme registration:**

The `init()` functions in `controlplane_types.go`, `credentialrotation_types.go`,
and `secretaggregate_types.go` each register their kind (and List kind) with the
shared `SchemeBuilder`. The operator entrypoint registers the group with the
manager's scheme through `internal/common/bootstrap` (which calls
`AddToScheme`), so every kind in the group is available to the manager:

```go
utilruntime.Must(c5c3v1alpha1.AddToScheme(scheme))
```

The manager runs with `LeaderElectionID` `c5c3.openstack.c5c3.io`.

---

## Resource Shape — ControlPlane

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  region: RegionOne
  infrastructure:
    database:
      clusterRef:
        name: mariadb
      database: keystone
      # credentialsMode selects how the service DB credential is provisioned.
      # Managed mode defaults to Dynamic (engine-issued, short-lived credentials
      # from the OpenBao database engine); set Static to opt out (migration
      # staging / brownfield). reconcileDBCredentials projects a per-ControlPlane
      # VaultDynamicSecret generator (Dynamic) or the stage-(a) KV-backed
      # ExternalSecret (Static). Dynamic requires clusterRef (managed mode).
      # credentialsMode: Dynamic
      # Per-service override: services.<svc>.databaseCredentialsMode (Static |
      # Dynamic) overrides this ControlPlane-wide mode for one service on the
      # shared database, so a staged migration can run one service Dynamic while
      # another stays Static; empty (the default) inherits the mode above.
      # In managed mode (clusterRef set) database.secretRef is
      # operator-owned — reconcileDBCredentials materialises a per-ControlPlane
      # Secret and the projected Keystone CR's secretRef is overridden to
      # {name}-keystone-db-credentials (key "password"); the value below is not
      # what Keystone consumes. A brownfield CR (database.host) must instead
      # supply its own database.secretRef Secret out-of-band.
      secretRef:
        name: keystone-db-credentials
        key: password
    cache:
      backend: dogpile.cache.pymemcache
      clusterRef:
        name: memcached
  services:
    keystone:
      replicas: 3
      rotationInterval: 168h
      gateway:
        parentRef:
          name: openstack-gw
        hostname: keystone.example.com
        path: /
      publicEndpoint: https://keystone.example.com/v3
  korc:
    adminCredential:
      cloudCredentialsRef:
        cloudName: admin
        secretName: k-orc-clouds-yaml
      passwordSecretRef:
        name: keystone-admin
        key: password
      applicationCredential:
        restricted: true
        rotation:
          mode: PasswordDriven
      bootstrapResources:
        - kind: Project
          name: admin
        - kind: Role
          name: admin
status:
  conditions:
    - type: Ready
      status: "True"
      reason: AllReady
      message: All sub-conditions are ready
      lastTransitionTime: "2026-06-02T00:00:00Z"
  observedGeneration: 4
  updatePhase: Idle
  services:
    - name: keystone
      ready: true
      release: "2025.2"
  adminApplicationCredential:
    id: 6f3c…
    restricted: true
    lastRotation: "2026-06-02T00:00:00Z"
```

### Printer Columns

`kubectl get controlplanes` displays these columns:

| Column | JSON Path | Type |
| --- | --- | --- |
| Ready | `.status.conditions[?(@.type=='Ready')].status` | string |
| Release | `.spec.openStackRelease` | string |
| Age | `.metadata.creationTimestamp` | date |

The status subresource is enabled via `+kubebuilder:subresource:status`.

---

## Resource Shape — CredentialRotation

```yaml
apiVersion: c5c3.io/v1alpha1
kind: CredentialRotation
metadata:
  name: rotate-admin
  namespace: openstack
spec:
  target: adminApplicationCredential
  bootstrap: false
  reMint: true
status:
  conditions:
    - type: Ready
      status: "True"
      reason: RotationTriggered
      lastTransitionTime: "2026-06-02T00:00:00Z"
  observedGeneration: 1
```

### Printer Columns

`kubectl get credentialrotations` displays these columns:

| Column | JSON Path | Type |
| --- | --- | --- |
| Target | `.spec.target` | string |
| Ready | `.status.conditions[?(@.type=='Ready')].status` | string |
| Age | `.metadata.creationTimestamp` | date |

---

## ControlPlaneSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `openStackRelease` | `string` | Yes | — | OpenStack release the control plane targets (e.g. `"2025.2"`). The reconciler (L2) projects this into each service CR's image tag. Must match the date-based release pattern `^\d{4}\.[12]$`, enforced by both the CRD `+kubebuilder:validation:Pattern` marker and the validating webhook. Upgrades are allowed on update, but **downgrades are rejected** (Keystone DB migrations are forward-only). Stays required in **both** keystone modes; in **External** mode it is **advisory** — no images are deployed, so the value only needs to match the external installation's release at the phase-3 managed takeover. |
| `region` | `string` | No | `"RegionOne"` | OpenStack region name applied across the control plane. Projected into the Keystone CR's `bootstrap.region`. Defaulted to `RegionOne` by **both** the `+kubebuilder:default` marker (normal admission) and the defaulting webhook (callers that bypass the CRD default). Immutable after create (the projected `bootstrap.region` is itself immutable). |
| `infrastructure` | [`*InfrastructureSpec`](#infrastructurespec) | Conditional | managed-mode defaulted | Shared backing services (database, cache) the control plane's services connect to. **Required** when `services.keystone.mode` is `Managed` (or unset, or `services.keystone` unset) — the defaulting webhook materializes a managed-mode `database`/`cache` when omitted, and the validating webhook rejects a non-External ControlPlane without it. **Forbidden** in **External** mode (an External ControlPlane provisions no backing services; phase 2 relaxes this to optional). The mode-conditional required/forbidden rule is webhook-enforced because CEL cannot span `spec.infrastructure` and `spec.services.keystone`; see [InfrastructureSpec](#infrastructurespec) and [Validation Rules](#validation-rules). |
| `services` | [`ServicesSpec`](#servicesspec) | Yes | — | Per-service configuration projected into the individual service CRs. |
| `globalPolicyOverrides` | [`*commonv1.PolicySpec`](../keystone/keystone-crd.md#policyspec) | No | `nil` | oslo.policy overrides applied across every service in the control plane. Per-service overrides (e.g. `services.keystone.policyOverrides`) take precedence over these global rules when both are set. |
| `globalExtraConfig` | `map[string]map[string]string` | No | `nil` | Free-form INI sections (`section` → `key` → `value`) applied to every INI-configured service the control plane declares (Keystone, Glance, Placement, and Barbican today). Merged **key by key** with each service's own `extraConfig`: sections are unioned, the per-service value wins per key, and a global key with no per-service counterpart stays effective, before the merged result is projected onto that service's child. **Never** applies to Horizon, which renders flat Django settings rather than INI. Legal but **inert** in External mode, the same posture as `globalPolicyOverrides`. Admission validates the merged result per declared INI service against that service's option catalog and operator-owned-key registry — see [ExtraConfig admission checks](#extraconfig-admission-checks). |
| `secretStoreRef` | [`*commonv1.SecretStoreRefSpec`](#secretstorerefspec) | No | `nil` (defaults to the shared cluster store `openbao-cluster-store`) | Selects the External Secrets store the control plane routes its ExternalSecrets and backup PushSecrets through, and is **projected onto the Keystone, Horizon, Glance, Placement, and Barbican children** — so operators normally set the store here rather than on the individual service CRs. **Mutable:** switching stores is supported — the operator moves the fernet/credential key material in place, never re-creating it. When omitted, defaults to the shared cluster-scoped `ClusterSecretStore` named `openbao-cluster-store`, so existing deployments are unchanged; set `{kind: SecretStore, name: <store>}` to reach OpenBao as a per-tenant identity resolved in the ControlPlane's own namespace. See [SecretStoreRefSpec](#secretstorerefspec). |
| `korc` | [`KORCSpec`](#korcspec) | No | defaulted | K-ORC integration used to bootstrap and rotate the admin application credential and any declared bootstrap resources. Optional — the defaulting webhook fills `adminCredential` (cloudCredentialsRef, passwordSecretRef, applicationCredential restriction/rotation) from well-known defaults when omitted. |

### SecretStoreRefSpec

`spec.secretStoreRef` selects the External Secrets store the control plane and
its projected children route their ExternalSecrets and backup PushSecrets
through. It reuses the shared `commonv1.SecretStoreRefSpec` — a `kind`
(`ClusterSecretStore` \| `SecretStore`, defaulted to `ClusterSecretStore`) plus a
required non-empty `name`; see the canonical two-field table in the
[Keystone CRD → SecretStoreRefSpec](../keystone/keystone-crd.md#secretstorerefspec).

When omitted the field defaults to the shared cluster-scoped `ClusterSecretStore`
named `openbao-cluster-store`, so existing deployments are unchanged. Set
`{kind: SecretStore, name: <store>}` to reach OpenBao as a per-tenant identity,
always resolved in the ControlPlane's own namespace (there is no namespace
field). The field is **mutable** — switching stores is supported, and the
operator moves the fernet/credential key material in place rather than
re-creating it. Its value is **projected onto the Keystone, Horizon, Glance, and
Placement children**, so operators normally set it on the ControlPlane rather
than on the individual service CRs.

---

## InfrastructureSpec

Declares the shared backing services for the control plane. All three
fields reuse the canonical `commonv1` shapes so the ControlPlane and the
per-service CRs validate the database, cache, and messaging the same way.
Database and cache are always present. `messaging` is an optional pointer: a
ControlPlane that declares no message bus gets none.

`spec.infrastructure` (and each of its `database` / `cache` blocks)
may be **omitted entirely** on a minimal managed-mode ControlPlane. The
defaulting webhook constructs a managed-mode database (`clusterRef:
openstack-db`, `database: keystone`, `secretRef.name: keystone-db`) and a
managed-mode cache (`clusterRef: openstack-memcached`, `backend:
dogpile.cache.pymemcache`) before validation runs. The two managed `clusterRef`
names are only invented when the brownfield discriminator (`database.host` /
`cache.servers`) is unset, so the database/cache XOR rule below still passes for
a brownfield CR — the webhook never coerces an explicit brownfield endpoint into
managed mode. See the [Defaulting Webhook](#defaulting-webhook) for the exact
conditions and mechanism.

The defaulted `database.secretRef.name` (`keystone-db`) is a **managed-mode
convenience name only** — in managed mode `database.secretRef` is operator-owned
and the projected Keystone CR's `secretRef` is overridden to a per-ControlPlane
Secret, and a brownfield CR must supply its own. See the [`database` field
notes](#infrastructurespec) below.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `database` | [`commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | No | managed `clusterRef: openstack-db`, `database: keystone`, `secretRef.name: keystone-db` | MariaDB connection parameters shared by the control plane. Supports managed (`clusterRef`) and brownfield (`host`) modes; exactly one must hold **after defaulting** (enforced by the CRD CEL `XValidation` rule and the validating webhook — see [Validation Rules](#validation-rules)). Optional because the defaulting webhook materializes a managed-mode block when omitted. **`database.secretRef` ownership:** in managed mode this reference is **operator-owned** — `reconcileDBCredentials` materialises a per-ControlPlane DB-credential Secret and the reconciler overrides the projected Keystone CR's `spec.database.secretRef` to point at it, so the `keystone-db` default `secretRef.name` is only a managed-mode convenience name (it is **not** what Keystone consumes and no longer resolves to a cluster Secret). A **brownfield** ControlPlane (`database.host` set, no `clusterRef`) **MUST supply** its own `database.secretRef` Secret out-of-band — the operator projects no ExternalSecret in brownfield mode. See [managed-mode provisioning](#infrastructurespec) below. |
| `cache` | [`commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | managed `clusterRef: openstack-memcached`, `backend: dogpile.cache.pymemcache` | Memcached configuration shared by the control plane. Supports managed (`clusterRef`) and brownfield (`servers`) modes; exactly one must hold **after defaulting** (enforced by the CRD CEL `XValidation` rule and the validating webhook). Optional because the defaulting webhook materializes a managed-mode block when omitted. |
| `messaging` | [`*commonv1.MessagingSpec`](#messagingspec) | No | `nil` (the defaulting webhook never materializes the block) | The shared RabbitMQ message bus. **Opt-in**: a ControlPlane that omits the block provisions no broker, and the webhook invents nothing for it, unlike `database` and `cache`. In managed mode (`clusterRef`) the reconciler provisions **one** `RabbitmqCluster` in the ControlPlane's own namespace whether or not a service consumes it: a bus is shared across services by nature, so declaring it is what asks for it. Brownfield mode (`secretRef`) attaches to an existing broker and provisions nothing. Adding the block to a live ControlPlane is allowed; removing it is rejected in **both** modes. See [MessagingSpec](#messagingspec). |

<!-- DECISION: `database`/`cache` Required flipped from Yes to No because the
     defaulting webhook now constructs a managed-mode block when the field is omitted.
     The validating webhook still enforces the clusterRef/host (resp. servers) XOR after
     defaulting, so a brownfield CR that sets host/servers is unaffected. -->

In managed mode the reconciler provisions an owned `MariaDB` CR (named after
`database.clusterRef.name`) and an owned `Memcached` CR (named after
`cache.clusterRef.name`) in the ControlPlane's own namespace. The Keystone CR
the reconciler projects points at the **same** `DatabaseSpec` / `CacheSpec`
verbatim, so the aggregate and the projected service agree on the backing
services.

The managed-mode MariaDB topology is derived from
`spec.infrastructure.database.replicas` (default `3`, minimum `1`): the default
projects a production-shaped Galera HA cluster (3 replicas, `galera.enabled`,
`100Gi` storage), while `database.replicas: 1` projects a single-instance,
non-Galera MariaDB so the fresh-create path schedules on a constrained cluster
such as a single-node kind. `cache.replicas` (also default `3`) drives the
Memcached replica count the same way. Both are only honoured in managed mode;
storage stays at `100Gi` regardless of the replica count, and a ControlPlane
that adopts a pre-existing MariaDB/Memcached leaves its topology untouched.

> **`database.secretRef` is operator-owned in managed mode.** The
> `DatabaseSpec` is projected onto the Keystone CR verbatim **except** for its
> `secretRef`. In managed mode the `reconcileDBCredentials` sub-reconciler
> create-or-updates a per-ControlPlane DB-credential `ExternalSecret` named
> `{controlplane.Name}-keystone-db-credentials` in the ControlPlane namespace
> (reading OpenBao path `openstack/keystone/{namespace}/{name}/db`), and
> `reconcileKeystone` then **overrides** the projected Keystone CR's
> `spec.database.secretRef` to `{name: "{controlplane.Name}-keystone-db-credentials",
> key: "password"}` — the operator-owned Secret. The source `cp.Spec` is left
> untouched; only the projected child's `secretRef` value is reassigned. The
> Secret that Keystone actually consumes is therefore the one this reconciler
> materialises, **not** a Secret literally named after `database.secretRef.name`.
> Consequently the `keystone-db` default for `database.secretRef.name` is
> a **managed-mode convenience name only**: the production deploy stack ships
> no `keystone-db` ExternalSecret (only the kind overlay materialises one, for
> standalone Keystone instances), and a managed ControlPlane never consumes
> it either way.
>
> A **brownfield** ControlPlane (`database.host` set, `clusterRef == nil`)
> **MUST supply** `spec.infrastructure.database.secretRef` pointing to a Secret
> it owns out-of-band. In brownfield mode `reconcileDBCredentials` is a no-op
> (it reports `DBCredentialsReady=True`, reason `BrownfieldUserSuppliedCredential`)
> and projects no ExternalSecret, so the operator never materialises the Secret
> — and the `keystone-db` default no longer resolves to a cluster Secret. See the
> [ControlPlane Reconciler reference](./controlplane-reconciler.md) for the
> `reconcileDBCredentials` flow.

---

## MessagingSpec

`spec.infrastructure.messaging` declares the RabbitMQ message bus the control
plane's services share. The block is optional and the defaulting webhook never
creates it: no `messaging` block, no broker.

Managed mode (`clusterRef`) hands the bus to the
[RabbitMQ Cluster Operator](../infrastructure/infrastructure-manifests.md#rabbitmq-cluster-operator).
The reconciler create-or-updates one owned `RabbitmqCluster` CR of that name.
Brownfield mode (`secretRef`) points at a Secret that already holds a complete
`rabbit://` transport URL, and nothing is provisioned.

The managed bus is the one backing-service class enumerated at the
**ControlPlane's own namespace** regardless of consumers. `database` and `cache`
are enumerated at the namespace of the service that resolves to them, and a
shared instance every service opted out of is skipped. A message bus is shared
across services by nature, so a declared bus is a wanted bus, even before the
first consumer exists. See
[reconcileInfrastructure](./controlplane-reconciler.md#reconcileinfrastructure).

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `clusterRef` | `*corev1.LocalObjectReference` | Conditional (XOR with `secretRef`) | `openstack-rabbitmq` (webhook, only when `secretRef` is unset) | Managed mode. Names the `RabbitmqCluster` CR the reconciler owns in the ControlPlane's namespace. |
| `secretRef` | [`*commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | Conditional (XOR with `clusterRef`) | `key: transport_url` | Brownfield mode. Names a Secret holding the complete `rabbit://user:password@host:port/` transport URL under `key`. The Secret is read, never written. |
| `replicas` | `int32` | No | `3` (schema default, `Minimum` 1) | Number of RabbitMQ pods in the managed cluster. Projected onto the owned CR's `spec.replicas` and re-projected when the value changes, so the broker scales with the field. Growing is an in-place update; **shrinking is a delete-and-recreate**, because the RabbitMQ Cluster Operator refuses an in-place scale-down — the reconciler deletes the owned `RabbitmqCluster` and creates it again at the declared count, which loses the broker's volumes and everything on them. Because the field carries a schema default, an edit that merely drops the line off a larger broker arrives as a scale-down nobody typed, so the recreate is **opt-in**: without the `c5c3.io/allow-messaging-recreate: "true"` annotation on the ControlPlane the reconciler refuses the shrink, leaves the broker running at its current size, and reports `InfrastructureReady=False` / `RabbitMQError` naming the annotation. Ignored in brownfield mode. Pin it to `1` at creation on a constrained cluster such as a single-node kind. |
| `tls` | [`*MessagingTLSSpec`](#messagingtlsspec) | No | `nil` (plaintext) | Client trust for the broker connection. **Brownfield mode only**: a managed `RabbitmqCluster` is provisioned without a TLS listener, so the webhook rejects `tls` beside a `clusterRef`. |

**Defaulting.** The block is never materialized; only its leaves are filled, and
only when it is present. `clusterRef.name` defaults to `openstack-rabbitmq`
(`DefaultMessagingClusterRefName`) and is brownfield-guarded the way
`database.clusterRef.name` is: a `secretRef` in the CR keeps the webhook from
inventing a `clusterRef` beside it, so the XOR rule still passes. An empty
`secretRef.key` becomes `transport_url` (`commonv1.DefaultTransportURLSecretKey`)
and an empty `tls.caBundleSecretRef.key` becomes `ca.crt`. See the
[Defaulting Webhook](#defaulting-webhook).

**Validation.** A type-level CEL rule on `commonv1.MessagingSpec` enforces
`has(self.clusterRef) != has(self.secretRef)` with the message `exactly one of
clusterRef or secretRef must be set`, so the XOR holds for a webhook-bypassed CR
too. The validating webhook mirrors it at `spec.infrastructure.messaging` and
adds three checks: a brownfield `secretRef.name` and a
`tls.caBundleSecretRef.name` cannot be empty (`field.Required`), and a `tls`
block beside a managed `clusterRef` is rejected outright — the reconciler
projects only `spec.replicas` onto the owned cluster, so a managed broker comes
up on the RabbitMQ Cluster Operator's default, plaintext listener. External
keystone mode forbids `spec.infrastructure` as a whole, and with it the messaging
block. On update the block is a **one-way add in both modes**: it can be
declared on a live ControlPlane and never removed again — the brownfield removal
is rejected too, because admitting it would launder the mode freeze into a
two-step flip. Neither the mode nor a managed `clusterRef.name` may change;
`replicas`, `secretRef` and `tls` stay editable. See
[Update-only immutability rules](#update-only-immutability-rules).

Managed, with a single pod for a constrained cluster:

```yaml
spec:
  infrastructure:
    messaging:
      clusterRef:
        name: openstack-rabbitmq
      replicas: 1
```

Brownfield, attaching to a broker someone else runs:

```yaml
spec:
  infrastructure:
    messaging:
      secretRef:
        name: openstack-transport-url
        key: transport_url
```

A consumer in the ControlPlane's namespace reaches the managed bus at
`<clusterRef.name>.<namespace>.svc` and reads the broker credential from the
Secret the RabbitMQ Cluster Operator publishes at the cluster's
`status.defaultUser.secretReference`. The shared resolver that turns both modes
into one derived `<instance>-transport-url` Secret lands with the first
consumer, together with the answer to how a service **placed on a target
cluster** reaches a bus that runs beside the ControlPlane (issue #906).

### MessagingTLSSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `caBundleSecretRef` | [`commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | Yes | `key: ca.crt` | The Secret holding the CA bundle a consumer trusts when it verifies the broker endpoint. |

The block carries **client trust only**: a consumer renders it into
`[oslo_messaging_rabbit]` as `ssl = true` and `ssl_ca_file`. Server-side TLS on a
managed `RabbitmqCluster` (its own `spec.tls`) is a platform concern and is not
projected from here, the posture the managed MariaDB already takes on TLS and
issuer refs.

That makes `tls` a **brownfield-only** leaf, and the webhook enforces it: a
managed bus is provisioned without a TLS listener, so a `tls` block beside a
`clusterRef` would ask for an encrypted connection nothing sets up — a mismatch
that would surface only once the first consumer rendered `ssl = true` against a
plaintext broker. In brownfield mode the broker's listeners belong to whoever
runs it, so the block only says which CA the consumer trusts.

There is no `enabled` flag: any present `tls` block asks for an encrypted
connection, a nil block for a plaintext one.

---

## ServicesSpec

Declares the per-service configuration of the control plane. Today
Keystone, the Horizon dashboard, the Glance image service, the Placement
service, the Barbican key manager, and the Neutron network service are modeled;
additional services are added as fields as the operator grows.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `keystone` | [`*ServiceKeystoneSpec`](#servicekeystonespec) | No | `nil` | Configuration for the Keystone service projected by the reconciler. Optional: when unset, this ControlPlane manages no Keystone service (staged adoption, or an externally-managed Keystone) and `KeystoneReady` is reported as not-managed. Flipping it from set to `nil` **preserves** the previously-projected Keystone child by default — deleting it would cascade to the child's irreplaceable `<name>-credential-keys` Secret (and its OpenBao backup), so an accidental unset is fail-safe. Set the `c5c3.io/allow-keystone-deletion: "true"` annotation on the ControlPlane to opt in to deleting the child on unset. |
| `horizon` | [`*ServiceHorizonSpec`](#servicehorizonspec) | No | `nil` | Configuration for the Horizon dashboard projected by the reconciler. Optional: when unset, this ControlPlane manages no dashboard and `HorizonReady` is reported as not-managed (`HorizonNotManaged`), so the aggregate `Ready` is not blocked. **Forbidden in External mode** — the dashboard needs its own External-mode design. Flipping it from set to `nil` preserves the previously-projected Horizon child by default; set the `c5c3.io/allow-horizon-deletion: "true"` annotation to opt in to deleting the child on unset. |
| `glance` | [`*ServiceGlanceSpec`](#serviceglancespec) | No | `nil` | Configuration for the Glance image service projected by the reconciler. Optional: when unset, this ControlPlane manages no image service and `GlanceReady` is reported as not-managed (`GlanceNotManaged`), so the aggregate `Ready` is not blocked. The projection is **gated on `KeystoneReady`** — Glance validates every token against the ControlPlane's Keystone child. **Forbidden in External mode** — Glance needs its own External-mode design. Flipping it from set to `nil` preserves the previously-projected Glance child by default; set the `c5c3.io/allow-glance-deletion: "true"` annotation to opt in to deleting the child (and its `GlanceBackend` children and DB-credential ExternalSecret) on unset. The dynamic DB-credential generator is torn down on unset regardless of the annotation — preserving a running service does not imply preserving a credential minter. |
| `placement` | [`*ServicePlacementSpec`](#serviceplacementspec) | No | `nil` | Configuration for the Placement service projected by the reconciler. Optional: when unset, this ControlPlane manages no placement service and `PlacementReady` is reported as not-managed (`PlacementNotManaged`), so the aggregate `Ready` is not blocked. The projection is **gated on `KeystoneReady`** (Placement validates every token against the ControlPlane's Keystone child) and on the `AccountReady` of the `KeystoneService` registration the reconciler projects for it. **Forbidden in External mode**: Placement needs its own External-mode design. Flipping it from set to `nil` preserves the previously-projected Placement child by default; set the `c5c3.io/allow-placement-deletion: "true"` annotation to opt in to deleting the child (with its DB-credential ExternalSecret and the placement catalog CRs) on unset. The dynamic DB-credential generator is torn down on unset regardless of the annotation, so no credential minter outlives the service. |
| `barbican` | [`*ServiceBarbicanSpec`](#servicebarbicanspec) | No | `nil` | Configuration for the Barbican key manager projected by the reconciler. Optional: when unset, this ControlPlane manages no key manager and `BarbicanReady` is reported as not-managed (`BarbicanNotManaged`), so the aggregate `Ready` is not blocked. The projection is **gated on `KeystoneReady`** (Barbican validates every token against the ControlPlane's Keystone child) and on the `AccountReady` of the `KeystoneService` registration the reconciler projects for it. **Forbidden in External mode**: Barbican needs its own External-mode design. Flipping it from set to `nil` preserves the previously-projected Barbican child by default; set the `c5c3.io/allow-barbican-deletion: "true"` annotation to opt in to deleting the child (with its `BarbicanSecretStore`, its DB-credential ExternalSecret, and the key-manager catalog CRs) on unset. The dynamic DB-credential generator is torn down on unset regardless of the annotation. Destroying a **dedicated** OpenBao instance and the secrets in it takes a second annotation on top, `c5c3.io/allow-barbican-secret-store-data-deletion: "true"`; see [ServiceBarbicanSecretStoreSpec](#servicebarbicansecretstorespec). |
| `neutron` | [`*ServiceNeutronSpec`](#serviceneutronspec) | No | `nil` | Configuration for the Neutron network service projected by the reconciler. Optional: when unset, this ControlPlane manages no network service and `NeutronReady` is reported as not-managed (`NeutronNotManaged`), so the aggregate `Ready` is not blocked. The projection is **gated on `KeystoneReady`** (Neutron validates every token against the ControlPlane's Keystone child), on **`OVNReady`** (the ML2/OVN mechanism driver writes every network into the referenced central's Northbound database), and on the `AccountReady` of the `KeystoneService` registration the reconciler projects for it. It also **requires `spec.infrastructure.messaging`**: the Neutron CRD requires `spec.messaging`, and the child's transport URL is derived from the shared bus, so the webhook rejects a `neutron` block declared without one. **Forbidden in External mode**: Neutron needs its own External-mode design. Flipping it from set to `nil` preserves the previously-projected Neutron child by default; set the `c5c3.io/allow-neutron-deletion: "true"` annotation to opt in to deleting the child (with its DB-credential ExternalSecret, the two messaging Secrets, and the network catalog registration) on unset. The dynamic DB-credential generator is torn down on unset regardless of the annotation. The referenced `OVNCentral` is never deleted: the ControlPlane only reads it. |

---

## ServiceKeystoneSpec

A **curated local subset** of the knobs the ControlPlane exposes for the
Keystone service.

> **DECISION:** This struct is intentionally **not**
> an import of `keystonev1alpha1.KeystoneSpec`. The reconciler (L2)
> **projects** this struct into a Keystone CR; the database, cache, and Fernet
> rotation schedule of that Keystone CR are **derived** from the ControlPlane
> (`infrastructure.*` and operator policy) rather than set by the user here.
> Keeping a curated subset avoids leaking every Keystone knob through the
> aggregate and keeps the L1 API package free of a dependency on the keystone
> module. Fields not present below (replica strategy, uWSGI, network policy,
> fernet key count, etc.) are governed by the Keystone operator's own defaults on
> the projected CR, not by the ControlPlane.

The `mode` discriminator gives the Keystone service three states, mirroring the
managed-vs-brownfield split of the infrastructure specs at the service level:

| `services.keystone` | Meaning |
| --- | --- |
| unset (`nil`) | No Keystone at all — `KeystoneReady` is reported not-managed (see [ServicesSpec](#servicesspec)). |
| `mode: Managed` (or unset) | The reconciler deploys and owns a full Keystone workload — today's behavior, byte-identical. |
| `mode: External` | Service-less: identity is managed against a pre-existing, externally-operated Keystone at [`external.authURL`](#externalkeystonespec) and no Keystone workload is deployed. |

In **External** mode every managed-only field below (`replicas`, `image`,
`policyOverrides`, `extraConfig`, `rotationInterval`, `gateway`,
`publicEndpoint`) is **forbidden** and the typed
[`external`](#externalkeystonespec) block is **required**. These intra-struct
rules are enforced by type-level CEL `XValidation` rules (so they hold at the
CRD schema layer even when the
validating webhook is bypassed) and mirrored by the validating webhook; see
[Validation Rules](#validation-rules).

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `mode` | `string` (`Managed` \| `External`) | No | `Managed` | Selects whether the Keystone service is **Managed** (the reconciler deploys and owns a full Keystone workload) or **External** (identity is managed against a pre-existing Keystone at `external.authURL` and no workload is deployed). Defaulted to `Managed` by both the `+kubebuilder:default` marker and the defaulting webhook. In External mode the [`external`](#externalkeystonespec) block is required and every managed-only field below is forbidden. |
| `external` | [`*ExternalKeystoneSpec`](#externalkeystonespec) | Conditional | `nil` | Connection parameters for an externally-operated Keystone. **Required** when `mode` is `External`, **forbidden** otherwise (CEL + webhook enforced). |
| `replicas` | `*int32` | No | `nil` (Keystone operator default, 3) | Overrides the number of Keystone API replicas. When `nil`, the reconciler leaves `replicas` unset on the projected Keystone CR, so the Keystone operator applies its own default. Minimum: 1. **Forbidden in External mode.** |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Keystone container image. When `nil`, the reconciler derives the image as `ghcr.io/c5c3/keystone:{spec.openStackRelease}`. When set, the whole image reference is used verbatim. |
| `policyOverrides` | [`*commonv1.PolicySpec`](../keystone/keystone-crd.md#policyspec) | No | `nil` | Per-service oslo.policy overrides for Keystone. When set, these take precedence over `spec.globalPolicyOverrides` for the Keystone service. |
| `extraConfig` | `map[string]map[string]string` | No | `nil` | Free-form INI sections for the Keystone service. Merged **key by key** with `spec.globalExtraConfig` (this per-service value winning per key) and the merged result projected onto the Keystone child's `spec.extraConfig`. **Forbidden in External mode** (CEL + webhook, message `services.keystone.extraConfig is forbidden when services.keystone.mode is External`): no Keystone workload is deployed, so there is no config to render. Admission runs shape, operator-owned-key, and option-catalog checks on the merged block — see [ExtraConfig admission checks](#extraconfig-admission-checks). |
| `rotationInterval` | `*metav1.Duration` | No | `nil` | Overrides the Fernet / credential-key rotation interval the reconciler derives for the projected Keystone CR. When `nil`, the reconciler derives a default schedule. When set, the duration is converted to a cron expression and applied to both `fernet.rotationSchedule` and `credentialKeys.rotationSchedule` on the projected Keystone CR. An unconvertible interval (not a positive whole number of days) is **rejected at admission** by the validating webhook; if the webhook is bypassed, the reconciler surfaces `KeystoneReady=False` with reason `InvalidRotationInterval` and returns the error so the reconcile chain stops and requeues with backoff. |
| `gateway` | [`*commonv1.GatewaySpec`](#gatewayspec) | No | `nil` | Exposes the projected Keystone API externally via a Gateway API HTTPRoute. When `nil`, no HTTPRoute is projected and the Keystone API is reachable in-cluster only (its ClusterIP Service). When set, the reconciler projects it onto the Keystone CR's `spec.gateway`, so the Keystone operator attaches an HTTPRoute to the referenced Gateway. When a `gateway` is set its `hostname` must be non-empty — enforced at admission by the validating webhook (see [Validation Rules](#validation-rules)). |
| `publicEndpoint` | `string` | No | `""` | Externally routable Keystone identity endpoint URL (e.g. `https://keystone.example.com/v3`). Projected into the Keystone bootstrap (`--bootstrap-public-url`) and used for the K-ORC identity catalog Endpoint, so external clients resolve the same URL Keystone advertises. When set, it must be an HTTP(S) URL (`+kubebuilder:validation:Pattern=^https?://`), so a malformed endpoint fails at admission rather than wedging the projected Keystone CR. When empty and `gateway` is set, the reconciler derives `https://{gateway.hostname}/v3` (the default-443 form); set it explicitly when the externally reachable port differs (e.g. a kind host-port mapping like `:8443`). |
| `federationProxyImage` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the `mod_auth_openidc` sidecar image projected onto the Keystone child's `spec.federation.proxyImage`. When `nil` the reconciler projects `ghcr.io/c5c3/keystone-federation-proxy:latest`. That default is a **mutable tag**: every node re-pulls it on each pod start, and a locally built sidecar cannot be exercised. Override it with a digest-carrying `ImageSpec` for the immutable pin published images are expected to carry. Inert until a federation-typed `KeystoneIdentityBackend` attaches. Forbidden in External mode (CEL + webhook). |
| `databaseCredentialsMode` | `string` (`Static` \| `Dynamic`) | No | `""` (inherits `spec.infrastructure.database.credentialsMode`) | Per-service override of the ControlPlane-wide credentials mode for the managed **shared** database, so a staged migration can run Keystone on one mode while another service (e.g. Glance) stays on the other. Empty (the default) **inherits** the shared mode — deliberately **not** materialized by the defaulting webhook, so "inherit" stays distinguishable from an explicit override. A `Dynamic` override is **rejected** when the Keystone service declares a [dedicated](#dedicatedbackingservices) database (dedicated is `Static`-only — set `dedicatedBackingServices.database.credentialsMode` instead; see [Credential modes](#credential-modes)) and when the shared database is **brownfield** (`clusterRef` unset); `Static` is always admitted. **Forbidden in External mode (CEL + webhook)** — no managed database is provisioned there, so there is no credentials mode to override. |
| `dedicatedBackingServices` | [`*KeystoneDedicatedBackingServicesSpec`](#dedicatedbackingservices) | No | `nil` (shares the ControlPlane-wide instances) | Opts the Keystone service **out** of the shared `spec.infrastructure` instances and gives it backing services of its own. Forbidden in External mode (CEL + webhook): no backing services are provisioned at all there. |
| `namespace` | [`*ServiceNamespaceSpec`](#service-namespaces) | No | `nil` (placed in the ControlPlane's namespace) | Places the Keystone service — and the backing services, secret store, and credential material that follow it — in a namespace of its own. Create-only. Forbidden in External mode (CEL + webhook): no Keystone workload is deployed, so there is nothing to place. See [Service Namespaces](#service-namespaces). |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | No | `nil` (the local cluster the operator runs on) | Places the Keystone service, and the backing services, secret store, and credential material that follow it, on a registered target cluster. The projected `Keystone` CR stays on the management cluster and carries the ref verbatim, so the keystone operator is what writes the workload to the target. Requires a `namespace` block of its own, plus a `publicEndpoint` or a `gateway` so the identity catalog advertises an address other clusters resolve (webhook). Create-only: the validating webhook freezes the ref after creation. Forbidden in External mode (CEL + webhook): no Keystone workload is deployed, so there is nothing to place. See [ControlPlane placement](../target-clusters.md#controlplane-placement). |
| `caBundleSecretRef` | [`*commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | No | `nil` (the container's system trust store) | The private CA bundle the operator verifies a **placed** Keystone's `publicEndpoint` with. K-ORC stays on the management cluster and dials that endpoint over `https` on every mint and re-mint, so a target published with a cert-manager-issued private CA needs its anchor here — the bundle is read from the ControlPlane's **own** namespace and projected verbatim as the inline `cacert` key into both K-ORC credentials Secrets, exactly as `external.caBundleSecretRef` is. `key` defaults to `ca.crt` (webhook-only). It reaches K-ORC and nothing else, so it is forbidden while any declared service does **not** share Keystone's target cluster: such a service dials the same endpoint through `[keystone_authtoken]`, which carries no option for a trust anchor. Placing Keystone away from a service therefore requires a publicly trusted certificate on its endpoint. Also forbidden without `targetClusterRef` (a co-located Keystone is reached over its in-cluster Service URL, which performs no TLS handshake for a bundle to verify) and forbidden in External mode (CEL + webhook), where `external.caBundleSecretRef` is the field. |

---

## ServiceHorizonSpec

A curated local subset of the knobs the ControlPlane exposes for the Horizon
dashboard, mirroring `ServiceKeystoneSpec`. The reconciler projects it into a
Horizon CR; the cache and the Keystone endpoint of that child are derived from
the ControlPlane rather than set here.

Forbidden entirely when `services.keystone.mode` is `External` (the dashboard
needs its own External-mode design), so — unlike `ServiceKeystoneSpec` — none of
its fields carry per-field External-mode forbid-rules.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `*int32` | No | `nil` | Overrides the number of dashboard replicas. When `nil` the reconciler applies the Horizon operator's own default (3). Minimum 1. |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Horizon container image. When `nil` the reconciler derives `ghcr.io/c5c3/horizon:{spec.openStackRelease}`. |
| `gateway` | [`*commonv1.GatewaySpec`](#gatewayspec) | No | `nil` | Exposes the projected dashboard externally via a Gateway API HTTPRoute. When `nil` the dashboard is reachable in-cluster only. |
| `secretKeyRef` | [`*commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | No | `nil` | Overrides the Secret holding the Django `SECRET_KEY` the dashboard replicas share. When `nil` the reconciler defaults to the kind-infrastructure shim Secret `horizon-secret-key`, which is pinned to the **default** ControlPlane identity — multi-ControlPlane deployments MUST set this explicitly. |
| `publicEndpoint` | `string` | No | `""` | The **browser-observed** dashboard base URL, without a trailing slash and **including a non-default port** (e.g. `https://horizon.example.com:8443`). The reconciler derives the WebSSO origin from it (`publicEndpoint + "/auth/websso/"`) and projects that onto the Keystone child's `spec.federation.trustedDashboards`. Keystone matches the origin the dashboard sends verbatim, so the value must reproduce exactly what the browser's address bar shows. When empty and `gateway` is set, the reconciler derives `https://{gateway.hostname}` (the default-443 form). Must match `^https?://`, parse with a host, and be at most 499 characters — the Keystone child's 512-character bound on `trustedDashboards[]` minus the 13 characters `/auth/websso/` appends. |
| `extraConfig` | `map[string]JSON` | No | `nil` | Free-form **flat Django settings** mirroring the Horizon child's `spec.extraConfig`: keys are Django setting names, values arbitrary JSON. Projected **verbatim** (deep-copied) onto the Horizon child. Because the dashboard renders `local_settings.py` rather than INI, `spec.globalExtraConfig` never applies and there is no merge for this block. Admission rejects empty or non-Python-identifier setting names and the operator-owned settings — `SECRET_KEY` and every WebSSO / multi-domain setting — see [ExtraConfig admission checks](#extraconfig-admission-checks). |
| `dedicatedBackingServices` | [`*HorizonDedicatedBackingServicesSpec`](#dedicatedbackingservices) | No | `nil` (shares the ControlPlane-wide cache) | Opts the dashboard **out** of the shared `spec.infrastructure.cache` and gives it a cache of its own. The dashboard consumes no database, so `cache` is the only class it can take dedicated. |
| `namespace` | [`*ServiceNamespaceSpec`](#service-namespaces) | No | `nil` (placed in the ControlPlane's namespace) | Places the dashboard — and the cache and secret store that follow it — in a namespace of its own. Create-only. A dashboard placed apart reads its `SECRET_KEY` from **that** namespace: the default `horizon-secret-key` shim Secret is namespace-local, so supply the key material there (and name it via `secretKeyRef`). See [Service Namespaces](#service-namespaces). |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | No | `nil` (the local cluster the operator runs on) | Places the dashboard, and the cache and secret material that follow it, on a registered target cluster. The projected `Horizon` CR stays on the management cluster and carries the ref verbatim. Requires a `namespace` block of its own (webhook); no public address is required, because the dashboard registers no catalog entry. Create-only: the validating webhook freezes the ref after creation. The `SECRET_KEY` Secret has to exist in that namespace on that cluster, where the dashboard pods mount it. See [ControlPlane placement](../target-clusters.md#controlplane-placement). |

> **`publicEndpoint` and `gateway.hostname` must name the same host.** Django
> derives the origin it sends from the request's `Host` header — i.e. from
> `gateway.hostname`, not from this field. A `publicEndpoint` whose host differs
> produces an origin Keystone will reject, so whenever a `gateway` is configured
> the validating webhook rejects the ControlPlane instead. The **port** may still
> differ, since Gateway API hostnames carry none. Behind a gateway the scheme
> must be `https`: the listener terminates TLS, and Keystone POSTs the unscoped
> WebSSO token to this origin. See the
> [End-to-End SSO guide](../../guides/end-to-end-sso.md).

---

## ServiceGlanceSpec

A **curated local subset** of the knobs the ControlPlane exposes for the Glance
image service, mirroring `ServiceKeystoneSpec` and `ServiceHorizonSpec`. The
reconciler (L2) **projects** it into a [`Glance`](../glance/glance-crd.md) CR;
the database, cache, and Keystone endpoint of that child are **derived** from the
ControlPlane (`infrastructure.*` and the Keystone child's naming convention)
rather than set here, and only a **subset** of the Glance CRD's knobs is exposed.
`spec.apiServer` on the child is deliberately **not** projected, so the
Glance operator's own release-conditional API-server defaults stay authoritative.

Forbidden entirely when `services.keystone.mode` is `External` (Glance needs its
own External-mode design), so — like `ServiceHorizonSpec` — none of its fields
carry per-field External-mode forbid-rules.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `*int32` | No | `nil` | Overrides the number of Glance API replicas. When `nil` the reconciler applies the Glance operator's own default (3). Minimum 1. |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Glance container image. When `nil` the reconciler derives `ghcr.io/c5c3/glance:{spec.openStackRelease}`. |
| `gateway` | [`*commonv1.GatewaySpec`](#gatewayspec) | No | `nil` | Exposes the projected Glance API externally via a Gateway API HTTPRoute. When `nil` (the default) no HTTPRoute is projected and the Glance API is reachable in-cluster only. When a `gateway` is set its `hostname` must be non-empty — enforced at admission by the validating webhook (see [Validation Rules](#validation-rules)). |
| `publicEndpoint` | `string` | No | `""` | Externally routable Glance image endpoint URL (e.g. `https://glance.127-0-0-1.nip.io:8443`). Used **only** for the K-ORC public image catalog Endpoint; it is projected into no child CR, so the validating webhook is the only gate on it. When set, it must match `^https?://`, parse to a bare origin with a host (no path, query, or fragment — the Glance API is served at the root), and be at most 512 characters. When a `gateway` is configured the scheme must be `https` and the host must equal `gateway.hostname` (the port may differ) — see [Validation Rules](#validation-rules). When empty and `gateway` is set, the reconciler derives `https://{gateway.hostname}` (the default-443 form); set it explicitly when the externally reachable port differs (e.g. a kind host-port mapping like `:8443`). |
| `backends` | [`[]GlanceBackendEntry`](#glancebackendentry) | Yes | — | The curated list of image stores, projected one-to-one into [`GlanceBackend`](../glance/glance-backend-crd.md) child CRs. **Exactly one** entry must set `isDefault`, promoting its store to the Glance `default_backend`. A `listType=map` list keyed on `name` (the API server rejects duplicate names); `MinItems` 1 and `MaxItems` 32 (every entry amplifies into one `GlanceBackend` CR). The single-default invariant holds at the CRD schema layer via a CEL `XValidation` rule and is mirrored by the validating webhook. |
| `importFiltering` | [`*ImportFilteringSpec`](../glance/glance-crd.md#importfilteringspec) | No | `nil` | Constrains the URIs the child's `web-download` image import may fetch from. Projected **unconditionally** onto the Glance child's `spec.importFiltering`, so clearing it here removes the child's field and the Glance operator's restrictive defaults (HTTPS on port 443 plus a literal host denylist) apply again instead of the last projected value staying pinned. The URI filter is platform security policy, which is why it is projected at all while `spec.apiServer`, whose child-side defaults stay authoritative, is not. An explicitly **empty** list does not survive the projection: the lists serialize with `omitempty`, so an empty one reaches the child as unset and resolves back to the operator default. The empty-list opt-out is therefore only expressible on the [`Glance`](../glance/glance-crd.md#importfilteringspec) CR itself, and the projection only ever resolves **more** restrictively than requested. |
| `staging` | [`*StagingSpec`](../glance/glance-crd.md#stagingspec) | No | `nil` | Bounds the node-local scratch space an image import may consume on the child. The glance operator stamps the resolved `sizeLimit` on **both** scratch `emptyDir`s (the staging store and the tasks-work store), so one glance-api pod is expected to occupy at most twice the value on its node — a best-effort eviction threshold, not a filesystem quota or a scheduling reservation; see [StagingSpec](../glance/glance-crd.md#stagingspec). Projected **unconditionally** onto the Glance child's `spec.staging`, so clearing it here removes the child's field and the glance operator's `10Gi` default applies again instead of the last projected value staying pinned. Set `unbounded: true` instead of a `sizeLimit` to render the scratch volumes with no bound at all — the shape a Glance had before the block existed, and the opt-out for a deployment the `10Gi` default would start evicting on upgrade. Typed as the glance module's own `StagingSpec` rather than a curated local mirror, and admitted through that module's exported validator, so the `1Mi` floor on `sizeLimit` and the `unbounded`/`sizeLimit` exclusion cannot drift between the two CRDs. |
| `imageCache` | [`*ImageCacheSpec`](../glance/glance-crd.md#imagecachespec) | No | `nil` | Turns on the child's per-replica local image cache: every glance-api pod then keeps a copy of the image data it has served on its own disk, so a repeat download is answered from the node instead of from the object store. An image is cached once per replica that served it, so the disk budget multiplies by replicas. `sizeLimit` bounds the cache `emptyDir` (default `10Gi`, floor `1Mi`) and `maintenanceInterval` sets the pruner cadence (default `5m`, floor `1m`); see [ImageCacheSpec](../glance/glance-crd.md#imagecachespec) for what the cache does and does not promise. Projected **unconditionally** onto the Glance child's `spec.imageCache`, so clearing it here removes the child's field and switches the cache off again on the next rollout instead of leaving the last projected budget pinned. Typed as the glance module's own `ImageCacheSpec` rather than a curated local mirror, and admitted through that module's exported validator, so neither floor can drift between the two CRDs. controller-gen **copies** that schema into this CRD instead of resolving it against the Glance CRD at runtime, and the two ship in separate Helm charts, so "admitted here, accepted by the child" holds only while both charts come from the same release. |
| `importPlugins` | [`*ImportPluginsSpec`](../glance/glance-crd.md#importpluginsspec) | No | `nil` | Selects the image-import plugins the child runs: `decompression` unpacks a compressed image after staging (and requires `staging` to answer for the scratch bound, since its expansion is unbounded), `conversion` rewrites it into one disk format (default `raw`), and `injectMetadata` stamps a fixed set of image properties onto every image it applies to. Presence of a sub-block enables that plugin; the rendered order (`image_decompression`, `image_conversion`, `inject_image_metadata`) is the glance operator's and not an input, and every default resolves there at render time; see [ImportPluginsSpec](../glance/glance-crd.md#importpluginsspec) for what each plugin does and which upload paths bypass it. Projected **unconditionally** onto the Glance child's `spec.importPlugins`, so clearing it here removes the child's field and the glance operator's defaults (no plugin, `image_import_plugins = []`) apply again on the next rollout instead of leaving the last projected selection pinned. Typed as the glance module's own `ImportPluginsSpec` rather than a curated local mirror, and admitted through that module's exported validator, so the output-format enum and the injected-property rules cannot drift between the two CRDs. controller-gen **copies** that schema into this CRD too, and the two charts ship separately, so "admitted here, accepted by the child" holds only while both come from the same release. |
| `databaseCredentialsMode` | `string` (`Static` \| `Dynamic`) | No | `""` (inherits `spec.infrastructure.database.credentialsMode`) | Per-service override of the ControlPlane-wide credentials mode for the managed **shared** database, so a staged migration can run Glance on one mode while another service (e.g. Keystone) stays on the other. Empty (the default) **inherits** the shared mode — deliberately **not** materialized by the defaulting webhook, so "inherit" stays distinguishable from an explicit override. A `Dynamic` override is **rejected** when Glance declares a [dedicated](#dedicatedbackingservices) database (dedicated is `Static`-only — set `dedicatedBackingServices.database.credentialsMode` instead; see [Credential modes](#credential-modes)) and when the shared database is **brownfield** (`clusterRef` unset); `Static` is always admitted. |
| `extraConfig` | `map[string]map[string]string` | No | `nil` | Free-form INI sections for the Glance service. Merged **key by key** with `spec.globalExtraConfig` (this per-service value winning per key) and the merged result projected onto the Glance child's `spec.extraConfig`. Admission runs shape, operator-owned-key, and option-catalog checks on the merged block; the always-rejected owned keys are `[keystone_authtoken] password`, the six `[import_filtering_opts]` keys, the three `[DEFAULT] image_cache_*` keys, and the four image-import plugin keys (`[image_import_opts] image_import_plugins`, `[image_conversion] output_format`, `[inject_metadata_properties] inject` and `ignore_user_roles`) — see [ExtraConfig admission checks](#extraconfig-admission-checks). |
| `dedicatedBackingServices` | [`*GlanceDedicatedBackingServicesSpec`](#dedicatedbackingservices) | No | `nil` (shares the ControlPlane-wide instances) | Opts Glance **out** of the shared `spec.infrastructure` instances and gives it a database and/or cache of its own. Glance consumes both classes, so it can take either or both dedicated. |
| `namespace` | [`*ServiceNamespaceSpec`](#service-namespaces) | No | `nil` (placed in the ControlPlane's namespace) | Places the Glance service — and the database, cache, secret store, and object-store credential material that follow it — in a namespace of its own. Create-only. See [Service Namespaces](#service-namespaces). |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | No | `nil` (the local cluster the operator runs on) | Places the Glance service, and the database, cache, secret store, and object-store credential material that follow it, on a registered target cluster. The projected `Glance` and `GlanceBackend` CRs stay on the management cluster; the `Glance` carries the ref verbatim and the attached backends follow it. Requires a `namespace` block of its own, plus a `publicEndpoint` or a `gateway` so the image catalog advertises an address other clusters resolve (webhook). Create-only: the validating webhook freezes the ref after creation. Every backend's S3 credentials Secret has to exist in that namespace on that cluster. See [ControlPlane placement](../target-clusters.md#controlplane-placement). |

### GlanceBackendEntry

One curated image store, projected one-to-one into a `GlanceBackend` child CR.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | Keys the `listType=map` `backends` list (duplicates rejected) and is embedded **verbatim** in the projected child's name `{controlplane.Name}-glance-{name}`, hence the DNS-1123 label shape (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, `MinLength` 1, `MaxLength` 63). The validating webhook additionally bounds the composed child-CR name to the 253-byte object-name limit (no CRD marker can express it). |
| `type` | `string` (`S3`) | Yes | — | The image-store driver. Phase 1 supports `S3` only (`Enum: S3`); the enum mirrors the `GlanceBackend` CR's own type enum, so an entry admitted here can never be rejected downstream. |
| `s3` | [`*GlanceBackendS3Spec`](#glancebackends3spec) | Conditional | `nil` | The S3-compatible object store. **Required exactly when `type` is `S3`**, forbidden otherwise (the type/s3 union CEL rule on `GlanceBackendEntry` + webhook). |
| `isDefault` | `bool` | No | `false` | Marks this backend as the Glance default store. Exactly one `backends` entry must set it (CEL + webhook enforced); the reconciler projects it onto the child `GlanceBackend`'s `spec.isDefault`. |

### GlanceBackendS3Spec

The curated local S3 store shape projected onto a `GlanceBackend` child's
`spec.s3`. It models only the exposed subset and defines its own field types
rather than importing the glance module, keeping the L1 API package free of a
dependency on it; the field bounds mirror the child's own S3 shape so a value
admitted here can never be rejected downstream.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `endpoint` | `string` | Yes | — | The S3 endpoint URL (e.g. `https://s3.example.com`). Pattern `^https?://`, `MinLength` 1. Projected onto the child's `spec.s3.host`. |
| `bucket` | `string` | Yes | — | The S3 bucket images are stored in. `MinLength` 1. Projected onto the child's `spec.s3.bucket`. |
| `region` | `string` | No | `""` | The S3 region the bucket lives in. Only projected onto the child's `spec.s3.region` when set. |
| `bucketURLFormat` | `string` (`path` \| `virtual`) | No | `""` (**no schema default**) | How the bucket is addressed in request URLs: `path` (`https://host/bucket`) or `virtual` (`https://bucket.host`). Carries **no schema default on purpose** — when unset the projection leaves the child's field unset so the `GlanceBackend` CR's own default (`path`) applies at exactly one layer. |
| `credentialsSecretRef` | [`SecretNameRef`](#secretnameref) | Yes | — | References the Secret holding the S3 credentials, **resolved in the namespace the Glance service is placed in**. |

### SecretNameRef

A **name-only** reference to a Kubernetes Secret, resolved in the namespace the
Glance service is placed in. The data keys it must expose are fixed by contract
downstream (the `GlanceBackend` controller reads them), so there is no key to
select.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | The referenced Secret's name. `MinLength` 1, `MaxLength` 253. |

---

## ServicePlacementSpec

A **curated local subset** of the knobs the ControlPlane exposes for the
Placement service, mirroring `ServiceKeystoneSpec` and `ServiceGlanceSpec`. The
reconciler (L2) **projects** it into a
[`Placement`](../placement/placement-crd.md) CR; the database, cache, and
Keystone endpoint of that child are **derived** from the ControlPlane
(`infrastructure.*` and the Keystone child's naming convention) rather than set
here. `spec.apiServer` on the child is deliberately **not** projected, so the
placement operator's own uWSGI defaults stay authoritative.

The subset is smaller than the Glance one because Placement keeps its whole state
in its database and serves it over HTTP: there are no image stores to curate and
no scratch space to bound, so nothing here corresponds to the `backends`,
`importFiltering`, `staging`, or `imageCache` blocks of
[`ServiceGlanceSpec`](#serviceglancespec).

Forbidden entirely when `services.keystone.mode` is `External` (Placement needs
its own External-mode design), so, like `ServiceGlanceSpec`, none of its fields
carry per-field External-mode forbid-rules.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `*int32` | No | `nil` | Overrides the number of Placement API replicas. When `nil` the reconciler applies the Placement operator's own default (3). Minimum 1. |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Placement container image. When `nil` the reconciler derives `ghcr.io/c5c3/placement:{spec.openStackRelease}`. |
| `gateway` | [`*commonv1.GatewaySpec`](#gatewayspec) | No | `nil` | Exposes the projected Placement API externally via a Gateway API HTTPRoute. When `nil` (the default) no HTTPRoute is projected and the Placement API is reachable in-cluster only. When a `gateway` is set its `hostname` must be non-empty, enforced at admission by the validating webhook (see [Validation Rules](#validation-rules)). |
| `publicEndpoint` | `string` | No | `""` | Externally routable Placement endpoint URL (e.g. `https://placement.127-0-0-1.nip.io:8443`). Used **only** for the K-ORC public placement catalog Endpoint, the URL every compute service resolves to place its allocations; it is projected into no child CR, so the validating webhook is the only gate on it. When set, it must match `^https?://`, parse to a bare origin with a host (no path, query, or fragment, since the Placement API is served at the root and clients append the API path to the catalog URL), and be at most 512 characters; a single trailing slash is tolerated. When a `gateway` is configured the scheme must be `https` and the host must equal `gateway.hostname` (the port may differ); see [Validation Rules](#validation-rules). Without a gateway an `http://` value stays admissible for development and raises an admission warning, because every allocation call carries the caller's scoped Keystone token to this URL. When empty and `gateway` is set, the reconciler derives `https://{gateway.hostname}` (the default-443 form); set it explicitly when the externally reachable port differs (e.g. a kind host-port mapping like `:8443`). |
| `databaseCredentialsMode` | `string` (`Static` \| `Dynamic`) | No | `""` (inherits `spec.infrastructure.database.credentialsMode`) | Per-service override of the ControlPlane-wide credentials mode for the managed **shared** database, so a staged migration can run Placement on one mode while another service stays on the other. Empty (the default) **inherits** the shared mode, and is deliberately **not** materialized by the defaulting webhook, so "inherit" stays distinguishable from an explicit override. A `Dynamic` override is **rejected** when Placement declares a [dedicated](#dedicatedbackingservices) database (dedicated is `Static`-only; set `dedicatedBackingServices.database.credentialsMode` instead, see [Credential modes](#credential-modes)) and when the shared database is **brownfield** (`clusterRef` unset); `Static` is always admitted. |
| `extraConfig` | `map[string]map[string]string` | No | `nil` | Free-form INI sections for the Placement service. Merged **key by key** with `spec.globalExtraConfig` (this per-service value winning per key) and the merged result projected onto the Placement child's `spec.extraConfig`. Admission runs shape, operator-owned-key, and option-catalog checks on the merged block; the always-rejected owned keys are `[keystone_authtoken] password` and `auth_strategy` in both of its spellings (`[api]`, which Placement reads, and the deprecated `[DEFAULT]` alias oslo.config still honors). See [ExtraConfig admission checks](#extraconfig-admission-checks). |
| `dedicatedBackingServices` | [`*PlacementDedicatedBackingServicesSpec`](#dedicatedbackingservices) | No | `nil` (shares the ControlPlane-wide instances) | Opts Placement **out** of the shared `spec.infrastructure` instances and gives it a `database` and/or `cache` of its own. Placement consumes both classes, so it can take either or both dedicated; a declared block must name at least one. |
| `namespace` | [`*ServiceNamespaceSpec`](#service-namespaces) | No | `nil` (placed in the ControlPlane's namespace) | Places the Placement service, and the database, cache, secret store, and credential material that follow it, in a namespace of its own. Create-only: the validating webhook freezes the block after creation. See [Service Namespaces](#service-namespaces). |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | No | `nil` (the local cluster the operator runs on) | Places the Placement service, and the database, cache, secret store, and credential material that follow it, on a registered target cluster. The projected `Placement` CR stays on the management cluster and carries the ref verbatim. Requires a `namespace` block of its own, plus a `publicEndpoint` or a `gateway` so the placement catalog advertises an address other clusters resolve (webhook). Create-only: the validating webhook freezes the ref after creation. See [ControlPlane placement](../target-clusters.md#controlplane-placement). |

Setting `services.placement` makes the reconciler project a `KeystoneService`
registration named `{controlplane.Name}-placement` into the namespace Placement
is placed in: the `placement` catalog entry plus the `placement` service account,
whose project `service-placement` is created with the role `service`. That
account is the Keystone user the projected child authenticates as, so the
Placement child is not projected until the registration reports it provisioned
(`PlacementReady=False/WaitingForServiceRegistration` until then).

Setting `services.placement` bounds the ControlPlane's own name too. The
projected child is `{controlplane.Name}-placement`, and the placement operator
names its API Service after the CR without truncating, so the composed name has
to fit the 63-byte cap Kubernetes puts on a Service name: the ControlPlane name
may be at most **53 characters** while `services.placement` is set. The Placement
CRD declares no `metadata.name` bound of its own, so without this rule an
overlong name reaches the placement operator, which then fails to apply the
Service on every reconcile: `PlacementReady` parks on `WaitingForPlacement`, and
`metadata.name` is immutable, so the only recovery is recreating the whole
control plane. The rule runs on create and on the update that newly enables
Placement.

---

## ServiceBarbicanSpec

A **curated local subset** of the knobs the ControlPlane exposes for the Barbican
key manager, mirroring `ServiceKeystoneSpec` and `ServicePlacementSpec`. The
reconciler (L2) **projects** it into a
[`Barbican`](../barbican/barbican-crd.md) CR; the database, cache, and Keystone
endpoint of that child are **derived** from the ControlPlane
(`infrastructure.*` and the Keystone child's naming convention) rather than set
here. `spec.apiServer` and `spec.dbClean` on the child are not projected, so the
barbican operator's own uWSGI parameters and clean-up schedule stay
authoritative.

The curated fields match the Placement ones plus
[`secretStore`](#servicebarbicansecretstorespec), which no other service has.
Barbican keeps its secret material in an OpenBao (or API-compatible Vault) KV
mount rather than in its own database, so the store is part of the service's
definition.

Forbidden entirely when `services.keystone.mode` is `External` (Barbican needs
its own External-mode design), so, like `ServicePlacementSpec`, none of its
fields carry per-field External-mode forbid-rules.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `*int32` | No | `nil` | Overrides the number of Barbican API replicas. When `nil` the reconciler projects the shared default of 3 (`commonv1.DefaultReplicas`). Minimum 1. |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Barbican container image. When `nil` the reconciler derives `ghcr.io/c5c3/barbican:{spec.openStackRelease}`. When set, the validating webhook mirrors the `commonv1.ImageSpec` tag/digest XOR, so an override naming neither or both is rejected at admission. |
| `gateway` | [`*commonv1.GatewaySpec`](#gatewayspec) | No | `nil` | Exposes the projected Barbican API externally via a Gateway API HTTPRoute. When `nil` (the default) no HTTPRoute is projected and the Barbican API is reachable in-cluster only. When a `gateway` is set its `hostname` must be non-empty and a usable DNS name, enforced at admission by the validating webhook. |
| `publicEndpoint` | `string` | No | `""` | Externally routable Barbican endpoint URL (e.g. `https://barbican.127-0-0-1.nip.io:8443`). Used **only** for the K-ORC public key-manager catalog Endpoint, the URL every client resolves to store and read its secret material; it is projected into no child CR, so the validating webhook is the only gate on it. When set, it must match `^https?://`, parse to a bare origin with a host (no path, query, or fragment, since the Barbican API is served at the root and clients append the API path to the catalog URL), and be at most 512 characters; a single trailing slash is tolerated. When a `gateway` is configured the scheme must be `https` and the host must equal `gateway.hostname` (the port may differ); see [Validation Rules](#validation-rules). Without a gateway an `http://` value stays admissible for development and raises an admission warning, because every call carries the caller's scoped Keystone token and the secret payload to this URL. When empty and `gateway` is set, the reconciler derives `https://{gateway.hostname}` (the default-443 form); set it explicitly when the externally reachable port differs (e.g. a kind host-port mapping like `:8443`). |
| `databaseCredentialsMode` | `string` (`Static` \| `Dynamic`) | No | `""` (inherits `spec.infrastructure.database.credentialsMode`) | Per-service override of the ControlPlane-wide credentials mode for the managed **shared** database, so a staged migration can run Barbican on one mode while another service stays on the other. Empty (the default) **inherits** the shared mode, and is not materialized by the defaulting webhook, so "inherit" stays distinguishable from an explicit override. A `Dynamic` override is **rejected** when Barbican declares a [dedicated](#barbicandedicatedbackingservicesspec) database (dedicated is `Static`-only; set `dedicatedBackingServices.database.credentialsMode` instead, see [Credential modes](#credential-modes)) and when the shared database is **brownfield** (`clusterRef` unset); `Static` is always admitted. |
| `extraConfig` | `map[string]map[string]string` | No | `nil` | Free-form INI sections for the Barbican service. Merged **key by key** with `spec.globalExtraConfig` (this per-service value winning per key) and the merged result projected onto the Barbican child's `spec.extraConfig`. Admission runs shape, operator-owned-key, and option-catalog checks on the merged block; the always-rejected owned keys are the three credential ones (`[keystone_authtoken] password`, `[vault_plugin] approle_secret_id`, and `[vault_plugin] root_token_id`). Each arrives through an environment override at runtime, so a file value is inert and would only copy credential material into the config Secret every API pod mounts. See [ExtraConfig admission checks](#extraconfig-admission-checks). |
| `secretStore` | [`ServiceBarbicanSecretStoreSpec`](#servicebarbicansecretstorespec) | Yes | — | The secret-store backend the key manager writes its secret material to. Required: a Barbican with no store attached parks on `SecretStoresReady=False/NoDefaultSecretStore` for as long as it exists, so a store-less service block would only ever project a child that can never reach Ready. |
| `dedicatedBackingServices` | [`*BarbicanDedicatedBackingServicesSpec`](#barbicandedicatedbackingservicesspec) | No | `nil` (shares the ControlPlane-wide instances) | Opts Barbican **out** of the shared `spec.infrastructure` instances and gives it a `database` and/or `cache` of its own. Barbican consumes both classes, so it can take either or both dedicated; a declared block must name at least one. |
| `namespace` | [`*ServiceNamespaceSpec`](#service-namespaces) | No | `nil` (placed in the ControlPlane's namespace) | Places the Barbican service, and the backing services, secret store, dedicated OpenBao instance, and credential material that follow it, in a namespace of its own. Create-only: the validating webhook freezes the block after creation. See [Service Namespaces](#service-namespaces). |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | No | `nil` (the local cluster the operator runs on) | Places the Barbican service, and the backing services, dedicated OpenBao instance, and credential material that follow it, on a registered target cluster. The projected `Barbican` and `BarbicanSecretStore` CRs stay on the management cluster; the `Barbican` carries the ref verbatim and the store follows it. Requires a `namespace` block of its own, plus a `publicEndpoint` or a `gateway` so the key-manager catalog advertises an address other clusters resolve (webhook). Create-only: the validating webhook freezes the ref after creation. See [ControlPlane placement](../target-clusters.md#controlplane-placement). |

The logical database name is not exposed here. The projection forces it to
`barbican` on the child, because the OpenBao database engine grants the dynamic
Barbican credential access to that schema alone; any other name would be issued
a credential it cannot use.

Setting `services.barbican` makes the reconciler project a `KeystoneService`
registration named `{controlplane.Name}-barbican` into the namespace Barbican is
placed in: the `key-manager` catalog entry plus the `barbican` service account,
whose project `service-barbican` is created with the role `service`. It creates
its own project rather than reusing Glance's `service-glance`, since two registrations
creating one project would each adopt the other's Keystone row. That
account is the Keystone user the projected child authenticates as, so the
Barbican child is not projected until the registration reports it provisioned
(`BarbicanReady=False/WaitingForServiceRegistration` until then).

Setting `services.barbican` bounds the ControlPlane's own name too. The projected
child is `{controlplane.Name}-barbican`, and the Barbican CRD caps
`metadata.name` at **43** characters so the barbican operator's db-clean CronJob
name (`{name}-db-clean`) still fits the API server's 52-character CronJob bound.
The ControlPlane name may therefore be at most **34** characters while
`services.barbican` is set. The rule runs on create and on the update that newly
enables Barbican.

### ServiceBarbicanSecretStoreSpec

Selects the secret-store backend of the Barbican service, in one of two modes.
`dedicated` has the ControlPlane provision an OpenBao instance for this Barbican
and wire the store to it; `external` points the service at an OpenBao or
HashiCorp Vault server run outside this control plane, whose AppRole credentials
the operator only reads.

The two modes address different servers with different credentials, so one of
them must be set and not the other. A type-level CEL `XValidation` rule on
`ServiceBarbicanSecretStoreSpec` carries the union
(`has(self.dedicated) != has(self.external)`), with the message
`exactly one of dedicated or external must be set`, and the validating webhook
mirrors it, so an API server old enough to skip `x-kubernetes-validations`
cannot admit a block naming neither.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `dedicated` | [`*BarbicanDedicatedSecretStoreSpec`](#barbicandedicatedsecretstorespec) | Conditional | `nil` | Has the ControlPlane provision an OpenBao instance for this Barbican and attach the store to it. Required when `external` is unset, forbidden otherwise. |
| `external` | [`*BarbicanExternalSecretStoreSpec`](#barbicanexternalsecretstorespec) | Conditional | `nil` | Attaches the store to an OpenBao or HashiCorp Vault server provisioned outside this control plane. Required when `dedicated` is unset, forbidden otherwise. |

### BarbicanDedicatedSecretStoreSpec

Field-less. The instance name, its KV mount, and the AppRole credentials are all
derived by convention from the ControlPlane, so there is nothing left to spell
out.

The instance the operator provisions is **proving-grade**. It runs the
openbao-operator's `Development` profile at a single replica with no
PodDisruptionBudget, so any disruption of that one pod stops every secret read
and write. It is sealed by a static key held in a plain Secret
(`{instance}-unseal-key`) in the same namespace as the raft volume that key
seals, so read access to that namespace's Secrets, or a single etcd or namespace
backup, yields both the ciphertext and the key. Admission repeats this as a
warning on every apply. A production key manager belongs on
[`external`](#barbicanexternalsecretstorespec), against a hardened server with a
KMS unseal and a real replica count. See
[reconcileBarbican](./controlplane-reconciler.md#reconcilebarbican) for the
ensemble the reconciler projects around it.

### BarbicanExternalSecretStoreSpec

Addresses an OpenBao or HashiCorp Vault server provisioned outside this control
plane. The operator reads the referenced Secrets and renders the store
configuration; it never creates a mount, a policy, or an AppRole on such a
server. The field surface mirrors the barbican module's `OpenBaoServerSpec` /
`OpenBaoStoreSpec`, so a value admitted here can never be rejected downstream by
the [`BarbicanSecretStore`](../barbican/barbican-secret-store-crd.md) CRD.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `url` | `string` | Yes | — | The server's API base URL, e.g. `https://openbao.example.com:8200`. TLS is mandatory (`MinLength` 1, pattern `^https://`, mirrored by the webhook): the operator's AppRole login and every secret barbican stores travel this URL, so a plaintext scheme would put the role ID, the secret ID, and the keys and certificates the service exists to protect on the wire in the clear. |
| `credentialsSecretRef` | [`barbicanv1alpha1.SecretNameRefSpec`](../barbican/barbican-secret-store-crd.md#secretnamerefspec) | Yes | — | References the Secret holding the AppRole credentials barbican authenticates with, under the fixed data keys `role-id` and `secret-id`. The barbican operator reads it from the store's namespace, so place the Secret in the namespace the Barbican service is placed in (`services.barbican.namespace`, or the ControlPlane's own). |
| `caBundleSecretRef` | [`*barbicanv1alpha1.SecretNameRefSpec`](../barbican/barbican-secret-store-crd.md#secretnamerefspec) | No | `nil` | References the Secret holding the PEM CA bundle that authenticates the server, under the fixed data key `ca.crt`. Resolves in the same namespace as `credentialsSecretRef`. Omit it when the server presents a certificate the pods already trust through their system store. |
| `kvMountpoint` | `string` | No | `barbican` | The path the KV v2 secrets engine holding barbican's secret material is mounted at on that server. The `+kubebuilder:default` only fills an absent field, so the `MinLength` 1 marker is what rejects an explicitly empty mount path. |
| `namespace` | `string` | No | `""` | Scopes every request to an OpenBao/Vault namespace (the enterprise-style multi-tenancy header). Brownfield only, which is the only mode this block describes: a dedicated instance is provisioned at the root namespace. |

### BarbicanDedicatedBackingServicesSpec

Declares the backing-service instances the Barbican service gets for itself
instead of the ControlPlane-wide shared ones, on the same contract as
[`KeystoneDedicatedBackingServicesSpec`](#dedicatedbackingservices). Barbican
consumes both a database and a cache, so it can take either or both dedicated; a
class left unset resolves to the ControlPlane-wide instance in
`spec.infrastructure`.

The block is optional, but a declared one must name at least one class. A CEL
`XValidation` rule (`has(self.database) || has(self.cache)`) rejects an empty
block, which would request nothing, with the message
`dedicatedBackingServices must declare at least one backing-service class (database, cache)`.
The Barbican child CRD requires `spec.cache`, so the effective cache has to
resolve on every path the projection takes.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `database` | [`*commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | No | `nil` (shares `spec.infrastructure.database`) | Gives Barbican its own database cluster. In managed mode `clusterRef.name` defaults to `{controlplane}-barbican-db`. A dedicated **managed** database is **`Static`-only** — the defaulting webhook materializes `credentialsMode: Static` and an explicit `Dynamic` is rejected, for the same reason as Keystone (see [Credential modes](#credential-modes)): the OpenBao database engine is bootstrapped once per namespace against the shared cluster, so no engine role can issue credentials for a dedicated instance. Seed and rotate the credential at the OpenBao source. |
| `cache` | [`*commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | `nil` (shares `spec.infrastructure.cache`) | Gives Barbican its own cache. In managed mode `clusterRef.name` defaults to `{controlplane}-barbican-cache`. |

---

## ServiceNeutronSpec

A **curated local subset** of the knobs the ControlPlane exposes for the Neutron
network service, mirroring `ServiceKeystoneSpec` and `ServicePlacementSpec`. The
reconciler (L2) **projects** it into a `Neutron` CR; the database, cache,
Keystone endpoint, and message bus of that child are **derived** from the
ControlPlane (`infrastructure.*` and the Keystone child's naming convention)
rather than set here. `spec.apiServer`, `spec.ovnDBSync`, `spec.networkPolicy`,
`spec.autoscaling`, and `spec.logging` on the child are not projected, so the
neutron operator's own uWSGI parameters, OVN schema-sync schedule, network
policies, autoscaling, and logging stay authoritative.

Two fields have no counterpart on the other services. `ovn` is required, because
the ML2/OVN mechanism driver writes every network, subnet, and port into an OVN
Northbound database. `workerReplicas` sizes the RPC worker Deployments the child
runs beside its API.

Forbidden entirely when `services.keystone.mode` is `External` (Neutron needs its
own External-mode design), so, like `ServicePlacementSpec`, none of its fields
carry per-field External-mode forbid-rules.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `*int32` | No | `nil` | Overrides the number of Neutron API replicas. When `nil` the reconciler applies the neutron operator's own default (3). Minimum 1. |
| `workerReplicas` | `*int32` | No | `nil` | Overrides the replica count of the two RPC worker Deployments the child runs beside its API, the periodic workers and the OVN maintenance worker. Projected onto the child's `spec.workers.deployment.replicas`, which sizes both. When `nil` the reconciler applies the neutron operator's own default (3), so six worker pods. The knob exists because a single-node devstack cannot carry six idle worker pods beside the rest of the control plane. Minimum 1. |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Neutron container image. When `nil` the reconciler derives `ghcr.io/c5c3/neutron:{spec.openStackRelease}`. When set, the validating webhook mirrors the `commonv1.ImageSpec` tag/digest XOR, so an override naming neither or both is rejected at admission. |
| `gateway` | [`*commonv1.GatewaySpec`](#gatewayspec) | No | `nil` | Exposes the projected Neutron API externally via a Gateway API HTTPRoute. When `nil` (the default) no HTTPRoute is projected and the Neutron API is reachable in-cluster only. When a `gateway` is set its `hostname` must be non-empty and a usable DNS name, enforced at admission by the validating webhook (see [Validation Rules](#validation-rules)). |
| `publicEndpoint` | `string` | No | `""` | Externally routable Neutron endpoint URL (e.g. `https://neutron.127-0-0-1.nip.io:8443`). Used **only** for the K-ORC public network catalog Endpoint, the URL every client resolves to create its networks, subnets, and ports; it is projected into no child CR, so the validating webhook is the only gate on it. When set, it must match `^https?://`, parse to a bare origin with a host (no path, query, or fragment, since the Neutron API is served at the root and clients append the API path to the catalog URL), and be at most 512 characters; a single trailing slash is tolerated. When a `gateway` is configured the scheme must be `https` and the host must equal `gateway.hostname` (the port may differ); see [Validation Rules](#validation-rules). Without a gateway an `http://` value stays admissible for development and raises an admission warning, because every network call carries the caller's scoped Keystone token to this URL. When empty and `gateway` is set, the reconciler derives `https://{gateway.hostname}` (the default-443 form); set it explicitly when the externally reachable port differs (e.g. a kind host-port mapping like `:8443`). |
| `databaseCredentialsMode` | `string` (`Static` \| `Dynamic`) | No | `""` (inherits `spec.infrastructure.database.credentialsMode`) | Per-service override of the ControlPlane-wide credentials mode for the managed **shared** database, so a staged migration can run Neutron on one mode while another service stays on the other. Empty (the default) **inherits** the shared mode, and is not materialized by the defaulting webhook, so "inherit" stays distinguishable from an explicit override. A `Dynamic` override is **rejected** when Neutron declares a [dedicated](#neutrondedicatedbackingservicesspec) database (dedicated is `Static`-only; set `dedicatedBackingServices.database.credentialsMode` instead, see [Credential modes](#credential-modes)) and when the shared database is **brownfield** (`clusterRef` unset); `Static` is always admitted. |
| `extraConfig` | `map[string]map[string]string` | No | `nil` | Free-form INI sections for the network service. Merged **key by key** with `spec.globalExtraConfig` (this per-service value winning per key) and the merged result projected onto the Neutron child's `spec.extraConfig`, which carries the `neutron.conf` and `ml2_conf.ini` sections alike. Admission runs shape, operator-owned-key, and option-catalog checks on the merged block; the always-rejected owned keys are the two `[ovn]` connection strings, the six `[ovn]` client-certificate and CA paths, `[DEFAULT] transport_url`, `auth_strategy` and `api_paste_config`, `[database] connection`, `[keystone_authtoken] password`, and `[securitygroup] enable_security_group`. See [ExtraConfig admission checks](#extraconfig-admission-checks). |
| `ovn` | [`NeutronOVNSpec`](#neutronovnspec) | Yes | — | Names the OVN control plane the projected Neutron programs. Required: the ML2/OVN mechanism driver has no logical network model to write to without one, so a Neutron with no central to address would park unready for as long as it exists. |
| `dedicatedBackingServices` | [`*NeutronDedicatedBackingServicesSpec`](#neutrondedicatedbackingservicesspec) | No | `nil` (shares the ControlPlane-wide instances) | Opts Neutron **out** of the shared `spec.infrastructure` instances and gives it a `database` and/or `cache` of its own. Neutron consumes both classes, so it can take either or both dedicated; a declared block must name at least one. |
| `namespace` | [`*ServiceNamespaceSpec`](#service-namespaces) | No | `nil` (placed in the ControlPlane's namespace) | Places the Neutron service, and the database, cache, secret store, and credential material that follow it, in a namespace of its own. Create-only: the validating webhook freezes the block after creation. See [Service Namespaces](#service-namespaces). |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | No | `nil` (the local cluster the operator runs on) | Places the Neutron service, and the database, cache, secret store, and credential material that follow it, on a registered target cluster. The projected `Neutron` CR stays on the management cluster and carries the ref verbatim. Requires a `namespace` block of its own, plus a `publicEndpoint` or a `gateway` so the network catalog advertises an address other clusters resolve (webhook). Create-only: the validating webhook freezes the ref after creation. See [ControlPlane placement](../target-clusters.md#controlplane-placement). |

The logical database name is not exposed here. The projection forces it to
`neutron` on the child, whether Neutron shares the ControlPlane's database
cluster or takes a dedicated one: that is the one schema the pre-wired OpenBao
engine role grants on, so any other name would be issued a credential it cannot
use.

Setting `services.neutron` requires `spec.infrastructure.messaging` beside it.
The Neutron CRD requires `spec.messaging`, and the ControlPlane derives the
child's transport URL from the shared bus, so a network service declared without
one would project a child its own admission rejects on every pass; the webhook
reports the omission on `spec.infrastructure.messaging`. The bus is declared and
read in the ControlPlane's namespace on the management cluster, while Neutron may
run in a namespace of its own or on another cluster, so the reconciler resolves
the transport URL itself and hands the child a **brownfield** `secretRef` naming
`{controlplane.Name}-neutron-messaging` in the Neutron's own namespace on the
Neutron's own cluster. A managed `clusterRef` and a brownfield `secretRef` on the
ControlPlane both arrive there as that one Secret. When the shared bus declares
`tls`, the CA bundle is mirrored beside it as
`{controlplane.Name}-neutron-messaging-ca` and named by the child's
`messaging.tls.caBundleSecretRef`; a bus without `tls` leaves no mirror behind.

Setting `services.neutron` makes the reconciler project a `KeystoneService`
registration named `{controlplane.Name}-neutron` into the namespace Neutron is
placed in: the `network` catalog entry plus the `neutron` service account, whose
project `service-neutron` is created with the role `service`. That account is the
Keystone user the projected child authenticates as, so the Neutron child is not
projected until the registration reports it provisioned
(`NeutronReady=False/WaitingForServiceRegistration` until then).

Setting `services.neutron` bounds the ControlPlane's own name too. The projected
child is `{controlplane.Name}-neutron`, and the Neutron CRD caps `metadata.name`
at **40** characters so the neutron operator's ovn-db-sync CronJob name
(`{name}-ovn-db-sync`) still fits the API server's 52-character CronJob bound.
The ControlPlane name may therefore be at most **32** characters while
`services.neutron` is set. The rule runs on create and on the update that newly
enables Neutron.

### NeutronOVNSpec

Names the OVN control plane the projected Neutron programs.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `centralRef` | [`NeutronOVNCentralRef`](#neutronovncentralref) | Yes | — | The `OVNCentral` whose Northbound and Southbound databases the ML2/OVN mechanism driver connects to. |

### NeutronOVNCentralRef

Names an `OVNCentral` the ControlPlane only **references**. The central is
deployed outside the plane, the way the infrastructure clusters in
`spec.infrastructure` are: the ControlPlane never projects it, never updates it,
and never deletes it. The plane reads the databases the central publishes and
mirrors its readiness into the [`OVNReady`](#ovnready) condition.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | The `OVNCentral`'s name. `MinLength` 1, mirrored by the validating webhook: a reference naming nothing leaves the child with no database to program. |
| `namespace` | `string` | No | the ControlPlane's own namespace | The namespace the `OVNCentral` lives in. A lowercase alphanumeric RFC-1123 label of at most 63 characters (CRD pattern, mirrored by the webhook). The defaulting webhook fills an empty value with the ControlPlane's own namespace, which is also how the reconciler resolves an empty value, so a CR that bypassed admission addresses the same central as one that went through it. The webhook additionally bounds which namespace it may name — see below. |

A central on a **different cluster** than the network service has to publish both
databases with `externallyReachable: true`, because the Neutron pods then reach
them over the node network rather than through cluster DNS. Until it does,
`OVNReady` reports `OVNCentralNotExternallyReachable`.

The namespace must be one the ControlPlane already reaches: **its own**, or one
it claims through a `services.<service>.namespace` assignment whose `lifecycle`
is `External`. The webhook rejects every other value, from two directions:

- A **foreign** namespace, because the reference is not read-only. The
  neutron-operator mirrors the central's client `Secret` out of that namespace
  into the Neutron's, so naming another plane's central hands this one a full
  mTLS identity for its Northbound and Southbound databases — its networks,
  ports and security groups, readable and writable — and `OVNReady` relays that
  central's database addresses and status message on the way. It is the
  isolation [namespace claims](#servicenamespacespec) enforce for service
  namespaces, one field over.
- A claimed namespace with lifecycle **`Managed`**, because the teardown deletes
  such a namespace together with the plane, and the cascade would take the
  referenced central, and the logical network model in its databases, with it.

That reach rule bounds the topology: a namespace belongs to
[at most one ControlPlane](#servicenamespacespec) cluster-wide, so **one
`OVNCentral` serves one ControlPlane**. A central shared by several planes has no
shape here — a second plane can neither claim the namespace it lives in, which is
already occupied, nor reference it without claiming it. Give each ControlPlane its
own `OVNCentral`.

The rule runs on create and on the two updates that can newly violate it, the one
that enables the network service and the one that moves the ref — never on every
update, for the reason the [projected child-name bounds](#validation-rules) are
also gated: an unconditional rule can only reject a CR a previous operator build
already admitted, and one of those rejections would land on the finalizer-removal
update that completes a deletion, leaving the ControlPlane in `Terminating` for
good. `reconcileOVN` re-runs the same check as its controller-side backstop, so a
CR that never passed admission — an unregistered webhook during install, a GitOps
or etcd restore replaying stored objects — is refused at
[`OVNReady`](#ovnready) with `OVNCentralNamespaceForbidden` and the central is
never read.

### NeutronDedicatedBackingServicesSpec

Declares the backing-service instances the Neutron service gets for itself
instead of the ControlPlane-wide shared ones, on the same contract as
[`KeystoneDedicatedBackingServicesSpec`](#dedicatedbackingservices). Neutron
consumes both a database and a cache, so it can take either or both dedicated; a
class left unset resolves to the ControlPlane-wide instance in
`spec.infrastructure`.

The block is optional, but a declared one must name at least one class. A CEL
`XValidation` rule (`has(self.database) || has(self.cache)`) rejects an empty
block, which would request nothing, with the message
`dedicatedBackingServices must declare at least one backing-service class (database, cache)`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `database` | [`*commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | No | `nil` (shares `spec.infrastructure.database`) | Gives Neutron its own database cluster. In managed mode `clusterRef.name` defaults to `{controlplane}-neutron-db`. A dedicated **managed** database is **`Static`-only**: the defaulting webhook materializes `credentialsMode: Static` and an explicit `Dynamic` is rejected, for the same reason as Keystone (see [Credential modes](#credential-modes)): the OpenBao database engine is bootstrapped once per namespace against the shared cluster, so no engine role can issue credentials for a dedicated instance. Seed and rotate the credential at the OpenBao source. |
| `cache` | [`*commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | `nil` (shares `spec.infrastructure.cache`) | Gives Neutron its own cache. In managed mode `clusterRef.name` defaults to `{controlplane}-neutron-cache`. |

---

## DedicatedBackingServices

By default every service a ControlPlane manages connects to the **shared**
instances declared in [`spec.infrastructure`](#infrastructurespec): one database
cluster, one cache. Isolation between services is logical only — each service
gets its own logical database and its own credentials on the shared MariaDB, and
shares the Memcached instance.

`services.<svc>.dedicatedBackingServices` is the **opt-in** that gives a single
service backing services of its own instead. It is declared per service and per
backing-service **class**:

```yaml
spec:
  services:
    keystone:
      dedicatedBackingServices:
        database:                       # Keystone gets its own database cluster
          clusterRef:
            name: prod-keystone-db
          credentialsMode: Static
          database: keystone
          secretRef:
            name: keystone-db
          replicas: 3
          storageSize: 200Gi
        cache:                          # …and its own cache
          clusterRef:
            name: prod-keystone-cache
          backend: dogpile.cache.pymemcache
          replicas: 3
    horizon:
      dedicatedBackingServices:
        cache:                          # the dashboard gets a cache of its own
          clusterRef:
            name: prod-horizon-cache
          backend: dogpile.cache.pymemcache
          replicas: 1
```

**Omitting the block is the default and keeps today's behavior**: the service
shares the ControlPlane-wide instances. A class left unset inside a declared
block is shared too — the Keystone service above could take a dedicated database
and keep sharing the cache.

### Fields

`KeystoneDedicatedBackingServicesSpec`:

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `database` | [`*commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | No | `nil` (shares `spec.infrastructure.database`) | Gives Keystone its own database cluster. In managed mode `clusterRef.name` defaults to `{controlplane}-keystone-db`. |
| `cache` | [`*commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | `nil` (shares `spec.infrastructure.cache`) | Gives Keystone its own cache. In managed mode `clusterRef.name` defaults to `{controlplane}-keystone-cache`. |

`HorizonDedicatedBackingServicesSpec`:

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cache` | [`*commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | `nil` (shares `spec.infrastructure.cache`) | Gives the dashboard its own cache. In managed mode `clusterRef.name` defaults to `{controlplane}-horizon-cache`. |

`GlanceDedicatedBackingServicesSpec`:

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `database` | [`*commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | No | `nil` (shares `spec.infrastructure.database`) | Gives Glance its own database cluster. In managed mode `clusterRef.name` defaults to `{controlplane}-glance-db`. A dedicated **managed** database is **`Static`-only** — the defaulting webhook materializes `credentialsMode: Static` and an explicit `Dynamic` is rejected, for the same reason as Keystone (see [Credential modes](#credential-modes)): the OpenBao database engine is bootstrapped once per namespace against the shared cluster, so no engine role can issue credentials for a dedicated instance. Seed and rotate the credential at the OpenBao source. |
| `cache` | [`*commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | `nil` (shares `spec.infrastructure.cache`) | Gives Glance its own cache. In managed mode `clusterRef.name` defaults to `{controlplane}-glance-cache`. |

Glance consumes both a database and a cache, so it can take either or both
dedicated; a class left unset resolves to the ControlPlane-wide instance.

`BarbicanDedicatedBackingServicesSpec` carries the same two classes on the same
terms and is documented under
[ServiceBarbicanSpec](#barbicandedicatedbackingservicesspec); its managed
`clusterRef.name` defaults are `{controlplane}-barbican-db` and
`{controlplane}-barbican-cache`.

Declaring the block with **no class set** is rejected — it would request nothing.
Omit it entirely to share.

### Lifecycle

A dedicated instance is not a second-class one. It reuses the same `commonv1`
shapes as the shared block, so it carries the same **managed-versus-brownfield**
split — managed mode (`clusterRef`) has the ControlPlane provision the instance,
brownfield mode (`database.host` / `cache.servers`) references an externally
operated endpoint and provisions nothing — and the reconciler puts it through the
same path as a shared instance:

| Guarantee | How it holds for a dedicated instance |
| --- | --- |
| Provisioning | `reconcileInfrastructure` ensures a `MariaDB` / `Memcached` child CR per managed instance a service **resolves to**, shared and dedicated alike, sized from **that instance's** `replicas` / `storageSize`. Opting out is a genuine opt-out: a shared instance every service has left has no consumer, so it is not provisioned. When every declared database consumer — Keystone, Glance, Placement, Barbican — takes a dedicated database, the shared cluster is never created — it would otherwise be an orphan (3 Galera replicas, 100Gi by default) that nothing talks to and readiness still waits for. |
| Ownership and teardown | The child carries a controller owner reference to the ControlPlane with `blockOwnerDeletion`, so it is garbage-collected with the ControlPlane. A pre-existing CR under the same name is **adopted read-only** and never GC-claimed. |
| Readiness gating | `InfrastructureReady` is `True` only once **every** managed instance is Ready. A service whose dedicated database is still converging holds the condition `False`, so its projection is deferred — it waits for the database it actually talks to, not just for the shared cluster. |
| Credentials | The service child's `spec.database` is projected from the dedicated spec, so credential provisioning and rotation follow the instance the service connects to (see [Credential modes](#credential-modes) below). |
| Network policy | The service operators derive their database/cache egress rules from the projected `spec.database` / `spec.cache`, so they follow the dedicated instance automatically. |

### Credential modes

On the managed **shared** database, `credentialsMode: Dynamic` — short-lived,
engine-issued credentials from the OpenBao database engine — is the **default**
for every database consumer: Keystone, Glance, Placement, and Barbican. Glance is
no longer forced onto `Static`. It carries a glance-scoped engine role, the
`glance-db-dynamic` policy, and the `glance-db` auth role; Placement and Barbican
carry the same three under their own names (`placement-{namespace}`,
`placement-db-dynamic`, `placement-db` and `barbican-{namespace}`,
`barbican-db-dynamic`, `barbican-db`). All three sets come from
`deploy/openbao/bootstrap/setup-database-tenant.sh` and `setup-auth.sh`, with
the policies under `deploy/openbao/policies/`, the way Keystone's do.

The mode is resolved from ControlPlane-wide to per-service:

1. `spec.infrastructure.database.credentialsMode` is the ControlPlane-wide
   default for the shared database — `Dynamic` unless you opt out to `Static`
   (migration staging / brownfield). `Dynamic` requires `clusterRef` (managed
   mode).
2. `spec.services.<svc>.databaseCredentialsMode` **overrides** it for one
   service. Empty (the default) **inherits** the shared mode — deliberately not
   materialized by the defaulting webhook, so "inherit" stays distinguishable
   from an explicit override — so a staged migration can run one service (say
   Keystone) on `Dynamic` while another (Glance) stays `Static`, or the reverse.
   A `Dynamic` override is rejected on a service that declares a dedicated
   database or when the shared database is brownfield; `Static` is always
   admitted.

In the shared **Static** branch — the ControlPlane-wide opt-out or a per-service
`Static` override — the operator projects a KV-backed ExternalSecret reading the
per-service path (`openstack/keystone/{namespace}/{name}/db` for Keystone,
`openstack/glance/{namespace}/{name}/db` for Glance), and the credential is
**seeded and rotated at the OpenBao source** (ESO refreshes it within the hour).

A dedicated **managed** database uses `credentialsMode: Static`. The defaulting
webhook materializes it, and an explicit `Dynamic` is **rejected at admission**:
the OpenBao database engine carries exactly one connection and one role **per
namespace** (`deploy/openbao/bootstrap/setup-database-tenant.sh`), bootstrapped
against the *shared* cluster, so no engine role exists that could issue
credentials for a dedicated instance — an admitted `Dynamic` dedicated database
would wedge on an ExternalSecret that can never sync. It reads the same
per-service KV path a shared `Static` opt-out does, and a brownfield dedicated
database keeps the user-supplied `secretRef` Secret, exactly as a brownfield
shared one does.

> **Seed the KV path before you expect `Ready`.** It is seeded by **neither the
> operator nor the bootstrap** — the per-ControlPlane static seed was retired when
> managed mode moved to engine-issued credentials. A service on the `Static`
> branch (a dedicated managed database, or the shared database opted out)
> therefore reaches `Ready` only once you have seeded its per-service KV path —
> `kv-v2/openstack/keystone/{namespace}/{name}/db` for Keystone,
> `kv-v2/openstack/glance/{namespace}/{name}/db` for Glance — with `username` and
> `password` yourself; see
> [Migrate the Keystone DB to dynamic credentials](../../guides/keystone/migrate-keystone-db-to-dynamic-credentials.md)
> and [Migrate the Glance DB to dynamic credentials](../../guides/glance/migrate-glance-db-to-dynamic-credentials.md)
> for the exact `bao kv put`. Until then Keystone's `DBCredentialsReady` stays
> `False` with reason `WaitingForDBCredentialSecret` and a message naming the
> path, and Glance's projected child cannot resolve its DB secret, so
> `GlanceReady` stays `False`.

### Name collisions

Managed `clusterRef` names must be **unique per backing-service class** across
the shared block and every dedicated instance. Two instances sharing a name would
resolve to a single child CR that both projections then fight over — silently
voiding the isolation the opt-in exists for — so the validating webhook rejects
the duplicate. The derived defaults (`{controlplane}-keystone-db`,
`{controlplane}-keystone-cache`, `{controlplane}-horizon-cache`,
`{controlplane}-glance-db`, `{controlplane}-glance-cache`,
`{controlplane}-barbican-db`, `{controlplane}-barbican-cache`) never collide
with each other or with the shared defaults (`openstack-db`,
`openstack-memcached`).

### Immutability

Both the per-service block and each class within it are **frozen on a live
ControlPlane**: a service cannot be moved between shared and dedicated backing
services in either direction, because the flip would re-point the consuming
child's (immutable) database fields at a different instance while the
previously-provisioned one keeps running with the data still on it. The
create-only leaves of a declared instance (`clusterRef.name`, `database`,
`replicas`, `storageSize`, and the managed-vs-brownfield mode) are frozen the same
way the shared block's are; a cache's `replicas` stays mutable.

The freeze is **webhook-only** — deliberately carrying no CEL transition rule —
so a later transition feature (with or without data migration) can relax it to a
gated migration. An immutable CEL marker never could.

### Adding a backing-service class

Database and cache exist today. A new class (Valkey, RabbitMQ) is added as **one
more optional pointer field** on the per-service block, reusing its own canonical
`commonv1` shape. The shared-by-default / dedicated-on-request contract and the
per-service opt-in surface are unchanged by that addition — which is why the
classes are individual fields rather than one opaque block. A service's block
only ever surfaces the classes that service actually consumes, which is why
Horizon has a `cache` and no `database`.

The message bus is modeled outside this block, at
[`spec.infrastructure.messaging`](#messagingspec): one bus per ControlPlane,
enumerated at the ControlPlane's own namespace regardless of which services
consume it. Neutron consumes that shared bus, and the validating webhook
requires it beside `services.neutron`; no dedicated-bus slot exists on any
service block. A **dedicated per-service bus** is the class this recipe would
apply to for a service that needs one. It takes:

- `Messaging *commonv1.MessagingSpec` on the service's dedicated block, listed in
  that block's at-least-one-class CEL rule;
- an accessor `Dedicated<Svc>Messaging()` beside the existing
  `Dedicated<Svc>Database()` and `Dedicated<Svc>Cache()`;
- a `<svc>MessagingDeclaredAt` helper naming the spec path the instance was
  declared at, so a pending-instance condition message tells a dedicated bus from
  the shared one;
- `validateDedicated<Svc>Messaging` in the webhook: the XOR, the two required
  Secret names, and the freeze twins of the shared block;
- a `defaultMessagingLeaves(m, obj.Name+"-<svc>-rabbitmq")` call in `Default`;
- an `addMessaging(...)` call in `managedInfraInstances` at the **service's**
  namespace, gated on the service being declared. That is the enumeration rule
  every dedicated class follows, and the one the shared bus is the single
  exception to.

---

## ExternalKeystoneSpec

Declares how the control plane reaches a pre-existing, externally-operated
Keystone in **External** mode. Present only under `services.keystone.external`
and required when `services.keystone.mode` is `External`. It mirrors the
brownfield infrastructure shape at the identity level: the endpoint and,
optionally, a private-CA bundle are supplied here, and the reconciler manages
identity against that endpoint rather than deploying a Keystone workload.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `authURL` | `string` | Yes | — | Identity endpoint of the external Keystone (e.g. `https://keystone.example.com/v3`). Must match the HTTP(S) URL pattern `^https?://[^\s/]+` — an HTTP(S) shape with a **non-empty host**, so a hostless endpoint is rejected at admission — and be at most **2048** characters. Both are enforced by the CRD `+kubebuilder:validation:Pattern` / `MaxLength` markers and mirrored by the validating webhook with a full `net/url` parse. The cap bounds the one unbounded input the reconciler interpolates into `status.conditions[].message`: the pattern is end-unanchored, so without it a multi-kilobyte path could push the assembled message past the apiserver's 32768-byte cap and fail the **whole** `status.conditions` write. Neither gate is an SSRF control — admission cannot resolve where the host points, so egress restrictions remain the operator's responsibility. |
| `endpointType` | `string` (`public` \| `internal` \| `admin`) | No | `public` | Which Keystone catalog interface to authenticate against. Defaulted to `public` by both the `+kubebuilder:default` marker and the defaulting webhook. Rendered as the clouds.yaml `endpoint_type` key of **both** generated credentials Secrets. Named `endpointType` (not `interface`) because K-ORC drops gophercloud's `Interface` field and only honours `endpoint_type` (the authoritative note lives on `buildAppCredCloudsYAML` in the reconciler's `korc_cloudsyaml.go`). The selected interface must exist in the external catalog for `spec.region` — otherwise the control plane fails loud with `KORCReady=False/CatalogEndpointMismatch`. |
| `caBundleSecretRef` | [`*commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | No | `nil` | References a Secret carrying a private CA bundle the client trusts when verifying the external endpoint. The bundle is projected verbatim as the inline `cacert` key into **both** generated K-ORC credentials Secrets — K-ORC reads that key natively from the same Secret that carries `clouds.yaml`, so no mount and no upstream change are needed. `key` defaults to `ca.crt`; this default is **webhook-only** because the shared `SecretRefSpec` carries no c5c3-specific marker (the same discipline as `passwordSecretRef.key`). When the ref is set its `name` must be non-empty (CRD `MinLength` marker + webhook). A missing Secret, a missing key, or a present-but-empty key defers the mint with `KORCReady=False/WaitingForCABundle` — the last shape is the normal transient of a two-step "create the Secret, then populate it" flow. |
| `catalog` | [`*ExternalCatalogSpec`](#externalcatalogspec) | No | `nil` | Tunes how the control plane stewards the external Keystone's service catalog. Omitting it selects the conservative default: the existing identity service and all three of its endpoint interfaces are **imported** as unmanaged K-ORC CRs, and **zero** catalog entries are created. |

> **`spec.region` must match the external catalog.** `region_name` in both
> generated `clouds.yaml` documents comes from `spec.region` (defaulted
> `RegionOne`). Against an external catalog that publishes a different region,
> gophercloud fails **loud** with *"No suitable endpoint could be found in the
> service catalog"*, which the reconciler classifies onto
> `KORCReady=False/CatalogEndpointMismatch` and annotates with the effective
> `spec.region` and `endpointType`. There is no silent fallback: the operator
> cannot repair an external catalog.

> **Rotating the CA bundle is not instantaneous.** Changing (or removing)
> `caBundleSecretRef` converges both credentials Secrets on the next reconcile —
> the CA Secret is watched, so the ControlPlane wakes immediately. K-ORC's
> provider-client cache, however, keys on the parsed cloud struct only; `cacert`
> is **not** part of the cache key. The new trust store therefore takes effect
> only once the cached client expires (TTL = token lifetime / 2, ≈30 min at
> Keystone defaults). Nothing in this operator can shorten that window.

> **TLS and egress prerequisites.** An IP-based `authURL` needs an IP SAN in the
> external Keystone's server certificate; hostnames resolve through the cluster
> DNS upstream forwarder. Nothing restricts `orc-system` egress today, but a
> cluster with restrictive egress NetworkPolicies must explicitly allow K-ORC to
> reach the external endpoint and port — see the
> [reconciler reference](./controlplane-reconciler.md#external-keystone-mode-and-the-chain).

---

## ExternalCatalogSpec

Tunes External-mode catalog stewardship. Present only under
`services.keystone.external.catalog`, and entirely optional: its zero value is
the conservative default.

In **External** mode the service catalog belongs to the pre-existing
installation, so the control plane is **import-first**. It never registers the
identity service, because Keystone enforces no uniqueness on service names and a
managed registration against a populated catalog would silently duplicate rows.
Instead it imports the existing identity service and each of its endpoint
interfaces as unmanaged K-ORC CRs, which resolve read-only and write nothing.
The ControlPlane itself therefore registers **zero** catalog entries in External
mode; a genuinely new entry is declared with a `KeystoneService` CR, which owns
its own catalog rows and their teardown.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `identityServiceName` | `string` | No | `""` | Disambiguates the identity `Service` import when the external catalog carries more than one `identity`-type service. When empty the import filters on type alone. Minimum length 1, maximum 255, and no comma, mirroring K-ORC's `OpenStackName` pattern `^[^,]+$` which the value is cast to on the import filter. |

> **All three interfaces are imported; only one is required.** The `public`,
> `internal` **and** `admin` endpoints of the identity service are imported, not
> only the one `endpointType` selects. Catalog rows are listable through the
> identity API whether or not the endpoint they advertise is reachable from this
> cluster, so full visibility costs nothing. Entries for unreachable interfaces
> are informational.
>
> Only the interface `endpointType` selects **gates** `CatalogReady`. The control
> plane already authenticates through that interface, so a catalog that does not
> publish it is not the catalog K-ORC was pointed at, and its import stalling is
> the silent-empty hazard the detector exists to surface. An external
> installation is free not to publish the other two — kolla-ansible stopped
> registering the identity `admin` endpoint after Zed, and a devstack
> bootstrapped with only a public URL publishes neither of the others. Those
> imports simply stay `resolved: false` in
> [`status.catalog.imports`](#catalogimportstatus). Gating readiness on an
> interface the installation never published would hold the aggregate `Ready` at
> `False` forever for the two most common brownfield deployment tools.

> **Disambiguation is by name only.** There is no import-by-id. K-ORC's
> `ServiceImport.id` carries a `Format:=uuid` marker (the RFC 4122 dashed form)
> while Keystone mints service ids as dashless `uuid4().hex`, so an id-based
> import is rejected by K-ORC's own CRD schema and cannot be offered. A catalog
> holding two identically **named** identity services therefore cannot be
> disambiguated from the spec at all: the control plane fails loud with
> `CatalogReady=False/CatalogFailed` and the external catalog must be repaired.

> **Multi-region catalogs.** K-ORC's `EndpointFilter` carries no region field, so
> an identity service publishing one `public` endpoint per region makes the
> endpoint import match several rows. K-ORC reports that as a terminal error and
> the control plane relays it — loud, never silent — but no spec field can select
> among them today.

---

## GatewaySpec

The shared `commonv1.GatewaySpec` (`internal/common/types`), the **single source
of truth** for the Gateway API HTTPRoute knobs reused by both the ControlPlane
and the Keystone CRD — see the [Keystone CRD →
GatewaySpec](../keystone/keystone-crd.md#gatewayspec) for the same shape on the
projected child. The reconciler (L2) maps it onto the projected Keystone CR's
`spec.gateway`. As with the other `commonv1` shapes, reusing this type still
keeps the L1 API package free of a dependency on the keystone module (the
formerly hand-curated local copy was consolidated into `commonv1`; the L1
package imports only `commonv1`, never the keystone module).

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `parentRef` | [`GatewayParentRefSpec`](#gatewayparentrefspec) | Yes | — | The pre-existing Gateway the HTTPRoute attaches to. The Gateway (and GatewayClass) are platform-team infrastructure managed outside this CR. |
| `hostname` | `string` | Yes | — | Externally reachable host (SNI / Host header) the HTTPRoute matches, e.g. `keystone.example.com`. Minimum length 1. |
| `path` | `string` | No | `"/"` (Keystone operator default) | URL path prefix matched by the HTTPRoute. |
| `annotations` | `map[string]string` | No | `nil` | Passed through to the generated HTTPRoute metadata verbatim (rate limits, CORS) without extending the CRD. The route timeout is operator-managed and reads nothing from these annotations — see the [HTTPRoute resource mapping](../keystone/keystone-crd.md#httproute-resource-mapping). |

---

## GatewayParentRefSpec

References a pre-existing Gateway that the projected Keystone's HTTPRoute
attaches to. The shared `commonv1.GatewayParentRefSpec` (`internal/common/types`),
nested under [`commonv1.GatewaySpec`](#gatewayspec) and reused by both the
ControlPlane and the Keystone CRD.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | Gateway resource name. Minimum length 1. |
| `namespace` | `string` | No | `""` | Namespace of the referenced Gateway. When empty, the projected Keystone CR's namespace is assumed. |
| `sectionName` | `string` | No | `""` | Targets a specific listener on the Gateway (e.g. `https`) when it defines multiple listeners. When empty, the HTTPRoute attaches to all compatible listeners. |

---

## KORCSpec

Configures the K-ORC (OpenStack Resource Controller) integration of the control
plane. It declares how the admin application credential is
bootstrapped and rotated and which bootstrap resources are reconciled.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `adminCredential` | [`AdminCredentialSpec`](#admincredentialspec) | Yes | — | The admin OpenStack credential K-ORC uses to reconcile resources, plus the application-credential rotation policy. |
| `serviceRegistrations` | [`*ServiceRegistrationsSpec`](#serviceregistrationsspec) | No | `nil` | Which namespaces this control plane consents to standalone [`KeystoneService`](./keystoneservice-crd.md) registrations from. Optional: its zero value admits only the namespaces the control plane already owns. |

---

## ServiceRegistrationsSpec

Carries the control plane's consent to standalone `KeystoneService` CRs
registering against it.

A `KeystoneService` mints a Keystone service user with the roles its spec asks
for, so a CR in a namespace the control plane does not consent to would turn
namespace access into cloud admin. The reconciler gates every registration on
this block and reports `NamespaceNotAllowed` on every declared block of a
registration whose namespace it does not admit, so that CR's `Ready` reads
`False/NotAllReady` and it projects nothing. See the
[KeystoneService Reconciler](./keystoneservice-reconciler.md) for where that gate
sits in the registration's flow.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `allowedNamespaces` | `[]string` | No | `[]` | Namespaces **outside** the ones this control plane already owns that may register against it. `listType=set`, so the API server rejects a duplicate entry. At most 32 entries, each 1 to 63 characters matching `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, the RFC-1123 label shape of a Kubernetes namespace name. |

> **The namespaces the control plane owns need no entry.** Its own namespace and
> its [dedicated service namespaces](#service-namespaces) are admitted
> implicitly: it provisions a tenant store in each of them already, and their
> contents are its own. A nil block and an empty list are therefore identical —
> both admit exactly those, and nothing else.

> **The list is an admission gate, not a revocation tool.** Removing a namespace
> freezes reconciliation of the `KeystoneService` CRs in it, which report
> `NamespaceNotAllowed` on their declared blocks and `Ready=False/NotAllReady`,
> while every Keystone user, catalog row and delivered Secret already minted
> stays in place and keeps authenticating.
> Teardown happens only through deletion of the `KeystoneService` CR itself, so an
> edit here can never destroy credentials a running service depends on. To revoke
> a registration, delete its CR.

> **A redundant entry is a no-op, not an error.** Naming the control plane's own
> namespace or one of its dedicated service namespaces is accepted and changes
> nothing. `validateServiceRegistrations` deliberately adds no rule of its own
> here: rejecting such an entry would couple the allowlist to unrelated spec
> edits, so dropping a service's `namespace` block would invalidate an allowlist
> entry that never changed, on the very update that removed the block.

The per-tenant secret store a registration in an allowlisted namespace delivers
its credentials through is provisioned by a sub-reconciler of its own and gated
by [`RegistrationTenantStoresReady`](#registrationtenantstoresready), not by
`ESOTenantStoreReady`.

The [Register a Service the ControlPlane Does Not Manage](../../guides/register-a-foreign-service.md)
guide walks the flow on the ControlPlane devstack, including what removing a
namespace from the allowlist does and does not do.

---

## AdminCredentialSpec

Declares the admin OpenStack credential and the application-credential rotation
policy for the control plane.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cloudCredentialsRef` | [`CloudCredentialsRef`](#cloudcredentialsref) | Yes | — | References the `clouds.yaml` Secret and cloud entry K-ORC authenticates as. |
| `passwordSecretRef` | [`commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | No | name `"keystone-admin"`, key `"password"` | References the Secret holding the admin password used to (re-)mint the application credential. The defaulting webhook materializes a missing `name` to `keystone-admin` and a missing `key` to `password`, so the block may be omitted on a minimal CR. The validating webhook still enforces `passwordSecretRef.name` non-empty as defense-in-depth (see [Validation Rules](#validation-rules)), but the defaulting webhook always satisfies it before validation runs, so a user may leave it unset. The reconciler's existing `"password"` key fallback also remains. **Mode-dependent use:** the `keystone-admin` default is the **brownfield / spec-level default**. In **brownfield mode** (`database.clusterRef == nil`) this field is used verbatim — the user supplies the admin-password Secret out-of-band and the operator projects no ExternalSecret, so this reference is projected onto the Keystone CR's `bootstrap.adminPasswordSecretRef` so Keystone and K-ORC agree on the admin password source. In **managed mode** (`database.clusterRef` set) the operator instead projects a per-ControlPlane admin ExternalSecret named `{controlplane.Name}-keystone-admin-credentials` (materialising the admin password from OpenBao) and **overrides** the projected Keystone CR's `bootstrap.adminPasswordSecretRef` to point at that operator-owned per-CP Secret's `password` key — the cp-level `passwordSecretRef` is **not** used as the child's ref in managed mode. See the [managed-mode admin-password provisioning](#admincredentialspec) note below. |
| `userName` | `string` | No | `"admin"` | OpenStack admin user name the control plane authenticates as. Defaulted to `admin` by both the `+kubebuilder:default` marker and the defaulting webhook. Valid in **both** keystone modes. Rendered as the `clouds.yaml` `username` **and** used as the K-ORC admin `User` import filter the application credential's `UserRef` resolves to — see the same-user constraint below. |
| `projectName` | `string` | No | `"admin"` | OpenStack admin project name, rendered as the `clouds.yaml` `project_name`. Defaulted to `admin` by both the marker and the defaulting webhook. Valid in both modes. |
| `domainName` | `string` | No | `"Default"` | OpenStack admin domain name. Defaulted to `Default` by both the marker and the defaulting webhook. Valid in both modes. It is the K-ORC admin `Domain` import filter. **Phase-1 nuance:** the single `domainName` feeds **both** `user_domain_name` and `project_domain_name` in the generated `clouds.yaml`, so the admin user and project must live in the **same** domain; a later `userDomainName`/`projectDomainName` split is a compatible extension. |

> **Same-user constraint (hard, enforced by Keystone).** Keystone's default
> policy allows creating an application credential only for the **token's own
> user** — even an admin token is refused (HTTP 403,
> `identity:create_application_credential`) when it targets another user. The
> `clouds.yaml` `username` and the imported admin `User` the credential's
> `UserRef` points at must therefore name the same OpenStack user. Both derive
> from this single `userName` field, and a unit test
> (`TestPasswordCloudsYAMLIdentityMatchesUserImportFilter`) pins the agreement.

> **Identity edits are not re-resolved.** Changing `userName`, `projectName` or
> `domainName` on a live ControlPlane updates the K-ORC import filters in place,
> but K-ORC imports resolve **once**: the already-resolved OpenStack id is not
> looked up again. The mismatch surfaces as `KORCReady=False/CredentialDrift`
> rather than silently repointing the credential. The Kubernetes CR names of the
> imports (`{controlplane.Name}-user-admin`, `{controlplane.Name}-domain-default`)
> are stable handles and deliberately do **not** track the identity.
| `applicationCredential` | [`ApplicationCredentialSpec`](#applicationcredentialspec) | Yes | — | Policy for the K-ORC admin application credential (restriction, access rules, rotation mode). |
| `bootstrapResources` | [`[]BootstrapResourceSpec`](#bootstrapresourcespec) | No | `nil` | OpenStack resources K-ORC bootstraps alongside the admin credential (e.g. the projects/roles a fresh control plane needs). The element shape is intentionally minimal at L1; the reconciler interprets it. |

> **`passwordSecretRef` is operator-owned in managed mode.** The
> `keystone-admin` default is the **brownfield / spec-level default** only. In
> **managed mode** (`database.clusterRef` set) the operator projects a
> per-ControlPlane admin `ExternalSecret` named
> `{controlplane.Name}-keystone-admin-credentials` in the ControlPlane namespace
> that materialises the admin password from OpenBao path
> `bootstrap/{namespace}/{controlplane.Name}-keystone/admin` (canonical:
> `bootstrap/openstack/controlplane-keystone/admin`, property `password`), and
> **overrides** the projected Keystone CR's `bootstrap.adminPasswordSecretRef` to
> point at that operator-owned per-CP Secret's `password` key. The cp-level
> `passwordSecretRef` is therefore **not** used as the child's ref in managed
> mode — the source `cp.Spec` is left untouched; only the projected child's ref
> is reassigned.
>
> In **brownfield mode** (`database.clusterRef == nil`, a Host-based DB) the
> operator projects **no** admin ExternalSecret: the user supplies the
> admin-password Secret out-of-band and the cp-level `passwordSecretRef` (default
> `keystone-admin`) is projected onto the Keystone CR's
> `bootstrap.adminPasswordSecretRef` verbatim. See the
> [ControlPlane Reconciler reference](./controlplane-reconciler.md) for the
> admin-credential flow.

---

## CloudCredentialsRef

References the `clouds.yaml` Secret and the cloud entry within it that K-ORC
authenticates as.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cloudName` | `string` | No | `"admin"` | The entry in `clouds.yaml` K-ORC authenticates as. Also used by the reconciler as the conventional K-ORC `User` reference name and projected onto the catalog `Service`/`Endpoint` CRs. Defaulted to `admin` by **both** the `+kubebuilder:default` marker (normal admission) and the defaulting webhook (callers that bypass the CRD default). |
| `secretName` | `string` | No | `"k-orc-clouds-yaml"` | Name of the Secret holding the `clouds.yaml` document. Defaulted to `k-orc-clouds-yaml` by **both** the `+kubebuilder:default` marker and the defaulting webhook. The Secret is namespace-local to the ControlPlane's child namespace; because the operator enforces one ControlPlane per namespace, the shared default name does not collide across control planes. The operator (`reconcileKORC` → `ensureKORCCloudsYAMLExternalSecret`) **creates and owns** a per-ControlPlane ExternalSecret of this name in the child namespace that materialises the Secret, reading the per-CR OpenBao path — so the shared default name is safe and needs no per-CR manifest. |

---

## ApplicationCredentialSpec

Declares the K-ORC admin application-credential policy.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `restricted` | `*bool` | No | `true` | Controls whether the application credential is restricted (least-privilege, unable to create further application credentials). Defaulted to `true` by **both** the `+kubebuilder:default` marker and the defaulting webhook. The pointer distinguishes "unset" (→ default `true`) from an explicit `false`, which is preserved. See the [restricted → unrestricted inversion](#restricted--unrestricted-inversion) note. |
| `accessRules` | [`[]AccessRule`](#accessrule) | No | `nil` | Optionally narrows the application credential to a specific set of service/method/path rules. When empty, the credential is not constrained by access rules. |
| `rotation` | [`RotationSpec`](#rotationspec) | Yes | — | How the application credential is rotated. |

### restricted → unrestricted inversion

The ControlPlane spec exposes a **`restricted`** flag (the safe, least-privilege
posture). K-ORC's `ApplicationCredentialResourceSpec` exposes the inverse field,
**`unrestricted`**. The reconciler performs the inversion when projecting the
K-ORC `ApplicationCredential` CR:

```
restricted=true  → K-ORC spec.resource.unrestricted=false
restricted=false → K-ORC spec.resource.unrestricted=true
```

The same inversion is applied in reverse when reflecting the K-ORC-reported
state back into `status.adminApplicationCredential.restricted`.

---

## AccessRule

Narrows an application credential to a specific service endpoint and method,
mirroring the Keystone application-credential access-rule shape.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `service` | `string` | Yes | — | OpenStack service type the rule applies to (e.g. `"compute"`). The reconciler uses this verbatim as the name of the referenced K-ORC `Service` CR (`serviceRef`). |
| `method` | `string` | No | — | HTTP method the rule allows (e.g. `"GET"`, `"POST"`). Projected onto the K-ORC typed `HTTPMethod` enum; constrained by `+kubebuilder:validation:Enum=CONNECT;DELETE;GET;HEAD;OPTIONS;PATCH;POST;PUT;TRACE` (mirrors that enum). Optional — omitted from the projected rule when empty. |
| `path` | `string` | No | — | Request path the rule allows (e.g. `"/v2.1/servers"`). When set it must be an absolute path (`+kubebuilder:validation:Pattern=^/`). Optional — omitted from the projected rule when empty. |

---

## RotationSpec

Declares the rotation policy for the admin application credential.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `mode` | [`RotationMode`](#rotationmode) | No | `PasswordDriven` | Selects the rotation strategy. Defaulted to `PasswordDriven` by **both** the `+kubebuilder:default` marker and the defaulting webhook. |

### RotationMode

`RotationMode` is a string enum
(`+kubebuilder:validation:Enum=PasswordDriven;Scheduled;Manual`).

| Value | Status | Meaning |
| --- | --- | --- |
| `PasswordDriven` | Active (default) | Re-mints the application credential whenever the underlying admin password changes. The reconciler compares the SHA-256 of the admin password against an annotation stamped on the K-ORC `ApplicationCredential` CR; a mismatch drives a re-mint. |
| `Scheduled` | **Reserved** | Rotates the application credential on a schedule. Surfaced in the enum now so the CRD schema is stable, but the scheduled-rotation logic is deferred to a later level. |
| `Manual` | **Reserved** | Rotates only when a [`CredentialRotation`](#credentialrotationspec) CR requests it. The `CredentialRotation` flow is the mechanism; the `Manual` mode value itself is reserved at this level. |

---

## BootstrapResourceSpec

Declares an OpenStack resource K-ORC bootstraps with the control plane.
The shape is intentionally minimal at L1 — the reconciler interprets
the kind/name and applies it.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `kind` | `string` | Yes | — | The K-ORC resource kind to bootstrap. Constrained to the kinds the control plane bootstraps today by `+kubebuilder:validation:Enum=Project;Role`; widen the enum when the reconciler learns to interpret additional kinds. |
| `name` | `string` | Yes | — | Name of the bootstrapped resource. |

> **RESERVED.** No controller reads `bootstrapResources` today. For the service
> user of another OpenStack service, declare a `KeystoneService` CR instead — it
> owns the full user + project + password lifecycle, and its credentials are
> delivered into its own namespace.

---

## ControlPlaneStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the control-plane state. Each condition carries an `observedGeneration`. See [Status Conditions](#status-conditions). |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled, so a stale status is distinguishable from a current one. |
| `updatePhase` | [`UpdatePhase`](#updatephase) | Current phase of a control-plane release update. Written on every status update; fixed at `Idle` in the current implementation because the release-update state machine is reserved (the other `UpdatePhase` values are not yet set). |
| `services` | `[]ServiceStatus` | Per-service readiness of the projected service CRs. A `listType=map` list keyed by `name`, so per-service entries merge under server-side apply and can grow per-service conditions cleanly. Written on every status update with one entry per managed service in a stable order — `keystone`, `horizon`, `glance`, `placement`, `barbican`, then `neutron` — each present only when its `spec.services.<svc>` is set. Each entry's `ready` mirrors the matching `KeystoneReady` / `HorizonReady` / `GlanceReady` / `PlacementReady` / `BarbicanReady` / `NeutronReady` condition and its `release` is `spec.openStackRelease`; an unmanaged service is omitted rather than reported. See [ServiceStatus](#servicestatus). |
| `catalog` | [`*CatalogStatus`](#catalogstatus) | Observed state of the External-mode catalog imports. Nil in Managed mode, where the control plane creates the catalog entries rather than importing them. See [CatalogStatus](#catalogstatus). |

> **`updatePhase` vs the Keystone CRD's `upgradePhase`.** These field names are
> intentionally distinct: `ControlPlane.status.updatePhase` is the control-plane
> release-update machine (`Idle` / `Updating` / …/ `RollingBack`), while
> `Keystone.status.upgradePhase` is the live per-service database expand-migrate-
> contract machine (`Expanding` / `Migrating` / `RollingUpdate` / `Contracting`).
> They carry different enum vocabularies for different concerns and are not the
> same field under two names.
| `adminApplicationCredential` | [`*AdminApplicationCredentialStatus`](#adminapplicationcredentialstatus) | Observed state of the K-ORC admin application credential. |

### ServiceStatus

Reports the observed readiness of a single projected service CR.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | Service name (e.g. `keystone`); keys the `listType=map` `services` list. |
| `ready` | `bool` | Yes | Whether the projected service CR is Ready. |
| `release` | `string` | No | The OpenStack release the service currently reports installed. |

### AdminApplicationCredentialStatus

Reports the observed state of the K-ORC admin application credential.

> **Multi-instance — per-ControlPlane OpenBao path.** The minted admin
> application credential is mirrored to OpenBao at the **per-ControlPlane** path
> `openstack/keystone/{namespace}/{name}/admin/app-credential` (for the default
> deployment identity `openstack/controlplane`, this is
> `openstack/keystone/openstack/controlplane/admin/app-credential`), replacing the
> earlier flat path shared across control planes. Because the validating webhook
> permits exactly one ControlPlane per namespace, these per-CR paths are disjoint
> across namespaces by construction. See the
> [ControlPlane Reconciler reference](./controlplane-reconciler.md) for the full
> OpenBao layout and the migration from the legacy flat path.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | `string` | No | The OpenStack application-credential ID currently in use, populated by K-ORC once the credential is minted. |
| `restricted` | `bool` | No | Whether the active credential is restricted. Computed as the inverse of the K-ORC-reported `unrestricted` (falling back to the desired value while K-ORC status is empty). |
| `lastRotation` | `*metav1.Time` | No | Timestamp of the last successful rotation. (Re-)stamped to "now" whenever the recorded credential `id` changes (initial mint or re-mint); preserved once the `id` is stable. |

### CatalogStatus

Reports how the External-mode identity catalog imports resolved. Nil in Managed
mode. It is the operator-visible answer to *"did the ControlPlane find the
catalog it was pointed at?"* — the aggregate [`CatalogReady`](#catalogready)
condition says whether they all resolved, this list says which ones did.

The list is rebuilt on every pass **before** any failure return, so an unresolved
import is reported as `resolved: false` rather than omitted.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `imports` | [`[]CatalogImportStatus`](#catalogimportstatus) | No | The unmanaged K-ORC CRs importing the external identity service and its endpoint interfaces. A `listType=map` list keyed by `name`. |

### CatalogImportStatus

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | The K-ORC CR name; keys the `listType=map` `imports` list. |
| `kind` | `string` (`Service` \| `Endpoint`) | Yes | The imported K-ORC kind. |
| `interface` | `string` (`public` \| `internal` \| `admin`) | No | The catalog interface of an imported `Endpoint`; empty for the `Service` import. |
| `resolved` | `bool` | Yes | Whether K-ORC matched this import against a live catalog entry (its `Available` condition is `True` for the CR's current generation). |
| `id` | `string` | No | The OpenStack id K-ORC resolved the import to. Empty while the import is unresolved. |

### UpdatePhase

`UpdatePhase` is a string enum
(`+kubebuilder:validation:Enum=Idle;Updating;UpdatingServices;Verifying;RollingBack`).

> **DECISION:** the enum surfaces the future phases alongside the
> active ones so the CRD schema is stable across levels and does not need a
> breaking change when the update state machine is implemented. The reserved
> values below are never set by the current reconciler; they are documented so
> consumers (dashboards, `kubectl`) see the full vocabulary.

| Value | Status | Meaning |
| --- | --- | --- |
| `Idle` | Active | No update is in progress. |
| `Updating` | Active | A release update has started. |
| `UpdatingServices` | **Reserved — not yet implemented** | Per-service CRs are being updated. |
| `Verifying` | **Reserved — not yet implemented** | The control plane is verifying an update. |
| `RollingBack` | **Reserved — not yet implemented** | A failed update is being rolled back. |

---

## CredentialRotationSpec

Defines the desired state of a `CredentialRotation` — a one-shot request to
rotate a control-plane credential. The reconciler re-mints the target
credential and reports progress via status conditions.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `target` | [`RotationTarget`](#rotationtarget) | Yes | — | Which credential to rotate. |
| `keystoneService` | `string` | Conditional | — | Names the `KeystoneService` CR, in the CredentialRotation's own namespace, whose managed account's password is rotated. **Required** exactly when `target` is `serviceAccountPassword`, **forbidden** otherwise (two CEL rules; there is no CredentialRotation webhook, so CEL is the only gate). DNS-1123 subdomain, ≤ 253. |
| `bootstrap` | `bool` | No | `false` | When `true`, requests an initial **mint** of the credential rather than a rotation of an existing one. Idempotent: if the credential already exists it is a no-op. |
| `reMint` | `bool` | No | `false` | When `true`, forces the reconciler to discard the current credential and mint a fresh one even if the existing credential is still valid. The nudge is **one-shot per spec generation** (latched on `status.lastTriggeredGeneration`), so a `reMint: true` left in the spec does not re-rotate on every resync. |
| `intervalDays` | `*int32` | No | `nil` | **Deferred** — accepted by the schema but ignored by the L1 reconciler. Rotation cadence in days for scheduled rotation. Minimum: 1. |
| `preRotationDays` | `*int32` | No | `nil` | **Deferred** — accepted but ignored. Days before expiry a replacement credential is minted (the overlap window). Minimum: 0. |
| `gracePeriodDays` | `*int32` | No | `nil` | **Deferred** — accepted but ignored. Days the superseded credential remains valid after a rotation before it is revoked. Minimum: 0. |

> **DECISION:** the scheduled-rotation fields (`intervalDays`,
> `preRotationDays`, `gracePeriodDays`) surface in the CRD schema now so the
> contract is stable, but the L1 reconciler **ignores** them — scheduled
> rotation (and the two-credential pre-rotation/grace overlap) is implemented in
> a later level. They are kept here, rather than introduced via a future
> breaking schema change, so dashboards and GitOps manifests can be written
> against the final shape.

### RotationTarget

`RotationTarget` is a string enum
(`+kubebuilder:validation:Enum=adminApplicationCredential;serviceAccountPassword`).

| Value | Meaning |
| --- | --- |
| `adminApplicationCredential` | Rotates the K-ORC admin application credential. |
| `serviceAccountPassword` | Rotates the password of the managed account of the `KeystoneService` named by `spec.keystoneService`. On demand only (no auto-detect: there is no external password source); fires on an explicit `reMint`, latched to the spec generation. |

## CredentialRotationStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the rotation state. Upserted via the shared conditions helper. |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled. |
| `lastTriggeredGeneration` | `int64` | The most recent `.metadata.generation` for which an explicit `reMint` nudge was performed. Latches `reMint` to a single spec generation so a `reMint: true` left in the spec fires once per edit, not on every resync or restart. |

---

## SecretAggregate

`SecretAggregate` aggregates the Secrets produced by a control plane into a
single materialized Secret.

> **DECISION:** this is **types-only** at this level — there is **no
> controller**. The reconciler is **deferred to a later level**, and the operator RBAC
> for this kind is **read-only** (`get`/`list`/`watch`) until that reconciler
> lands, so the operator can observe `SecretAggregate` CRs without being granted
> write access to a kind it does not yet manage. The `Spec`/`Status` below are
> intentionally minimal placeholders; a later level will flesh them out.

### SecretAggregateSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `targetSecretName` | `string` | No | `""` | Name of the materialized aggregate Secret the (deferred) reconciler will produce. |

### SecretAggregateStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the aggregate state. Upserted via the shared conditions helper. |

`SecretAggregate` has no printer columns and no defaulting/validating webhook at
this level.

---

## Shared Types (from `internal/common/types`)

The `ControlPlane` reuses the canonical `commonv1` shapes imported from
`github.com/c5c3/cobaltcore/internal/common/types`. These are shared across all
CobaltCore operator CRDs and are documented in full in the Keystone reference;
this section links rather than re-documents them to keep a single source of
truth.

| Type | Used by | Reference |
| --- | --- | --- |
| `ImageSpec` | `services.keystone.image` | [Keystone CRD → ImageSpec](../keystone/keystone-crd.md#imagespec) |
| `DatabaseSpec` | `infrastructure.database` | [Keystone CRD → DatabaseSpec](../keystone/keystone-crd.md#databasespec) |
| `CacheSpec` | `infrastructure.cache` | [Keystone CRD → CacheSpec](../keystone/keystone-crd.md#cachespec) |
| `MessagingSpec` | `infrastructure.messaging` | [MessagingSpec](#messagingspec) (on this page: the ControlPlane is the first CRD to embed it) |
| `SecretRefSpec` | `korc.adminCredential.passwordSecretRef` | [Keystone CRD → SecretRefSpec](../keystone/keystone-crd.md#secretrefspec) |
| `SecretStoreRefSpec` | `secretStoreRef` (projected onto the Keystone, Horizon, Glance, Placement, and Barbican children) | [Keystone CRD → SecretStoreRefSpec](../keystone/keystone-crd.md#secretstorerefspec) |
| `PolicySpec` | `globalPolicyOverrides`, `services.keystone.policyOverrides` | [Keystone CRD → PolicySpec](../keystone/keystone-crd.md#policyspec) |

> **Note on `DatabaseSpec.tls` / `CacheSpec`:** the `commonv1` shapes carry the
> full Keystone field set, including the optional `database.tls` block.
> Those fields are part of the reused struct and are validated by the API server,
> but the ControlPlane reconciler projects the `DatabaseSpec`/`CacheSpec` onto the
> Keystone CR verbatim — TLS behavior is therefore governed by the
> [Keystone DatabaseTLSSpec](../keystone/keystone-crd.md#databasetlsspec) on the
> projected child, not re-implemented in the aggregate.

---

## Validation Rules

The c5c3 ControlPlane uses a **two-layer** validation strategy, mirroring the
Keystone discipline:

1. **CRD schema markers** (`+kubebuilder:validation:*`) enforced by the API
   server before webhooks run — patterns, enums, and minimums.
2. **The validating webhook** (`validate()`), which re-checks the schema-level
   rules **and** adds the cross-field invariants that cannot be expressed as
   simple field markers.

> **CEL `x-kubernetes-validations` on this CRD.** The ControlPlane CRD carries
> CEL `XValidation` rules inherited from the shared `commonv1` types: the
> database clusterRef/host and cache clusterRef/servers mutual-exclusivity (from
> `DatabaseSpec`/`CacheSpec`, applied to `spec.infrastructure.database` and
> `spec.infrastructure.cache`), and the policy-rule name/value constraints (from
> `PolicySpec`, applied to `spec.globalPolicyOverrides` and `spec.services.keystone.policyOverrides`).
> The required `passwordSecretRef.name` remains enforced **only by the validating
> webhook**: a cluster that disables or bypasses the webhook (e.g. envtest without
> the webhook wired up, or a direct etcd write) will not reject a `ControlPlane`
> on that one rule, only on the CEL rules and the pattern/enum/minimum markers
> below. The markers and webhook together are defense-in-depth for the fields
> that **can** be expressed at both layers.

### CRD schema markers (API-server enforced)

| Field | Rule |
| --- | --- |
| `spec.openStackRelease` | Pattern `^\d{4}\.[12]$` |
| `spec.services.keystone.publicEndpoint` | Pattern `^https?://`; MaxLength 512 (the Horizon child's bound on `websso.keystoneURL`, which this value is projected onto) |
| `spec.services.horizon.publicEndpoint` | Pattern `^https?://`; MaxLength 499 (the Keystone child's 512-character bound on `trustedDashboards[]` minus `/auth/websso/`) |
| `spec.services.keystone.mode` | Enum: `Managed`, `External`; schema default `Managed` |
| `spec.services.keystone.external.authURL` | Pattern `^https?://[^\s/]+`; MaxLength 2048 (bounds the one unbounded input interpolated into `status.conditions[].message`) |
| `spec.services.keystone.external.endpointType` | Enum: `public`, `internal`, `admin`; schema default `public` |
| `spec.services.keystone.external.caBundleSecretRef.name` | MinLength 1 (shared `SecretRefSpec` marker) |
| `spec.services.keystone.caBundleSecretRef.name` | MinLength 1 (shared `SecretRefSpec` marker) |
| `spec.services.keystone.databaseCredentialsMode` | Enum: `Static`, `Dynamic` |
| `spec.services.{keystone,horizon,glance,placement,barbican,neutron}.targetClusterRef.name` | MinLength 1; Pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$` — a DNS-1123 subdomain, from the shared `commonv1.TargetClusterRefSpec` markers, so a name no registration Secret could carry is refused before the resolver ever sees it |
| `spec.korc.adminCredential.applicationCredential.accessRules[].method` | Enum: `CONNECT`, `DELETE`, `GET`, `HEAD`, `OPTIONS`, `PATCH`, `POST`, `PUT`, `TRACE` |
| `spec.korc.adminCredential.applicationCredential.accessRules[].path` | Pattern `^/` |
| `spec.korc.adminCredential.bootstrapResources[].kind` | Enum: `Project`, `Role` |
| `spec.korc.adminCredential.applicationCredential.rotation.mode` | Enum: `PasswordDriven`, `Scheduled`, `Manual` |
| `spec.services.keystone.replicas` | Minimum: 1 |
| `spec.infrastructure.database.replicas` | Minimum: 1, schema default `3`. The webhook additionally rejects exactly `2` (Galera quorum — see below). |
| `spec.infrastructure.cache.replicas` | Minimum: 1, schema default `3` |
| `spec.infrastructure.messaging.replicas` | Minimum: 1, schema default `3` |
| `CredentialRotation spec.target` | Enum: `adminApplicationCredential`, `serviceAccountPassword` |
| `CredentialRotation spec.keystoneService` | Pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`; MinLength 1; MaxLength 253 |
| `CredentialRotation` (CEL) | `target == 'serviceAccountPassword'` ⇒ `has(self.keystoneService)` → "keystoneService is required when target is serviceAccountPassword" |
| `CredentialRotation` (CEL) | `has(self.keystoneService)` ⇒ `target == 'serviceAccountPassword'` → "keystoneService may only be set when target is serviceAccountPassword" |
| `CredentialRotation spec.intervalDays` | Minimum: 1 |
| `CredentialRotation spec.preRotationDays` | Minimum: 0 |
| `CredentialRotation spec.gracePeriodDays` | Minimum: 0 |
| `status.updatePhase` | Enum: `Idle`, `Updating`, `UpdatingServices`, `Verifying`, `RollingBack` |
| `spec.infrastructure.database` (CEL) | `has(self.clusterRef) != has(self.host)` → "exactly one of clusterRef or host must be set" |
| `spec.infrastructure.cache` (CEL) | `has(self.clusterRef) != (has(self.servers) && size(self.servers) > 0)` → "exactly one of clusterRef or servers must be set" |
| `spec.infrastructure.messaging` (CEL) | `has(self.clusterRef) != has(self.secretRef)` → "exactly one of clusterRef or secretRef must be set" |
| `spec.globalPolicyOverrides`, `spec.services.keystone.policyOverrides` (CEL) | `!has(self.rules) \|\| self.rules.all(k, size(k) > 0)` → "policy rule name must not be empty" |
| `spec.globalPolicyOverrides`, `spec.services.keystone.policyOverrides` (CEL) | `!has(self.rules) \|\| self.rules.all(k, size(self.rules[k]) > 0)` → "policy rule value must not be empty" |
| `spec.services.keystone` (CEL) | `mode == 'External'` ⇒ `has(self.external)` → "external is required when services.keystone.mode is External" |
| `spec.services.keystone` (CEL) | `has(self.external)` ⇒ `mode == 'External'` → "external may only be set when services.keystone.mode is External" |
| `spec.services.keystone` (CEL) | `mode == 'External'` ⇒ each managed-only field (`replicas`, `image`, `policyOverrides`, `extraConfig`, `rotationInterval`, `gateway`, `publicEndpoint`, `federationProxyImage`, `databaseCredentialsMode`, `targetClusterRef`, `caBundleSecretRef`) absent → "services.keystone.\<field\> is forbidden when services.keystone.mode is External" (one rule per field) |
| `spec.services.glance.replicas` | Minimum: 1 |
| `spec.services.glance.databaseCredentialsMode` | Enum: `Static`, `Dynamic` |
| `spec.services.glance.gateway.hostname` | MinLength 1 (shared `GatewaySpec` marker) |
| `spec.services.glance.publicEndpoint` | Pattern `^https?://`; MaxLength 512 (mirrors `services.keystone.publicEndpoint`; the value feeds the K-ORC image Endpoint URL) |
| `spec.services.glance.backends` | listType=map keyed by `name`; MinItems 1; MaxItems 32 |
| `spec.services.glance.backends` (CEL) | `self.filter(b, has(b.isDefault) && b.isDefault).size() == 1` → "exactly one backends entry must set isDefault" |
| `spec.services.glance.backends[].name` | Pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`; MinLength 1; MaxLength 63 |
| `spec.services.glance.backends[].type` | Enum: `S3` |
| `spec.services.glance.backends[]` (CEL) | `(self.type == 'S3') == has(self.s3)` → "the s3 block must be set exactly when type is S3" |
| `spec.services.glance.backends[].s3.endpoint` | Pattern `^https?://`; MinLength 1 |
| `spec.services.glance.backends[].s3.bucket` | MinLength 1 |
| `spec.services.glance.backends[].s3.bucketURLFormat` | Enum: `path`, `virtual` (no schema default, so an unset value serialises away) |
| `spec.services.glance.backends[].s3.credentialsSecretRef.name` | MinLength 1; MaxLength 253 |
| `spec.services.glance.importFiltering.allowedSchemes`, `.disallowedSchemes` | MaxItems 64; item Enum: `http`, `https` |
| `spec.services.glance.importFiltering.allowedHosts`, `.disallowedHosts` | MaxItems 64; item MinLength 1; item MaxLength 253 |
| `spec.services.glance.importFiltering.allowedPorts`, `.disallowedPorts` | MaxItems 64; item Minimum 1; item Maximum 65535 |
| `spec.services.glance.importFiltering` (CEL) | `!(has(self.allowedSchemes) && self.allowedSchemes.size() > 0 && has(self.disallowedSchemes) && self.disallowedSchemes.size() > 0)` → "allowedSchemes and disallowedSchemes are mutually exclusive: glance ignores the deny-list when the allow-list is non-empty" |
| `spec.services.glance.importFiltering` (CEL) | `!(has(self.allowedHosts) && self.allowedHosts.size() > 0 && has(self.disallowedHosts) && self.disallowedHosts.size() > 0)` → "allowedHosts and disallowedHosts are mutually exclusive: glance ignores the deny-list when the allow-list is non-empty" |
| `spec.services.glance.importFiltering` (CEL) | `!(has(self.allowedPorts) && self.allowedPorts.size() > 0 && has(self.disallowedPorts) && self.disallowedPorts.size() > 0)` → "allowedPorts and disallowedPorts are mutually exclusive: glance ignores the deny-list when the allow-list is non-empty" |
| `spec.services.glance.importPlugins.conversion.outputFormat` | Enum: `qcow2`, `raw`, `vmdk` |
| `spec.services.glance.importPlugins.injectMetadata.properties` | Required; MinProperties 1; MaxProperties 64; CEL: `self.all(k, size(k) <= 255 && size(self[k]) <= 255)` → "each injected property name and value must be at most 255 characters" (the CEL rule is the only marker that reaches the map's halves) |
| `spec.services.glance.importPlugins.injectMetadata.ignoreUserRoles` | MaxItems 64; item MinLength 1; item MaxLength 255 |
| `spec.korc.serviceRegistrations.allowedNamespaces` | `listType=set` (the API server rejects duplicate entries); MaxItems 32; item MinLength 1; item MaxLength 63; item Pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` |

### Validating-webhook rules

The `validate()` method accumulates all errors in a `field.ErrorList` and
returns a single `apierrors.NewInvalid` error keyed on
`GroupKind{Group: "c5c3.io", Kind: "ControlPlane"}`. It does **not**
short-circuit on the first error.

| Rule | Field Path | Error Type | Condition |
| --- | --- | --- | --- |
| Release pattern | `spec.openStackRelease` | `field.Invalid` | Value does not match `^\d{4}\.[12]$`. Defense-in-depth alongside the CRD `+kubebuilder:validation:Pattern` marker. |
| Database mutual exclusivity | `spec.infrastructure.database` | `field.Invalid` | Both `clusterRef` and `host` set, or neither (`(clusterRef != nil) == (host != "")`). Defense-in-depth alongside the CEL `XValidation` rule on `commonv1.DatabaseSpec`. |
| Cache mutual exclusivity | `spec.infrastructure.cache` | `field.Invalid` | Both `clusterRef` and `servers` set, or neither (`(clusterRef != nil) == (len(servers) > 0)`). Defense-in-depth alongside the CEL `XValidation` rule on `commonv1.CacheSpec`. |
| Messaging mutual exclusivity | `spec.infrastructure.messaging` | `field.Invalid` | The block is present with both `clusterRef` and `secretRef` set, or with neither. Defense-in-depth alongside the CEL `XValidation` rule on `commonv1.MessagingSpec`; a nil block has nothing to validate, since messaging is opt-in. |
| Messaging brownfield Secret name required | `spec.infrastructure.messaging.secretRef.name` | `field.Required` | A brownfield `secretRef` with an empty `name`. Mirrors the shared `SecretRefSpec` MinLength marker. |
| Messaging CA bundle name required | `spec.infrastructure.messaging.tls.caBundleSecretRef.name` | `field.Required` | A `tls` block with an empty `caBundleSecretRef.name`. Mirrors the shared `SecretRefSpec` MinLength marker. |
| Messaging TLS is brownfield-only | `spec.infrastructure.messaging.tls` | `field.Invalid` | A `tls` block beside a managed `clusterRef`. `ensureRabbitMQ` projects `spec.replicas` and nothing else, so the owned `RabbitmqCluster` comes up on the operator's default, plaintext listener and the requested client trust would never be honoured. **Webhook-only**: the shared `commonv1.MessagingSpec` must not carry a c5c3-specific CEL rule the keystone operator would inherit. |
| Database replicas quorum | `spec.infrastructure.database.replicas` | `field.Invalid` | Value is exactly `2`. The managed-mode projection turns any `replicas > 1` into a Galera cluster, and a two-node Galera cluster cannot hold a majority — a single pod disruption then loses quorum. Replicas must be 1 (standalone) or >=3. The CRD marker enforces only `Minimum=1` (the shared `commonv1.DatabaseSpec` must not carry a c5c3-specific CEL rule the keystone operator, which ignores `replicas`, would inherit), so this check is **webhook-only**; a zero value (defaulting bypassed) is left to the reconciler's floor. |
| Admin password Secret required | `spec.korc.adminCredential.passwordSecretRef.name` | `field.Required` | `name` is empty — without it the reconciler cannot (re-)mint the admin application credential. **Webhook-only**. |
| Gateway hostname required | `spec.services.keystone.gateway.hostname` | `field.Required` | A `gateway` is configured but its `hostname` is empty. Mirrors the `+kubebuilder:validation:MinLength=1` marker on `commonv1.GatewaySpec.Hostname`; without it the reconciler derives an empty `https:///v3` public endpoint. |
| Empty policy rule name | `spec.globalPolicyOverrides.rules[<key>]`, `spec.services.keystone.policyOverrides.rules[<key>]` | `field.Required` | A rule name (map key) is the empty string. Enforced via the shared `policy.ValidatePolicyRules`, mirrored by the CEL rule on `commonv1.PolicySpec`. |
| Empty policy rule value | `spec.globalPolicyOverrides.rules[<key>]`, `spec.services.keystone.policyOverrides.rules[<key>]` | `field.Required` | A rule value is the empty string. Enforced via the shared `policy.ValidatePolicyRules`, mirrored by the CEL rule on `commonv1.PolicySpec`. |
| External block required | `spec.services.keystone.external` | `field.Required` | `mode: External` but `external` unset. Defense-in-depth mirror of the CEL rule. |
| External authURL required/URL | `spec.services.keystone.external.authURL` | `field.Required` / `field.Invalid` | In External mode, `authURL` empty (Required), or not matching `^https?://[^\s/]+` / failing a full `net/url` parse / exceeding 2048 characters (Invalid). Mirrors the CRD required/pattern/maxLength markers. |
| External caBundle name required | `spec.services.keystone.external.caBundleSecretRef.name` | `field.Required` | `caBundleSecretRef` set with an empty `name`. Mirrors the shared `SecretRefSpec` MinLength marker. |
| Managed-only field forbidden in External mode | `spec.services.keystone.{replicas,image,policyOverrides,extraConfig,rotationInterval,gateway,publicEndpoint,federationProxyImage}` | `field.Forbidden` | The field is set while `mode: External`. Defense-in-depth mirror of the per-field CEL rules. |
| Keystone credentials-mode override forbidden in External mode | `spec.services.keystone.databaseCredentialsMode` | `field.Forbidden` | The per-service override is set while `mode: External` — no managed database is provisioned, so there is no credentials mode to override. Defense-in-depth mirror of the per-field CEL rule. |
| Dynamic credentials-mode override on a dedicated database | `spec.services.{keystone,glance,placement,barbican,neutron}.databaseCredentialsMode` | `field.Forbidden` | The override is `Dynamic` while that service declares a dedicated database: the override retargets the shared database the service does not use, and a dedicated database is `Static`-only (set `dedicatedBackingServices.database.credentialsMode` instead). `Static` stays admitted. **Cross-field, webhook-only.** |
| Dynamic credentials-mode override on a brownfield shared database | `spec.services.{keystone,glance,placement,barbican,neutron}.databaseCredentialsMode` | `field.Forbidden` | The override is `Dynamic` while the shared database is brownfield (`clusterRef` unset): the dynamic engine issues per-tenant DB users only against a cluster the operator provisions. **Cross-field, webhook-only.** |
| Federation proxy image resolvable | `spec.services.keystone.federationProxyImage` | `field.Required` / `field.Invalid` | Empty `repository`, or neither/both of `tag` and `digest`. Surfaces on the ControlPlane the operator edits rather than as an opaque `KeystoneProjectionRejected` condition on the child. |
| Dashboard public endpoint is a URL | `spec.services.horizon.publicEndpoint` | `field.Invalid` | Not an absolute HTTP(S) URL with a host. Keystone matches the derived WebSSO origin verbatim, so an unusable endpoint could never match any dashboard. |
| Dashboard public endpoint is a bare origin | `spec.services.horizon.publicEndpoint` | `field.Invalid` | Carries a path, query, or fragment (a single trailing `/` is trimmed and allowed). The `^https?://` pattern anchors only the prefix, so `https://horizon.example.com?utm=1` is schema-legal and would render the trusted origin `https://horizon.example.com?utm=1/auth/websso/` — accepted by Keystone, matched by nothing. **Webhook-only.** |
| Dashboard public endpoint agrees with the gateway | `spec.services.horizon.publicEndpoint` | `field.Invalid` | With `services.horizon.gateway` set: the scheme is not `https` (the listener terminates TLS, and Keystone POSTs the unscoped WebSSO token to this origin), or its host differs from `gateway.hostname` (Django derives the origin it sends from the request `Host` header). The port may differ. **Cross-field, webhook-only.** |
| Gateway hostname is a usable DNS name | `spec.services.{keystone,horizon}.gateway.hostname` | `field.Invalid` | A wildcard, an embedded port, a path, a scheme, a control character, or over 253 characters. Each shape either breaks the browser-facing origins derived from the hostname or overruns the children's own `MaxLength` markers on those origins. |
| External block forbidden in non-External mode | `spec.services.keystone.external` | `field.Forbidden` | `external` set while `mode` is not `External`. Defense-in-depth mirror of the CEL rule. |
| Infrastructure forbidden in External mode | `spec.infrastructure` | `field.Forbidden` | `spec.infrastructure` set while `mode: External`. **Cross-field, webhook-only** — CEL cannot span `spec.infrastructure` and `spec.services.keystone` (phase 2 relaxes this to optional). |
| Horizon forbidden in External mode | `spec.services.horizon` | `field.Forbidden` | `services.horizon` set while `mode: External` (P2 — Horizon needs its own External-mode design). **Cross-field, webhook-only.** |
| Infrastructure required in non-External mode | `spec.infrastructure` | `field.Required` | `spec.infrastructure` unset while the keystone mode is not `External` (Managed, unset mode, or `services.keystone` unset). Preserves today's contract now the Go field is an optional pointer. **Webhook-only.** |
| Glance gateway hostname required | `spec.services.glance.gateway.hostname` | `field.Required` | A `gateway` is configured but its `hostname` is empty. Mirrors the `+kubebuilder:validation:MinLength=1` marker on `commonv1.GatewaySpec.Hostname`; without it the derived public endpoint has an empty host. The same usable-DNS-name check that applies to the Keystone/Horizon gateway hostnames applies here too. |
| Glance public endpoint is a URL | `spec.services.glance.publicEndpoint` | `field.Invalid` | Not an absolute HTTP(S) URL with a host. The value is advertised verbatim as the public image catalog Endpoint and is projected into no child CR, so `https://` would register a hostless URL that no client can resolve and nothing downstream would catch. |
| Glance public endpoint is a bare origin | `spec.services.glance.publicEndpoint` | `field.Invalid` | Carries a path, query, or fragment (a single trailing `/` is allowed). The `^https?://` pattern anchors only the prefix, so `https://glance.example.com?utm=1` is schema-legal; the Glance API is served at the root and clients append the API path to the catalog endpoint, yielding `https://glance.example.com?utm=1/v2/images` and a 404 on every image call. **Webhook-only.** |
| Glance public endpoint agrees with the gateway | `spec.services.glance.publicEndpoint` | `field.Invalid` | With `services.glance.gateway` set: the scheme is not `https` (the listener terminates TLS, and every image call carries the caller's scoped Keystone token plus the image payload to this endpoint), or its host differs from `gateway.hostname` (the listener is what routes that hostname to the Glance API). The port may differ. **Cross-field, webhook-only.** |
| Glance image resolvable | `spec.services.glance.image` | `field.Invalid` | The image override sets neither or both of `tag` and `digest` (mirrors the `commonv1.ImageSpec` tag/digest XOR). |
| Glance backends non-empty | `spec.services.glance.backends` | `field.Required` | No backend is declared. Mirrors the `MinItems=1` marker. |
| Glance single default backend | `spec.services.glance.backends` | `field.Invalid` | Not exactly one entry sets `isDefault`. Mirrors the single-`isDefault` CEL rule. |
| Glance backend type/s3 union | `spec.services.glance.backends[]` | `field.Invalid` | The `s3` block is not set exactly when `type` is `S3`. Mirrors the per-entry union CEL rule on `GlanceBackendEntry`. |
| Glance backend S3 endpoint is a URL | `spec.services.glance.backends[].s3.endpoint` | `field.Invalid` | Not an HTTP(S) URL (`^https?://`, full `net/url` parse). Mirrors the CRD pattern marker. |
| Glance backend credentials Secret required | `spec.services.glance.backends[].s3.credentialsSecretRef.name` | `field.Required` | The S3 credentials Secret name is empty. |
| Glance backend child-CR name length | `spec.services.glance.backends[].name` | `field.Invalid` | The composed `GlanceBackend` child name `{controlplane.Name}-glance-{name}` would exceed the apiserver's 253-byte `metadata.name` limit. **Webhook-only** — no CRD marker can express it. |
| Glance import-filtering pairs are exclusive | `spec.services.glance.importFiltering.disallowedSchemes` / `.disallowedHosts` / `.disallowedPorts` | `field.Invalid` | Both halves of one attribute's allow/deny pair are non-empty. Mirrors the three mutual-exclusivity CEL rules with byte-identical messages, keyed on the deny-list Glance would ignore. |
| Glance import-filtering item bounds | `spec.services.glance.importFiltering.*` | `field.TooMany` / `field.NotSupported` / `field.Invalid` | A list exceeds 64 items, a scheme is neither `http` nor `https`, a host is empty or longer than 253 characters, or a port falls outside 1–65535. Mirrors the item markers on every list. |
| Glance import-filtering host INI-injection guard | `spec.services.glance.importFiltering.allowedHosts` / `.disallowedHosts` | `field.Invalid` | A host carries a newline or carriage return. The host lists are the only free-form strings in the block and are joined verbatim into `[import_filtering_opts]`, so a newline would inject arbitrary config lines. **Webhook-only** — the item markers bound length, not content. |
| Glance import-filtering posture | `spec.services.glance.importFiltering.*` | admission **warning** | Two admissible-but-misleading shapes, warned about rather than rejected because each is a legal deployment choice: a `disallowedSchemes`/`disallowedPorts` list whose sibling allow-list is unset (and therefore resolves to a non-empty operator default) is never evaluated by Glance; and an `allowedSchemes`/`allowedPorts` list widened past the operator default drops the primary `web-download` control, leaving only literal host matching. **Webhook-only**; raised identically on the `Glance` CR. |
| Glance staging bound clears the floor | `spec.services.glance.staging.sizeLimit` | `field.Invalid` | The size limit is below `1Mi`, naming no usable bound for the two scratch `emptyDir`s while the CR reads as though they were capped. The floor is `1Mi` rather than mere positivity because the schema pattern also admits the sub-byte milli suffix (`100m`, the common typo for `100Mi`), which would evict the glance-api pod on its first staged byte. Enforced by calling the glance module's exported validator, so the ControlPlane admits what the projected `Glance` child admits. **Webhook-only** — a `resource.Quantity` renders as `x-kubernetes-int-or-string`, which carries no `Minimum` marker, in either CRD. |
| Glance staging bound contradicts the opt-out | `spec.services.glance.staging.unbounded` | `field.Invalid` | `unbounded: true` renders both scratch `emptyDir`s with no `sizeLimit`, so a `sizeLimit` set alongside it names a bound nothing applies. Enforced by a CEL rule on the shared `StagingSpec` and repeated by the glance module's exported validator, so the ControlPlane and the projected `Glance` child agree. |
| Glance image-cache bound clears the floor | `spec.services.glance.imageCache.sizeLimit` | `field.Invalid` | The cache bound is below `1Mi`, which names no usable cache budget: the derived `image_cache_max_size` would then be smaller than any image, so every cached download overruns the `emptyDir` and the kubelet evicts the pod that just served it. Like the staging floor it also catches the sub-byte milli suffix (`100m` for `100Mi`). Enforced by calling the glance module's exported validator, so the ControlPlane admits what the projected `Glance` child admits. **Webhook-only** — a `resource.Quantity` renders as `x-kubernetes-int-or-string`, which carries no `Minimum` marker, in either CRD. |
| Glance image-cache interval clears the floor | `spec.services.glance.imageCache.maintenanceInterval` | `field.Invalid` | The maintenance cadence is below `1m`. Every tick walks the cache directory and its sqlite index, so a sub-minute loop spends the pod's local-disk bandwidth on maintenance instead of on the downloads the cache exists to accelerate. Enforced by the same exported validator as the size floor. **Webhook-only** — a `metav1.Duration` renders as a plain string, which carries no `Minimum` marker, in either CRD. |
| Glance import-plugin output format | `spec.services.glance.importPlugins.conversion.outputFormat` | `field.NotSupported` | The format is none of `qcow2`, `raw`, `vmdk`. Mirrors the CRD enum marker by calling the glance module's exported validator, so the ControlPlane admits what the projected `Glance` child admits; a value outside the enum would make `qemu-img` fail on every import instead of at admission. An empty value is the request for the operator default (`raw`) and is admitted. |
| Glance injected properties present | `spec.services.glance.importPlugins.injectMetadata.properties` | `field.Required` / `field.TooMany` | The property map is absent or empty (the `inject_image_metadata` plugin would have nothing to inject, so enabling it says nothing), or it carries more than 64 entries. Mirrors the `MinProperties`/`MaxProperties` markers. |
| Glance injected property name is Dict-safe | `spec.services.glance.importPlugins.injectMetadata.properties[<key>]` | `field.Invalid` | A property name is empty, starts or ends with whitespace, exceeds 255 characters, or carries a colon, comma, newline, or carriage return. The rendered `[inject_metadata_properties] inject` value is an oslo Dict that splits pairs on commas and each pair on its first colon, so either character in a name silently produces a property nobody wrote, and a newline injects whole config lines the way an unfiltered import-filtering host would. **Webhook-only** apart from the length bound, which the CEL rule on `properties` repeats — no other marker reaches a map key. |
| Glance injected property value is Dict-safe | `spec.services.glance.importPlugins.injectMetadata.properties[<key>]` | `field.Invalid` | A property value carries a comma, a newline or carriage return, leading/trailing whitespace, or exceeds 255 characters. A colon is allowed: everything after the pair's first one belongs to the value, which is what lets a URL be injected. The length bound also has the CEL counterpart, because every pair renders verbatim into the one `inject` line and an unbounded value would push the child's rendered `glance-api.conf` past the 1 MiB ConfigMap ceiling. The rest is **webhook-only.** |
| Glance ignored-role bounds | `spec.services.glance.importPlugins.injectMetadata.ignoreUserRoles` | `field.TooMany` / `field.Invalid` | The list exceeds 64 items, or an item is empty, longer than 255 characters, or carries a comma, newline, or carriage return. The rendered `ignore_user_roles` is a plain comma join, so a comma would split one role into two. The item markers bound length; the content checks are **webhook-only**. |
| Glance decompression needs a chosen staging bound | `spec.services.glance.staging.sizeLimit` | `field.Required` | `services.glance.importPlugins.decompression` is set while `services.glance.staging` leaves both `sizeLimit` and `unbounded` unset. The plugin expands the staged image by a ratio the caller picks and nothing caps the result, which makes that bound the only one in the path — and the operator default was sized against the largest download, not the largest unpacked image. Both blocks are projected onto the Glance child untouched, so the same exported validator enforces the pairing there. **Cross-field, webhook-only.** |
| Glance forbidden in External mode | `spec.services.glance` | `field.Forbidden` | `services.glance` set while `mode: External` (Glance needs its own External-mode design). **Cross-field, webhook-only.** |
| Neutron needs the shared bus | `spec.infrastructure.messaging` | `field.Required` | `services.neutron` is set while `spec.infrastructure` carries no `messaging` block: "is required when services.neutron is set: the Neutron CRD requires spec.messaging, and the ControlPlane derives the child's transport URL from the shared bus". Without it the ControlPlane would project a child its own admission rejects on every pass. A nil `spec.infrastructure` is left to the mode matrix, which already requires the block outside External mode and forbids `services.neutron` inside it. **Cross-field, webhook-only.** |
| Neutron names its OVN control plane | `spec.services.neutron.ovn.centralRef.name` | `field.Required` | The name is empty: "must be set: it names the OVNCentral the projected Neutron programs". Defense-in-depth mirror of the `MinLength=1` marker; the ML2/OVN mechanism driver writes every network, subnet, and port into that central's Northbound database. |
| OVN central namespace shape | `spec.services.neutron.ovn.centralRef.namespace` | `field.Invalid` | A non-empty namespace is not a lowercase alphanumeric RFC-1123 label; it names a Kubernetes namespace. Defense-in-depth mirror of the `Pattern` marker. |
| OVN central stays inside the plane | `spec.services.neutron.ovn.centralRef.namespace` | `field.Forbidden` | The namespace is neither the ControlPlane's own nor one it claims through a `services.<service>.namespace` assignment, or it is such a claim with `lifecycle: Managed`. It is the one ControlPlane field that addresses another namespace: consuming a foreign central mirrors that central's client certificate — a full mTLS identity for its Northbound and Southbound databases — into this plane, and a `Managed` claim is deleted with the plane, taking the referenced central and its databases along. Runs on create and on the two updates that can newly violate it — the one that enables the network service and the one that moves the ref — so a grandfathered CR stays updatable and deletable; `reconcileOVN` re-runs the check as the controller-side backstop. **Cross-field, webhook-only.** |
| Neutron public endpoint is a URL | `spec.services.neutron.publicEndpoint` | `field.Invalid` | Not an absolute HTTP(S) URL with a host. The value is advertised verbatim as the public network catalog Endpoint and is projected into no child CR, so `https://` would register a hostless URL that no client can resolve and nothing downstream would catch. |
| Neutron public endpoint is a bare origin | `spec.services.neutron.publicEndpoint` | `field.Invalid` | Carries a path, query, or fragment (a single trailing `/` is allowed). The `^https?://` pattern anchors only the prefix, so `https://neutron.example.com?utm=1` is schema-legal; the Neutron API is served at the root and clients append the API path to the catalog endpoint, yielding `https://neutron.example.com?utm=1/v2.0/networks` and a 404 on every network call. **Webhook-only.** |
| Neutron public endpoint agrees with the gateway | `spec.services.neutron.publicEndpoint` | `field.Invalid` | With `services.neutron.gateway` set: the scheme is not `https` (the listener terminates TLS, and every network call sends the caller's scoped Keystone token to this endpoint), or its host differs from `gateway.hostname` (the listener is what routes that hostname to the Neutron API). The port may differ, since Gateway API hostnames carry none. **Cross-field, webhook-only.** |
| Neutron image resolvable | `spec.services.neutron.image` | `field.Invalid` | The image override sets neither or both of `tag` and `digest` (mirrors the `commonv1.ImageSpec` tag/digest XOR). |
| Projected Neutron name bound | `metadata.name` | `field.Invalid` | The projected child `{controlplane.Name}-neutron` would exceed the 40-character `metadata.name` cap the Neutron CRD enforces, so a 33-character ControlPlane name is rejected ("would be 41 characters") and a 32-character one is accepted. Runs on create and on the update that newly declares `services.neutron`: the ControlPlane name is immutable, so on a routine update the rule could only fire against a CR a pre-upgrade operator already admitted, including the finalizer-removal update that completes its deletion. **Webhook-only.** |
| Target-cluster ref shape | `spec.services.{keystone,horizon,glance,placement,barbican,neutron}.targetClusterRef.name` | `field.Required` | The ref is set with an empty `name`. Defense-in-depth mirror of the `MinLength=1` marker on the shared `commonv1.TargetClusterRefSpec`, applied through `validation.TargetClusterRef` for a caller that bypasses CRD schema admission. |
| Placed service needs a namespace of its own | `spec.services.{keystone,horizon,glance,placement,barbican,neutron}.namespace` | `field.Required` | `targetClusterRef` is set while the service declares no `namespace` block. Every namespace maps to exactly one cluster and the ControlPlane's own stays on the local one, so the service's database, tenant store, and credential material would be provisioned in a namespace living on a different cluster than the ref names. **Cross-field, webhook-only.** |
| Placed catalog service needs a public address | `spec.services.{keystone,glance,placement,barbican,neutron}.publicEndpoint` | `field.Required` | The service is placed with neither a `publicEndpoint` nor a `gateway`. The catalog would then advertise the in-cluster Service DNS name, which resolves nowhere outside the cluster that service runs on, so every client reading the catalog from elsewhere gets an address it cannot connect to. Horizon is exempt: the dashboard is reached by a browser rather than looked up in the catalog. **Cross-field, webhook-only.** |
| Co-located services agree on the target cluster | `spec.services.{keystone,horizon,glance,placement,barbican,neutron}.targetClusterRef` | `field.Invalid` | Two services declare the same `namespace.name` but do not name the same cluster — an unplaced service counts as naming the local one. The namespace exists on exactly one cluster, together with the backing services, the tenant store, and the credential material scoped to it. The co-location rule of the `namespace` assignment, one level out. **Cross-item, webhook-only.** |
| Target-cluster ref forbidden in External mode | `spec.services.keystone.targetClusterRef` | `field.Forbidden` | The ref is set while `mode: External` — no Keystone workload is deployed, so there is nothing to place. Defense-in-depth mirror of the per-field CEL rule. |
| Placed service needs a published Keystone | `spec.services.keystone.publicEndpoint` | `field.Required` | Another service is placed on a target cluster while Keystone advertises neither a `publicEndpoint` nor a `gateway`. That service validates its tokens against Keystone and cannot resolve Keystone's in-cluster Service DNS name from another cluster, so the operator would project an empty `spec.keystoneEndpoint` onto the placed child — which the child's own CRD refuses (`MinLength=1`, `^https?://`) on every pass. The rule above only reaches a service carrying a ref of its own, so an unplaced Keystone falls outside it. **Cross-field, webhook-only.** |
| Keystone endpoint must use https across a cluster boundary | `spec.services.keystone.publicEndpoint` | `field.Invalid` | The endpoint's scheme is `http` while Keystone carries a `targetClusterRef` **or** another service is placed away from an unplaced Keystone. Either way that URL is the `auth_url` the operator renders the admin password and every service-account password next to, and those credentials cross a cluster boundary to reach it — K-ORC dials it from the management cluster when Keystone moves, a placed service dials it from the target when the service moves. The `^https?://` pattern admits `http://` for the all-local case, where the URL feeds only the bootstrap and the catalog. **Cross-field, webhook-only.** |
| Keystone CA bundle needs a placement | `spec.services.keystone.caBundleSecretRef` | `field.Forbidden` | The bundle is set while `targetClusterRef` is not. A co-located Keystone is reached over its in-cluster Service URL, which performs no TLS handshake the bundle could verify, so accepting the pair would report trust that nothing enforces — the hazard the External-mode plaintext rule rejects from the other side. **Cross-field, webhook-only.** |
| Keystone CA bundle needs every service co-located | `spec.services.keystone.caBundleSecretRef` | `field.Forbidden` | The bundle is set while a declared service does not share Keystone's target cluster. The bundle reaches K-ORC and nothing else — it is projected as the inline `cacert` key into the two K-ORC credentials Secrets — while a service on another cluster gets that same `https` URL as its projected `spec.keystoneEndpoint` and renders it into `[keystone_authtoken]`, which carries no option for a trust anchor. That service would fail every token validation with `certificate signed by unknown authority` and no field on any CR to repair it with, so the combination is refused rather than admitted as TLS-configured: publish a placed Keystone with a publicly trusted certificate, or co-locate every service with it. **Cross-field, webhook-only.** |
| Keystone CA bundle name required | `spec.services.keystone.caBundleSecretRef.name` | `field.Required` | `caBundleSecretRef` set with an empty `name`. Mirrors the shared `SecretRefSpec` MinLength marker. |
| Keystone CA bundle forbidden in External mode | `spec.services.keystone.caBundleSecretRef` | `field.Forbidden` | The bundle is set while `mode: External`, where `external.caBundleSecretRef` is the field that verifies the external endpoint. Defense-in-depth mirror of the per-field CEL rule. |
| Registration allowlist cap | `spec.korc.serviceRegistrations.allowedNamespaces` | `field.TooMany` | More than 32 entries. Defense-in-depth alongside the `MaxItems` marker, for a caller that bypasses CRD schema admission. |
| Registration allowlist entry shape | `spec.korc.serviceRegistrations.allowedNamespaces[i]` | `field.Invalid` | An entry is not a lowercase alphanumeric RFC-1123 label; it names a Kubernetes namespace. Defense-in-depth alongside the item `Pattern` marker. |
| Registration allowlist duplicates | `spec.korc.serviceRegistrations.allowedNamespaces[i]` | `field.Duplicate` | An entry repeats an earlier one. Defense-in-depth alongside the `listType=set` marker. `validateServiceRegistrations` adds **no** rule beyond these three: an entry naming a namespace the control plane already owns is a redundant no-op rather than an error, see [ServiceRegistrationsSpec](#serviceregistrationsspec). |

Whether the named target cluster is **registered** is deliberately not checked.
A registration is a runtime fact that can appear and disappear long after the
edit, so an unresolvable name surfaces per CR as a `TargetClusterUnavailable`
condition and converges once the cluster is engaged, instead of rejecting a
ControlPlane the operator can still reconcile later. See
[When the name does not resolve](../target-clusters.md#when-the-name-does-not-resolve).

### ExtraConfig admission checks

`spec.globalExtraConfig` and the per-service `services.<svc>.extraConfig` blocks
are checked at admission by the validating webhook only. There is **no CEL or
CRD-schema backstop** (beyond the External-mode forbid rule on
`services.keystone.extraConfig` above), so a cluster that bypasses the webhook
accepts a misspelled option and surfaces it at render time. The checks run on the
**merged** result — `spec.globalExtraConfig` unioned with each service's block,
the per-service value winning per key — which is the same map the reconciler
projects, so admission and projection never disagree. Findings are computed once
on the merged map and attributed back to every block that carries the offending
`(section, key)`: an error anchored at `spec.globalExtraConfig[<section>][<key>]`
and/or `spec.services.<svc>.extraConfig[<section>][<key>]`. A key present in both
blocks yields one error per path. Because `spec.globalExtraConfig` reaches every
declared INI service, a Keystone-only option placed there is rejected while
`services.glance` is declared; the fix is to move it to
`services.keystone.extraConfig`.

Two families run, with different gating:

- **Shape and ownership** — un-gated, on every create **and** every update, since
  it depends on nothing a regenerated catalog can invalidate. It rejects empty
  section or key names in any INI block, and empty or non-Python-identifier
  (`[A-Za-z_][A-Za-z0-9_]*`) setting names in the Horizon block. It rejects the
  operator-owned keys the ControlPlane projects itself: Glance's
  `[keystone_authtoken] password`, its six `[import_filtering_opts]` keys
  (always — those are owned by `services.glance.importFiltering`, which is
  the only surface that runs the exclusivity rules, the host INI guard, and the
  loosened-filter warning), its three `[DEFAULT] image_cache_*` keys (always
  — owned by `services.glance.imageCache`, and each override ends in an evicted
  glance-api pod or in database rows nothing reclaims) and its four image-import
  plugin keys (`[image_import_opts] image_import_plugins`,
  `[image_conversion] output_format`, `[inject_metadata_properties] inject` and
  `ignore_user_roles`; always — owned by `services.glance.importPlugins`, the
  only surface that holds the decompression-before-conversion order, the
  output-format enum, and the oslo Dict rules on the injected property names),
  Horizon's `SECRET_KEY`
  and every
  WebSSO / multi-domain setting (always — the ControlPlane projects `websso` /
  `multiDomain` dynamically from the attached identity backends, so a collision is
  not decidable at admission), and Keystone's `[federation] trusted_dashboard`
  **only when** the ControlPlane derives a dashboard endpoint from
  `services.horizon`. With no Horizon block that Keystone key is admitted with a
  warning, supporting an externally-run dashboard doing WebSSO against the managed
  Keystone. Every other operator-owned key is honored but produces an admission
  warning naming the key, its owner, its impact, and the contributing block
  path(s).
- **Option catalog** — on every create, and on update **only when a catalog input
  changed**: either INI block, `spec.openStackRelease`, `services.keystone.image`,
  or a newly-declared service. So a stored CR whose `extraConfig` went
  stale-invalid against a regenerated catalog is not rejected by an unrelated edit
  such as a replicas bump. The merged block of each declared INI service is
  validated against that service's per-release option catalog embedded in the
  operator (Keystone's release resolved from `services.keystone.image.tag` when
  the image override is set, otherwise `spec.openStackRelease`; Glance's from
  `spec.openStackRelease`). An unknown section or option is rejected with the
  section and key named; a deprecated-but-accepted option is admitted with a
  warning naming its replacement. Plugin-registered sections are rejected as
  unknown, because the ControlPlane has no `plugins` field and never sets
  `spec.plugins` on a child; configure plugin sections on the service CR directly.
  The check **fails open** with exactly one warning per service (and no error)
  when no catalog resolves: a digest-pinned image, an unparseable tag, or a
  release the operator build ships no catalog for.

The network service's catalog is resolved from `spec.openStackRelease` alone and
exempts no sections: it is the flat union of the `neutron.conf`, `ml2_conf.ini`,
and `neutron_ovn_metadata_agent.ini` generator files, so it already enumerates
every section the child configures and the exemptions stay keys-only. Neutron's
always-rejected owned keys are the two `[ovn]` connection strings
(`ovn_nb_connection`, `ovn_sb_connection`), the six `[ovn]` client-certificate and
CA paths (`ovn_nb_private_key`, `ovn_nb_certificate`, `ovn_nb_ca_cert` and their
`ovn_sb_` twins), `[DEFAULT] transport_url`, `[DEFAULT] auth_strategy`,
`[DEFAULT] api_paste_config`, `[database] connection`,
`[keystone_authtoken] password`, and `[securitygroup] enable_security_group`.
Each one either points the mechanism driver at a logical model the operator does
not own, copies credential material into the config Secret every pod mounts, or
takes the API off token validation and the instance ports off their OVN ACLs, and
all of it lands the moment the pods load the rendered file.

The catalogs consulted here are the ones embedded in the **c5c3-operator** build.
A deployed service operator of a different build may embed a different catalog;
the child service webhook remains the defense-in-depth check for that skew, and a
skewed rejection surfaces as `KeystoneProjectionRejected` /
`GlanceProjectionRejected` / `NeutronProjectionRejected` on the ControlPlane's
conditions.

::: warning A newly-rejected key wedges a stored ControlPlane
The ownership family runs on **update** as well as create, so a key that becomes
Rejected in a later operator build blocks every subsequent write to a
ControlPlane that already carries it — including writes that touch nothing near
it, such as an image bump or a replica change. The merged `extraConfig` is also
projected onto the child unconditionally, so the child's own webhook refuses the
projection and the ControlPlane reports `GlanceReady` / `KeystoneReady` `False`
with reason `GlanceProjectionRejected` / `KeystoneProjectionRejected` until the
key is removed.

The four Glance image-import plugin keys are the current instance: setting
`[image_import_opts] image_import_plugins` through `extraConfig` was the only
way to enable those plugins before `services.glance.importPlugins` existed. Move
such settings onto the typed field **before** rolling the operator — see
[Migrate these keys out of `extraConfig`](../glance/glance-crd.md#importpluginsspec).
:::

### Update-only immutability rules

On **update** the validating webhook additionally rejects changes to the
create-only fields below, accumulating them into the same `field.ErrorList` as
the spec checks above. These fields are **webhook-only** (the affected leaves
live in the shared `commonv1.DatabaseSpec`/`CacheSpec`, which the keystone
operator reuses and which must not carry c5c3-specific CEL immutability markers).
Flipping the database/cache mode or renaming a managed `clusterRef` would leave
the previously-projected MariaDB/Memcached child (and its per-ControlPlane
credential) orphaned and owned until the ControlPlane is deleted; renaming
`cloudCredentialsRef.secretName` would leak the previously-projected K-ORC
clouds.yaml ExternalSecret. The database **name** and the **region** are also
immutable: both are projected verbatim into the Keystone child's now-immutable
`spec.database.database` / `spec.bootstrap.region`, so a rename here would make
the next reconcile attempt an update the Keystone CEL rule rejects, wedging the
loop — rejecting it at the ControlPlane layer surfaces a clean error instead.

The webhook additionally rejects an **openStackRelease downgrade**: a monotonic
upgrade check parses the `YYYY.N` release into `(year, minor)` and rejects a
lower tuple while allowing upgrades and same-release no-ops. Keystone DB
migrations are forward-only, so a downgrade would project an older image against
an already-migrated schema — an unrecoverable state.

The webhook also **gates the keystone-mode transition**. Both directions are
rejected with **distinct messages** — and deliberately as *rejections*, not
immutable markers, because `External → Managed` must become a *gated* takeover
in phase 3 (an immutable marker could never be relaxed to a gated transition):

- `Managed → External` is rejected outright — adopting an existing installation
  must be a fresh External-mode ControlPlane, not an in-place flip of a live
  one.
- `External → Managed` (or away from External by removing the keystone service)
  is rejected with a message naming the **reserved phase-3 takeover**.

An **infrastructure presence flip** (adding or removing the whole
`spec.infrastructure` block on update while the mode is unchanged) is rejected
independently as defense-in-depth for webhook-bypassed states.

The **messaging block is a one-way add in both modes**. Declaring
`spec.infrastructure.messaging` on a live ControlPlane is always allowed;
removing it is not.

The **managed** half is the direct one: the owned `RabbitmqCluster` holds the
queues, so the removal would leave it running and unreferenced — delete the
ControlPlane to tear that bus down. The **brownfield** half provisions nothing,
so its removal strands no state on its own, but admitting it would turn the mode
freeze below into a two-step operation: null the brownfield block, then re-add it
with a `clusterRef` as an ordinary opt-in, and the ControlPlane has reached
exactly the state the mode freeze exists to reject — a fresh, empty
`RabbitmqCluster` every consumer renders a transport URL for while the queues
stay on the external broker the `secretRef` named — without a single admission
error. Neither spec nor status remembers the mode a previous revision declared,
so the one-step rejection is only worth having while the two-step path is closed
too. Re-pointing a brownfield bus at a different broker never needed the removal:
`secretRef` stays mutable.

The mode and a managed `clusterRef.name` are frozen the way the cache ones are.
`replicas`, `secretRef` and `tls` stay mutable: `ensureRabbitMQ` re-projects the
replica count onto the owned CR on every pass — converging a scale-**down** by
recreating the cluster, since the RabbitMQ Cluster Operator refuses an in-place
shrink — and the brownfield Secret and the client trust are re-read on every
reconcile. Freezing the scale-down instead would leave an oversized bus on a
constrained cluster unrepairable: the broker never reaches its declared replica
count, so `InfrastructureReady` never goes `True`, and the only remaining action
would be deleting the whole ControlPlane. The destructive half of that mutability
is gated in the reconciler rather than at admission: a shrink runs only when the
ControlPlane carries `c5c3.io/allow-messaging-recreate: "true"`, so the repair
path stays open while an unintended decrement is refused with the broker intact.

The freeze is webhook-only, with no CEL transition rule, so a later teardown or
migration feature can relax it.

| Rule | Field Path | Condition |
| --- | --- | --- |
| Managed → External rejected | `spec.services.keystone.mode` | Old not External, new External — "cannot be changed to External" |
| External → Managed rejected | `spec.services.keystone.mode` | Old External, new not External — names the reserved phase-3 takeover |
| Infrastructure presence immutable | `spec.infrastructure` | The block was added or removed (presence flip) while the mode is unchanged |
| Database mode immutable | `spec.infrastructure.database` | `clusterRef` nil-ness changed (managed ↔ brownfield) |
| Database clusterRef.name immutable | `spec.infrastructure.database.clusterRef.name` | Both managed, but the name changed |
| Database name immutable | `spec.infrastructure.database.database` | The database name changed |
| Database replicas immutable | `spec.infrastructure.database.replicas` | The value changed. `replicas` is projected into the managed MariaDB child's replica count and derived Galera topology, so a live edit would drive a destructive Update on the owned cluster (toggling Galera off or scaling a running Galera cluster down); the topology can only be changed safely by recreating the control plane. |
| Cache mode immutable | `spec.infrastructure.cache` | `clusterRef` nil-ness changed (managed ↔ brownfield) |
| Cache clusterRef.name immutable | `spec.infrastructure.cache.clusterRef.name` | Both managed, but the name changed |
| Messaging removal rejected | `spec.infrastructure.messaging` | A block was declared on the old revision and is absent on the new one, in **either** mode: "spec.infrastructure.messaging cannot be removed once declared: …". Adding the block is allowed; the brownfield removal is rejected because admitting it would launder the mode freeze below into a two-step flip. |
| Messaging mode immutable | `spec.infrastructure.messaging` | `clusterRef` nil-ness changed (managed ↔ brownfield): "messaging mode (managed clusterRef vs brownfield secretRef) is immutable" |
| Messaging clusterRef.name immutable | `spec.infrastructure.messaging.clusterRef.name` | Both managed, but the name changed: "managed messaging clusterRef.name is immutable" |
| Cloud secretName immutable | `spec.korc.adminCredential.cloudCredentialsRef.secretName` | The value changed |
| Region immutable | `spec.region` | The region changed |
| Service target cluster immutable | `spec.services.{keystone,horizon,glance,placement,barbican,neutron}.targetClusterRef` | The ref was added, removed, or renamed on a service the old revision already declared; the message contains `targetClusterRef is immutable`, the same string the workload CRDs' CEL transition rules pin. Re-pointing a live service leaves its workload, its database, its tenant store, and its credential material on the cluster they were created on, and nothing in the following reconcile moves or reaps them. Webhook-only, with **no** CEL transition rule, so a migration between clusters can be gated later rather than being blocked forever — the same rationale as the `namespace` freeze. A service the old revision did **not** declare may appear placed: that is the service's creation, not a move. |
| Release downgrade rejected | `spec.openStackRelease` | New release `(year, minor)` is lower than the old (upgrades and same-release updates allowed) |

---

## Webhooks

The `ControlPlaneWebhook` struct implements both defaulting and validating
admission webhooks for the `ControlPlane` CRD via the typed-generic
`admission.Defaulter[*ControlPlane]` and `admission.Validator[*ControlPlane]`
interfaces from controller-runtime. `CredentialRotation` and `SecretAggregate`
have no webhook at this level.

The struct carries a `Client client.Reader`, injected at startup with the
manager's **uncached API reader** (`mgr.GetAPIReader()`). `ValidateCreate` uses
it to enforce one ControlPlane per namespace; reading the API server directly
ensures concurrent or cache-sync-window CREATEs cannot both pass the check
against an empty informer cache. The spec-level `validate()` rules do not touch
the client.

### Registration

```go
func (w *ControlPlaneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error
```

Registers both webhooks with the manager using
`builder.WebhookManagedBy[*ControlPlane]`. The generated webhook paths are
`/mutate-c5c3-io-v1alpha1-controlplane` (mutating) and
`/validate-c5c3-io-v1alpha1-controlplane` (validating); both use
`failurePolicy=fail`, `sideEffects=None`, and `admissionReviewVersions=v1`.
Both webhooks fire on `create`/`update` only. `delete` is deliberately **not**
registered: the webhook is served in-process by the operator, so with
`failurePolicy=fail` a `delete` rule would let a down operator block CR — and
thereby namespace — deletion.

### Defaulting Webhook

```go
func (w *ControlPlaneWebhook) Default(_ context.Context, obj *ControlPlane) error
```

Fills only zero-valued fields with their documented defaults, leaving any
explicit value untouched. It is **idempotent**: applying it twice produces the
same result. The defaults split across **two layers** that do **not** uniformly
overlap:

- **Dual-layer defaults** — also expressed as a `+kubebuilder:default` marker on
  the corresponding spec field, so the marker covers the normal admission path
  and the webhook covers callers that bypass the CRD default. These are `region`,
  `cloudCredentialsRef.secretName`, `applicationCredential.restricted`,
  `applicationCredential.rotation.mode`, `cloudCredentialsRef.cloudName`,
  `services.keystone.mode` (→ `Managed`), `services.keystone.external.endpointType`
  (→ `public`, when the external block is present), and the admin identity fields
  `adminCredential.userName` / `.projectName` (→ `admin`) / `.domainName`
  (→ `Default`).

**Mode-aware infrastructure defaulting.** The webhook defaults
`services.keystone.mode` to `Managed` first (when a keystone block is present
with an empty mode), then branches on the mode:

- In **External** mode it **does not invent** any managed database/cache
  `clusterRef` — `spec.infrastructure` is left unset (the validating webhook
  forbids it in External mode) — and only defaults the external block's own
  `endpointType` (→ `public`) and `caBundleSecretRef.key` (→ `ca.crt`, when the
  ref is set).
- In **Managed** mode (or when the keystone service is unset) it materializes
  and defaults the shared backing services exactly as before, so a minimal
  managed CR still round-trips unchanged.
- **Webhook-only defaults** — materialized by the webhook with **no**
  `+kubebuilder:default` marker. These are the shared-`commonv1`-leaf defaults
  (`database.database`, `database.secretRef.name`, `cache.backend`,
  `passwordSecretRef.name`, `passwordSecretRef.key`) and the two managed
  `clusterRef` names (`openstack-db`, `openstack-memcached`). They carry no
  marker because the `commonv1` `DatabaseSpec` / `CacheSpec` / `SecretRefSpec`
  types are reused by the Keystone CRD, and a c5c3-specific `+kubebuilder:default`
  on those shared types would leak into Keystone's CRD. The two managed
  `clusterRef` names are **brownfield-guarded**: they are only invented when the
  brownfield discriminator (`database.host` / `cache.servers`) is unset, so the
  database/cache XOR validation still passes for a brownfield CR.

**Messaging is opt-in and never materialized.** The webhook does not construct
`spec.infrastructure.messaging`, so a ControlPlane that omits it is admitted with
no message bus at all. When the block is present, `defaultMessagingLeaves` fills
its well-known leaves on the same brownfield discipline as the database:
`clusterRef.name` (`openstack-rabbitmq`) is invented only when `secretRef` is
unset, a brownfield `secretRef.key` becomes `transport_url`, and a
`tls.caBundleSecretRef.key` becomes `ca.crt`.

The defaulting constants in `controlplane_webhook.go` (e.g. `DefaultRegion`
`"RegionOne"`, `DefaultDatabaseName` `"keystone"`, `DefaultCacheBackend`
`"dogpile.cache.pymemcache"`) are the single source of truth shared with the
markers' documented values where a marker also exists.

| Field | Condition | Default Value | Mechanism |
| --- | --- | --- | --- |
| `spec.region` | `== ""` | `"RegionOne"` | Marker + webhook |
| `spec.korc.adminCredential.cloudCredentialsRef.secretName` | `== ""` | `"k-orc-clouds-yaml"` | Marker + webhook |
| `spec.korc.adminCredential.applicationCredential.restricted` | `== nil` | `true` (pointer set to `true`; an explicit `false` is preserved) | Marker + webhook |
| `spec.korc.adminCredential.applicationCredential.rotation.mode` | `== ""` | `PasswordDriven` | Marker + webhook |
| `spec.korc.adminCredential.cloudCredentialsRef.cloudName` | `== ""` | `"admin"` | Marker + webhook |
| `spec.infrastructure.database.database` | `== ""` | `"keystone"` | Webhook-only |
| `spec.infrastructure.database.secretRef.name` | `== ""` | `"keystone-db"` [†](#secretref-default-note) | Webhook-only |
| `spec.infrastructure.database.clusterRef.name` | `host == ""` (managed mode) | `"openstack-db"` | Webhook-only, brownfield-guarded |
| `spec.infrastructure.cache.backend` | `== ""` | `"dogpile.cache.pymemcache"` | Webhook-only |
| `spec.infrastructure.cache.clusterRef.name` | `len(servers) == 0` (managed mode) | `"openstack-memcached"` | Webhook-only, brownfield-guarded |
| `spec.infrastructure.messaging.clusterRef.name` | messaging block present, `secretRef == nil` (managed mode) | `"openstack-rabbitmq"` | Webhook-only, brownfield-guarded |
| `spec.infrastructure.messaging.secretRef.key` | messaging block present, `secretRef` set, `key == ""` | `"transport_url"` | Webhook-only |
| `spec.infrastructure.messaging.tls.caBundleSecretRef.key` | messaging block present, `tls` set, `key == ""` | `"ca.crt"` | Webhook-only |
| `spec.korc.adminCredential.passwordSecretRef.name` | `== ""` | `"keystone-admin"` | Webhook-only |
| `spec.korc.adminCredential.passwordSecretRef.key` | `== ""` | `"password"` | Webhook-only |
| `spec.services.keystone.mode` | `== ""` (keystone block present) | `Managed` | Marker + webhook |
| `spec.services.keystone.external.endpointType` | `== ""` (External mode, external present) | `public` | Marker + webhook |
| `spec.services.keystone.external.caBundleSecretRef.key` | `== ""` (ref present) | `"ca.crt"` | Webhook-only |
| `spec.services.keystone.caBundleSecretRef.key` | `== ""` (ref present) | `"ca.crt"` | Webhook-only |
| `spec.korc.adminCredential.userName` | `== ""` | `"admin"` | Marker + webhook |
| `spec.korc.adminCredential.projectName` | `== ""` | `"admin"` | Marker + webhook |
| `spec.korc.adminCredential.domainName` | `== ""` | `"Default"` | Marker + webhook |
| `spec.services.glance.dedicatedBackingServices.database.clusterRef.name` | managed dedicated database declared, `== ""` | `{controlplane}-glance-db` (and `credentialsMode` → `Static`) | Webhook-only, brownfield-guarded |
| `spec.services.glance.dedicatedBackingServices.cache.clusterRef.name` | managed dedicated cache declared, `len(servers) == 0` | `{controlplane}-glance-cache` | Webhook-only, brownfield-guarded |
| `spec.services.neutron.ovn.centralRef.namespace` | `== ""` (a `neutron` block is declared) | the ControlPlane's own namespace | Webhook-only |
| `spec.services.neutron.dedicatedBackingServices.database.clusterRef.name` | managed dedicated database declared, `== ""` | `{controlplane}-neutron-db` (and `credentialsMode` → `Static`) | Webhook-only, brownfield-guarded |
| `spec.services.neutron.dedicatedBackingServices.cache.clusterRef.name` | managed dedicated cache declared, `len(servers) == 0` | `{controlplane}-neutron-cache` | Webhook-only, brownfield-guarded |
| `spec.services.{keystone,horizon,glance,placement,barbican,neutron}.namespace.lifecycle` | `== ""` (a `namespace` block is declared) | `Managed` | Marker + webhook |

The `centralRef.namespace` default is a convenience only.
`NeutronOVNCentralNamespace()` reads an empty value as the ControlPlane's
namespace too, so a CR that bypassed the webhook is reconciled against the same
`OVNCentral` as one that went through it.

<a id="secretref-default-note"></a>
> **† `database.secretRef.name` default — managed-mode convenience name only.**
> The webhook still defaults `secretRef.name` to the `keystone-db` value
> (unchanged), but that default is no longer the Secret Keystone consumes. In
> **managed mode** `database.secretRef` is **operator-owned**: `reconcileDBCredentials`
> materialises a per-ControlPlane `ExternalSecret` and `reconcileKeystone`
> overrides the projected Keystone CR's `spec.database.secretRef` to the
> operator-owned Secret `{controlplane.Name}-keystone-db-credentials` (key
> `"password"`) — see [managed-mode provisioning](#infrastructurespec). In
> production the bare name `keystone-db` does not resolve to any cluster
> Secret (only the kind overlay ships a `keystone-db` ExternalSecret, pinned
> to the default identity's path for standalone Keystone instances); a
> managed ControlPlane never consumes it either way.
> A **brownfield** ControlPlane (`database.host` set,
> `clusterRef == nil`) **MUST supply** its own `database.secretRef` Secret
> out-of-band; the operator projects no ExternalSecret in brownfield mode.

> **Operational contract.** The webhook only materializes the Secret
> *names and references* — it never invents credential material. In **managed
> mode** (`database.clusterRef` set) the operator itself projects the admin
> password: it creates a per-ControlPlane admin `ExternalSecret`
> (`{controlplane.Name}-keystone-admin-credentials`) materialising the password
> from OpenBao and **overrides** the projected Keystone CR's
> `bootstrap.adminPasswordSecretRef` onto it, so the cp-level `passwordSecretRef`
> (default `keystone-admin`) is **not** the Secret the child consumes — see the
> [`passwordSecretRef` managed-mode note](#admincredentialspec) above. In
> **brownfield mode** (`database.clusterRef == nil`) the operator projects no
> admin ExternalSecret, so a ControlPlane that omits `spec.korc` (or
> `spec.infrastructure.database.secretRef`) relies on the cluster operator having
> **pre-seeded** the referenced Secrets in the ControlPlane's namespace before the
> credential sub-reconcilers can advance: the admin password Secret
> (`keystone-admin`, key `password`) and the K-ORC `clouds.yaml`
> ExternalSecret/Secret (`k-orc-clouds-yaml`). The infrastructure layer seeds
> these (see the [quick-start](../../quick-start-controlplane.md)). A missing
> admin password Secret degrades to `KORCReady=False` / `WaitingForAdminPassword`,
> and a not-yet-synced `clouds.yaml` to `AdminCredentialReady=False` /
> `WaitingForCloudsYaml` — never a silent authentication. A `clouds.yaml` that is
> synced but stale (a re-mint revoked the old credential while ESO has not yet
> re-materialised the Secret) degrades to `AdminCredentialReady=False` /
> `WaitingForCloudsYamlSync`: `reconcileAdminCredential` semantically compares the
> materialised Secret (by parsed application-credential id+secret) against the
> freshly assembled credential and forces an ESO re-sync, so the gate never passes
> against a revoked credential.
> `TestIntegration_MinimalManagedToReady` encodes this contract by pre-creating
> those Secrets at the defaulted names.

### Validating Webhook

```go
func (w *ControlPlaneWebhook) ValidateCreate(_ context.Context, obj *ControlPlane) (admission.Warnings, error)
func (w *ControlPlaneWebhook) ValidateUpdate(_ context.Context, _, newObj *ControlPlane) (admission.Warnings, error)
func (w *ControlPlaneWebhook) ValidateDelete(_ context.Context, _ *ControlPlane) (admission.Warnings, error)
```

- `ValidateCreate` and `ValidateUpdate` both delegate to the internal
  `validate()` method (see [Validating-webhook rules](#validating-webhook-rules)).
  `ValidateCreate` additionally enforces the one-ControlPlane-per-namespace
  contract: it lists existing ControlPlanes in the new object's namespace
  through the uncached API reader and rejects the CREATE with a `Forbidden`
  error naming the incumbent when one already exists. The check runs only on
  CREATE so an existing CR stays mutable; `ValidateUpdate` validates the new
  object only.
- `ValidateDelete` always returns `nil, nil`. It exists only to satisfy the
  `admission.Validator` interface and is **never invoked** — the validating
  webhook does not register the `delete` verb, so **deletion is unconditionally
  allowed** even while the operator is down.

---

## Status Conditions

The ControlPlane status is driven by seventeen sub-reconcilers, each owning one
condition type, plus an aggregate `Ready` condition. The condition-type
constants in `controlplane_controller.go` (`subConditionTypes`) are the single
source of truth; call sites reference the constants rather than inline literals.

The sub-reconcilers run in dependency order; a stage that has not converged
requeues and stops the chain, so later conditions are never computed against a
half-built earlier stage. Eight stages additionally gate **explicitly** on an
earlier condition being `True` (`reconcileKeystone` on `InfrastructureReady`,
`reconcileHorizon` on `KeystoneReady`, `reconcileGlance`, `reconcilePlacement`
and `reconcileBarbican` on `KeystoneReady` and on the `AccountReady` of the
`KeystoneService` registration each of them projects for itself,
`reconcileNeutron` on `KeystoneReady` and `OVNReady` plus its own registration,
`reconcileAdminCredential` on `KORCReady`, `reconcileCatalog` on
`AdminCredentialReady`):

```
NamespacesReady → InfrastructureReady → ESOTenantStoreReady → DBCredentialsReady
  → AdminPasswordReady → KeystoneReady → HorizonReady → KORCReady
  → AdminCredentialReady → CatalogReady → GlanceReady → PlacementReady
  → BarbicanReady → OVNReady → NeutronReady → ServiceAccountsReady
  → RegistrationTenantStoresReady
```

`ESOTenantStoreReady` runs ahead of every store-consuming stage because it
provisions the per-tenant `SecretStore` they route their ExternalSecrets and
PushSecrets through. It is **mode-independent**: an External-mode ControlPlane
provisions the same tenant store (it just never seeds a bootstrap path through
it), so both `ESOTenantStoreReady` and `HorizonReady` appear on an External-mode
CR's status — the latter as `HorizonNotManaged`, since the dashboard is forbidden
in External mode.

`ServiceAccountsReady` and `RegistrationTenantStoresReady` run **last** and carry
no gate of their own. The first only reads the `KeystoneService` registrations
the Glance, Placement, Barbican and Neutron legs wrote earlier in the same pass,
so there is no projection it could defer. The second writes into namespaces the
control plane does not own, which is why it sits at the end of the chain rather
than beside `ESOTenantStoreReady`: a namespace someone else administers must never
park this control plane's own credential material behind it.

`Ready` is `True` (reason `AllReady`) **only** when all sub-conditions are
`True` (via `conditions.AllTrue`); otherwise it is `False` (reason
`NotAllReady`). One exception bypasses the aggregation: when a namespace holds
more than one ControlPlane (possible only for CRs that predate the
one-per-namespace webhook guard or bypassed it), every CR except the oldest is
parked with `Ready=False` reason `DuplicateControlPlane` naming the incumbent,
and none of its sub-reconcilers run. For the full flow, see the
[ControlPlane Reconciler reference](./controlplane-reconciler.md).

### InfrastructureReady

Set by `reconcileInfrastructure`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `InfrastructureReady` | All managed backing services are ensured and report Ready (or the control plane uses only brownfield infra, so there is nothing to provision). |
| `False` | `WaitingForDatabase` | Managed MariaDB is ensured but not yet Ready. |
| `False` | `WaitingForCache` | Managed Memcached is ensured but not yet Ready. |
| `False` | `MariaDBError` | Error create-or-updating the MariaDB child. |
| `False` | `MemcachedError` | Error create-or-updating the Memcached child. |
| `False` | `WaitingForMessaging` | The managed `RabbitmqCluster` is ensured but does not report `AllReplicasReady` yet. Message: `RabbitmqCluster "<name>" in namespace "<ns>" (spec.infrastructure.messaging) is not ready`. |
| `False` | `RabbitMQError` | Error create-or-updating the `RabbitmqCluster` child. Message: `ensuring RabbitmqCluster "<name>" in namespace "<ns>" (spec.infrastructure.messaging): <error>`. A cluster that does not serve the `rabbitmq.com` CRD fails closed here, with a `no matches for kind` error from the `Get`. An unauthorised scale-down lands here too: the declared `replicas` is below the owned cluster's and `c5c3.io/allow-messaging-recreate` is not set, so the destructive recreate is refused and the error names the annotation. |
| `False` | `FinalizingMessaging` | On deletion, the managed `RabbitmqCluster` has been deleted by the teardown (foreground propagation) and the ControlPlane finalizer waits for the RabbitMQ Cluster Operator to release its own finalizer on it before releasing; see [Owner-ref / GC model](./controlplane-reconciler.md#owner-ref--gc-model). Message: `waiting for the managed RabbitmqCluster "<name>" to be deleted before releasing the ControlPlane`. |
| `True` | `ExternallyManaged` | `services.keystone.mode` is `External`: identity is managed against `services.keystone.external.authURL`, so no MariaDB/Memcached is provisioned. |
| `False` | `InfrastructureNotConfigured` | `spec.infrastructure` is unset on a **non**-External ControlPlane. The validating webhook requires the block outside External mode, so this only fires for a webhook-bypassed CR; it fails closed rather than dereferencing the nil block. |

### ESOTenantStoreReady

Set by `reconcileESOTenantStore`. It provisions the per-tenant `SecretStore` (plus
its ServiceAccount and mTLS certificate) that every store-consuming stage routes
its ExternalSecrets and PushSecrets through, which is why it runs ahead of them.
It is **mode-independent** — an External-mode ControlPlane provisions the same
store.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `ESOTenantStoreReady` | The operator-provisioned per-tenant `SecretStore` is Ready. |
| `True` | `StoreRefOverridden` | An explicit `spec.secretStoreRef` opts out of the operator-provisioned store: the ControlPlane owns the referenced store's lifecycle, so nothing is provisioned. The selected store's readiness is still gated by each store-consuming sub-reconciler. |
| `False` | `SecretStoreNotReady` | The per-tenant `SecretStore` is not Ready yet; waiting on cert issuance and the OpenBao backend. |
| `False` | `ProvisioningError` | Error ensuring the per-tenant secret-store objects (SecretStore, ServiceAccount, certificate). |

### DBCredentialsReady

Set by `reconcileDBCredentials`. In managed mode (`database.clusterRef` set) it
create-or-updates the per-ControlPlane DB-credential `ExternalSecret`
(`{name}-keystone-db-credentials`, reading OpenBao path
`openstack/keystone/{namespace}/{name}/db`, hourly refresh) and mirrors its
Ready status. The OpenBao-backed `ClusterSecretStore` is checked first so an
ESO/OpenBao outage surfaces promptly instead of hiding behind the
ExternalSecret's stale per-object Ready cache.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `DBCredentialsReady` | The DB-credential ExternalSecret is Ready; the materialised Secret exists. |
| `True` | `BrownfieldUserSuppliedCredential` | Brownfield database (`clusterRef` unset): the user supplies the DB-credential Secret out-of-band, so no ExternalSecret is projected and the chain proceeds immediately. |
| `True` | `ExternallyManaged` | `services.keystone.mode` is `External`: the ControlPlane manages no database at all, so nothing is projected and neither OpenBao nor the `ClusterSecretStore` is consulted. |
| `False` | `SecretStoreNotReady` | The store selected by `spec.secretStoreRef` (a ClusterSecretStore or a namespaced SecretStore) is not Ready; the secret backend is unreachable. |
| `False` | `GeneratorError` | Error ensuring the dynamic DB-credential objects (the MariaDB `database` engine-backed generator that issues short-lived credentials). |
| `False` | `ExternalSecretError` | Error ensuring or checking the DB-credential ExternalSecret. |
| `False` | `WaitingForDBCredentialSecret` | The ExternalSecret is ensured but ESO has not yet synced it to Ready. In `Static` mode (the explicit opt-out, and every dedicated managed database) the message names the OpenBao KV path the credential has to be **seeded at out-of-band** — nothing seeds it, so until it exists this is where the ControlPlane stops. |

### AdminPasswordReady

Set by `reconcileAdminPassword`. It runs **before** the Keystone stage because
the keystone-operator's `SecretsReady` gate needs the admin-password
ExternalSecret to exist before the projected Keystone child references it. In
managed mode it create-or-updates the per-ControlPlane admin-password
`ExternalSecret` (`{name}-keystone-admin-credentials`, reading OpenBao path
`bootstrap/{namespace}/{name}-keystone/admin` — keystone-**name**-scoped so it
matches the seeder and the keystone-operator rotation PushSecret) and mirrors
its Ready status.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `AdminPasswordReady` | The admin-password ExternalSecret is Ready; the materialised Secret exists. |
| `True` | `BrownfieldUserSuppliedCredential` | Brownfield database (`clusterRef` unset): the user supplies the admin-password Secret out-of-band, so no ExternalSecret is projected and the chain proceeds immediately. |
| `True` | `ExternallyManaged` | `services.keystone.mode` is `External`: the admin password is read from the user-supplied `korc.adminCredential.passwordSecretRef` Secret; no ExternalSecret is projected and no OpenBao bootstrap path is seeded. Updating that Secret is what drives a hash-driven re-mint of the admin application credential. |
| `False` | `SecretStoreNotReady` | The store selected by `spec.secretStoreRef` (a ClusterSecretStore or a namespaced SecretStore) is not Ready; the secret backend is unreachable. |
| `False` | `ExternalSecretError` | Error ensuring or checking the admin-password ExternalSecret. |
| `False` | `WaitingForAdminPasswordSecret` | The ExternalSecret is ensured but ESO has not yet synced it to Ready. |

### KeystoneReady

Set by `reconcileKeystone` (gated on `InfrastructureReady`).

| Status | Reason | When |
| --- | --- | --- |
| `True` | `KeystoneReady` | The projected Keystone CR reports Ready. |
| `True` | `KeystoneNotManaged` | `services.keystone` is unset: this ControlPlane manages no identity plane. |
| `True` | `ExternallyManaged` | `services.keystone.mode` is `External`: identity is managed against `services.keystone.external.authURL`; no Keystone child is projected and none is deleted. |
| `False` | `WaitingForInfrastructure` | `InfrastructureReady` is not `True`; Keystone projection deferred. |
| `False` | `WaitingForKeystone` | The Keystone CR is ensured but not yet Ready. |
| `False` | `InvalidRotationInterval` | `services.keystone.rotationInterval` could not be converted to a cron schedule. |
| `False` | `KeystoneProjectionRejected` | The Keystone API server rejected the projected spec (HTTP 422) — almost always a now-immutable db/bootstrap field that diverged from the frozen Keystone child. Reconcile the ControlPlane spec back to the child's values, or recreate the child, to recover. Distinct from `KeystoneError` so the wedge is diagnosable from the condition. |
| `False` | `KeystoneError` | Error create-or-updating the Keystone CR. |

### HorizonReady

Set by `reconcileHorizon` (gated on `KeystoneReady` — the dashboard authenticates
against the Keystone child). The dashboard is **forbidden in External mode**, so
an External-mode ControlPlane always reports `HorizonNotManaged`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `HorizonReady` | The projected Horizon CR reports Ready. |
| `True` | `HorizonNotManaged` | `services.horizon` is unset (or the ControlPlane is in External mode): no dashboard is managed, so the aggregate `Ready` is not blocked. Any previously-projected Horizon child is **preserved** unless the `c5c3.io/allow-horizon-deletion: "true"` annotation opts in to its deletion. |
| `False` | `WaitingForKeystone` | `KeystoneReady` is not `True`; Horizon projection deferred. |
| `False` | `WaitingForHorizon` | The Horizon CR is ensured but not yet Ready. |
| `False` | `IdentityBackendsUnavailable` | Listing the Keystone child's identity backends for the Horizon projection failed. The chain stops rather than projecting an empty `websso` block, which would silently remove a working SSO button from the login page. |
| `False` | `HorizonProjectionRejected` | The Horizon API server rejected the projected spec (HTTP 422) — the projection violates a CRD/webhook rule. Reconcile the ControlPlane spec to a valid projection to recover. |
| `False` | `HorizonError` | Error create-or-updating the Horizon CR. |

### GlanceReady

Set by `reconcileGlance` (gated on `KeystoneReady` — Glance validates every token
against the Keystone child — **and** on the projected `KeystoneService`
registration having provisioned the `glance` service account). Glance is
**forbidden in External mode**, so it is only ever managed against a Managed-mode
Keystone.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `GlanceReady` | The projected Glance CR reports Ready. |
| `True` | `GlanceNotManaged` | `spec.services.glance` is unset: no image service is managed, so the aggregate `Ready` is not blocked. Any previously-projected Glance child (and its `GlanceBackend` children and DB-credential ExternalSecret) is **preserved** unless the `c5c3.io/allow-glance-deletion: "true"` annotation opts in to its deletion. The dynamic DB-credential generator, its ServiceAccount, and its client Certificate are torn down **either way** — preserving a running service does not imply preserving the generator that keeps minting its credentials. |
| `False` | `WaitingForKeystone` | `KeystoneReady` is not `True`; Glance projection deferred. |
| `False` | `WaitingForServiceRegistration` | The projected `KeystoneService` registration has not provisioned the `glance` account yet; projection deferred until its Keystone user and password exist. The message relays the registration's own failing sub-condition, so a collision on the `glance` user or its catalog row reads here verbatim. |
| `False` | `ServiceRegistrationError` | Kubernetes-level error writing or reading the `KeystoneService` registration child — a refused adoption of a same-named foreign CR among them. |
| `False` | `GlanceDBCredentialError` | Error ensuring the Glance DB-credential ExternalSecret (managed database only). |
| `False` | `WaitingForGlanceDBCredential` | `credentialsMode: Dynamic` is in effect but no engine-issued credential has materialised yet — either the generator-backed DB-credential ExternalSecret has not synced, or (on a Static→Dynamic migration, where the ExternalSecret is updated in place and keeps reporting the previous Static sync's `Ready`) the Secret it targets still carries the retired static username. No Glance CR is projected, and an existing child keeps its current mode, until one does. The message names the `database/mariadb/creds/glance-<namespace>` path, which only exists once `setup-database-tenant.sh` has onboarded the tenant, or the stale username it found. |
| `False` | `GlanceBackendProjectionRejected` | The Glance API server rejected a projected `GlanceBackend` (HTTP 422). Reconcile the `services.glance.backends` entries to a valid projection to recover. |
| `False` | `GlanceBackendError` | Error projecting or pruning a `GlanceBackend` child. |
| `False` | `WaitingForGlance` | The Glance CR is ensured but not yet Ready. |
| `False` | `GlanceProjectionRejected` | The Glance API server rejected the projected Glance spec (HTTP 422) — the projection violates a CRD/webhook rule. Reconcile the ControlPlane spec to a valid projection to recover. |
| `False` | `GlanceError` | Error create-or-updating the Glance CR. |

### OVNReady

Set by `reconcileOVN`. It carries no gate and writes nothing. The `OVNCentral`
named by `spec.services.neutron.ovn.centralRef` is deployed outside the plane,
the way the infrastructure clusters in `spec.infrastructure` are, so this pass
only reads it and records whether the two databases the ML2/OVN mechanism driver
needs are usable. The read goes through the local client whatever cluster the
network service is placed on: the `OVNCentral` CR lives on the management
cluster, where the ovn-operator reconciles it. Every not-yet arm requeues after
15s instead of erroring, because nothing this ControlPlane does can converge a
central it does not own.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `OVNCentralReady` | The central reports `Ready` and publishes both database addresses and its client Secret. The message carries the Northbound and Southbound addresses that were selected. |
| `True` | `OVNNotManaged` | `spec.services.neutron` is unset: no OVN control plane is consumed, so the aggregate `Ready` is not blocked. |
| `False` | `OVNCentralNamespaceForbidden` | The resolved `centralRef` namespace is outside the plane's reach — the same rule the validating webhook enforces, re-run here because a CR can reach etcd without passing through admission. The central is **not** read, so none of its addresses or status reaches this plane. The message is the webhook's own, naming the field and the direction of the refusal. Requeue 15s. |
| `False` | `OVNCentralNotFound` | No `OVNCentral` of that name exists in the resolved namespace. The message carries the full `<namespace>/<name>` the ref resolved to. Requeue 15s. |
| `False` | `OVNCentralReadError` | The `OVNCentral` CRD is not served on this cluster, in which case the message says to install the ovn-operator and the pass requeues after 15s; any other read failure is returned as an error. |
| `False` | `OVNCentralNotExternallyReachable` | The network service runs on another cluster than the central, and the central does not set both `spec.northbound.externallyReachable` and `spec.southbound.externallyReachable`. The Neutron pods would reach the databases over the node network, which the in-cluster addresses do not serve. Requeue 15s. |
| `False` | `WaitingForOVNCentral` | The central has not reported `Ready` yet, or reports it not `True`. The central's own reason and message are relayed verbatim, since "not ready" alone names nothing to act on. Requeue 15s. |
| `False` | `OVNEndpointsPending` | The central reports `Ready` but has not published one of the two database addresses or `status.clientSecretName` yet; the ovn-operator fills them in as its children converge. The message names the missing fields. Requeue 15s. |

### NeutronReady

Set by `reconcileNeutron`. It is gated on `KeystoneReady` (Neutron validates
every token against the Keystone child), on `OVNReady`, and on the projected
`KeystoneService` registration having provisioned the `neutron` service account.
Once gated through, the pass delivers the shared message bus into the namespace
the network service runs in and ensures the DB credential before it projects the
child, so the Secrets the child references exist by the time the neutron operator
resolves them. Neutron is **forbidden in External mode**, so it is only ever
managed against a Managed-mode Keystone.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `NeutronReady` | The projected Neutron CR reports Ready and its registration reports Ready. |
| `True` | `NeutronNotManaged` | `spec.services.neutron` is unset: no network service is managed, so the aggregate `Ready` is not blocked. Any previously-projected Neutron child (with its DB-credential ExternalSecret, the two messaging Secrets, and the registration) is **preserved** unless the `c5c3.io/allow-neutron-deletion: "true"` annotation opts in to its deletion. The dynamic DB-credential generator, its ServiceAccount, and its client Certificate are torn down **either way**. The referenced `OVNCentral` is never touched. |
| `False` | `WaitingForKeystone` | `KeystoneReady` is not `True`; Neutron projection deferred. Requeue 5s. |
| `False` | `WaitingForOVN` | `OVNReady` is not `True`; Neutron projection deferred. A Neutron pointed at a central whose databases do not serve cannot program a single network. Requeue 5s. |
| `False` | `WaitingForMessagingCredentials` | The shared bus has not delivered its transport URL yet: the `RabbitmqCluster`, its default-user Secret, or the brownfield Secret is missing. Nothing is written, so the child never sees a partial URL. Requeue 15s. |
| `False` | `WaitingForMessagingCABundle` | `spec.infrastructure.messaging.tls` names a CA bundle Secret that does not exist, or one that carries no data under the referenced key. Requeue 15s. |
| `False` | `NeutronMessagingError` | Error resolving the shared transport URL, writing either messaging Secret into the Neutron namespace, or removing the stale CA mirror after the `tls` block was dropped. |
| `False` | `TargetClusterUnavailable` | The cluster the Neutron namespace lives on did not resolve, so the messaging Secrets cannot be written there. The resolver's own message is relayed. Requeue 15s. |
| `False` | `WaitingForServiceRegistration` | The projected `KeystoneService` registration has not provisioned the `neutron` account yet; projection deferred until its Keystone user and password exist. The message relays the registration's own failing sub-condition, so a collision on the `neutron` user or its catalog row reads here verbatim. |
| `False` | `ServiceRegistrationError` | Kubernetes-level error writing or reading the `KeystoneService` registration child; a refused adoption of a same-named foreign CR is among them. |
| `False` | `ServiceRegistrationFieldsReclaimed` | The pass reset a spec field another field manager had written on the registration child (an `adopt` consent, a `rotation` block, or an extra catalog endpoint). The condition names the same fields as the `Warning` event and stands until a pass reads an untampered child. |
| `False` | `NeutronDBCredentialError` | Error ensuring or reading the Neutron DB-credential objects (managed database only). |
| `False` | `WaitingForNeutronDBCredential` | `credentialsMode: Dynamic` is in effect but no engine-issued credential has materialised yet: either the generator-backed DB-credential ExternalSecret has not synced, or (on a Static→Dynamic migration, where the ExternalSecret is updated in place and keeps reporting the previous Static sync's `Ready`) the Secret it targets still carries the retired static username. No Neutron CR is projected, and an existing child keeps its current mode, until one does. The message names the `database/mariadb/creds/neutron-<namespace>` path, which only exists once `setup-database-tenant.sh` has onboarded the tenant, or the stale username it found. |
| `False` | `WaitingForNeutron` | The Neutron CR is ensured but not yet Ready. Requeue 15s. |
| `False` | `NeutronProjectionRejected` | The Neutron API server rejected the projected Neutron spec (HTTP 422): the projection violates a CRD/webhook rule. Reconcile the ControlPlane spec to a valid projection to recover. |
| `False` | `NeutronError` | Error create-or-updating the Neutron CR. |

A registration that is provisioned but not yet fully `Ready` relays its own
first failing sub-condition's reason onto `NeutronReady`, so a catalog-level
collision surfaces here under the registration's own vocabulary.

### KORCReady

Set by `reconcileKORC`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `ApplicationCredentialMinted` | The K-ORC admin `ApplicationCredential` is minted and reports `Available=True`. |
| `False` | `WaitingForAdminPassword` | The admin password Secret/key is not yet available; minting deferred. |
| `False` | `WaitingForCABundle` | The Secret referenced by the Keystone endpoint's CA bundle — `external.caBundleSecretRef` in External mode, `caBundleSecretRef` on a **placed** Keystone — does not exist yet, or exists with a missing/empty key; the mint is deferred rather than attempted against an endpoint whose certificate cannot be verified. The present-but-empty shape is the normal transient of a two-step "create the Secret, then populate it" flow. |
| `False` | `CABundleError` | Non-missing error reading that CA bundle. |
| `False` | `ApplicationCredentialFailed` | The `ApplicationCredential` reports a terminal K-ORC error (`GetTerminalError` — an unrecoverable/invalid-config reason such as K-ORC being unable to authenticate); the message folds in any stuck admin Domain/User import. |
| `False` | `WaitingForApplicationCredential` | The `ApplicationCredential` CR is ensured but not yet `Available`; the message folds in any stuck admin Domain/User import. |
| `False` | `ReMinting` | The admin application credential is being re-minted (delete + recreate) after an admin-password change or a `CredentialRotation` request; awaiting K-ORC's revoke of the previous credential. The old credential is **already revoked** at the Keystone level — see the [consumption contract](#admincredentialspec). |
| `False` | `ReMintStalled` | The `ApplicationCredential` has been `Terminating` longer than `remintStallTimeout`; K-ORC may be unable to reach Keystone to revoke the previous credential. Escalated from `ReMinting` so a stuck finalizer is alertable instead of looping silently. |
| `False` | `AdminPasswordError` | Non-missing error reading the admin password. |
| `False` | `PasswordCloudError` | Error ensuring the operator-owned password-based `clouds.yaml` Secret the mint authenticates with. |
| `False` | `AdminImportError` | Error create-or-updating the K-ORC admin `User`/`Domain` imports. |
| `False` | `SecretError` | Error ensuring the application-credential Secret, or regenerating its `value` for a re-mint. |
| `False` | `SeedCloudsYamlError` | Error seeding the bootstrap `clouds.yaml`. |
| `False` | `PushSecretError` | Error ensuring the admin app-credential `PushSecret`, or forcing its CA-bundle re-push. |
| `False` | `ExternalSecretError` | Error ensuring the K-ORC `clouds.yaml` `ExternalSecret`. |
| `False` | `ApplicationCredentialError` | Error create-or-updating (or deleting, for a re-mint) the `ApplicationCredential` CR. |
| `False` | `FinalizingServiceRegistrations` | The ControlPlane is being deleted and the operator is waiting for the `KeystoneService` registrations it projects for its built-in services to finish their own teardown. They go first: their K-ORC CRs belong to the registration, and its controller tears them down through the admin credential the next step revokes. Set by `reconcileDelete`; see the [reconciler reference](./controlplane-reconciler.md). |
| `False` | `FinalizingORC` | The ControlPlane is being deleted and the operator is waiting for the owned K-ORC CRs and PushSecrets to finish their teardown (K-ORC's finalizer revokes the credential against Keystone) before releasing the `c5c3.io/orc-teardown` finalizer. Set by `reconcileDelete`; see the [reconciler reference](./controlplane-reconciler.md). |
| `False` | `AuthenticationFailed` | **External mode only.** The external Keystone rejected the admin credential (HTTP 401) — typically the password was rotated out-of-band and `passwordSecretRef` is stale. K-ORC's message is relayed verbatim. |
| `False` | `EndpointUnreachable` | **External mode only.** `services.keystone.external.authURL` could not be dialled (DNS failure, connection refused, timeout). |
| `False` | `TLSVerificationFailed` | **External mode only.** The external endpoint's certificate did not verify; supply the private CA via `external.caBundleSecretRef`. |
| `False` | `CatalogEndpointMismatch` | **External mode only.** Authentication succeeded but the requested interface/region is absent from the external service catalog — a wrong `external.endpointType` or `spec.region`. |
| `False` | `CredentialDrift` | **External mode only.** An application-credential create against a stale resolve-once import id yielded a Keystone 403 (`identity:create_application_credential`). Drift is surfaced, never remediated. |
| `False` | `ImportStalled` | **External mode only.** An admin Domain/User import has been waiting to be "created externally" for longer than `externalImportStallGrace` (2m). In External mode the import target already exists, so this is a misconfiguration. |

### AdminCredentialReady

Set by `reconcileAdminCredential` (gated on `KORCReady`, the OpenBao-backed
`ClusterSecretStore` being Ready, the K-ORC `clouds.yaml` ExternalSecret being
Ready, the admin app-credential `PushSecret` having actually synced to OpenBao,
**and** the materialised `clouds.yaml` Secret semantically matching (parsed
application-credential id+secret) the freshly assembled credential).

| Status | Reason | When |
| --- | --- | --- |
| `True` | `AdminCredentialReady` | The admin application credential is committed to the owned Secret, mirrored to OpenBao, and the materialised `clouds.yaml` Secret matches the assembled credential. |
| `False` | `WaitingForKORC` | `KORCReady` is not `True`; credential push deferred. |
| `False` | `CredentialDrift` | **External mode only.** `KORCReady` reports drift in the external installation (`AuthenticationFailed` or `CredentialDrift`). The operator never remediates the external Keystone; update the `passwordSecretRef` Secret to drive a re-mint. |
| `False` | `SecretStoreNotReady` | The store selected by `spec.secretStoreRef` (a ClusterSecretStore or a namespaced SecretStore) is not Ready; the secret backend is unreachable. |
| `False` | `SecretStoreError` | Error checking whether the store selected by `spec.secretStoreRef` is Ready (as opposed to it reporting not-Ready). The condition is stamped before the error is returned so `reconcileCatalog` and `reconcileServiceAccounts` — which gate on `AdminCredentialReady` and run in the same reconcile pass — cannot act on a stale `True`. |
| `False` | `WaitingForCloudsYaml` | The operator-created per-ControlPlane `k-orc-clouds-yaml` ExternalSecret in the control-plane namespace (co-located with the K-ORC CRs per C1; created and owned by `reconcileKORC`) is not yet Ready. |
| `False` | `WaitingForCredentialID` | K-ORC has not yet reported the minted application credential's id; the assembly is deferred until it does. |
| `False` | `WaitingForAppCredentialSecret` | The operator-owned app-credential Secret does not exist yet, or carries no credential key — the mint is not complete. |
| `False` | `WaitingForPushSecret` | The admin app-credential `PushSecret` has not synced the assembled `clouds.yaml` to OpenBao yet. `AdminCredentialReady` is gated on the PushSecret's `Ready` condition — not merely on the CR existing — so a backend permission failure (e.g. the ESO role missing the push policy) cannot yield a false-positive Ready while OpenBao still serves the password-based bootstrap `clouds.yaml`. |
| `False` | `WaitingForCloudsYamlSync` | The materialised `k-orc-clouds-yaml` Secret is absent or still holds a stale credential (a re-mint revoked the old one but ESO has not re-synced yet). `reconcileAdminCredential` stamps the `external-secrets.io/force-sync` annotation to force an immediate re-sync and compares the materialised Secret semantically (parsed application-credential id+secret) against the assembled credential before reporting Ready, so the condition never reads `True` against a revoked credential. |
| `False` | `CloudsYamlSyncStuck` | The materialised `k-orc-clouds-yaml` Secret has failed to match the assembled credential for longer than `cloudsYamlSyncStuckTimeout` (measured from the credential's `LastRotation`); the ESO ExternalSecret or OpenBao backend may be unable to sync. Escalated from `WaitingForCloudsYamlSync` so a never-converging sync is alertable and distinguishable from a transient miss. |
| `False` | `CloudsYamlError` | Error checking the `clouds.yaml` ExternalSecret, forcing its re-sync, or reading the materialised Secret. |
| `False` | `SecretError` | Error ensuring the operator-owned application-credential Secret. |
| `False` | `PushSecretError` | Error ensuring the OpenBao PushSecret, stamping its content-hash re-push annotation, or reading it back for the sync gate. |

### CatalogReady

Set by `reconcileCatalog` (gated on `AdminCredentialReady`, **and** on every
catalog child reporting `Available` for its current generation —
`korcAvailableUpToDate`, which refuses a stale `Available` condition whose
`ObservedGeneration` lags the object, so an endpoint/region edit cannot flip
`CatalogReady` True before K-ORC re-reconciles).

What "every catalog child" means depends on the Keystone mode. In **Managed**
mode the control plane owns the catalog and registers the identity `Service` and
its public `Endpoint`. In **External** mode it is import-first: the identity
`Service` and the `Endpoint` of the interface `endpointType` selects are the
gating unmanaged imports, and nothing else — the other two interfaces are
imported for visibility only (see
[ExternalCatalogSpec](#externalcatalogspec)). Catalog rows a `KeystoneService`
registers are gated by that CR's own `CatalogReady`, not by this condition.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `CatalogRegistered` | **Managed mode only.** Every managed catalog entry is registered as K-ORC CRs **and** reports `Available`. The catalog is a per-service table whose only entry today is the identity (Keystone) `Service` and its public `Endpoint`; the message counts the registered entries, so a future second service is one more entry rather than a reworded condition. |
| `True` | `CatalogImported` | **External mode only.** The external identity `Service` and the endpoint interface `endpointType` selects resolved as unmanaged imports. The message reports how many of the three endpoint interfaces resolved. Deliberately distinct from `CatalogRegistered`: nothing was registered, and conflating the two would make "did this ControlPlane write to my catalog?" unanswerable from status. |
| `False` | `WaitingForAdminCredential` | `AdminCredentialReady` is not `True`; catalog reconciliation deferred. |
| `False` | `WaitingForCatalog` | A catalog child is reconciled but not yet `Available` for the current generation (a stale `Available` condition whose `ObservedGeneration` lags the object does not count). In External mode this names the gating import that has not resolved. |
| `False` | `CatalogFailed` | A catalog child reports a terminal K-ORC error (`GetTerminalError`). In External mode this is where the **>1-match** half of the ambiguity contract lands: K-ORC refuses to guess and stops retrying, and the message relays it verbatim plus a hint at `external.catalog.identityServiceName` (or, for an endpoint import, at the region limitation no spec field can fix). Terminal errors are surfaced for **every** import, gating or not — with one exception: a >1-match on a **non-gating** interface has no remediation and nothing depends on it, so it is tolerated exactly like a non-gating `ImportStalled` and reported as `resolved: false`. |
| `False` | `ImportStalled` | **External mode only.** A **gating** catalog import has been waiting to be "created externally" for longer than `externalImportStallGrace` (2m). This is the **0-match** half of the ambiguity contract: a gating import's target pre-exists by definition, so the wait never ends on its own. The message names `external.endpointType` and `spec.region` as the likely causes, and for an endpoint import the third possibility — the external catalog publishes no such interface. A non-gating interface import stalls on the same marker without failing the condition. |
| `False` | `AuthenticationFailed` \| `EndpointUnreachable` \| `TLSVerificationFailed` \| `CatalogEndpointMismatch` \| `CredentialDrift` | **External mode only.** An unresolved import carries a K-ORC message identifying one of these failure classes; it is relayed verbatim (see [`KORCReady`](#korcready) for each class). `CatalogEndpointMismatch` additionally names the effective `endpointType` and `spec.region`. |
| `False` | `ServiceError` | **Managed mode only.** Error create-or-updating the identity `Service` CR. |
| `False` | `EndpointError` | **Managed mode only.** Error create-or-updating the identity `Endpoint` CR. |
| `False` | `ImportError` | **External mode only.** Kubernetes-level error create-or-updating one of the unmanaged import CRs. |

### ServiceAccountsReady

Set by `reconcileServiceAccounts`. It **aggregates** the readiness of the
`KeystoneService` registrations the built-in service legs project — one per
declared `services.glance` / `.placement` / `.barbican` — into the one condition
operators alert on. It projects nothing itself and needs no gate of its own: it
only reads the children those legs wrote earlier in the same pass.

The relayed reason is the failing registration's **own** first failing
sub-condition, not an aggregate placeholder, so any reason a `KeystoneService`
reports can appear here — including catalog-side ones such as `CatalogFailed`,
because `CatalogReady` is folded in before `AccountReady`. The message always
names the registration child it came from.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `NoServiceRegistrationsProjected` | No built-in service block is declared, which is every External-mode ControlPlane. Still `True` so the condition schema does not depend on the spec. |
| `True` | `ServiceAccountsProvisioned` | Every projected registration reports `Ready`. The message counts them. |
| `False` | `WaitingForServiceRegistration` | A registration child has not been projected yet (its service leg is gated on something upstream of its own apply), or it has not reported `Ready` yet. |
| `False` | `ServiceRegistrationError` | Kubernetes-level error reading a registration child. Returned as an error too, so the reconcile group joins it. |
| `False` | *(the child's reason)* | A registration reports a `False` sub-condition — `ServiceAccountCollision`, `ProbingForCollision`, `WaitingForServiceAccounts`, `ServiceAccountsFailed`, `SecretStoreNotReady`, `CatalogFailed`, … — relayed verbatim with the child's own message. |

### RegistrationTenantStoresReady

Set by `reconcileRegistrationTenantStores`. It provisions the per-tenant
`openbao-tenant-store` trio (a `ServiceAccount`, an mTLS `Certificate`, and the
`SecretStore` itself) in every **allowlisted** namespace hosting at least one
`KeystoneService`, and collects the trio again once the last registration there
is gone. Credential delivery is namespace-local, so without it an allowlisted
foreign registration would mint its Keystone account and then wait forever on a
store nobody creates.

It is deliberately separate from [`ESOTenantStoreReady`](#esotenantstoreready),
which covers only the namespaces the control plane occupies. That one runs in the
chain's blocking prefix, where a non-zero result parks `DBCredentialsReady`,
`AdminPasswordReady` and `KeystoneReady` behind it. These namespaces are not the
operator's — a foreign object can occupy the store's name there, a certificate can
fail to issue — and none of that may reach the plane's core reconciliation. A
failure here surfaces in this condition, and through it in the aggregate `Ready`,
and stops there.

An allowlisted namespace that hosts no registration is not provisioned. A
namespace **removed** from the allowlist while it still holds registrations keeps
its store rather than losing it: de-listing is an admission gate, and revoking the
store under a running service would destroy credentials it depends on (see
[ServiceRegistrationsSpec](#serviceregistrationsspec)).

| Status | Reason | When |
| --- | --- | --- |
| `True` | `StoreRefOverridden` | An explicit `spec.secretStoreRef` opts the whole control plane out of operator-provisioned stores. Nothing is provisioned, and nothing this sub-reconciler created is standing to collect; a registration resolves that store in its own namespace itself. |
| `True` | `NoRegistrationNamespaces` | No allowlisted namespace hosts a service registration. Covers both arms that reach it: an empty or absent allowlist with nothing standing, and an allowlist whose namespaces hold no `KeystoneService`. |
| `True` | `RegistrationTenantStoresReady` | The `openbao-tenant-store` `SecretStore` is Ready in every allowlisted registration namespace. The message counts them. |
| `False` | `ProvisioningError` | Writing or collecting a trio failed. Every namespace is attempted before the failures are reported together, so one broken namespace cannot starve its peers, and the message names the failing namespaces and the joined error. It **requeues** (10s) instead of returning an error: the cause is usually a tenant-side one no backoff resolves, such as a foreign object holding the store's name, and returning an error would put the whole ControlPlane reconcile into exponential backoff for it. Listing the provisioned namespaces or counting the registrations is the one arm that does return the error as well. |
| `False` | `SecretStoreNotReady` | A store is written but not yet Ready, naming the namespace. A registration's delivery leg gates on exactly this, so reporting `True` while a store is still issuing its client certificate would claim a delivery path that does not carry yet. Requeue 10s. A failed readiness **read** returns the error with no condition written, leaving the previous value standing until the retry. |

### Ready (aggregate)

Set by `setReadyCondition`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `AllReady` | All seventeen sub-conditions are `True`. |
| `False` | `NotAllReady` | One or more sub-conditions are not `True`. |

---

## Service Namespaces

By default every service a ControlPlane projects lands in the **ControlPlane's
own namespace**: namespace and ControlPlane are the same boundary, so no
network-policy, RBAC, or quota line can be drawn between the services of one
control plane. A `namespace` assignment on `services.keystone`,
`services.horizon`, or `services.glance` makes the target namespace a
**per-service choice** — a
service can be placed in a namespace of its own, and the backing services, secret
store, and credential material that belong to it follow it there. A service
without an assignment stays in the ControlPlane's namespace exactly as before.

### ServiceNamespaceSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | The namespace the service is placed in. Must be an RFC-1123 label (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, ≤ 63 chars) and must **differ** from the ControlPlane's own namespace — omit the whole block to keep the service there. |
| `lifecycle` | `string` (`Managed` \| `External`) | No | `Managed` | Who owns the namespace's lifecycle. Defaulted to `Managed` by both the `+kubebuilder:default` marker and the defaulting webhook. |

### Lifecycles

The two lifecycles are deliberately asymmetric — they differ on who owns the
namespace, both when the ControlPlane is created and when it is deleted:

| Lifecycle | On reconcile | On ControlPlane deletion |
| --- | --- | --- |
| `Managed` | The operator **creates** the namespace and stamps it with the ownership labels plus `app.kubernetes.io/managed-by: c5c3-operator`. A namespace that already exists **without** those labels is never adopted — the operator fails loud with `NamespacesReady=False/NamespaceNotOwned` rather than taking over a namespace it did not create. | The operator **deletes** the namespace (only if it carries the ownership labels), which cascades everything left in it. |
| `External` | The operator only **verifies** the namespace exists; it never creates, labels, or mutates it. A missing one parks on `NamespacesReady=False/NamespaceNotFound` and requeues. | The namespace **survives**. The residue the ControlPlane placed in it — backing services, credential material, tenant store — is swept by name, but the namespace itself is left standing. |

Use `External` for namespaces whose quotas, RBAC, and policies are provisioned
out-of-band, and `Managed` for a namespace the operator should own end to end.

### Backing services follow the service

Each namespace that hosts at least one service of the ControlPlane gets its **own
set of backing-service instances** (database, cache) materialized from the shared
`spec.infrastructure` block. Services co-located in one namespace share that
namespace's instances (with the same per-service logical databases, users, and
cache isolation used within a single namespace); services placed apart each get
their own. Within a namespace a service can still opt into dedicated instances
via [`dedicatedBackingServices`](#dedicatedbackingservices) — that opt-in simply
follows its service into the assigned namespace.

### Ownership and garbage collection

Kubernetes garbage collection only cascades **within** a namespace, so a
controller owner reference cannot cross one — the API server rejects it. A child
the ControlPlane places in a service namespace therefore carries no owner
reference; it is stamped with two **ownership labels** instead —
`c5c3.io/controlplane-name` and `c5c3.io/controlplane-namespace`, which together
name the owning ControlPlane — and the [ORC-teardown
finalizer](./controlplane-reconciler.md#owner-ref--gc-model) deletes it
explicitly, because nothing else collects it. The finalizer deletes
the service children first and waits for them (their own operators run a
sequenced ESO cleanup through the tenant store in the same namespace), then takes
the namespace down per its lifecycle.

### Secret distribution

An ESO `SecretStore` and the Secrets it materializes are namespace-local, so a
store in the ControlPlane's namespace cannot deliver anything into a service
namespace. Every namespace the ControlPlane occupies therefore gets its own
per-tenant `openbao-tenant-store` (its own ServiceAccount, client certificate,
and store object), and `ESOTenantStoreReady` gates on **all** of them. This needs
no OpenBao-side change: the `eso-tenant` role binds the ServiceAccount name in any
namespace and its templated policy scopes every path to the caller's own
namespace.

The credential material follows the Keystone service, and its OpenBao paths are
re-keyed on the Keystone service namespace:

- the admin-password seed path is
  `bootstrap/<keystone-namespace>/<controlplane>-keystone/admin` — the same path
  the keystone-operator's rotation `PushSecret` writes to (both follow the
  Keystone child), and the path
  [`write-bootstrap-secrets.sh`](../infrastructure/infrastructure-manifests.md)
  seeds (its `KORC_CONTROLPLANES` entries accept an optional
  `<namespace>/<controlplane>/<keystone-namespace>` third segment);
- the database-engine role is `keystone-<keystone-namespace>`, which
  `setup-database-tenant.sh` provisions by resolving the service namespace from
  the live ControlPlane spec.

A **brownfield** or **External** admin-password Secret is the user's, supplied
against the ControlPlane they wrote, so it stays in the ControlPlane's own
namespace where they put it.

A namespace the control plane does **not** occupy gets a trio too when it is
allowlisted for service registrations and hosts at least one `KeystoneService`,
because that CR's consumer Secret is materialized through the store in its own
namespace. Those trios are provisioned by a sub-reconciler of their own and gated
by [`RegistrationTenantStoresReady`](#registrationtenantstoresready), so a
namespace somebody else administers cannot park the control plane's own
credential material. OpenBao again needs no change: the `eso-tenant` role binds
the ServiceAccount name in any namespace, and its templated policy confines each
token to its own paths.

### Cross-namespace service discovery

A service placed in one namespace still reaches the identity service in another
through the **namespace-qualified Service DNS**:
`http://<controlplane>-keystone.<keystone-namespace>.svc:5000/v3`. ClusterIP
Service DNS resolves across namespaces unchanged, so K-ORC's `clouds.yaml`
`auth_url` and the dashboard's `spec.keystoneEndpoint` reach a Keystone placed
apart with no extra wiring. What does **not** come for free is reachability —
namespaces are where NetworkPolicy is attached, so a default-deny namespace must
explicitly allow this flow (see below).

### Network policies

The operator creates **no NetworkPolicies** — it never has, in either the
single-namespace or the split-namespace case. Splitting a control plane across
namespaces is precisely what makes drawing them possible, so a platform operator
who wants a default-deny posture writes them per namespace. The cross-namespace
flows one ControlPlane needs are:

| From | To | Port | Purpose |
| --- | --- | --- | --- |
| the Horizon namespace | the Keystone namespace | `5000` | the dashboard authenticates every login against Keystone |
| the ControlPlane's namespace (K-ORC) | the Keystone namespace | `5000` | K-ORC reconciles the identity catalog and the admin credential |
| each service's namespace | its own database | `3306` | the service's DB connection |
| each service's namespace | its own cache | `11211` | the service's cache connection |
| a gateway namespace | the exposed service's namespace | the service port | external ingress via Gateway API |

### Uniqueness and immutability

A namespace is the **tenant key** the whole secret stack is scoped by (the
OpenBao KV paths, the `keystone-<namespace>` database-engine role, the templated
`eso-tenant` policy), so it belongs to **at most one** ControlPlane. Admission
rejects an assignment that names a namespace another ControlPlane already occupies
(its own or one of its service namespaces), and vice versa — the same rule
[`validateUniqueInNamespace`](#validation-rules) already enforces for a
ControlPlane's own namespace, one level out. An `External`-lifecycle namespace
that also hosts unrelated third-party workloads shares that namespace's OpenBao
path scope by design, so pick a dedicated namespace when that scope matters.

The assignment is **create-only**: the block's presence, its `name`, and its
`lifecycle` are all frozen after creation (webhook-only, no CEL transition rule,
so a later gated migration can relax it). Moving a live service across namespaces
would leave its backing services, its secret store, and every OpenBao path scoped
to the old namespace behind with no migration path — remove and recreate the
ControlPlane to change it. Two services co-located in one namespace must also
agree on its lifecycle: they share that namespace's backing services and tenant
store, so one must not have the teardown delete what the other declared
untouchable.

> **Chart mode:** the Helm chart's namespace-scoped RBAC mode
> (`rbac.namespaceScoped: true`) does **not** support dedicated service
> namespaces — the operator needs cluster-scoped `namespaces` (`create`,
> `delete`) and cross-namespace child access, which only the default ClusterRole
> mode grants.

---

## Child Namespace

> **DECISION:** the **ControlPlane-scoped** children the reconciler projects —
> the ones that belong to the ControlPlane as a whole rather than to one service:
> the K-ORC `ApplicationCredential` / `Service` / `Endpoint` / `User` / `Domain`
> CRs, the `clouds.yaml` Secret, the OpenBao `PushSecret`, and the
> service-account material — are created in the **ControlPlane's own namespace**
> (`childNamespace = cp.Namespace`), owned by it through a controller owner
> reference so the GC cascade reaps them.

A **service** and the things that follow it — its `MariaDB`, `Memcached`, and
`Keystone`/`Horizon`/`Glance` (and `GlanceBackend`) CRs, its tenant store, its
credential material — are placed in `cp.KeystoneNamespace()` /
`cp.HorizonNamespace()` / `cp.GlanceNamespace()`, the service's own namespace when
[`services.<svc>.namespace`](#service-namespaces) assigns one and the
ControlPlane's namespace otherwise. Only the latter can carry an owner
reference; a child in a different namespace carries the ownership labels instead
(see [Ownership and garbage collection](#ownership-and-garbage-collection)).

The rationale is garbage collection: `controllerutil.SetControllerReference`
rejects cross-namespace owner references because Kubernetes GC only cascades
within a single namespace. A child in `openstack` owned by a ControlPlane in
`default` would fail admission and, even if forced, would never be GC'd.
Co-locating a same-namespace child with its owner keeps the owner reference valid
and the GC cascade intact; a cross-namespace child is instead cleaned up by the
finalizer. In production a ControlPlane without any namespace assignments is
deployed into the `openstack` control-plane namespace, so its projected children
land in `openstack` exactly as expected — the namespace is **derived from the
owner (or the assignment)** rather than assumed. Projected child names are
deterministic and derived from the ControlPlane name (e.g. `{name}-keystone`,
`{name}-glance`, `{name}-glance-{backend}` (a projected `GlanceBackend`),
`{name}-admin-app-credential`, `{name}-identity-service`,
`{name}-identity-endpoint`, `{name}-service-account-{account}-role-{slug}` (an
unmanaged `Role` import) and `{name}-service-account-{account}-assign-{slug}` (a
managed `RoleAssignment`)) so a single namespace can host the children of
multiple ControlPlanes without clashing.

---

## Related References

- [ControlPlane Reconciler](./controlplane-reconciler.md) — the reconciliation
  flow, sub-reconciler ordering, and gating semantics.
- [Keystone CRD](../keystone/keystone-crd.md) — the shared `commonv1` types and
  the reference operator the c5c3 aggregate projects into.
- [Keystone Reconciler](../keystone/keystone-reconciler.md) — the per-service
  reconciler that owns the projected Keystone CR.
- [Target Clusters](../target-clusters.md) — registering a target cluster, what a
  placed service takes with it, and how its children are owned and torn down.
- [Kubernetes Packages](../backend/kubernetes-packages.md) — operator image and
  Helm-chart packaging.
- [Infrastructure Manifests](../infrastructure/infrastructure-manifests.md) —
  the GitOps stack that deploys the c5c3-operator, K-ORC, and the backing
  services.
