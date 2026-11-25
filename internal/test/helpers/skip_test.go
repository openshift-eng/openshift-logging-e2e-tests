package helpers

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("SkipIfLoggingVersionLessThan", func() {
	const minVersion = "6.7"

	BeforeEach(func() {
		DeferCleanup(os.Unsetenv, envloggingTestVersion)
	})

	DescribeTable("version comparison against minimum 6.7",
		func(envValue string, expectSkip bool) {
			os.Setenv(envloggingTestVersion, envValue)
			if expectSkip {
				Expect(func() { SkipIfLoggingVersionLessThan(minVersion) }).To(Panic())
			} else {
				Expect(func() { SkipIfLoggingVersionLessThan(minVersion) }).NotTo(Panic())
			}
		},
		Entry("equal version is not skipped", "6.7", false),
		Entry("older version is skipped", "6.5", true),
	)
})

func TestLoggingVersionAtLeast(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		isSet bool
		want  bool
	}{
		{name: "equal version", env: "6.7", isSet: true, want: true},
		{name: "newer version", env: "6.8", isSet: true, want: true},
		{name: "older version", env: "6.6", isSet: true, want: false},
		{name: "unset is treated as latest", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv restores the original value; unset the variable for the "unset" case.
			t.Setenv(envloggingTestVersion, tt.env)
			if !tt.isSet {
				os.Unsetenv(envloggingTestVersion)
			}
			if got := LoggingVersionAtLeast("6.7"); got != tt.want {
				t.Errorf("LoggingVersionAtLeast(\"6.7\") with %s=%q = %v, want %v", envloggingTestVersion, tt.env, got, tt.want)
			}
		})
	}
}
