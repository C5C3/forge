<!--
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
-->

# CobaltCore (C5C3) Operator

A Kubernetes-native OpenStack distribution for operating Hosted Control Planes across a multi-cluster topology
(Management, Control Plane, Hypervisor, Storage). This repository delivers everything needed for a fully
self-contained Keystone deployment stack — from infrastructure deployment manifests through the Keystone Operator
to the c5c3-operator orchestration layer — built with Operator SDK (Go), controller-runtime, and Kubebuilder.

The implementation follows the documented Keystone-first strategy: the Keystone Operator serves as the reference
implementation establishing patterns for all subsequent operators. The c5c3-operator is implemented with
Keystone-only orchestration, ready to be extended for additional services later.

The architecture is organized as a Go Workspace monorepo with a shared library (`internal/common/`), individual
operator modules (`operators/keystone/`, `operators/c5c3/`), container image builds (`images/`), declarative
infrastructure deployment manifests (`deploy/`), and comprehensive tests at every level (unit, envtest integration,
Chainsaw E2E). Test infrastructure — builders, simulators, assertion helpers — is built before any operator logic.

## Phase 1: Project Foundation & Test Infrastructure

Initialize the Go Workspace monorepo, scaffold the shared library and operator modules, establish the build system,
test harnesses, and CI pipeline so that `make test`, `make test-integration`, and `make lint` pass green.

- **S001: Go Workspace & Module Scaffolding** [high]
  Create `go.work` with `use` directives for `./internal/common`,
  `./operators/keystone`, `./operators/c5c3`. Create the full directory
  structure (`internal/common/`, `operators/keystone/`, `operators/c5c3/`,
  `images/`, `releases/`, `deploy/`, `tests/e2e/`). Initialize `go.mod`
  for each module with appropriate dependencies (controller-runtime v0.23+,
  k8s.io/apimachinery v0.35+). Scaffold `main.go` for both operators with
  scheme registration, manager setup, leader election, and health probes.
  Create the top-level Makefile with targets: `generate`, `manifests`,
  `build`, `test`, `test-integration`, `lint`, `docker-build`,
  `helm-package`, `e2e`, `deploy-infra`, `install-test-deps`. Configure
  golangci-lint (`.golangci.yml`) with standard linters and exclusions
  for generated code.
- **S002: Shared Test Infrastructure** [high] (depends on: S001)
  Implement the reusable test framework in `internal/common/testutil/`:
  envtest bootstrap (`SetupEnvTest()` that starts API server + etcd,
  installs own and external CRDs), fake CRD YAML schemas in `fake_crds/`
  for MariaDB Operator, ESO, cert-manager, Memcached, RabbitMQ. Fluent
  test data builders (`KeystoneBuilder`, `ControlPlaneBuilder`,
  `SecretBuilder` with chainable `With` methods). External operator
  simulators (`SimulateMariaDBReady`, `SimulateMemcachedReady`,
  `SimulateExternalSecretSync`, `SimulateJobComplete`). Custom gomega
  assertion helpers (`AssertCondition`, `EventuallyCondition`,
  `AssertResourceExists`, `AssertResourceNotExists`). Configure Chainsaw
  (`tests/e2e/chainsaw-config.yaml`) with directory structure for
  `keystone/`, `c5c3/`, `infrastructure/` tests.
- **S003: GitHub Actions CI Workflow (Minimal)** [medium] (depends on: S001)
  Create `.github/workflows/ci.yaml` running on PRs and pushes to main.
  Jobs: `lint` (golangci-lint-action), `test` (`make test`),
  `test-integration` (`make test-integration`). All jobs must pass for
  PR merge.

## Phase 2: Shared Library Packages

Implement the `internal/common/` packages that all operators depend on. Pure-function packages are tested through
table-driven unit tests targeting 80%+ coverage. Kubernetes-interacting packages are validated through envtest
integration tests.

- **S004: Common Types & Pure Function Packages** [high] (depends on: S001)
  Implement all shared type definitions in `internal/common/types/`:
  `ImageSpec`, `DatabaseSpec` (ClusterRef/Host mutual exclusivity),
  `MessagingSpec`, `CacheSpec`, `SecretRefSpec`, `PolicySpec`,
  `PluginSpec`, `MiddlewareSpec`, `PipelinePosition`. Implement
  `conditions/` package (`SetCondition`, `IsReady`, `GetCondition`,
  `AllTrue`). Implement `config/` package (`RenderINI`, `MergeDefaults`,
  `InjectSecrets`, `InjectOsloPolicyConfig`). Implement `plugins/`
  package (`RenderPastePipeline`, `RenderPluginConfig`). Implement
  `policy/` package (`RenderPolicyYAML`, `MergePolicies`,
  `ValidatePolicyRules`). Write table-driven unit tests for all functions.
