---
title: CI Workflow
quadrant: infrastructure
---

::: v-pre

# CI Workflow

Reference documentation for the GitHub Actions CI workflow.

Repeated E2E logic is factored into reusable shell scripts (`hack/ci-*.sh`) and a
composite GitHub Action (`.github/actions/setup-e2e-infra/`), reducing duplication across
the `e2e-infra`, `e2e-operator`, and `tempest` jobs.

The `build-e2e-images` job centralises E2E image builds. It builds the images whose
sources the pull request changed and pushes those to GHCR under run-scoped tags
(`e2e-${run_id}-<orig_tag>`), then resolves every other image to the digest behind
the tag `main` last published. Both kinds go into the job's `image-map` output. The
`e2e-operator`, `e2e-chaos`, and `tempest` jobs `docker pull` whatever the map names
via the `load-e2e-images` composite action and re-tag the images to their canonical
local references, so a pull request that touches one operator spends about four
minutes here instead of the 39 the full build took. The `cleanup-e2e-tags` job prunes
the run-scoped tags at the end of the workflow, with a nightly safety net in
`cleanup-images.yaml` for cancelled runs (GH-310).

## File Location

`.github/workflows/ci.yaml`

The file uses the `.yaml` extension (matching `reuse.yaml` and `deploy-docs.yaml`) and
quotes the trigger key as `"on"` to prevent YAML boolean interpretation.

## Trigger Events

The workflow triggers on three event types:

| Event | Scope | Description |
| --- | --- | --- |
| `push` | `branches: [main]` | Runs on every push to the main branch |
| `push` | `tags: ["v*"]` | Runs on every v-prefixed tag push (triggers publish and release jobs) |
| `pull_request` | `branches: [main]`, `types: [opened, synchronize, reopened, labeled]` | Runs on every pull request targeting main; the `labeled` type lets a `ci:*` label schedule jobs the paths alone did not |

Most gate, test, and E2E jobs run **only on `pull_request` events** — they each carry a
`github.event_name == 'pull_request'` guard. `test-shell` is the exception. It carries no
guard and also runs on pushes to `main` and on tag pushes. A breakage that lands on `main`
therefore turns main's own run red at the commit that caused it, instead of surfacing on
the next PR branch. Otherwise, pushes to `main` and tag pushes
(`v*`) run only the publish and release jobs (`build-and-push`,
`merge-operator-images`, `helm-push`, `github-release`): the merged commit's PR was
already green, so the E2E suite is not re-run on push
("publish-only-on-merge"). On tag pushes the `changes` job forces all areas and all
operators active, so every operator's images and charts are published regardless of
which files the tagged commit touched.

## Change classes and labels

A pull request runs the jobs whose inputs it changes. The `changes` job
classifies the changed paths with `dorny/paths-filter`, and
`hack/ci-resolve-changes.sh` turns those classes into one flag per job. A job's
inputs are the code it tests, the images it loads, the suite it runs, and the
scripts it calls. Nothing runs because Go code changed somewhere else.

| Change class | Paths | What it schedules |
| --- | --- | --- |
| `<op>` (one per operator) | `operators/<op>/**` | that operator's Go gates, its `test` and `test-integration` legs, its `e2e-operator` leg. `keystone` adds `e2e-operator-upgrade`; `c5c3` adds the three ControlPlane jobs |
| `image_<svc>` | `images/<svc>/**`, `patches/<svc>/**` | that service's `e2e-operator` leg |
| `image_ovn`, `image_proxy`, `image_tempest` | the OVN, federation-proxy and Tempest image sources | the `ovn` and `keystone` e2e legs; the Tempest image rebuild |
| `go_common` | `internal/**`, `go.work*`, `operators/Dockerfile`, `.golangci.yml` | every operator's Go gates, every `e2e-operator` leg, `e2e-operator-upgrade` |
| `images_base` | `images/python-base/**`, `images/venv-builder/**`, `releases/**`, `scripts/**`, `overrides/**` | every service image, both Tempest images, every service operator's e2e leg |
| `tempest_src` and `tempest_<svc>` | `images/tempest/**`, `tests/tempest/**`, the Tempest scripts | `tempest`, narrowed to the services whose configuration changed |
| `tests_e2e_<op>` | `tests/e2e/<op>/**`, `tests/e2e/<op>-operator/**` | that operator's `e2e-operator` leg |
| `tests_controlplane`, `tests_controlplane_sso`, `tests_external_keystone` | the three ControlPlane suites | the job that runs that suite |
| `tests_chaos`, `tests_multicluster`, `tests_operator_upgrade`, `tests_prometheus` | each suite's own tree | the job that runs it |
| `e2e_infra` | `tests/e2e/infrastructure/**` | `e2e-infra` |
| `e2e_shared`, `e2e_openbao`, `ci_plumbing`, `makefile` | the deploy stack, the infra scripts, the shared composite actions, the workflow file, the remaining CI helpers | the canary: `e2e-infra` plus the keystone `e2e-operator` leg. `deploy/openbao/**` adds the barbican leg |
| `actionlint` | `.github/**` | `actionlint` |
| `docs`, `helm`, `target_cluster_chart` | as before | `docs`, `helm-validate`, `helm-push-target-cluster` |
| `publish_legacy` | the wide paths a push publishes from | push events only, ignored on pull requests |

The canary is the deliberate gap. A change to the substrate every e2e job stands
on could break any of them, and running all of them costs about 450 runner
minutes. Instead it runs the infrastructure suite and one operator leg, and a
label asks for the rest.

### Labels

Five labels add jobs. None of them ever removes one.

| Label | Schedules |
| --- | --- |
| `ci:full` | everything, and builds every image |
| `ci:tempest` | the Tempest legs of the services the pull request touches, or the keystone legs when it touches none |
| `ci:chaos` | both `e2e-chaos` legs. `run-chaos` is an alias |
| `ci:controlplane` | `e2e-controlplane`, `e2e-controlplane-sso`, `e2e-external-keystone` |
| `ci:multicluster` | `e2e-multicluster` |

