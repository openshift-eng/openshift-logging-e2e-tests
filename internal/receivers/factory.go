package receivers

import (
	"fmt"

	g "github.com/onsi/ginkgo/v2"
	exutil "github.com/openshift/origin/test/extended/util"

	"github.com/openshift/openshift-logging-e2e-tests/internal/receivers/http/vector"
)

type ReceiverType string

const ReceiverTypeHttp ReceiverType = "http"

// New returns a receiver of the given type. It panics on an unrecognized
// type: the receiver type is chosen in test code, so an unknown value is a
// programming error, not a test failure.
func New(receiverType ReceiverType, namespace, name string, oc *exutil.CLI) Receiver {
	switch receiverType {
	case ReceiverTypeHttp:
		return vector.New(namespace, name, oc)
	default:
		panic(fmt.Sprintf("Unrecognized receiver type: %v", receiverType))
	}
}

// As returns the receiver cast to its concrete implementation type T, e.g.
//
//	vrec, err := receivers.As[*vector.HttpReceiver](receiver)
//
// It errors when the receiver is not a T, e.g. because a different receiver
// type was deployed.
func As[T any](r Receiver) (T, error) {
	impl, ok := r.(T)
	if !ok {
		var want T
		return want, fmt.Errorf("receiver %q of type %T cannot be cast to %T", r.Name(), r, want)
	}
	return impl, nil
}

// MustAs returns the receiver cast to its concrete implementation type T,
// failing the test when the receiver is not a T, e.g. because a different
// receiver type was deployed.
//
//	vrec := receivers.MustAs[*vector.HttpReceiver](receiver)
func MustAs[T any](r Receiver) T {
	impl, err := As[T](r)
	if err != nil {
		g.Fail(err.Error())
	}
	return impl
}
