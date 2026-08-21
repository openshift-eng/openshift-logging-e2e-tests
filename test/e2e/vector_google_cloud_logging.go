package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
	"k8s.io/apimachinery/pkg/util/wait"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease Forward to GCL", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("vector-to-google-cloud-logging")
		loggingBaseDir string
	)

	g.BeforeEach(func() {
		loggingBaseDir = testdata.FixturePath("logging")

		g.By("deploy CLO")
		subTemplate := filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml")
		CLO := SubscriptionObjects{
			OperatorName:  "cluster-logging-operator",
			Namespace:     cloNS,
			PackageName:   "cluster-logging",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}
		CLO.SubscribeOperator(oc)
		oc.SetupProject()
	})

	//author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-53731-Forward logs to GCL using different logName for each log type and using Service Account authentication.", func() {
		platform := compat_otp.CheckPlatform(oc)
		if platform != "gcp" {
			g.Skip("Skip for the platform is not GCP.")
		}
		g.By("Create log producer")
		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		projectID, err := compat_otp.GetGcpProjectID(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		logName := getInfrastructureName(oc) + "-53731"
		logTypes := []string{"infrastructure", "audit", "application"}
		for _, logType := range logTypes {
			defer googleCloudLogging{projectID: projectID, logName: logName + "-" + logType}.removeLogs()
		}

		oc.SetupProject()
		clfNS := oc.Namespace()
		gcpSecret := resource{"secret", "gcp-secret-53731", clfNS}
		defer gcpSecret.clear(oc)
		err = createSecretForGCL(oc, gcpSecret.name, gcpSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())

		clf := clusterlogforwarder{
			name:                      "clf-53731",
			namespace:                 clfNS,
			secretName:                gcpSecret.name,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "google-cloud-logging-multi-logids.yaml"),
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			serviceAccountName:        "test-clf-" + getRandomString(),
		}
		defer clf.delete(oc)
		clf.create(oc, "ID_TYPE=project", "ID_VALUE="+projectID, "LOG_ID="+logName)

		for _, logType := range logTypes {
			gcl := googleCloudLogging{
				projectID: projectID,
				logName:   logName + "-" + logType,
			}
			err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				logs, err := gcl.getLogByType(logType)
				if err != nil {
					return false, err
				}
				return len(logs) > 0, nil
			})
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("%s logs are not found", logType))
		}
	})

	//author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-71003-Collect or exclude logs by matching pod expressions[Slow]", func() {
		clfNS := oc.Namespace()
		projectID, err := getGCPProjectID(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		gcl := googleCloudLogging{
			projectID: projectID,
			logName:   getInfrastructureName(oc) + "-71003",
		}
		defer gcl.removeLogs()
		gcpSecret := resource{"secret", "gcp-secret-71003", clfNS}
		defer gcpSecret.clear(oc)
		err = createSecretForGCL(oc, gcpSecret.name, gcpSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())

		clf := clusterlogforwarder{
			name:                   "clf-71003",
			namespace:              clfNS,
			secretName:             gcpSecret.name,
			templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "googleCloudLogging.yaml"),
			waitForPodReady:        true,
			collectApplicationLogs: true,
			serviceAccountName:     "clf-" + getRandomString(),
		}
		defer clf.delete(oc)

		clf.create(oc, "ID_TYPE=project", "ID_VALUE="+gcl.projectID, "LOG_ID="+gcl.logName, "INPUT_REFS=[\"application\"]")
		patch := `[{"op": "add", "path": "/spec/inputs", "value": [{"name": "new-app","type": "application","application": {"selector": {"matchExpressions": [{"key": "test.logging.io/logging.qe-test-label", "operator": "In", "values": ["logging-71003-test-0", "logging-71003-test-1", "logging-71003-test-2"]},{"key":"test", "operator":"Exists"}]}}}]}, {"op": "replace", "path": "/spec/pipelines/0/inputRefs", "value": ["new-app"]}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

		compat_otp.By("Create project for app logs and deploy the log generator")
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		var namespaces []string
		for i := 0; i < 4; i++ {
			ns := "logging-project-71003-" + strconv.Itoa(i)
			defer oc.DeleteSpecifiedNamespaceAsAdmin(ns)
			oc.CreateSpecifiedNamespaceAsAdmin(ns)
			namespaces = append(namespaces, ns)
		}
		err = oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", namespaces[0], "-p", "LABELS={\"test\": \"logging-71003-0\", \"test.logging.io/logging.qe-test-label\": \"logging-71003-test-0\"}").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", namespaces[1], "-p", "LABELS={\"test.logging-71003\": \"logging-71003-1\", \"test.logging.io/logging.qe-test-label\": \"logging-71003-test-1\"}").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", namespaces[2], "-p", "LABELS={\"test.logging.io/logging.qe-test-label\": \"logging-71003-test-2\"}").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", namespaces[3], "-p", "LABELS={\"test\": \"logging-71003-3\"}").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		compat_otp.By("Check data in google cloud logging, only logs from project namespaces[0] should be collected")
		err = gcl.waitForLogsAppearByType("application")
		compat_otp.AssertWaitPollNoErr(err, "application logs are not collected")
		appLogs1, err := gcl.getLogByNamespace(namespaces[0])
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(appLogs1) > 0).Should(o.BeTrue())
		for i := 1; i < 4; i++ {
			appLogs, err := gcl.getLogByNamespace(namespaces[i])
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(appLogs) == 0).Should(o.BeTrue())
		}

		compat_otp.By("Update CLF, change the matchExpressions")
		patch = `[{"op": "replace", "path": "/spec/inputs/0/application/selector/matchExpressions", "value": [{"key": "test.logging.io/logging.qe-test-label", "operator": "In", "values": ["logging-71003-test-0", "logging-71003-test-1", "logging-71003-test-2"]},{"key":"test", "operator":"DoesNotExist"}]}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

		// sleep 10 seconds for collector pods to send the cached records
		time.Sleep(10 * time.Second)
		gcl.removeLogs()
		compat_otp.By("Check data in google cloud logging, logs from project namespaces[1] and namespaces[2] should be collected")
		err = gcl.waitForLogsAppearByNamespace(namespaces[1])
		compat_otp.AssertWaitPollNoErr(err, "can't find logs from project "+namespaces[1])
		err = gcl.waitForLogsAppearByNamespace(namespaces[2])
		compat_otp.AssertWaitPollNoErr(err, "can't find logs from project "+namespaces[2])

		for _, ns := range []string{namespaces[0], namespaces[3]} {
			appLogs, err := gcl.getLogByNamespace(ns)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(appLogs) == 0).Should(o.BeTrue(), "find logs from project"+ns+", this is not expected")
		}

		compat_otp.By("Update CLF, change the matchExpressions")
		patch = `[{"op": "replace", "path": "/spec/inputs/0/application/selector/matchExpressions", "value": [{"key": "test.logging.io/logging.qe-test-label", "operator": "NotIn", "values": ["logging-71003-test-0", "logging-71003-test-1", "logging-71003-test-2"]},{"key":"test", "operator":"Exists"}]}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

		// sleep 10 seconds for collector pods to send the cached records
		time.Sleep(10 * time.Second)
		gcl.removeLogs()
		compat_otp.By("Check data in google cloud logging, logs from project namespaces[3] should be collected")
		err = gcl.waitForLogsAppearByNamespace(namespaces[3])
		compat_otp.AssertWaitPollNoErr(err, "can't find logs from project "+namespaces[3])
		for i := 0; i < 3; i++ {
			appLogs, err := gcl.getLogByNamespace(namespaces[i])
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(appLogs) == 0).Should(o.BeTrue(), "find logs from project"+namespaces[i]+", this is not expected")
		}
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:ikanse-High-61602-Collector external to GCL complies with the tlsSecurityProfile configuration. [Slow][Disruptive]", func() {
		platform := compat_otp.CheckPlatform(oc)
		if platform != "gcp" {
			g.Skip("Skip for the platform is not GCP.")
		}

		compat_otp.By("Check if the current tlsSecurityProfile is the expected one")
		if !compareExpectedTLSConfigWithCurrent(oc, `{"intermediate":{},"type":"Intermediate"}`) {
			g.Skip("Current tlsSecurityProfile is not the expected one, skipping the test...")
		}

		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		compat_otp.By("Create log producer")
		appProj1 := oc.Namespace()
		err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj1, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		oc.SetupProject()
		clfNS := oc.Namespace()

		projectID, err := compat_otp.GetGcpProjectID(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		gcl := googleCloudLogging{
			projectID: projectID,
			logName:   getInfrastructureName(oc) + "-61602",
		}
		defer gcl.removeLogs()
		gcpSecret := resource{"secret", "gcp-secret-61602", clfNS}
		defer gcpSecret.clear(oc)
		err = createSecretForGCL(oc, gcpSecret.name, gcpSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())

		clf := clusterlogforwarder{
			name:                   "clf-61602",
			namespace:              clfNS,
			secretName:             gcpSecret.name,
			templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "googleCloudLogging.yaml"),
			waitForPodReady:        true,
			collectApplicationLogs: true,
			serviceAccountName:     "test-clf-" + getRandomString(),
		}
		defer clf.delete(oc)

		clf.create(oc, "ID_TYPE=project", "ID_VALUE="+gcl.projectID, "LOG_ID="+gcl.logName, "INPUT_REFS=[\"application\"]")

		compat_otp.By("The Google Cloud sink in Vector config must use the intermediate tlsSecurityProfile")
		searchString := `[sinks.output_gcp_logging.tls]
