# Forge - Multi-operator Go workspace
#
# NOTE: `go build ./...` from the workspace root does not work because the root
# directory has no go.mod (only go.work). Build/test/lint iterate over each module
# directory instead. Use `make build` to compile all operators.
#
# Operators to build/test/lint (override with: make build OPERATOR=keystone)
OPERATORS ?= keystone c5c3

ifdef OPERATOR
OPERATORS = $(OPERATOR)
endif

MODULE_DIRS = $(addprefix operators/,$(OPERATORS))
ALL_MODULE_DIRS = internal/common $(MODULE_DIRS)

.PHONY: build test lint generate manifests docker-build helm-package e2e deploy-infra install-test-deps test-integration

## Build all operator binaries (output to bin/ to avoid accidental commits)
build:
	@mkdir -p bin
	@for dir in $(MODULE_DIRS); do \
		name=$$(basename $$dir); \
		echo "Building $$dir..."; \
		(cd $$dir && go build -o ../../bin/$$name ./...) || exit 1; \
	done
	@echo "Building internal/common..."
	@(cd internal/common && go build ./...) || exit 1

## Run unit tests for all modules
test:
	@for dir in $(ALL_MODULE_DIRS); do \
		echo "Testing $$dir..."; \
		(cd $$dir && go test ./...) || exit 1; \
	done

## Run golangci-lint on all modules
lint:
	@for dir in $(ALL_MODULE_DIRS); do \
		echo "Linting $$dir..."; \
		(cd $$dir && golangci-lint run) || exit 1; \
	done

## Generate code (no-op until controller-gen is configured)
generate:
	@echo "generate: no-op until controller-gen is configured"

## Generate manifests (no-op until controller-gen is configured)
manifests:
	@echo "manifests: no-op until controller-gen is configured"

## Build Docker images (stub - requires S006)
docker-build:
	$(error docker-build target requires S006 implementation)

## Package Helm charts (stub - requires S017)
helm-package:
	$(error helm-package target requires S017 implementation)

## Run end-to-end tests (stub - requires S002)
e2e:
	$(error e2e target requires S002 implementation)

## Deploy infrastructure (stub - requires S008)
deploy-infra:
	$(error deploy-infra target requires S008 implementation)

## Install test dependencies (stub - requires S002)
install-test-deps:
	$(error install-test-deps target requires S002 implementation)

## Run integration tests (stub - requires S002)
test-integration:
	$(error test-integration target requires S002 implementation)
