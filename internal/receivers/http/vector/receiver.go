package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	o "github.com/onsi/gomega"
	internaltypes "github.com/openshift/openshift-logging-e2e-tests/internal/types"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/apps/deployment"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// VectorHttpReceiver is a vector based HTTP receiver. It accepts records forwarded by a
// ClusterLogForwarder http output on port 8090 and writes them to files by log type:
// container records to /tmp/container/<namespace>_<pod>_<container>.json, journal records
// to /tmp/journal/journal.json and audit records to /tmp/audit/audit.json.
// The receiver pod carries the vector.dev/exclude label so its own logs are never
// collected, which would otherwise create a feedback loop through the forwarder.
type VectorHttpReceiver struct {
	name      string // the name of the receiver, it's also used to name the cm/deployment/svc
	namespace string // the namespace where the receiver is deployed in
	Oc        *exutil.CLI
}

func New(namespace, name string, oc *exutil.CLI) *VectorHttpReceiver {
	return &VectorHttpReceiver{
		name:      name,
		namespace: namespace,
		Oc:        oc,
	}
}

func (v VectorHttpReceiver) Name() string {
	return v.name
}

func (v VectorHttpReceiver) Namespace() string {
	return v.namespace
}

func (v VectorHttpReceiver) Deploy() {
	oc := v.Oc
	filePath := []string{"logging", "external-log-stores", "vector-http-receiver"}

	cm := internaltypes.Resource{Kind: "configmap", Name: v.name, Namespace: v.namespace}
	cmFile := testdata.FixturePath(append(filePath, "configmap.yaml")...)
	err := cm.ApplyFromTemplate(oc, "-f", cmFile, "-p", "NAMESPACE="+v.namespace, "-p", "NAME="+v.name)
	o.Expect(err).NotTo(o.HaveOccurred())

	deploy := internaltypes.Resource{Kind: "deployment", Name: v.name, Namespace: v.namespace}
	deployFile := testdata.FixturePath(append(filePath, "deployment.yaml")...)
	err = deploy.ApplyFromTemplate(oc, "-f", deployFile, "-p", "NAMESPACE="+v.namespace, "-p", "NAME="+v.name)
	o.Expect(err).NotTo(o.HaveOccurred())
	deployment.WaitForPodsToBeReady(oc, v.namespace, v.name)

	err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("deployment", v.name, "-n", v.namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (v VectorHttpReceiver) Remove() {
	internaltypes.NewResource("configmap", v.name, v.namespace).Clear(v.Oc)
	internaltypes.NewResource("deployment", v.name, v.namespace).Clear(v.Oc)
	internaltypes.NewResource("svc", v.name, v.namespace).Clear(v.Oc)
}

func (v VectorHttpReceiver) PodName() string {
	pods, err := v.Oc.AdminKubeClient().CoreV1().Pods(v.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "component=" + v.name})
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(pods.Items).NotTo(o.BeEmpty())
	return pods.Items[0].Name
}

// ExecCmd runs a shell command in the receiver pod and returns its output.
// Transient exec transport failures are retried because the verification polls
// below depend on it and must not flake on a single failed roundtrip.
func (v VectorHttpReceiver) ExecCmd(cmd string) (string, error) {
	var out string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		out, err = v.Oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("exec").Args("-n", v.namespace, v.PodName(), "--", "/bin/sh", "-c", cmd).Output()
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

func (v VectorHttpReceiver) ListReceiverFiles(oc *exutil.CLI) (*receiverFiles, error) {
	cmd := `ls -1 /tmp/container 2>/dev/null; echo "JOURNAL:$(ls /tmp/journal/journal.json 2>/dev/null)"; echo "AUDIT:$(ls /tmp/audit/audit.json 2>/dev/null)"`
	out, err := v.ExecCmd(cmd)
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

// ReadAllRecords returns every complete JSON record line the receiver has written so far
func (v VectorHttpReceiver) ReadAllRecords(oc *exutil.CLI) ([]string, error) {
	// "true" keeps the exit status zero when one of the globs has no match yet
	out, err := v.ExecCmd("cat /tmp/container/*.json /tmp/journal/*.json /tmp/audit/*.json 2>/dev/null; true")
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

// VerifyNoRecordsOlderThan asserts that every record the receiver wrote carries a
// timestamp at or after the cutoff. The viaq normalizer emits both a "timestamp" and
// an "@timestamp" field with the same value, so either is accepted. Records without
// a timestamp are ignored because the drop filter retains them by design.
func (v VectorHttpReceiver) VerifyNoRecordsOlderThan(oc *exutil.CLI, cutoff time.Time) error {
	records, err := v.ReadAllRecords(oc)
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

// VerifyPostCutoffRecordsArrived asserts the receiver keeps getting fresh records of
// every log type: application records from the log generator project, infrastructure
// container records from an infrastructure namespace, journal records and audit records
func (v VectorHttpReceiver) VerifyPostCutoffRecordsArrived(oc *exutil.CLI, appProj string) error {
	files, err := v.ListReceiverFiles(oc)
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
	out, err := v.ExecCmd("cat /tmp/container/" + appFile + " 2>/dev/null")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "MERGE_JSON_LOG") {
		return fmt.Errorf("application records from project %s do not contain the logtest marker", appProj)
	}
	return nil
}

// ContainerRecordCount returns the number of records the receiver wrote for a namespace
func (v VectorHttpReceiver) ContainerRecordCount(oc *exutil.CLI, namespace string) (int, error) {
	out, err := v.ExecCmd("cat /tmp/container/" + namespace + "_*.json 2>/dev/null | wc -l")
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("can't parse record count %q: %v", out, err)
	}
	return count, nil
}
