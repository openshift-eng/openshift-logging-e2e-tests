package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("vector-splunk")
		loggingBaseDir string
	)
	g.Context("Log Forward to Splunk", func() {
		// author anli@redhat.com
		g.BeforeEach(func() {
			nodes, err := oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "kubernetes.io/os=linux,kubernetes.io/arch=amd64"})
			if err != nil || len(nodes.Items) == 0 {
				g.Skip("Skip for the cluster doesn't have amd64 node")
			}

			loggingBaseDir = testdata.FixturePath("logging")
			g.By("deploy CLO")
			CLO := SubscriptionObjects{
				OperatorName:  "cluster-logging-operator",
				Namespace:     cloNS,
				PackageName:   "cluster-logging",
				Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
				OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			}
			CLO.SubscribeOperator(oc)
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-High-54980-Vector forward logs to Splunk 9.0 over HTTP", func() {
			oc.SetupProject()
			splunkProject := oc.Namespace()
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "default-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()
			// The secret used in CLF to splunk server
			clfSecret := toSplunkSecret{
				name:       "to-splunk-secret-54980",
				namespace:  splunkProject,
				hecToken:   sp.hecToken,
				caFile:     "",
				keyFile:    "",
				certFile:   "",
				passphrase: "",
			}

			clf := clusterlogforwarder{
				name:                      "clf-54980",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")

			g.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			g.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name, "INDEX=main")

			g.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("check logs in splunk")
			o.Expect(sp.anyLogFound()).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Medium-56248-vector forward logs to splunk 8.2 over TLS - SkipVerify", func() {
			oc.SetupProject()
			splunkProject := oc.Namespace()
			keysPath := filepath.Join("/tmp/temp" + getRandomString())
			sp := splunkPodServer{
				namespace:  splunkProject,
				name:       "splunk-https",
				authType:   "tls_serveronly",
				version:    "8.2",
				caFile:     keysPath + "/ca.crt",
				keyFile:    keysPath + "/server.key",
				certFile:   keysPath + "/server.crt",
				passphrase: "",
			}
			sp.init()
			// The secret used in CLF to splunk server
			clfSecret := toSplunkSecret{
				name:       "to-splunk-secret-56248",
				namespace:  splunkProject,
				hecToken:   sp.hecToken,
				caFile:     keysPath + "/fake_ca.crt",
				keyFile:    "",
				certFile:   "",
				passphrase: "",
			}

			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")

			g.By("generate fake certifate for testing")
			defer exec.Command("rm", "-r", keysPath).Output()
			err := os.MkdirAll(keysPath, 0755)
			o.Expect(err).NotTo(o.HaveOccurred())
			_, err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("cm/kube-root-ca.crt", "-n", clfSecret.namespace, "--confirm", "--to="+keysPath).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			err = os.Rename(keysPath+"/ca.crt", clfSecret.caFile)
			o.Expect(err).NotTo(o.HaveOccurred())
			cert := certsConf{sp.serviceName, sp.namespace, sp.passphrase}
			cert.generateCerts(oc, keysPath)

			g.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			g.By("create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                      "clf-56248",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk-serveronly.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=https://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name, "SKIP_VERIFY=true")

			g.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("check logs in splunk")
			o.Expect(sp.anyLogFound()).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Critical-54976-vector forward logs to splunk 9.0 over TLS - ServerOnly", func() {
			oc.SetupProject()
			splunkProject := oc.Namespace()
			keysPath := filepath.Join("/tmp/temp" + getRandomString())
			sp := splunkPodServer{
				namespace:  splunkProject,
				name:       "splunk-https",
				authType:   "tls_serveronly",
				version:    "9.0",
				caFile:     keysPath + "/ca.crt",
				keyFile:    keysPath + "/server.key",
				certFile:   keysPath + "/server.crt",
				passphrase: "",
			}
			sp.init()
			// The secret used in CLF to splunk server
			clfSecret := toSplunkSecret{
				name:       "to-splunk-secret-55976",
				namespace:  splunkProject,
				hecToken:   sp.hecToken,
				caFile:     keysPath + "/ca.crt",
				keyFile:    "",
				certFile:   "",
				passphrase: "",
			}

			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")

			g.By("Generate certifcate for testing")
			defer exec.Command("rm", "-r", keysPath).Output()
			err := os.MkdirAll(keysPath, 0755)
			o.Expect(err).NotTo(o.HaveOccurred())
			cert := certsConf{sp.serviceName, sp.namespace, sp.passphrase}
			cert.generateCerts(oc, keysPath)

			g.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			g.By("create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                      "clf-55976",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk-serveronly.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=https://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name)

			g.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("check logs in splunk")
			o.Expect(sp.allTypeLogsFound()).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Medium-54978-vector forward logs to splunk 8.2 over TLS - Client Key Passphase", func() {
			oc.SetupProject()
			splunkProject := oc.Namespace()
			keysPath := filepath.Join("/tmp/temp" + getRandomString())
			sp := splunkPodServer{
				namespace:  splunkProject,
				name:       "splunk-https",
				authType:   "tls_clientauth",
				version:    "8.2",
				caFile:     keysPath + "/ca.crt",
				keyFile:    keysPath + "/server.key",
				certFile:   keysPath + "/server.crt",
				passphrase: "aosqetmp",
			}
			sp.init()
			clfSecret := toSplunkSecret{
				name:       "to-splunk-secret-54978",
				namespace:  splunkProject,
				hecToken:   sp.hecToken,
				caFile:     keysPath + "/ca.crt",
				keyFile:    keysPath + "/client.key",
				certFile:   keysPath + "/client.crt",
				passphrase: sp.passphrase,
			}

			clf := clusterlogforwarder{
				name:                      "clf-54978",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk-mtls-passphrase.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")

			g.By("Generate certifcate for testing")
			defer exec.Command("rm", "-r", keysPath).Output()
			err := os.MkdirAll(keysPath, 0755)
			o.Expect(err).NotTo(o.HaveOccurred())
			cert := certsConf{sp.serviceName, sp.namespace, sp.passphrase}
			cert.generateCerts(oc, keysPath)

			g.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			g.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=https://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name)

			g.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("check logs in splunk")
			o.Expect(sp.anyLogFound()).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Medium-54979-vector forward logs to splunk 9.0 over TLS - ClientAuth", func() {
			oc.SetupProject()
			splunkProject := oc.Namespace()
			keysPath := filepath.Join("/tmp/temp" + getRandomString())
			sp := splunkPodServer{
				namespace:  splunkProject,
				name:       "splunk-https",
				authType:   "tls_clientauth",
				version:    "9.0",
				caFile:     keysPath + "/ca.crt",
				keyFile:    keysPath + "/server.key",
				certFile:   keysPath + "/server.crt",
				passphrase: "",
			}
			sp.init()
			clfSecret := toSplunkSecret{
				name:       "to-splunk-secret-54979",
				namespace:  splunkProject,
				hecToken:   sp.hecToken,
				caFile:     keysPath + "/ca.crt",
				keyFile:    keysPath + "/client.key",
				certFile:   keysPath + "/client.crt",
				passphrase: "",
			}
			clf := clusterlogforwarder{
				name:                      "clf-54979",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk-mtls.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")

			g.By("Generate certifcate for testing")
			defer exec.Command("rm", "-r", keysPath).Output()
			err := os.MkdirAll(keysPath, 0755)
			o.Expect(err).NotTo(o.HaveOccurred())

			cert := certsConf{sp.serviceURL, sp.namespace, ""}
			cert.generateCerts(oc, keysPath)

			g.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			g.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=https://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name)

			g.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("check logs in splunk")
			o.Expect(sp.anyLogFound()).To(o.BeTrue())
		})

	})

	g.Context("Splunk Custom Tenant", func() {
		g.BeforeEach(func() {
			nodes, err := oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "kubernetes.io/os=linux,kubernetes.io/arch=amd64"})
			if err != nil || len(nodes.Items) == 0 {
				g.Skip("Skip for the cluster doesn't have amd64 node")
			}

			loggingBaseDir = testdata.FixturePath("logging")
			compat_otp.By("deploy CLO")
			CLO := SubscriptionObjects{
				OperatorName:  "cluster-logging-operator",
				Namespace:     cloNS,
				PackageName:   "cluster-logging",
				Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
				OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			}
			CLO.SubscribeOperator(oc)
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-High-71028-Forward logs to Splunk index by setting indexName", func() {
			oc.SetupProject()
			splunkProject := oc.Namespace()
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "default-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()
			indexName := "custom-index-" + getRandomString()
			compat_otp.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)
			errIndex := sp.createIndexes(oc, indexName)
			o.Expect(errIndex).NotTo(o.HaveOccurred())

			compat_otp.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			clfSecret := toSplunkSecret{
				name:      "splunk-secret-71028",
				namespace: splunkProject,
				hecToken:  sp.hecToken,
			}
			clf := clusterlogforwarder{
				name:                      "clf-71028",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			compat_otp.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name, "INDEX="+indexName)

			compat_otp.By("check logs in splunk")
			for _, logType := range []string{"application", "audit", "infrastructure"} {
				o.Expect(sp.checkLogs("index=\""+indexName+"\" log_type=\""+logType+"\"")).To(o.BeTrue(), "can't find "+logType+" logs in "+indexName+" index")
				r, e := sp.searchLogs("index=\"main\" log_type=\"" + logType + "\"")
				o.Expect(e).NotTo(o.HaveOccurred())
				o.Expect(len(r.Results) == 0).Should(o.BeTrue(), "find "+logType+" logs in default index, this is not expected")
			}
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-71029-Forward logs to Splunk indexes by kubernetes.namespace_name[Slow]", func() {
			compat_otp.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			oc.SetupProject()
			splunkProject := oc.Namespace()
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "default-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()

			compat_otp.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)
			var indexes []string
			namespaces, err := oc.AdminKubeClient().CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			for _, ns := range namespaces.Items {
				if ns.Name != "default" {
					indexes = append(indexes, ns.Name)
				}
			}
			errIndex := sp.createIndexes(oc, indexes...)
			o.Expect(errIndex).NotTo(o.HaveOccurred())

			clfSecret := toSplunkSecret{
				name:      "splunk-secret-71029",
				namespace: splunkProject,
				hecToken:  sp.hecToken,
			}
			clf := clusterlogforwarder{
				name:                      "clf-71029",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			compat_otp.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name, "INDEX={.kubernetes.namespace_name||\"\"}")

			compat_otp.By("check logs in splunk")
			// not all of the projects in cluster have container logs, so here only check some of the projects
			// container logs should only be stored in the index named as it's namespace name
			for _, index := range []string{appProj, "openshift-cluster-version", "openshift-dns", "openshift-ingress", "openshift-monitoring"} {
				o.Expect(sp.checkLogs("index=\""+index+"\"")).To(o.BeTrue(), "can't find logs in "+index+" index")
				r, e := sp.searchLogs("index=\"" + index + "\" kubernetes.namespace_name!=\"" + index + "\"")
				o.Expect(e).NotTo(o.HaveOccurred())
				o.Expect(len(r.Results) == 0).Should(o.BeTrue(), "find logs from other namespaces in "+index+" index, this is not expected")
				r, e = sp.searchLogs("index!=\"" + index + "\" kubernetes.namespace_name=\"" + index + "\"")
				o.Expect(e).NotTo(o.HaveOccurred())
				o.Expect(len(r.Results) == 0).Should(o.BeTrue(), "find logs from project "+index+" in other indexes, this is not expected")
			}
			// audit logs and journal logs should be stored in the default index, which is named main
			for _, logType := range []string{"audit", "infrastructure"} {
				o.Expect(sp.checkLogs("index=\"main\" log_type=\""+logType+"\"")).To(o.BeTrue(), "can't find "+logType+" logs in main index")
			}
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-71031-Forward logs to Splunk indexes by openshift.labels", func() {
			compat_otp.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			oc.SetupProject()
			splunkProject := oc.Namespace()
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "default-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()

			compat_otp.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			index := "multi_splunk-indexes_71031"
			errIndex := sp.createIndexes(oc, index)
			o.Expect(errIndex).NotTo(o.HaveOccurred())

			clfSecret := toSplunkSecret{
				name:      "splunk-secret-71031",
				namespace: splunkProject,
				hecToken:  sp.hecToken,
			}
			clf := clusterlogforwarder{
				name:                      "clf-71031",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk.yaml"),
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			compat_otp.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name, "INDEX={.openshift.labels.test||\"\"}")
			patch := `[{"op": "add", "path": "/spec/filters", "value": [{"name": "labels", "type": "openshiftLabels", "openshiftLabels": {"test": "` + index + `"}}]}, {"op": "add", "path": "/spec/pipelines/0/filterRefs", "value": ["labels"]}]`
			clf.update(oc, "", patch, "--type=json")
			clf.waitForCollectorPodsReady(oc)

			//sleep 10 seconds for collector pods to send logs to splunk
			time.Sleep(10 * time.Second)
			compat_otp.By("check logs in splunk")
			for _, logType := range []string{"infrastructure", "application", "audit"} {
				o.Expect(sp.checkLogs("index=\""+index+"\" log_type=\""+logType+"\"")).To(o.BeTrue(), "can't find "+logType+" logs in "+index+" index")
			}

			for _, logType := range []string{"application", "infrastructure", "audit"} {
				r, e := sp.searchLogs("index=\"main\" log_type=\"" + logType + "\"")
				o.Expect(e).NotTo(o.HaveOccurred())
				o.Expect(len(r.Results) == 0).Should(o.BeTrue(), "find "+logType+" logs in default index, this is not expected")
			}
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-71035-Forward logs to Splunk indexes by kubernetes.labels", func() {
			compat_otp.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate, "-p", `LABELS={"test-logging": "logging-OCP_71035", "test.logging.io/logging.qe-test-label": "logging-OCP_71035"}`).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			oc.SetupProject()
			splunkProject := oc.Namespace()
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "default-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()

			compat_otp.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			index := "logging-OCP_71035"
			errIndex := sp.createIndexes(oc, index)
			o.Expect(errIndex).NotTo(o.HaveOccurred())

			clfSecret := toSplunkSecret{
				name:      "splunk-secret-71035",
				namespace: splunkProject,
				hecToken:  sp.hecToken,
			}
			clf := clusterlogforwarder{
				name:                      "clf-71035",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			compat_otp.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name, "INDEX={.kubernetes.labels.\"test.logging.io/logging.qe-test-label\"||\"\"}")

			compat_otp.By("check logs in splunk")
			// logs from project appProj should be stored in 'logging-OCP_71035', other logs should be in default index
			o.Expect(sp.checkLogs("index=\""+index+"\"")).To(o.BeTrue(), "can't find logs in "+index+" index")
			r, e := sp.searchLogs("index=\"" + index + "\", kubernetes.namespace_name!=\"" + appProj + "\"")
			o.Expect(e).NotTo(o.HaveOccurred())
			o.Expect(len(r.Results) == 0).Should(o.BeTrue(), "find logs from other namespaces in "+index+" index, this is not expected")
			r, e = sp.searchLogs("index!=\"" + index + "\", kubernetes.namespace_name=\"" + appProj + "\"")
			o.Expect(e).NotTo(o.HaveOccurred())
			o.Expect(len(r.Results) == 0).Should(o.BeTrue(), "find logs from project "+appProj+" in other indexes, this is not expected")

			for _, logType := range []string{"audit", "infrastructure"} {
				o.Expect(sp.checkLogs("index=\"main\", log_type=\""+logType+"\"")).To(o.BeTrue(), "can't find "+logType+" logs in main index")
				r, e := sp.searchLogs("index=\"" + index + "\", log_type=\"" + logType + "\"")
				o.Expect(e).NotTo(o.HaveOccurred())
				o.Expect(len(r.Results) == 0).Should(o.BeTrue(), "find "+logType+" logs in "+index+" index, this is not expected")
			}
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-High-75234-logs fallback to default splunk index if template syntax can not be found", func() {
			compat_otp.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate, "-p", "LABELS={\"test-logging\": \"logging-OCP-71322\"}").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			oc.SetupProject()
			splunkProject := oc.Namespace()
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "default-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()

			compat_otp.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)
			o.Expect(sp.createIndexes(oc, appProj)).NotTo(o.HaveOccurred())
			o.Expect(sp.createIndexes(oc, "openshift-operator-lifecycle-manager")).NotTo(o.HaveOccurred())

			clfSecret := toSplunkSecret{
				name:      "splunk-secret-71322",
				namespace: splunkProject,
				hecToken:  sp.hecToken,
			}
			clf := clusterlogforwarder{
				name:                      "clf-71322",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			compat_otp.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name, "INDEX={.kubernetes.namespace_name||\"main\"}")

			compat_otp.By("verify logs can are found in namespace_name index")
			allFound := true
			for _, logIndex := range []string{appProj, "openshift-operator-lifecycle-manager"} {
				if sp.checkLogs("index=" + logIndex) {
					e2e.Logf("found logs in index %s", logIndex)
				} else {
					e2e.Logf("can not find logs in index %s", logIndex)
					allFound = false
				}
			}
			o.Expect(allFound).To(o.BeTrue(), "can't find some logs in namespace_name index ")

			compat_otp.By("verify infra and audit logs are send to main index")
			allFound = true
			for _, logType := range []string{"audit", "infrastructure"} {
				if sp.checkLogs(`index="main" log_type="` + logType + `"`) {
					e2e.Logf("found logs %s in index main", logType)
				} else {
					e2e.Logf("Can not find logs %s in index main ", logType)
					allFound = false
				}
			}
			o.Expect(allFound).To(o.BeTrue(), "can't find some type of logs in main index")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Critical-68303-mCLF Inputs.receiver.http multiple Inputs.receivers to splunk", func() {
			oc.SetupProject()
			clfNS := oc.Namespace()
			splunkProject := clfNS

			g.By("Deploy splunk server")
			//define splunk deployment
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "splunk-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()

			defer sp.destroy(oc)
			sp.deploy(oc)

			g.By("create clusterlogforwarder/instance")
			// The secret used in CLF to splunk server
			clfSecret := toSplunkSecret{
				name:       "to-splunk-secret-68303",
				namespace:  clfNS,
				hecToken:   sp.hecToken,
				caFile:     "",
				keyFile:    "",
				certFile:   "",
				passphrase: "",
			}
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			clf := clusterlogforwarder{
				name:                      "http-to-splunk",
				namespace:                 clfNS,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "httpserver-to-splunk.yaml"),
				secretName:                clfSecret.name,
				serviceAccountName:        "clf-" + getRandomString(),
				waitForPodReady:           true,
				collectAuditLogs:          false,
				collectApplicationLogs:    false,
				collectInfrastructureLogs: false,
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088")

			g.By("send data to httpservers")
			o.Expect(postDataToHttpserver(oc, "https://"+clf.name+"-httpserver1."+clfNS+".svc:8081", `{"test data" : "from httpserver1"}`)).To(o.BeTrue())
			o.Expect(postDataToHttpserver(oc, "https://"+clf.name+"-httpserver2."+clfNS+".svc:8082", `{"test data" : "from httpserver2"}`)).To(o.BeTrue())
			o.Expect(postDataToHttpserver(oc, "https://"+clf.name+"-httpserver3."+clfNS+".svc:8083", `{"test data" : "from httpserver3"}`)).To(o.BeTrue())

			g.By("check logs in splunk")
			o.Expect(sp.auditLogFound()).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Medium-75386-ClusterLogForwarder input validation testing.", func() {
			oc.SetupProject()
			splunkProject := oc.Namespace()
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "default-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()

			compat_otp.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			clfSecret := toSplunkSecret{
				name:      "splunk-secret-75386",
				namespace: splunkProject,
				hecToken:  sp.hecToken,
			}
			clf := clusterlogforwarder{
				name:                      "clf-75386",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			compat_otp.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name, "INDEX={.kubernetes.non_existing.key||\"\"}")

			compat_otp.By("update CLF to set invalid glob for namespace")
			patch := `[{"op":"add","path":"/spec/inputs","value":[{"name":"new-app","type":"application","application":{"excludes":[{"namespace":"invalid-name@"}],"includes":[{"namespace":"tes*t"}]}}]},{"op":"replace","path":"/spec/pipelines/0/inputRefs","value":["new-app"]}]`
			clf.update(oc, "", patch, "--type=json")
			checkResource(oc, true, false, "globs must match", []string{"clusterlogforwarder.observability.openshift.io", clf.name, "-n", clf.namespace, "-ojsonpath={.status.inputConditions[0].message}"})

			compat_otp.By("update CLF to set invalid sources for infrastructure logs")
			patch = `[{"op":"replace","path":"/spec/inputs","value":[{"name":"selected-infra","type":"infrastructure","infrastructure":{"sources":["nodesd","containersf"]}}]},{"op":"replace","path":"/spec/pipelines/0/inputRefs","value":["selected-infra"]}]`
			outString, _ := clf.patch(oc, patch)
			o.Expect(strings.Contains(outString, `The ClusterLogForwarder "clf-75386" is invalid`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `Unsupported value: "nodesd": supported values: "container", "node"`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `Unsupported value: "containersf": supported values: "container", "node"`)).To(o.BeTrue())

			compat_otp.By("update CLF to set invalid sources for audit logs")
			patch = `[{"op":"replace","path":"/spec/pipelines/0/inputRefs","value":["selected-audit"]},{"op":"replace","path":"/spec/inputs","value":[{"name":"selected-audit","type":"audit","audit":{"sources":["nodess","containersf"]}}]}]`
			outString, _ = clf.patch(oc, patch)
			o.Expect(strings.Contains(outString, `The ClusterLogForwarder "clf-75386" is invalid`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `Unsupported value: "nodess": supported values: "auditd", "kubeAPI", "openshiftAPI", "ovn"`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `Unsupported value: "containersf": supported values: "auditd", "kubeAPI", "openshiftAPI", "ovn"`)).To(o.BeTrue())

			compat_otp.By("update CLF to use string as matchExpressions values")
			patch = `[{"op":"replace","path":"/spec/inputs/0/application","value":{"selector":{"matchExpressions":[{"key":"test.logging.io/logging.qe-test-label","operator":"Exists","values":"logging-71749-test-1"}]}}}]`
			outString, _ = clf.patch(oc, patch)
			o.Expect(strings.Contains(outString, `The ClusterLogForwarder "clf-75386" is invalid`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `spec.inputs[0].application.selector.matchExpressions[0].values: Invalid value: "string"`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `spec.inputs[0].application.selector.matchExpressions[0].values in body must be of type array: "string"`)).To(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-75390-CLF should be rejected and show error message if the filters are invalid", func() {
			oc.SetupProject()
			splunkProject := oc.Namespace()
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "default-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()

			compat_otp.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			clfSecret := toSplunkSecret{
				name:      "splunk-secret-75390",
				namespace: splunkProject,
				hecToken:  sp.hecToken,
			}
			clf := clusterlogforwarder{
				name:                      "clf-75390",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			compat_otp.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name)

			compat_otp.By("verfy clf without drop spec is rejected")
			patch := `[{"op":"add","path":"/spec/filters","value":[{"name":"drop-logs","type":"drop"}]},{"op":"add","path":"/spec/pipelines/0/filterRefs","value":["drop-logs"]}]`
			outString, _ := clf.patch(oc, patch)
			o.Expect(strings.Contains(outString, `The ClusterLogForwarder "clf-75390" is invalid:`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `invalid: spec.filters[0]: Invalid value: "object": Additional type specific spec is required for the filter type`)).To(o.BeTrue())

			compat_otp.By("verfy clf with invalid drop fileds is rejected")
			patch = `[{"op":"add","path":"/spec/filters","value":[{"name":"drop-logs","type":"drop","drop":[{"test":[{"field":".kubernetes.labels.test.logging.io/logging.qe-test-label","matches":".+"}]}]}]},{"op":"add","path":"/spec/pipelines/0/filterRefs","value":["drop-logs"]}]`
			outString, _ = clf.patch(oc, patch)
			o.Expect(strings.Contains(outString, `The ClusterLogForwarder "clf-75390" is invalid:`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `spec.filters[0].drop[0].test[0].field: Invalid value: ".kubernetes.labels.test.logging.io/logging.qe-test-label"`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `spec.filters[0].drop[0].test[0].field in body should match '^(\.[a-zA-Z0-9_]+|\."[^"]+")(\.[a-zA-Z0-9_]+|\."[^"]+")*$`)).To(o.BeTrue())

			compat_otp.By("verify CLF without prune spec is rejected")
			patch = `[{"op":"add","path":"/spec/filters", "value": [{"name": "prune-logs", "type": "prune"}]},{"op":"add","path":"/spec/pipelines/0/filterRefs","value":["prune-logs"]}]`
			outString, _ = clf.patch(oc, patch)
			o.Expect(strings.Contains(outString, `The ClusterLogForwarder "clf-75390" is invalid:`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, ` Invalid value: "object": Additional type specific spec is required for the filter type`)).To(o.BeTrue())

			compat_otp.By("verify CLF with invalid prune value is rejected")
			patch = `[{"op":"add","path":"/spec/filters","value":[{"name":"prune-logs","type":"prune","prune":{"in":[".kubernetes.namespace_labels.pod-security.kubernetes.io/audit",".file",".kubernetes.annotations"]}}]},{"op":"add","path":"/spec/pipelines/0/filterRefs","value":["prune-logs"]}]`
			outString, _ = clf.patch(oc, patch)
			o.Expect(strings.Contains(outString, `The ClusterLogForwarder "clf-75390" is invalid:`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `Invalid value: ".kubernetes.namespace_labels.pod-security.kubernetes.io/audit"`)).To(o.BeTrue())
			o.Expect(strings.Contains(outString, `body should match '^(\.[a-zA-Z0-9_]+|\."[^"]+")(\.[a-zA-Z0-9_]+|\."[^"]+")*$'`)).To(o.BeTrue())

			compat_otp.By("verify filtersStatus show error when prune fields include .log_type, .message or .log_source")
			patch = `[{"op":"add","path":"/spec/filters","value":[{"name":"prune-logs","prune":{"in":[".log_type",".message",".log_source"]},"type":"prune"}]},{"op":"add","path":"/spec/pipelines/0/filterRefs","value":["prune-logs"]}]`
			clf.update(oc, "", patch, "--type=json")
			checkResource(oc, true, false, `prune-logs: [[".log_type" ".message" ".log_source"] is/are required fields and must be removed from the`+" `in` list.]", []string{"clusterlogforwarder.observability.openshift.io", clf.name, "-n", clf.namespace, "-ojsonpath={.status.filterConditions[0].message}"})
			patch = `[{"op":"replace","path":"/spec/filters","value":[{"name":"prune-logs","prune":{"notIn":[".kubernetes",".\"@timestamp\"",".openshift",".hostname"]},"type":"prune"}]}]`
			clf.update(oc, "", patch, "--type=json")
			checkResource(oc, true, false, `prune-logs: [[".log_source" ".log_type" ".message"] is/are required fields and must be included in`+" the `notIn` list.]", []string{"clusterlogforwarder.observability.openshift.io", clf.name, "-n", clf.namespace, "-ojsonpath={.status.filterConditions[0].message}"})

		})
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-High-81855-Vector forward logs to Splunk - customized source", func() {
			oc.SetupProject()
			splunkProject := oc.Namespace()
			sp := splunkPodServer{
				namespace: splunkProject,
				name:      "default-http",
				authType:  "http",
				version:   "9.0",
			}
			sp.init()
			// The secret used in CLF to splunk server
			clfSecret := toSplunkSecret{
				name:       "to-splunk-secret-81855",
				namespace:  splunkProject,
				hecToken:   sp.hecToken,
				caFile:     "",
				keyFile:    "",
				certFile:   "",
				passphrase: "",
			}

			clf := clusterlogforwarder{
				name:                      "clf-81855",
				namespace:                 splunkProject,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "splunk.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}

			josnLogTemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")

			g.By("Deploy splunk")
			defer sp.destroy(oc)
			sp.deploy(oc)

			g.By("create clusterlogforwarder")
			defer clfSecret.delete(oc)
			clfSecret.create(oc)
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+sp.serviceURL+":8088", "SECRET_NAME="+clfSecret.name, "INDEX=main")

			g.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", josnLogTemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("check the default logs source in splunk")
			//For journal log: the source=comm name
			o.Expect(sp.checkLogs("source IN (ovs-vswitchd,kubenswrapper,crio,system)")).To(o.BeTrue())
			//For container Logs:  source=projectName+podName+ContainerName
			o.Expect(sp.checkLogs("source=" + appProj + "*")).To(o.BeTrue())
			o.Expect(sp.checkLogs("source=kubeAPI")).To(o.BeTrue())
			o.Expect(sp.checkLogs("source=openshiftAPI")).To(o.BeTrue())
			o.Expect(sp.checkLogs("source=auditd")).To(o.BeTrue())

			patch := `[{"op": "add", "path": "/spec/outputs/0/splunk/source", "value": "{.log_type||\"logcollector\"}"}]`
			clf.update(oc, "", patch, "--type=json")

			g.By("check customized logs source is logType in splunk")
			o.Expect(sp.checkLogs("source=infrastructure")).To(o.BeTrue())
			o.Expect(sp.checkLogs("source=application")).To(o.BeTrue())
			o.Expect(sp.checkLogs("source=audit")).To(o.BeTrue())
		})

	})
})
