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
│       ├── fixtures.go          # FixturePath() helper (backed by embedded bindata)
│       ├── bindata.go           # Generated — do not edit (run `make build` to regenerate)
│       └── logging/             # YAML fixtures (operators, log-stores, certs, …)
├── go.mod
├── go.sum
└── Makefile
```

## Prerequisites

| Tool | Version |
|------|---------|
| Go | 1.23+ (toolchain 1.26 via `GOTOOLCHAIN=auto`) |
| `go-bindata` | any — install with `go install github.com/jteeuwen/go-bindata/go-bindata@latest` |
| `KUBECONFIG` | pointing at a running OpenShift cluster |

> **Note**: `openshift-tests` is only needed if you want to run suites via the OTE runner in CI. For local development, use the extension binary directly (see below).

## Building

```bash
make build
# produces: bin/openshift-logging-e2e-tests-tests-ext
```

`make build` does two things:
1. Regenerates `test/e2e/testdata/bindata.go` from the YAML fixtures under `testdata/logging/`
2. Compiles the extension binary

To clean build artifacts:

```bash
make clean
```

## Test suites

The extension registers five named suites:

| Suite | What it runs |
|-------|-------------|
| `openshift-logging-e2e-tests/conformance/parallel` | `[Level0]` tests that are not `[Serial]` or `[Disruptive]` |
| `openshift-logging-e2e-tests/conformance/serial` | `[Level0]` + `[Serial]` tests |
| `openshift-logging-e2e-tests/disruptive` | `[Disruptive]` tests |
| `openshift-logging-e2e-tests/non-disruptive` | All tests that are not `[Disruptive]` |
| `openshift-logging-e2e-tests/all` | Every test in the extension |

The conformance suites are children of `openshift/conformance/parallel` and `openshift/conformance/serial` respectively, so they are automatically included in OpenShift CI full-suite runs.

## Running tests

### List available tests

```bash
export KUBECONFIG=/path/to/kubeconfig
./bin/openshift-logging-e2e-tests-tests-ext list
```

### Run a single test (local development)

Use `make run-case` with the case ID — no need to look up the full name:

```bash
export KUBECONFIG=/path/to/kubeconfig
make run-case CASE=65131
```

### Find tests by case ID or keyword

```bash
make list-tests GREP=65131
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

## Test annotations

All tests follow the OTE annotation conventions:

- `[sig-openshift-logging]` — SIG ownership label
- `[Level0]` — smoke / must-pass tests included in conformance suites
- `[Serial]` — test must not run concurrently with other tests
- `[Disruptive]` — test may affect cluster state

## Adding testdata fixtures

1. Place new YAML files under `test/e2e/testdata/logging/`
2. Run `make build` — this regenerates `bindata.go` and embeds the new files
3. Access them in tests via `testdata.FixturePath("logging", "your-dir", "file.yaml")`

## Module notes

- `go.mod` declares `go 1.23` (minimum) with `toolchain go1.26.0` (preferred)
- `GOTOOLCHAIN=auto` in the Makefile allows Go to download and use the declared toolchain automatically
- `GONOSUMDB="*"` is required because several OpenShift-internal packages are fetched from private GitHub forks
- Fixtures are embedded at build time via `go-bindata` so the binary is self-contained and does not need testdata on disk at runtime
