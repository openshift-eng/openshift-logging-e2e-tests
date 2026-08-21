# AGENTS.md

Guidance for AI agents working on this repository.

## Build

```bash
make build
# Regenerates testdata/bindata.go and compiles the extension binary to bin/openshift-logging-e2e-tests-tests-ext
```

Always rebuild after editing any `.go` file or testdata fixtures before running tests.

## Running tests

```bash
export KUBECONFIG=/path/to/kubeconfig

# Run by case ID (preferred)
make run-case CASE=65131

# Find a test by ID
make list-tests GREP=65131
```

## Critical patterns

### `NewCLIWithoutNamespace` — never `NewCLI`

All `oc` variables must use `exutil.NewCLIWithoutNamespace(...)`. Do not use `exutil.NewCLI(...)`. The `NewCLI` variant registers an automatic `BeforeEach(SetupProject)` which creates an extra namespace on every test run.

### `SetupProject()` before `oc.Namespace()`

Tests that call `oc.Namespace()` at the top of a `g.It()` block will get an empty string unless `oc.SetupProject()` has been called first — either in the enclosing `g.BeforeEach` or at the start of the `g.It` block itself. The symptom is `error: resource name may not be empty` with `-n ` (empty namespace) in the `oc` command.

Fix: add `oc.SetupProject()` immediately before the first `oc.Namespace()` call in the It block.

### `FixturePath` — no `"testdata"` prefix

`go-bindata` is invoked with `-prefix "testdata"`, so asset names are stored **without** the `testdata/` prefix. Any `FixturePath(...)` call that includes `"testdata"` as the first element will panic with `Asset not found`.

Correct:
```go
filePath := []string{"logging", "external-log-stores", "rsyslog"}
```

Wrong:
```go
filePath := []string{"testdata", "logging", "external-log-stores", "rsyslog"}
```

### `bindata.go` is tracked in git

`test/e2e/testdata/bindata.go` is a generated file but it **is** committed to the repository. Do not add it to `.gitignore`.

## Test file conventions

- Each `g.Describe` / `g.Context` has a `g.BeforeEach` that deploys the required operator(s) (CLO, LokiOperator, etc.)
- Tests that need multiple namespaces call `oc.SetupProject()` multiple times within the `g.It` block — each call creates a new project and advances `oc.Namespace()`
- Shared utilities live in `utils.go`, `loki_utils.go`, `elasticsearch_utils.go`, `splunk_util.go`, `aws_utils.go`, `azure_utils.go`
- Testdata fixtures are embedded at build time; add new YAML files under `test/e2e/testdata/logging/` and run `make build`
