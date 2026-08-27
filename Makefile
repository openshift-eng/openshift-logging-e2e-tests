BINARY := bin/openshift-logging-e2e-tests-tests-ext

.PHONY: build
build:
	@echo "Building extension binary..."
	@cd test/e2e && $(MAKE) -f bindata.mk update-bindata
	@mkdir -p bin
	GOTOOLCHAIN=auto GONOSUMDB="*" GOFLAGS="" go build -mod=mod -o $(BINARY) ./cmd
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

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build       - Build extension binary"
	@echo "  clean       - Remove binaries and bindata"
	@echo "  run-test    - Run a test by name (set TEST=<name>)"
	@echo "  run-case    - Run a test by case ID (set CASE=<id>)"
	@echo "  list-tests  - List tests, optionally filtered (set GREP=<pattern>)"
