package vector

import (
	"context"
	"fmt"
	"path"
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

const (
	PathContainer = "/tmp/container"
)

// HttpReceiver is a vector based HTTP receiver. It accepts records forwarded by a
// ClusterLogForwarder http output on port 8090 and writes them to files by log type:
// container records to /tmp/container/<namespace>_<pod>_<container>.json, journal records
// to /tmp/journal/journal.json and audit records to /tmp/audit/audit.json.
// The receiver pod carries the vector.dev/exclude label so its own logs are never
// collected, which would otherwise create a feedback loop through the forwarder.
type HttpReceiver struct {
	name      string // the name of the receiver, it's also used to name the cm/deployment/svc
	namespace string // the namespace where the receiver is deployed in
	Oc        *exutil.CLI
}

func New(namespace, name string, oc *exutil.CLI) *HttpReceiver {
	return &HttpReceiver{
		name:      name,
		namespace: namespace,
		Oc:        oc,
	}
}

func (v HttpReceiver) Name() string {
	return v.name
}

func (v HttpReceiver) Namespace() string {
	return v.namespace
}

func (v HttpReceiver) Deploy() {
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

func (v HttpReceiver) Remove() {
	internaltypes.NewResource("configmap", v.name, v.namespace).Clear(v.Oc)
	internaltypes.NewResource("deployment", v.name, v.namespace).Clear(v.Oc)
	internaltypes.NewResource("svc", v.name, v.namespace).Clear(v.Oc)
}

func (v HttpReceiver) PodName() string {
	pods, err := v.Oc.AdminKubeClient().CoreV1().Pods(v.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "component=" + v.name})
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(pods.Items).NotTo(o.BeEmpty())
	return pods.Items[0].Name
}

// ExecCmd runs a shell command in the receiver pod and returns its output.
// Transient exec transport failures are retried because the verification polls
// below depend on it and must not flake on a single failed roundtrip.
func (v HttpReceiver) ExecCmd(cmd string) (string, error) {
	var out string
	var stderr string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
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

func (v HttpReceiver) catFile(file string) ([]string, error) {
	out, err := v.ExecCmd(fmt.Sprintf("cat %s", file))
	if err != nil {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

func (v HttpReceiver) AuditLogs() ([]string, error) {
	return v.catFile("/tmp/audit/audit.json")
}

func (v HttpReceiver) JournalLogs() ([]string, error) {
	return v.catFile("/tmp/journal/journal.json")
}

func (v HttpReceiver) ContainerLogs(namespace string) ([]string, error) {
	files, err := v.listReceiverFiles()
	if err != nil {
		return nil, err
	}
	for _, file := range files.containerFiles {
		fileNamespace := strings.SplitN(file, "_", 2)[0]
		if namespace == fileNamespace {
			return v.catFile(path.Join(PathContainer, file))
		}
	}
	return nil, fmt.Errorf("no application records from namespace %s received", namespace)
}

// receiverFiles is a snapshot of the files the receiver has written so far
type receiverFiles struct {
	containerFiles []string
	journal        bool
	audit          bool
}

func (v HttpReceiver) listReceiverFiles() (*receiverFiles, error) {
	cmd := `ls -1 /tmp/container; echo "JOURNAL:$(ls /tmp/journal/journal.json)"; echo "AUDIT:$(ls /tmp/audit/audit.json)"`
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