The `labeled` trigger means a label applied after the last push starts a run that
evaluates the new label set. A label outside this set resolves to nothing: the
`changes` job emits `noop=true`, every gated job skips, and the run lands in a
concurrency group of its own so it does not cancel the pipeline in flight. See
[Concurrency](#concurrency).

On a `v*` tag push every flag is forced on. On a push to `main` the operator
matrix keeps its publish semantics, driven by the `publish_legacy` class, so what
a merge publishes is unchanged.

## Environment Variables

Top-level environment variables centralise registry configuration and pin tool versions
for CI reproducibility:

```yaml
env:
  REGISTRY: ghcr.io
  IMAGE_PREFIX: ghcr.io/c5c3
  KIND_CLUSTER: cobaltcore
  KIND_VERSION: v0.32.0
  CONTROLLER_GEN_VERSION: v0.21.0
  GOFUMPT_VERSION: v0.10.0
  GOLANGCI_LINT_VERSION: v2.13.1
```

`REGISTRY` and `IMAGE_PREFIX` are referenced by the `build-and-push`, `helm-push`,
`e2e-operator`, and `tempest` jobs to construct image names and registry URLs.
`KIND_CLUSTER` is the single source of truth for every E2E job's kind cluster name
(mirroring the `CLUSTER_NAME` default in `hack/deploy-infra.sh`). `KIND_VERSION` is
the kind binary every E2E job creates its cluster with, passed to
`helm/kind-action`'s `version` input. The action defaults to v0.31.0, whose kindnetd
does not enforce NetworkPolicy egress against the post-DNAT destination; pinning it
keeps CI on the enforcing build and in lockstep with the `KIND_VERSION` in
`hack/install-test-deps.sh` that local development installs. Renovate groups the two
pins so they bump in one pull request, and
`tests/unit/renovate/workflow_pins_custommanager_test.sh` asserts they agree.
`CONTROLLER_GEN_VERSION` is used by `verify-codegen` to pin controller-gen to a specific
version. `GOFUMPT_VERSION` is used by `format-check` to pin gofumpt to a specific version; the same version is mirrored in the Makefile (`GOFUMPT_VERSION ?= v0.10.0`) so
that `make fmt` and `make format-check` use a consistent version locally.
`GOLANGCI_LINT_VERSION` pins the golangci-lint binary installed by the `lint` job and
keys its analysis cache. `setup-envtest` is installed via `@release-0.23` because the
sub-module does not publish its own release tags.

## Permissions

Top-level permissions are restricted to least privilege:

```yaml
permissions:
  contents: read
```

Jobs that need elevated access declare per-job `permissions:` blocks:

| Job | Additional Permissions | Reason |
| --- | --- | --- |
| `build-and-push` | `packages: write` | Push per-platform operator image digests to GHCR |
| `merge-operator-images` | `packages: write` | Push final multi-arch operator image manifest list |
| `helm-push` | `packages: write` | Push Helm charts to GHCR OCI registry |
| `github-release` | `contents: write` | Create GitHub Releases |

## Job Dependency DAG

The workflow defines 34 jobs organised in a directed acyclic graph. Every gate, test,
and E2E job except `test-shell` additionally carries a
`github.event_name == 'pull_request'` guard; the publish and release jobs run only on
push events:

```
Gate Jobs (pull requests; test-shell also on push):
  lint ────────────────────────┐   (go == 'true')
  format-check                 │   (go == 'true')
  shellcheck ──────────────────┤   (always on PRs)
  feature-ids                  │   (always on PRs)
  test-shell                   │   (PRs + push: main, tags)
  verify-codegen ──────────────┤   (go == 'true')
  verify-invalid-cr-fixtures ──┤   (always on PRs)
  chainsaw-lint ───────────────┤   (always on PRs, unless the run is a no-op)
  actionlint ──────────────────┤   (actionlint == 'true')
  test (matrix) ───────────────┼──> build-e2e-images ──> E2E Jobs
  test-integration ────────────┘

Conditional Jobs (pull requests only, path-filtered via changes job):
  test-race ────> needs: [changes], if: needs.changes.outputs.go == 'true'
  govulncheck ─> needs: [changes], if: needs.changes.outputs.go == 'true'
  helm-validate ──> needs: [changes], if: needs.changes.outputs.helm == 'true'
  docs ──────────> needs: [changes], if: needs.changes.outputs.docs == 'true'

Image Build (pull requests only, depends on gates):
  build-e2e-images ──> needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, verify-invalid-cr-fixtures, chainsaw-lint]

E2E Jobs (pull requests only, depend on build-e2e-images):
  e2e-infra ──────> needs: [changes], if: needs.changes.outputs.e2e-infra == 'true'
  e2e-operator ───> needs: [changes, build-e2e-images]
  e2e-operator-upgrade > needs: [changes, build-e2e-images], if: needs.changes.outputs.e2e-operator-upgrade == 'true'
  e2e-chaos ──────> needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images, e2e-operator]
  e2e-prometheus ─> needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]
                     if: needs.changes.outputs.e2e-prometheus == 'true'
  e2e-controlplane > needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]
                     if: needs.changes.outputs.e2e-controlplane == 'true'
  e2e-controlplane-sso > needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]
                     if: needs.changes.outputs.e2e-controlplane == 'true'
  e2e-external-keystone > needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]
                     if: needs.changes.outputs.e2e-controlplane == 'true'
  e2e-ovn-overlay > needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]
                     if: needs.changes.outputs.e2e-ovn-overlay == 'true'
  tempest ────────> needs: [changes, build-e2e-images, e2e-infra, e2e-operator, e2e-chaos, e2e-prometheus]
  cleanup-e2e-tags > needs: [build-e2e-images, e2e-operator, e2e-operator-upgrade, e2e-chaos, e2e-ovn-overlay, tempest]

Publish Jobs (push events only — main and v* tags; publish-only-on-merge):
  build-and-push (matrix: operator × platform) ──> needs: [changes], if: push && has-e2e-operators == 'true'
    └──> merge-operator-images ──> needs: [changes, build-and-push], if: push event
  helm-push ──> needs: [changes], if: push && has-e2e-operators == 'true'

Release Job (v* tags only, depends on publish):
  github-release ──> needs: [changes, merge-operator-images, helm-push], if: v* tag
```

The E2E jobs (`e2e-infra`, `e2e-operator`, `e2e-operator-upgrade`, `e2e-chaos`,
`e2e-prometheus`, `e2e-controlplane`, `e2e-external-keystone`, `tempest`) share
infrastructure setup via
the `setup-e2e-infra` composite action and diagnostic teardown via
`hack/ci-dump-diagnostics.sh`. They run on the `self-hosted` runners, as does
`test-integration` — with two exceptions: the keystone leg of the
`e2e-operator` matrix and the `pod` leg of the `e2e-chaos` matrix are pinned
back to the `blacksmith-4vcpu-ubuntu-2404` runner for now, because those suites
have not been stable on the self-hosted runners.

## Jobs

### lint

Runs golangci-lint using the project's `.golangci.yml` configuration.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `actions/cache@v5` | Persists the golangci-lint analysis cache, keyed on the Go and golangci-lint versions |
| 4 | `golangci/golangci-lint-action@v9` | Installs golangci-lint binary (`install-only: true`); version pinned via `GOLANGCI_LINT_VERSION` (`v2.13.1`) |
| 5 | `make lint` | Runs golangci-lint per module via the Makefile |

The `golangci-lint-action@v9` step is used with `install-only: true`, which installs the
pinned golangci-lint binary (and caches it) without running lint. The actual linting is
delegated to `make lint`, which `cd`s into each module directory and runs
`golangci-lint run ./...` — a necessary pattern for Go multi-module workspaces. The
`actions/setup-go@v6` step is required because `install-only` mode does not set up Go
internally.

**Enabled linters** (12 total, configured in `.golangci.yml`):

| Linter | Category | Description |
| --- | --- | --- |
| `errcheck` | correctness | Checks for unchecked errors in Go code |
| `gocritic` | style | Provides diagnostics for bugs, performance, and style issues |
| `govet` | correctness | Reports suspicious constructs, roughly equivalent to `go vet` |
| `ineffassign` | correctness | Detects assignments to existing variables that are never used |
| `staticcheck` | correctness | Comprehensive static analysis rules from the staticcheck suite |
| `unused` | correctness | Checks for unused constants, variables, functions, and types |
| `bodyclose` | resource-leak | Checks whether HTTP response bodies are closed successfully |
| `errorlint` | correctness | Validates Go 1.13+ error wrapping patterns (`%w`, `errors.Is`, `errors.As`) |
| `exhaustive` | correctness | Checks exhaustiveness of enum switch statements |
| `gosec` | security | Inspects source code for security problems (hardcoded credentials, weak crypto, unsafe operations) |
| `nilerr` | correctness | Finds code that returns nil even after checking that an error is not nil |
| `noctx` | correctness | Detects HTTP requests and TLS dials missing `context.Context` propagation |

Generated code matching `zz_generated.*.go` is excluded from all lint checks via the
`exclusions.paths` configuration.

### format-check

Verifies all Go files conform to gofumpt formatting. gofumpt is a strict superset
of gofmt — it applies all standard gofmt rules plus additional formatting conventions for
consistency. Detects non-conforming files and prints a unified diff showing the required
changes, so developers can identify and fix formatting issues without guessing.

Only git-tracked Go files are checked (`git ls-files '*.go'`) to avoid unexpected failures
on generated, vendored, or tooling code that may not follow gofumpt conventions.

The same version and check logic are available locally via the Makefile: `make install-gofumpt`
installs the pinned version, `make format-check` mirrors the CI check, and `make fmt` applies
formatting to all tracked Go files. The Makefile targets use `xargs` without the `-r` flag
(unlike CI) for macOS portability — BSD `xargs` does not support `-r`. This is safe because
the repository always contains tracked `.go` files.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.go == 'true'`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `go install mvdan.cc/gofumpt@${{ env.GOFUMPT_VERSION }}` | Installs gofumpt at the pinned version (`v0.10.0`) |
| 4 | `git ls-files '*.go' \| xargs -r gofumpt -l` | Lists non-conforming tracked Go files; on failure, prints unified diff and exits 1 |

The check uses `git ls-files '*.go' | xargs -r gofumpt -l` to collect non-conforming files
from tracked sources only. If any are found, their paths are printed along with a unified
diff (`gofumpt -d`), and the job exits 1. The `-r` flag prevents `xargs` from running
`gofumpt` when no Go files are piped (GNU coreutils, available on `ubuntu-latest`).

Timeout: 8 minutes.

### shellcheck

Validates shell scripts with shellcheck to catch scripting issues early.
The shellcheck binary is pre-installed on `ubuntu-latest` runners.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `make shellcheck` | Runs `shellcheck --severity=warning` over `hack/*.sh` and the operator rotation scripts (`operators/*/internal/controller/scripts/*.sh`) |

Timeout: 8 minutes.

### feature-ids

Verifies the whole tracked tree (code, tests, CI, scripts, docs) is free of internal
feature/requirement IDs. Runs unconditionally on every pull request — not
path-filtered — so a stray ID added anywhere is caught. This job folds in the former
docs-only check.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `make check-feature-ids` | Greps the tracked tree for internal feature/requirement ID patterns and fails on any hit |

Timeout: 8 minutes.

### verify-invalid-cr-fixtures

Enforces the canonical-scaffold contract for the invalid-CR Chainsaw fixtures.
Runs `_generate.py --check` (drift mode) and the `test_generate.py` unit suite
(FIXTURES count + `chainsaw-test.yaml` cross-reference) so a hand-edit to any
`02-…/03-…/…/12-*.yaml` fixture, or a rename or removal that desynchronises FIXTURES
from `chainsaw-test.yaml`, fails the build before the heavy cluster-bound `e2e-operator`
job runs. Always-on because the check is sub-second and `python3` is preinstalled on
`ubuntu-latest` runners.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `make verify-invalid-cr-fixtures` | Runs `_generate.py --check` and `test_generate.py` |

Timeout: 8 minutes.

### chainsaw-lint

Schema-lints every Chainsaw test (`tests/**/chainsaw-test.yaml`) and configuration
(`tests/{e2e,e2e-chaos}/chainsaw-config.yaml`) via `chainsaw lint` so typos, removed
fields, or schema drift after a chainsaw version bump fail fast — before the
cluster-bound `e2e-operator` and `e2e-chaos` jobs spin up a kind cluster. Always-on
because no cluster is needed: chainsaw is restored from the shared testdeps cache via
the `setup-test-deps` composite action, the same one consumed internally by
`setup-e2e-infra`. A schema break therefore surfaces in `needs.*.result` for both
`build-e2e-images` and `e2e-chaos`.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `./.github/actions/setup-test-deps` | Restores the testdeps cache and runs `make install-test-deps` (puts `chainsaw` on `PATH`) |
| 3 | `make chainsaw-lint` | Runs `chainsaw lint test -f` and `chainsaw lint configuration -f` over every matching file under `tests/` |

Timeout: 8 minutes.

### actionlint

Static-lints every workflow file. actionlint resolves the expression contexts, so
a `needs.changes.outputs.<name>` that no job exports is caught here instead of
evaluating to the empty string and leaving a job unscheduled for good. It also
runs shellcheck over every `run:` script.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.actionlint == 'true'`
**Path filter:** `.github/**`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | Install actionlint | Downloads the pinned release, verifies `ACTIONLINT_SHA256` with `sha256sum --check --strict`, installs to `/usr/local/bin` |
| 3 | `actionlint -no-color` | Lints every workflow under `.github/workflows/` |

The binary is pinned and checksummed the way `verify-container-images.yaml`
installs `yq`. Renovate bumps `ACTIONLINT_VERSION`; the checksum then fails until
it is updated alongside, which is the intended signal.

`.github/actionlint.yaml` declares `blacksmith-4vcpu-ubuntu-2404`. actionlint
validates `runs-on:` against the labels GitHub itself provides, so a self-hosted
pool has to be declared there or the lint fails on a runner that exists.

Three `run:` scripts carry inline `# shellcheck disable=` directives for
deliberate word splitting: the release-list loops in `e2e-operator` (SC2086), the
digest expansion in `merge-operator-images` (SC2012, SC2046), and the version
appends in `build-images.yaml` (SC2129).

Timeout: 8 minutes.

### test-shell

Runs every shell unit test under `tests/unit/` (hack/, deploy/, docs/,
renovate/). The job carries no event guard, so it runs on pull requests and
on the push runs for `main` and `v*` tags: a breakage that lands on `main`
fails main's own run. Tests read repo files only (no cluster, no untrusted
input) and the job finishes in about a minute on a cold runner. Tests that
depend on `yq` or `kustomize` are written to skip gracefully when those
tools are missing; the job installs `kustomize` explicitly so the deploy/
overlay assertions run their full check set (`yq` is preinstalled on
ubuntu-latest).

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | Install kustomize | Downloads the pinned kustomize binary into `/usr/local/bin` |
| 3 | `make test-shell` | Iterates every `tests/unit/**/*_test.sh` and aggregates exit status |

Timeout: 8 minutes.

### test

Runs unit tests with a matrix strategy resolved per pull request out of
`[common, keystone, c5c3, horizon, glance, placement, barbican, ovn, neutron]`.
Each matrix leg tests a single target — either `internal/common` or one operator — producing
a single coverage profile uploaded to Codecov under a dedicated flag.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `make test-common` or `make test-operator` | Runs unit tests for the matrix target |
| 4 | `codecov/codecov-action@v5` | Uploads coverage profile with target-specific flag |

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}
```

The resolver builds that list: `common` when shared Go code or the `Makefile`
changed, plus every operator whose own code changed. A pull request touching one
operator runs one leg.

The `common` leg runs `make test-common` (producing `cover-unit-common.out`). Operator legs
run `make test-operator OPERATOR=<target>` (producing `cover-unit-<operator>.out`). This
deduplicates common coverage into a single leg instead of uploading it under each operator
flag.

**Coverage upload:**

```yaml
files: cover-unit-${{ matrix.target }}.out
flags: unit-${{ matrix.target }}
```

The `if: always()` condition ensures coverage is uploaded even when tests fail, so partial
coverage data is not lost.

### test-integration

Runs envtest-based integration tests with a matrix strategy resolved per pull
request out of `[common, keystone, c5c3, horizon, glance, placement, barbican, ovn, neutron]`
and coverage uploaded to Codecov. Requires `setup-envtest` to
download kubebuilder assets (kube-apiserver, etcd) for the test API server.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `go install setup-envtest@release-0.23` | Installs envtest asset downloader (pinned to release branch) |
| 4 | `make test-integration-common` or `make test-integration` | Runs integration tests for the matrix target |
| 5 | `codecov/codecov-action@v5` | Uploads coverage with `integration-<target>` flag |

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}
```

The resolver builds that list: `common` when shared Go code or the `Makefile`
changed, plus every operator whose own code changed. A pull request touching one
operator runs one leg.

The `common` leg runs `make test-integration-common` (producing
`cover-integration-common.out`), which tests `./internal/common/...` with
`-tags=integration`. Operator legs run `make test-integration OPERATOR=<target>` (producing
`cover-integration-<operator>.out`). Both targets set `KUBEBUILDER_ASSETS` via
`$(SETUP_ENVTEST) use <pinned-k8s-version> -p path`.

Timeout: 30 minutes (longer than unit tests to account for envtest startup).

### test-race

Runs all Go unit tests with the race detector enabled to catch data races in concurrent
operator code — reconcilers, watches, informer caches. Separate from the main
`test` job because the race detector adds 2–5x overhead. Uses `-count=1` to disable test
caching, since race conditions are non-deterministic and cached results could mask real
races.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.go == 'true'`
**Path filter:** Go source files (same filter as `test` and `test-integration`)

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `make test-race RACE_FLAGS="-count=1"` | Delegates to the Makefile so the module list stays in sync |

CI delegates to `make test-race` so the list of modules under race testing is defined in one
place (the Makefile's `OPERATORS` variable and `internal/common`). `RACE_FLAGS="-count=1"`
disables test caching — race conditions are non-deterministic, so cached results could mask
real races. No `continue-on-error` or `if: always()` — a detected data race fails the job
immediately.

This job runs independently and does **not** appear in any other job's `needs:` array. It is
not on the critical path for E2E or publish jobs, so race detector overhead does not slow
down the primary feedback loop. The corresponding local command is `make test-race`
(which omits `-count=1` via the default empty `RACE_FLAGS` for developer convenience).

Timeout: 30 minutes (accommodates 2–5x race detector overhead).

### govulncheck

Scans all Go modules for reachable vulnerabilities using govulncheck, the official Go
vulnerability scanner maintained by the Go team. Unlike dependency-list scanners,
govulncheck analyses call graphs to detect only vulnerabilities in code paths that are
actually reachable — reducing false positives. Catches supply-chain vulnerabilities at the
PR stage, before container images are built.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.go == 'true'`
**Path filter:** Go source files (same filter as `test`, `test-integration`, and `test-race`)

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `go install golang.org/x/vuln/cmd/govulncheck@latest` | Installs the latest govulncheck binary |
| 4 | `make govulncheck` | Delegates to `hack/ci-govulncheck.sh`, which scans `internal/common` and all `$(OPERATORS)` modules with an explicit allowlist |

govulncheck uses `@latest` intentionally — unlike other pinned tools (controller-gen,
gofumpt), pinning govulncheck to an old version defeats the purpose of vulnerability
scanning because the vulnerability database is updated frequently. This is a deliberate
deviation from the general pinning policy, justified by the security tool's nature.