- **S005: Kubernetes-Interacting Packages** [high] (depends on: S004)
  Implement all packages that interact with the Kubernetes API:
  `config/` ConfigMap helpers (`CreateImmutableConfigMap` with
  content-hash naming and owner references). `secrets/` package
  (`WaitForExternalSecret`, `IsSecretReady`, `GetSecretValue`,
  `EnsurePushSecret`). `database/` package (`EnsureDatabase`,
  `EnsureDatabaseUser`, `RunDBSyncJob`). `deployment/` package
  (`EnsureDeployment`, `EnsureService`, `IsDeploymentReady`). `job/`
  package (`RunJob`, `EnsureCronJob`, `IsJobComplete`). `tls/` package
  (`EnsureCertificate`, `GetTLSSecret`). `policy/` loader
  (`LoadPolicyFromConfigMap`). Write envtest integration tests for all
  packages verifying actual Kubernetes object creation and status handling.

## Phase 3: Keystone Container Image Build Pipeline

Create the container image infrastructure for building the Keystone service image as a multi-stage Docker build,
including base images, release configuration, and the GitHub Actions build workflow.

- **S006: Keystone Container Image & Release Config** [high]
  Create base images: `images/python-base/Dockerfile` (Ubuntu Noble,
  Python 3.12, runtime libraries, service user UID 42424, non-root) and
  `images/venv-builder/Dockerfile` (build dependencies, uv package
  manager, virtualenv at `/var/lib/openstack/`). Create the Keystone
  service Dockerfile `images/keystone/Dockerfile` as a multi-stage build:
  Stage 1 (venv-builder) installs Keystone + extras via `uv pip install`
  with `--constraint upper-constraints.txt`; Stage 2 (python-base) copies
  compiled virtualenv, adds Keystone runtime packages. Create release
  configuration: `releases/2025.2/source-refs.yaml`,
  `releases/2025.2/upper-constraints.txt`,
  `releases/2025.2/extra-packages.yaml`, and
  `scripts/apply-constraint-overrides.sh`. Verify image builds and
  `keystone-manage --version` succeeds.
- **S007: Container Image CI Workflow** [medium] (depends on: S006)
  Create `.github/workflows/build-images.yaml` with matrix build for
  service images, GHCR push, multi-arch support (linux/amd64,
  linux/arm64), tag schema
  (`<service>:<upstream-version>-<c5c3-patch>-<branch>-<sha>`). Include
  smoke test step running `keystone-manage --version`.

## Phase 4: Infrastructure Deployment Stack

Create the declarative deployment manifests for all infrastructure dependencies organized under `deploy/`,
including FluxCD HelmReleases, OpenBao HA deployment, ESO integration, and infrastructure CRs.

- **S008: FluxCD & Infrastructure Manifests** [high]
  Create `deploy/flux-system/sources/` with HelmRepository CRs for
  cert-manager, mariadb-operator, ESO, OpenBao, memcached-operator.
  Create `deploy/flux-system/releases/` with HelmRelease CRs for
  cert-manager (with default ClusterIssuer), MariaDB Operator, Memcached
  Operator, ESO — with production-ready values and `dependsOn` ordering.
  Create `deploy/infrastructure/mariadb.yaml` (MariaDB Galera cluster,
  3 replicas) and `deploy/infrastructure/memcached.yaml` (Memcached CR,
  3 replicas). Add `deploy/flux-system/kustomization.yaml` for FluxCD
  integration.
- **S009: OpenBao Deployment & Secret Management** [high]
  Create `deploy/openbao/helmrelease.yaml` (HA 3-replica Raft, TLS via
  cert-manager, injector disabled). Create idempotent bootstrap scripts
  in `deploy/openbao/bootstrap/`: init-unseal, secret engines (KV v2,
  PKI), auth methods (Kubernetes auth per cluster, AppRole for CI/CD),
  policy application. Create HCL policies in `deploy/openbao/policies/`:
  `eso-control-plane.hcl`, `eso-hypervisor.hcl`, `eso-storage.hcl`,
  `eso-management.hcl`, `push-app-credentials.hcl`, `push-ceph-keys.hcl`,
  `ci-cd-provisioner.hcl`, `pki-issuer.hcl`. Create ESO integration:
  `deploy/eso/clustersecretstore.yaml` (OpenBao with K8s auth) and
  `deploy/eso/externalsecrets/` for Keystone DB and admin credentials.
