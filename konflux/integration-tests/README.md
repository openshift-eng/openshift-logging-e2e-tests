# Konflux integration tests for the Logging ART builds

Tekton pipelines for the Konflux Integration Test Scenarios (ITS) of Red Hat OpenShift Logging in the ART tenant
`art-logging-tenant` (cluster `kflux-ocp-p01`). They run on the FBC (file-based catalog) images that ART builds for the
Cluster Logging Operator (`cluster-logging-operator-fbc`) and the Loki Operator (`loki-operator-fbc`), and run the tests of
this repository. The goal is to ship CVE fixes and patch releases without manual QE, as the Red Hat build of
OpenTelemetry does.

The ITS definitions are in `konflux-release-data`
(`tenants-config/cluster/kflux-ocp-p01/tenants/art-logging-tenant/`) and point at the files in this directory through a git
resolver. Only the scenarios name the release (the application `fbc-logging-<major>-<minor>`); the pipelines read the release
and the OCP version from the FBC at run time.

| Pipeline | What it does |
|---|---|
| `cluster-logging-e2e-test-fbc-pipeline.yaml` | Creates the catalog sources of the FBC images on a new cluster and runs a suite of the tests with in-cluster MinIO as object storage. |

## How a run works

1. `parse-metadata` gets the image of the component that triggered the Snapshot, that is the FBC that ART built now, for one OCP
   version. `resolve-fbc` reads its labels `com.redhat.art.name` and `com.redhat.art.nvr`
   (`cluster-logging-operator-fbc-6.7.1-20261112164651.ocp5.0`): the operator, the logging release (`6.7`) and the OCP
   version (`5.0`). When the OCP version is not `ocp_version`, all other tasks are skipped and the run succeeds, so every
   scenario sees every build and works on its own. The Snapshot has the FBC of both operators (the components
   `fbc-logging-<release>-cluster-logging-operator` and `fbc-logging-<release>-loki-operator`), but the one that did not trigger
   it is only the newest build of that operator, for any OCP version. So the FBC of the other operator is the newest build of the
   same release and OCP version in `quay.io/redhat-user-workloads/ocp-art-tenant/art-fbc`.
2. `prepare-cluster-env` and `provision-cluster` create a HyperShift hosted cluster of that OCP version with the
   `provision-ephemeral-cluster` task of `openshift/konflux-tasks` (OpenShift CI, cluster profile `aws-konflux-prod`). The cluster
   mirrors `registry.redhat.io/openshift-logging` from `registry.stage.redhat.io/openshift-logging`, because the ART images exist only in the
   stage registry until the release is published. The cluster is removed with the PipelineRun.
3. `add-stage-credentials` adds the credentials of the Secret `stage-registry-creds` to the pull secret of the cluster.
4. `create-catalog-sources` creates the catalog sources `cluster-logging-operator-registry` and `loki-operator-registry` in
   `openshift-marketplace` from the FBC images. The tests subscribe the operators from catalog sources with these names when
   they exist, with the channel `stable-<release>`.
5. `build-and-run-tests` builds the extension of this repository (`go build ./cmd`) and runs the tests of the suite `test_suite`
   one by one. The environment is `LOGGING_TEST_VERSION=<release>` (channel and version gating) and
   `LOGGING_TEST_STORAGE=minio` (the LokiStacks use MinIO in the cluster, so no cloud credentials are needed). The task fails
   when a test fails, or when no test passed (skipped tests do not count).

## Parameters

| Parameter | Default | |
|---|---|---|
| `ocp_version` | `5.0` | The OCP version of the scenario. |
| `test_suite` | `openshift-logging-e2e-tests/cluster-logging-operator` | Suite of the extension. |
| `skip_tests` | `CloudWatch\|s3 output` | Case-insensitive regular expression of test names that are not run: the tests that need credentials of AWS CloudWatch or S3. |
| `tests_repo`, `tests_revision` | this repository, `main` | Where the tests are built from. |
| `test_timeout` | `60m` | Timeout of a single test. |

## Secrets in the tenant

| Secret | Used for |
|---|---|
| `stage-registry-creds` (dockerconfigjson) | pulling the ART images from `registry.stage.redhat.io` (added by ART) |

## Run by hand

Maintainers of the tenant can manage ITS and patch Snapshots, but cannot create PipelineRuns:

1. Create a test ITS (the scenario of `konflux-release-data`, with `contexts: [{name: disabled}]` so Konflux does not start it on its
   own, and the `revision` of the resolver set to your branch of this repository).
2. Label a Snapshot of the FBC application: `oc label snapshot <name> -n art-logging-tenant test.appstudio.openshift.io/run=<its>`.
3. Follow the PipelineRun in the Konflux UI (`https://konflux-ui.apps.kflux-ocp-p01.7ayg.p1.openshiftapps.com/ns/art-logging-tenant`).
   Log in with `oc login --web https://api.kflux-ocp-p01.7ayg.p1.openshiftapps.com:6443/`.
4. Remove the test ITS; the tenant configuration is managed from `konflux-release-data`.

## Not verified yet

- The pipeline has not run in the ART tenant yet. Needed first: `art-logging-tenant` as an owner of the `aws-konflux-prod` cluster
  profile in `openshift/release` (https://github.com/openshift/release/pull/88988).
- Whether the PipelineRun of an ITS may run as long as the tests need (the task timeout is 4 hours). The OpenTelemetry e2e
  scenario ran for 67 minutes in `art-rhosdt-tenant` and passed, so more than an hour is possible; the limit is not known. A cluster
  of the OCP version of the scenario can be provisioned only when the `stable` channel has it (`stable-5.0` has 5.0.0 and 5.0.1).
- How the stage credentials reach the nodes of a HyperShift hosted cluster (see `add-stage-credentials`).
- Which tests of the suite pass on a hosted cluster: the default `skip_tests` is a starting point.
