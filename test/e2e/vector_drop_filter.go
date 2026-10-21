package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// vectorHttpReceiver is a vector based HTTP receiver. It accepts records forwarded by a
// ClusterLogForwarder http output on port 8090 and writes them to files by log type:
// container records to /tmp/container/<namespace>_<pod>_<container>.json, journal records
// to /tmp/journal/journal.json and audit records to /tmp/audit/audit.json.
// The receiver pod carries the vector.dev/exclude label so its own logs are never
// collected, which would otherwise create a feedback loop through the forwarder.
type vectorHttpReceiver struct {
	name      string // the name of the receiver, it's also used to name the cm/deployment/svc
	namespace string // the namespace where the receiver is deployed in
}

func (v vectorHttpReceiver) deploy(oc *exutil.CLI) {
	filePath := []string{"logging", "external-log-stores", "vector-http-receiver"}

	cm := resource{"configmap", v.name, v.namespace}
	cmFile := testdata.FixturePath(append(filePath, "configmap.yaml")...)
	err := cm.applyFromTemplate(oc, "-f", cmFile, "-p", "NAMESPACE="+v.namespace, "-p", "NAME="+v.name)
	o.Expect(err).NotTo(o.HaveOccurred())

	deploy := resource{"deployment", v.name, v.namespace}
	deployFile := testdata.FixturePath(append(filePath, "deployment.yaml")...)
	err = deploy.applyFromTemplate(oc, "-f", deployFile, "-p", "NAMESPACE="+v.namespace, "-p", "NAME="+v.name)
	o.Expect(err).NotTo(o.HaveOccurred())
	WaitForDeploymentPodsToBeReady(oc, v.namespace, v.name)

	err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("deployment", v.name, "-n", v.namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (v vectorHttpReceiver) remove(oc *exutil.CLI) {
	resource{"configmap", v.name, v.namespace}.clear(oc)
	resource{"deployment", v.name, v.namespace}.clear(oc)
	resource{"svc", v.name, v.namespace}.clear(oc)
}

func (v vectorHttpReceiver) getPodName(oc *exutil.CLI) string {
	pods, err := oc.AdminKubeClient().CoreV1().Pods(v.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "component=" + v.name})
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(pods.Items).NotTo(o.BeEmpty())
	return pods.Items[0].Name
}

// execCmd runs a shell command in the receiver pod and returns its output.
// Transient exec transport failures are retried because the verification polls
// below depend on it and must not flake on a single failed roundtrip.
func (v vectorHttpReceiver) execCmd(oc *exutil.CLI, cmd string) (string, error) {
	var out string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		out, err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("exec").Args("-n", v.namespace, v.getPodName(oc), "--", "/bin/sh", "-c", cmd).Output()
		if err == nil {
			return out, nil
		}
		e2e.Logf("exec %q in the receiver pod failed (attempt %d): %v", cmd, attempt+1, err)
		time.Sleep(5 * time.Second)
	}
	return out, err
}

// receiverFiles is a snapshot of the files the receiver has written so far
type receiverFiles struct {
	containerFiles []string
	journal        bool
	audit          bool
}

func (v vectorHttpReceiver) listReceiverFiles(oc *exutil.CLI) (*receiverFiles, error) {
	cmd := `ls -1 /tmp/container 2>/dev/null; echo "JOURNAL:$(ls /tmp/journal/journal.json 2>/dev/null)"; echo "AUDIT:$(ls /tmp/audit/audit.json 2>/dev/null)"`
	out, err := v.execCmd(oc, cmd)
	if err != nil {
		return nil, err
	}
	files := &receiverFiles{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "JOURNAL:"):
			files.journal = strings.TrimPrefix(line, "JOURNAL:") != ""
		case strings.HasPrefix(line, "AUDIT:"):
			files.audit = strings.TrimPrefix(line, "AUDIT:") != ""
		case strings.HasSuffix(line, ".json"):
			files.containerFiles = append(files.containerFiles, line)
		}
	}
	return files, nil
}

// readAllRecords returns every complete JSON record line the receiver has written so far
func (v vectorHttpReceiver) readAllRecords(oc *exutil.CLI) ([]string, error) {
	// "true" keeps the exit status zero when one of the globs has no match yet
	out, err := v.execCmd(oc, "cat /tmp/container/*.json /tmp/journal/*.json /tmp/audit/*.json 2>/dev/null; true")
	if err != nil {
		return nil, err
	}
	var records []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// incomplete lines are torn writes of records appended while cat runs
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			records = append(records, line)
		}
	}
	return records, nil
}

// truncateForLog keeps failure messages readable
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// verifyNoRecordsOlderThan asserts that every record the receiver wrote carries a
// timestamp at or after the cutoff. The viaq normalizer emits both a "timestamp" and
// an "@timestamp" field with the same value, so either is accepted. Records without
// a timestamp are ignored because the drop filter retains them by design.
func (v vectorHttpReceiver) verifyNoRecordsOlderThan(oc *exutil.CLI, cutoff time.Time) error {
	records, err := v.readAllRecords(oc)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return fmt.Errorf("no records received by the receiver")
	}
	for _, record := range records {
		var entry struct {
			Timestamp       string `json:"timestamp"`
			TimestampLegacy string `json:"@timestamp"`
		}
		if err := json.Unmarshal([]byte(record), &entry); err != nil {
			e2e.Logf("skipping unparseable receiver record: %s", truncateForLog(record, 200))
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
			return fmt.Errorf("received record is older than the cutoff %s: %s", cutoff.Format(time.RFC3339), truncateForLog(record, 300))
		}
	}
	return nil
}