- **S010: Infrastructure Deployment Automation & E2E** [medium]
  (depends on: S008, S009)
  Create `make deploy-infra` target deploying the full stack to kind in
  correct order: cert-manager, OpenBao, ESO, MariaDB Operator, Memcached
  Operator, infrastructure CRs, ExternalSecrets. Include
  `make teardown-infra` for cleanup. Create Chainsaw E2E test
  `tests/e2e/infrastructure/infra-stack-health/chainsaw-test.yaml`
  validating all components come up healthy.

## Phase 5: Keystone CRD & Webhooks

Define the Keystone API types with full Kubebuilder markers, implement defaulting and validation webhooks,
and write tests for CRD registration and webhook behavior.

- **S011: Keystone API Types, Webhooks & CRD Generation** [high]
  (depends on: S001, S004)
  Define `operators/keystone/api/v1alpha1/keystone_types.go`: Keystone
  root type with Kubebuilder markers (object:root, subresource:status,
  printcolumn for Ready/Endpoint/Age). KeystoneSpec with Replicas (min 1,
  default 3), Image, Database (XValidation for clusterRef/host mutual
  exclusivity), Cache, Fernet (RotationSchedule default `"0 0 * * 0"`,
  MaxActiveKeys min 3 default 3), Federation, Bootstrap (AdminUser default
  "admin", AdminPasswordSecretRef, Region default "RegionOne"), Middleware,
  Plugins, PolicyOverrides (XValidation requiring at least one source),
  ExtraConfig. KeystoneStatus with Conditions and Endpoint. Implement
  defaulting webhook (`Default()`) and validation webhook
  (`ValidateCreate`/`ValidateUpdate` with cron validation, duplicate
  plugin detection, policy source requirement). Generate CRD YAML via
  controller-gen. Write unit tests for every webhook rule.
- **S012: Keystone CRD Tests** [medium] (depends on: S002, S011)
  Write envtest integration test verifying CRD registration, valid CR
  acceptance, invalid CR schema rejection, and webhook defaulting. Create
  Chainsaw E2E test `tests/e2e/keystone/invalid-cr/` asserting webhook
  rejection (HTTP 422) for invalid cron expressions and duplicate plugin
  sections.

## Phase 6: Keystone Reconciler & Sub-reconciler Pattern

Implement the KeystoneReconciler following the sequential sub-reconciler pattern with condition progression,
RBAC markers, watches on owned resources, and appropriate requeue delays.

- **S013: Keystone Reconciler & Sub-reconcilers** [high]
  (depends on: S004, S005, S011)
  Implement `KeystoneReconciler` with Client, Scheme, Recorder fields.
  `SetupWithManager` with watches for owned Deployments, Services,
  ConfigMaps, Jobs, CronJobs, Secrets. RBAC markers for all permissions.
  Main `Reconcile` function calling sub-reconcilers sequentially:
  `reconcileSecrets` (check ESO Secrets, set SecretsReady),
  `reconcileDatabase` (managed/brownfield MariaDB CRs + db\_sync Job,
  set DatabaseReady), `reconcileFernetKeys` (initial generation +
  rotation CronJob + PushSecret, set FernetKeysReady),
  `reconcileConfig` (build keystone.conf + api-paste.ini + policy.yaml,
  create immutable ConfigMap), `reconcileDeployment` (Deployment +
  Service with probes and volume mounts, set DeploymentReady),
  `reconcileBootstrap` (bootstrap Job, set BootstrapReady). Aggregate
  Ready condition when all sub-conditions True. Wire into
  `operators/keystone/main.go` with scheme registration, webhooks,
  and health probes.
- **S014: Keystone Reconciler Integration Tests** [high]
  (depends on: S002, S013)
  Write envtest integration tests exercising the full reconciliation
  loop: create prerequisite Secrets (simulating ESO), create Keystone CR,
  verify condition progression (SecretsReady, DatabaseReady,
  FernetKeysReady, DeploymentReady, BootstrapReady, Ready), verify
  Deployment/Service/ConfigMap/Job creation, verify status fields. Use
  simulators for MariaDB readiness and Job completion. Target 70%+
  coverage for reconciler code.

## Phase 7: Keystone Dependencies & E2E Tests

Implement the detailed dependency interactions, Fernet key rotation full cycle, and the complete Chainsaw E2E
test suite for the Keystone Operator.

