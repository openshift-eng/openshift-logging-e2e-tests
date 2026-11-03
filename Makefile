BINARY := bin/openshift-logging-e2e-tests-tests-ext

# Tools
GOLANGCI_LINT := go tool -modfile=.tools/golangci-lint.mod golangci-lint

# Lint timeout. The deadline covers package loading, not just analysis:
# cold-cache CI containers must download and type-check the full dependency
# tree (openshift/origin et al.) before any linter runs, which alone can
# exceed 5 minutes. Overridable: make lint GOLANGCI_LINT_TIMEOUT=30m
GOLANGCI_LINT_TIMEOUT ?= 7m

# Prints one test name per line. The binary exits early (with status 0) unless
# KUBECONFIG is set, even for listing; listing never contacts the cluster, so
# any value works there. klog also writes to stdout, so only lines that look
# like test names are kept.
LIST_TESTS := KUBECONFIG=$${KUBECONFIG:-/dev/null} $(BINARY) list tests -o names | grep '^\['

# run_tests runs the test named by $(1), prints its result and fails if the test
# failed. Every spec is Informing (see cmd/main.go), so run-test exits 0 even
# when a test fails; the outcome is read from its JSONL results instead. klog
# also writes to stdout, so only the result lines (starting with "{") are kept.
# The test log and failure details go to stderr and stay on the terminal.
define run_tests
[ -n "$$KUBECONFIG" ] || { echo "❌ KUBECONFIG is not set, export it to point at the test cluster"; exit 1; }; \
command -v jq >/dev/null || { echo "❌ jq is required to read the test results"; exit 1; }; \
RESULTS=$$(mktemp); trap 'rm -f "$$RESULTS"' EXIT; \
$(BINARY) run-test -o jsonl $(1) | grep '^{' > "$$RESULTS"; \
if [ ! -s "$$RESULTS" ]; then \
	echo "❌ No test results recorded, see the output above"; exit 1; \
fi; \
echo; \
jq -r '(if .result == "passed" then "✅" elif .result == "skipped" then "⏭️ " else "❌" end) + " " + .result + ": " + .name' "$$RESULTS"; \
jq -se 'all(.[]; .result != "failed")' "$$RESULTS" > /dev/null
endef

.PHONY: build
build: ## Build extension binary.
	@echo "Building extension binary..."
	@mkdir -p bin
# GOFLAGS is pinned because the OpenShift Go builder images export
# GOFLAGS=-mod=vendor. This repo has no vendor/ directory, so an inherited
# value sends the build looking for vendor/modules.txt and fails with
# "inconsistent vendoring" in CI while still succeeding locally.
	GOFLAGS=-mod=readonly GOTOOLCHAIN=auto GONOSUMDB="*" go build -o $(BINARY) ./cmd
	@echo "✅ Binary built: $(BINARY)"

.PHONY: clean
clean: ## Remove binaries.
	@rm -f $(BINARY)

# Usage: make run-test TEST="<exact test name>"
# Example: make run-test TEST="[sig-openshift-logging] LOGGING Logging Author:..."
.PHONY: run-test
run-test: ## Run a test by name (set TEST=<name>).
	@$(call run_tests,"$(TEST)")

# Usage: make list-tests GREP="<case-id or keyword>"
.PHONY: list-tests
list-tests: ## List tests, optionally filtered (set GREP=<pattern>).
	@$(LIST_TESTS) | grep "$(GREP)"

# Usage: make run-case CASE=<case-id>
# Example: make run-case CASE=76728
.PHONY: run-case
run-case: ## Run a test by case ID (set CASE=<id>).
	@NAME=$$($(LIST_TESTS) | grep -F -- '-$(CASE)-'); \
	COUNT=$$(echo "$$NAME" | grep -c .); \
	if [ "$$COUNT" -eq 0 ]; then \
		echo "❌ No test found matching case ID: $(CASE)"; exit 1; \
	elif [ "$$COUNT" -gt 1 ]; then \
		echo "❌ Multiple tests match $(CASE), use make run-test TEST=\"<name>\":"; \
		echo "$$NAME"; exit 1; \
	fi; \
	echo "Running: $$NAME"; \
	$(call run_tests,"$$NAME")

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

# Lists every target that has a "## description" comment on its rule line.
.PHONY: help
help: ## Show this help.
	@echo "Available targets:"
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
