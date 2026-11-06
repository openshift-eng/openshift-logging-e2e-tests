package receivers

import (
	exutil "github.com/openshift/origin/test/extended/util"

	"github.com/openshift/openshift-logging-e2e-tests/internal/receivers/http/vector"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Receiver factory", func() {
	var oc *exutil.CLI

	BeforeEach(func() {
		// none of the factory paths touch the cluster; a nil CLI is enough
		// to construct the receiver value
		oc = nil
	})

	Describe("New", func() {
		It("returns the http implementation for ReceiverTypeHttp", func() {
			receiver := New(ReceiverTypeHttp, "ns", "http-rec", oc)
			Expect(receiver).NotTo(BeNil())
			Expect(receiver.Name()).To(Equal("http-rec"))
			Expect(receiver.Namespace()).To(Equal("ns"))
		})

		It("fails for an unrecognized receiver type", func() {
			Expect(func() { New(ReceiverType("nosuchtype"), "ns", "rec", oc) }).To(Panic())
		})
	})

	Describe("As", func() {
		It("casts a receiver to its concrete implementation", func() {
			receiver := New(ReceiverTypeHttp, "ns", "as-rec", oc)
			impl, err := As[*vector.HttpReceiver](receiver)
			Expect(err).NotTo(HaveOccurred())
			Expect(impl).To(Equal(receiver))
			Expect(impl.Name()).To(Equal("as-rec"))
		})

		It("errors when the receiver is not the requested type", func() {
			receiver := New(ReceiverTypeHttp, "ns", "as-rec", oc)
			_, err := As[string](receiver)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot be cast to"))
		})

		It("MustAs returns the impl on success", func() {
			receiver := New(ReceiverTypeHttp, "ns", "must-rec", oc)
			impl := MustAs[*vector.HttpReceiver](receiver)
			Expect(impl).To(Equal(receiver))
		})
	})
})
