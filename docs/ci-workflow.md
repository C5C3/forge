<!--
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
-->

---
title: CI Workflow
quadrant: infrastructure
---

# CI Workflow

Reference documentation for the GitHub Actions CI workflow (`.github/workflows/ci.yaml`).

## File Location

```text
.github/workflows/ci.yaml
```

## Trigger Events

The workflow triggers on:

- **`push`** to the `main` branch — runs CI on every direct push to main.
- **`pull_request`** (all activity types) — runs CI on PR open, synchronize, and reopen.

Pushes to non-main branches without an open PR do **not** trigger the workflow.

## Jobs

All three jobs run in **parallel** with no inter-job dependencies (`needs:` is absent).
Each job uses `runs-on: ubuntu-latest`.

### `lint`

Runs golangci-lint against the Go workspace root.

| Step | Action |
| --- | --- |
| 1 | `actions/checkout` (SHA-pinned, v4) |
| 2 | `golangci/golangci-lint-action` (SHA-pinned, v9) with `version: v2.10` |

The golangci-lint-action handles Go installation internally — no separate `actions/setup-go` step is needed. The action reads the project's `.golangci.yml` for linter configuration.

### `test`

Runs unit tests via the Makefile.

| Step | Action |
| --- | --- |
| 1 | `actions/checkout` (SHA-pinned, v4) |
| 2 | `actions/setup-go` (SHA-pinned, v5) with `go-version-file: go.work` |
| 3 | `make test` |

### `test-integration`

Runs envtest-based integration tests via the Makefile.

| Step | Action |
| --- | --- |
| 1 | `actions/checkout` (SHA-pinned, v4) |
| 2 | `actions/setup-go` (SHA-pinned, v5) with `go-version-file: go.work` |
| 3 | `make test-integration` |

## Go Setup Convention

Both `test` and `test-integration` jobs use `actions/setup-go@v5` with:

- **`go-version-file: go.work`** — the Go version is read from the workspace file rather than hardcoded, so upgrading Go only requires updating `go.work`.
- **Module caching** — enabled by default in `actions/setup-go@v5` (caches `~/go/pkg/mod`). Not explicitly disabled.

The `lint` job does not use a separate `actions/setup-go` step because `golangci-lint-action` manages Go installation internally.

## Concurrency

```yaml
concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: true
```

- Scoped per-branch per-workflow: pushing to branch A does not cancel runs on branch B.
- New pushes to the same branch cancel any in-progress CI run for that branch.
- Matches the concurrency pattern used in `mega-linter.yml`.

## Permissions

```yaml
permissions:
  contents: read
```

Top-level `contents: read` applies to all jobs (least-privilege). No job-level `permissions:` overrides exist.

## Dependencies

This workflow depends on artifacts from [CC-0001](https://github.com/C5C3/forge/pull/1):

- **`go.work`** — used by `actions/setup-go` to determine the Go version.
- **`Makefile`** — provides `test` and `test-integration` targets that iterate over operator modules via the `OPERATORS` variable.
- **`.golangci.yml`** — linter configuration consumed by `golangci-lint-action`.

## Conventions

- **SPDX header**: `Copyright 2026 SAP SE or an SAP affiliate company`, `Apache-2.0` — matching `deploy-docs.yaml`.
- **File extension**: `.yaml` (not `.yml`) — matching `reuse.yaml` and `deploy-docs.yaml`.
- **`"on"` quoting**: the trigger key is quoted to prevent YAML boolean interpretation.
- **YAML document separator**: `---` follows the SPDX header block.
- **SHA-pinned actions**: all `uses:` references pin to full 40-character commit SHAs with a version comment (e.g., `actions/checkout@<sha> # v4`). This prevents supply-chain attacks via mutable tags. When updating an action version, resolve the new tag to its commit SHA and update both the hash and the comment.