The CI step delegates to `make govulncheck`, which runs `hack/ci-govulncheck.sh` over
`internal/common` and each operator in the `$(OPERATORS)` Makefile variable. govulncheck
has no native suppression flag, so the wrapper runs it in JSON mode per module, keeps
only the *reachable* symbol-level findings (the ones that fail the default text report),
and drops any whose advisory ID appears in the script's `ALLOWLIST` map. The build fails
if, and only if, a reachable finding survives the allowlist — matching govulncheck's
normal failure semantics while letting the project ride out advisories that have no fix
and no real exposure. Every allowlist entry carries a one-line justification, and if an
allowlisted advisory is no longer reported, the wrapper prints a notice so the stale
entry can be removed. Dependencies with known CVEs whose vulnerable functions are not
called in project code are reported as informational but do not fail the job.

This job runs independently and does **not** appear in any other job's `needs:` array. It
is not on the critical path for E2E or publish jobs, matching the `test-race` pattern.
When a new Go module is added to `go.work`, the `OPERATORS` variable in the Makefile must
be updated with the new module name. The verification test
(`tests/ci/verify_govulncheck_modules.sh`) catches drift between `go.work` and the
Makefile automatically.

Timeout: 15 minutes.

### verify-codegen

Verifies that generated code (CRD, webhook and RBAC manifests, deepcopy
functions, the chart RBAC rules templates) is committed and up-to-date. This is a gate job — it blocks merge alongside `lint`,
`test`, and `shellcheck`.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `go install controller-gen@${{ env.CONTROLLER_GEN_VERSION }}` | Installs the pinned code generator |
| 4 | `make manifests && make generate` | Regenerates CRD, webhook and RBAC (`config/rbac/role.yaml`) manifests and deepcopy functions |
| 5 | `make verify-crd-sync` | Verifies Helm chart CRD copies match controller-gen output |
| 6 | `make verify-helm-rbac` | Verifies each chart's `templates/_rbac-rules.tpl` matches the regenerated `config/rbac/role.yaml` |
| 7 | `git diff --exit-code` | Fails if any files changed (stale generated code) |

When the diff check fails, the job produces a GitHub Actions `::error::` annotation with
instructions to run `make manifests && make generate` locally and commit the result.

### docs

Builds the VitePress documentation site to catch broken links and build errors.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.docs == 'true'`
**Path filter:** `docs/**`, `package.json`, `package-lock.json`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Full history (`fetch-depth: 0`) for git-based features |
| 2 | `actions/setup-node@v6` | Node.js 24, npm cache enabled |
| 3 | `npm ci` | Installs dependencies from lockfile |
| 4 | `npm run docs:build` | Builds the documentation site |

### helm-validate

Validates Helm chart structure, template rendering, and unit tests for every
operator chart and the operator-library testbed without requiring a cluster.
Verifies the generated `values.schema.json` and `_rbac-rules.tpl` files are in
sync with their sources, vendors the shared `operator-library` subchart, then
runs `helm lint`, `helm template` with five value override scenarios, and
`helm unittest` for each chart to catch regressions at PR time. The chart list
is the `operators/*/helm/*-operator` and `operators/*/helm/*-testbed` globs, so
a new operator chart in that layout is validated without editing the job.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.helm == 'true'`
**Path filter:** `operators/*/helm/**`, `deploy/target-cluster/**`, `hack/gen-helm-values-schema.py`, `hack/gen-helm-rbac-rules.py`, `Makefile` (forced `true` on `v*` tag pushes)

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v5` | Installs Helm CLI (SHA-pinned) |
| 3 | `helm plugin install helm-unittest` | Installs helm-unittest plugin (pinned to `v1.0.3`) |
| 4 | `make verify-helm-schema` | Fails if any chart's `values.schema.json` has drifted from the shared generator |
| 5 | `make verify-helm-rbac` | Fails if any chart's `templates/_rbac-rules.tpl` has drifted from its committed `config/rbac/role.yaml` |
| 6 | `make helm-deps` | Vendors the `operator-library` subchart into each consumer chart's `charts/` |
| 7 | `helm lint` | Validates chart structure and syntax for every chart |
| 8 | `helm template` (5 scenarios) | Renders each chart with value overrides to catch broken conditionals and invalid YAML |
| 9 | `helm unittest` | Runs the unit test suites under each chart's `tests/` directory |

**Template scenarios (step 8), run against each chart:**

| Scenario | Values | Purpose |
| --- | --- | --- |
| 1 — default values | (none) | Validates baseline rendering with chart defaults |
| 2 — webhook disabled | `webhook.enabled=false` | Validates conditional exclusion of webhook resources |
| 3 — external service account | `serviceAccount.create=false`, `serviceAccount.name=existing-sa` | Validates ServiceAccount conditional logic |
| 4 — custom resources | `resources.limits.cpu=100m`, `resources.limits.memory=64Mi` | Validates resource override wiring |
| 5 — namespace-scoped RBAC | `rbac.namespaceScoped=true`, `webhook.enabled=false` | Validates Role/RoleBinding rendering instead of ClusterRole/ClusterRoleBinding. A chart that refuses the mode by design (ovn-operator, neutron-operator) fails the render with the documented `is not supported by <chart>` message, which the job accepts; any other failure fails the job |

**Unit test suites (step 9):** the shared templates are tested once, in the
operator-library testbed (`operators/shared/helm/operator-library-testbed/tests/`);
each operator chart's own `tests/` suites cover what that chart adds.

| Chart | Test File | Key Assertions |
| --- | --- | --- |
| testbed | `deployment_test.yaml` | Image, replicas, resources, securityContext, probes, args, `extraArgs`/`extraEnv`, conditional webhook volume mount |
| testbed | `networkpolicy_test.yaml`, `certificate_test.yaml`, `service_test.yaml`, `serviceaccount_test.yaml`, `clusterrolebinding_test.yaml`, `rolebinding_test.yaml`, `pdb_test.yaml`, `servicemonitor_test.yaml`, `release_namespace_test.yaml` | The shared manifests, their conditionals and the release-namespace threading |
| testbed | `clusterrole_test.yaml`, `role_test.yaml` | The shared RBAC templates: rendering per scope, the webhook guard, the hook-less default |
| testbed | `schema_validation_test.yaml` | The shared values schema: type, enum, range, quantity and conditional constraints |
| operator | `clusterrole_test.yaml`, `role_test.yaml` | Wiring of the generated rules, the grants whose restriction is deliberate, a chart's refusal of namespace-scoped mode |
| operator | `deployment_test.yaml` | The chart's image default and what the chart adds through its hooks (keystone federation flag, c5c3 barbican-operator identity) |
| operator | `webhook_test.yaml` | Mutating/Validating configs for the chart's CR kinds when enabled, absent when disabled |
| operator | `schema_validation_test.yaml` | The chart ships an enforced schema; the keys its `values.schema.extras.json` adds |

Timeout: 15 minutes.

### e2e-infra

End-to-end infrastructure deployment and Chainsaw test. Deploys the full
infrastructure stack (Flux, cert-manager, MariaDB, ESO, OpenBao) to a kind cluster and
validates health of all operators, CRs, and ExternalSecrets.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.e2e-infra == 'true'`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`cobaltcore`) at `KIND_VERSION` |
| 4 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack |
| 5 | `chainsaw test` | Runs E2E tests from `tests/e2e/infrastructure/` |
| 6 | `make deploy-infra` (re-run) | Unchanged-parameter re-run (no `SKIP_KIND_CREATE`) — exercises the script's existing-cluster detection |
| 7 | `chainsaw test --report-name chainsaw-report-rerun` | Re-runs the full infrastructure suite to prove the healthy stack is left unchanged |
| 8 | `make deploy-infra` with `WITH_METRICS_SERVER=true` | Additive re-run — the script's Phase-3 wait gates the new metrics-server HelmRelease on Ready |
| 9 | `chainsaw test --report-name chainsaw-report-additive` | Scoped run over infra-stack-health, garage-health, flux-web-health, no-prometheus-when-disabled, and openbao-instance; the metrics-server absence suite is deliberately excluded |
| 10 | `hack/ci-dump-diagnostics.sh` (on failure) | Dumps HelmReleases, pods, events, Flux logs |
| 11 | Upload JUnit report | Uploads test results as artifact (14-day retention) |

Timeout: 45 minutes.

The two re-run legs (steps 6–9) lock the `make deploy-infra` idempotency
contract: the unchanged-parameter re-run must converge against the provisioned
cluster, and the additive `WITH_METRICS_SERVER=true` re-run must install only
the newly enabled component while leaving the base stack untouched.

### build-e2e-images

Centralised image build for E2E test jobs. Builds the images whose sources the pull
request changed, pushes them to GHCR under run-scoped tags
(`e2e-${run_id}-<orig_tag>`), and resolves the rest to the digests `main` published.
The `e2e-operator`, `e2e-chaos`, and `tempest` jobs `docker pull` from GHCR via the
`load-e2e-images` composite action instead of rebuilding.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, verify-invalid-cr-fixtures, chainsaw-lint]`

**Condition:** Runs only when `build-e2e-images == 'true'` and no gate job
failed. The resolver sets that flag when any job downstream of this one is
scheduled, so the four-way OR the job used to carry lives in one place. Uses `always()` so the job runs when upstream Go jobs are
skipped (e.g. pure E2E test-definition PRs where `go=false`). Skipped on PRs from
forks (the workflow's `GITHUB_TOKEN` is read-only on `packages:` for forked
`pull_request` events, so GHCR push would fail) — see `github.event.pull_request.head.repo.fork`
guard.

**Permissions:** `contents: read`, `packages: write` (required for GHCR push).

**Outputs:** `image-map`, the JSON object every consumer resolves its images through.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `docker/setup-buildx-action@v4` | Sets up BuildKit for `type=gha` cache support |
| 3 | `docker/login-action@v4` | Authenticates to GHCR with `GITHUB_TOKEN` |
| 4 | Resolve images | Runs `hack/ci-resolve-e2e-images.sh` with `changed-operators`, `changed-services`, `changed-tempest` and `changed-proxy`; writes the `BUILD_*` variables and the `image-map` output |
| 5 | Build base images | Builds `python-base` and `venv-builder`, only when `NEEDS_BASE_IMAGES` is true |
| 6 | Build federation proxy image | Builds `<IMAGE_PREFIX>/keystone-federation-proxy:dev`, only when `BUILD_PROXY` is true |
| 7 | Build operator images | Builds `<IMAGE_PREFIX>/<op>-operator:dev` for each name in `BUILD_OPERATORS` |
| 8 | Build service images | Builds `<IMAGE_PREFIX>/<svc>:<release>` for each pair in `BUILD_SERVICE_IMAGES`; passes `GITHUB_TOKEN` so the source clones from `github.com` are authenticated |
| 9 | Build OVN image | Builds `<IMAGE_PREFIX>/ovn:<version>` from `images/ovn/Dockerfile`, with the version resolved by `hack/ci-resolve-ovn-version.sh`; passes `GITHUB_TOKEN` as the `github_token` BuildKit secret for the fetches inside the build |
| 10 | Build Tempest images | Builds `<IMAGE_PREFIX>/tempest:<release>` for each release in `BUILD_TEMPEST_RELEASES` |
| 11 | Push E2E images to GHCR | For each image built above, `docker tag` to `<repo>:e2e-${run_id}-<orig_tag>` and `docker push` |

`hack/ci-resolve-e2e-images.sh` derives the image set from the tree: an operator image
per `operators/<op>/` with a `go.mod`, a service image per (operator, release) pair in
`releases/*/source-refs.yaml`, a Tempest image per `releases/<release>/`, and the
federation proxy. An image whose sources this pull request changed is built here and
mapped to its run-scoped tag. Every other image is mapped to the index digest behind
its published tag (`<op>-operator:latest`, `<svc>:<release>`, `tempest:<release>`,
`keystone-federation-proxy:latest`), which the run pulls instead of rebuilding.
A source that has never been published, the state of a new operator before its first
merge, is built instead of failing the run.

The OVN daemon image is the one image outside that set: it carries no OpenStack
release and no `go.mod`, so nothing derives it from the tree. Its own step builds it
on every run of this job and pushes it under the run-scoped tag, and the consumers
reach it through `load-e2e-images`, which falls back to that tag for a reference the
map does not carry.

Only the service and Tempest images build `FROM python-base` and `venv-builder`, so a
run that reuses both skips the base-image step. A reused image is never tagged in the
registry: the consumers pull it by digest, which is what keeps the run-scoped tag off
the published version that `cleanup-e2e-tags` would then refuse to prune.
`tests/unit/hack/ci_resolve_e2e_images_test.sh` pins the decisions against a stubbed
registry.

GH-310 replaced the previous `docker save | zstd | upload-artifact` transport with
GHCR push/pull because the 355 MB single-blob artifact intermittently timed out at
the 5-minute `actions/download-artifact` window. Layer-level pull retries plus the
GHCR CDN dramatically reduce the failure rate.

Timeout: 120 minutes. The job needed 45 before the OVN build joined it, and an
uncached OVN build compiles Open vSwitch and OVN from source, which is what the
`build-ovn` job of `build-images.yaml` budgets 60 minutes for. The GHA cache
does not shorten the first run of a pull request: this job writes the scope
`e2e-ovn`, which nothing on the default branch populates, and Actions caches are
isolated per PR ref. Every E2E job gates on `build-e2e-images` succeeding, so a
job killed at its timeout costs the pull request its whole E2E coverage.

### e2e-operator

End-to-end operator test using kind cluster and Chainsaw.
Pulls pre-built images from GHCR via the `load-e2e-images`
composite action, deploys the infrastructure stack and operator via Helm, and runs
Chainsaw E2E test suites.

**Dependencies:** `needs: [changes, build-e2e-images]`
**Condition:** Runs only when `has-e2e-operators == 'true'` and `build-e2e-images` succeeded.
**Permissions:** `contents: read`, `packages: read` (required for GHCR pull).

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`cobaltcore`) at `KIND_VERSION` |
| 4 | `load-e2e-images` composite action | Pulls run-scoped GHCR tags and re-tags to canonical local refs |
| 5 | `kind load docker-image` | Loads operator, 2025.2 service, 2025.2-upgraded, and 2026.1 service images into kind, plus `ovn:<pin>` on the `ovn` and `neutron` legs |
| 6 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack; the `ovn` and `neutron` legs pass `WITH_OVN_KERNEL_MODULES: true` |
| 7 | `hack/ci-deploy-operator.sh` (`neutron` leg) | Deploys the ovn-operator into `ovn-system` |
| 8 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys operator via Helm |
| 9 | `chainsaw test` | Runs E2E tests from `tests/e2e/<operator>/` |
| 10 | `hack/ci-dump-diagnostics.sh` (always) | Dumps operator pods, all pods, events, operator logs |
| 11 | `hack/ci-dump-diagnostics.sh` (always, `neutron` leg) | Same dump for `ovn-system` |
| 12 | Upload JUnit report | Uploads test results as artifact (14-day retention) |

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix: ${{ fromJson(needs.changes.outputs.e2e-operators) }}
```

