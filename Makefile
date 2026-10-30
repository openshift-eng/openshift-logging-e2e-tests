BINARY := bin/openshift-logging-e2e-tests-tests-ext

# GOFLAGS is pinned because the OpenShift Go builder images export
# GOFLAGS=-mod=vendor. This repo has no vendor/ directory, so an inherited
# value sends the build looking for vendor/modules.txt and fails with
# "inconsistent vendoring" in CI while still succeeding locally.
.PHONY: build
build:
	@echo "Building extension binary..."
	@cd test/e2e && $(MAKE) -f bindata.mk update-bindata
	@mkdir -p bin
	GOFLAGS=-mod=readonly GOTOOLCHAIN=auto GONOSUMDB="*" go build -o $(BINARY) ./cmd
	@echo "✅ Binary built: $(BINARY)"

.PHONY: clean
clean:
	@rm -f $(BINARY)
	@cd test/e2e && $(MAKE) -f bindata.mk clean-bindata

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

# Usage: make run-labels LABELS="<label or space-separated labels>"
# Example: make run-labels LABELS="jira:LOG-9876"
#          make run-labels LABELS="component:clo exec:slow"
# All given labels must match a test (AND semantics).
.PHONY: run-labels
run-labels:
	@if [ -z "$(LABELS)" ]; then \
		echo "❌ Set LABELS=<label or space-separated labels>, e.g. make run-labels LABELS=\"component:clo exec:slow\""; exit 1; \
	fi; \
	NAMES=$$($(BINARY) list -o json 2>/dev/null | python3 -c 'import json,sys; raw=sys.stdin.read(); specs=json.loads(raw[raw.find("["):]); wanted=set("$(LABELS)".split()); print("\n".join(s["name"] for s in specs if wanted <= set(s.get("labels") or {})))'); \
	if [ -z "$$NAMES" ]; then \
		echo "❌ No tests match labels: $(LABELS)"; \
		echo "Hint: make list-labels to see available labels"; exit 1; \
	fi; \
	echo "Running tests matching labels [$(LABELS)]:"; echo "$$NAMES" | sed 's/^/  /'; \
	echo "$$NAMES" | while IFS= read -r NAME; do \
		[ -n "$$NAME" ] || continue; \
		$(BINARY) run-test "$$NAME" > /dev/null || exit 1; \
	done

# Usage: make list-labels [GREP="<substring>"]
# Lists labels in use across all tests, optionally filtered.
.PHONY: list-labels
list-labels:
	@$(BINARY) list -o json 2>/dev/null | python3 -c 'import json,sys; raw=sys.stdin.read(); specs=json.loads(raw[raw.find("["):]); print("\n".join(sorted({l for s in specs for l in (s.get("labels") or {}) if "$(GREP)" in l})))'

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build       - Build extension binary"
	@echo "  clean       - Remove binaries and bindata"
	@echo "  run-test    - Run a test by name (set TEST=<name>)"
	@echo "  run-case    - Run a test by case ID (set CASE=<id>)"
	@echo "  run-labels  - Run all tests matching labels (set LABELS=\"<l1> <l2>...\")"
	@echo "  list-tests  - List tests, optionally filtered (set GREP=<pattern>)"
	@echo "  list-labels - List labels in use, optionally filtered (set GREP=<pattern>)"
