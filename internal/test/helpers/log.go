package helpers

import (
	"strings"

	e2e "k8s.io/kubernetes/test/e2e/framework"
)

func LogOnError(err error, message ...string) {
	if err != nil {
		format := "%v"
		if len(message) > 0 {
			format = strings.Join(message, " ") + ":" + format
		}
		e2e.Logf(format, err)
	}
}
