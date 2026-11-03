BINARY := bin/openshift-logging-e2e-tests-tests-ext

# Tools
GOLANGCI_LINT := go tool -modfile=.tools/golangci-lint.mod golangci-lint

# Lint timeout. The deadline covers package loading, not just analysis:
# cold-cache CI containers must download and type-check the full dependency
# tree (openshift/origin et al.) before any linter runs, which alone can
# exceed 5 minutes. Overridable: make lint GOLANGCI_LINT_TIMEOUT=30m
GOLANGCI_LINT_TIMEOUT ?= 7m

.PHONY: build
build:
	@echo "Building extension binary..."
	@mkdir -p bin
# GOFLAGS is pinned because the OpenShift Go builder images export
# GOFLAGS=-mod=vendor. This repo has no vendor/ directory, so an inherited
# value sends the build looking for vendor/modules.txt and fails with
# "inconsistent vendoring" in CI while still succeeding locally.
	GOFLAGS=-mod=readonly GOTOOLCHAIN=auto GONOSUMDB="*" go build -o $(BINARY) ./cmd
	@echo "✅ Binary built: $(BINARY)"

.PHONY: clean
clean:
	@rm -f $(BINARY)

# Usage: make run-test TEST="<exact test name>"
# Example: make run-test TEST="[sig-openshift-logging] LOGGING Logging Author:..."
.PHONY: run-test
run-test:
	@$(BINARY) run-test "$(TEST)" > /dev/null

# Usage: make list-tests GREP="<case-id or keyword>"
.PHONY: list-tests
list-tests:
	@$(BINARY) list 2>&1 | grep "$(GREP)"

# Usage: make run-case CASE=<case-id>
# Example: make run-case CASE=76728
.PHONY: run-case
run-case:
	@MATCHES=$$($(BINARY) list 2>&1 | grep '$(CASE)'); \
	COUNT=$$(echo "$$MATCHES" | grep -c '"name"'); \
	if [ "$$COUNT" -eq 0 ]; then \
		echo "❌ No test found matching case ID: $(CASE)"; exit 1; \
	elif [ "$$COUNT" -gt 1 ]; then \
		echo "❌ Multiple tests match $(CASE), use make run-test TEST=\"<name>\":"; \
		echo "$$MATCHES"; exit 1; \
	fi; \
	NAME=$$(echo "$$MATCHES" | sed 's/.*"name": "\([^"]*\)".*/\1/'); \
	echo "Running: $$NAME"; \
	$(BINARY) run-test "$$NAME" > /dev/null

# Unit tests that need no cluster. Scoped to the testdata package: importing
# test/e2e would register every Ginkgo spec. The path has no "..." because Go
# skips directories named testdata in wildcard patterns.
.PHONY: test
test: ## Run cluster-free unit tests.
	GOFLAGS=-mod=readonly GOTOOLCHAIN=auto GONOSUMDB="*" go test ./test/e2e/testdata

.PHONY: lint
lint: ## Run golangci-lint on source code.
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run --timeout=$(GOLANGCI_LINT_TIMEOUT) ./...

.PHONY: lint-fix
lint-fix: ## Attempt to automatically fix lint issues in source code.
	$(GOLANGCI_LINT) run --fix --timeout=$(GOLANGCI_LINT_TIMEOUT) ./...

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build       - Build extension binary"
	@echo "  clean       - Remove binaries"
	@echo "  test        - Run cluster-free unit tests"
	@echo "  lint        - Run golangci-lint on source code"
	@echo "  lint-fix    - Attempt to automatically fix lint issues"
	@echo "  run-test    - Run a test by name (set TEST=<name>)"
	@echo "  run-case    - Run a test by case ID (set CASE=<id>)"
	@echo "  list-tests  - List tests, optionally filtered (set GREP=<pattern>)"
