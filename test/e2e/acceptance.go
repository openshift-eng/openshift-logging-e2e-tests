// Package logging is used to test openshift-logging features
package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-openshift-logging] LOGGING Logging", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("log-accept")
		loggingBaseDir string
		CLO, LO        SubscriptionObjects
	)

	g.BeforeEach(func() {
		compat_otp.SkipNoCapabilities(oc, "OperatorLifecycleManager")
		loggingBaseDir = testdata.FixturePath("logging")
		subTemplate := filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml")
		CLO = SubscriptionObjects{
			OperatorName:       "cluster-logging-operator",
			Namespace:          cloNS,
			PackageName:        "cluster-logging",
			Subscription:       subTemplate,
			OperatorGroup:      filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			SkipCaseWhenFailed: true,
		}
		LO = SubscriptionObjects{
			OperatorName:       "loki-operator-controller-manager",
			Namespace:          loNS,
			PackageName:        "loki-operator",
			Subscription:       subTemplate,
			OperatorGroup:      filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			SkipCaseWhenFailed: true,
		}

		g.By("deploy CLO")
		CLO.SubscribeOperator(oc)
		oc.SetupProject()
	})

	// author qitang@redhat.com
	// port=no - 64.8% pass rate (253 runs last 60 days)
	g.It("Author:qitang-Critical-74397-[InterOps] Forward logs to LokiStack.[Slow][Serial]", func() {
		g.By("deploy LO")
		LO.SubscribeOperator(oc)
		s := getStorageType(oc)
		sc, err := getStorageClassName(oc)
		if err != nil || len(sc) == 0 {
			g.Skip("can't get storageclass from cluster, skip this case")
		}

		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		if !hasMaster(oc) {
			nodeName, err := genLinuxAuditLogsOnWorker(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			defer deleteLinuxAuditPolicyFromNode(oc, nodeName)
		}

		g.By("deploy loki stack")
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
		ls := lokiStack{
			name:          "loki-74397",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   s,
			storageSecret: "storage-secret-74397",
			storageClass:  sc,
			bucketName:    "logging-loki-74397-" + getInfrastructureName(oc),
			template:      lokiStackTemplate,
		}
		defer ls.removeObjectStorage(oc)
		err = ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("deploy logfilesmetricexporter")
		lfme := logFileMetricExporter{
			name:          "instance",
			namespace:     loggingNS,
			template:      filepath.Join(loggingBaseDir, "logfilemetricexporter", "lfme.yaml"),
			waitPodsReady: true,
		}
		defer lfme.delete(oc)
		lfme.create(oc)

		compat_otp.By("create a CLF to test forward to lokistack")
		clf := clusterlogforwarder{
			name:                      "clf-74397",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector-74397",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret-74397",
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

		//check logs in loki stack
		g.By("check logs in loki")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)
		for _, logType := range []string{"application", "infrastructure", "audit"} {
			lc.waitForLogsAppearByKey(logType, "log_type", logType)
			labels, err := lc.listLabels(logType, "")
			o.Expect(err).NotTo(o.HaveOccurred(), "got error when checking %s log labels", logType)
			e2e.Logf("\nthe %s log labels are: %v\n", logType, labels)
		}
		journalLog, err := lc.searchLogsInLoki("infrastructure", `{log_type = "infrastructure", kubernetes_namespace_name !~ ".+"}`)
		o.Expect(err).NotTo(o.HaveOccurred())
		journalLogs := extractLogEntities(journalLog)
		o.Expect(len(journalLogs) > 0).Should(o.BeTrue(), "can't find journal logs in lokistack")
		e2e.Logf("find journal logs")
		lc.waitForLogsAppearByProject("application", appProj)

		compat_otp.By("Checking for stream labels following OTEL Semantic Conventions as a forward compatibility")
		// For App tenant
		lc.waitForLogsAppearByKey("application", "k8s_namespace_name", appProj)
		lc.waitForLogsAppearByKey("application", "k8s_container_name", "logging-centos-logtest")
		// For Infra tenant
		logs, err := lc.searchLogsInLoki("infrastructure", `{openshift_log_type = "infrastructure", k8s_namespace_name=~".+"}`)
		o.Expect(err).NotTo(o.HaveOccurred())
		extractedLogs := extractLogEntities(logs)
		o.Expect(len(extractedLogs) > 0).Should(o.BeTrue())
		journalLog, err = lc.searchLogsInLoki("infrastructure", `{openshift_log_type = "infrastructure", k8s_namespace_name !~ ".+"}`)
		o.Expect(err).NotTo(o.HaveOccurred())
		journalLogs = extractLogEntities(journalLog)
		o.Expect(len(journalLogs) > 0).Should(o.BeTrue())
		// For Audit tenant
		lc.waitForLogsAppearByKey("audit", "openshift_log_type", "audit")
		e2e.Logf("Otel semantic convention labels found!")

		g.By("Check if the ServiceMonitor object for Vector is created.")
		resource{"servicemonitor", clf.name, clf.namespace}.WaitForResourceToAppear(oc)

		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		g.By("check metrics exposed by collector")
		for _, job := range []string{clf.name, "logfilesmetricexporter"} {
			checkMetric(oc, promToken, "{job=\""+job+"\"}", 3)
		}
		for _, metric := range []string{"log_logged_bytes_total", "vector_component_received_events_total"} {
			checkMetric(oc, promToken, metric, 3)
		}

		g.By("check metrics exposed by loki")
		svcs, err := oc.AdminKubeClient().CoreV1().Services(ls.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/created-by=lokistack-controller"})
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, svc := range svcs.Items {
			if !strings.Contains(svc.Name, "grpc") && !strings.Contains(svc.Name, "ring") {
				checkMetric(oc, promToken, "{job=\""+svc.Name+"\"}", 3)
			}
		}
		for _, metric := range []string{"loki_boltdb_shipper_compactor_running", "loki_distributor_bytes_received_total", "loki_inflight_requests", "workqueue_work_duration_seconds_bucket{namespace=\"" + loNS + "\", job=\"loki-operator-controller-manager-metrics-service\"}", "loki_build_info", "loki_ingester_streams_created_total"} {
			checkMetric(oc, promToken, metric, 3)
		}
		compat_otp.By("Validate log streams are pushed to external storage bucket/container")
		ls.validateExternalObjectStorageForLogs(oc, []string{"application", "audit", "infrastructure"})
	})

	// port=no - 69.1% pass rate (687 runs last 60 days)
	g.It("Author:qitang-ConnectedOnly-Critical-74926-[InterOps] Forward logs to Cloudwatch.", func() {
		clfNS := oc.Namespace()
		cw := cloudwatchSpec{
			collectorSAName: "cloudwatch-" + getRandomString(),
			groupName:       "logging-74926-" + getInfrastructureName(oc) + `.{.log_type||"none-typed-logs"}`,
			logTypes:        []string{"infrastructure", "application", "audit"},
			secretNamespace: clfNS,
			secretName:      "logging-74926-" + getRandomString(),
		}
		cw.init(oc)
		defer cw.deleteResources(oc)

		g.By("Create log producer")
		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		if !cw.hasMaster {
			nodeName, err := genLinuxAuditLogsOnWorker(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			defer deleteLinuxAuditPolicyFromNode(oc, nodeName)
		}

		g.By("Create clusterlogforwarder")
		var template string
		if cw.stsEnabled {
			template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
		} else {
			template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
		}
		clf := clusterlogforwarder{
			name:                      "clf-74926",
			namespace:                 clfNS,
			secretName:                cw.secretName,
			templateFile:              template,
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			enableMonitoring:          true,
			serviceAccountName:        cw.collectorSAName,
		}
		defer clf.delete(oc)
		clf.createServiceAccount(oc)
		cw.createClfSecret(oc)
		clf.create(oc, "REGION="+cw.awsRegion, "GROUP_NAME="+cw.groupName, `TUNING={"compression": "snappy", "deliveryMode": "AtMostOnce", "maxRetryDuration": 20, "maxWrite": "1M", "minRetryDuration": 5}`)

		nodes, err := clf.getCollectorNodeNames(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		cw.nodes = append(cw.nodes, nodes...)

		g.By("Check logs in Cloudwatch")
		o.Expect(cw.logsFound()).To(o.BeTrue())

		compat_otp.By("check tuning in collector configurations")
		expectedConfigs := []string{
			`compression = "snappy"`,
			`[sinks.output_cloudwatch.batch]
max_bytes = 1000000`,
			`[sinks.output_cloudwatch.buffer]
when_full = "drop_newest"`,
			`[sinks.output_cloudwatch.request]
retry_initial_backoff_secs = 5
retry_max_duration_secs = 20`,
		}
		result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", expectedConfigs...)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(result).Should(o.BeTrue())
	})

	//author qitang@redhat.com
	// port=yes - 99.1% pass rate (687 runs last 60 days)
	g.It("Author:qitang-ConnectedOnly-Critical-74924-Forward logs to GCL", func() {
		projectID, err := getGCPProjectID(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		gcl := googleCloudLogging{
			projectID: projectID,
			logName:   getInfrastructureName(oc) + "-74924",
		}
		defer gcl.removeLogs()

		g.By("Create log producer")
		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		oc.SetupProject()
		clfNS := oc.Namespace()
		gcpSecret := resource{"secret", "gcp-secret-74924", clfNS}
		defer gcpSecret.clear(oc)
		err = createSecretForGCL(oc, gcpSecret.name, gcpSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())

		clf := clusterlogforwarder{
			name:                      "clf-74924",
			namespace:                 clfNS,
			secretName:                gcpSecret.name,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "googleCloudLogging.yaml"),
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			serviceAccountName:        "test-clf-" + getRandomString(),
		}
		defer clf.delete(oc)
		clf.create(oc, "ID_TYPE=project", "ID_VALUE="+gcl.projectID, "LOG_ID="+gcl.logName)

		for _, logType := range []string{"infrastructure", "audit", "application"} {
			err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				logs, err := gcl.getLogByType(logType)
				if err != nil {
					return false, err
				}
				return len(logs) > 0, nil
			})
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("%s logs are not found", logType))
		}
		err = gcl.waitForLogsAppearByNamespace(appProj)
		compat_otp.AssertWaitPollNoErr(err, "can't find app logs from project/"+appProj)

		// Check tuning options for GCL under collector configMap
		expectedConfigs := []string{"[sinks.output_gcp_logging.batch]", "[sinks.output_gcp_logging.buffer]", "[sinks.output_gcp_logging.request]", "retry_initial_backoff_secs = 10", "retry_max_duration_secs = 20"}
		result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", expectedConfigs...)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(result).Should(o.BeTrue())
	})

	//author anli@redhat.com
	// port=maybe - 88.2% pass rate (687 runs last 60 days)
	g.It("Author:anli-ConnectedOnly-Critical-71772-Forward logs to AZMonitor -- full options", func() {
		platform := compat_otp.CheckPlatform(oc)
		if platform == "azure" && compat_otp.IsWorkloadIdentityCluster(oc) {
			g.Skip("Skip on the workload identity enabled cluster!")
		}
		var (
			resourceGroupName string
			location          string
		)
		infraName := getInfrastructureName(oc)
		if platform != "azure" {
			if !readAzureCredentials() {
				g.Skip("Skip for the platform is not Azure and can't get credentials from env vars.")
			}
			resourceGroupName = infraName + "-logging-71772-rg"
			azureSubscriptionID := os.Getenv("AZURE_SUBSCRIPTION_ID")
			cred := createNewDefaultAzureCredential()
			location = "westus" //TODO: define default location

			_, err := createAzureResourceGroup(resourceGroupName, azureSubscriptionID, location, cred)
			defer deleteAzureResourceGroup(resourceGroupName, azureSubscriptionID, cred)
			if err != nil {
				g.Skip("Failed to create azure resource group: " + err.Error() + ", skip the case.")
			}
			e2e.Logf("Successfully created resource group %s", resourceGroupName)
		} else {
			cloudName := getAzureCloudName(oc)
			if !(cloudName == "azurepubliccloud" || cloudName == "azureusgovernmentcloud") {
				g.Skip("The case can only be running on Azure Public and Azure US Goverment now!")
			}
			resourceGroupName, _ = compat_otp.GetAzureCredentialFromCluster(oc)
		}

		g.By("Prepre Azure Log Storage Env")
		workSpaceName := infraName + "case71772"
		azLog, err := newAzureLog(oc, location, resourceGroupName, workSpaceName, "case71772")
		defer azLog.deleteWorkspace()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Create log producer")
		clfNS := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", clfNS, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Deploy CLF to send logs to Log Analytics")
		azureSecret := resource{"secret", "azure-secret-71772", clfNS}
		defer azureSecret.clear(oc)
		err = azLog.createSecret(oc, azureSecret.name, azureSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		clf := clusterlogforwarder{
			name:                      "clf-71772",
			namespace:                 clfNS,
			secretName:                azureSecret.name,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "azureMonitor.yaml"),
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			serviceAccountName:        "test-clf-" + getRandomString(),
		}
		defer clf.delete(oc)
		clf.create(oc, "PREFIX_OR_NAME="+azLog.tPrefixOrName, "CUSTOMER_ID="+azLog.customerID, "RESOURCE_ID="+azLog.workspaceID, "AZURE_HOST="+azLog.host)

		g.By("Verify the test result")
		for _, tableName := range []string{azLog.tPrefixOrName + "infra_log_CL", azLog.tPrefixOrName + "audit_log_CL", azLog.tPrefixOrName + "app_log_CL"} {
			_, err := azLog.getLogByTable(tableName)
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("can't find logs from %s in AzureLogWorkspace", tableName))
		}
	})

	// port=no - 67.6% pass rate (253 runs last 60 days)
	g.It("Author:kbharti-Critical-85642-Deploy LokiOperator with network policies enabled[Slow][Serial]", func() {

		platform := compat_otp.CheckPlatform(oc)
		if platform != "aws" && platform != "azure" && platform != "gcp" {
			g.Skip("This test only runs on aws, azure, or gcp platforms due to open bugs against Swift, ODF, and minIO backend storage")
		}

		compat_otp.By("deploy LokiOperator")
		LO.SubscribeOperator(oc)
		s := getStorageType(oc)
		sc, err := getStorageClassName(oc)
		if err != nil || len(sc) == 0 {
			g.Skip("can't get storageclass from cluster, skip this case")
		}
		compat_otp.By("Create an application project and deploy a log generator")
		appProj := oc.Namespace()
		oc.AsAdmin().WithoutNamespace().Run("label").Args("namespace", appProj, "openshift.io/cluster-monitoring=true").Execute()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		if !hasMaster(oc) {
			nodeName, err := genLinuxAuditLogsOnWorker(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			defer deleteLinuxAuditPolicyFromNode(oc, nodeName)
		}

		compat_otp.By("deploy loki stack with network policies enabled.")
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
		ls := lokiStack{
			name:          "loki-85642",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   s,
			storageSecret: "storage-secret-85642",
			storageClass:  sc,
			bucketName:    "logging-loki-85642-" + getInfrastructureName(oc),
			template:      lokiStackTemplate,
		}
		defer ls.removeObjectStorage(oc)
		err = ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		// Deploy with network policies enabled (ruleSet is set to RestrictIngressEgress)
		err = ls.deployLokiStack(oc, "LOKISTACK_NETWORK_POLICIES_RULESET=RestrictIngressEgress")
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Create Loki Alerting Rule for application logs")
		alertingTemplate := filepath.Join(loggingBaseDir, "loki-log-alerts", "loki-app-alerting-rule-template.yaml")
		alertRule := resource{"alertingrule", "my-app-workload-alert", appProj}
		defer alertRule.clear(oc)
		err = alertRule.applyFromTemplate(oc, "-n", alertRule.namespace, "-f", alertingTemplate, "-p", "NAMESPACE="+appProj, "-p", "ALERT_LOGQL_EXPR=count_over_time({k8s_namespace_name=\""+appProj+"\"}[1m]) > 10")
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Verify LokiStacknetwork policies are created and validate their content")
		policiesExist := ls.verifyIfLokiStackNetworkPoliciesExist(oc)
		o.Expect(policiesExist).Should(o.BeTrue(), "Network policies should exist when ruleSet is RestrictIngressEgress")
		ls.verifyLokiStackNetworkPolicies(oc)

		compat_otp.By("deploy logfilesmetricexporter")
		lfme := logFileMetricExporter{
			name:          "instance",
			namespace:     loggingNS,
			template:      filepath.Join(loggingBaseDir, "logfilemetricexporter", "lfme.yaml"),
			waitPodsReady: true,
		}
		defer lfme.delete(oc)
		lfme.create(oc)

		compat_otp.By("create a CLF to test forward to lokistack")
		clf := clusterlogforwarder{
			name:                      "clf-85642",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector-85642",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret-85642",
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

		//check logs in loki stack
		compat_otp.By("Check logs in LokiStack")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)
		for _, logType := range []string{"application", "infrastructure", "audit"} {
			lc.waitForLogsAppearByKey(logType, "log_type", logType)
			labels, err := lc.listLabels(logType, "")
			o.Expect(err).NotTo(o.HaveOccurred(), "got error when checking %s log labels", logType)
			e2e.Logf("\nthe %s log labels are: %v\n", logType, labels)
		}
		journalLog, err := lc.searchLogsInLoki("infrastructure", `{log_type = "infrastructure", kubernetes_namespace_name !~ ".+"}`)
		o.Expect(err).NotTo(o.HaveOccurred())
		journalLogs := extractLogEntities(journalLog)
		o.Expect(len(journalLogs) > 0).Should(o.BeTrue(), "can't find journal logs in lokistack")
		e2e.Logf("find journal logs")
		lc.waitForLogsAppearByProject("application", appProj)

		compat_otp.By("Check metrics are exposed by LokiStack")
		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		svcs, err := oc.AdminKubeClient().CoreV1().Services(ls.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/created-by=lokistack-controller"})
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, svc := range svcs.Items {
			if !strings.Contains(svc.Name, "grpc") && !strings.Contains(svc.Name, "ring") {
				checkMetric(oc, promToken, "{job=\""+svc.Name+"\"}", 3)
			}
		}
		for _, metric := range []string{"loki_boltdb_shipper_compactor_running", "loki_distributor_bytes_received_total", "loki_inflight_requests", "workqueue_work_duration_seconds_bucket{namespace=\"" + loNS + "\", job=\"loki-operator-controller-manager-metrics-service\"}", "loki_build_info", "loki_ingester_streams_created_total"} {
			checkMetric(oc, promToken, metric, 3)
		}
		e2e.Logf("Metrics are successfully exposed by LokiStack")

		compat_otp.By("Query AlertManager for Firing Alerts")
		queryAlertManagerForActiveAlerts(oc, bearerToken, false, "MyAppLogVolumeIsHigh", 5)

		compat_otp.By("Validate log streams are pushed to external storage bucket/container")
		ls.validateExternalObjectStorageForLogs(oc, []string{"application", "audit", "infrastructure"})
	})
})