- **S015: Keystone Dependency Details** [high] (depends on: S013)
  Implement detailed dependency interactions: DB connection string
  assembly for managed mode (resolve from MariaDB CR status) and
  brownfield mode (explicit host/port). Memcached server list discovery
  (pod DNS from Memcached CR in managed mode, explicit list in brownfield
  mode). Complete Fernet rotation CronJob (fernet\_rotate command,
  writable Secret mount, maxActiveKeys enforcement, Deployment annotation
  for rolling restart). Complete bootstrap Job (admin-credentials mount,
  bootstrap arguments, backoffLimit, ttlSecondsAfterFinished). Write
  unit tests for connection string assembly, memcached discovery, and
  envtest tests for CronJob and Job details.
- **S016: Keystone Chainsaw E2E Test Suite** [medium] (depends on: S013)
  Create the full Chainsaw E2E test suite in `tests/e2e/keystone/`:
  `basic-deployment/` (happy path, all conditions Ready=True),
  `missing-secret/` (SecretsReady=False, recovery on Secret creation),
  `fernet-rotation/` (CronJob schedule, manual trigger, key update,
  rolling restart), `scale/` (replicas 3 to 5 to 2),
  `deletion-cleanup/` (owner reference garbage collection),
  `policy-overrides/` (policy.yaml in ConfigMap, oslo\_policy config),
  `middleware-config/` (api-paste.ini pipeline modification),
  `brownfield-database/` (explicit host/port, no MariaDB CRs),
  `image-upgrade/` (rolling update, Ready maintained).

## Phase 8: Keystone Operator Packaging & CI

Create the operator container image, Helm chart, and complete the GitHub Actions CI pipeline with
coverage thresholds and FluxCD integration validation.

- **S017: Keystone Operator Packaging** [high] (depends on: S013)
  Create `operators/keystone/Dockerfile` (golang:1.25 builder,
  distroless/static:nonroot runtime, CGO\_ENABLED=0). Create Helm chart
  in `operators/keystone/helm/keystone-operator/`: Chart.yaml,
  values.yaml (image, replicas, resources, leaderElection, webhook,
  metrics, serviceAccount), CRD manifests, templates (deployment,
  service, serviceaccount, clusterrole, clusterrolebinding,
  webhook-configuration, \_helpers.tpl). Create FluxCD HelmRelease
  `deploy/flux-system/releases/keystone-operator.yaml`.
- **S018: Complete CI Pipeline & Coverage** [high]
  (depends on: S003, S017)
  Extend `.github/workflows/ci.yaml` with full job matrix: lint, test
  (unit + integration per operator), E2E (kind + Chainsaw), build-and-push
  (docker + GHCR on main/tags), helm-push (OCI registry on main/tags).
  Tag-triggered builds produce versioned images, charts, and GitHub
  Release. Configure `.codecov.yml` with thresholds: 80%+ for
  `internal/common/`, 70%+ for operator controllers, 90%+ for webhooks.

## Phase 9: c5c3-operator (Keystone-only)

Scaffold the c5c3-operator module and implement the ControlPlane CRD with Keystone-only orchestration,
including infrastructure lifecycle, service CR projection, policy merging, auxiliary CRDs, and rollout strategy.

- **S019: ControlPlane & Auxiliary CRD Types** [high] (depends on: S001, S004)
  Define ControlPlane CRD in `operators/c5c3/api/v1alpha1/`: ControlPlane
  root type (printcolumn for Ready/Phase/Age), ControlPlaneSpec with
  OpenStackRelease (pattern `^\d{4}\.\d$`), Region, Infrastructure
  (database, messaging, cache sub-specs), Services.Keystone
  (enabled/replicas/fernet/policyOverrides), Global (TLS,
  PolicyOverrides). ControlPlaneStatus with Conditions (Ready,
  InfrastructureReady, KeystoneReady), UpdatePhase enum, Services map.
  Define SecretAggregate CRD (multi-source secret merging: Sources with
  SecretRef and optional key filtering, Target name). Define
  CredentialRotation CRD (TargetServiceUser, RotationType, Schedule with
  IntervalDays/PreRotationDays, GracePeriodDays). Implement defaulting
  and validation webhooks for ControlPlane. Generate CRD manifests.
  Write unit tests for all webhook rules.
