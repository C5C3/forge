---
name: prepare-new-service
description: >-
  Analyze and prepare the onboarding of a new OpenStack service into CobaltCore —
  inventory the five layers (container image, service operator, CI/e2e,
  ControlPlane integration, documentation) against the Keystone reference
  implementation, check what keystone scaffolding must be generalized into
  internal/common first, and draft the phased meta issue ready to be split
  into sub-issues. Use when asked to onboard or add a new OpenStack service
  (e.g. Glance, Nova, Neutron, Placement), to prepare a service meta issue,
  or to assess readiness for the next service operator. Also covers
  OpenStack-adjacent services from outside the upstream (e.g. the Aurora
  dashboard) — see § Non-OpenStack-upstream services.
---

# Prepare a new service onboarding

This skill turns "we want service X next" into a **phased meta issue** whose
checkboxes are sized to become sub-issues, plus (when needed) a separate
**generalization pre-work issue**. It analyzes; it does not implement.

Worked examples: Horizon — meta issue #552, generalization pre-work #551;
Glance — meta issue #656 with pre-work #653 (Garage S3 test infra), #654
(c5c3 groundwork: role assignments, generalized catalog, cross-namespace
credential delivery), #655 (common extractions). Read them before drafting
a new one; they are the calibration for scope, tone, and checkbox
granularity. #656 also demonstrates the alternative implementation mode —
Phases 2–4 as one continuous stacked-PR arc instead of fanned-out
sub-issues — and a Phase-0 decision record (D1–D10) worth imitating.
Aurora dashboard — meta issue #758 with pre-work #757 (shared workload
builder) — is the first **non-OpenStack-upstream** service; its Phase-0
block records the release-decoupling decisions that
§ Non-OpenStack-upstream services generalizes. Meta issue #846 is the
worked example for the registration mechanism Phase 4 consumes: it
replaced the ControlPlane's inline catalog table and service-account
entries with the projected `KeystoneService` child described below.

## The five layers

Every service in CobaltCore threads through five layers. Keystone
(`operators/keystone`) is the reference implementation for all of them.