The operator matrix is dynamically constructed by the `changes` job, including only operators
whose code (or shared code) changed. The `imagePullPolicy: Never` Helm value ensures the
kind-loaded image is used instead of attempting a registry pull. Timeout: 68 minutes.

**The two OVN legs.** `ovn` ships no per-release service image. Its Pods all
run `ghcr.io/c5c3/ovn:<pin>`, where `<pin>` is what
`hack/ci-resolve-ovn-version.sh` reads out of `images/ovn/Dockerfile`. The
`Resolve E2E images` step appends that ref and `Load images into kind` loads it.

The `neutron` leg loads the same image plus `ovn-operator:dev`, and deploys the
ovn-operator into `ovn-system` ahead of the neutron-operator. A Neutron stays
out of `Ready` until an OVNCentral publishes its Northbound and Southbound
addresses, and a NeutronMetadataAgent resolves an OVNChassis. That second
operator gets a diagnostics dump of its own with `OPERATOR: ovn`, since the
first dump derives its Namespace from the matrix operator and never looks at
`ovn-system`.

Both legs set `WITH_OVN_KERNEL_MODULES: true` on the `setup-e2e-infra` step,
which loads `openvswitch` and `geneve` on the kind node for the chassis
DaemonSets. See
[Kernel-module-dependent suites](#kernel-module-dependent-suites).

### e2e-operator-upgrade

Operator helm-upgrade-in-place E2E. Installs the last released keystone-operator
chart+image from GHCR as the baseline, brings a Keystone CR to Ready, then
`helm upgrade`s the release to the locally built chart+CRDs and asserts the
deployed Keystone survives the operator upgrade (Ready persists,
`status.installedRelease` unchanged, no re-bootstrap). See
[Operator Upgrade E2E Tests](../testing/operator-upgrade-e2e-tests.md) for suite
details.

**Dependencies:** `needs: [changes, build-e2e-images]`
**Condition:** Runs on `pull_request` when `e2e-operator-upgrade == 'true'`,
`build-e2e-images` succeeded, and no dependency failed or was cancelled. The
resolver sets that flag from the suite's own tree or a keystone code change.
**Permissions:** `contents: read`, `packages: read` (required for GHCR pull).

Unlike the per-CR `e2e-operator` matrix, this suite manages the operator Helm
release itself, so it runs in its own single job. The job pulls the run-scoped
`:dev` operator and `2025.2` service images, `helm registry login`s GHCR,
fetches the released baseline via `hack/ci-fetch-released-operator.sh`, installs
it via `hack/ci-deploy-operator.sh` (with `CHART_DIR` pointing at the pulled
chart and `IMAGE_TAG=latest`), deploys the infra stack, and runs the suite from
`tests/e2e-operator-upgrade/`. Blocking (no `continue-on-error`). Timeout: 68
minutes.

### e2e-chaos

End-to-end chaos tests using kind cluster, Chaos Mesh, and Chainsaw. Pulls the
operator and service images its leg needs from GHCR via the `load-e2e-images`
composite action, deploys them alongside Chaos Mesh infrastructure, and runs the
chaos test suites (MariaDB pod kill, Memcached pod kill, OpenBao pod kill,
MariaDB network partition, MariaDB network latency, the two Neutron outage
suites, OVN Southbound outage). See
[Chaos E2E Test Suites](../testing/chaos-e2e-tests.md) for test suite details.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images, e2e-operator]`
**Condition:** Runs only when `e2e-chaos == 'true'`, `build-e2e-images` succeeded, and no dependency failed or was cancelled. The resolver sets that flag from the `tests_chaos` class or the `ci:chaos` label, so the job's own condition no longer reads the label set.
**Permissions:** `contents: read`, `packages: read` (required for GHCR pull).

The `e2e-chaos` job depends on the standard gate jobs plus `e2e-operator`, so chaos
tests run after the happy-path operator E2E suite has passed. Gating is set per
matrix leg via `continue-on-error: ${{ matrix.suite == 'network' || matrix.suite
== 'ovn' }}`: the `pod` leg is **blocking**, so an operator-restart, PDB, or
rotation regression fails the build. The `network` and `ovn` legs stay
**non-blocking**. The expression names those two rather than negating `pod`, so
a leg added to the matrix later gates merges until it is argued out of that.
The `network`
leg's `ip_set`/`sch_netem` kernel-module dependency keeps it prone to
environment flakiness, and the `ovn` leg builds an OVN datapath out of the
host's `openvswitch` and `geneve` modules, which places it under
[Kernel-module-dependent suites](#kernel-module-dependent-suites). On-demand
pre-validation of any leg is available via the `ci:chaos` PR label, with
`run-chaos` kept as an alias.

The job runs as a three-entry matrix, split by chaos type and by the stack each
leg needs:

| Leg | Runner | Operators deployed | Suites |
| --- | --- | --- | --- |
| `pod` | `blacksmith-4vcpu-ubuntu-2404` | keystone, horizon, glance, placement, barbican | the PodChaos suites |
| `network` | `self-hosted` | keystone, horizon, glance, barbican, ovn, neutron | the NetworkChaos suites, `neutron-mariadb-outage` and `neutron-broker-outage` among them |
| `ovn` | `self-hosted` | ovn | `ovn-southbound-outage` |

The `pod` leg is pinned to the `blacksmith-4vcpu-ubuntu-2404` runner for now,
because it is the blocking leg and has not been stable on the self-hosted
runners. The split keeps the legs independently gated and lets them run in
parallel. Each matrix entry lists its per-suite test directories explicitly.

A `Resolve OVN version` step reads the pin from `images/ovn/Dockerfile` through
`hack/ci-resolve-ovn-version.sh` and writes `OVN_VERSION` into `$GITHUB_ENV`, so
the tag itself never appears in the workflow. The `network` and `ovn` legs load
`ovn-operator:dev` and `ovn:$OVN_VERSION` into kind, the `network` leg adds
`neutron-operator:dev` and `neutron:2025.2`, and the `ovn` leg loads none of the
keystone stack. `Setup E2E infrastructure` passes
`WITH_OVN_KERNEL_MODULES: ${{ matrix.suite == 'ovn' && 'true' || '' }}`, which is
what makes `hack/deploy-infra.sh` load `openvswitch` and `geneve` for the chassis
DaemonSet. The diagnostics dump follows the leg through
`OPERATOR: ${{ matrix.suite == 'ovn' && 'ovn' || 'keystone' }}`, because the
`ovn` leg has no `keystone-system` namespace to dump.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `helm/kind-action@v1.14.0` | Creates kind cluster (`cobaltcore`) at `KIND_VERSION` |
| 3 | `hack/ci-resolve-ovn-version.sh` | Writes the `images/ovn/Dockerfile` pin to `$GITHUB_ENV` as `OVN_VERSION` |
| 4 | `load-e2e-images` composite action | Pulls the run-scoped GHCR tags this leg needs and re-tags to canonical local refs |
| 5 | `kind load docker-image` | Loads the keystone stack (all but `ovn`), placement (`pod`), the OVN images (all but `pod`), the neutron images (`network`) |
| 6 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack with `WITH_CHAOS_MESH=true`, plus `WITH_OVN_KERNEL_MODULES=true` on the `ovn` leg |
| 7 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys this leg's operators via Helm |
| 8 | `chainsaw test` | Runs chaos E2E tests from `tests/e2e-chaos/` with `tests/e2e-chaos/chainsaw-config.yaml` |
| 9 | `hack/ci-dump-diagnostics.sh` (always) | Dumps operator pods, all pods, events, operator logs with `OPERATOR=keystone`, or `OPERATOR=ovn` on the `ovn` leg |
| 10 | Upload JUnit report | Uploads `_output/reports/` as `e2e-chaos-junit-report-<suite>` artifact (14-day retention) |

**Key differences from `e2e-operator`:**

| Aspect | `e2e-operator` | `e2e-chaos` |
| --- | --- | --- |
| Matrix | Dynamic per-operator | Three suites (`pod` / `network` / `ovn`) on different runners |
| Test config | `tests/e2e/chainsaw-config.yaml` | `tests/e2e-chaos/chainsaw-config.yaml` |
| Test directory | `tests/e2e/<operator>/` | per-suite `test_dirs` under `tests/e2e-chaos/` |
| Timeout | 68 minutes | 90 minutes |
| Blocking | Yes | `pod` leg blocking; `network` and `ovn` legs non-blocking (`continue-on-error: ${{ matrix.suite != 'pod' }}`) |
| Dependencies | Gate jobs | Gate jobs + `e2e-operator` |
| Service images | 2025.2 + 2025.2-upgraded + 2026.1 | 2025.2 only, plus the pinned OVN daemon image |

The chaos test Chainsaw config uses `parallel: 1` (serial execution) because chaos tests
mutate shared infrastructure pod availability. The assert timeout is 300s (vs 120s for
happy-path tests) to allow multiple reconciliation cycles and pod restart time during
fault recovery.

**Path filter:** `tests/e2e-chaos/**`, `hack/**`, `deploy/**`, `.github/workflows/ci.yaml`, `.github/actions/**`
(separate from `e2e_infra` to allow independent gating). Additionally, any Go code change
— operator-specific (e.g., `operators/keystone/**/*.go`) or shared (`internal/common/**/*.go`
via `go_common`) — triggers the job via `go_changed` in `ci-resolve-changes.sh`, since chaos
tests validate operator resilience against the current codebase.

### Kernel-module-dependent suites

A suite whose workload needs host kernel modules enters CI non-blocking: it
runs on the `self-hosted` runners with `continue-on-error: true`. A separate,
later change flips it to blocking, once its pass history on `main` justifies
the flip. The `network` leg of `e2e-chaos` is the precedent, and it is still on
the non-blocking side of that path.

**Blocking baseline.** The suite that earns the flip first is the single-node
one on `hack/kind-config.yaml`: the DaemonSet renders, its pods start, and the
chassis registers itself in the OVN southbound database. Neither assertion
needs a second schedulable node.

**Multi-node legs.** A suite that does need one uses
`hack/kind-config-multinode.yaml` (one control-plane node plus two workers) in
a job of its own, never on a runner that also holds a `hack/kind-config.yaml`
cluster. Both configs bind the same host ports, so the second cluster fails to
create. The `e2e-multicluster` job sidesteps the same collision by creating its
management cluster with no kind config.

The path the file takes into the job is the `config:` input of the
`helm/kind-action` step, not the `KIND_CONFIG` variable: `setup-e2e-infra` runs
with `SKIP_KIND_CREATE: "true"`, so the cluster already exists by the time
`hack/deploy-infra.sh` sees the variable, and it warns that the value is being
ignored rather than recreating anything.

The modules come from `WITH_OVN_KERNEL_MODULES=true` in the `setup-e2e-infra`
step's `env`, which threads through to `hack/deploy-infra.sh` and runs
`modprobe` for `openvswitch` and `geneve` before the cluster is created. The
load is best-effort, through the helper that also loads the chaos suite's
`ip_set` and `sch_netem` modules. On a host without root or passwordless sudo
it logs a warning and continues, and the suite then fails on its own
assertions.

**Where each OVN suite sits (author's decision, 2026-08-27).** The single-node
chassis baseline (`tests/e2e/ovn/chassis-single-node`) and the Neutron
metadata-agent suite (`tests/e2e/neutron/metadata-agent`) run blocking, inside
the `e2e-operator` legs for `ovn` and `neutron`. Both assert what one node can
answer. The `e2e-ovn-overlay` job takes the non-blocking entry described above,
and so does the `ovn` leg of `e2e-chaos`.

### e2e-ovn-overlay

Runs the `tests/e2e-ovn-overlay/geneve-datapath/` Chainsaw suite on a two-worker
kind cluster: an `OVNCentral`, an `OVNChassis` selecting both workers, one
logical switch port per worker, and a ping between two network namespaces that
can only reach each other across the Geneve tunnel. The suite asserts
`numberReady: 2` on the chassis, two `status.nodes` entries carrying two
different system-ids, a `Port_Binding` for `lsp-a` bound to a chassis, and two
Geneve `Encap` rows on two addresses. A single node answers none of those
questions, so the suite lives outside `tests/e2e/` and gets a job of its own.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]`
**Condition:** Runs only when `e2e-ovn-overlay == 'true'`, the upstream
`build-e2e-images` job succeeded, and no dependency failed or was cancelled.

