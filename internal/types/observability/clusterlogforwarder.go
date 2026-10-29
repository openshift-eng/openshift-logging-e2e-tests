package observability

import (
	"context"
	"fmt"
	"os"

	o "github.com/onsi/gomega"
	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/metrics"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/apps/daemonset"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/core/serviceaccount"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/openshift"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/compat_otp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

type ClusterLogForwarder struct {
	CollectApplicationLogs    bool // optional, if true, will add cluster-role/collect-application-logs to the serviceAccount
	CollectAuditLogs          bool // optional, if true, will add cluster-role/collect-audit-logs to the serviceAccount
	CollectInfrastructureLogs bool // optional, if true, will add cluster-role/collect-infrastructure-logs to the serviceAccount
	EnableMonitoring          bool // optional, if true, will add label `openshift.io/cluster-monitoring: "true"` to the project, and create role/prometheus-k8s rolebinding/prometheus-k8s in the namespace, works when when !(clf.namespace == "openshift-operators-redhat" || clf.namespace == "openshift-logging")
	ServiceAccountName        string
	TemplateFile              string // the template used to create clusterlogforwarder, no default value
	SecretName                string // optional, if it's specified, when creating CLF, the parameter `"SECRET_NAME="+clf.secretName` will be added automatically
	WaitForPodReady           bool   // optional, if true, will check daemonset stats
	Oc                        *exutil.CLI
	types.Resource
}

func NewClusterLogForwarder(oc *exutil.CLI, namespace, name string, inits ...func(*ClusterLogForwarder)) *ClusterLogForwarder {
	clf := &ClusterLogForwarder{
		Oc:       oc,
		Resource: types.NewResource(ApiGroupClusterLogForwarder, name, namespace),
	}
	for _, f := range inits {
		f(clf)
	}
	return clf
}

// Create ClusterLogForwarder CR from a template
func (clf *ClusterLogForwarder) Create(optionalParameters ...string) {
	oc := clf.Oc
	//parameters := []string{"-f", clf.templateFile, "--ignore-unknown-parameters=true", "-p", "NAME=" + clf.name, "NAMESPACE=" + clf.namespace}
	if clf.Namespace != "openshift-logging" {
		compat_otp.SetNamespacePrivileged(oc, clf.Namespace)
	}

	if clf.Namespace != NamespaceClusterLoggingOperator && clf.Namespace != NamespaceLokiOperator && clf.EnableMonitoring {
		metrics.EnableClusterMonitoring(oc, clf.Namespace)
	}

	parameters := []string{"-n", clf.Namespace, "-f", clf.TemplateFile, "-p", "NAME=" + clf.Name, "NAMESPACE=" + clf.Namespace}
	if clf.SecretName != "" {
		parameters = append(parameters, "SECRET_NAME="+clf.SecretName)
	}

	if clf.ServiceAccountName != "" {
		clf.CreateServiceAccount()
		parameters = append(parameters, "SERVICE_ACCOUNT_NAME="+clf.ServiceAccountName)
	}

	if len(optionalParameters) > 0 {
		parameters = append(parameters, optionalParameters...)
	}

	file, processErr := openshift.ProcessTemplate(oc, parameters...)
	defer os.Remove(file)
	if processErr != nil {
		e2e.Failf("error processing file: %v", processErr)
	}
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args("-f", file, "-n", clf.Namespace).Execute()
	if err != nil {
		e2e.Failf("error creating clusterlogforwarder: %v", err)
	}
	clf.WaitForResourceToAppear(clf.Oc)
	if clf.WaitForPodReady {
		clf.WaitForCollectorPodsReady()
	}
}

func (clf *ClusterLogForwarder) WaitForCollectorPodsReady() {
	daemonset.WaitForPodsToBeReady(clf.Oc, clf.Namespace, clf.Name)
}

// CreateServiceAccount creates the serviceaccount and add the required clusterroles to the serviceaccount
func (clf *ClusterLogForwarder) CreateServiceAccount() {
	oc := clf.Oc
	_, err := oc.AdminKubeClient().CoreV1().ServiceAccounts(clf.Namespace).Get(context.Background(), clf.ServiceAccountName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		err = serviceaccount.Create(clf.Oc, clf.Namespace, clf.ServiceAccountName)
		if err != nil {
			e2e.Failf("can't create the serviceaccount: %v", err)
		}
	}
	if clf.CollectApplicationLogs {
		err = serviceaccount.AddClusterRole(oc, clf.Namespace, clf.ServiceAccountName, ClusterRoleCollectApplicationLogs)
		o.Expect(err).NotTo(o.HaveOccurred())
	}
	if clf.CollectInfrastructureLogs {
		err = serviceaccount.AddClusterRole(oc, clf.Namespace, clf.ServiceAccountName, ClusterRoleCollectInfrastructureLogs)
		o.Expect(err).NotTo(o.HaveOccurred())
	}
	if clf.CollectAuditLogs {
		err = serviceaccount.AddClusterRole(oc, clf.Namespace, clf.ServiceAccountName, ClusterRoleCollectAuditLogs)
		o.Expect(err).NotTo(o.HaveOccurred())
	}
}

// Delete the ClusterLogForwarder CR
func (clf *ClusterLogForwarder) Delete() {
	err := clf.Resource.Clear(clf.Oc)
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("clusterlogforwarder/%s in project/%s is not deleted", clf.Name, clf.Namespace))
	if clf.ServiceAccountName != "" {
		if clf.CollectApplicationLogs {
			serviceaccount.RemoveClusterRole(clf.Oc, clf.Namespace, clf.SecretName, ClusterRoleCollectApplicationLogs)
		}
		if clf.CollectInfrastructureLogs {
			serviceaccount.RemoveClusterRole(clf.Oc, clf.Namespace, clf.SecretName, ClusterRoleCollectInfrastructureLogs)
		}
		if clf.CollectAuditLogs {
			serviceaccount.RemoveClusterRole(clf.Oc, clf.Namespace, clf.SecretName, ClusterRoleCollectAuditLogs)

		}
		types.NewResource(serviceaccount.Kind, clf.ServiceAccountName, clf.Namespace).Clear(clf.Oc)
	}

	err = types.NewResource(daemonset.Kind, clf.Name, clf.Namespace).WaitUntilResourceIsGone(clf.Oc)
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("daemonset/%s in project/%s is not deleted", clf.Resource.Name, clf.Namespace))

}
