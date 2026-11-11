package logging

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/clients/aws"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease", func() {
	defer g.GinkgoRecover()
	var (
		oc             = exutil.NewCLIWithoutNamespace("vector-cw")
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
		g.It("Author:qitang-Medium-76074-Forward logs to Cloudwatch group by namespaceName and groupPrefix[CLO]", func() {
			platform := compat_otp.CheckPlatform(oc)
			if platform != "aws" {
				g.Skip("Skip for the platform is not AWS.")
			}
			g.By("Init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := aws.CloudwatchSpec{
				CollectorSAName: "cloudwatch-" + getRandomString(),
				SecretNamespace: clfNS,
				SecretName:      "logging-76074-" + getRandomString(),
				GroupName:       "logging-76074-" + infraName + `.{.kubernetes.namespace_name||.log_type||"none-typed-logs"}`,
				LogTypes:        []string{"infrastructure", "application", "audit"},
			}
			defer cw.DeleteResources(oc)
			cw.Init(oc)

			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.SelAppNamespaces = append(cw.SelAppNamespaces, appProj)
			if !cw.HasMaster {
				nodeName, err := genLinuxAuditLogsOnWorker(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				defer deleteLinuxAuditPolicyFromNode(oc, nodeName)
			}

			g.By("Create clusterlogforwarder")
			var template string
			if cw.StsEnabled {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
			} else {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
			}

			clf := clusterlogforwarder{
				name:                      "clf-76074",
				namespace:                 clfNS,
				templateFile:              template,
				secretName:                cw.SecretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        cw.CollectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.CreateClfSecret(oc)
			clf.create(oc, "REGION="+cw.AwsRegion, "GROUP_NAME="+cw.GroupName)
			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.Nodes = append(cw.Nodes, nodes...)

			g.By("Check logs in Cloudwatch")
			o.Expect(cw.LogsFound()).To(o.BeTrue())
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-76075-Forward logs to Cloudwatch using namespaceUUID and groupPrefix[CLO]", func() {
			platform := compat_otp.CheckPlatform(oc)
			if platform != "aws" {
				g.Skip("Skip for the platform is not AWS.")
			}
			g.By("Init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := aws.CloudwatchSpec{
				CollectorSAName: "cloudwatch-" + getRandomString(),
				SecretNamespace: clfNS,
				SecretName:      "logging-76075-" + getRandomString(),
				GroupName:       "logging-76075-" + infraName + `.{.kubernetes.namespace_id||.log_type||"none-typed-logs"}`,
				LogTypes:        []string{"infrastructure", "application", "audit"},
			}
			defer cw.DeleteResources(oc)
			cw.Init(oc)

			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			g.By("Create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			uuid, err := oc.WithoutNamespace().Run("get").Args("project", appProj, "-ojsonpath={.metadata.uid}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.SelNamespacesID = []string{uuid}
			if !cw.HasMaster {
				nodeName, err := genLinuxAuditLogsOnWorker(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				defer deleteLinuxAuditPolicyFromNode(oc, nodeName)
			}

			g.By("Create clusterlogforwarder")
			var template string
			if cw.StsEnabled {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
			} else {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
			}
			clf := clusterlogforwarder{
				name:                      "clf-76075",
				namespace:                 clfNS,
				templateFile:              template,
				secretName:                cw.SecretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        cw.CollectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.CreateClfSecret(oc)
			clf.create(oc, "REGION="+cw.AwsRegion, "GROUP_NAME="+cw.GroupName)

			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.Nodes = append(cw.Nodes, nodes...)

			g.By("Check logs in Cloudwatch")
			o.Expect(cw.CheckLogGroupByNamespaceID()).To(o.BeTrue())
			o.Expect(cw.InfrastructureLogsFound(false)).To(o.BeTrue())
			o.Expect(cw.AuditLogsFound(false)).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-High-61600-Collector External Cloudwatch output complies with the tlsSecurityProfile configuration[CLO][Slow][Disruptive]", func() {
			compat_otp.By("Check if the current tlsSecurityProfile is the expected one")
			if !compareExpectedTLSConfigWithCurrent(oc, `{"custom":{"ciphers":["ECDHE-ECDSA-CHACHA20-POLY1305","ECDHE-RSA-CHACHA20-POLY1305","ECDHE-RSA-AES128-GCM-SHA256","ECDHE-ECDSA-AES128-GCM-SHA256"],"minTLSVersion":"VersionTLS12"},"type":"Custom"}`) {
				g.Skip("Current tlsSecurityProfile is not the expected one, skipping the test...")
			}

			compat_otp.By("Init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := aws.CloudwatchSpec{
				CollectorSAName: "cloudwatch-" + getRandomString(),
				SecretNamespace: clfNS,
				SecretName:      "logging-61600-" + getRandomString(),
				GroupName:       "logging-61600-" + infraName + `.{.log_type||"none-typed-logs"}`,
				LogTypes:        []string{"infrastructure", "application", "audit"},
			}
			defer cw.DeleteResources(oc)
			cw.Init(oc)

			compat_otp.By("create clusterlogforwarder")
			var template string
			if cw.StsEnabled {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
			} else {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
			}
			clf := clusterlogforwarder{
				name:                      "clf-61600",
				namespace:                 clfNS,
				templateFile:              template,
				secretName:                cw.SecretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        cw.CollectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.CreateClfSecret(oc)
			clf.create(oc, "REGION="+cw.AwsRegion, "GROUP_NAME="+cw.GroupName)
			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.Nodes = append(cw.Nodes, nodes...)

			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			compat_otp.By("Create log producer")
			appProj1 := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj1, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.SelAppNamespaces = []string{appProj1}
			if !cw.HasMaster {
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
			o.Expect(cw.LogsFound()).To(o.BeTrue())
			filteredLogs, err := cw.GetLogRecordsByNamespace(30, logGroupName, appProj1)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(filteredLogs).ShouldNot(o.BeEmpty(), "Couldn't filter logs by namespace")

			compat_otp.By("Set Intermediate tlsSecurityProfile for the Cloudwatch output.")
			patch := `[{"op": "add", "path": "/spec/outputs/0/tls", "value": {"securityProfile": {"type": "Intermediate"}}}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			compat_otp.By("Create log producer")
			oc.SetupProject()
			appProj2 := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj2, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.SelAppNamespaces = []string{appProj2}

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
			o.Expect(cw.LogsFound()).To(o.BeTrue())
			filteredLogs, err = cw.GetLogRecordsByNamespace(30, logGroupName, appProj2)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(filteredLogs).ShouldNot(o.BeEmpty(), "Couldn't filter logs by namespace")
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-71778-Collect or exclude logs by matching pod labels and namespaces[CLO][Slow]", func() {
			platform := compat_otp.CheckPlatform(oc)
			if platform != "aws" {
				g.Skip("Skip for the platform is not AWS.")
			}
			g.By("Init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := aws.CloudwatchSpec{
				CollectorSAName: "cloudwatch-" + getRandomString(),
				SecretNamespace: clfNS,
				SecretName:      "logging-71778-" + getRandomString(),
				GroupName:       "logging-71778-" + infraName + `.{.log_type||"none-typed-logs"}`,
				LogTypes:        []string{"application"},
			}
			defer cw.DeleteResources(oc)
			cw.Init(oc)

			compat_otp.By("Create projects for app logs and deploy the log generators")
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			oc.SetupProject()
			appNS1 := oc.Namespace()
			err := oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", appNS1, "-p", "LABELS={\"test\": \"logging-71778\", \"test.logging.io/logging.qe-test-label\": \"logging-71778-test\"}").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			var namespaces []string
			for i := range 3 {
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
			if cw.StsEnabled {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
			} else {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
			}
			clf := clusterlogforwarder{
				name:                   "clf-71778",
				namespace:              clfNS,
				templateFile:           template,
				secretName:             cw.SecretName,
				collectApplicationLogs: true,
				serviceAccountName:     cw.CollectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.CreateClfSecret(oc)
			clf.create(oc, "REGION="+cw.AwsRegion, "GROUP_NAME="+cw.GroupName, "INPUT_REFS=[\"application\"]")
			patch := `[{"op": "add", "path": "/spec/inputs", "value": [{"name": "myapplogdata", "type": "application", "application": {"selector": {"matchLabels": {"test.logging.io/logging.qe-test-label": "logging-71778-test"}}}}]}, {"op": "replace", "path": "/spec/pipelines/0/inputRefs", "value": ["myapplogdata"]}]`
			clf.update(oc, "", patch, "--type=json")
			clf.waitForCollectorPodsReady(oc)

			compat_otp.By("Check logs in Cloudwatch")
			cw.SelAppNamespaces = []string{namespaces[0], namespaces[1], appNS1}
			cw.DisAppNamespaces = []string{namespaces[2]}
			o.Expect(cw.LogsFound()).To(o.BeTrue())

			compat_otp.By("Update CLF to combine label selector and namespace selector")
			patch = `[{"op": "add", "path": "/spec/inputs/0/application/includes", "value": [{"namespace": "*71778*"}]}, {"op": "add", "path": "/spec/inputs/0/application/excludes", "value": [{"namespace": "` + namespaces[1] + `"}]}]`
			clf.update(oc, "", patch, "--type=json")
			clf.waitForCollectorPodsReady(oc)
			// sleep 10 seconds to wait for the caches in collectors to be cleared
			time.Sleep(10 * time.Second)

			compat_otp.By("Check logs in Cloudwatch")
			newGroupName := "new-logging-71778-" + infraName
			clf.update(oc, "", `[{"op": "replace", "path": "/spec/outputs/0/cloudwatch/GroupName", "value": "`+newGroupName+`"}]`, "--type=json")
			clf.waitForCollectorPodsReady(oc)
			defer cw.DeleteGroups("logging-71778-" + infraName)
			cw.SetGroupName(newGroupName)
			cw.SelAppNamespaces = []string{namespaces[0]}
			cw.DisAppNamespaces = []string{namespaces[1], namespaces[2], appNS1}
			o.Expect(cw.LogsFound()).To(o.BeTrue())
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-71488-Collect container logs from infrastructure projects in an application input[CLO]", func() {
			g.By("Init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := aws.CloudwatchSpec{
				CollectorSAName: "clf-71488",
				SecretName:      "clf-71488",
				SecretNamespace: clfNS,
				GroupName:       "logging-71488-" + infraName + `.{.log_type||"none-typed-logs"}`,
				LogTypes:        []string{"infrastructure"},
			}
			defer cw.DeleteResources(oc)
			cw.Init(oc)

			compat_otp.By("Create clusterlogforwarder")
			var template string
			if cw.StsEnabled {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
			} else {
				template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
			}
			clf := clusterlogforwarder{
				name:                   "clf-71488",
				namespace:              clfNS,
				templateFile:           template,
				secretName:             cw.SecretName,
				collectApplicationLogs: true,
				serviceAccountName:     cw.CollectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.CreateClfSecret(oc)
			clf.create(oc, "REGION="+cw.AwsRegion, "GROUP_NAME="+cw.GroupName, "INPUT_REFS=[\"application\"]")

			compat_otp.By("Update CLF to add infra projects to application logs")
			patch := `[{"op": "add", "path": "/spec/inputs", "value": [{"name": "new-app", "type": "application", "application": {"includes": [{"namespace": "openshift*"}]}}]}, {"op": "replace", "path": "/spec/pipelines/0/inputRefs", "value": ["new-app"]}]`
			clf.update(oc, "", patch, "--type=json")
			compat_otp.By("CLF should be rejected as the serviceaccount doesn't have sufficient permissions")
			checkResource(oc, true, false, `insufficient permissions on service account, not authorized to collect ["infrastructure"] logs`, []string{"clusterlogforwarder.observability.openshift.io", clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})

			compat_otp.By("Add cluster-role/collect-infrastructure-logs to the serviceaccount")
			defer removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-infrastructure-logs")
			err := addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-infrastructure-logs")
			o.Expect(err).NotTo(o.HaveOccurred())
			// sleep 2 minutes for CLO to update the CLF
			time.Sleep(2 * time.Minute)
			checkResource(oc, false, false, `insufficient permissions on service account, not authorized to collect ["infrastructure"] logs`, []string{"clusterlogforwarder.observability.openshift.io", clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})
			clf.waitForCollectorPodsReady(oc)

			compat_otp.By("Check logs in Cloudwatch, should find some logs from openshift* projects")
			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw.Nodes = append(cw.Nodes, nodes...)
			o.Expect(cw.CheckInfraContainerLogs(false)).To(o.BeTrue())
			o.Expect(cw.AuditLogsFound(false)).To(o.BeFalse())
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-75417-Validation for multiple CloudWatch outputs in awsAccessKey mode[CLO]", func() {
			platform := compat_otp.CheckPlatform(oc)
			if platform != "aws" {
				g.Skip("Skip for the platform is not AWS.")
			}
			if compat_otp.IsSTSCluster(oc) {
				g.Skip("Skip for the cluster have STS enabled.")
			}

			g.By("Init Cloudwatch test spec")
			clfNS := oc.Namespace()
			cw := aws.CloudwatchSpec{
				CollectorSAName: "clf-75417",
				SecretName:      "clf-75417",
				SecretNamespace: clfNS,
				GroupName:       "logging-75417-" + infraName + `.{.log_type||"none-typed-logs"}`,
				LogTypes:        []string{"application"},
			}
			defer cw.DeleteResources(oc)
			cw.Init(oc)

			fakeCW := aws.CloudwatchSpec{
				CollectorSAName: "clf-75417",
				SecretName:      "clf-75417-fake",
				SecretNamespace: clfNS,
				GroupName:       "logging-75417-" + infraName + "-logs",
				LogTypes:        []string{"application"},
			}
			defer fakeCW.DeleteResources(oc)
			fakeCW.Init(oc)

			compat_otp.By("Create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                   "clf-75417",
				namespace:              clfNS,
				templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml"),
				secretName:             cw.SecretName,
				collectApplicationLogs: true,
				waitForPodReady:        true,
				serviceAccountName:     cw.CollectorSAName,
			}
			defer clf.delete(oc)
			clf.createServiceAccount(oc)
			cw.CreateClfSecret(oc)
			clf.create(oc, "REGION="+cw.AwsRegion, "GROUP_NAME="+cw.GroupName, "INPUT_REFS=[\"application\"]")

			compat_otp.By("add one output to the CLF with same same secret")
			patch := `[{"op": "add", "path": "/spec/outputs/-", "value": {"name": "new-cloudwatch-2", "type": "cloudwatch", "cloudwatch": {"authentication": {"type": "awsAccessKey", "awsAccessKey": {"keyId": {"key": "aws_access_key_id", "SecretName": "` + cw.SecretName + `"}, "keySecret": {"key": "aws_secret_access_key", "SecretName": "` + cw.SecretName + `"}}}, "GroupName": "` + fakeCW.GroupName + `", "region": "` + fakeCW.AwsRegion + `"}}},{"op": "add", "path": "/spec/pipelines/0/outputRefs/-", "value": "new-cloudwatch-2"}]`
			clf.update(oc, "", patch, "--type=json")
			clf.waitForCollectorPodsReady(oc)

			compat_otp.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			o.Expect(cw.LogsFound() && fakeCW.LogsFound()).Should(o.BeTrue())

			compat_otp.By("update one of the output to use another secret")
			// since we can't get another aws key pair, here add a secret with fake aws_access_key_id and aws_secret_access_key
			err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", "-n", clf.namespace, fakeCW.SecretName, "--from-literal=aws_access_key_id="+getRandomString(), "--from-literal=aws_secret_access_key="+getRandomString()).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			patch = `[{"op": "replace", "path": "/spec/outputs/0/cloudwatch/authentication/awsAccessKey/keyId/SecretName", "value": "` + fakeCW.SecretName + `"}, {"op": "replace", "path": "/spec/outputs/0/cloudwatch/authentication/awsAccessKey/keySecret/SecretName", "value": "` + fakeCW.SecretName + `"}]`
			clf.update(oc, "", patch, "--type=json")
			// sleep 10 seconds for collector pods to load new credentials
			time.Sleep(10 * time.Second)
			clf.waitForCollectorPodsReady(oc)

			cw.DeleteGroups("")
			fakeCW.DeleteGroups("")
			// ensure collector pods still can forward logs to cloudwatch with correct credentials
			o.Expect(cw.LogsFound() || fakeCW.LogsFound()).Should(o.BeTrue())
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-81604-Support Multiple CloudWatch Outputs with unique STS Role[CLO]", func() {
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

			cw1 := aws.CloudwatchSpec{
				CollectorSAName: clf.serviceAccountName,
				SecretName:      "multiple-cw-1",
				SecretNamespace: clf.namespace,
				GroupName:       "logging-81604-1-" + infraName + `.{.log_type||"none-typed-logs"}`,
				LogTypes:        []string{"application"},
			}
			defer cw1.DeleteResources(oc)
			cw1.Init(oc)
			cw1.CreateClfSecret(oc)

			cw2 := aws.CloudwatchSpec{
				CollectorSAName: clf.serviceAccountName,
				SecretName:      "multiple-cw-2",
				SecretNamespace: clf.namespace,
				GroupName:       "logging-81604-2-" + infraName + `.{.log_type||"none-typed-logs"}`,
				LogTypes:        []string{"infrastructure", "audit"},
			}
			defer cw2.DeleteResources(oc)
			cw2.Init(oc)
			cw2.CreateClfSecret(oc)

			compat_otp.By("Create clusterlogforwarder")
			clf.create(oc, "SECRET_NAME_1="+cw1.SecretName, "GROUP_NAME_1="+cw1.GroupName, "SECRET_NAME_2="+cw2.SecretName, "GROUP_NAME_2="+cw2.GroupName, "REGION_1="+cw1.AwsRegion, "REGION_2="+cw2.AwsRegion)

			compat_otp.By("Check collector configurations")
			err = oc.AsAdmin().WithoutNamespace().Run("get").Args("configmap", "-n", clf.namespace, clf.name+"-aws-creds").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", `auth.profile = "output_cloudwatch-1"`, `auth.profile = "output_cloudwatch-2"`, `auth.credentials_file = "/var/run/ocp-collector/config/`+clf.name+`-aws-creds/credentials"`)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the cloudwatch auth configuration is not in vector.toml")

			compat_otp.By("Check data in cloudwatch")
			nodes, err := clf.getCollectorNodeNames(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			cw1.Nodes = append(cw1.Nodes, nodes...)
			cw2.Nodes = append(cw2.Nodes, nodes...)
			o.Expect(cw1.LogsFound()).Should(o.BeTrue())
			o.Expect(cw2.LogsFound()).Should(o.BeTrue())

			o.Expect(cw1.InfrastructureLogsFound(false)).Should(o.BeFalse())
			o.Expect(cw1.AuditLogsFound(false)).Should(o.BeFalse())
			o.Expect(cw2.ApplicationLogsFound()).Should(o.BeFalse())

			err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("configmap", "-n", clf.namespace, clf.name+"-aws-creds").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			resource{"configmap", clf.name + "-aws-creds", clf.namespace}.WaitForResourceToAppear(oc)
		})
	})
})
