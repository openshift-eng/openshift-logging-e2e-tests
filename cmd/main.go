package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/openshift-eng/openshift-tests-extension/pkg/cmd"
	e "github.com/openshift-eng/openshift-tests-extension/pkg/extension"
	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	g "github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"
	"github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"github.com/spf13/cobra"
	"k8s.io/component-base/logs"
	framework "k8s.io/kubernetes/test/e2e/framework"

	"github.com/openshift/openshift-logging-e2e-tests/internal/labels"
	"github.com/openshift/openshift-logging-e2e-tests/internal/specs"

	// Import test packages from this module
	_ "github.com/openshift/openshift-logging-e2e-tests/test/e2e"
	// Import testdata package from this module
	_ "github.com/openshift/openshift-logging-e2e-tests/test/e2e/collection/filters"
	_ "github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
)

func main() {
	// Initialize test framework flags (required for kubeconfig, provider, etc.)
	util.InitStandardFlags()
	framework.AfterReadingAllFlags(&framework.TestContext)

	logs.InitLogs()
	defer logs.FlushLogs()

	registry := e.NewRegistry()
	ext := e.NewExtension("openshift", "payload", "openshift-logging-e2e-tests")

	// Register test suites (parallel, serial, disruptive, all)
	registerSuites(ext)

	// Build test specs from Ginkgo
	// Note: ModuleTestsOnly() is applied by default, which filters out /vendor/
	// and k8s.io/kubernetes tests; module-cache specs are excluded below
	allSpecs, err := g.BuildExtensionTestSpecsFromOpenShiftGinkgoSuite()
	if err != nil {
		panic(fmt.Sprintf("couldn't build extension test specs from ginkgo: %+v", err.Error()))
	}

	// Filter to only include tests from this module's test/e2e/ directory
	// Excludes tests from /go/pkg/mod/ (module cache) and /vendor/
	componentSpecs := allSpecs.Select(specs.IsModuleSpec)

	// Initialize test framework before all tests
	componentSpecs.AddBeforeAll(func() {
		if err := compat_otp.InitTest(false); err != nil {
			panic(err)
		}
		// oc is a kubectl superset and is always present in the test runner container;
		// InitTest hardcodes KubectlPath="kubectl" which is not available here.
		framework.TestContext.KubectlPath = "oc"
		// Set testsStarted = true to allow OTP functions like oc.Run() to work
		// WithCleanup sets this flag and it remains true for all subsequent tests
		util.WithCleanup(func() {
			// Empty function - we just need WithCleanup to set testsStarted = true
		})
	})

	// Process all specs
	componentSpecs.Walk(func(spec *et.ExtensionTestSpec) {
		// Apply platform filters based on Platform: labels
		for label := range spec.Labels {
			if after, ok := strings.CutPrefix(label, "Platform:"); ok {
				platformName := after
				spec.Include(et.PlatformEquals(platformName))
			}
		}

		// Apply platform filters based on [platform:xxx] in test names
		re := regexp.MustCompile(`\[platform:([a-z]+)\]`)
		if match := re.FindStringSubmatch(spec.Name); match != nil {
			platform := match[1]
			spec.Include(et.PlatformEquals(platform))
		}

		// Set lifecycle to Informing
		spec.Lifecycle = et.LifecycleInforming
	})

	// Add filtered component specs to extension
	ext.AddSpecs(componentSpecs)

	registry.Register(ext)

	root := &cobra.Command{
		Long: "Openshift-logging-e2e-tests Tests",
	}

	root.AddCommand(cmd.DefaultExtensionCommands(registry)...)

	if err := func() error {
		return root.Execute()
	}(); err != nil {
		os.Exit(1)
	}
}

// registerSuites registers test suites with proper categorization
func registerSuites(ext *e.Extension) {
	suites := []e.Suite{
		{
			Name: "openshift-logging-e2e-tests/conformance/parallel",
			Parents: []string{
				"openshift/conformance/parallel",
			},
			Description: "Parallel conformance tests (Level0, non-serial, non-disruptive)",
			Qualifiers: []string{
				// no test carries a Level0 label yet; the bracket tag stays
				// name-based until the conformance suites are populated (LOG-10400)
				et.And(`name.contains("[Level0]")`, labels.Lacks(labels.ExecSerial, labels.ExecDisruptive)),
			},
		},
		{
			Name: "openshift-logging-e2e-tests/conformance/serial",
			Parents: []string{
				"openshift/conformance/serial",
			},
			Description: "Serial conformance tests (must run sequentially)",
			Qualifiers: []string{
				et.And(`name.contains("[Level0]")`, labels.Has(labels.ExecSerial), labels.Lacks(labels.ExecDisruptive)),
			},
		},
		{
			Name:        "openshift-logging-e2e-tests/cluster-logging-operator",
			Description: "PR-gate regression tests for cluster-logging-operator (non-disruptive)",
			Qualifiers: []string{
				et.And(labels.Has(labels.GatePreMerge, labels.ComponentCLO), labels.Lacks(labels.ExecDisruptive)),
			},
		},
		{
			Name:        "openshift-logging-e2e-tests/loki-operator",
			Description: "PR-gate regression tests for loki-operator (non-disruptive)",
			Qualifiers: []string{
				et.And(labels.Has(labels.GatePreMerge, labels.ComponentLokiOperator), labels.Lacks(labels.ExecDisruptive)),
			},
		},
		{
			Name:        "openshift-logging-e2e-tests/cluster-logging-operator/all",
			Description: "All non-disruptive cluster-logging-operator tests",
			Qualifiers: []string{
				// Longduration is an author-string marker, not a bracket tag; it stays name-based
				et.And(labels.Has(labels.ComponentCLO), labels.Lacks(labels.ExecDisruptive), `!name.contains("Longduration")`),
			},
		},
		{
			Name:        "openshift-logging-e2e-tests/loki-operator/all",
			Description: "All non-disruptive loki-operator tests",
			Qualifiers: []string{
				et.And(labels.Has(labels.ComponentLokiOperator), labels.Lacks(labels.ExecDisruptive), `!name.contains("Longduration")`),
			},
		},
		{
			Name:        "openshift-logging-e2e-tests/disruptive",
			Parents:     []string{"openshift/disruptive"},
			Description: "Disruptive tests (may affect cluster state)",
			Qualifiers: []string{
				labels.Has(labels.ExecDisruptive),
			},
		},
		{
			Name:        "openshift-logging-e2e-tests/non-disruptive",
			Description: "All non-disruptive tests (safe for development clusters)",
			Qualifiers: []string{
				labels.Lacks(labels.ExecDisruptive),
			},
		},
		{
			Name:        "openshift-logging-e2e-tests/all",
			Description: "All openshift-logging-e2e-tests tests",
		},
	}

	for _, suite := range suites {
		ext.AddSuite(suite)
	}
}
