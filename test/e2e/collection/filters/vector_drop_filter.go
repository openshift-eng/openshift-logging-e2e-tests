package filters

import (
	"path/filepath"
	"time"

	exutil "github.com/openshift/origin/test/extended/util"

	"github.com/openshift/openshift-logging-e2e-tests/internal/labels"
	librandom "github.com/openshift/openshift-logging-e2e-tests/internal/lib/random"
	"github.com/openshift/openshift-logging-e2e-tests/internal/receivers"
	"github.com/openshift/openshift-logging-e2e-tests/internal/receivers/http/vector"
	"github.com/openshift/openshift-logging-e2e-tests/internal/test/helpers"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/core/serviceaccount"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/observability"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("[sig-openshift-logging] ClusterLogForwarder drop filter",
	Label(labels.ComponentCLO, labels.ExecSlow, labels.ReleaseLogging67, labels.FilterDrop), func() {
		defer GinkgoRecover()
		var (
			oc             = exutil.NewCLIWithoutNamespace("logfwddropfilter")
			loggingBaseDir string
		)

		Context("when has olderThan", func() {
			BeforeEach(func() {
				helpers.SkipIfLoggingVersionLessThan(labels.ReleaseLogging67)
				loggingBaseDir = testdata.FixturePath("logging")
				By("Deploy CLO")
				CLO := logging.SubscriptionObjects{
					OperatorName:  observability.OperatorName,
					Namespace:     observability.NamespaceClusterLoggingOperator,
					PackageName:   observability.PackageClusterLogging,
					Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
					OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
				}
				CLO.SubscribeOperator(oc)
			})

			// Regression test for the LOG-9876 olderThan drop filter: a ClusterLogForwarder
			// created with a drop filter whose olderThan must discard the historical
			// backlog a first-time collector reads (container files, journald and audit inputs) while
			// forwarding every record written after the cutoff. The filter must keep applying after a
			// collector restart, including to records retained by checkpoints.
			It("should discards logs before the cutoff and retain newer logs of all input types[Slow][CLO]",
				Label(labels.ComponentCLO, labels.ExecSlow, labels.Jira("LOG-9876")), func() {
					oc.SetupProject()
					receiverProj := oc.Namespace()
					receiver := receivers.New(
						receivers.ReceiverTypeHttp, receiverProj,
						"vector-http-receiver-"+librandom.String(),
						oc,
					)
					defer receiver.Remove()
					receiver.Deploy()

					By("compute the olderThan cutoff as the current time minus a clock-skew margin")
					// the margin keeps records written around "now" on clock-skewed nodes on the
					// retained side of the cutoff; the journal/audit/container backlog still
					// guarantees plenty of strictly older records to drop. The cutoff is truncated
					// to whole seconds because the olderThan string is second precision: the filter
					// enforces the truncated instant, and comparing against the sub-second remainder
					// of time.Now() would flag records in that window the filter correctly retains.
					cutoff := time.Now().UTC().Add(-1 * time.Minute).Truncate(time.Second)
					olderThan := cutoff.Format(time.RFC3339)

					By("initializing a ClusterLogForwarder with a drop olderThan filter for all log types")
					clf := observability.NewClusterLogForwarder(oc, "clf-"+librandom.String(), receiverProj, func(f *observability.ClusterLogForwarder) {
						f.TemplateFile = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "drop-olderthan-log9876.yaml")
						f.CollectApplicationLogs = true
						f.CollectAuditLogs = true
						f.CollectInfrastructureLogs = true
						f.EnableMonitoring = true
						f.WaitForPodReady = true
						f.ServiceAccountName = "test-clf-" + librandom.String()
					})
					defer clf.Delete()
					// the forwarder gets a unique name and namespace, so its vector checkpoint
					// directory is empty and the collector reads every source from the beginning,
					// i.e. the first-time collection scenario the filter is meant for
					clf.Create("URL=http://"+receiver.Name()+"."+receiver.Namespace()+".svc:8090", "OLDER_THAN="+olderThan)

					By("generate application logs written after the cutoff")
					oc.SetupProject()
					appProj := oc.Namespace()
					jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
					err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile, "-p", "RATE=1").Execute()
					Expect(err).NotTo(HaveOccurred())

					By("wait for post-cutoff application, infrastructure-container, journal and audit records to reach the receiver")
					Eventually(func() error {
						return VerifyPostCutoffRecordsArrived(receiver, appProj)
					}, 5*time.Minute, 30*time.Second).Should(Succeed())

					By("verify the drop filter actually discarded the historical backlog")
					promToken := serviceaccount.Token(oc, "prometheus-k8s", "openshift-monitoring")
					waitDropFilterDiscardedEvents(oc, promToken, clf.Name, clf.Namespace)

					By("assert no record older than the cutoff reached the receiver")
					Consistently(func() error {
						return VerifyNoRecordsOlderThanCutoff(receivers.MustAs[*vector.HttpReceiver](receiver).HasOldLogs)
					}, 3*time.Minute, 45*time.Second).Should(Succeed())

					By("restart the collector and verify the filter still applies to subsequently read records")
					recordsBefore, err := receiver.ContainerRecordCount(appProj)
					Expect(err).NotTo(HaveOccurred())
					err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pod", "-n", clf.Namespace, "-l", "app.kubernetes.io/component=collector").Execute()
					Expect(err).NotTo(HaveOccurred())
					clf.WaitForCollectorPodsReady()

					By("wait for records written while the collector was down to be forwarded")
					Eventually(func() (int, error) {
						recordsAfter, err := receiver.ContainerRecordCount(appProj)
						if err != nil {
							return 0, err
						}
						return recordsAfter, nil
					}, 3*time.Minute, 30*time.Second).Should(BeNumerically(">", recordsBefore))

					By("assert again that no record older than the cutoff reached the receiver")
					Consistently(func() error {
						return VerifyNoRecordsOlderThanCutoff(receivers.MustAs[*vector.HttpReceiver](receiver).HasOldLogs)
					}, 2*time.Minute, 60*time.Second).Should(Succeed())
				})
		})
	})