// verifyPostCutoffRecordsArrived asserts the receiver keeps getting fresh records of
// every log type: application records from the log generator project, infrastructure
// container records from an infrastructure namespace, journal records and audit records
func (v vectorHttpReceiver) verifyPostCutoffRecordsArrived(oc *exutil.CLI, appProj string) error {
	files, err := v.listReceiverFiles(oc)
	if err != nil {
		return err
	}
	var appFile string
	infraSeen := false
	for _, file := range files.containerFiles {
		namespace := strings.SplitN(file, "_", 2)[0]
		if namespace == appProj {
			appFile = file
		}
		if strings.HasPrefix(namespace, "openshift-") || strings.HasPrefix(namespace, "kube-") || namespace == "default" {
			infraSeen = true
		}
	}
	if appFile == "" {
		return fmt.Errorf("no application records from project %s received", appProj)
	}
	if !infraSeen {
		return fmt.Errorf("no infrastructure container records received")
	}
	if !files.journal {
		return fmt.Errorf("no journal records received")
	}
	if !files.audit {
		return fmt.Errorf("no audit records received")
	}
	out, err := v.execCmd(oc, "cat /tmp/container/"+appFile+" 2>/dev/null")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "MERGE_JSON_LOG") {
		return fmt.Errorf("application records from project %s do not contain the logtest marker", appProj)
	}
	return nil
}

// containerRecordCount returns the number of records the receiver wrote for a namespace
func (v vectorHttpReceiver) containerRecordCount(oc *exutil.CLI, namespace string) (int, error) {
	out, err := v.execCmd(oc, "cat /tmp/container/"+namespace+"_*.json 2>/dev/null | wc -l")
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("can't parse record count %q: %v", out, err)
	}
	return count, nil
}

// waitDropFilterDiscardedEvents waits until every collector pod reports the drop filter
// transform discarded at least one event, proving historical records were actually read
// and dropped rather than never collected
func waitDropFilterDiscardedEvents(oc *exutil.CLI, token, clfName, clfNamespace string) {
	query := fmt.Sprintf(`vector_component_discarded_events_total{job=%q,namespace=%q,component_id=~".*drop.*"}`, clfName, clfNamespace)
	err := wait.PollUntilContextTimeout(context.Background(), 15*time.Second, 5*time.Minute, true, func(context.Context) (done bool, err error) {
		metrics, err := getMetric(oc, token, query)
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
			g.By("deploy CLO")
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
			g.By("deploy the vector http receiver")
			oc.SetupProject()
			receiverProj := oc.Namespace()
			receiver := vectorHttpReceiver{
				name:      "vector-http-receiver-" + getRandomString(),
				namespace: receiverProj,
			}
			defer receiver.remove(oc)
			receiver.deploy(oc)

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
			clf.create(oc, "URL=http://"+receiver.name+"."+receiver.namespace+".svc:8090", "OLDER_THAN="+olderThan)

			g.By("generate application logs written after the cutoff")
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile, "-p", "RATE=1").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("wait for post-cutoff application, infrastructure-container, journal and audit records to reach the receiver")
			o.Eventually(func() error {
				return receiver.verifyPostCutoffRecordsArrived(oc, appProj)
			}, 10*time.Minute, 30*time.Second).Should(o.Succeed())

			g.By("verify the drop filter actually discarded the historical backlog")
			promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
			waitDropFilterDiscardedEvents(oc, promToken, clf.name, clf.namespace)

			g.By("assert no record older than the cutoff reached the receiver")
			o.Consistently(func() error {
				return receiver.verifyNoRecordsOlderThan(oc, cutoff)
			}, 3*time.Minute, 45*time.Second).Should(o.Succeed())

			g.By("restart the collector and verify the filter still applies to subsequently read records")
			recordCountBefore, err := receiver.containerRecordCount(oc, appProj)
			o.Expect(err).NotTo(o.HaveOccurred())
			err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pod", "-n", clf.namespace, "-l", "app.kubernetes.io/component=collector").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			clf.waitForCollectorPodsReady(oc)

			g.By("wait for records written while the collector was down to be forwarded")
			o.Eventually(func() (int, error) {
				return receiver.containerRecordCount(oc, appProj)
			}, 5*time.Minute, 30*time.Second).Should(o.BeNumerically(">", recordCountBefore))

			g.By("assert again that no record older than the cutoff reached the receiver")
			o.Consistently(func() error {
				return receiver.verifyNoRecordsOlderThan(oc, cutoff)
			}, 2*time.Minute, 60*time.Second).Should(o.Succeed())
		})
	})
})
