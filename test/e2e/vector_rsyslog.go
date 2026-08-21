package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"path/filepath"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("vector-syslog")
		loggingBaseDir string
	)

	g.Context("to Syslog", func() {

		g.BeforeEach(func() {
			loggingBaseDir = testdata.FixturePath("logging")
			CLO := SubscriptionObjects{
				OperatorName:  "cluster-logging-operator",
				Namespace:     cloNS,
				PackageName:   "cluster-logging",
				Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
				OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			}
			g.By("Deploy CLO")
			CLO.SubscribeOperator(oc)
			oc.SetupProject()
		})

		// author gkarager@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:gkarager-High-60699-Vector forward logs to syslog(RFCThirtyOneSixtyFour)", func() {
			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Deploy rsyslog server")
			oc.SetupProject()
			syslogProj := oc.Namespace()
			rsyslog := rsyslog{
				serverName: "rsyslog",
				namespace:  syslogProj,
				tls:        false,
				loggingNS:  syslogProj,
			}
			defer rsyslog.remove(oc)
			rsyslog.deploy(oc)

			g.By("Create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-60699",
				namespace:                 syslogProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "syslog-selected-ns.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "RFC=RFC3164", "URL=udp://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:514", "NAMESPACE_PATTERN="+appProj)

			g.By("Check logs in rsyslog server")
			rsyslog.checkData(oc, true, "app-container.log")
			rsyslog.checkData(oc, true, "infra-container.log")
			rsyslog.checkData(oc, true, "audit.log")
			rsyslog.checkData(oc, true, "infra.log")

			g.By("Verify the syslog content")
			dataPods, err := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: "test=centos-logtest"})
			if err != nil || len(dataPods.Items) < 1 {
				e2e.Failf("failed to get pods by label test=centos-logtest")
			}
			//RFC3164 Format: <PRI>TIMESTAMP HOSTNAME TAG: MESSAGE
			//tag is namespacePod (Vector RFC3164 format truncates at 31 chars in Logging 6.6+)
			tagName := strings.Replace(appProj+dataPods.Items[0].Name, "-", "", -1)[0:31]
			rsyslog.checkDataContent(oc, true, "app-container.log", ` `+tagName+`:`)
			// Validate some key fileds exits
			rsyslog.checkDataContent(oc, true, "app-container.log", `"hostname":`)
			rsyslog.checkDataContent(oc, true, "app-container.log", `"@timestamp":`)
			rsyslog.checkDataContent(oc, true, "app-container.log", `"kubernetes":`)
			rsyslog.checkDataContent(oc, true, "app-container.log", `"container_name":`)
			rsyslog.checkDataContent(oc, true, "app-container.log", `"namespace_name":`)
			rsyslog.checkDataContent(oc, true, "app-container.log", `"pod_name":`)
			rsyslog.checkDataContent(oc, true, "app-container.log", `"message":`)
			rsyslog.checkDataContent(oc, true, "app-container.log", `"openshift":{"cluster_id":`)

			//tag is SYSLOG_IDENTIFIER[ProcId] or SYSLOG_IDENTIFIER
			rsyslog.checkDataContent(oc, true, "infra.log", ` (crio|systemd|kubenswrapper|ovs-vswitchd)\[\d+\]: `)
			rsyslog.checkDataContent(oc, true, "infra.log", `"@timestamp":`)
			rsyslog.checkDataContent(oc, true, "infra.log", `"hostname":`)
			rsyslog.checkDataContent(oc, true, "infra.log", `"systemd":`)
			rsyslog.checkDataContent(oc, true, "infra.log", `"tag":".journal.system"`)
			rsyslog.checkDataContent(oc, true, "infra.log", `"openshift":{"cluster_id":`)

			//tag is log_source for audit logs
			if hasMaster(oc) {
				rsyslog.checkDataContent(oc, true, "audit-openshiftAPI.log", ` openshiftAPI: `)
				rsyslog.checkDataContent(oc, true, "audit-openshiftAPI.log", `@timestamp`)
				rsyslog.checkDataContent(oc, true, "audit-openshiftAPI.log", `"hostname":`)
				rsyslog.checkDataContent(oc, true, "audit-openshiftAPI.log", `"auditID":`)
				rsyslog.checkDataContent(oc, true, "audit-openshiftAPI.log", `"openshift":{"cluster_id":`)
				rsyslog.checkDataContent(oc, true, "audit-kubeAPI.log", ` kubeAPI: `)
				rsyslog.checkDataContent(oc, true, "audit-kubeAPI.log", `@timestamp`)
				rsyslog.checkDataContent(oc, true, "audit-kubeAPI.log", `"hostname":`)
				rsyslog.checkDataContent(oc, true, "audit-kubeAPI.log", `"auditID":`)
				rsyslog.checkDataContent(oc, true, "audit-kubeAPI.log", `"openshift":{"cluster_id":`)
			}
			rsyslog.checkDataContent(oc, true, "audit-linux.log", ` auditd: `)
			rsyslog.checkDataContent(oc, true, "audit-linux.log", `@timestamp`)
			rsyslog.checkDataContent(oc, true, "audit-linux.log", `"hostname":`)
			rsyslog.checkDataContent(oc, true, "audit-linux.log", `"openshift":{"cluster_id":`)
			rsyslog.checkDataContent(oc, true, "audit-linux.log", `"audit.linux":`)
			rsyslog.checkDataContent(oc, true, "audit-linux.log", `"message":`)
		})
		// author anli@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-High-75431-forward logs to Syslog using KubernetesMinimal", func() {
			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Deploy rsyslog server")
			oc.SetupProject()
			syslogProj := oc.Namespace()
			rsyslog := rsyslog{
				serverName: "rsyslog",
				namespace:  syslogProj,
				tls:        false,
				loggingNS:  syslogProj,
			}
			defer rsyslog.remove(oc)
			rsyslog.deploy(oc)

			g.By("Create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-75431",
				namespace:                 syslogProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "syslog-75431.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "RFC=RFC5424", "URL=udp://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:514", "LOG_LEVEL=debug", "NAMESPACE_PATTERN="+appProj)

			g.By("Check logs in rsyslog server")
			rsyslog.checkData(oc, true, "app-container.log")
			rsyslog.checkData(oc, true, "infra-container.log")
			rsyslog.checkData(oc, true, "audit.log")
			rsyslog.checkData(oc, true, "infra.log")

			g.By("Verify the KubernetesMinimal work")
			dataPods, err := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: "test=centos-logtest"})
			if err != nil || len(dataPods.Items) < 1 {
				e2e.Failf("failed to get pods by label test=centos-logtest")
			}
			rsyslog.checkDataContent(oc, true, "app-container.log", "namespace_name="+appProj+", container_name=logging-centos-logtest, pod_name="+dataPods.Items[0].Name)
			rsyslog.checkDataContent(oc, true, "infra-container.log", "namespace_name=.*container_name=.*pod_name=")
			rsyslog.checkDataContent(oc, false, "infra.log", "namespace_name=.*container_name=.*pod_name=")
			rsyslog.checkDataContent(oc, false, "audit.log", "namespace_name=.*container_name=.*pod_name=")

			g.By("Verify the syslog RFC5424 default fields")
			//RFC5424 Format: <PRI> VERSION TIMESTAMP HOSTNAME APP-NAME PROCID MSGID [STRUCTURED-DATA] MESSAGE
			//app_name=namespace_podname_container_name[pod_id]
			appName := appProj + "_" + dataPods.Items[0].Name + `_logging-centos-logtest\[[0-9a-f-]+\]`
			rsyslog.checkDataContent(oc, true, "app-container.log", " "+appName+" ")
			rsyslog.checkDataContent(oc, true, "infra.log", ` (crio|systemd|kubenswrapper|ovs-vswitchd)\[\d+\] `)

			if hasMaster(oc) {
				//app_name is logSource by default.it is logSource[auditID]
				rsyslog.checkDataContent(oc, true, "audit-kubeAPI.log", ` kubeAPI\[[0-9a-f-]+] `)
				rsyslog.checkDataContent(oc, true, "audit-openshiftAPI.log", ` openshiftAPI\[[0-9a-f-]+\] `)
			}
			//app_name is logSource by default.it is logSource
			rsyslog.checkDataContent(oc, true, "audit-linux.log", ` auditd `)
		})

		// author anli@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Medium-75317-forward logs to Syslog customized fields and debug mode", func() {
			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Deploy rsyslog server")
			oc.SetupProject()
			syslogProj := oc.Namespace()
			rsyslog := rsyslog{
				serverName: "rsyslog",
				namespace:  syslogProj,
				tls:        false,
				loggingNS:  syslogProj,
			}
			defer rsyslog.remove(oc)
			rsyslog.deploy(oc)

			g.By("Create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-75317",
				namespace:                 syslogProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "syslog-75317.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "RFC=RFC5424", "URL=udp://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:514", "LOG_LEVEL=debug", "NAMESPACE_PATTERN="+appProj)

			g.By("Check logs in rsyslog server")
			rsyslog.checkData(oc, true, "app-container.log")
			rsyslog.checkData(oc, true, "infra-container.log")
			rsyslog.checkData(oc, true, "audit.log")
			rsyslog.checkData(oc, true, "infra.log")

			g.By("Verify the syslog customized fields")
			dataPods, err := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: "test=centos-logtest"})
			if err != nil || len(dataPods.Items) < 1 {
				e2e.Failf("failed to get pods by label test=centos-logtest")
			}

			//RFC5424 Format: <PRI> VERSION TIMESTAMP HOSTNAME APP-NAME PROCID MSGID [STRUCTURED-DATA] MESSAGE
			//app_name=namespace_podname_container_name by default, here we use appName[proc_id]=containerName[podName]
			rsyslog.checkDataContent(oc, true, "app-container.log", ` logging-centos-logtest\[.+\] `)

			//app_name=namespace_podname_container_name by default, here we appName[proc_id]=containerName[podName]
			rsyslog.checkDataContent(oc, true, "infra-container.log", ` .+\[.+\] `)

			//app_name is SYSLOG_IDENTIFIER[proc_id] by default, here we use appName[proc_id]=logSource[logSource]
			rsyslog.checkDataContent(oc, true, "infra.log", ` node\[node\] `)

			if hasMaster(oc) {
				//app_name is kubeAPI by default. here we use appName[proc_id]=logSource[logSource]
				rsyslog.checkDataContent(oc, true, "audit-kubeAPI.log", ` kubeAPI\[kubeAPI\] `)
				rsyslog.checkDataContent(oc, true, "audit-openshiftAPI.log", ` openshiftAPI\[openshiftAPI\] `)
			}
			//app_name is kubeAPI by default. here we use appName[proc_id]=logSource[logSource]
			rsyslog.checkDataContent(oc, true, "audit-linux.log", ` auditd\[auditd\] `)

			g.By("Check collector expose debug logs")
			collectorPods, err := oc.AdminKubeClient().CoreV1().Pods(clf.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=collector"})
			if err != nil || len(collectorPods.Items) < 1 {
				e2e.Failf("failed to get pods by label app.kubernetes.io/component=collector")
			}
			output, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("-n", clf.namespace, collectorPods.Items[0].Name, "--since=30s", "--tail=30").Output()
			if err != nil {
				e2e.Failf("oc logs collector pod failed. %v", err)
			}
			o.Expect(strings.Contains(output, " DEBUG ")).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:gkarager-WRS-Critical-61479-V-ICA.02-V-ICA.03-Vector forward logs to syslog(tls)", func() {
			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Deploy rsyslog server")
			oc.SetupProject()
			syslogProj := oc.Namespace()
			rsyslog := rsyslog{
				serverName: "rsyslog",
				namespace:  syslogProj,
				tls:        true,
				secretName: "rsyslog-tls",
				loggingNS:  syslogProj,
			}
			defer rsyslog.remove(oc)
			rsyslog.deploy(oc)

			g.By("Create clusterlogforwarder/clf-61479")
			clf := clusterlogforwarder{
				name:                      "clf-61479",
				namespace:                 syslogProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "rsyslog-serverAuth.yaml"),
				secretName:                rsyslog.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "RFC=RFC5424", "URL=tls://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:6514", "NAMESPACE_PATTERN="+appProj)
			g.By("Check logs in rsyslog server")
			rsyslog.checkData(oc, true, "app-container.log")
			rsyslog.checkData(oc, true, "infra-container.log")
			rsyslog.checkData(oc, true, "audit.log")
			rsyslog.checkData(oc, true, "infra.log")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:gkarager-High-61477-Vector-Forward logs to syslog - mtls with private key passphrase", func() {
			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			oc.SetupProject()
			clfNS := oc.Namespace()

			g.By("Deploy rsyslog server")
			oc.SetupProject()
			syslogProj := oc.Namespace()
			rsyslog := rsyslog{
				serverName:          "rsyslog",
				namespace:           syslogProj,
				tls:                 true,
				loggingNS:           clfNS,
				clientKeyPassphrase: "test-rsyslog-mtls",
				secretName:          "rsyslog-mtls",
			}
			defer rsyslog.remove(oc)
			rsyslog.deploy(oc)

			g.By("Create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-61477",
				namespace:                 clfNS,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "rsyslog-mtls.yaml"),
				secretName:                rsyslog.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=tls://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:6514")
			g.By("Check logs in rsyslog server")
			rsyslog.checkData(oc, true, "app-container.log")
			rsyslog.checkData(oc, true, "infra-container.log")
			rsyslog.checkData(oc, true, "audit.log")
			rsyslog.checkData(oc, true, "infra.log")

			searchString := `key_file = "/var/run/ocp-collector/secrets/rsyslog-mtls/tls.key"
crt_file = "/var/run/ocp-collector/secrets/rsyslog-mtls/tls.crt"
ca_file = "/var/run/ocp-collector/secrets/rsyslog-mtls/ca-bundle.crt"`
			result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString, rsyslog.clientKeyPassphrase)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-High-62527-Collector External syslog output complies with the tlsSecurityProfile configuration.[Slow][Disruptive]", func() {

			compat_otp.By("Check if the current tlsSecurityProfile is the expected one")
			if !compareExpectedTLSConfigWithCurrent(oc, `{"custom":{"ciphers":["ECDHE-ECDSA-CHACHA20-POLY1305","ECDHE-RSA-CHACHA20-POLY1305","ECDHE-RSA-AES128-GCM-SHA256","ECDHE-ECDSA-AES128-GCM-SHA256"],"minTLSVersion":"VersionTLS12"},"type":"Custom"}`) {
				g.Skip("Current tlsSecurityProfile is not the expected one, skipping the test...")
			}

			compat_otp.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("Deploy rsyslog server")
			oc.SetupProject()
			syslogProj := oc.Namespace()
			rsyslog := rsyslog{
				serverName: "rsyslog",
				namespace:  syslogProj,
				tls:        true,
				secretName: "rsyslog-tls",
				loggingNS:  syslogProj,
			}
			defer rsyslog.remove(oc)
			rsyslog.deploy(oc)

			compat_otp.By("Create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                      "clf-62527",
				namespace:                 syslogProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "rsyslog-serverAuth.yaml"),
				secretName:                rsyslog.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=tls://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:6514")

			compat_otp.By("The Syslog sink in Vector config must use the Custom tlsSecurityProfile")
			searchString := `[sinks.output_external_syslog.tls]
enabled = true
min_tls_version = "VersionTLS12"
ciphersuites = "ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES128-GCM-SHA256"
ca_file = "/var/run/ocp-collector/secrets/rsyslog-tls/ca-bundle.crt"`
			result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("Check logs in rsyslog server")
			rsyslog.checkData(oc, true, "app-container.log")
			rsyslog.checkData(oc, true, "infra-container.log")
			rsyslog.checkData(oc, true, "audit.log")
			rsyslog.checkData(oc, true, "infra.log")

			compat_otp.By("Set Intermediate tlsSecurityProfile for the External Syslog output.")
			patch := `[{"op": "add", "path": "/spec/outputs/0/tls/securityProfile", "value": {"type": "Intermediate"}}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			compat_otp.By("The Syslog sink in Vector config must use the Intermediate tlsSecurityProfile")
			searchString = `[sinks.output_external_syslog.tls]
