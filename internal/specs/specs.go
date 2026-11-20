// Package specs identifies which OTE test specs belong to this module.
package specs

import (
	"strings"

	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
)

// IsModuleSpec reports whether spec is defined inside this module's test/e2e/
// directory. Module-cache and vendored sources are excluded: importing the
// origin test framework transitively registers thousands of upstream
// kubernetes e2e specs that must not ship with this extension.
//
// BuildExtensionTestSpecsFromOpenShiftGinkgoSuite applies ModuleTestsOnly by
// default, which drops /vendor/ and k8s.io/kubernetes specs but NOT specs
// from the module cache; this filter excludes those.
//
// cmd/main.go uses it to pick the specs the binary lists, and the drift test
// in test/e2e uses it to check the same set, so the two cannot diverge.
func IsModuleSpec(spec *et.ExtensionTestSpec) bool {
	for _, loc := range spec.CodeLocations {
		if strings.Contains(loc, "/test/e2e/") && !strings.Contains(loc, "/go/pkg/mod/") && !strings.Contains(loc, "/vendor/") {
			return true
		}
	}
	return false
}
