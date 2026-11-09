package helpers

import (
	"os"

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
