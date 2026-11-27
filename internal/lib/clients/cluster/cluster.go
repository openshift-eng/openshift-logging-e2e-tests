package cluster

import (
	"context"
	"strings"

	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/compat_otp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	. "github.com/onsi/gomega"
)

// NetworkType retrieves the default network type for the cluster
func NetworkType(oc *exutil.CLI) string {
	output, err := oc.WithoutNamespace().AsAdmin().Run("get").Args("network.operator", "cluster", "-o=jsonpath={.spec.defaultNetwork.type}").Output()
	Expect(err).NotTo(HaveOccurred())
	return strings.ToLower(output)
}

// Domain returns the cluster domain
func Domain(oc *exutil.CLI) (string, error) {
	subDomain, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("ingresses.config/cluster", "-ojsonpath={.spec.domain}").Output()
	if err != nil {
		return "", err
	}
	return subDomain, nil
}

// ID returns the id of the cluster or errors
func ID(oc *exutil.CLI) (string, error) {
	return oc.AsAdmin().WithoutNamespace().Run("get").Args("clusterversion/version", "-ojsonpath={.spec.clusterID}").Output()
}

func HasMaster(oc *exutil.CLI) bool {
	masterNodes, err := oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/master="})
	if err != nil {
		e2e.Logf("hit error when listing master Nodes: %v", err)
	}
	return len(masterNodes.Items) > 0
}

func IsFipsEnabled(oc *exutil.CLI) bool {
	nodes, err := compat_otp.GetSchedulableLinuxWorkerNodes(oc)
	Expect(err).NotTo(HaveOccurred())
	fips, err := compat_otp.DebugNodeWithChroot(oc, nodes[0].Name, "bash", "-c", "fips-mode-setup --check")
	Expect(err).NotTo(HaveOccurred())
	return strings.Contains(fips, "FIPS mode is enabled.")
}

// OIDCProvider for the cluster
func OIDCProvider(oc *exutil.CLI) (string, error) {
	oidc, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("authentication.config", "cluster", "-o=jsonpath={.spec.serviceAccountIssuer}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(oidc, "https://"), nil
}

// InfrastructureName for the cluster For example:  anli922-jglp4
func InfrastructureName(oc *exutil.CLI) string {
	infrastructureName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure/cluster", "-o=jsonpath={.status.infrastructureName}").Output()
	Expect(err).NotTo(HaveOccurred())
	return infrastructureName
}
