# openshift-logging-e2e-tests

End-to-end tests for OpenShift Logging, implemented as an [OpenShift Tests Extension (OTE)](https://github.com/openshift-eng/openshift-tests-extension).

## Repository structure

```
openshift-logging-e2e-tests/
├── cmd/
│   └── main.go                  # Extension entry point; registers suites and wires OTE
├── test/e2e/
│   ├── acceptance.go            # Acceptance / smoke tests
│   ├── logging_operators.go     # Operator deployment, audit, flow-control, network-policy tests
│   ├── logging_performance.go   # LokiStack performance tests
│   ├── loki.go                  # LokiStack functional tests
│   ├── loki_managed_sts_wif.go  # LokiStack managed-auth / STS / WIF tests
│   ├── loki_utils.go            # Shared Loki helpers
│   ├── otlp.go                  # OTLP output tests
│   ├── scheduler.go             # Log-scheduler tests
│   ├── splunk_util.go           # Splunk helpers
│   ├── aws_utils.go             # AWS CloudWatch / S3 helpers
│   ├── azure_utils.go           # Azure Monitor / Blob helpers
│   ├── elasticsearch_utils.go   # Elasticsearch helpers
│   ├── const.go                 # Shared constants
│   └── testdata/
│       ├── fixtures.go          # FixturePath() helper (backed by //go:embed)
│       └── logging/             # YAML fixtures (operators, log-stores, certs, …)
├── go.mod
├── go.sum
└── Makefile
```

## Prerequisites

| Tool | Version |
|------|---------|
| Go | 1.26+ (fetched automatically via `GOTOOLCHAIN=auto`) |
| `KUBECONFIG` | pointing at a running OpenShift cluster |
| `jq` | any — used by `make run-case` / `make run-test` to read test results |

> **Note**: `openshift-tests` is only needed if you want to run suites via the OTE runner in CI. For local development, 
> use the extension binary directly (see below).

## Building

```bash
make build
# produces: bin/openshift-logging-e2e-tests-tests-ext
```

`make build` compiles the extension binary. The YAML fixtures under `test/e2e/testdata/logging/` are embedded into it 
via `//go:embed`, so there is no code generation step.

To run the cluster-free unit tests — fixture embedding and fixture path references in `test/e2e/testdata`, plus unit 
tests for packages under `internal/`:

```bash
make test
```

To compile the helper tools under `internal/tools/` into `bin/` (done automatically by the targets that need them):

```bash
make tools
```

To clean build artifacts (the extension binary and the tool binaries):

```bash
make clean
```

## Test suites

The extension registers seven named suites:

| Suite | What it runs |
|-------|-------------|
| `openshift-logging-e2e-tests/conformance/parallel` | `[Level0]` tests that are not `[Serial]` or `[Disruptive]` |
| `openshift-logging-e2e-tests/conformance/serial` | `[Level0]` + `[Serial]` tests |
| `openshift-logging-e2e-tests/cluster-logging-operator` | `[PreMerge]` + `[CLO]` tests that are not `[Disruptive]` |
| `openshift-logging-e2e-tests/loki-operator` | `[PreMerge]` + `[LokiOperator]` tests that are not `[Disruptive]` |
| `openshift-logging-e2e-tests/disruptive` | `[Disruptive]` tests |
| `openshift-logging-e2e-tests/non-disruptive` | All tests that are not `[Disruptive]` |
| `openshift-logging-e2e-tests/all` | Every test in the extension |

The conformance suites are children of `openshift/conformance/parallel` and `openshift/conformance/serial` respectively,
so they are automatically included in OpenShift CI full-suite runs.

## Running tests

### List available tests

```bash
make list-tests GREP=65131
```

### Run a single test (local development)

Use `make run-case` with the case ID — no need to look up the full name:

```bash
export KUBECONFIG=/path/to/kubeconfig
make run-case CASE=65131
```

The target prints whether the test passed, failed or was skipped, and exits non-zero if it failed. This is needed because every test is registered as Informing, so the binary itself exits 0 even on failure.

To inspect the cluster state of a failed test, set `LOGGING_E2E_KEEP_ON_FAILURE=true`. The resources a failed spec created are then left in place: its namespaces (named `e2e-test-<suite>-<suffix>`, find them with `oc get ns | grep e2e-test`), the forwarder, the Loki stack, and also cluster-scoped objects such as role bindings. Delete the namespaces by hand (`oc delete project <ns>`) when you are done, and check for leftover cluster-scoped objects. Use it with `run-case` or `run-test`, not a whole suite, so kept resources don't affect other tests.

Cleanup is registered with `deferCleanup(func() { ... })` instead of `defer`; use it for anything the test creates in the cluster. Keep a plain `defer` for local clean-up (temp files, `Close`) and for restoring shared cluster state the test changed but did not create (for example the API server TLS profile or node audit rules), because that must run even when resources are kept. `deferCleanup` runs after the namespaces start deleting, so don't wait on namespaced resources that are expected to still exist.

### Find tests by case ID or keyword

```bash
make list-tests GREP=65131
```

### List and run tests by ginkgo label

Some tests carry ginkgo labels (e.g. `component:clo`, `exec:slow`, `jira:LOG-9876`) via `Label(...)` decorators, using 
the constants and helpers from `internal/labels`. To see every label in use — a static source scan, no cluster or test 
run required:

```bash
# All labels
make list-labels

# Filter by substring
make list-labels GREP=jira
```

The target compiles the scanner tool under `internal/tools/` (via `make tools`) and resolves label constants such as
`labels.ComponentCLO` and helpers such as `labels.Jira("LOG-9876")` to their final values.

To run all tests matching a set of labels (all must match, AND semantics):

```bash
make run-labels LABELS="component:clo exec:slow"
```

### Run a suite via `openshift-tests` (CI)

If `openshift-tests` is available (e.g. in CI), you can run entire suites:

```bash
export KUBECONFIG=/path/to/kubeconfig

# Run the parallel conformance suite
openshift-tests run openshift-logging-e2e-tests/conformance/parallel \
  --provider '{"type":"aws"}' \
  -o /tmp/e2e.log \
  --junit-dir /tmp/junit/ \
  --from-repository "file://$(pwd)/bin/openshift-logging-e2e-tests-tests-ext"

# Run all tests
openshift-tests run openshift-logging-e2e-tests/all \
  --from-repository "file://$(pwd)/bin/openshift-logging-e2e-tests-tests-ext"
```

### Filter by pattern via `openshift-tests`

```bash
# Only LokiStack tests
openshift-tests run openshift-logging-e2e-tests/all \
  --from-repository "file://$(pwd)/bin/openshift-logging-e2e-tests-tests-ext" \
  --run "LokiStack"
```

## Version gating with `LOGGING_TEST_VERSION`

Some tests only apply to certain logging releases. `LOGGING_TEST_VERSION` tells the suite which logging version is under
test, and version-gated tests skip themselves when it is below their minimum:

```bash
export LOGGING_TEST_VERSION=6.7
```

- **Unset** — version-gated tests run without restriction (local convenience default).
- **Set** — tests call `helpers.SkipIfLoggingVersionLessThan(min)` (from `internal/test/helpers`) and skip with a message when the value is below `min`. The value must be a bare semver-style version (e.g. `6.7`); it is compared as-is, so `release:`-prefixed values from OpenShift release labels are not valid here.
- Some tests (e.g. upgrade tests) *require* the variable and fail if it is unset.

The `make` run targets pass the variable through for you (optional, defaults to empty = no gating):

```bash
make run-case CASE=76728 LOGGING_TEST_VERSION=6.5
```

To gate a new test, pair the check with a release label constant from `internal/labels`:

```go
helpers.SkipIfLoggingVersionLessThan(labels.ReleaseLogging67)
```

## Test annotations

All tests follow the OTE annotation conventions:

- `[sig-openshift-logging]` — SIG ownership label
- `[Level0]` — smoke / must-pass tests included in conformance suites
- `[PreMerge]` — candidate regression test selected for operator PR CI
- `[CLO]` — test exercises cluster-logging-operator behavior
- `[LokiOperator]` — test exercises loki-operator behavior
- `[Serial]` — test must not run concurrently with other tests
- `[Disruptive]` — test may affect cluster state

## Adding testdata fixtures

1. Place new YAML files under `test/e2e/testdata/logging/`
2. Run `make build` — the new files are embedded automatically
3. Access them in tests via `testdata.FixturePath("logging", "your-dir", "file.yaml")`

## Module notes

- `go.mod` declares `go 1.26.0` (minimum)
- `GOTOOLCHAIN=auto` in the Makefile allows Go to download and use that version automatically if the local Go is older
- Fixtures are embedded at build time via `//go:embed` so the binary is self-contained and does not need testdata on disk at runtime
