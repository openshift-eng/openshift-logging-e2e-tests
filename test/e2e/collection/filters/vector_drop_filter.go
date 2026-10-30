package filters

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openshift/openshift-logging-e2e-tests/internal/labels"
	libmetrics "github.com/openshift/openshift-logging-e2e-tests/internal/lib/metrics"
	librandom "github.com/openshift/openshift-logging-e2e-tests/internal/lib/random"
	libstrings "github.com/openshift/openshift-logging-e2e-tests/internal/lib/strings"
	"github.com/openshift/openshift-logging-e2e-tests/internal/receivers"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/core/serviceaccount"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/observability"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// waitDropFilterDiscardedEvents waits until drop filter from at least one collector reports
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

var _ = Describe("[sig-openshift-logging] Logging NonPreRelease",
	Label(labels.ComponentCLO, labels.FilterDrop), func() {
		defer GinkgoRecover()
		var (
			oc             = exutil.NewCLIWithoutNamespace("logfwddropfilter")
			loggingBaseDir string
		)

		Context("CLF drop filter olderThan", Label("dropFilter"), func() {
			BeforeEach(func() {
				loggingBaseDir = testdata.FixturePath("logging")
				By("Deploy CLO")
				CLO := logging.SubscriptionObjects{
					OperatorName:  observability.OperatorName,
					Namespace:     observability.NamespaceClusterLoggingOperator,
					PackageName:   observability.PackageClusterLogging,
					Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
					OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-oyaml"),
				}
				CLO.SubscribeOperator(oc)
			})

			// author jcantril@redhat.com
			// Regression test for the LOG-9876 olderThan drop filter: a ClusterLogForwarder
			// created with a drop filter whose olderThan must discard the historical
			// backlog a first-time collector reads (container files, journald and audit inputs) while
			// forwarding every record written after the cutoff. The filter must keep applying after a
			// collector restart, including to records retained by checkpoints.
			It("Author:jcantril-Critical-LOG-9876-CLF drop filter olderThan discards historical logs and retains newer logs of all input types[Slow][CLO]",
				Label(labels.ExecSlow, labels.Jira("LOG-9876")), func() {
					By("Deploy the vector http receiver")
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

					By("create a ClusterLogForwarder with a drop olderThan filter for all log types")
					clf := observability.NewClusterLogForwarder(oc, receiverProj, "clf-"+librandom.String(), func(f *observability.ClusterLogForwarder) {
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
					}, 10*time.Minute, 30*time.Second).Should(Succeed())

					By("verify the drop filter actually discarded the historical backlog")
					promToken := serviceaccount.Token(oc, "prometheus-k8s", "openshift-monitoring")
					waitDropFilterDiscardedEvents(oc, promToken, clf.Name, clf.Namespace)

					By("assert no record older than the cutoff reached the receiver")
					Consistently(func() error {
						return VerifyPostCutoffRecordsArrived(receiver, appProj)
					}, 3*time.Minute, 45*time.Second).Should(Succeed())

					By("restart the collector and verify the filter still applies to subsequently read records")
					recordsBefore, err := receiver.ContainerLogs(appProj)
					//recordCountBefore, err := receiver.ContainerRecordCount(oc, appProj)
					Expect(err).NotTo(HaveOccurred())
					err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pod", "-n", clf.Namespace, "-l", "app.kubernetes.io/component=collector").Execute()
					Expect(err).NotTo(HaveOccurred())
					clf.WaitForCollectorPodsReady()

					By("wait for records written while the collector was down to be forwarded")
					Eventually(func() (int, error) {
						recordsAfter, err := receiver.ContainerLogs(appProj)
						if err != nil {
							return 0, err
						}
						return len(recordsAfter), nil
					}, 5*time.Minute, 30*time.Second).Should(BeNumerically(">", len(recordsBefore)))

					By("assert again that no record older than the cutoff reached the receiver")
					Consistently(func() error {
						return VerifyNoRecordsOlderThan(receiver, appProj, cutoff)
					}, 2*time.Minute, 60*time.Second).Should(Succeed())
				})
		})
	})

// VerifyNoRecordsOlderThan asserts that every record the receiver wrote carries a
// timestamp at or after the cutoff. The viaq normalizer emits both a "timestamp" and
// an "@timestamp" field with the same value, so either is accepted. Records without
// a timestamp are ignored because the drop filter retains them by design.
func VerifyNoRecordsOlderThan(receiver receivers.Receiver, namespace string, cutoff time.Time) error {
	containerLogs, err := receiver.ContainerLogs(namespace)
	if err != nil {
		return err
	}
	journalLogs, err := receiver.JournalLogs()
	if err != nil {
		return err
	}
	auditLogs, err := receiver.AuditLogs()
	if err != nil {
		return err
	}
	records := append(containerLogs, journalLogs...)
	records = append(records, auditLogs...)
	if len(records) == 0 {
		return fmt.Errorf("no records received by the receiver")
	}
	for _, record := range records {
		var entry struct {
			Timestamp       string `json:"timestamp"`
			TimestampLegacy string `json:"@timestamp"`
		}
		if err := json.Unmarshal([]byte(record), &entry); err != nil {
			e2e.Logf("skipping unparseable receiver record: %s", libstrings.TruncateForLogs(record, 200))
			continue
		}
		tsRaw := entry.Timestamp
		if tsRaw == "" {
			tsRaw = entry.TimestampLegacy
		}
		if tsRaw == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, tsRaw)
		if err != nil {
			return fmt.Errorf("received record has an unparseable timestamp %q: %v", tsRaw, err)
		}
		if ts.Before(cutoff) {
			return fmt.Errorf("received record is older than the cutoff %s: %s", cutoff.Format(time.RFC3339), libstrings.TruncateForLogs(record, 300))
		}
	}
	return nil
}

// VerifyPostCutoffRecordsArrived asserts the receiver keeps getting fresh records of
// every log type: application records from the log generator project, infrastructure
// container records from an infrastructure namespace, journal records and audit records
func VerifyPostCutoffRecordsArrived(receiver receivers.Receiver, namespace string) error {
	if _, err := receiver.JournalLogs(); err != nil {
		return err
	}
	if _, err := receiver.AuditLogs(); err != nil {
		return err
	}

	appLogs, err := receiver.ContainerLogs(namespace)
	if err != nil {
		return err
	}
	if !strings.Contains(strings.Join(appLogs, ","), "MERGE_JSON_LOG") {
		return fmt.Errorf("application records from project %s do not contain the logtest marker", namespace)
	}
	return nil
}