min_tls_version = "VersionTLS12"
ciphersuites = "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256,ECDHE-ECDSA-AES128-GCM-SHA256,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES256-GCM-SHA384,ECDHE-RSA-AES256-GCM-SHA384,ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,DHE-RSA-AES128-GCM-SHA256,DHE-RSA-AES256-GCM-SHA384"`
		result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

		err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
			logs, err := gcl.getLogByType("application")
			if err != nil {
				return false, err
			}
			return len(logs) > 0, nil
		})
		compat_otp.AssertWaitPollNoErr(err, "application logs are not found")

		appLogs1, err := gcl.getLogByNamespace(appProj1)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(appLogs1) > 0).Should(o.BeTrue())

		compat_otp.By("Set Modern tlsSecurityProfile for the External Google Cloud logging output.")
		patch := `[{"op": "add", "path": "/spec/outputs/0/tls", "value": {"securityProfile": {"type": "Modern"}}}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

		compat_otp.By("Delete logs from Google Cloud Logging")
		err = gcl.removeLogs()
		o.Expect(err).NotTo(o.HaveOccurred())

		compat_otp.By("The Google Cloud sink in Vector config must use the Modern tlsSecurityProfile")
		searchString = `[sinks.output_gcp_logging.tls]
min_tls_version = "VersionTLS13"
ciphersuites = "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256"`
		result, err = checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

		err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
			logs, err := gcl.getLogByType("application")
			if err != nil {
				return false, err
			}
			return len(logs) > 0, nil
		})
		compat_otp.AssertWaitPollNoErr(err, "application logs are not found")

		appLogs1, err = gcl.getLogByNamespace(appProj1)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(appLogs1) > 0).Should(o.BeTrue())
	})

	//author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-Medium-71777-Include or exclude logs by combining namespace and container selectors.[Slow]", func() {
		platform := compat_otp.CheckPlatform(oc)
		if platform != "gcp" {
			g.Skip("Skip for the platform is not GCP.")
		}
		clfNS := oc.Namespace()
		projectID, err := getGCPProjectID(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		gcl := googleCloudLogging{
			projectID: projectID,
			logName:   getInfrastructureName(oc) + "-71777",
		}
		defer gcl.removeLogs()
		gcpSecret := resource{"secret", "gcp-secret-71777", clfNS}
		defer gcpSecret.clear(oc)
		err = createSecretForGCL(oc, gcpSecret.name, gcpSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())

		clf := clusterlogforwarder{
			name:                   "clf-71777",
			namespace:              clfNS,
			secretName:             gcpSecret.name,
			templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "googleCloudLogging.yaml"),
			waitForPodReady:        true,
			collectApplicationLogs: true,
			serviceAccountName:     "clf-" + getRandomString(),
		}
		defer clf.delete(oc)

		clf.create(oc, "ID_TYPE=project", "ID_VALUE="+gcl.projectID, "LOG_ID="+gcl.logName, "INPUT_REFS=[\"application\"]")
		compat_otp.By("exclude logs from specific container in specific namespace")
		patch := `[{"op": "add", "path": "/spec/inputs", "value": [{"name": "new-app","type": "application" ,"application": {"excludes": [{"namespace": "logging-log-71777", "container": "exclude-log-71777"}]}}]}, {"op": "replace", "path": "/spec/pipelines/0/inputRefs", "value": ["new-app"]}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

		compat_otp.By("Create project for app logs and deploy the log generator")
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		namespaces := []string{"logging-log-71777", "logging-data-71777", "e2e-test-log-71777"}
		containerNames := []string{"log-71777-include", "exclude-log-71777"}
		for _, ns := range namespaces {
			defer oc.DeleteSpecifiedNamespaceAsAdmin(ns)
			oc.CreateSpecifiedNamespaceAsAdmin(ns)
			for _, container := range containerNames {
				err := oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", ns, "-p", "CONTAINER="+container, "-p", "CONFIGMAP="+container, "-p", "REPLICATIONCONTROLLER="+container).Execute()
				o.Expect(err).NotTo(o.HaveOccurred())
			}
		}

		generateQuery := func(ns, container string) string {
			return fmt.Sprintf(` AND jsonPayload.kubernetes.namespace_name="%s" AND jsonPayload.kubernetes.container_name="%s"`, ns, container)
		}
		compat_otp.By("Check data in google cloud logging, logs from container/exclude-log-71777 in project/logging-log-71777 shouldn't be collected")
		err = gcl.waitForLogsAppearByType("application")
		compat_otp.AssertWaitPollNoErr(err, "application logs are not collected")
		for _, ns := range []string{"logging-data-71777", "e2e-test-log-71777"} {
			for _, container := range containerNames {
				appLogs, err := gcl.listLogEntries(generateQuery(ns, container))
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(len(appLogs) > 0).Should(o.BeTrue(), "can't find logs from container "+container+" in project"+ns+", this is not expected")
			}
		}
		appLogs, err := gcl.listLogEntries(generateQuery("logging-log-71777", "log-71777-include"))
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(appLogs) > 0).Should(o.BeTrue(), "can't find logs from container/log-71777-include in project/logging-log-71777, this is not expected")
		appLogs, err = gcl.listLogEntries(generateQuery("logging-log-71777", "exclude-log-71777"))
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(appLogs) == 0).Should(o.BeTrue(), "find logs from container/exclude-log-71777 in project/logging-log-71777, this is not expected")

		compat_otp.By("exclude logs from specific containers in all namespaces")
		patch = `[{"op": "replace", "path": "/spec/inputs/0/application/excludes", "value": [{"namespace": "*", "container": "exclude-log-71777"}]}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
		// sleep 10 seconds for collector pods to send the cached records
		time.Sleep(10 * time.Second)
		gcl.removeLogs()
		compat_otp.By("Check data in google cloud logging, no logs from container/exclude-log-71777")
		err = gcl.waitForLogsAppearByType("application")
		compat_otp.AssertWaitPollNoErr(err, "application logs are not collected")
		for _, ns := range namespaces {
			err = gcl.waitForLogsAppearByNamespace(ns)
			compat_otp.AssertWaitPollNoErr(err, "can't find logs from project "+ns)
		}
		logs, err := gcl.listLogEntries(` AND jsonPayload.kubernetes.container_name="exclude-log-71777"`)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) == 0).Should(o.BeTrue(), "find logs from container exclude-log-71777, this is not expected")

		compat_otp.By("exclude logs from all containers in specific namespaces")
		patch = `[{"op": "replace", "path": "/spec/inputs/0/application/excludes", "value": [{"namespace": "e2e-test-log-71777", "container": "*"}]}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
		// sleep 10 seconds for collector pods to send the cached records
		time.Sleep(10 * time.Second)
		gcl.removeLogs()
		compat_otp.By("Check data in google cloud logging, no logs from project/e2e-test-log-71777")
		err = gcl.waitForLogsAppearByType("application")
		compat_otp.AssertWaitPollNoErr(err, "application logs are not collected")
		for _, ns := range []string{"logging-log-71777", "logging-data-71777"} {
			for _, container := range containerNames {
				appLogs, err := gcl.listLogEntries(generateQuery(ns, container))
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(len(appLogs) > 0).Should(o.BeTrue(), "can't find logs from container "+container+" in project"+ns+", this is not expected")
			}
		}
		logs, err = gcl.getLogByNamespace("e2e-test-log-71777")
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) == 0).Should(o.BeTrue(), "find logs from project e2e-test-log-71777, this is not expected")

		compat_otp.By("Update CLF to collect logs from specific containers in specific namespaces")
		patch = `[{"op": "remove", "path": "/spec/inputs/0/application/excludes"}, {"op": "add", "path": "/spec/inputs/0/application", "value": {"includes": [{"namespace": "logging-log-71777", "container": "log-71777-include"}]}}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
		// sleep 10 seconds for collector pods to send the cached records
		time.Sleep(10 * time.Second)
		gcl.removeLogs()
		compat_otp.By("Check data in google cloud logging, only logs from container log-71777-include in project logging-log-71777 should be collected")
		err = gcl.waitForLogsAppearByNamespace("logging-log-71777")
		compat_otp.AssertWaitPollNoErr(err, "logs from project logging-log-71777 are not collected")
		includeLogs, err := gcl.listLogEntries(generateQuery("logging-log-71777", "log-71777-include"))
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(includeLogs) > 0).Should(o.BeTrue(), "can't find logs from container log-71777-include in project logging-log-71777, this is not expected")
		excludeLogs, err := gcl.listLogEntries(` AND jsonPayload.kubernetes.namespace_name!="logging-log-71777" OR jsonPayload.kubernetes.container_name!="log-71777-include"`)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(excludeLogs) == 0).Should(o.BeTrue(), "find logs from other containers, this is not expected")

		compat_otp.By("collect logs from specific containers in all namespaces")
		patch = `[{"op": "replace", "path": "/spec/inputs/0/application/includes", "value": [{"namespace": "*", "container": "log-71777-include"}]}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
		// sleep 10 seconds for collector pods to send the cached records
		time.Sleep(10 * time.Second)
		gcl.removeLogs()
		compat_otp.By("Check data in google cloud logging, only logs from container/log-71777-include should be collected")
		err = gcl.waitForLogsAppearByType("application")
		compat_otp.AssertWaitPollNoErr(err, "application logs are not collected")
		for _, ns := range namespaces {
			err = gcl.waitForLogsAppearByNamespace(ns)
			compat_otp.AssertWaitPollNoErr(err, "can't find logs from project "+ns)
		}
		logs, err = gcl.listLogEntries(` AND jsonPayload.kubernetes.container_name != "log-71777-include"`)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) == 0).Should(o.BeTrue(), "find logs from other containers, this is not expected")
		// no logs from openshift* projects

		compat_otp.By("collect logs from all containers in specific namespaces")
		patch = `[{"op": "replace", "path": "/spec/inputs/0/application/includes", "value": [{"namespace": "logging-data-71777", "container": "*"}]}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
		// sleep 10 seconds for collector pods to send the cached records
		time.Sleep(10 * time.Second)
		gcl.removeLogs()
		compat_otp.By("Check data in google cloud logging, only logs from project/logging-data-71777 should be collected")
		err = gcl.waitForLogsAppearByType("application")
		compat_otp.AssertWaitPollNoErr(err, "application logs are not collected")
		logs, err = gcl.listLogEntries(` AND jsonPayload.kubernetes.namespace_name != "logging-data-71777"`)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) == 0).Should(o.BeTrue(), "find logs from other projects, this is not expected")
		logs, err = gcl.getLogByNamespace("logging-data-71777")
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) > 0).Should(o.BeTrue(), "can't find logs from project logging-data-71777, this is not expected")

		compat_otp.By("combine includes and excludes")
		patch = `[{"op": "replace", "path": "/spec/inputs/0/application", "value": {"includes": [{"namespace": "*log*", "container": "log-71777*"}], "excludes": [{"namespace": "logging*71777", "container": "log-71777-include"}]}}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
		// sleep 10 seconds for collector pods to send the cached records
		time.Sleep(10 * time.Second)
		gcl.removeLogs()
		compat_otp.By("Check data in google cloud logging, only logs from container/log-71777-include in project/e2e-test-log-71777 should be collected")
		err = gcl.waitForLogsAppearByType("application")
		compat_otp.AssertWaitPollNoErr(err, "application logs are not collected")
		logs, err = gcl.listLogEntries(generateQuery("e2e-test-log-71777", "log-71777-include"))
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) > 0).Should(o.BeTrue(), "can't find logs from container log-71777-include in project e2e-test-log-71777, this is not expected")
		logs, err = gcl.listLogEntries(` AND jsonPayload.kubernetes.namespace_name!="e2e-test-log-71777" OR jsonPayload.kubernetes.container_name!="log-71777-include"`)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) == 0).Should(o.BeTrue(), "find logs from other containers, this is not expected")
	})

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-ConnectedOnly-Medium-71753-Prune fields from log messages", func() {
		platform := compat_otp.CheckPlatform(oc)
		if platform != "gcp" {
			g.Skip("Skip for the platform is not GCP.")
		}
		compat_otp.By("Create CLF")
		clfNS := oc.Namespace()
		projectID, err := getGCPProjectID(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		gcl := googleCloudLogging{
			projectID: projectID,
			logName:   getInfrastructureName(oc) + "-71753",
		}
		defer gcl.removeLogs()
		gcpSecret := resource{"secret", "gcp-secret-71753", clfNS}
		defer gcpSecret.clear(oc)
		err = createSecretForGCL(oc, gcpSecret.name, gcpSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())

		clf := clusterlogforwarder{
			name:                      "clf-71753",
			namespace:                 clfNS,
			secretName:                gcpSecret.name,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "googleCloudLogging.yaml"),
			collectApplicationLogs:    true,
			collectInfrastructureLogs: true,
			collectAuditLogs:          true,
			serviceAccountName:        "clf-" + getRandomString(),
		}
		defer clf.delete(oc)

		clf.create(oc, "ID_TYPE=project", "ID_VALUE="+gcl.projectID, "LOG_ID="+gcl.logName)
		compat_otp.By("Add prune filters to CLF")
		patch := `[{"op": "add", "path": "/spec/filters", "value": [{"name": "prune-logs", "type": "prune", "prune": {"in": [".kubernetes.namespace_name",".kubernetes.labels.\"test.logging.io/logging.qe-test-label\"",".file",".kubernetes.annotations"]}}]},
		{"op": "add", "path": "/spec/pipelines/0/filterRefs", "value": ["prune-logs"]}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

		compat_otp.By("Create project for app logs and deploy the log generator")
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		oc.SetupProject()
		ns := oc.Namespace()
		err = oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", ns, "-p", "LABELS={\"test\": \"logging-71753-test\", \"test.logging.io/logging.qe-test-label\": \"logging-71753-test\"}").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		compat_otp.By("Check logs in google cloud logging")
		for _, logType := range []string{"application", "infrastructure", "audit"} {
			err = gcl.waitForLogsAppearByType(logType)
			compat_otp.AssertWaitPollNoErr(err, logType+" logs are not collected")
		}
		compat_otp.By("Fields kubernetes.namespace_name, kubernetes.labels.\"test.logging.io/logging.qe-test-label\", kubernetes.annotations and file should be pruned")
		// sleep 30 seconds for collector pods to send new data to google cloud logging
		time.Sleep(30 * time.Second)
		logs, err := gcl.getLogByType("application")
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) > 0).Should(o.BeTrue())
		extractedLogs, err := extractGoogleCloudLoggingLogs(logs)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(extractedLogs) > 0).Should(o.BeTrue())
		o.Expect(extractedLogs[0].File == "").Should(o.BeTrue())
		o.Expect(extractedLogs[0].Kubernetes.Annotations == nil).Should(o.BeTrue())
		o.Expect(extractedLogs[0].Kubernetes.NamespaceName == "").Should(o.BeTrue())
		o.Expect(extractedLogs[0].Kubernetes.Lables["test_logging_io_logging_qe-test-label"] == "").Should(o.BeTrue())

		compat_otp.By("Prune .hostname, the CLF should be rejected")
		patch = `[{"op": "replace", "path": "/spec/filters/0/prune/in", "value": [".hostname",".kubernetes.namespace_name",".kubernetes.labels.\"test.logging.io/logging.qe-test-label\"",".file",".kubernetes.annotations"]}]`
		clf.update(oc, "", patch, "--type=json")
		checkResource(oc, true, false, "\"prune-logs\" prunes the `.hostname` field which is required for output: \"gcp-logging\" of type \"googleCloudLogging\"", []string{"clusterlogforwarders.observability.openshift.io", clf.name, "-n", clf.namespace, "-ojsonpath={.status.pipelineConditions[0].message}"})

		compat_otp.By("Update CLF to only reserve several fields")
		patch = `[{"op": "replace", "path": "/spec/filters/0/prune", "value": {"notIn": [".log_type",".log_source",".message",".kubernetes",".\"@timestamp\"",".openshift",".hostname"]}}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
		// sleep 60 seconds for collector pods to send new data to google cloud logging
		time.Sleep(60 * time.Second)

		compat_otp.By("Check logs in google cloud logging")
		err = gcl.waitForLogsAppearByNamespace(ns)
		compat_otp.AssertWaitPollNoErr(err, "logs from project/"+ns+" are not collected")
		logs, err = gcl.getLogByNamespace(ns)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) > 0).Should(o.BeTrue())
		extractedLogs, err = extractGoogleCloudLoggingLogs(logs)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(extractedLogs) > 0).Should(o.BeTrue())
		o.Expect(extractedLogs[0].File == "").Should(o.BeTrue())
		o.Expect(extractedLogs[0].Kubernetes.Annotations != nil).Should(o.BeTrue())
		o.Expect(extractedLogs[0].Kubernetes.Lables["test_logging_io_logging_qe-test-label"] == "logging-71753-test").Should(o.BeTrue())

		compat_otp.By("Prune .hostname, the CLF should be rejected")
		patch = `[{"op": "replace", "path": "/spec/filters/0/prune/notIn", "value": [".log_type",".log_source",".message",".kubernetes",".\"@timestamp\"",".openshift"]}]`
		clf.update(oc, "", patch, "--type=json")
		checkResource(oc, true, false, "\"prune-logs\" prunes the `.hostname` field which is required for output: \"gcp-logging\" of type \"googleCloudLogging\"", []string{"clusterlogforwarders.observability.openshift.io", clf.name, "-n", clf.namespace, "-ojsonpath={.status.pipelineConditions[0].message}"})

		compat_otp.By("Combine in and notIn")
		patch = `[{"op": "replace", "path": "/spec/filters/0/prune", "value": {"notIn": [".log_type",".log_source",".message",".kubernetes",".\"@timestamp\"",".hostname"],
		"in": [".kubernetes.namespace_name",".kubernetes.labels.\"test.logging.io/logging.qe-test-label\"",".file",".kubernetes.annotations"]}}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
		// sleep 30 seconds for collector pods to send new data to google cloud logging
		time.Sleep(30 * time.Second)

		logs, err = gcl.getLogByType("application")
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(logs) > 0).Should(o.BeTrue())
		extractedLogs, err = extractGoogleCloudLoggingLogs(logs)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(extractedLogs) > 0).Should(o.BeTrue())
		o.Expect(extractedLogs[0].File == "").Should(o.BeTrue())
		o.Expect(extractedLogs[0].OpenShift.ClusterID == "").Should(o.BeTrue())
		o.Expect(extractedLogs[0].Kubernetes.Annotations == nil).Should(o.BeTrue())
		o.Expect(extractedLogs[0].Kubernetes.NamespaceName == "").Should(o.BeTrue())
		o.Expect(extractedLogs[0].Kubernetes.Lables["test_logging_io_logging_qe-test-label"] == "").Should(o.BeTrue())
	})
})
