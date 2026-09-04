---
title: Advanced Configuration
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Advanced Configuration

Beyond the minimal control plane from the
[Quick Start (ControlPlane)](../quick-start-controlplane.md), the operators
support a number of configuration options for real cluster deployments. This
guide covers the ones the `ControlPlane` CR exposes and points to the reference
for the rest — and to the [Standalone Keystone](#standalone-keystone-without-a-controlplane)
section for the knobs that live only on a Keystone CR you own.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace and its projected
`controlplane-keystone` Keystone child is running. Every resource name in the
examples below is one that devstack produces.
:::

::: warning The Keystone child is operator-owned
On a ControlPlane deployment the `controlplane-keystone` Keystone CR is
**projected** by the c5c3-operator; the projected fields are re-asserted on every
reconcile, so editing them on the child is reverted. Configure the knobs the
`ControlPlane` CRD exposes on the `ControlPlane` CR. A knob the CRD does not
expose is **standalone-only** — apply it to a Keystone CR you own, in the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section. See
the [ControlPlane Reconciler](../reference/c5c3/controlplane-reconciler.md) for
the projection contract.
:::

Each pattern below is an independent recipe — apply only what you need.

---

## Brownfield database and cache

The Quick Start uses "managed mode", where the operator provisions the MariaDB
and Memcached the control plane connects to (`spec.infrastructure.database.clusterRef`
/ `cache.clusterRef`). If you already run MariaDB/Galera and Memcached outside the
operator's reach — managed by another team, hosted externally, or on a different
operator — use **brownfield mode** with explicit connection parameters on the
`ControlPlane` CR.

Brownfield is a **creation-time** decision. The validating webhook freezes
infrastructure presence and the database/cache mode (managed `clusterRef` vs
brownfield `host`/`servers`), the database name, replicas, and storageSize after
the ControlPlane is created, so you cannot flip a managed control plane to
brownfield in place — set `spec.infrastructure` when you first apply the CR:

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  # services.keystone and korc as in the Quick Start (ControlPlane)
  infrastructure:
    database:
      # brownfield: explicit host/port, no clusterRef
      host: mariadb.db.example.com
      port: 3306
      database: keystone
      secretRef:
        name: keystone-db
    cache:
      backend: dogpile.cache.pymemcache
      # brownfield cache: explicit server list, no clusterRef
      servers:
        - "memcached.cache.example.com:11211"
```

The reconciler deep-copies the whole `infrastructure.database` and
`infrastructure.cache` blocks onto the `controlplane-keystone` child, so the
child connects to the servers you declared here.

::: warning In brownfield mode you own schema setup
In brownfield mode (no `clusterRef`) the operator leaves the `secretRef` you
supplied in place — you own that Secret out-of-band — and does **not** create the
database, user, or grants. Provision them before the control plane reconciles:

```sql
CREATE DATABASE keystone DEFAULT CHARACTER SET utf8 COLLATE utf8_general_ci;
CREATE USER 'keystone'@'%' IDENTIFIED BY '<password-from-secretRef>';
GRANT ALL PRIVILEGES ON keystone.* TO 'keystone'@'%';
FLUSH PRIVILEGES;
```

The Secret referenced by `secretRef` must contain both a `username` and a
`password` key matching the SQL user — the keystone-operator gates `SecretsReady`
on the child on both, so a Secret with only `password` leaves
`controlplane-keystone` stuck at `SecretsReady=False`. Once those exist,
`db_sync` creates the Keystone schema on first reconcile. The OpenBao
database-tenant onboarding from the [Quick Start (ControlPlane)](../quick-start-controlplane.md)
(Step 4) applies to **managed** mode's engine-issued (Dynamic) credentials only —
a brownfield control plane draws no credentials from the OpenBao database engine.
:::

The webhook enforces that exactly one of `clusterRef` or `host` (`servers` for
cache) is set — never both — for both `database` and `cache`.

---

## Free-form service configuration

The `ControlPlane` exposes the same free-form configuration escape hatch the
service CRs carry, at two levels. `spec.globalExtraConfig` applies to every
INI-configured service the control plane declares (Keystone, Glance, Placement,
Barbican, and Neutron today). `spec.services.<svc>.extraConfig` sets one
service's own block. Both take the INI `map[section][key] = value` shape the
child renders into its service config (`keystone.conf`, `glance-api.conf`,
`placement.conf`, `barbican.conf`, and Neutron's `neutron.conf` plus
`ml2_conf.ini`, which share one block). The dashboard is the exception:
`spec.services.horizon.extraConfig` is a flat map of Django settings, covered in
[Horizon settings are flat, not INI](#horizon-settings-are-flat-not-ini).

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  # Applied to every INI service the control plane declares (Keystone, Glance,
  # Placement, Barbican, Neutron):
  globalExtraConfig:
    database:
      pool_timeout: "30"
  services:
    keystone:
      extraConfig:
        database:
          pool_timeout: "60"           # wins over the global value for Keystone
        token:
          expiration: "43200"          # 12h instead of the default 1h
    horizon:
      extraConfig:
        SESSION_TIMEOUT: 7200           # flat Django setting, not INI
```

The reconciler projects the merged INI result onto each INI service child's
`spec.extraConfig`, and the Horizon block verbatim onto the dashboard child.

### Merge semantics

For each INI service the global and per-service blocks merge **key by key**, the
per-service value winning. Whole sections are unioned: a per-service `[database]`
block that sets only `pool_timeout` still inherits every other `[database]` key
from the global block, and a global key with no per-service counterpart stays
effective. In the example above Keystone renders `[database] pool_timeout = 60`
(its own value), while a Glance service declared with no block of its own would
render `30`, inherited from the global block.

The option catalog that guards a service CR's own `spec.extraConfig` guards the
merged result here too, so a global key must be valid against **every** declared
INI service's catalog. Because `spec.globalExtraConfig` reaches Glance,
Placement, Barbican, and Neutron as well, a Keystone-only option placed there is
rejected while `services.glance`, `services.placement`, `services.barbican`, or
`services.neutron` is declared; the fix is to move that key to
`spec.services.keystone.extraConfig`.

### Horizon settings are flat, not INI

The dashboard renders `local_settings.py`, so `spec.services.horizon.extraConfig`
is a flat `map[setting] = value` of Django settings (`SESSION_TIMEOUT` above),
projected verbatim onto the Horizon child. `spec.globalExtraConfig` is INI and
never reaches the dashboard, and there is no merge for the Horizon block.

### External keystone mode

`spec.services.keystone.extraConfig` is forbidden when `services.keystone.mode`
is `External`: no Keystone workload is deployed, so there is nothing to render.
Both a CEL rule and the webhook reject it, with the message
`services.keystone.extraConfig is forbidden when services.keystone.mode is External`.
`spec.globalExtraConfig` stays legal but inert in External mode, the same posture
`spec.globalPolicyOverrides` holds, since no INI-configured workload consumes it.
Glance, Horizon, Placement, and Barbican are forbidden entirely in External mode,
so their blocks cannot appear at all.

### Admission checks

The validating webhook runs two families of check at `ControlPlane` admission,
using option catalogs and ownership registries embedded from the service API
packages.

**Shape and ownership** run on every create and every update. Empty section or
key names in any INI block, and empty or non-Python-identifier setting names in
the Horizon block, are rejected. Keys the ControlPlane projects itself are
rejected outright: Glance's `[keystone_authtoken] password` (always), the Horizon
`SECRET_KEY` and every WebSSO / multi-domain setting (always, since the
ControlPlane projects those dynamically from the attached identity backends), and
Keystone's `[federation] trusted_dashboard` **only when** the ControlPlane derives
a dashboard endpoint from `services.horizon`. With no Horizon block that Keystone
key is admitted with a warning, so an externally-run dashboard can still do WebSSO
against the managed Keystone. Any other operator-owned key is honored but draws an
admission warning naming the key, its owner, any impact, and the block that set it.

**Option-catalog** validation runs on every create, and on update only when a
catalog input changed: either INI block, `spec.openStackRelease`,
`services.keystone.image`, or a newly-declared service. A replicas bump alone does
not re-run it, so a stored CR whose `extraConfig` went stale-invalid against a
regenerated catalog is not rejected by an unrelated edit. The merged result per
INI service is checked against that service's per-release option catalog
(Keystone's resolved from `services.keystone.image.tag` when the image is
overridden, otherwise `spec.openStackRelease`; Glance's from
`spec.openStackRelease`). Unknown sections and options are rejected; a
deprecated-but-accepted option draws a warning naming its replacement. The check
**fails open** with one warning per service and no error when no catalog resolves:
a digest-pinned image, an unparseable tag, or a release the operator build ships
no catalog for. Plugin-registered INI sections are rejected as unknown, because
the ControlPlane has no plugins field and never sets `spec.plugins` on a child;
configure plugin sections on the service CR directly. Neither family has a CEL or
CRD-schema backstop; both live only in the webhook.

Rejections name the block that carries the offending key. A finding is computed
once on the merged config, then attributed to every contributing path:
`spec.globalExtraConfig[<section>][<key>]` and
`spec.services.<svc>.extraConfig[<section>][<key>]`. A key present in both blocks
yields one error per path.

::: warning The projected children are operator-owned
The merged INI result and the Horizon block are re-asserted on the service
children on every reconcile. While no `ControlPlane` block is set the projection
carries no `extraConfig`, so a value set directly on a child stays untouched. Once
you set any `ControlPlane` block, the projection owns the child's field: a direct
edit on the child is reverted on the next reconcile. Clearing every `ControlPlane`
block projects nothing and the child's field reverts to unset. Configure the
free-form config on the `ControlPlane`, not on the child.
:::

### Catalog skew across operator builds

The catalogs consulted at `ControlPlane` admission are the ones embedded in the
**c5c3-operator** build. A deployed service operator of a different build may
embed a different catalog. The service CR's own validating webhook stays the
defense-in-depth check for that skew: when it rejects the projected child, the
ControlPlane surfaces `KeystoneProjectionRejected` or `GlanceProjectionRejected`
on its conditions, making a skewed rejection visible.

---

## Feature pointer table

Everything else the control plane supports. One-line hints, the ControlPlane knob
that projects it (or "not exposed" where it is standalone-only), and a link to the
full Keystone CR reference.

| Feature | Keystone CR field | ControlPlane path | Reference |
|---------|-------------------|-------------------|-----------|
| Replica count | `spec.deployment.replicas` | `spec.services.keystone.replicas` | [Day 2 — Scale](./day-2-operations.md#scale-replicas) |
| Release / image | `spec.image` | `spec.openStackRelease` (tag) + `spec.services.keystone.image` (override) | [Day 2 — Upgrade](./day-2-operations.md#upgrade-the-openstack-release) |
| Policy overrides | `spec.policyOverrides` | `spec.services.keystone.policyOverrides` (+ `spec.globalPolicyOverrides`) | [PolicySpec](../reference/keystone/keystone-crd.md#policyspec) |
| Federation proxy image | `spec.federation.proxyImage` | `spec.services.keystone.federationProxyImage` | [Attach an OIDC Federation Backend](./keystone/oidc-federation.md) |
| Public endpoint / gateway | `spec.bootstrap.publicEndpoint`, `spec.gateway` | `spec.services.keystone.publicEndpoint`, `spec.services.keystone.gateway` | [BootstrapSpec](../reference/keystone/keystone-crd.md#bootstrapspec) |
| Fernet / credential-key schedule | `spec.fernet`, `spec.credentialKeys` | `spec.services.keystone.rotationInterval` (schedule only) | [Day 2 — Rotate Fernet keys](./day-2-operations.md#rotate-fernet-keys-manually) |
| Database TLS/mTLS | `spec.database.tls` | `spec.infrastructure.database.tls` | [Enable Keystone Database TLS/mTLS](./keystone/enable-keystone-database-tls.md) |
| Autoscaling (HPA) | `spec.autoscaling` | not exposed — standalone-only | [Autoscaling (HPA)](#autoscaling-hpa) |
| Network policy | `spec.networkPolicy` | not exposed — standalone-only | [Network policy](#network-policy) |
| Free-form config (`extraConfig`) | `spec.extraConfig` | `spec.services.<svc>.extraConfig` (+ `spec.globalExtraConfig`) | [Free-form service configuration](#free-form-service-configuration) |
| Scheduled admin-password rotation | `spec.passwordRotation` | not exposed — standalone-only | [Schedule Admin Password Rotation](./keystone/keystone-admin-password-scheduled-rotation.md) |
| uWSGI tuning | `spec.uwsgi` | not exposed — standalone-only | [UWSGISpec](../reference/keystone/keystone-crd.md#uwsgispec) |
| Logging | `spec.logging` | not exposed — standalone-only | [LoggingSpec](../reference/keystone/keystone-crd.md#loggingspec) |
| Trust flush | `spec.trustFlush` | not exposed — standalone-only | [TrustFlushSpec](../reference/keystone/keystone-crd.md#trustflushspec) |
| Middleware | `spec.middleware` | not exposed — standalone-only | [MiddlewareSpec](../reference/keystone/keystone-crd.md#middlewarespec) |
| Plugins | `spec.plugins` | not exposed — standalone-only | [PluginSpec](../reference/keystone/keystone-crd.md#pluginspec) |
| Rollout strategy | `spec.deployment.strategy` | not exposed — standalone-only | [Graceful-termination fields](../reference/keystone/keystone-crd.md#graceful-termination-fields) |
| Graceful termination | `spec.deployment.terminationGracePeriodSeconds`, `spec.deployment.preStopSleepSeconds` | not exposed — standalone-only | [Graceful-termination fields](../reference/keystone/keystone-crd.md#graceful-termination-fields) |
| Topology spread | `spec.deployment.topologySpreadConstraints` | not exposed — standalone-only | [TopologySpreadConstraints](../reference/keystone/keystone-crd.md#topologyspreadconstraints) |
| Priority class | `spec.deployment.priorityClassName` | not exposed — standalone-only | [PriorityClassName](../reference/keystone/keystone-crd.md#priorityclassname) |
| Resource requests/limits | `spec.deployment.resources` | not exposed — standalone-only | [KeystoneSpec](../reference/keystone/keystone-crd.md#keystonespec) |

The "not exposed — standalone-only" knobs are not projectable through the
`ControlPlane` CRD today; set them on a Keystone CR you own, as shown in the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section.

---

## Standalone Keystone, without a ControlPlane

On the [Quick Start](../quick-start.md) / [Quick Start (Extended)](../quick-start-extended.md)
devstacks a standalone Keystone CR named `keystone` runs with no ControlPlane
projecting it. The recipes below apply to that CR. Two of them —
`spec.autoscaling` and `spec.networkPolicy` — are **not exposed on the
`ControlPlane` CRD today**, so a standalone Keystone is the only place they can be
set. `spec.extraConfig` is exposed on the ControlPlane, through
[Free-form service configuration](#free-form-service-configuration); the recipe
below is the standalone equivalent.

### Brownfield database

The standalone equivalent of the ControlPlane brownfield recipe above — explicit
`host`/`port` and `servers` set directly on the Keystone CR:

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  deployment:
    replicas: 1
  image:
    repository: ghcr.io/c5c3/keystone
    tag: "2025.2"
  database:
    # brownfield: explicit host/port, no clusterRef
    host: mariadb.db.example.com
    port: 3306
    database: keystone
    secretRef:
      name: keystone-db
  cache:
    backend: dogpile.cache.pymemcache
    # brownfield cache: explicit server list, no clusterRef
    servers:
      - "memcached.cache.example.com:11211"
  fernet:
    rotationSchedule: "0 0 * * 0"
    maxActiveKeys: 3
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: keystone-admin
    region: RegionOne
```

The same SQL provisioning and `username`+`password` Secret contract from the
ControlPlane recipe apply. The webhook enforces that exactly one of `clusterRef`
or `host` is set — never both — for both `database` and `cache`.

### Autoscaling (HPA)

`spec.autoscaling` is not exposed on the `ControlPlane` CRD today, so autoscaling
is standalone-only. Replace hand-patching `spec.deployment.replicas` with a
`HorizontalPodAutoscaler` managed by the operator. When `spec.autoscaling` is
present, the HPA owns the Deployment's replica count.

```yaml
spec:
  deployment:
    replicas: 3       # seeds the Deployment; HPA owns the Deployment replica count once created
  autoscaling:
    minReplicas: 2
    maxReplicas: 10
    targetCPUUtilization: 80
    targetMemoryUtilization: 70
```

- At least one of `targetCPUUtilization` or `targetMemoryUtilization` is required.
- `minReplicas` defaults to `spec.deployment.replicas` if unset — omitting it will floor the HPA at your current hand-set replica count, not at 1.
- The generated HPA references `deploy/keystone` and uses the Kubernetes standard
  `metrics-server`. The Quick Start kind cluster does **not** ship one by default —
  the HPA will sit at `unknown/80%` until a resource-metrics API is available.

  On the kind devstack, opt in with the `WITH_METRICS_SERVER` flag. Bring the
  devstack up with it set (the recipe here also needs the ControlPlane, so the
  flags compose):

  ```bash
  KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_METRICS_SERVER=true make deploy-infra
  ```

  Or, if the devstack is already running, apply the kind overlay additively and
  wait for it to reconcile:

  ```bash
  kubectl apply -k deploy/kind/metrics-server
  kubectl wait helmrelease/metrics-server -n kube-system --for=condition=Ready --timeout=5m
  kubectl top pods -n openstack   # sanity check: real utilisation, not an error
  ```

  The overlay pins the chart to a single major range and bakes in
  `--kubelet-insecure-tls`, which kind requires because its kubelets serve the
  metrics endpoint with self-signed certificates — no runtime patch needed.

  On non-kind clusters, `metrics-server` is usually already present: most managed
  Kubernetes distributions ship it. If yours does not, install it per the
  [upstream project](https://github.com/kubernetes-sigs/metrics-server) rather
  than copy-pasting an unpinned manifest.

Inspect the HPA:

```bash
kubectl get hpa -n openstack -l app.kubernetes.io/instance=keystone
kubectl describe hpa keystone -n openstack
```

Removing `spec.autoscaling` deletes the HPA and returns replica control to
`spec.deployment.replicas`. See [HPA Resource Mapping in the CRD reference](../reference/keystone/keystone-crd.md#hpa-resource-mapping)
for the exact field-to-resource mapping.

### Network policy

`spec.networkPolicy` is not exposed on the `ControlPlane` CRD today, so it is
standalone-only. When set, it creates a Kubernetes `NetworkPolicy` that restricts
ingress to the Keystone API pods. Egress rules for database, cache, and DNS are
derived automatically from the rest of the CR — you only declare the ingress sources.

```yaml
spec:
  networkPolicy:
    ingress:
      # Allow the ingress gateway to reach the Keystone API
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: envoy-gateway-system
      # Allow the monitoring namespace to scrape metrics
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
```

Each list entry requires a `namespaceSelector` and may narrow it with an optional
`podSelector`. Both are full Kubernetes `metav1.LabelSelector`s, so you can use
`matchLabels` (as above) or set-based `matchExpressions`.
Within one entry the two selectors AND together; multiple entries OR. Ingress is
always restricted to TCP 5000 — there is no per-entry port configuration. When the
list is non-empty, all other ingress is blocked by default — **including kubelet
probes from other namespaces, which is normally not an issue because probes
originate from the node, but verify in your cluster topology.**

For brownfield or external targets that the auto-derivation cannot see (an off-cluster
MariaDB host, an external IdP), append explicit rules with `spec.networkPolicy.additionalEgress`
— they are added after the auto-derived ones rather than replacing them.

Removing `spec.networkPolicy` deletes the NetworkPolicy and restores unrestricted
traffic. See the [NetworkPolicy reference](../reference/keystone/keystone-crd.md#networkpolicyspec)
for the auto-derived egress rules (Keystone API → MariaDB, Memcached, DNS).

### ExtraConfig — free-form INI sections

On a standalone Keystone CR you set `spec.extraConfig` directly. On a ControlPlane
the equivalent surface is `spec.globalExtraConfig` plus
`spec.services.<svc>.extraConfig` (see
[Free-form service configuration](#free-form-service-configuration)). The typed
fields on the CR cover the supported configuration surface. For everything else —
logging levels, oslo.messaging tuning, experimental Keystone flags —
`spec.extraConfig` takes a `map[section][key] = value` that is rendered into the
generated `keystone.conf`.

For the INI-file services (Keystone, Glance, Placement, Barbican) the operator
renders configuration through a single precedence chain: `plugins < operator
defaults < spec.extraConfig`. Each stage is merged key-wise, so a plugin section
cannot shadow an operator-computed value, the operator defaults win over any
colliding plugin section, and `spec.extraConfig` is the only door past the
operator's own defaults.

Each operator ships a registry of the configuration keys it computes. Overriding
one of those keys through `spec.extraConfig` is honored — the value is rendered —
but reported: the operator sets the informational `ExtraConfigHealthy=False`
condition naming every overridden key and emits a one-shot
`ExtraConfigOwnedKeyOverride` Warning event on the transition into that state.
Most registered keys are report-only; a few are rejected outright at admission by
the service webhook because a typed spec field owns them — for example Keystone's
`[federation] trusted_dashboard`, Glance's `[keystone_authtoken] password`, or
Horizon's `SECRET_KEY`.

```yaml
spec:
  extraConfig:
    DEFAULT:
      debug: "true"
      log_dir: "/var/log/keystone"
    token:
      expiration: "43200"        # 12h instead of default 1h
      allow_expired_window: "172800"
    oslo_messaging_rabbit:
      heartbeat_timeout_threshold: "60"
```

The `[DEFAULT] debug` override above is an operator-owned key — Keystone computes
it from the typed `spec.logging.debug` field — so this example now draws an
`ExtraConfigOwnedKeyOverride` Warning event and flips `ExtraConfigHealthy` to
`False` while still taking effect.

Beyond the ownership registry, the validating webhook checks every option name in
`spec.extraConfig` against a catalog of the options the service actually accepts.
Each operator embeds one catalog per OpenStack release, generated from the
`oslo-config-generator` run of the exact service image CobaltCore ships for that
release. The catalog answers a single question: does this option name exist?
Values are never inspected. Keystone derives the release from `spec.image.tag`;
Glance derives it from `spec.openStackRelease`. An option that sits in a known
section but is absent from the catalog is rejected at apply time with `no such
option in the <service> <release> option catalog`, naming the section and key. An
unknown section is rejected with `no such section in the <service> <release>
option catalog (sections registered by a loaded plugin must be declared via
spec.plugins)`.

Three classes of section or key are exempt from the catalog check. A section
declared by a `spec.plugins` entry's `configSection` is trusted, because the
plugin owns it and its options are not in the base catalog. Every key in the
operator-ownership registry above is exempt too, since the operator already
governs those. For Glance, the reserved store sections `os_glance_staging_store`
and `os_glance_tasks_store` are additionally allowed.

The check fails open when it cannot reason about a release. A digest-pinned
Keystone image (or any tag that does not name a release) carries no release to
look up, so the option check is skipped and the admission response returns a
warning. A release newer than the operator build, one with no embedded catalog,
is skipped the same way. A deprecated option that the service still accepts is
admitted with a warning naming its replacement, for example `[DEFAULT] logfile`
superseded by `[DEFAULT] log_file`.

This check lives only in the webhook. There is no CEL or CRD-schema backstop, so
a cluster with the validating webhook disabled accepts a misspelled option name
and surfaces it only at render time. Updates re-run the check only when
`spec.extraConfig`, the plugin section list, or the release field changes.

The operator still does not validate the values in these sections, but a
misspelled option name is now rejected at apply time. A wrong value on a real
option becomes a silent no-op at best and a crash loop at worst, so test changes
in a lab before rolling out. A change to `extraConfig` triggers a ConfigMap
rehash and a rolling Deployment update.

---

## Further reading

- [Keystone CRD API Reference](../reference/keystone/keystone-crd.md) — complete field-by-field reference with validation rules and examples
- [ControlPlane CRD API Reference](../reference/c5c3/controlplane-crd.md) — the `spec.*` fields the ControlPlane exposes, including `spec.infrastructure`
- [Observability & Diagnostics](./observability.md) — how to verify a new configuration took effect
- [Day 2 Operations](./day-2-operations.md) — scale, upgrade, rotate using the configured CR

## Tested by

The recipes above are exercised on the CI e2e kind cluster — the operator
installed with a dev image — by these chainsaw suites:

```bash
chainsaw test --test-dir tests/e2e/keystone/brownfield-database
chainsaw test --test-dir tests/e2e/keystone/autoscaling
chainsaw test --test-dir tests/e2e/keystone/network-policy
```