enabled = true
min_tls_version = "VersionTLS12"
ciphersuites = "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256,ECDHE-ECDSA-AES128-GCM-SHA256,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES256-GCM-SHA384,ECDHE-RSA-AES256-GCM-SHA384,ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,DHE-RSA-AES128-GCM-SHA256,DHE-RSA-AES256-GCM-SHA384"
ca_file = "/var/run/ocp-collector/secrets/rsyslog-tls/ca-bundle.crt"`
			result, err = checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("Check for errors in collector pod logs.")
			e2e.Logf("Wait for a minute before the collector logs are generated.")
			time.Sleep(60 * time.Second)
			collectorLogs, err := oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("logs").Args("-n", clf.namespace, "--selector=app.kubernetes.io/component=collector").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(strings.Contains(collectorLogs, "Error trying to connect")).ShouldNot(o.BeTrue(), "Unable to connect to the external Syslog server.")

			compat_otp.By("Delete the rsyslog pod to recollect logs")
			err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pods", "-n", syslogProj, "-l", "component=rsyslog").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			waitForPodReadyWithLabel(oc, syslogProj, "component=rsyslog")

			compat_otp.By("Check logs in rsyslog server")
			rsyslog.checkData(oc, true, "app-container.log")
			rsyslog.checkData(oc, true, "infra-container.log")
			rsyslog.checkData(oc, true, "audit.log")
			rsyslog.checkData(oc, true, "infra.log")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-71143-Collect or exclude audit logs.", func() {
			compat_otp.By("Deploy rsyslog server")
			syslogProj := oc.Namespace()
			rsyslog := rsyslog{
				serverName: "rsyslog",
				namespace:  syslogProj,
				tls:        true,
				secretName: "rsyslog-tls",
				loggingNS:  syslogProj,
			}
			defer rsyslog.remove(oc)
			rsyslog.deploy(oc)

			compat_otp.By("Create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:               "clf-71143",
				namespace:          syslogProj,
				templateFile:       filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "rsyslog-serverAuth.yaml"),
				secretName:         rsyslog.secretName,
				waitForPodReady:    true,
				collectAuditLogs:   true,
				serviceAccountName: "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=tls://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:6514", "INPUTREFS=[\"audit\"]")

			compat_otp.By("Check logs in rsyslog server")
			rsyslog.checkData(oc, true, "audit.log")

			compat_otp.By("Update CLF to collect linux audit logs")
			patch := `[{"op": "add", "path": "/spec/inputs", "value": [{"name": "selected-audit", "type": "audit", "audit": {"sources":["auditd"]}}]},{"op": "replace", "path": "/spec/pipelines/0/inputRefs", "value": ["selected-audit"]}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
			// sleep 10 seconds for collector pods to send the cached records
			time.Sleep(10 * time.Second)
			_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pod", "-n", rsyslog.namespace, "-l", "component="+rsyslog.serverName).Execute()
			WaitForDeploymentPodsToBeReady(oc, rsyslog.namespace, rsyslog.serverName)
			nodeName, err := genLinuxAuditLogsOnWorker(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			defer deleteLinuxAuditPolicyFromNode(oc, nodeName)
			compat_otp.By("Check data in log store, only linux audit logs should be collected")
			rsyslog.checkData(oc, true, "audit-linux.log")
			rsyslog.checkData(oc, false, "audit-ovn.log")
			if hasMaster(oc) {
				rsyslog.checkData(oc, false, "audit-kubeAPI.log")
				rsyslog.checkData(oc, false, "audit-openshiftAPI.log")
			}

			compat_otp.By("Update CLF to collect kubeAPI audit logs")
			patch = `[{"op": "replace", "path": "/spec/inputs/0/audit/sources", "value": ["kubeAPI"]}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
			// sleep 10 seconds for collector pods to send the cached records
			time.Sleep(10 * time.Second)
			_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pod", "-n", rsyslog.namespace, "-l", "component="+rsyslog.serverName).Execute()
			WaitForDeploymentPodsToBeReady(oc, rsyslog.namespace, rsyslog.serverName)
			compat_otp.By("Check data in log store, only kubeAPI audit logs should be collected")
			rsyslog.checkData(oc, false, "audit-ovn.log")
			if hasMaster(oc) {
				rsyslog.checkData(oc, true, "audit-kubeAPI.log")
				rsyslog.checkData(oc, false, "audit-linux.log")
				rsyslog.checkData(oc, false, "audit-openshiftAPI.log")

				compat_otp.By("Update CLF to collect openshiftAPI audit logs")
				patch = `[{"op": "replace", "path": "/spec/inputs/0/audit/sources", "value": ["openshiftAPI"]}]`
				clf.update(oc, "", patch, "--type=json")
				WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
				// sleep 10 seconds for collector pods to send the cached records
				time.Sleep(10 * time.Second)
				_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pod", "-n", rsyslog.namespace, "-l", "component="+rsyslog.serverName).Execute()
				WaitForDeploymentPodsToBeReady(oc, rsyslog.namespace, rsyslog.serverName)
				compat_otp.By("Check data in log store, only openshiftAPI audit logs should be collected")
				rsyslog.checkData(oc, true, "audit-openshiftAPI.log")
				rsyslog.checkData(oc, false, "audit-kubeAPI.log")
				rsyslog.checkData(oc, false, "audit-linux.log")
			}
			rsyslog.checkData(oc, false, "audit-ovn.log")

			if strings.Contains(checkNetworkType(oc), "ovnkubernetes") {
				compat_otp.By("Update CLF to collect OVN audit logs")
				patch := `[{"op": "replace", "path": "/spec/inputs/0/audit/sources", "value": ["ovn"]}]`
				clf.update(oc, "", patch, "--type=json")
				WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
				// sleep 10 seconds for collector pods to send the cached records
				time.Sleep(10 * time.Second)
				_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pod", "-n", rsyslog.namespace, "-l", "component="+rsyslog.serverName).Execute()
				WaitForDeploymentPodsToBeReady(oc, rsyslog.namespace, rsyslog.serverName)

				compat_otp.By("Create a test project, enable OVN network log collection on it, add the OVN log app and network policies for the project")
				oc.SetupProject()
				ovnProj := oc.Namespace()
				ovn := resource{"deployment", "ovn-app", ovnProj}
				ovnAuditTemplate := filepath.Join(loggingBaseDir, "generatelog", "42981.yaml")
				err := ovn.applyFromTemplate(oc, "-n", ovn.namespace, "-f", ovnAuditTemplate, "-p", "NAMESPACE="+ovn.namespace)
				o.Expect(err).NotTo(o.HaveOccurred())
				WaitForDeploymentPodsToBeReady(oc, ovnProj, ovn.name)

				g.By("Access the OVN app pod from another pod in the same project to generate OVN ACL messages")
				ovnPods, err := oc.AdminKubeClient().CoreV1().Pods(ovnProj).List(context.Background(), metav1.ListOptions{LabelSelector: "app=ovn-app"})
				o.Expect(err).NotTo(o.HaveOccurred())
				podIP := ovnPods.Items[0].Status.PodIP
				e2e.Logf("Pod IP is %s ", podIP)
				var ovnCurl string
				if strings.Contains(podIP, ":") {
					ovnCurl = "curl --globoff [" + podIP + "]:8080"
				} else {
					ovnCurl = "curl --globoff " + podIP + ":8080"
				}
				_, err = e2eoutput.RunHostCmdWithRetries(ovnProj, ovnPods.Items[1].Name, ovnCurl, 3*time.Second, 30*time.Second)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Check for the generated OVN audit logs on the OpenShift cluster nodes")
				nodeLogs, err := oc.AsAdmin().WithoutNamespace().Run("adm").Args("-n", ovnProj, "node-logs", "-l", "beta.kubernetes.io/os=linux", "--path=/ovn/acl-audit-log.log").Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(strings.Contains(nodeLogs, ovnProj)).Should(o.BeTrue(), "The OVN logs doesn't contain logs from project %s", ovnProj)

				compat_otp.By("Check data in log store, only ovn audit logs should be collected")
				rsyslog.checkData(oc, true, "audit-ovn.log")
				if hasMaster(oc) {
					rsyslog.checkData(oc, false, "audit-kubeAPI.log")
					rsyslog.checkData(oc, false, "audit-openshiftAPI.log")
				}
				rsyslog.checkData(oc, false, "audit-linux.log")
			}

		})
		// author anli@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Medium-81512-Syslog output payloadKey customized", func() {
			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Deploy rsyslog server")
			oc.SetupProject()
			syslogProj := oc.Namespace()
			rsyslog := rsyslog{
				serverName: "rsyslog",
				namespace:  syslogProj,
				tls:        true,
				secretName: "rsyslog-tls",
				loggingNS:  syslogProj,
			}
			defer rsyslog.remove(oc)
			rsyslog.deploy(oc)

			g.By("Create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-81512",
				namespace:                 syslogProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "syslog-81512.yaml"),
				secretName:                rsyslog.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          false,
				collectInfrastructureLogs: false,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			//PAYLOAD_KEY can be .message; .a.b.c and etc
			clf.create(oc, "URL=tls://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:6514", "LOG_LEVEL=off", "NAMESPACE_PATTERN="+appProj, "PAYLOAD_KEY=.message")

			g.By("Check logs in rsyslog server")
			//Logs are not sent to app-container.log as payload_key=.message
			rsyslog.checkData(oc, false, "app-container.log")
			rsyslog.checkData(oc, true, "other.log")

			g.By("Verify the syslog payloadKey fields")
			//Only message is included. the @timestemp, kubernetes,metadata, openshift.cluster_id are dropped
			//2025-04-21T08:16:43+00:00 ip-10-0-136-217.ec2.internal e2etestvectorsyslog9znqjloggingc: MERGE_JSON_LOG=true
			rsyslog.checkDataContent(oc, true, "other.log", `MERGE_JSON_LOG=true`)
			rsyslog.checkDataContent(oc, false, "other.log", `timestamp`)
			rsyslog.checkDataContent(oc, false, "other.log", `kubernetes`)
			rsyslog.checkDataContent(oc, false, "other.log", `cluster_id`)

			g.By("Check collector not expose debug logs")
			collectorPods, err := oc.AdminKubeClient().CoreV1().Pods(clf.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=collector"})
			if err != nil || len(collectorPods.Items) < 1 {
				e2e.Failf("failed to get pods by label app.kubernetes.io/component=collector")
			}
			output, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("-n", clf.namespace, collectorPods.Items[0].Name, "--since=30s", "--tail=30").Output()
			if err != nil {
				e2e.Failf("oc logs collector pod failed. %v", err)
			}
			o.Expect(strings.Contains(output, " DEBUG ")).To(o.BeFalse())
		})
	})
})