| Layer | Canonical locations | Auto-extends? |
|---|---|---|
| 1. Container image | `images/<svc>/Dockerfile`, `releases/*/source-refs.yaml`, `releases/*/extra-packages.yaml`, `tests/container-images/verify_<svc>.sh` | build/test matrix: **yes** (from source-refs keys); hadolint matrix in `build-images.yaml`: **no** |
| 2. Service operator | `operators/<svc>/` (api, controller — including the recurring-maintenance CronJobs of § Recurring maintenance jobs and the target-cluster placement artefacts of § Placement on target clusters —, webhook, helm chart on `operators/shared/helm/operator-library`), `go.work`, `Makefile` `OPERATORS` | **no** — module + enumerations by hand |
| 3. CI / e2e / deploy | `ci.yaml` paths-filter + `ALL_OPERATORS` + matrices, `tests/ci/verify_<svc>_ci_pipeline.sh`, `tests/e2e/<svc>/`, `tests/e2e/<svc>-operator/`, `tests/e2e-chaos/<svc>-*/`, `tests/tempest/<svc>-*/`, `deploy/flux-system/releases/<svc>-operator.yaml`, kind devstack wiring (`deploy/kind/base/openstack-gateway.yaml` listener + `deploy/kind/infrastructure/<svc>-nip-io-tls-certificate.yaml`, the `deploy/kind/base/kustomization.yaml` HelmRelease suspend patch **plus its counterparts in `hack/deploy-infra.sh`**: the flux-path un-suspend patch, the `enable_operator_servicemonitor` call, and the `hack/refresh-operator-image-digests.sh` target tuple — a service missing from the un-suspend list stays suspended on the quick-start path, its CRDs never install, and the c5c3-operator's controlplane cache never syncs), OpenBao bootstrap legs (`deploy/openbao/bootstrap/`, `deploy/openbao/policies/`) | chainsaw suites: **yes** (auto-discovered); ci.yaml wiring: **no** (3-step procedure in `hack/ci-resolve-changes.sh` header); devstack/OpenBao wiring: **no** |
| 4. ControlPlane (c5c3) | `ServicesSpec` in `operators/c5c3/api/v1alpha1/controlplane_types.go` (incl. `publicEndpoint`, `databaseCredentialsMode`, and `targetClusterRef` per-service fields), `reconcile_<svc>.go` (+ `reconcile_<svc>_dbcredentials.go` for DB services, + `reconcile_<svc>_messaging.go` for a bus consumer), the projected `KeystoneService` registration (a `desired<Svc>Registration` builder in `builtin_registrations.go`, the `reconcileBuiltinRegistration` and `foldBuiltinRegistrationReady` calls in `reconcile_<svc>.go`, the `projectedBuiltinRegistrations` entry in `reconcile_serviceaccounts.go`, the `<svc>CatalogURL` helper in `reconcile_catalog.go`, the `catalog: true` row in `declaredServiceTargetClusters` in `controlplane_webhook.go`, and the registration delete in `deleteOrphaned<Svc>`), teardown in `reconcile_delete.go` + the placed-namespace sweep, condition/instrumentation maps, RBAC markers (the chart's `_rbac-rules.tpl` is generated from them by `make sync-helm-rbac`), scheme, webhook (incl. the per-service placement rules — `tests/e2e/c5c3/invalid-cr/` pins them), envtest full chain (`integration_test.go`) + `tests/e2e/c5c3/full-controlplane-keystone/` | **no** — ~10 enumeration points |
| 5. Documentation | `docs/reference/<svc>/` (hand-written, `quadrant: operator` frontmatter), VitePress sidebar, per-service guides under `docs/guides/<svc>/`, quick-start extension (`docs/quick-start-controlplane.md`), `tests/unit/docs/` conventions | **no** — no doc generator exists |

## Procedure

### 1. Profile the service

Answer these before anything else — they decide which keystone machinery
applies and which decisions need a Phase-0 spike:

- **Database?** Which migration tool (alembic `db_sync` vs Django
  `migrate` vs none)? No DB ⇒ drop the database/db-sync/upgrade
  sub-reconcilers entirely. Which zero-downtime upgrade mechanism does
  the manage tool support (expand-migrate-contract, single-pass
  `db sync`, online data migrations), and which phases must straddle the
  workload rollout? A database-backed service adopts the shared
  `database.ReconcileUpgrade` flow (`internal/common/database/upgrade.go`),
  keying it off the image tag (keystone) or `spec.openStackRelease`
  (glance), or documents in its reference set why single-pass is
  acceptable. A DB service also inherits the **dynamic
  credential chain**: the shared `credentialsMode` on `DatabaseSpec`, a
  per-service `databaseCredentialsMode` override on its c5c3 service spec,
  a `provision_service_tenant` leg in
  `deploy/openbao/bootstrap/setup-database-tenant.sh`, a `<svc>-db` auth
  role + `<svc>-db-dynamic` policy, the generator-backed ExternalSecret
  glue (`reconcile_<svc>_dbcredentials.go` on the shared builders in
  `reconcile_dbcredentials.go`), and a
  `migrate-<svc>-db-to-dynamic-credentials` guide (keystone and glance are
  the two worked examples).
- **db-sync side-loads?** Some services need seed data beyond the schema
  migration (glance: `db load_metadefs`) — chain the extra `*-manage` step
  into the db-sync Job with an explicit `--path`, because the config-free
  image does not ship the oslo default locations.
- **Recurring maintenance?** Which housekeeping does the service expect
  somebody to run on a schedule — above all the database cleanup every
  soft-deleting service needs? Enumerate it here, in the profile, and
  carry it into Phase 2; it is not follow-up work. A package deployment
  has a cron entry on the controller node doing this, CobaltCore has nothing:
  unless the operator projects a CronJob, no run ever happens, and the
  growth is silent — no probe, condition, or metric observes a purge that
  was never scheduled. § Recurring maintenance jobs below is the build
  sheet and the retro-fit bill.
- **Message bus?** The shared bus exists: `commonv1.MessagingSpec` at
  `spec.infrastructure.messaging` (opt-in, managed `clusterRef` or brownfield
  `secretRef`), the `RabbitmqCluster` the ControlPlane provisions in its own
  namespace, and the rabbitmq-cluster-operator delivered through Flux. So does
  the consumer side, in `internal/common/messaging`:
  `ReconcileTransportURLSecret` turns either mode into the derived
  `<instance>-transport-url` Secret, `TransportURLEnvVar` is the
  `OS_DEFAULT__TRANSPORT_URL` override that sources it, `RabbitSection`
  renders the `[oslo_messaging_rabbit]` posture, and `EgressPort` gives the
  NetworkPolicy port. Neutron is its first consumer. The helper reads and
  writes in the consumer's own namespace through the consumer's own client, so
  a consumer on another cluster or in another namespace than the bus is handed
  a brownfield `secretRef` by whoever projects it. On the ControlPlane side that
  projector is `reconcileNeutronMessaging` (`reconcile_neutron_messaging.go`):
  it resolves `spec.infrastructure.messaging`
  read-only through `messaging.ResolveTransportURL`, writes
  `{cp}-neutron-messaging` (and `{cp}-neutron-messaging-ca` when the bus declares
  `tls`) into the Neutron's own namespace on the Neutron's own cluster, and hands
  the child a brownfield `secretRef` naming it. The validating webhook requires
  `spec.infrastructure.messaging` beside `services.neutron`, because the Neutron
  CRD requires `spec.messaging`. A new service still has to answer whether it
  shares that bus or wants a **dedicated** one: no dedicated-bus slot exists on
  any service block. The recipe for building one is
  `### Adding a backing-service class` in
  `docs/reference/c5c3/controlplane-crd.md`.
- **Extra backing store?** (object store, message queue, cache beyond
  memcached) — a new backing service is its own pre-work issue (#653:
  Garage S3 for glance is the template — operator + declarative
  buckets/keys in `deploy/flux-system/infrastructure/`, OpenBao-seeded
  credentials materialized via ESO, kind ExternalSecrets), and it earns a
  **backing-store-outage chaos suite** (`glance-garage-outage`: writes
  fail closed, `/healthcheck` and `Ready` stay up).
- **Consumes another service's API?** A service user
  (`[keystone_authtoken]`-style) is the `account` block of the
  `KeystoneService` child the ControlPlane projects for the service:
  user `<svc>`, its own project `service-<svc>` with `create: true`, and
  the single role `service`, all assembled by `builtinRegistration` in
  `builtin_registrations.go`. Add the two name constants beside
  `GlanceServiceAccountName` / `GlanceServiceProjectName` in
  `controlplane_webhook.go`. Each service creates its own project;
  sharing one would make two registrations adopt each other's Keystone
  row. The registration delivers a consumer Secret `{child}-credentials`
  carrying `clouds.yaml` and `password` keys, and the service leg reads
  only the `password` key into the child's service-user secret ref
  (`reconcile_glance.go` is the template). A service placed on a target
  cluster gets those credentials mirrored there by
  `ensureBuiltinRegistrationMirror`; a dedicated service namespace gets
  its tenant store from `reconcileRegistrationTenantStores`. Glance was
  the first consumer and #654/#655 are the pre-work templates; #846 is
  the mechanism.
- **Pluggable backends?** If users attach a variable number of backends
  (image stores, identity domains), model them as a **satellite CRD**
  mirroring `KeystoneIdentityBackend`/`GlanceBackend`: inverted
  attachment via `<svc>Ref`, dedicated per-backend controller + an
  aggregating sub-reconciler on the parent, curated `backends[]`
  projection from c5c3 with prefix-guarded pruning, plus the three suites
  the pattern demands (multi-instance, default/aggregation-switch with
  last-good retention, `invalid-<child>-cr`).
- **Service-catalog endpoints?** If yes, they are the `catalog` block of
  the same projected `KeystoneService` child. `builtinRegistration`
  registers **public and internal from birth** (the glance D6 posture is
  now the builder's shape), so adding an interface later is still a
  catalog migration. The `internal` URL is the service's
  `<svc>EndpointURL` in `reconcile_<svc>.go` wrapped by
  `internalCatalogURL`; the `public` URL is `<svc>CatalogURL` in
  `reconcile_catalog.go`, which prefers an explicit `publicEndpoint`,
  then the gateway hostname, then the in-cluster URL. Do **not** add a
  row to `managedCatalogRows`: that table holds the identity row alone
  and never gains another. Teardown needs no `reconcile_delete.go` edit
  either, since the registration's own finalizer removes its rows and
  `deleteRegistrationsBeforeTeardown` sweeps every projected child; what
  the service leg owns is deleting its registration in
  `deleteOrphaned<Svc>`. Add the service's row to
  `declaredServiceTargetClusters` (`controlplane_webhook.go`) with
  `catalog: true`, which is what requires a placed service to publish a
  reachable URL. A service the ControlPlane will not manage skips all of
  this and registers itself: see
  `docs/guides/register-a-foreign-service.md`.
- **Config format?** oslo INI is covered by `internal/common/config`;
  anything else (Django settings, JSON) needs a renderer decision first.
- **Behavior breaks between the supported releases?** Check upstream
  release notes for launch-mode/WSGI/paste divergence between the pinned
  releases (glance 2025.2 eventlet vs 2026.1 uWSGI forced a
  release-switched command in the deployment builder). If yes, the
  per-release `basic-deployment-<slug>` e2e variant and the tempest legs
  must genuinely differ, and unit tests must pin the rendered config for
  **both** releases.
- **Stateful key material?** (fernet-like) — keystone's rotation machinery
  is deliberately NOT extracted; a second consumer changes that calculus.
- **Operator-side dial-outs when placed?** Every service CR is born
  placeable on a target cluster (§ Placement on target clusters — the
  five artefacts are scaffold, not follow-up). The profile question is
  what *breaks* when the children live elsewhere: an operator that dials
  a cluster-local endpoint itself (OpenBao provisioning, DB admin
  connections) needs the port-forward tunnel seam
  (`commonmulticluster.NewPortForwardDialer` — barbican's OpenBao dials
  are the worked example), HTTP health probes go through
  `ResolveHTTPDoer` (the API-server service proxy when placed), and any
  optional child kind (HTTPRoute) needs the per-cluster capability probe
  (`ChildrenServeKind`).
- **Depends on other services?** Determines the gating condition in the
  c5c3 sub-reconciler chain (e.g. Horizon gates on `KeystoneReady`).
- **Tempest plugin maintained upstream?** If not (e.g. horizon), plan
  HTTP-level chainsaw assertions instead and say so explicitly.
- **Ingress?** `commonv1.GatewaySpec` / HTTPRoute via
  `internal/common/gateway` — plus the full public surface that hangs off
  it: an `https-<svc>` listener + hostname on the shared kind Gateway
  (`deploy/kind/base/openstack-gateway.yaml`) with a
  `<svc>-nip-io-tls-certificate.yaml` Certificate, a `publicEndpoint`
  catalog override on the c5c3 service spec (webhook-validated; it may be
  projected into no child, making the webhook the only gate), a
  `gateway-quick-start-smoke` e2e suite driving real host→listener→pod
  traffic, and the quick-start walkthrough doing its user-facing calls
  through the gateway.

### 2. Run the deterministic inventory

```bash
bash .claude/skills/prepare-new-service/scripts/inventory-touchpoints.sh <service>
```

It prints `[DONE]`/`[TODO]` per touch point across the five layers plus
gotcha warnings (e.g. the service already pinned in `upper-constraints.txt`).
It is an inventory, not a gate — for a fresh service everything is `[TODO]`;
its real value is catching **partial** onboarding and stale enumerations
when re-run mid-effort.

### 3. Verify the reference paths still hold

The repo evolves — do not trust this skill's tables blindly. Spot-check
that the enumeration points named above still exist at HEAD (grep for
`ALL_OPERATORS`, `subConditionTypes`, `OPERATORS ?=`, `ServicesSpec`,
`desiredGlanceRegistration`), and
skim the per-layer "Adding a New Service" docs, which are authoritative
for layer 1, 2, and 3 details:

- `docs/contributing/adding-a-new-operator.md` (layer 2 — the documented
  onboarding path over `internal/common`)
- `docs/reference/ci-cd/build-images-workflow.md` § Adding a New Service
- `docs/reference/ci-cd/container-images.md` (release config files)
- `docs/reference/testing/tempest-test-infrastructure.md` § Adding a New Service
- `docs/reference/infrastructure/infrastructure-manifests.md` § Extensibility

### 4. Generalization pre-check (before drafting the meta)

Ask: **what would the new operator copy-paste from `operators/keystone`
a second (or third) time?** Classify keystone internals into:

1. thin wrappers over `internal/common` — copy as pattern, fine;
2. generic logic living in keystone (pipeline/status machinery, watch
   mappers, webhook validators) — **extraction candidates**;
3. genuinely keystone-specific (fernet, bootstrap, trust-flush) — leave
   alone, rule of three.

Read § The shared operator scaffold first: pod-template and Service
assembly, scheme wiring, the `ValidateDelete` shim, and the
instrumentation glue have already been extracted, so they are no longer
candidates — they are consumption, and a copy of the pre-extraction
keystone shapes reintroduces boilerplate the repo has deleted.

If category 2 is non-empty, file (or update) a **separate refactor issue**
listing the candidates with file:line references, S/M/L effort, and a
must-before / opportunistic split — then mark the meta **blocked on it**.
#551 is the template (#655 is the second round: DB orchestration +
`keystone_authtoken` renderer for glance; #757 the third: the shared
workload builder plus the boilerplate collapse); check first whether it
(or a successor) is still open and simply needs extending. Also check open
API-shape issues (e.g. #471) — a new CRD must be born with the target
shape, not the legacy one.

Extract **with** a consumer, never ahead of one. #757 deliberately left
out both API bits its own issue sketched — `ContainerParams.EnvFrom` and
a `webhook.Setup[T]` wrapper — because no existing operator sets either;
the fourth operator adds `EnvFrom` when its env contract needs it. A
field no caller sets is untested surface, and the same calculus applies
to whatever the new service seems to ask for.

### 5. Draft the meta issue

Follow the house format (#552, #550, #481): `Meta:` title prefix,
Background, phases with checkbox scope, explicit blocking relations,
Out of scope, italic footer with date + `main` SHA + relations.
Standard phase skeleton (drop/merge phases the profile rules out):

- **Phase 0 — decisions (spike):** session/config/secret-sourcing choices,
  upper-constraints handling, WSGI/launch mode per release, endpoint and
  catalog-interface wiring, backend-CRD shape if the profile calls for one,
  the recurring-maintenance inventory with its cadence and retention
  defaults (record the decisions in the meta as #656 records D1–D10).
- **Phase 1 — container image** (usually independent of pre-work).
- **Phase 2 — service operator scaffold** (blocked on generalization) —
  built on the shared forms of § The shared operator scaffold, with the
  rendered Deployment and Service pinned in the same checkbox that adds
  them; one checkbox per recurring-maintenance task from the profile,
  which is part of the scaffold, not a follow-up (§ Recurring maintenance
  jobs); the five placement artefacts of § Placement on target clusters
  are scaffold too — [[check-service-parity]] P12 flags a service that
  lands without them.
- **Phase 3 — CI, e2e, deploy stack** (alongside Phase 2) — including the
  kind Gateway listener/cert, OpenBao bootstrap legs, and chaos suites.
- **Phase 4 — ControlPlane integration** (blocked on Phase 2) — including
  the projected `KeystoneService` registration: a
  `desired<Svc>Registration` builder in `builtin_registrations.go`, the
  `reconcileBuiltinRegistration` and `foldBuiltinRegistrationReady` calls
  in the service leg, the `projectedBuiltinRegistrations` entry, the
  consumer-Secret glue, and the orphan delete; plus envtest
  (`integration_test.go`) and the registrations step of
  `tests/e2e/c5c3/full-controlplane-keystone/`.
- **Phase 5 — documentation** (continuous, gates each phase) — reference
  set, per-service guides, and the quick-start extension.

Rules that keep it splittable:

- one checkbox = one sub-issue = one PR (Phase 0 may be a single spike);
- every checkbox names concrete files/paths, not intentions;
- include an ordering diagram when phases overlap;
- recommendations are stated as recommendations ("recommended: no DB
  sessions"), so the sub-issue can overturn them cheaply.

Create the issue with `gh issue create --label enhancement`, then
cross-link the pre-work issue's footer (`blocks #<meta>`).

### 6. Cross-check

Before publishing, sanity-check the claims that rot fastest:

- file:line references — re-grep each one at HEAD;
- "already pinned in upper-constraints" — `grep '^<svc>===' releases/*/upper-constraints.txt`;
- open-issue relations — `gh issue list --state open` for overlaps, so the
  meta references instead of duplicates.

Related skills for the implementation phase (mention them in the meta so
sub-issues use them as gates): [[check-crd-drift]], [[check-fixture-drift]],
[[check-condition-coverage]], [[check-validation-parity]],
[[check-doc-drift]], [[check-renovate-coverage]],
[[check-go-workspace-deps]], [[check-spdx-reuse]], and
[[check-service-parity]] as the closing cross-layer gate once the
onboarding lands (#656 used it exactly that way).

## The shared operator scaffold (verified 2026-07-28 post-#757, re-verify at HEAD)

The three existing service operators no longer hand-write their pod
template, their scheme, or their webhook and instrumentation shims. A new
operator consumes the shared forms below; writing the pre-extraction
keystone shapes from memory (or from an older PR) reintroduces
boilerplate that has been deleted repo-wide, and review will send it
back.

| Consume | Instead of |
|---|---|
| `deployment.BuildWorkload(WorkloadParams{...})` — replicas/selector/strategy, pod-level knobs, container resources, restricted security context, preStop hook; everything else renders verbatim, **nilness included** | a hand-assembled `appsv1.Deployment` literal |
| `deployment.BuildService(ns, name, labels, selector, port, targetPort)` — `port` and `targetPort` stay separate so traffic can route to a sidecar | a hand-assembled `corev1.Service` literal |
| the component-label contract from `internal/common/naming`: API pod template labelled `ComponentLabels(app, instance, ComponentAPI)`, API Service selecting on `APISelectorLabels`, API PDB on `SelectorLabels` + `ExcludeJobPods()`, while `Deployment.spec.selector` and the NetworkPolicy `podSelector` stay on `SelectorLabels` (see § Recurring maintenance jobs for why) | a name+instance Service selector that admits every pod of the instance — including maintenance Job pods — as an API endpoint |
| `bootstrap.NewScheme(<extra AddToScheme funcs>...)` in `main.go` | `var scheme = runtime.NewScheme()` plus an `init()` block |
| `mcbuilder.ControllerManagedBy(mgr)` (sigs.k8s.io/multicluster-runtime) with `commonmulticluster.EngageLocalCluster` / `EngageNoProviderClusters`, remote child watches via `AddRemoteChildWatches` (+ `ClusterServesKind` for optional kinds), and every child access routed through `ResolveChildrenClient` — see § Placement on target clusters | a single-cluster `ctrl.NewControllerManagedBy` with `Owns()` watches and direct `r.Client` child writes |
| embedding `webhook.NoopDeleteValidator[T]` | a per-webhook `ValidateDelete` method |
| passing the bound method `instrumenter.Instrument` into the pipeline | a package-local `instrumentSubReconciler` wrapper |
| referencing `commonreconcile.*` / `healthcheck.*` requeue constants directly | package-local aliases in a `requeue_intervals.go` (keep that file only for genuinely operator-specific waits; horizon has none and therefore no file) |

What stays service-specific is the residue the builder cannot know:
conditional volume/mount/container appends (db-TLS keypair, backend
config, sidecars), the launch command, probes, and any pod annotation
that must roll the Deployment when a secret rotates.

Copy the Service-selector latch with the label contract, not just the
labels: every `reconcileDeployment` gates the narrow `APISelectorLabels`
selector on `deployment.TemplateConverged` and latches it one-way via
`deployment.APISelectorNarrowed`, read through the uncached
`mgr.GetAPIReader()` (the informer cache can predate the narrowing
write). A new operator's pods carry the component label from birth, so
the latch closes on the first converged rollout — but the gate is what
keeps the Service from ever selecting zero serving pods during a
rollout, and horizon adopted the full shape for exactly that reason
(#785) despite projecting no maintenance pods at all.

**Pin the rendered objects.** All three operators carry a
`reconcile_deployment_pin_test.go`: the full Deployment and Service
marshalled with `sigs.k8s.io/yaml` and compared as a plain string, one
golden per input that perturbs the pod template (launch mode, TLS,
sidecar, autoscaling, hash annotations). Generate the new operator's pins
in the same commit that adds its builders — they are what makes the next
change to the shared `BuildWorkload` provably byte-neutral for this
service, and they cost nothing to write while the builder is fresh.

## Placement on target clusters (verified 2026-08-17 post-multicluster, re-verify at HEAD)

Since the multicluster conversion, every service CR is born placeable:
`spec.targetClusterRef` keeps the CR, its status, and its webhook on the
management cluster while every projected child lands on the named target.
The five artefacts below are one contract and land with the scaffold —
[[check-service-parity]] P12 audits them, and a follower service picking
up only the spec field is the drift it exists to catch:

- **API** — `TargetClusterRef *commonv1.TargetClusterRefSpec` on the
  Spec (shared type in `internal/common/types`; carries its own CEL
  immutability rule).
- **Webhook** — mirror the immutability via
  `validation.TargetClusterRefImmutable` plus the create-time checks
  via `validation.TargetClusterRef`.
- **invalid-cr fixture** — the `targetclusterref-empty-name` rejection
  (every operator's corpus carries one; copy the keystone shape).
- **Controller** — the multicluster builder
  (`mcbuilder.ControllerManagedBy` with `EngageLocalCluster` /
  `EngageNoProviderClusters`), remote requests mapped via
  `commonmulticluster.TargetClusterOf`, remote child watches via
  `AddRemoteChildWatches`, and **every** child write through the
  resolved children client (`ResolveChildrenClient`; a resolver miss
  fails the operator's first gate condition with reason
  `TargetClusterUnavailable`). Remote children are written label-owned
  via the ownership claim (`Claim` stamps labels instead of a
  cross-cluster ownerReference, which cannot exist).
- **Deletion** — a conditional `RemoteChildrenFinalizer` plus
  `SweepRemoteChildren`: nothing cascades for label-owned remote
  children, so an operator that skips the sweep leaks every placed
  child.

What stays service-specific: cluster-local dial-outs from the operator
need the port-forward tunnel (`NewPortForwardDialer` — barbican's
OpenBao provisioning is the worked example), HTTP health probes route
through `ResolveHTTPDoer` (API-server service proxy when placed), and
optional child kinds gate on the per-cluster capability probe
(`ChildrenServeKind` — the HTTPRoute precedent). Prove the split on the
dual envtest (`internal/common/testutil/multicluster`; keystone's
two-cluster and remote-teardown tests are the templates). The c5c3 side
adds `targetClusterRef` to the service's `Service<Svc>Spec`, the
placement rules to the ControlPlane webhook (+ invalid-cr fixtures),
and the placed ensemble to the deletion sweep. Whether the service
joins the two-cluster placed-services suite
(`tests/e2e-multicluster/`, keystone + barbican today) is a Phase-3
decision — membership also means extending the hard-coded image list
in the ci.yaml `e2e-multicluster` job. `docs/reference/target-clusters.md`
is the authoritative contract (ownership labels, teardown order,
per-service placement notes) and gains the new service's row in Phase 5.

## Recurring maintenance jobs

Every OpenStack service that soft-deletes rows, caches artefacts, or ages
out records assumes an operator sweeps up on a schedule. Upstream ships
the commands and documents the cadence; scheduling them is the deployer's
job. Package deployments answer that with a cron entry on the controller
node — CobaltCore has no such place, so a maintenance task that the service
operator does not project simply never runs. That failure is invisible by
construction: the deployment stays Ready, every probe passes, and the
only symptom is a table, a cache, or a disk that keeps growing until it
becomes an incident.

Treat this as part of the initial implementation. Glance is the
cautionary tale: it onboarded without one, and adding `db purge` later
(#729) cost an API block, a sub-reconciler, a condition, a metric pair,
an admission bound on `metadata.name` in **two** operators plus a
hash-collapse fallback for the CRs admitted before that bound existed,
and three invalid-CR fixtures — all of which would have been a handful of
extra lines inside the original scaffolding PRs.

### Find the tasks

For each candidate ask: does upstream document a periodic invocation, and
what happens over a year without it? Sources, in order of authority: the
service's admin/operations guide for the pinned release, `<svc>-manage
--help` run against the built image (authoritative for the subcommand
names at that exact ref), and the upstream deployment projects
(openstack-k8s-operators, Yaook, kolla-ansible cron roles) for the
cadences they picked.

The recurring shapes, with the commands to look for — a starting list to
verify, not a lookup table:

| Shape | Examples |
|---|---|
| Hard-delete soft-deleted rows | `glance-manage db purge` / `db purge_images_table`, `nova-manage db archive_deleted_rows` + `db purge`, `cinder-manage db purge`, `heat-manage purge_deleted`, `barbican-manage db clean` |
| Expire ephemeral records | `keystone-manage trust_flush` (implemented), token/session expiry sweeps — for Django-session services only when sessions are DB-backed (horizon's signed-cookie default needs none) |
| Prune caches and staging areas | glance image cache pruner/cleaner, orphaned upload staging directories |
| Rotate key material | keystone fernet + credential rotation (`internal/common/rotation` — already generalized) |
| Upstream daemons, not CronJobs | some services ship a long-running housekeeper (octavia) — model it as a Deployment, and say so in the profile rather than leaving the row blank |

A service with no maintenance task is a legitimate answer; record it in
the meta issue as a decision with its reasoning, so the next audit reads
it as considered rather than forgotten.

### Build sheet

`reconcile_dbpurge.go` (glance) and `reconcile_trustflush.go` (keystone)
are the two worked examples; both build on `internal/common/job`. One
maintenance task touches:

- **API** — an optional `spec.<task>` block (`DBPurgeSpec` in
  `operators/glance/api/v1alpha1/glance_types.go`) with `schedule`,
  retention/scope knobs, and `suspend`. Resolve the defaults at reconcile
  time in an `effective<Task>` helper, not in the defaulting webhook, so
  an unset field tracks the operator default across upgrades.
- **Sub-reconciler** — a parallel-group step calling `job.EnsureCronJob`,
  plus `Owns(&batchv1.CronJob{})` on the builder.
- **Condition** — `<Task>Ready` in the operator's condition list and in
  the `subReconcilerConditionTypes` map (else the metric label reads
  `UNKNOWN`; [[check-condition-coverage]] gates this). Suspension keeps
  it `True` under its own reason — a pause is a posture, not a failure —
  but the message must name the backlog that stops draining.
- **Run visibility** — the CronJob controller prunes its Jobs by history
  limit, so list the Jobs by the CR's common labels, keep the ones the
  CronJob controls, and report on the newest that reached a terminal
  state (`job.TerminalCondition`). Feed
  `job.RecordJobTerminalState`, which dedupes on the Job UID, into a
  metric pair (`<svc>_operator_<task>_total` +
  `_duration_seconds`) and emit a Warning event on failure.
- **Webhook** — cron grammar via `validation.CronSchedule`
  (`internal/common/validation`; it accepts `@daily`-style descriptors
  that a CRD pattern would not), bounds on the retention knobs mirroring
  the CEL rules, and an update warning when a retention window shrinks,
  because the shortened window applies retroactively at the next firing.
- **Safety knobs on the CronJob** — `ConcurrencyPolicy: Forbid` (two
  purges deleting from the same tables contend: Galera certification
  failures, InnoDB lock timeouts), a per-invocation row cap
  (`--max_rows`) keeping each write-set Galera-friendly, and
  `ActiveDeadlineSeconds` so a wedged run turns into a terminal failure
  instead of an active Job that holds the condition `True` forever.
  Document what the cap means for throughput: it is the schedule, not
  the job, that sets the drain rate.
- **The same runtime contract as the workload** — config volume, the
  `database.ConnectionEnvVar` override so the job reads dynamic
  credentials from the derived Secret rather than the ConfigMap, the
  db-TLS keypair mount under the same gate the Deployment uses, and pod
  labels from `naming.ComponentLabels` with the task's own component
  value (`trust-flush`, `db-purge`, `<kind>-rotation`): a superset of
  the selector labels so the NetworkPolicy covers the job pods, but
  **never** bare `commonLabels` — a Job pod whose labels satisfy the
  API Service selector becomes a Service endpoint with nothing
  listening (numeric `targetPort`, no readiness probe) and answers API
  traffic with `ECONNREFUSED` for its whole runtime, the #778 outage
  the component contract exists to prevent. The API PDB keeps the Job
  pods out via `naming.ExcludeJobPods`, not via the component label. A
  job that also talks to the API server needs its own egress rule
  (keystone's rotation CronJobs do).
- **Naming** — Kubernetes caps a CronJob name at 52 characters. Bound
  `metadata.name` in the webhook accordingly (see the gotcha below) and
  mirror that bound in the c5c3 webhook for the projected child name.
- **Tests** — unit tests pinning the rendered CronJob and every condition
  arm, invalid-CR fixtures for each new validation rule
  (`tests/e2e/glance/invalid-cr/17`–`19` are the template), and a
  `basic-deployment` assertion that the CronJob exists. For the endpoint
  isolation itself, `tests/e2e/keystone/maintenance-endpoint-isolation`
  is the template: a fast schedule, the API EndpointSlices sampled
  against the live Deployment's replica count, and a live maintenance
  pod required during the window so the suite proves isolation rather
  than an idle cluster.
- **Docs** — the spec block and the projected CronJob shape in
  `docs/reference/<svc>/<svc>-crd.md`, the step and its condition in
  `<svc>-reconciler.md`, the events plus an alerting rule in
  `<svc>-events.md`.

### Sub-issue checkbox

Phase 2 carries one checkbox per maintenance task, named after the
command it runs, e.g.:

> - [ ] `spec.dbPurge` + `reconcileDBPurge`: project the
>   `{name}-db-purge` CronJob (`<svc>-manage db purge`), report the
>   newest terminal run via `DBPurgeReady`, metric pair, invalid-CR
>   fixtures, CRD/reconciler/events docs

## Non-OpenStack-upstream services (verified 2026-07-27 pre-aurora, re-verify at HEAD)

A service from outside the OpenStack upstream (own org and repo, own
release cadence, usually not Python — worked example: Aurora dashboard,
meta #758) keeps **layers 2–5 unchanged**: the operator is Go regardless
of the workload, and the horizon subtraction (no
database/job/release/rotation/tls/keystoneauth) usually applies. Layer 1
breaks structurally. Profile these instead of the Python questions:

- **Upstream artifacts first:** check what upstream actually publishes
  (Aurora: npm packages only, via Changesets; both upstream Dockerfiles
  are dev-grade). Default posture: CobaltCore builds the production image from
  source at a pinned upstream git ref.
- **Release decoupling — do NOT add a `releases/*/source-refs.yaml`
  key.** The keys *are* the build/test/verify matrix
  (`hack/ci-generate-build-matrix.sh:44-52`), imply per-OpenStack-release
  versioning, and obligate `verify_release_config.sh`. Use the
  `keystone-federation-proxy` precedent instead: dedicated
  build/merge/verify jobs outside the matrix (`build-images.yaml:391-456`),
  image tagged with the service's own version.
- **Source org:** two clone sites hardcode `openstack/<service>` —
  `.github/actions/checkout-service-source/action.yaml` and
  `hack/ci-build-service-image.sh:62-63`. Parameterize them or bypass both
  via the dedicated job.
- **Renovate:** the source-refs customManager templates
  `opendev.org/openstack/{{depName}}` (`renovate.json:24`). A decoupled pin
  needs its own customManager (github-tags/releases datasource) plus a
  packageRules entry — gate with [[check-renovate-coverage]].
- **The Python gotchas don't transfer:** upper-constraints,
  `extra-packages.yaml`, uv/PBR/WSGI, the `python-base`/`venv-builder`
  lineage, and the `openstack`-user assertion in
  `verify_deviation_comments.sh` are all Python-specific. A non-Python
  image writes its own `verify_<svc>.sh` contract (runtime present, built
  assets present, non-root, no build toolchain in the final image) and its
  own deviation-comment function. No shared base image for a new language
  before the rule of three.
- **e2e inversion:** instead of one service version per OpenStack release,
  **one** service version runs against **N** OpenStack releases — the
  `basic-deployment` fork pair carries the same service tag against both
  stacks, and the kind image-load legs (`ci.yaml:894-903`, `:940-947`)
  need an entry outside `hack/ci-service-image-releases.sh`'s release
  loop.
- **Docs that go stale the moment this lands:**
  `docs/reference/ci-cd/build-images-workflow.md` § Adding a New Service
  and `docs/reference/ci-cd/container-images.md` both claim source-refs is
  the sole registration and assume pip/venv — extending them is part of
  the first decoupled service's Phase 5, not follow-up work.

## Known gotchas (verified 2026-07 post-glance, re-verify at HEAD)

- **upper-constraints pin conflict:** some services (horizon, most
  clients' dashboards/libraries) are already pinned in
  `releases/*/upper-constraints.txt`. Installing from source with
  `--constraint` then requires the source ref to match the pin exactly,
  or a `-<svc>` line in `overrides/<release>/constraints.txt`
  (`scripts/apply-constraint-overrides.sh`). Check the service's
  **libraries** too, not just the service: glance itself is unpinned, but
  `glance_store`/`boto3` are — the driver extra must resolve against the
  existing pins.
- **hadolint matrix is static** in `build-images.yaml` — new Dockerfiles
  must be added by hand even though the build matrix auto-discovers.
- **Hand-maintained per-service lists in the verify/matrix scripts:**
  `tests/container-images/verify_release_config.sh` (`SERVICES=` list),
  `verify_deviation_comments.sh` (per-service functions),
  `hack/ci-generate-tempest-matrix.sh` (`for service in …` loop), and the
  chaos CI job's image-load lists in `ci.yaml` are all generalized now but
  still enumerate services by hand — extend each one, or the new
  service's coverage silently never runs.
- **The parameterized `operators/Dockerfile` (`ARG OPERATOR`) is still
  coupled to `go.work`:** it COPYs every module's go.mod and source, so a
  new module still edits that one Dockerfile's COPY lines (no more
  per-operator Dockerfiles, though).
- **WSGI entry points:** `uv pip install --prefix` skips PBR
  `wsgi_scripts` generation — service Dockerfiles hand-write their WSGI
  launcher (see `images/keystone/Dockerfile`). Also verify the stock WSGI
  module actually honors `--config-dir`/`--pyargv`: glance's
  `glance.wsgi.api:application` reads only its default config path and
  needed a hand-shipped shim (`images/glance/glance-wsgi-api`) to load
  the operator's mounted config dirs.
- **A CronJob shrinks the CR's name budget:** Kubernetes caps a CronJob
  name at 52 characters (`MaxCronJobNameLength` in
  `operators/glance/api/v1alpha1/glance_webhook.go`), so
  `{name}-<suffix>` bounds `metadata.name` itself. Enforce that bound
  **on create only** — `metadata.name` is immutable, so on update the
  rule can only fire against objects a pre-bound operator already
  admitted, including the finalizer-removal update that completes a
  deletion, wedging them permanently. The controller therefore stays
  total for over-long names by collapsing them onto a content-stable
  hash (`dbPurgeCronJobName`). Adding the first CronJob to an existing
  operator means paying all of this; adding it during onboarding means
  the bound exists before any CR does.
- **In-cluster API probes must retry the connection, not the response:**
  every `basic-deployment` suite fires a one-off pod at the service API
  right after the CR flips Ready, and kube-proxy's endpoint programming
  can trail that flip by a second or two — a single-shot request loses
  that race with every pod serving. Copy the hardened idiom (`d11fef10`):
  retry connection-level failures inside the probe pod (15 attempts, 2s
  apart) with a step timeout covering the whole budget, while `HTTPError`
  and the status/body assertions still fail on the first response, so the
  suite tolerates the endpoint lag and nothing else.
- **Paste-deploy divergence between releases:** factory references
  (`API.factory` vs `API_factory`) and oslo.middleware healthcheck
  semantics (filter tolerated in 2025.2, app-only in 2026.1) differ
  between the pinned releases — pin the rendered paste config in unit
  tests and run the e2e/tempest legs against both releases.

## Notes

- This skill is read-only with respect to the codebase; its outputs are
  GitHub issues (and this analysis). Implementation belongs to the
  sub-issues.
- If the user only wants the analysis, deliver the phase plan as text and
  skip issue creation — but still report what the inventory script found.