The job runs on `self-hosted` with `continue-on-error: true` under the
kernel-module rule above, and creates its cluster from
`config: hack/kind-config-multinode.yaml`. `setup-e2e-infra` receives
`WITH_OVN_KERNEL_MODULES: "true"`, `hack/ci-deploy-operator.sh` installs the
ovn-operator into `ovn-system`, and the suite runs through
`make e2e-ovn-overlay`, the same target a developer calls locally. The OVN
daemon image is loaded at the tag `hack/ci-resolve-ovn-version.sh` reads from
`images/ovn/Dockerfile`, so the workflow spells no version of its own.
Diagnostics run with `OPERATOR: ovn`, and `_output/reports/` is uploaded as the
`e2e-ovn-overlay-junit-report` artifact (14-day retention). `cleanup-e2e-tags`
lists the job in its `needs`, so the run-scoped image tags survive until it
finishes.

**Path filter:** `tests/e2e-ovn-overlay/**`, `operators/ovn/**`, `images/ovn/**`,
`hack/**`, `deploy/**`, `.github/actions/**`, `.github/workflows/ci.yaml`. Any Go
code change and any E2E test-definition change also force the job on, through
`go_changed` and `any_e2e_tests` in `ci-resolve-changes.sh`.

### e2e-prometheus