- **S020: ControlPlane Orchestration Reconciler** [high]
  (depends on: S005, S011, S019)
  Implement `ControlPlaneReconciler` with `SetupWithManager` watching
  ControlPlane and owned resources (MariaDB, Memcached, Keystone CRs).
  Phase 1 (`reconcileInfrastructure`): create/update MariaDB Galera and
  Memcached CRs from infrastructure spec, set owner references, wait for
  Ready, set InfrastructureReady condition. Phase 2
  (`reconcileKeystone`): create/update Keystone CR with image tag from
  openStackRelease, clusterRefs to infrastructure CRs, fernet cron from
  rotationInterval conversion, merged policyOverrides, bootstrap region.
  Implement `projectPolicyOverrides` (global rules as base, per-service
  overrides). Implement `intervalToCron` conversion (e.g. `"168h"` to
  `"0 0 * * 0"`). Implement UpdatePhase state machine (Idle, Validating,
  UpdatingInfra, UpdatingKeystone, Verifying, Complete, RollingBack).
  Write table-driven unit tests for policy projection and cron conversion.
- **S021: Auxiliary Reconcilers** [medium] (depends on: S019)
  Implement `SecretAggregateReconciler`: watch SecretAggregate CR and
  source Secrets, merge data into target Secret with owner reference.
  Implement `CredentialRotationReconciler`: track rotation schedule,
  create new credentials in pre-rotation window, delete old after grace
  period. Write envtest integration tests for both.
- **S022: c5c3-operator Wiring, Tests & Packaging** [high]
  (depends on: S020, S021)
  Wire all reconcilers and webhooks into `operators/c5c3/main.go`.
  Write envtest integration tests for the full orchestration: create
  ControlPlane CR, verify infrastructure CR creation, simulate Ready,
  verify Keystone CR projection (image tag, clusterRefs, merged policies),
  simulate Keystone Ready, verify ControlPlane Ready=True. Create Chainsaw
  E2E tests: `full-controlplane/`, `single-service/`,
  `update-phase-tracking/`, `policy-projection/`. Create Helm chart in
  `operators/c5c3/helm/c5c3-operator/` (same pattern as keystone-operator
  chart). Extend CI pipeline matrix for the c5c3-operator.

## Phase 10: End-to-End Integration Hardening

Exercise the complete deployment lifecycle end-to-end, covering advanced scenarios, failure recovery,
stress tests, and validation of the full secret management chain.

- **S023: Full-Stack E2E Validation** [high] (depends on: S016, S022)
  Create an end-to-end integration test on kind: deploy full
  infrastructure stack (Phase 4), deploy both operators, apply
  ControlPlane CR, verify the c5c3-operator creates infrastructure CRs,
  waits for readiness, creates Keystone CR with correct projection.
  Verify the Keystone Operator deploys a fully functional Keystone service
  with Ready=True and the API responds on /v3. Validate the complete
  secret chain: OpenBao bootstrap writes secrets, ESO ExternalSecrets
  sync to K8s Secrets, operators consume Secrets for DB connection
  strings and admin password.
- **S024: Advanced Scenarios & Stress Tests** [medium]
  (depends on: S023)
  Test ControlPlane-driven image upgrade (rolling update, Ready
  maintained). Test database failure recovery (MariaDB failure, condition
  degradation, restoration). Test complete Fernet rotation cycle (initial
  generation, CronJob trigger, maxActiveKeys enforcement, PushSecret
  backup). Test config change propagation (ControlPlane to Keystone CR to
  ConfigMap to Deployment restart). Validate leader election (2 replicas,
  failover). Stress test with 10+ rapid ControlPlane spec changes
  verifying convergence without resource leaks.

## Phase 11: Release Preparation

Validate the complete release workflow, create deployment documentation, and ensure all CI gates pass
for a v0.1.0 release.

- **S025: Release Workflow & CI Gates** [high] (depends on: S018, S022)
  Validate the full release process: SemVer version bump, `git tag
  v0.1.0`, CI builds all artifacts (Keystone service image, Keystone
  Operator image, c5c3-operator image, Helm charts), pushes to GHCR,
  FluxCD HelmRelease picks up the new version. Add generated code
  verification gate (`make manifests && make generate &&
  git diff --exit-code`). Verify all CI gates pass: lint green, tests
  with coverage thresholds, E2E scenarios pass reliably (zero flakiness
  across 3 consecutive runs).
- **S026: Documentation** [high] (depends on: S023)
  Write deployment guide: prerequisites, deploy infrastructure stack,
  deploy c5c3-operator, apply ControlPlane CR, verify Keystone running,
  troubleshooting. Document all CRD API surfaces (ControlPlane, Keystone,
  SecretAggregate, CredentialRotation) with example CRs for common use
  cases. Document Helm chart values for both operators with resource
  recommendations and example overrides.
