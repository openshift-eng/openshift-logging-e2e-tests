package logging_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	g "github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"

	"github.com/openshift/openshift-logging-e2e-tests/internal/labels"
	"github.com/openshift/openshift-logging-e2e-tests/internal/specs"

	// register this module's ginkgo specs, as cmd/main.go does
	_ "github.com/openshift/openshift-logging-e2e-tests/test/e2e"
	_ "github.com/openshift/openshift-logging-e2e-tests/test/e2e/collection/filters"
)

// TestBracketTagsMatchLabels fails when a spec's bracket tags and ginkgo
// labels disagree in either direction; the contract is labels.BracketTags.
// It walks the same spec list the extension binary lists (cmd/main.go), so
// it sees exactly what label-based suite selection sees.
//
// The package's init exits early when KUBECONFIG is unset, leaving every test
// unrun but green; run via `make test`, which exports KUBECONFIG=/dev/null.
func TestBracketTagsMatchLabels(t *testing.T) {
	allSpecs, err := g.BuildExtensionTestSpecsFromOpenShiftGinkgoSuite()
	if err != nil {
		t.Fatalf("building test specs from ginkgo suite: %v", err)
	}

	specs := allSpecs.Select(specs.IsModuleSpec)
	if len(specs) == 0 {
		t.Fatal("no specs found; drift check cannot run")
	}

	tags := slices.Sorted(maps.Keys(labels.BracketTags))
	for _, spec := range specs {
		if !spec.Labels.HasAny(labels.ComponentCLO, labels.ComponentLokiOperator) {
			t.Errorf("%s:\n  no component label; add labels.ComponentCLO or "+
				"labels.ComponentLokiOperator so component-based suites can select it", spec.Name)
		}
		// TODO: drop this agreement check (and labels.BracketTags) once bracket
		// tags are decommissioned from test names. The last dependency on them
		// is origin's own suites reading [Serial]/[Disruptive] from the name.
		for _, tag := range tags {
			label := labels.BracketTags[tag]
			hasTag := strings.Contains(spec.Name, tag)
			hasLabel := spec.Labels.Has(label)
			switch {
			case hasTag && !hasLabel:
				t.Errorf("%s:\n  name has %s but spec lacks %q; bracket tags and "+
					"labels must agree (see internal/labels.BracketTags)",
					spec.Name, tag, label)
			case hasLabel && !hasTag:
				t.Errorf("%s:\n  spec has %q but name lacks %s; origin suites still "+
					"read the tag from the name", spec.Name, label, tag)
			}
		}
	}
}