End-to-end kube-prometheus-stack tests using kind cluster, Flux-managed
`kube-prometheus-stack` HelmRelease, and Chainsaw. Builds the
keystone operator image, deploys it alongside the monitoring stack, and runs
the prometheus suite under `tests/e2e/keystone/prometheus-stack/` to verify
HelmRelease readiness, ServiceMonitor presence, and live Prometheus scraping
of the operator metrics endpoint.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]`
**Condition:** Runs only when `e2e-prometheus == 'true'`, the upstream
`build-e2e-images` job succeeded, and no dependency failed or was cancelled.

The `setup-e2e-infra` composite action is invoked with `WITH_PROMETHEUS: "true"`
in its step `env`, which threads through to `hack/deploy-infra.sh` and gates the
`kube-prometheus-stack` overlay (`deploy/kind/prometheus/`) plus the
post-deploy `enable_operator_servicemonitor` patch (applied to both the
keystone-operator and horizon-operator HelmReleases). The Deploy
operator step runs `hack/ci-deploy-operator.sh` with `WITH_PROMETHEUS: "true"`
in its step `env`, which adds `--set monitoring.serviceMonitor.enabled=true`
to the Helm install command — without this flag the chart's gated
`ServiceMonitor` template renders nothing and the chainsaw step
`servicemonitor-exists` (and the dependent `prometheus-target-up`) cannot
pass. The kind base kustomization keeps the keystone-operator HelmRelease
suspended, so the runtime `kubectl patch` cannot reactively enable the
ServiceMonitor — the install-time flag is the single source of truth.

Unlike `e2e-chaos`, `e2e-prometheus` runs with `continue-on-error: false`:
the kube-prometheus stack is deterministic on kind, so any failure is a
genuine regression of the kind-only Quick Start observability story.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `helm/kind-action@v1.14.0` | Creates kind cluster (`cobaltcore`) at `KIND_VERSION` |
| 3 | `load-e2e-images` composite | Restores prebuilt operator and service images from the build-e2e-images artifact |
| 4 | `kind load docker-image` | Loads operator and service images into kind |
| 5 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack with `WITH_PROMETHEUS: "true"` |
| 6 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys keystone operator via Helm with `WITH_PROMETHEUS: "true"` (gates `--set monitoring.serviceMonitor.enabled=true`) |
| 7 | `chainsaw test` | Runs the prometheus E2E suite from `tests/e2e/keystone/prometheus-stack/` |
| 8 | `hack/ci-dump-diagnostics.sh` (always) | Dumps operator pods, all pods, events, operator logs with `OPERATOR=keystone` |
| 9 | Upload JUnit report | Uploads `_output/reports/` as `e2e-prometheus-junit-report` artifact (14-day retention) |

**Path filter:** `deploy/kind/prometheus/**`, `tests/e2e/keystone/prometheus-stack/**`,
`hack/**`, `deploy/**`, `.github/workflows/ci.yaml`, `.github/actions/**`. As
with `e2e-chaos`, any Go code change (`go_changed`) or any E2E test change
(`any_e2e_tests`) also triggers the job via `ci-resolve-changes.sh`, since
the prometheus suite scrapes live operator metrics.

### e2e-controlplane

Runs the full c5c3 `ControlPlane` → Keystone chain on kind. It deploys the
`keystone`, `horizon`, `glance`, `placement`, `barbican`, `ovn`, and `neutron`
operators plus K-ORC and `c5c3-operator` as local dev images (rather than the
GHCR-published Flux chart) and runs the
`tests/e2e/c5c3/full-controlplane-keystone/` Chainsaw suite, which asserts the
whole orchestration link by link: managed MariaDB/Memcached provisioning, the
projected Keystone CR, the minted restricted K-ORC application credential, the
OpenBao → ESO credential round-trip, the identity catalog, and finally a live
`openstack token issue` / `catalog list` against the Keystone `/v3` endpoint. The
suite applies a standalone `OVNCentral` of its own beside the ControlPlane, since
the plane only references a central and never projects one, and asserts the
network service on top: `OVNReady` mirroring that central and `NeutronReady` over
the projected `Neutron` child.

A second chainsaw step on the same cluster runs
`tests/e2e/c5c3/keystone-service-foreign-namespace/`, the cross-namespace
`KeystoneService` suite: it brings up a Keystone-only ControlPlane of its own,
seeds that plane's per-CR OpenBao paths itself, and proves an allowlisted
namespace can register and authenticate while an unlisted one is refused. The two
suites are separate steps because the shared chainsaw config sets `failFast`, so
one invocation over both directories would let either abort the other; the second
step writes its JUnit XML under a distinct report name so each survives in the
uploaded artifact.

A third chainsaw step runs `tests/e2e/c5c3/keystone-service/`, the own-namespace
`KeystoneService` suite: it brings up a Keystone-only ControlPlane of its own and
seeds that plane's OpenBao paths itself, then proves the round-trip through the
materialised `clouds.yaml`, the rotation driven by a `CredentialRotation`, a
collision held at `ServiceCollision` / `ServiceAccountCollision` until
`catalog.adopt` takes the row over, and residue-free deletion. It is a separate
step for the same `failFast` reason and writes its JUnit XML as
`chainsaw-report-keystone-service-own-namespace`, so the uploaded artifact
carries three report files.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]`
**Condition:** Runs only when `e2e-controlplane == 'true'`, the upstream
`build-e2e-images` job succeeded, and no dependency failed or was cancelled.

`setup-e2e-infra` is invoked with `WITH_CONTROLPLANE: "true"`,
`CONTROLPLANE_OPERATORS: external`, and
`CONTROLPLANE_NAME: controlplane-keystone`. Under `CONTROLPLANE_OPERATORS=external`
`hack/deploy-infra.sh` prepares only the shared prerequisites (TLS issuers,
OpenBao with per-CR admin-password seeding, the ESO ClusterSecretStore) and
suspends the Flux ControlPlane stack, so the dev-image operators deployed by the
subsequent steps own the reconcile. K-ORC is applied by `hack/ci-deploy-korc.sh`
at the tag pinned in `deploy/flux-system/sources/k-orc.yaml`.

The suite runs with `E2E_REQUIRE_CONTROLPLANE_STACK: "true"`, which flips its
presence guard from a silent SKIP to a hard failure — so a broken operator/CRD
deployment fails the build instead of going green. Like `e2e-prometheus`, the
job runs with `continue-on-error: false`, and it uses a 195-minute timeout on the
larger runner because a real MariaDB + Memcached + Keystone + eight operators +
OpenBao + ESO + K-ORC on one node is resource-heavy, and its three chainsaw
suites run in sequence on that one node, so their budgets add up rather than
overlap. A suite's ceiling is not its `exec` budget alone: chainsaw applies that
budget to every script operation, so `try`, `catch` and `finally` each get one,
and the `cleanup` budget runs after all three. Each of the three suites
therefore pins its `catch` and `finally` timeouts explicitly, which puts the
ceilings at 43, 80 and 90 minutes. The job wall has to outlast the bring-up plus
the suites that pass plus the full ceiling of the one that stalls, or a stalled
suite is killed before its own timeout fires and reports as a cancelled job with
no JUnit XML.

### e2e-controlplane-sso

Runs the `tests/e2e-controlplane-sso/` Chainsaw suite: the end-user SSO
experience — the Horizon websso projection, the login page's SSO choice and
domain dropdown, the websso round trip through the gateway, and LDAP-domain
login. The suite lives outside `tests/e2e/` so the per-CR `e2e-operator` matrix
leg, which runs `tests/e2e/<operator>/` wholesale, does not sweep it up.

**A sibling job rather than a second suite directory on `e2e-controlplane`.**
The ControlPlane webhook permits one ControlPlane per namespace, and
`openstack-gw` sets `allowedRoutes.namespaces.from: Same` (the operators
deliberately do not manage `ReferenceGrant`), so the two suites can share
neither the `openstack` namespace nor the Gateway. Each therefore needs its own
kind cluster.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]`
**Condition:** Runs only when `e2e-controlplane == 'true'`, the upstream
`build-e2e-images` job succeeded, and no dependency failed or was cancelled.

It mirrors `e2e-controlplane`'s setup with `CONTROLPLANE_NAME: controlplane-sso`
(so the OpenBao bootstrap seeds the per-CR admin-password and Horizon
`SECRET_KEY` paths the chain reads) and additionally loads
`keystone-federation-proxy:dev` into kind, because the suite's ControlPlane CR
pins `services.keystone.federationProxyImage.tag: dev`. Without that override
the suite would validate the sidecar already published on `main` rather than the
one under review — which is why the `e2e_controlplane` path filter also watches
`images/keystone-federation-proxy/**`.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `helm/kind-action@v1.14.0` | Creates kind cluster (`cobaltcore`) at `KIND_VERSION` |
| 3 | `load-e2e-images` composite | Restores `keystone-operator:dev`, `c5c3-operator:dev`, `keystone:2025.2`, `tempest:2025.2` from GHCR |
| 4 | `kind load docker-image` | Loads the four images into kind |
| 5 | `setup-e2e-infra` composite action | Deploys infra with `WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=external CONTROLPLANE_NAME=controlplane-keystone` |
| 6 | `hack/ci-deploy-korc.sh` | Applies K-ORC CRDs + controller at the pinned commit; runs with `GITHUB_TOKEN` so the clone from `github.com` is authenticated (see [hack/ci-build-service-image.sh](#hackci-build-service-imagesh) for why) |
| 7 | `hack/ci-deploy-operator.sh` (keystone) | Deploys the keystone-operator dev image into `keystone-system` |
| 8 | `hack/ci-deploy-operator.sh` (c5c3) | Deploys the c5c3-operator dev image into `c5c3-system` |
| 9 | `chainsaw test` | Runs the full-chain suite with `E2E_REQUIRE_CONTROLPLANE_STACK=true` |
| 10 | `hack/ci-dump-diagnostics.sh` (always) | Dumps diagnostics with `OPERATOR=c5c3` |
| 11 | Upload JUnit report | Uploads `_output/reports/` as `e2e-controlplane-junit-report` (14-day retention) |

**Path filter:** `operators/c5c3/**`, `operators/keystone/**`, `tests/e2e/c5c3/**`,
`deploy/**`, `hack/**`, `.github/actions/**`, `.github/workflows/ci.yaml`. As with
`e2e-prometheus`, any Go code change (`go_changed`) or any E2E test change
(`any_e2e_tests`) also triggers the job via `ci-resolve-changes.sh`. The job pulls
`c5c3-operator:dev` from the image map: built in this run when `operators/c5c3/**`
changed, and otherwise the digest behind `ghcr.io/c5c3/c5c3-operator:latest`, so both
dev images exist even for a full-chain-test-only change.

### e2e-external-keystone

Runs the `tests/e2e/c5c3/external-keystone/` Chainsaw suite: an External-mode
`ControlPlane` driven against a plain Keystone the operator does **not** own. The
suite brings up its own operator-free, SQLite-backed Keystone fixture in a
separate namespace, then drives four External `ControlPlane`s against it and
asserts the whole adoption contract — convergence with zero
MariaDB/Memcached/Keystone children, admin and catalog imports, the application
credential minted against the external API and round-tripped through OpenBao, no
catalog pollution (compared against a pre-recorded inventory), service-account
usability and rotation, out-of-band password rotation and drift detection, the
`endpoint_type` failure detection, the wrong-password and ambiguous-catalog
negative paths, and zero-blast-radius deletion.

**A sibling job rather than a second suite directory on `e2e-controlplane`.** The
ControlPlane webhook permits one ControlPlane per namespace, and this suite
stands up a wholly different infrastructure shape (a plain, operator-free
Keystone fixture plus four External ControlPlanes, none provisioning
MariaDB/Memcached), so it needs its own kind cluster.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]`
**Condition:** Runs only when `e2e-controlplane == 'true'`, the upstream
`build-e2e-images` job succeeded, and no dependency failed or was cancelled.

It mirrors `e2e-controlplane`'s setup with `WITH_CONTROLPLANE: "true"`,
`CONTROLPLANE_OPERATORS: external`, and `WITH_CONTROLPLANE_CR: "false"`, but
leaves `CONTROLPLANE_NAME` at its default: the OpenBao bootstrap only seeds the
managed-mode admin-password path, which the External ControlPlanes — in their own
namespaces, authenticating from user-supplied Secrets — never read, and the suite
asserts their own per-CR OpenBao paths are never-seeded. It loads the
`keystone:2025.2` and `tempest:2025.2` service images (but **not** `horizon:2025.2`
— External mode never runs a Horizon workload; the horizon-operator is deployed
only for its CRD) and runs with `E2E_REQUIRE_CONTROLPLANE_STACK: "true"` so a
broken deployment fails the build instead of the suite skipping.

**Path filter:** shares the `e2e-controlplane` change-detection output, so the
same `e2e_controlplane` filter (`operators/c5c3/**`, `operators/keystone/**`,
`operators/horizon/**`, `tests/e2e/c5c3/**`, `deploy/**`, `hack/**`,
`.github/actions/**`, `.github/workflows/ci.yaml`) triggers it.

### tempest

Tempest API integration tests. Deploys services into a kind
cluster and runs the OpenStack Tempest test suite against them. Uses a release matrix to validate each OpenStack release independently, with per-release Tempest
configuration, Keystone CRs, and K8s service names. Pulls pre-built images from
GHCR (run-scoped tag) via the `load-e2e-images` composite action.

**Dependencies:** `needs: [changes, build-e2e-images, e2e-infra, e2e-operator, e2e-chaos, e2e-prometheus]`
**Condition:** Runs only when `tempest == 'true'`, `build-e2e-images` succeeded, and no other E2E job failed or was cancelled; tempest is the last E2E job in the chain. Without a label it runs for a change to its own sources, and the matrix is narrowed to the services whose configuration changed.
**Permissions:** `contents: read`, `packages: read` (required for GHCR pull).

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix:
    include:
      - release: "2025.2"
        config-dir: tests/tempest/keystone
        cr-name: keystone-tempest
        service-k8s-name: keystone-tempest-api
      - release: "2026.1"
        config-dir: tests/tempest/keystone-2026-1
        cr-name: keystone-tempest-2026-1
        service-k8s-name: keystone-tempest-2026-1-api
```

Each matrix entry specifies: the release version, the Tempest configuration directory,
the Keystone CR name, and the K8s service name used for port-forwarding. Steps reference
these via `matrix.release`, `matrix.config-dir`, `matrix.cr-name`, and
`matrix.service-k8s-name`.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`cobaltcore`) at `KIND_VERSION` |
| 4 | Resolve OVN version | `hack/ci-resolve-ovn-version.sh` writes `OVN_VERSION` to `$GITHUB_ENV`; `images/ovn/Dockerfile` holds the pin |
| 5 | `load-e2e-images` composite action | Pulls run-scoped GHCR tags and re-tags to canonical local refs; the neutron leg also pulls `neutron-operator:dev`, `neutron:<release>`, `ovn-operator:dev` and `ovn:<OVN_VERSION>` |
| 6 | `kind load docker-image` | Loads keystone operator and service images into kind |
| 7 | `kind load docker-image` *(neutron leg only)* | Loads the two operator images, the neutron and OVN service images, and the tempest image the catalog Job runs in-cluster |
| 8 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack |
| 9 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys operator via Helm |
| 10 | `hack/ci-deploy-operator.sh` ×2 *(neutron leg only)* | Deploys the ovn-operator into `ovn-system` and the neutron-operator into `neutron-system`; a Neutron never reaches Ready without a live OVNCentral |
| 11 | Deploy Keystone CR | Applies `matrix.config-dir/00-keystone-cr.yaml` and waits for `matrix.cr-name` Ready |
| 12 | Bootstrap network catalog *(neutron leg only)* | Applies `matrix.config-dir/01-catalog-setup-job.yaml` and waits 300 s for the `neutron-tempest-catalog-setup` Job to complete |
| 13 | Deploy OVNCentral *(neutron leg only)* | Applies `02-messaging-secret.yaml` and `03-ovncentral-cr.yaml`, waits 300 s for `ovncentral/ovn-neutron-tempest-<slug>` Ready |
| 14 | Deploy Neutron CR *(neutron leg only)* | Applies `04-neutron-cr.yaml`, waits 600 s for `matrix.neutron-cr-name` Ready |
| 15 | `hack/ci-run-tempest.sh` | Runs Tempest API tests with `CONFIG_DIR=matrix.config-dir`, `SERVICE_K8S_NAME=matrix.service-k8s-name`, and on the neutron leg `NEUTRON_K8S_NAME=matrix.neutron-cr-name` (empty elsewhere, which disables the 9696 port-forward) |
| 16 | Upload Tempest results | Uploads `_output/tempest/` as `tempest-<release>-results` artifact (14-day retention) |
| 17 | `hack/ci-dump-diagnostics.sh` (always) | Dumps diagnostic info with `OPERATOR=keystone` |

Timeout: 68 minutes.

### cleanup-e2e-tags

GH-310. Prunes the run-scoped GHCR tags pushed by `build-e2e-images`
(`e2e-${run_id}-*`) so they don't accumulate on the package page. Runs as a
matrix over the E2E target packages after every consumer that might still pull
the images has finished. The `always() && needs.build-e2e-images.result ==
'success'` condition means the cleanup runs on success, failure, cancelled, or
skipped consumer outcomes — but only when `build-e2e-images` actually pushed
something.

The package list is the `cleanup-e2e-packages` output of the `changes` job,
derived by `hack/ci-generate-cleanup-matrix.sh` from `images/` and `operators/`.
It was a hardcoded list until `keystone-federation-proxy` was left out of it and
accumulated 352 stale tags. A package whose image this run reused rather than built
carries no run-scoped tag, so the narrowed plan finds no candidate there and deletes
nothing.

Deletion runs through `hack/ghcr-prune-stale-versions.py` in
`--only-tag-pattern` mode, scoped to `^e2e-${run_id}-`. That mode only considers
versions whose tags all match the pattern, so untagged versions are out of scope
by construction — the job runs in parallel with `build-and-push`, which uploads
per-platform manifests untagged via `push-by-digest` and needs those digests
intact for `merge-operator-images` (GH-312).

**Dependencies:** `needs: [changes, build-e2e-images, e2e-operator,
e2e-operator-upgrade, e2e-chaos, e2e-ovn-overlay, tempest]`
**Permissions:** `contents: read`, `packages: write`

The job is `continue-on-error`: pruning is housekeeping, and a package whose only
tagged version is the run-scoped one cannot be pruned at all (GHCR refuses to
delete a package's last version). The nightly `cleanup-e2e-stale-tags` job in
`cleanup-images.yaml` is the safety net for anything a cancelled run leaks.

Timeout: 15 minutes.

### build-and-push

Builds operator container images per platform on native runners and pushes each
single-arch image by digest. Runs only on push events (main branch or
v* tags) — skipped on pull requests. The multi-arch manifest list and final tags are
assembled by the subsequent `merge-operator-images` job.

Publish-only-on-merge: the merged commit's PR was already green, so the E2E suite is
not re-run on push. The job depends only on `changes` (for the operator matrix) and
builds the image from its own cache scope, independent of `build-e2e-images`.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'push' && needs.changes.outputs.has-e2e-operators == 'true'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | Prepare platform pair | Shell | Converts `linux/amd64` → `linux-amd64` for artifact names and cache scopes |
| 3 | `docker/setup-buildx-action@v4` | Sets up Docker Buildx |
| 4 | `docker/login-action@v4` | Authenticates to GHCR (`github.actor` / `GITHUB_TOKEN`) |
| 5 | `docker/metadata-action@v6` | Generates OCI labels (two-layer annotation pattern) |
| 6 | `docker/build-push-action@v7` | Builds single-platform image; `push-by-digest=true`; digest exported as artifact |
| 7 | Export digest | Shell | Writes digest filename to `/tmp/digests/` |
| 8 | Upload digest | `actions/upload-artifact@v7` | Artifact name: `digests-operator-<operator>-<platform-pair>`, retention: 1 day |

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix:
    operator: ${{ fromJson(needs.changes.outputs.e2e-operators).operator }}
    platform: [linux/amd64, linux/arm64]
    include:
      - platform: linux/amd64
        runner: ubuntu-latest
      - platform: linux/arm64
        runner: ubuntu-24.04-arm
```

Build context is the repository root (required by `go.work`), with the Dockerfile at
`operators/<operator>/Dockerfile`. GitHub Actions cache (`type=gha`) is scoped per
platform (`<operator>-operator-linux-amd64` / `<operator>-operator-linux-arm64`).

### merge-operator-images

Downloads per-platform digests from `build-and-push`, assembles the multi-arch manifest
list, and pushes it with the final tags.

**Dependencies:** `needs: [changes, build-and-push]`
**Condition:** `if: github.event_name == 'push' && needs.build-and-push.result == 'success'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `docker/setup-buildx-action@v4` + `docker/login-action@v4` | Authenticates to GHCR |
| 3 | `docker/metadata-action@v6` | Generates final image tags |
| 4 | Download digests | `actions/download-artifact@v8` | Downloads all `digests-operator-<operator>-*` artifacts |
| 5 | Create and push manifest list | Shell | `docker buildx imagetools create` assembles per-platform digests under the final tags from step 3 |

**Matrix strategy:** Same `operator` dimension as `build-and-push` (via `fromJson(needs.changes.outputs.e2e-operators)`).

**Image tagging strategy:**

| Trigger | Tags Applied |
| --- | --- |
| Push to main | `sha-<full-sha>`, `latest` |
| Push v* tag (from main) | `sha-<full-sha>`, `latest`, `<version>` (e.g. `0.1.0`, v prefix stripped) |
| Push v* tag (from non-main) | `sha-<full-sha>`, `<version>` (no `latest` — restricted to default branch) |

Images are published at `ghcr.io/c5c3/<operator>-operator:<tag>`.

### helm-push

Packages and pushes operator Helm charts to the GHCR OCI registry.
Runs only on push events — skipped on pull requests. Like `build-and-push`, this is
publish-only-on-merge: the chart is packaged and pushed for every changed operator on
push without re-running the E2E suite.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'push' && needs.changes.outputs.has-e2e-operators == 'true'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v5` | Installs Helm CLI |
| 3 | Helm registry login | Authenticates to GHCR via `helm registry login` |
| 4 | Package and push | Packages chart and pushes to `oci://ghcr.io/c5c3/charts/` |

**Chart version derivation:**

| Trigger | Version |
| --- | --- |
| Push to main | Default version from `Chart.yaml` |
| Push v* tag | SemVer derived from tag (v prefix stripped, e.g. `v0.1.0` → `0.1.0`) |

**Matrix strategy:** Same `operator` dimension as `build-and-push` (via
`fromJson(needs.changes.outputs.e2e-operators)`), so every changed operator's chart is
packaged and pushed.

The `make helm-package` target packages `operators/<operator>/helm/<operator>-operator/`.
When `CHART_VERSION` is set (for tag pushes), it overrides the version in `Chart.yaml`.

### github-release

Creates a GitHub Release with auto-generated release notes on v* tag pushes.

**Dependencies:** `needs: [changes, merge-operator-images, helm-push]`
**Condition:** `if: startsWith(github.ref, 'refs/tags/v') && needs.merge-operator-images.result == 'success' && needs.helm-push.result == 'success'`
**Permissions:** `contents: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v5` | Installs Helm CLI for chart packaging |
| 3 | Package Helm charts | Packages operator Helm charts with release version |
| 4 | `softprops/action-gh-release@v3` | Creates release with `generate_release_notes: true` and attaches chart tarballs |

This job runs only after both `merge-operator-images` and `helm-push` complete
successfully, ensuring the final multi-arch manifest list and charts are published before
the release is created. Helm chart tarballs
are attached as release assets for direct download. Timeout: 8 minutes.

## Reusable CI Scripts

Repeated inline shell logic from E2E jobs is extracted into standalone scripts under
`hack/`. Each script uses `set -euo pipefail`, includes an SPDX Apache-2.0 header, and
passes shellcheck. All scripts are designed to work both in CI and locally
against any kubeconfig.

### hack/ci-dump-diagnostics.sh

Dumps diagnostic information after E2E failures. Shared across `e2e-infra`, `e2e-operator`,
and `tempest` jobs.

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OPERATOR` | No | (empty) | When set, emits operator-specific diagnostics (pod logs, CR status, job logs) |
| `NAMESPACE` | No | `openstack` | Kubernetes namespace for operator-specific queries |

**Infrastructure diagnostics (always emitted):** HelmReleases, pods, DaemonSets, events
(last 50), and Flux logs across all namespaces.

**Operator diagnostics (when `OPERATOR` is set):** Operator pods and logs, job descriptions
and logs in the target namespace, all pod logs (current and previous) in the namespace,
operator CR status conditions, and ConfigMaps.

Usage:

```bash
hack/ci-dump-diagnostics.sh                    # infra-only diagnostics
OPERATOR=keystone hack/ci-dump-diagnostics.sh   # + operator-specific diagnostics
```

### hack/ci-build-service-image.sh

Builds an OpenStack service container image by resolving upstream source refs, cloning the
project at the pinned ref, applying patches from `patches/<service>/<release>/` (the same
set the Build Images workflow applies), applying constraint overrides, and building the
full image chain (`python-base` -> `venv-builder` -> service image).

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OPERATOR` | Yes | - | OpenStack service name (e.g. `keystone`) |
| `IMAGE_PREFIX` | Yes | - | Container image prefix (e.g. `ghcr.io/c5c3`) |
| `RELEASE` | No | `2025.2` | Release directory name under `releases/` |
| `GITHUB_TOKEN` | No | (unset) | Authenticates the clone from `github.com`; the `Build service images` step passes the workflow token, a run without one clones anonymously |

The script reads `releases/<RELEASE>/source-refs.yaml` for the upstream Git ref and
`releases/<RELEASE>/extra-packages.yaml` for additional pip/apt packages. The final image
is tagged `<IMAGE_PREFIX>/<OPERATOR>:<RELEASE>`.

The source is cloned from the GitHub mirror first, the host the Build Images
workflow checks out, and from `opendev.org` when the mirror does not serve the
ref after three attempts with a linear backoff. Both hosts carry the same
tags. GitHub does not reliably serve unauthenticated `upload-pack` requests to
the git a distribution ships (the self-hosted runners run Ubuntu 24.04's
2.43): since 2026-09-02 the anonymous request can come back as a 401
challenge, which a runner without a terminal reports as `could not read
Username for 'https://github.com'`. With `GITHUB_TOKEN` set the script sends
the token the way `actions/checkout` does, as a basic-auth header scoped to
`https://github.com/`, through git's environment config, so it lands in no
file, no argument list and never reaches `opendev.org`. `GIT_TERMINAL_PROMPT=0`
keeps a rejected clone from waiting for a username.

Usage:

```bash
OPERATOR=keystone IMAGE_PREFIX=ghcr.io/c5c3 hack/ci-build-service-image.sh
```

### hack/ci-deploy-operator.sh

Deploys an operator into a kind cluster by installing CRDs, waiting for establishment, and
deploying the operator via Helm with the specified container image.

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OPERATOR` | Yes | - | Operator name (e.g. `keystone`) |
| `IMAGE_REPO` | Yes | - | Full image repository (e.g. `ghcr.io/c5c3/keystone-operator`) |
| `IMAGE_TAG` | No | `dev` | Image tag |

The script runs `kubectl apply -f <chart>/crds/`, waits for CRD establishment, then runs
`helm install` with `image.pullPolicy=Never` (suitable for kind-loaded images).

Usage:

```bash
OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator hack/ci-deploy-operator.sh
```

### hack/ci-build-tempest-image.sh

Builds the Tempest test container image by resolving Tempest and plugin version refs from
the release config, then running `docker build` with the pinned versions.

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `RELEASE` | No | `2025.2` | Release directory name under `releases/` |
| `TEMPEST_IMAGE` | No | `c5c3/tempest:local` | Target image name:tag |

The script reads `releases/<RELEASE>/test-refs.yaml` to resolve `tempest` and
`keystone-tempest-plugin` versions, then builds `images/tempest/Dockerfile` with the
appropriate build args and `upper-constraints` build context.

Usage:

```bash
hack/ci-build-tempest-image.sh
RELEASE=2025.2 TEMPEST_IMAGE=c5c3/tempest:local hack/ci-build-tempest-image.sh
```

### hack/ci-resolve-ovn-version.sh

Prints the OVN version pinned in `images/ovn/Dockerfile`. It reads the single
`ARG OVN_VERSION=` line and writes the tag without its leading `v` to stdout,
so `v26.03.2` becomes `26.03.2`. The script is the only parser of that line:
the `build-ovn` and `merge-ovn-image` jobs in `build-images.yaml`,
`hack/ci-build-ovn-image.sh` and `tests/container-images/verify_ovn.sh` all
call it.

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OVN_DOCKERFILE` | No | `images/ovn/Dockerfile` | Dockerfile whose `ARG OVN_VERSION` line is parsed |

Three cases make the script print an `::error::` annotation on stderr and exit
1: the Dockerfile does not exist, it holds no `ARG OVN_VERSION=` line, or the
value is not a `vX.Y.Z` tag (a second `ARG OVN_VERSION=` line fails that
anchored match too). Annotations go to stderr, so stdout carries the version
and nothing else.

Usage:

```bash
hack/ci-resolve-ovn-version.sh
OVN_DOCKERFILE=path/to/Dockerfile hack/ci-resolve-ovn-version.sh
```

### hack/ci-build-ovn-image.sh

Builds the OVN container image. It resolves the pin with
`hack/ci-resolve-ovn-version.sh`, then runs `docker build` on
`images/ovn/Dockerfile` with `images/ovn/` as the build context. The Dockerfile
clones OVN at the pinned tag and takes Open vSwitch from that tag's `ovs`
submodule gitlink, so the build needs no host checkout and no `--build-arg`.
`OVN_DOCKERFILE` moves both halves at once — the resolver reads the pin from
that file and `docker build` builds it, with its directory as the context — so
the tag can never claim a version the image was not built from.

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OVN_IMAGE` | No | `c5c3/ovn:<version>` | Target image name:tag |
| `OVN_DOCKERFILE` | No | `images/ovn/Dockerfile` | Dockerfile to resolve the pin from and to build; its directory is the build context |
| `DOCKER_BUILD_CACHE_FROM` | No | (unset) | buildx `--cache-from` spec |
| `DOCKER_BUILD_CACHE_TO` | No | (unset) | buildx `--cache-to` spec |
| `GITHUB_TOKEN` | No | (unset) | Authenticates the source fetches from `github.com` inside the build; mounted as the `github_token` BuildKit secret, never a build-arg, so it reaches no layer. The `Build OVN image` step passes the workflow token |

Usage:

```bash
hack/ci-build-ovn-image.sh
OVN_IMAGE=ghcr.io/c5c3/ovn:$(hack/ci-resolve-ovn-version.sh) hack/ci-build-ovn-image.sh
```

The `build-e2e-images` job wires the script into its `Build OVN image` step.

### hack/ci-run-tempest.sh

CI-specific Tempest execution wrapper that handles port-forwarding, config generation, and
Docker-based test execution. This is the CI counterpart to `hack/run-tempest.sh` (which
handles local execution including image building).

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `SERVICE` | No | `keystone` | Service under test |
| `CONFIG_DIR` | No | `tests/tempest/<SERVICE>` | Directory containing `tempest.conf` template and include/exclude lists |
| `NAMESPACE` | No | `openstack` | Kubernetes namespace |
| `ADMIN_SECRET` | No | `keystone-admin` | Secret name holding admin password |
| `OUTPUT_DIR` | No | `_output/tempest` | Test output directory |
| `TEMPEST_IMAGE` | No | `c5c3/tempest:local` | Tempest container image |
| `SERVICE_K8S_NAME` | No | `<SERVICE>-tempest-api` | K8s Service name for port-forwarding (allows override for release-specific CR names, e.g. `keystone-tempest-2026-1-api`) |

The script:
1. Extracts the admin password from the Kubernetes secret
2. Sets up `kubectl port-forward` to the service and waits for readiness
3. Generates `tempest.conf` from the template, substituting endpoint and credentials
4. Runs Tempest in a Docker container with `--network host` and host-alias DNS entries
5. Converts subunit output to JUnit XML and checks for failures

Usage:

```bash
hack/ci-run-tempest.sh
SERVICE=keystone OUTPUT_DIR=_output/tempest hack/ci-run-tempest.sh
```

## Composite Action: setup-test-deps

`.github/actions/setup-test-deps/action.yaml`

A composite GitHub Action that encapsulates the shared cache + `make install-test-deps`
step used by every job that needs the pinned `chainsaw`/`flux`/`kind`/`kubectl` binaries.
Extracted so the cache key, `restore-keys:`, and `PATH` wiring live in one place:
`setup-e2e-infra` (cluster-bound jobs) and the lightweight `chainsaw-lint` job both
consume this and inherit any future tweaks (key bump, additional pinned tool) for free.

| Step | Description |
| --- | --- |
| 1 | Restores `$HOME/.local/bin` from cache, keyed on the hash of `hack/install-test-deps.sh` (auto-invalidates when any pinned tool version changes) |
| 2 | Runs `make install-test-deps` (no-op on cache hit thanks to the script's skip-if-correct-version logic) and appends `~/.local/bin` to `GITHUB_PATH` |

The action takes no inputs.

## Composite Action: setup-e2e-infra

`.github/actions/setup-e2e-infra/action.yaml`

A composite GitHub Action that encapsulates the shared Flux CLI + test dependencies +
infrastructure deployment sequence used by `e2e-infra`, `e2e-operator`, and `tempest` jobs.
This replaces three duplicated step sequences with a single `uses:` reference.

**Prerequisite:** A kind cluster must already exist (the action sets `SKIP_KIND_CREATE=true`
internally).

| Step | Description |
| --- | --- |
| 1 | Installs Flux CLI via `fluxcd/flux2/action@v2.9.0` (SHA-pinned) |
| 2 | Delegates to the `setup-test-deps` composite action (cache restore + `make install-test-deps` + `PATH` wiring) |
| 3 | Runs `make deploy-infra` with `SKIP_KIND_CREATE=true` |

Usage in a workflow job:

```yaml
- name: Setup E2E infrastructure
  uses: ./.github/actions/setup-e2e-infra
```

The action takes no inputs. All configuration is handled by existing Makefile targets and
environment variables.

## Composite Action: load-e2e-images

`.github/actions/load-e2e-images/action.yaml`

A composite GitHub Action that pulls pre-built E2E images from GHCR and re-tags them
to their canonical local references so downstream `kind load docker-image` calls work
unchanged. Shared between `e2e-operator`, `e2e-chaos`, and `tempest` jobs.

| Step | Description |
| --- | --- |
| 1 | `docker/login-action@v4` authenticates to GHCR using the workflow's `GITHUB_TOKEN` |
| 2 | For each input ref, `docker pull` the reference `image-map` gives for it, then `docker tag` to the canonical local ref |

The map holds one of two forms per ref: `<repo>:e2e-<run-id>-<tag>` for an image this
run built, or `<repo>@sha256:...` for one it reused from `main`. A ref the map does
not carry falls back to the run-scoped tag, and so does every ref when `image-map` is
left empty. A malformed map fails the step before the first pull.

| Input | Default | Description |
| --- | --- | --- |
| `run-id` | `${{ github.run_id }}` | Run ID used as the tag prefix (`e2e-<run-id>-`) |
| `images` | (required) | Multiline list of canonical local refs (e.g. `ghcr.io/c5c3/keystone:2025.2`); blank/comment lines are ignored |
| `image-map` | `''` | The `image-map` output of `build-e2e-images`; empty means pull every ref by run-scoped tag |
| `registry` | `ghcr.io` | Registry to authenticate against |
| `username` | `${{ github.actor }}` | Login user |
| `password` | `${{ github.token }}` | Login token |

Usage in a workflow job:

```yaml
- name: Load E2E images
  uses: ./.github/actions/load-e2e-images
  with:
    image-map: ${{ needs.build-e2e-images.outputs.image-map }}
    images: |
      ${{ env.IMAGE_PREFIX }}/keystone-operator:dev
      ${{ env.IMAGE_PREFIX }}/keystone:2025.2
```

GH-310 replaced the previous `actions/download-artifact` + `zstd | docker load`
sequence: the 355 MB single-blob artifact intermittently timed out at the
five-minute window (`actions/download-artifact` has no built-in retry on a stalled
download). Layer-level pull retries plus the GHCR CDN dramatically reduce the
failure rate.

## How the Pieces Fit Together

The E2E jobs follow a common pattern with shared components:

```
1. Checkout + Go setup + kind cluster creation     (workflow steps)
2. Pull pre-built images from GHCR                  (load-e2e-images composite action)
3. Load images into kind                            (workflow steps)
4. Deploy infrastructure                            (setup-e2e-infra composite action)
5. Deploy operator                                  (hack/ci-deploy-operator.sh)
6. Run tests                                        (chainsaw / hack/ci-run-tempest.sh)
7. Dump diagnostics                                 (hack/ci-dump-diagnostics.sh)
8. Upload artifacts                                 (workflow steps)
```

Image building is centralised in `build-e2e-images`, which runs once before the E2E jobs
and pushes the images it built to GHCR under a run-scoped tag. An image it reused from
`main` is pulled by digest and carries no run-scoped tag. The `e2e-infra` job uses steps
1, 4, 6-8 (no operator or service images needed). The `e2e-operator`, `e2e-chaos`, and
`tempest` jobs use all steps, pulling their required images from GHCR via
`load-e2e-images`. The `e2e-chaos` job uses a chaos-specific Chainsaw config
(`tests/e2e-chaos/chainsaw-config.yaml`) and test directory (`tests/e2e-chaos/`). The
`tempest` job additionally deploys a Keystone CR before running `hack/ci-run-tempest.sh`
instead of Chainsaw. The `cleanup-e2e-tags` job prunes the run-scoped tags after every
consumer finishes.

## Go Setup Convention

All Go-based jobs use `actions/setup-go@v6` with:

```yaml
go-version-file: go.work
```

This reads the Go version from `go.work` (currently Go 1.26.6) rather than hardcoding a
`go-version` value. The repository root contains `go.work` (not `go.mod`) because the
project uses a Go Workspace with multiple modules (`internal/common`, `operators/keystone`,
`operators/c5c3`). Module dependency caching is enabled by default in `actions/setup-go@v6`.

## Concurrency

The workflow uses a concurrency group scoped per-branch per-workflow:

```yaml
concurrency:
  group: ${{ github.event_name == 'pull_request' && github.event.action == 'labeled' && !startsWith(github.event.label.name, 'ci:') && github.event.label.name != 'run-chaos' && format('noop-{0}', github.run_id) || format('{0}-{1}', github.ref, github.workflow) }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

For pull requests, pushing new commits cancels any in-progress CI run for that same PR
branch, preventing wasted CI resources on outdated code. For pushes to `main`, in-progress
runs are **not** cancelled, ensuring every merge commit is fully validated. Different
branches do not cancel each other's runs.

Applying a label that does not steer CI used to cancel the run in flight and start
it again from scratch, because every `labeled` event landed in the branch's group.
Such an event now gets a group of its own, keyed on the run id, and the `changes`
job resolves it to nothing; the run costs one `ubuntu-latest` runner for about
twenty seconds. A `ci:*` label (or `run-chaos`) stays in the branch's group and is
meant to supersede the run in flight, since it asks for jobs that run did not
schedule.

## Action Pinning

All GitHub Actions are referenced by full SHA hash with a trailing version comment:

```yaml
- uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7
```

This prevents supply chain attacks via mutable tag retargeting and provides audit
traceability. The version comment preserves human readability.

## SPDX Header

The file starts with the standard SPDX license header:

```text
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
---
```

## Codecov Configuration

`.codecov.yml` defines coverage status checks and component-level thresholds.

### Ignored Paths

The top-level `ignore` block drops generated code from every coverage
denominator (project, patch, and component targets):

| Pattern | Reason |
| --- | --- |
| `**/zz_generated*.go` | controller-gen DeepCopy plumbing — mechanically generated, no hand-written logic to test. Counting it understates real coverage, notably for the `webhooks` component (target 90%), whose `api/` paths include the generated deepcopy file. |

This mirrors the lint exclusion of the same files (see the note above the
`verify-codegen` job).

### Status Checks

| Check | Target | Description |
| --- | --- | --- |
| Project | `auto` (threshold: 1%) | Overall coverage must not decrease by more than 1% |
| Patch | `90%` | New/changed lines in a PR must meet 90% coverage |

`fail_ci_if_error: false` is set on each `codecov/codecov-action` step in the workflow
(not in `.codecov.yml`, where it is not a valid key) because fork PRs do not have access
to `CODECOV_TOKEN`. This prevents CI from failing due to upload issues on forks.

### Flag Management

The `flag_management` section in `.codecov.yml` links CI-uploaded flags to coverage tracking
rules. Flags follow the `[unit|integration]-<target>` naming convention, matching the CI
matrix targets (`common`, `keystone`, `c5c3`, `horizon`, `glance`, `placement`, `barbican`).
Each flag has `carryforward: true`, which ensures that when only a subset of flags is
uploaded (e.g., only one operator changed), the missing flags carry forward their
last-known coverage instead of reducing the total.

Defined flags:

| Flag | Paths | Source |
| --- | --- | --- |
| `unit-common` | `internal/common/` | `test` job, `common` matrix leg |
| `unit-keystone` | `operators/keystone/` | `test` job, `keystone` matrix leg |
| `unit-c5c3` | `operators/c5c3/` | `test` job, `c5c3` matrix leg |
| `unit-horizon` | `operators/horizon/` | `test` job, `horizon` matrix leg |
| `unit-glance` | `operators/glance/` | `test` job, `glance` matrix leg |
| `unit-placement` | `operators/placement/` | `test` job, `placement` matrix leg |
| `unit-barbican` | `operators/barbican/` | `test` job, `barbican` matrix leg |
| `integration-common` | `internal/common/` | `test-integration` job, `common` matrix leg |
| `integration-keystone` | `operators/keystone/` | `test-integration` job, `keystone` matrix leg |
| `integration-c5c3` | `operators/c5c3/` | `test-integration` job, `c5c3` matrix leg |
| `integration-horizon` | `operators/horizon/` | `test-integration` job, `horizon` matrix leg |
| `integration-glance` | `operators/glance/` | `test-integration` job, `glance` matrix leg |
| `integration-placement` | `operators/placement/` | `test-integration` job, `placement` matrix leg |
| `integration-barbican` | `operators/barbican/` | `test-integration` job, `barbican` matrix leg |

### Component Thresholds

Each component is tracked independently on the Codecov dashboard:

| Component | Paths | Target | Rationale |
| --- | --- | --- | --- |
| `common` | `internal/common/**` | 80% | Shared library code underpinning all operators |
| `controllers` | `operators/*/internal/controller/**` | 70% | Controller reconciliation logic (envtest-dependent paths harder to cover) |
| `webhooks` | `operators/*/api/**` | 90% | Webhook validation/defaulting (incorrect admission logic causes silent data corruption) |

## Makefile Targets

The CI workflow depends on several Makefile targets:

### docker-build

Builds the operator Docker image from `operators/<operator>/Dockerfile` with the
repository root as build context (required by `go.work`).

```
make docker-build OPERATOR=keystone [IMG=custom:tag]
```

The `IMG` variable controls the image tag, defaulting to
`ghcr.io/c5c3/<operator>-operator:latest`. The `OPERATOR` variable is required.

### helm-package

Packages the operator Helm chart from
`operators/<operator>/helm/<operator>-operator/`.

```
make helm-package OPERATOR=keystone [CHART_VERSION=1.2.3]
```

When `CHART_VERSION` is set, it overrides the version in the chart's `Chart.yaml`. The
packaged `.tgz` is output to the current directory. The `OPERATOR` variable is required.

### test-common

Runs unit tests for `internal/common` only, producing a single coverage profile.

```
make test-common
```

Produces `cover-unit-common.out`. Used by the `common` matrix leg in the `test` CI job to
deduplicate common coverage into a single upload.

### test-operator

Runs unit tests for a single operator without `internal/common`.

```
make test-operator OPERATOR=keystone
```

Produces `cover-unit-<operator>.out`. Used by operator matrix legs in the `test` CI job.
The `OPERATOR` variable is required.

### test-integration

Runs envtest-based integration tests (tagged with `//go:build integration`) for operators.
Requires `setup-envtest` to be installed.

```
make test-integration [OPERATOR=keystone]
```

Sets `KUBEBUILDER_ASSETS` via `setup-envtest use <pinned-k8s-version> -p path`, then runs
`go test -tags=integration` for each operator module. Produces
`cover-integration-<operator>.out` files. Without `OPERATOR`, runs for all operators in
the `OPERATORS` list.

### test-integration-common

Runs envtest-based integration tests for `internal/common` only.

```
make test-integration-common
```

Sets `KUBEBUILDER_ASSETS` via `setup-envtest use <pinned-k8s-version> -p path`, then runs
`go test -tags=integration ./internal/common/...`. Produces `cover-integration-common.out`.
Used by the `common` matrix leg in CI to meet the 80% codecov target for `internal/common/`.

## Dependencies on Prior Features

The CI workflow depends on the following artifacts:

| Artifact | Used by | Purpose |
| --- | --- | --- |
| `Makefile` (`lint` target) | `lint` job | Iterates over `OPERATORS` variable to run golangci-lint per module |
| `Makefile` (`test-common` target) | `test` job (`common` leg) | Runs unit tests for `internal/common` with coverage profile |
| `Makefile` (`test-operator` target) | `test` job (operator legs) | Runs unit tests for a single operator with coverage profile |
| `Makefile` (`test-integration` target) | `test-integration` job (operator legs) | Runs envtest integration tests per operator with coverage profiles |
| `Makefile` (`test-integration-common` target) | `test-integration` job (`common` leg) | Runs envtest integration tests for `internal/common` with coverage profile |
| `Makefile` (`docker-build` target) | `build-e2e-images`, `e2e-chaos`, `build-and-push` jobs | Builds operator Docker images |
| `Makefile` (`helm-package` target) | `helm-push` job | Packages operator Helm charts |
| `.golangci.yml` | `lint` job | Provides linter configuration (enabled linters, exclusion rules, timeout) |
| `go.work` | All Go-based jobs | Provides the Go version for `actions/setup-go@v6` |
| `hack/*.sh` | `shellcheck` job | Shell scripts validated by shellcheck |
| `.codecov.yml` | Codecov integration | Component-level coverage thresholds |
| `hack/ci-dump-diagnostics.sh` | `e2e-infra`, `e2e-operator`, `e2e-chaos`, `tempest` jobs | Shared diagnostic dump |
| `hack/ci-build-service-image.sh` | `build-e2e-images` job | Builds OpenStack service images from an authenticated clone of the upstream source |
| `hack/ci-deploy-korc.sh` | `e2e-operator` (c5c3 leg), `e2e-controlplane`, `e2e-controlplane-sso`, `e2e-external-keystone` jobs | Applies K-ORC from an authenticated clone at the pinned commit |
| `hack/ci-deploy-operator.sh` | `e2e-operator`, `e2e-chaos`, `tempest` jobs | Deploys operator via Helm |
| `hack/ci-run-tempest.sh` | `tempest` job | Runs Tempest API tests |
| `.github/actions/setup-test-deps/` | `chainsaw-lint` job, `setup-e2e-infra` composite action | Composite action for testdeps cache + `make install-test-deps` |
| `.github/actions/setup-e2e-infra/` | `e2e-infra`, `e2e-operator`, `e2e-chaos`, `tempest` jobs | Composite action for infra setup |
| `.github/actions/load-e2e-images/` | `e2e-operator`, `e2e-chaos`, `tempest` jobs | Composite action that pulls run-scoped GHCR tags and re-tags them to canonical local refs (GH-310) |
| `hack/ghcr-prune-stale-versions.py` | `cleanup-e2e-tags` job, `cleanup-images.yaml` | Deletes GHCR package versions that carry no keeper tag; resolves multi-arch children and cosign referrers first |
| `hack/ci-generate-cleanup-matrix.sh` | `changes` job, `cleanup-images.yaml` | Derives the GHCR package lists from `images/` and `operators/` |
| `tests/e2e-chaos/chainsaw-config.yaml` | `e2e-chaos` job | Chaos-specific Chainsaw configuration |

:::
