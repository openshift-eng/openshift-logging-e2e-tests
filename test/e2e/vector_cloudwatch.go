package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("vector-cw")
		loggingBaseDir string
		infraName      string
	)

	g.Context("Log Forward to Cloudwatch", func() {

		g.BeforeEach(func() {
			loggingBaseDir = testdata.FixturePath("logging")
			CLO := SubscriptionObjects{
				OperatorName:  "cluster-logging-operator",
				Namespace:     cloNS,
				PackageName:   "cluster-logging",
				Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
				OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			}

			g.By("deploy CLO")
			CLO.SubscribeOperator(oc)
			oc.SetupProject()
			infraName = getInfrastructureName(oc)
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-76074-Forward logs to Cloudwatch group by namespaceName and groupPrefix", func() {
			platform := compat_otp.CheckPlatform(oc)
			if platform != "aws" {
				g.Skip("Skip for the platform is not AWS.")
			}
			g.By("init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := cloudwatchSpec{
				collectorSAName: "cloudwatch-" + getRandomString(),
				secretNamespace: clfNS,
				secretName:      "logging-76074-" + getRandomString(),
				groupName:       "logging-76074-" + infraName + `.{.kubernetes.namespace_name||.log_type||"none-typed-logs"}`,
				logTypes:        []string{"infrastructure", "application", "audit"},
			}
			defer cw.deleteResources(oc)
			cw.init(oc)

			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.selAppNamespaces = append(cw.selAppNamespaces, appProj)
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
				name:                      "clf-76074",
				namespace:                 clfNS,
				templateFile:              template,
				secretName:                cw.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        cw.collectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.createClfSecret(oc)
			clf.create(oc, "REGION="+cw.awsRegion, "GROUP_NAME="+cw.groupName)
			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.nodes = append(cw.nodes, nodes...)

			g.By("Check logs in Cloudwatch")
			o.Expect(cw.logsFound()).To(o.BeTrue())
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-76075-Forward logs to Cloudwatch using namespaceUUID and groupPrefix", func() {
			platform := compat_otp.CheckPlatform(oc)
			if platform != "aws" {
				g.Skip("Skip for the platform is not AWS.")
			}
			g.By("init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := cloudwatchSpec{
				collectorSAName: "cloudwatch-" + getRandomString(),
				secretNamespace: clfNS,
				secretName:      "logging-76075-" + getRandomString(),
				groupName:       "logging-76075-" + infraName + `.{.kubernetes.namespace_id||.log_type||"none-typed-logs"}`,
				logTypes:        []string{"infrastructure", "application", "audit"},
			}
			defer cw.deleteResources(oc)
			cw.init(oc)

			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			g.By("Create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			uuid, err := oc.WithoutNamespace().Run("get").Args("project", appProj, "-ojsonpath={.metadata.uid}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.selNamespacesID = []string{uuid}
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
				name:                      "clf-76075",
				namespace:                 clfNS,
				templateFile:              template,
				secretName:                cw.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        cw.collectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.createClfSecret(oc)
			clf.create(oc, "REGION="+cw.awsRegion, "GROUP_NAME="+cw.groupName)

			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.nodes = append(cw.nodes, nodes...)

			g.By("Check logs in Cloudwatch")
			o.Expect(cw.checkLogGroupByNamespaceID()).To(o.BeTrue())
			o.Expect(cw.infrastructureLogsFound(false)).To(o.BeTrue())
			o.Expect(cw.auditLogsFound(false)).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-High-61600-Collector External Cloudwatch output complies with the tlsSecurityProfile configuration.[Slow][Disruptive]", func() {
			compat_otp.By("Check if the current tlsSecurityProfile is the expected one")
			if !compareExpectedTLSConfigWithCurrent(oc, `{"custom":{"ciphers":["ECDHE-ECDSA-CHACHA20-POLY1305","ECDHE-RSA-CHACHA20-POLY1305","ECDHE-RSA-AES128-GCM-SHA256","ECDHE-ECDSA-AES128-GCM-SHA256"],"minTLSVersion":"VersionTLS12"},"type":"Custom"}`) {
				g.Skip("Current tlsSecurityProfile is not the expected one, skipping the test...")
			}

			compat_otp.By("init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := cloudwatchSpec{
				collectorSAName: "cloudwatch-" + getRandomString(),
				secretNamespace: clfNS,
				secretName:      "logging-61600-" + getRandomString(),
				groupName:       "logging-61600-" + infraName + `.{.log_type||"none-typed-logs"}`,
				logTypes:        []string{"infrastructure", "application", "audit"},
			}
			defer cw.deleteResources(oc)
			cw.init(oc)

			compat_otp.By("create clusterlogforwarder")
			var template string
			if cw.stsEnabled {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
			} else {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
			}
			clf := clusterlogforwarder{
				name:                      "clf-61600",
				namespace:                 clfNS,
				templateFile:              template,
				secretName:                cw.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        cw.collectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.createClfSecret(oc)
			clf.create(oc, "REGION="+cw.awsRegion, "GROUP_NAME="+cw.groupName)
			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.nodes = append(cw.nodes, nodes...)

			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			compat_otp.By("Create log producer")
			appProj1 := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj1, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.selAppNamespaces = []string{appProj1}
			if !cw.hasMaster {
				nodeName, err := genLinuxAuditLogsOnWorker(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				defer deleteLinuxAuditPolicyFromNode(oc, nodeName)
			}

			compat_otp.By("The Cloudwatch sink in Vector config must use the Custom tlsSecurityProfile")
			searchString := `[sinks.output_cloudwatch.tls]
min_tls_version = "VersionTLS12"
ciphersuites = "ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES128-GCM-SHA256"`
			result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("check logs in Cloudwatch")
			logGroupName := "logging-61600-" + infraName + ".application"
			o.Expect(cw.logsFound()).To(o.BeTrue())
			filteredLogs, err := cw.getLogRecordsByNamespace(30, logGroupName, appProj1)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(filteredLogs) > 0).Should(o.BeTrue(), "Couldn't filter logs by namespace")

			compat_otp.By("Set Intermediate tlsSecurityProfile for the Cloudwatch output.")
			patch := `[{"op": "add", "path": "/spec/outputs/0/tls", "value": {"securityProfile": {"type": "Intermediate"}}}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			compat_otp.By("Create log producer")
			oc.SetupProject()
			appProj2 := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj2, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.selAppNamespaces = []string{appProj2}

			compat_otp.By("The Cloudwatch sink in Vector config must use the Intermediate tlsSecurityProfile")
			searchString = `[sinks.output_cloudwatch.tls]
min_tls_version = "VersionTLS12"
ciphersuites = "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256,ECDHE-ECDSA-AES128-GCM-SHA256,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES256-GCM-SHA384,ECDHE-RSA-AES256-GCM-SHA384,ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,DHE-RSA-AES128-GCM-SHA256,DHE-RSA-AES256-GCM-SHA384"`
			result, err = checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("Check for errors in collector pod logs")
			e2e.Logf("Wait for a minute before the collector logs are generated.")
			time.Sleep(60 * time.Second)
			collectorLogs, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("-n", clf.namespace, "--selector=app.kubernetes.io/component=collector").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(strings.Contains(collectorLogs, "Error trying to connect")).ShouldNot(o.BeTrue(), "Unable to connect to the external Cloudwatch server.")

			compat_otp.By("check logs in Cloudwatch")
			o.Expect(cw.logsFound()).To(o.BeTrue())
			filteredLogs, err = cw.getLogRecordsByNamespace(30, logGroupName, appProj2)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(filteredLogs) > 0).Should(o.BeTrue(), "Couldn't filter logs by namespace")
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-71778-Collect or exclude logs by matching pod labels and namespaces.[Slow]", func() {
			platform := compat_otp.CheckPlatform(oc)
			if platform != "aws" {
				g.Skip("Skip for the platform is not AWS.")
			}
			g.By("init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := cloudwatchSpec{
				collectorSAName: "cloudwatch-" + getRandomString(),
				secretNamespace: clfNS,
				secretName:      "logging-71778-" + getRandomString(),
				groupName:       "logging-71778-" + infraName + `.{.log_type||"none-typed-logs"}`,
				logTypes:        []string{"application"},
			}
			defer cw.deleteResources(oc)
			cw.init(oc)

			compat_otp.By("Create projects for app logs and deploy the log generators")
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			oc.SetupProject()
			appNS1 := oc.Namespace()
			err := oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", appNS1, "-p", "LABELS={\"test\": \"logging-71778\", \"test.logging.io/logging.qe-test-label\": \"logging-71778-test\"}").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			var namespaces []string
			for i := 0; i < 3; i++ {
				ns := "logging-project-71778-" + strconv.Itoa(i) + "-" + getRandomString()
				defer oc.DeleteSpecifiedNamespaceAsAdmin(ns)
				oc.CreateSpecifiedNamespaceAsAdmin(ns)
				namespaces = append(namespaces, ns)
			}
			err = oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", namespaces[0], "-p", "LABELS={\"test.logging-71778\": \"logging-71778\", \"test.logging.io/logging.qe-test-label\": \"logging-71778-test\"}").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			err = oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", namespaces[1], "-p", "LABELS={\"test.logging.io/logging.qe-test-label\": \"logging-71778-test\"}").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			err = oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", namespaces[2], "-p", "LABELS={\"test\": \"logging-71778\"}").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("Create clusterlogforwarder")
			var template string
			if cw.stsEnabled {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
			} else {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
			}
			clf := clusterlogforwarder{
				name:                   "clf-71778",
				namespace:              clfNS,
				templateFile:           template,
				secretName:             cw.secretName,
				collectApplicationLogs: true,
				serviceAccountName:     cw.collectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.createClfSecret(oc)
			clf.create(oc, "REGION="+cw.awsRegion, "GROUP_NAME="+cw.groupName, "INPUT_REFS=[\"application\"]")
			patch := `[{"op": "add", "path": "/spec/inputs", "value": [{"name": "myapplogdata", "type": "application", "application": {"selector": {"matchLabels": {"test.logging.io/logging.qe-test-label": "logging-71778-test"}}}}]}, {"op": "replace", "path": "/spec/pipelines/0/inputRefs", "value": ["myapplogdata"]}]`
			clf.update(oc, "", patch, "--type=json")
			clf.waitForCollectorPodsReady(oc)

			compat_otp.By("Check logs in Cloudwatch")
			cw.selAppNamespaces = []string{namespaces[0], namespaces[1], appNS1}
			cw.disAppNamespaces = []string{namespaces[2]}
			o.Expect(cw.logsFound()).To(o.BeTrue())

			compat_otp.By("Update CLF to combine label selector and namespace selector")
			patch = `[{"op": "add", "path": "/spec/inputs/0/application/includes", "value": [{"namespace": "*71778*"}]}, {"op": "add", "path": "/spec/inputs/0/application/excludes", "value": [{"namespace": "` + namespaces[1] + `"}]}]`
			clf.update(oc, "", patch, "--type=json")
			clf.waitForCollectorPodsReady(oc)
			//sleep 10 seconds to wait for the caches in collectors to be cleared
			time.Sleep(10 * time.Second)

			compat_otp.By("Check logs in Cloudwatch")
			newGroupName := "new-logging-71778-" + infraName
			clf.update(oc, "", `[{"op": "replace", "path": "/spec/outputs/0/cloudwatch/groupName", "value": "`+newGroupName+`"}]`, "--type=json")
			clf.waitForCollectorPodsReady(oc)
			defer cw.deleteGroups("logging-71778-" + infraName)
			cw.setGroupName(newGroupName)
			cw.selAppNamespaces = []string{namespaces[0]}
			cw.disAppNamespaces = []string{namespaces[1], namespaces[2], appNS1}
			o.Expect(cw.logsFound()).To(o.BeTrue())
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-71488-Collect container logs from infrastructure projects in an application input.", func() {
			g.By("init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := cloudwatchSpec{
				collectorSAName: "clf-71488",
				secretName:      "clf-71488",
				secretNamespace: clfNS,
				groupName:       "logging-71488-" + infraName + `.{.log_type||"none-typed-logs"}`,
				logTypes:        []string{"infrastructure"},
			}
			defer cw.deleteResources(oc)
			cw.init(oc)

			compat_otp.By("Create clusterlogforwarder")
			var template string
			if cw.stsEnabled {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
			} else {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
			}
			clf := clusterlogforwarder{
				name:                   "clf-71488",
				namespace:              clfNS,
				templateFile:           template,
				secretName:             cw.secretName,
				collectApplicationLogs: true,
				serviceAccountName:     cw.collectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.createClfSecret(oc)
			clf.create(oc, "REGION="+cw.awsRegion, "GROUP_NAME="+cw.groupName, "INPUT_REFS=[\"application\"]")

			compat_otp.By("Update CLF to add infra projects to application logs")
			patch := `[{"op": "add", "path": "/spec/inputs", "value": [{"name": "new-app", "type": "application", "application": {"includes": [{"namespace": "openshift*"}]}}]}, {"op": "replace", "path": "/spec/pipelines/0/inputRefs", "value": ["new-app"]}]`
			clf.update(oc, "", patch, "--type=json")
			compat_otp.By("CLF should be rejected as the serviceaccount doesn't have sufficient permissions")
			checkResource(oc, true, false, `insufficient permissions on service account, not authorized to collect ["infrastructure"] logs`, []string{"clusterlogforwarder.observability.openshift.io", clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})

			compat_otp.By("Add cluster-role/collect-infrastructure-logs to the serviceaccount")
			defer removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-infrastructure-logs")
			err := addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-infrastructure-logs")
			o.Expect(err).NotTo(o.HaveOccurred())
			//sleep 2 minutes for CLO to update the CLF
			time.Sleep(2 * time.Minute)
			checkResource(oc, false, false, `insufficient permissions on service account, not authorized to collect ["infrastructure"] logs`, []string{"clusterlogforwarder.observability.openshift.io", clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})
			clf.waitForCollectorPodsReady(oc)

			compat_otp.By("Check logs in Cloudwatch, should find some logs from openshift* projects")
			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.nodes = append(cw.nodes, nodes...)
			o.Expect(cw.checkInfraContainerLogs(false)).To(o.BeTrue())
			o.Expect(cw.auditLogsFound(false)).To(o.BeFalse())
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-75417-Validation for multiple CloudWatch outputs in awsAccessKey mode.", func() {
			platform := compat_otp.CheckPlatform(oc)
			if platform != "aws" {
				g.Skip("Skip for the platform is not AWS.")
			}
			if compat_otp.IsSTSCluster(oc) {
				g.Skip("Skip for the cluster have STS enabled.")
			}

			g.By("init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := cloudwatchSpec{
				collectorSAName: "clf-75417",
				secretName:      "clf-75417",
				secretNamespace: clfNS,
				groupName:       "logging-75417-" + infraName + `.{.log_type||"none-typed-logs"}`,
				logTypes:        []string{"application"},
			}
			defer cw.deleteResources(oc)
			cw.init(oc)

			fakeCW := cloudwatchSpec{
				collectorSAName: "clf-75417",
				secretName:      "clf-75417-fake",
				secretNamespace: clfNS,
				groupName:       "logging-75417-" + infraName + "-logs",
				logTypes:        []string{"application"},
			}
			defer fakeCW.deleteResources(oc)
			fakeCW.init(oc)

			compat_otp.By("Create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                   "clf-75417",
				namespace:              clfNS,
				templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml"),
				secretName:             cw.secretName,
				collectApplicationLogs: true,
				waitForPodReady:        true,
				serviceAccountName:     cw.collectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.createClfSecret(oc)
			clf.create(oc, "REGION="+cw.awsRegion, "GROUP_NAME="+cw.groupName, "INPUT_REFS=[\"application\"]")

			compat_otp.By("add one output to the CLF with same same secret")
			patch := `[{"op": "add", "path": "/spec/outputs/-", "value": {"name": "new-cloudwatch-2", "type": "cloudwatch", "cloudwatch": {"authentication": {"type": "awsAccessKey", "awsAccessKey": {"keyId": {"key": "aws_access_key_id", "secretName": "` + cw.secretName + `"}, "keySecret": {"key": "aws_secret_access_key", "secretName": "` + cw.secretName + `"}}}, "groupName": "` + fakeCW.groupName + `", "region": "` + fakeCW.awsRegion + `"}}},{"op": "add", "path": "/spec/pipelines/0/outputRefs/-", "value": "new-cloudwatch-2"}]`
			clf.update(oc, "", patch, "--type=json")
			clf.waitForCollectorPodsReady(oc)

			compat_otp.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			o.Expect(cw.logsFound() && fakeCW.logsFound()).Should(o.BeTrue())

			compat_otp.By("update one of the output to use another secret")
			//since we can't get another aws key pair, here add a secret with fake aws_access_key_id and aws_secret_access_key
			err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", "-n", clf.namespace, fakeCW.secretName, "--from-literal=aws_access_key_id="+getRandomString(), "--from-literal=aws_secret_access_key="+getRandomString()).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			patch = `[{"op": "replace", "path": "/spec/outputs/0/cloudwatch/authentication/awsAccessKey/keyId/secretName", "value": "` + fakeCW.secretName + `"}, {"op": "replace", "path": "/spec/outputs/0/cloudwatch/authentication/awsAccessKey/keySecret/secretName", "value": "` + fakeCW.secretName + `"}]`
			clf.update(oc, "", patch, "--type=json")
			//sleep 10 seconds for collector pods to load new credentials
			time.Sleep(10 * time.Second)
			clf.waitForCollectorPodsReady(oc)

			cw.deleteGroups("")
			fakeCW.deleteGroups("")
			//ensure collector pods still can forward logs to cloudwatch with correct credentials
			o.Expect(cw.logsFound() || fakeCW.logsFound()).Should(o.BeTrue())
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-81604-Support Multiple CloudWatch Outputs with unique STS Role", func() {
			platform := compat_otp.CheckPlatform(oc)
			if platform != "aws" {
				g.Skip("Skip for the platform is not AWS.")
			}
			if !compat_otp.IsSTSCluster(oc) {
				g.Skip("Skip for the cluster doesn't have STS enabled.")
			}
			compat_otp.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("initialize Cloudwatch spec and CLF")
			oc.SetupProject()
			clf := clusterlogforwarder{
				name:                      "clf-81604",
				namespace:                 oc.Namespace(),
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-multiple-iamRole.yaml"),
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "multiple-cw-logcollector",
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)

			cw1 := cloudwatchSpec{
				collectorSAName: clf.serviceAccountName,
				secretName:      "multiple-cw-1",
				secretNamespace: clf.namespace,
				groupName:       "logging-81604-1-" + infraName + `.{.log_type||"none-typed-logs"}`,
				logTypes:        []string{"application"},
			}
			defer cw1.deleteResources(oc)
			cw1.init(oc)
			cw1.createClfSecret(oc)

			cw2 := cloudwatchSpec{
				collectorSAName: clf.serviceAccountName,
				secretName:      "multiple-cw-2",
				secretNamespace: clf.namespace,
				groupName:       "logging-81604-2-" + infraName + `.{.log_type||"none-typed-logs"}`,
				logTypes:        []string{"infrastructure", "audit"},
			}
			defer cw2.deleteResources(oc)
			cw2.init(oc)
			cw2.createClfSecret(oc)

			compat_otp.By("Create clusterlogforwarder")
			clf.create(oc, "SECRET_NAME_1="+cw1.secretName, "GROUP_NAME_1="+cw1.groupName, "SECRET_NAME_2="+cw2.secretName, "GROUP_NAME_2="+cw2.groupName, "REGION_1="+cw1.awsRegion, "REGION_2="+cw2.awsRegion)

			compat_otp.By("Check collector configurations")
			err = oc.AsAdmin().WithoutNamespace().Run("get").Args("configmap", "-n", clf.namespace, clf.name+"-aws-creds").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			checkCollectorConfiguration(oc, clf.namespace, clf.name, `auth.profile = "output_cloudwatch-1"`, `auth.profile = "output_cloudwatch-2"`, `auth.credentials_file = "/var/run/ocp-collector/config/`+clf.name+`-aws-creds/credentials"`)

			compat_otp.By("Check data in cloudwatch")
			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw1.nodes = append(cw1.nodes, nodes...)
			cw2.nodes = append(cw2.nodes, nodes...)
			o.Expect(cw1.logsFound()).Should(o.BeTrue())
			o.Expect(cw2.logsFound()).Should(o.BeTrue())

			o.Expect(cw1.infrastructureLogsFound(false)).Should(o.BeFalse())
			o.Expect(cw1.auditLogsFound(false)).Should(o.BeFalse())
			o.Expect(cw2.applicationLogsFound()).Should(o.BeFalse())

			err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("configmap", "-n", clf.namespace, clf.name+"-aws-creds").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			resource{"configmap", clf.name + "-aws-creds", clf.namespace}.WaitForResourceToAppear(oc)
		})

	})
})
