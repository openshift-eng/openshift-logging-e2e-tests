package helpers

import (
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/util/version"

	. "github.com/onsi/ginkgo/v2"
)

const (
	envloggingTestVersion = "LOGGING_TEST_VERSION"
)

// SkipIfLoggingVersionLessThan skips the test when LOGGING_TEST_VERSION is set to a
// version below minVersion. The env value must be a bare semver-style version
// (e.g. "6.7"); the test is not gated when the variable is unset. An optional
// format string and args replace the default skip message, e.g.
// SkipIfLoggingVersionLessThan("6.7", "drop filter requires %s+", minVersion).
func SkipIfLoggingVersionLessThan(minVersion string, args ...any) {
	v := os.Getenv(envloggingTestVersion)
	if v == "" {
		return
	}
	parsed := version.MustParseGeneric(v)
	if !parsed.LessThan(version.MustParseGeneric(minVersion)) {
		return
	}
	message := fmt.Sprintf("test requires logging %s+, LOGGING_TEST_VERSION=%s", minVersion, v)
	if len(args) > 0 {
		if format, ok := args[0].(string); ok {
			message = fmt.Sprintf(format, args[1:]...)
		}
	}
	Skip(message)
}
