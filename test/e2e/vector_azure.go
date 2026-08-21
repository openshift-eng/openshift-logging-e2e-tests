// Package logging is used to test openshift-logging features
package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"fmt"
	"path/filepath"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
)

var _ = g.Describe("[sig-openshift-logging] LOGGING Logging", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("log-to-azure")
		loggingBaseDir string
		CLO            SubscriptionObjects
	)

	g.BeforeEach(func() {
		loggingBaseDir = testdata.FixturePath("logging")
		subTemplate := filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml")
		CLO = SubscriptionObjects{
			OperatorName:  "cluster-logging-operator",
			Namespace:     cloNS,
			PackageName:   "cluster-logging",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}
		g.By("deploy CLO")
		CLO.SubscribeOperator(oc)
		oc.SetupProject()
	})

	//author anli@redhat.com
	// port=no - insufficient data: 5 runs last 60 days
	g.It("Author:anli-ConnectedOnly-High-71770-Forward logs to AZMonitor -- Minimal Options", func() {
		if compat_otp.IsWorkloadIdentityCluster(oc) {
			g.Skip("Skip on the workload identity enabled cluster!")
		}

		cloudName := getAzureCloudName(oc)
		if cloudName != "azurepubliccloud" {
			g.Skip("Skip as the cluster is not on Azure Public!")
		}

		g.By("Create log producer")
		clfNS := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err := oc.WithoutNamespace().Run("new-app").Args("-n", clfNS, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Prepre Azure Log Storage Env")
		resourceGroupName, err := compat_otp.GetAzureCredentialFromCluster(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		workSpaceName := getInfrastructureName(oc) + "case71770"
		azLog, err := newAzureLog(oc, "", resourceGroupName, workSpaceName, "case71770")
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Deploy CLF to send logs to Log Analytics")
		azureSecret := resource{"secret", "azure-secret-71770", clfNS}
		defer azureSecret.clear(oc)
		err = azLog.createSecret(oc, azureSecret.name, azureSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		clf := clusterlogforwarder{
			name:                      "clf-71770",
			namespace:                 clfNS,
			secretName:                azureSecret.name,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "azureMonitor-min-opts.yaml"),
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			serviceAccountName:        "test-clf-" + getRandomString(),
		}
		defer clf.delete(oc)
		defer azLog.deleteWorkspace()
		clf.create(oc, "PREFIX_OR_NAME="+azLog.tPrefixOrName, "CUSTOMER_ID="+azLog.customerID)

		g.By("Verify the test result")
		for _, tableName := range []string{azLog.tPrefixOrName + "infra_log_CL", azLog.tPrefixOrName + "audit_log_CL", azLog.tPrefixOrName + "app_log_CL"} {
			_, err := azLog.getLogByTable(tableName)
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("logs are not found in %s in AzureLogWorkspace", tableName))
		}
	})
})
