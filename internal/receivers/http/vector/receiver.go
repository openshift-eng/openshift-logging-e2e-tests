package vector

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	internaltypes "github.com/openshift/openshift-logging-e2e-tests/internal/types"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/apps/deployment"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"

	. "github.com/onsi/ginkgo/v2"
)

const (
	// PathTmp is the receiver pod directory holding all sink files.
	PathTmp = "/tmp"
)

// HttpReceiver is a vector based HTTP receiver. It accepts records forwarded by a
// ClusterLogForwarder http output on port 8090 and writes them to files in /tmp by
// log type: container records to /tmp/<namespace>_<pod>_<container>.json, journal
// records to /tmp/journal.json and audit records to /tmp/audit.json.
// When the OLDER_THAN template parameter is passed to Deploy (an RFC3339 instant),
// records whose timestamp is older than the cutoff are instead written to
// /tmp/old.json and excluded from the per-log-type files.
// The receiver pod carries the vector.dev/exclude label so its own logs are never
// collected, which would otherwise create a feedback loop through the forwarder.
type HttpReceiver struct {
	name      string // the name of the receiver, it's also used to name the cm/deployment/svc
	namespace string // the namespace where the receiver is deployed in
	Oc        *exutil.CLI
	podName   string
}

func New(namespace, name string, oc *exutil.CLI) *HttpReceiver {
	return &HttpReceiver{
		name:      name,
		namespace: namespace,
		Oc:        oc,
	}
}

func (v *HttpReceiver) Name() string {
	return v.name
}

func (v *HttpReceiver) Namespace() string {
	return v.namespace
}

