package receivers

import (
	"fmt"

	g "github.com/onsi/ginkgo/v2"
	"github.com/openshift/openshift-logging-e2e-tests/internal/receivers/http/vector"
	exutil "github.com/openshift/origin/test/extended/util"
)

type ReceiverType string

const ReceiverTypeHttp ReceiverType = "http"

// New returns a receiver of the given type
func New(receiverType ReceiverType, namespace, name string, oc *exutil.CLI) Receiver {
	switch receiverType {
	case ReceiverTypeHttp:
		return vector.New(namespace, name, oc)
	default:
		g.Fail(fmt.Sprintf("Unrecognized receiver type: %v", receiverType))
	}
	return nil
}
