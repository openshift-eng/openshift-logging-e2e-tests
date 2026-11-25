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

// LoggingVersionAtLeast reports whether LOGGING_TEST_VERSION is at least minVersion.
// The env value must be a bare semver-style version (e.g. "6.7"). When the variable
// is unset the version is treated as the latest, so it returns true.
func LoggingVersionAtLeast(minVersion string) bool {
	v := os.Getenv(envloggingTestVersion)
	if v == "" {
		return true
	}
	return !version.MustParseGeneric(v).LessThan(version.MustParseGeneric(minVersion))
}

// SkipIfLoggingVersionLessThan skips the test when LOGGING_TEST_VERSION is set to a
// version below minVersion. The test is not gated when the variable is unset. An
// optional format string and args replace the default skip message, e.g.
// SkipIfLoggingVersionLessThan("6.7", "drop filter requires %s+", minVersion).
func SkipIfLoggingVersionLessThan(minVersion string, args ...any) {
	if LoggingVersionAtLeast(minVersion) {
		return
	}
	message := fmt.Sprintf("test requires logging %s+, LOGGING_TEST_VERSION=%s", minVersion, os.Getenv(envloggingTestVersion))
	if len(args) > 0 {
		if format, ok := args[0].(string); ok {
			message = fmt.Sprintf(format, args[1:]...)
		}
	}
	Skip(message)
}