// Deploy deploys the receiver. Optional template parameters override the
// configmap template defaults, e.g. Deploy("OLDER_THAN=2026-11-03T13:00:00Z")
// routes records with an older timestamp to /tmp/old.json.
func (v *HttpReceiver) Deploy(optionalParameters ...string) {
	By("deploy the http receiver")
	oc := v.Oc
	filePath := []string{"logging", "external-log-stores", "vector-http-receiver"}

	cm := internaltypes.Resource{Kind: "configmap", Name: v.name, Namespace: v.namespace}
	cmFile := testdata.FixturePath(append(filePath, "configmap.yaml")...)
	cmParams := append([]string{"-f", cmFile, "-p", "NAMESPACE=" + v.namespace, "-p", "NAME=" + v.name}, optionalParameters...)
	err := cm.ApplyFromTemplate(oc, cmParams...)
	o.Expect(err).NotTo(o.HaveOccurred())

	deploy := internaltypes.Resource{Kind: "deployment", Name: v.name, Namespace: v.namespace}
	deployFile := testdata.FixturePath(append(filePath, "deployment.yaml")...)
	err = deploy.ApplyFromTemplate(oc, "-f", deployFile, "-p", "NAMESPACE="+v.namespace, "-p", "NAME="+v.name)
	o.Expect(err).NotTo(o.HaveOccurred())
	deployment.WaitForPodsToBeReady(oc, v.namespace, v.name)

	pods, err := v.Oc.AdminKubeClient().CoreV1().Pods(v.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "component=" + v.name})
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(pods.Items).NotTo(o.BeEmpty())
	v.podName = pods.Items[0].Name

	err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("deployment", v.name, "-n", v.namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (v *HttpReceiver) Remove() {
	_ = internaltypes.NewResource("configmap", v.name, v.namespace).Clear(v.Oc)
	_ = internaltypes.NewResource("deployment", v.name, v.namespace).Clear(v.Oc)
	_ = internaltypes.NewResource("svc", v.name, v.namespace).Clear(v.Oc)
}

func (v *HttpReceiver) PodName() string {
	return v.podName
}

// ExecCmd runs a shell command in the receiver pod and returns its output.
// Transient exec transport failures are retried because the verification polls
// below depend on it and must not flake on a single failed roundtrip.
func (v *HttpReceiver) ExecCmd(cmd string) (string, error) {
	var out string
	var stderr string
	var err error
	for attempt := range 3 {
		out, stderr, err = v.Oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("exec").Args("-n", v.namespace, v.PodName(), "--", "/bin/sh", "-c", cmd).Outputs()
		if err == nil {
			return out, nil
		}
		e2e.Logf("exec %q in the receiver pod failed (attempt %d): %v, stderr: %s", cmd, attempt+1, err, stderr)
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		return stderr, err
	}
	return out, err
}

func (v *HttpReceiver) catFile(file string) ([]string, error) {
	out, err := v.ExecCmd(fmt.Sprintf("cat %s", file))
	if err != nil {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

func (v *HttpReceiver) AuditLogs() ([]string, error) {
	return v.catFile("/tmp/audit.json")
}

// OldLogs retrieves the records routed to the separate old-timestamp file.
// The file only exists when the OLDER_THAN template parameter was passed to
// Deploy and at least one record with an older timestamp has been received.
func (v *HttpReceiver) OldLogs() ([]string, error) {
	return v.catFile("/tmp/old.json")
}

// HasOldLogs determines if records were routed to the separate old-timestamp file.
// The file only exists when the OLDER_THAN template parameter was passed to
// Deploy and at least one record with an older timestamp has been received.
func (v *HttpReceiver) HasOldLogs() (bool, error) {
	return v.hasFile("/tmp/old.json")
}

// hasFile reports whether the given file exists in the receiver pod and is
// non-empty, without reading its content. It runs a single exec (no retry) so
// it is cheap to poll: the shell always exits 0 and prints the result.
func (v *HttpReceiver) hasFile(file string) (bool, error) {
	cmd := fmt.Sprintf("test -s %s && echo present || echo absent", file)
	out, _, err := v.Oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("exec").Args(
		"-n", v.namespace, v.PodName(), "--", "/bin/sh", "-c", cmd,
	).Outputs()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "present", nil
}

// HasJournalLogs reports whether the journal file exists and is non-empty,
// without reading its content. Cheap to poll.
func (v *HttpReceiver) HasJournalLogs() (bool, error) {
	return v.hasFile("/tmp/journal.json")
}

// HasAuditLogs reports whether the audit file exists and is non-empty,
// without reading its content. Cheap to poll.
func (v *HttpReceiver) HasAuditLogs() (bool, error) {
	return v.hasFile("/tmp/audit.json")
}

func (v *HttpReceiver) JournalLogs() ([]string, error) {
	return v.catFile("/tmp/journal.json")
}

func (v *HttpReceiver) ContainerLogs(namespace string) ([]string, error) {
	file, err := v.containerFileFor(namespace)
	if err != nil {
		return nil, err
	}
	return v.catFile(file)
}

// ContainerRecordCount returns the number of records received from the given
// namespace. The count runs inside the receiver pod (wc -l on the container
// file), so the file content is never transferred. It runs a single exec (no
// retry), making it cheap to poll.
func (v *HttpReceiver) ContainerRecordCount(namespace string) (int, error) {
	file, err := v.containerFileFor(namespace)
	if err != nil {
		return 0, err
	}
	cmd := fmt.Sprintf("wc -l < %s", file)
	out, _, err := v.Oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("exec").Args(
		"-n", v.namespace, v.PodName(), "--", "/bin/sh", "-c", cmd,
	).Outputs()
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parsing record count %q for namespace %s: %w", out, namespace, err)
	}
	return count, nil
}

// HasContainerLogsWith reports whether any record received from the given
// namespace matches the regex pattern. The regex is a POSIX extended regular
// expression (ERE), interpreted by the receiver pod's grep -E. The match runs
// inside the receiver pod, so the file content is never transferred. It runs a
// single exec (no retry): the shell always exits 0 and prints the result.
func (v *HttpReceiver) HasContainerLogsWith(namespace, regexPattern string) (bool, error) {
	file, err := v.containerFileFor(namespace)
	if err != nil {
		return false, err
	}
	cmd := fmt.Sprintf("grep -Eq '%s' %s && echo present || echo absent", strings.ReplaceAll(regexPattern, "'", `'\''`), file)
	out, _, err := v.Oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("exec").Args(
		"-n", v.namespace, v.PodName(), "--", "/bin/sh", "-c", cmd,
	).Outputs()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "present", nil
}

// containerFileFor returns the receiver-side container log file path for the
// given namespace, or an error when no record from that namespace has been
// received yet.
func (v *HttpReceiver) containerFileFor(namespace string) (string, error) {
	files, err := v.listReceiverFiles()
	if err != nil {
		return "", err
	}
	for _, file := range files.containerFiles {
		fileNamespace, _, _ := strings.Cut(file, "_")
		if namespace == fileNamespace {
			return path.Join(PathTmp, file), nil
		}
	}
	return "", fmt.Errorf("no application records from namespace %s received", namespace)
}

// receiverFiles is a snapshot of the files the receiver has written so far
type receiverFiles struct {
	containerFiles []string
	journal        bool
	audit          bool
}

func (v *HttpReceiver) listReceiverFiles() (*receiverFiles, error) {
	// all sink files live in /tmp; container files are named <ns>_<pod>_<container>.json
	cmd := `ls -1 /tmp/*.json 2>/dev/null | sed 's|.*/||'; echo "JOURNAL:$(ls /tmp/journal.json)"; echo "AUDIT:$(ls /tmp/audit.json)"`
	out, err := v.ExecCmd(cmd)
	if err != nil {
		return nil, err
	}
	files := &receiverFiles{}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "JOURNAL:"):
			files.journal = strings.TrimPrefix(line, "JOURNAL:") != ""
		case strings.HasPrefix(line, "AUDIT:"):
			files.audit = strings.TrimPrefix(line, "AUDIT:") != ""
		case strings.HasSuffix(line, ".json") && line != "journal.json" && line != "audit.json" && line != "old.json":
			files.containerFiles = append(files.containerFiles, line)
		}
	}
	return files, nil
}
