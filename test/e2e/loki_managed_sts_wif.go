package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"path/filepath"
	"strings"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease loki-operator - Managed auth/STS mode", func() {
	defer g.GinkgoRecover()

	var (
		oc = exutil.NewCLIWithoutNamespace("loki-sts-wif-support")
		loggingBaseDir, s, sc string
	)

	g.BeforeEach(func() {
		if !compat_otp.IsWorkloadIdentityCluster(oc) {
			g.Skip("Not a STS/WIF cluster")
		}
		s = getStorageType(oc)
		if len(s) == 0 {
			g.Skip("Current cluster doesn't have a proper object storage for this test!")
		}
		sc, _ = getStorageClassName(oc)
		if len(sc) == 0 {
			g.Skip("The cluster doesn't have a storage class for this test!")
		}

		loggingBaseDir = testdata.FixturePath("logging")
		subTemplate := filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml")
		CLO := SubscriptionObjects{
			OperatorName:  "cluster-logging-operator",
			Namespace:     cloNS,
			PackageName:   "cluster-logging",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}
		LO := SubscriptionObjects{
			OperatorName:  "loki-operator-controller-manager",
			Namespace:     loNS,
			PackageName:   "loki-operator",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}

		g.By("deploy CLO and Loki Operator")
		CLO.SubscribeOperator(oc)
		LO.SubscribeOperator(oc)
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Critical-71534-Verify CCO support on AWSSTS cluster and forward logs to default LokiStack[Serial]", func() {
		currentPlatform := compat_otp.CheckPlatform(oc)
		if strings.ToLower(currentPlatform) != "aws" {
			g.Skip("The platform is not AWS. Skipping case..")
		}

		g.By("Create log producer")
		oc.SetupProject()
		appNS := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err := oc.WithoutNamespace().Run("new-app").Args("-n", appNS, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		nodeName, err := genLinuxAuditLogsOnWorker(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer deleteLinuxAuditPolicyFromNode(oc, nodeName)

		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
		ls := lokiStack{
			name:          "lokistack-71534",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   s,
			storageSecret: "storage-secret-71534",
			storageClass:  sc,
			bucketName:    "logging-loki-71534-" + getInfrastructureName(oc) + "-" + compat_otp.GetRandomString(),
			template:      lokiStackTemplate,
		}

		compat_otp.By("Deploy LokiStack")
		defer ls.removeObjectStorage(oc)
		err = ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("create a CLF to test forward to lokistack")
		clf := clusterlogforwarder{
			name:                      "clf-71534",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret",
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			waitForPodReady:           true,
			enableMonitoring:          true,
		}
		clf.createServiceAccount(oc)
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer resource{"secret", clf.secretName, clf.namespace}.clear(oc)
		ls.createSecretFromGateway(oc, clf.secretName, clf.namespace, "")
		defer clf.delete(oc)
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace)

		compat_otp.By("Validate Logs in Loki")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)
		for _, logType := range []string{"infrastructure", "audit", "application"} {
			lc.waitForLogsAppearByKey(logType, "log_type", logType)
		}

		compat_otp.By("Validate that log streams are pushed to S3 bucket")
		ls.validateExternalObjectStorageForLogs(oc, []string{"application", "audit", "infrastructure"})
	})

	// Case for Microsoft Azure WIF cluster
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Critical-71773-Verify CCO support with custom region on a AZWIF cluster and forward logs to LokiStack logstore[Serial]", func() {

		currentPlatform := compat_otp.CheckPlatform(oc)
		if currentPlatform != "azure" {
			g.Skip("The platform is not Azure. Skipping case..")
		}

		compat_otp.By("Deploy LokiStack")
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
		ls := lokiStack{
			name:          "lokistack-71773",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   s,
			storageSecret: "storage-secret-71773",
			storageClass:  sc,
			bucketName:    "loki-71773-" + compat_otp.GetRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		// Validate that credentials request created by LO has same region as the cluster (non-default scenario)
		clusterRegion, err := getAzureClusterRegion(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		credentialsRequestRegion, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("CredentialsRequest", ls.name, "-n", ls.namespace, `-o=jsonpath={.spec.providerSpec.azureRegion}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(credentialsRequestRegion).Should(o.Equal(clusterRegion))

		compat_otp.By("create a CLF to test forward to lokistack")
		clf := clusterlogforwarder{
			name:                      "clf-71773",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret",
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			waitForPodReady:           true,
			enableMonitoring:          true,
		}
		clf.createServiceAccount(oc)
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer resource{"secret", clf.secretName, clf.namespace}.clear(oc)
		ls.createSecretFromGateway(oc, clf.secretName, clf.namespace, "")
		defer clf.delete(oc)
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace)

		compat_otp.By("Validate Logs in Loki")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)
		for _, logType := range []string{"infrastructure", "audit"} {
			lc.waitForLogsAppearByKey(logType, "log_type", logType)
		}

		compat_otp.By("Validate log streams are pushed to external Azure Blob container")
		ls.validateExternalObjectStorageForLogs(oc, []string{"application", "audit", "infrastructure"})
	})

	// Case for Microsoft Azure WIF cluster
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Critical-71794-Verify CCO support with default region on a AZWIF cluster and forward logs to LokiStack logstore[Serial]", func() {

		currentPlatform := compat_otp.CheckPlatform(oc)
		if currentPlatform != "azure" {
			g.Skip("The platform is not Azure. Skipping case..")
		}

		compat_otp.By("Deploy LokiStack")
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
		ls := lokiStack{
			name:          "lokistack-71794",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   s,
			storageSecret: "storage-secret-71794",
			storageClass:  sc,
			bucketName:    "loki-71794-" + compat_otp.GetRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Patch to remove region from Loki Operator subscription (default case)
		removeRegion := `[
				{
				  "op": "remove",
				  "path": "/spec/config/env/3"
				}
		]`

		err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("patch").Args("sub", "loki-operator", "-n", loNS, "-p", removeRegion, "--type=json").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		// LO controller pod will restart with new configuration after region is removed
		waitForPodReadyWithLabel(oc, loNS, "name=loki-operator-controller-manager")

		// Create LokiStack CR
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		// Validate that credentials request created by LO has region as 'centralus' for default case
		defaultRegion, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("CredentialsRequest", ls.name, "-n", ls.namespace, `-o=jsonpath={.spec.providerSpec.azureRegion}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(defaultRegion).Should(o.Equal("centralus"))

		compat_otp.By("create a CLF to test forward to lokistack")
		clf := clusterlogforwarder{
			name:                      "clf-71794",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret",
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			waitForPodReady:           true,
			enableMonitoring:          true,
		}
		clf.createServiceAccount(oc)
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer resource{"secret", clf.secretName, clf.namespace}.clear(oc)
		ls.createSecretFromGateway(oc, clf.secretName, clf.namespace, "")
		defer clf.delete(oc)
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace)

		compat_otp.By("Validate Logs in Loki")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)
		for _, logType := range []string{"infrastructure", "audit"} {
			lc.waitForLogsAppearByKey(logType, "log_type", logType)
		}

		compat_otp.By("Validate log streams are pushed to external Azure Blob container")
		ls.validateExternalObjectStorageForLogs(oc, []string{"application", "audit", "infrastructure"})
	})
})
