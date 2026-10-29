package logging

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	libmetrics "github.com/openshift/openshift-logging-e2e-tests/internal/lib/metrics"
	"github.com/openshift/openshift-logging-e2e-tests/internal/receivers"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// waitDropFilterDiscardedEvents waits until every collector pod reports the drop filter
// transform discarded at least one event, proving historical records were actually read
// and dropped rather than never collected
func waitDropFilterDiscardedEvents(oc *exutil.CLI, token, clfName, clfNamespace string) {
	query := fmt.Sprintf(`vector_component_discarded_events_total{job=%q,namespace=%q,component_id=~".*drop.*"}`, clfName, clfNamespace)
	err := wait.PollUntilContextTimeout(context.Background(), 15*time.Second, 5*time.Minute, true, func(context.Context) (done bool, err error) {
		metrics, err := libmetrics.Get(oc, token, query)
		if err != nil {
			e2e.Logf("error querying %q, try next time: %v", query, err)
			return false, nil
		}
		for _, m := range metrics {
			if len(m.Value) != 2 {
				continue
			}
			if value, ok := m.Value[1].(string); ok {
				if count, err := strconv.ParseFloat(value, 64); err == nil && count > 0 {
					e2e.Logf("drop filter discarded %s events so far", value)
					return true, nil
				}
			}
		}
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, "the drop filter did not discard any event: "+query)
}

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease", func() {
	defer g.GinkgoRecover()
	var (
		oc             = exutil.NewCLIWithoutNamespace("logfwddropfilter")
		loggingBaseDir string
	)

	g.Context("CLF drop filter olderThan", func() {
		g.BeforeEach(func() {
			loggingBaseDir = testdata.FixturePath("logging")
			g.By("Deploy CLO")
			CLO := SubscriptionObjects{
				OperatorName:  "cluster-logging-operator",
				Namespace:     cloNS,
				PackageName:   "cluster-logging",
				Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
				OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			}
			CLO.SubscribeOperator(oc)
		})

		// author jcantril@redhat.com
		// LOG-10200 regression test for the LOG-9876 olderThan drop filter: a ClusterLogForwarder
		// created with a drop filter whose olderThan cutoff is "now" must discard the historical
		// backlog a first-time collector reads (container files, journald and audit inputs) while
		// forwarding every record written after the cutoff. The filter must keep applying after a
		// collector restart, including to records retained by checkpoints.
		g.It("Author:jcantril-Critical-86600-CLF drop filter olderThan discards historical logs and retains newer logs of all input types[Slow][CLO]", func() {
			g.By("Deploy the vector http receiver")
			oc.SetupProject()
			receiverProj := oc.Namespace()
			receiver := receivers.New(
				receivers.ReceiverTypeHttp, receiverProj,
				"vector-http-receiver-"+getRandomString(),
				oc,
			)
			defer receiver.Remove()
			receiver.Deploy()

			g.By("compute the olderThan cutoff as the current time minus a clock-skew margin")
			// the margin keeps records written around "now" on clock-skewed nodes on the
			// retained side of the cutoff; the journal/audit/container backlog still
			// guarantees plenty of strictly older records to drop. The cutoff is truncated
			// to whole seconds because the olderThan string is second precision: the filter
			// enforces the truncated instant, and comparing against the sub-second remainder
			// of time.Now() would flag records in that window the filter correctly retains.
			cutoff := time.Now().UTC().Add(-1 * time.Minute).Truncate(time.Second)
			olderThan := cutoff.Format(time.RFC3339)

			g.By("create a ClusterLogForwarder with a drop olderThan filter for all log types")
			clf := clusterlogforwarder{
				name:                      "clf-" + getRandomString(),
				namespace:                 receiverProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "drop-olderthan-86600.yaml"),
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				enableMonitoring:          true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			// the forwarder gets a unique name and namespace, so its vector checkpoint
			// directory is empty and the collector reads every source from the beginning,
			// i.e. the first-time collection scenario the filter is meant for
			clf.create(oc, "URL=http://"+receiver.Name()+"."+receiver.Namespace()+".svc:8090", "OLDER_THAN="+olderThan)

			g.By("generate application logs written after the cutoff")
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile, "-p", "RATE=1").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("wait for post-cutoff application, infrastructure-container, journal and audit records to reach the receiver")
			o.Eventually(func() error {
				return receiver.VerifyPostCutoffRecordsArrived(oc, appProj)
			}, 10*time.Minute, 30*time.Second).Should(o.Succeed())

			g.By("verify the drop filter actually discarded the historical backlog")
			promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
			waitDropFilterDiscardedEvents(oc, promToken, clf.name, clf.namespace)

			g.By("assert no record older than the cutoff reached the receiver")
			o.Consistently(func() error {
				return receiver.VerifyNoRecordsOlderThan(oc, cutoff)
			}, 3*time.Minute, 45*time.Second).Should(o.Succeed())

			g.By("restart the collector and verify the filter still applies to subsequently read records")
			recordCountBefore, err := receiver.ContainerRecordCount(oc, appProj)
			o.Expect(err).NotTo(o.HaveOccurred())
			err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pod", "-n", clf.namespace, "-l", "app.kubernetes.io/component=collector").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			clf.waitForCollectorPodsReady(oc)

			g.By("wait for records written while the collector was down to be forwarded")
			o.Eventually(func() (int, error) {
				return receiver.ContainerRecordCount(oc, appProj)
			}, 5*time.Minute, 30*time.Second).Should(o.BeNumerically(">", recordCountBefore))

			g.By("assert again that no record older than the cutoff reached the receiver")
			o.Consistently(func() error {
				return receiver.VerifyNoRecordsOlderThan(oc, cutoff)
			}, 2*time.Minute, 60*time.Second).Should(o.Succeed())
		})
	})
})
