---
title: CI Workflow
quadrant: infrastructure
---

# CI Workflow

## File Location

`.github/workflows/ci.yaml`

## Trigger Events

| Event          | Filter             | Description                              |
|----------------|--------------------|------------------------------------------|
| `push`         | `branches: [main]` | Runs on every push to the `main` branch  |
| `pull_request` | (all)               | Runs on every pull request event         |

Pushes to non-main branches without an open PR do **not** trigger the workflow.

## Jobs

All three jobs run **in parallel** with no inter-job dependencies (`needs:` is absent).
Each job runs on `ubuntu-latest`.

### `lint`

Runs `golangci-lint` against the Go workspace root.

| Step | Action                              | Config             |
|------|-------------------------------------|--------------------|
| 1    | `actions/checkout@v4`               |                    |
| 2    | `golangci/golangci-lint-action@v9`  | `version: v2.10`   |

The lint action handles Go installation internally — no separate `actions/setup-go` step is needed.
It uses the project's `.golangci.yml` configuration from the repository root.

### `test`

Runs unit tests via `make test`.

| Step | Action                  | Config                         |
|------|-------------------------|--------------------------------|
| 1    | `actions/checkout@v4`   |                                |
| 2    | `actions/setup-go@v5`   | `go-version-file: go.work`     |
| 3    | `make test`             |                                |

### `test-integration`

Runs envtest-based integration tests via `make test-integration`.

| Step | Action                  | Config                         |
|------|-------------------------|--------------------------------|
| 1    | `actions/checkout@v4`   |                                |
| 2    | `actions/setup-go@v5`   | `go-version-file: go.work`     |
| 3    | `make test-integration` |                                |

## Go Setup Convention

Both `test` and `test-integration` jobs use `actions/setup-go@v5` with `go-version-file: go.work`.
This reads the Go version from the workspace file rather than hardcoding it in the workflow.
Module caching is enabled by default (the `cache` input defaults to `true`).

## Concurrency

```yaml
concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: true
```

This cancels in-progress runs when new commits are pushed to the same branch,
saving CI resources. Different branches do not cancel each other because the
concurrency group key includes `github.ref`.

## Permissions

```yaml
permissions:
  contents: read
```

The workflow uses least-privilege permissions. Only `contents: read` is granted
at the top level, and no job overrides this.

## Dependencies

- **CC-0001 Makefile targets**: `make test` and `make test-integration` are defined in the
  root `Makefile`. They iterate over all modules listed in the `OPERATORS` variable.
- **CC-0001 `.golangci.yml`**: The lint job relies on this configuration file at the
  repository root.
- **CC-0001 `go.work`**: Used by `actions/setup-go` to determine the Go version.
