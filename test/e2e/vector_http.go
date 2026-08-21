package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"os"
	"path/filepath"
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
		oc = exutil.NewCLIWithoutNamespace("logfwdhttp")
		loggingBaseDir string
	)

	g.Context("Log Forward to Http", func() {
		g.BeforeEach(func() {
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

		// author anli@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-WRS-Medium-61253-V-ICA.03-vector forward logs to fluentdserver over http - mtls", func() {
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			oc.SetupProject()
			clfNS := oc.Namespace()

			g.By("deploy fluentd server")
			oc.SetupProject()
			fluentdProj := oc.Namespace()
			keyPassphase := getRandomString()
			fluentdS := fluentdServer{
				serverName:                 "fluentdtest",
				namespace:                  fluentdProj,
				serverAuth:                 true,
				clientAuth:                 true,
				clientPrivateKeyPassphrase: keyPassphase,
				secretName:                 "to-fluentd-61253",
				loggingNS:                  clfNS,
				inPluginType:               "http",
			}
			defer fluentdS.remove(oc)
			fluentdS.deploy(oc)

			g.By("create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-" + getRandomString(),
				namespace:                 clfNS,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "https-output-mtls.yaml"),
				secretName:                fluentdS.secretName,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=https://"+fluentdS.serverName+"."+fluentdS.namespace+".svc:24224")

			g.By("check logs in fluentd server")
			fluentdS.checkData(oc, true, "app.log")
			fluentdS.checkData(oc, true, "audit.log")
			fluentdS.checkData(oc, true, "infra.log")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-High-60933-vector Forward logs to fluentd over http - https", func() {
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("deploy fluentd server")
			oc.SetupProject()
			fluentdProj := oc.Namespace()
			fluentdS := fluentdServer{
				serverName:   "fluentdtest",
				namespace:    fluentdProj,
				serverAuth:   true,
				clientAuth:   false,
				secretName:   "to-fluentd-60933",
				loggingNS:    fluentdProj,
				inPluginType: "http",
			}
			defer fluentdS.remove(oc)
			fluentdS.deploy(oc)

			g.By("create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-60933",
				namespace:                 fluentdProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "https-output-ca.yaml"),
				secretName:                fluentdS.secretName,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=https://"+fluentdS.serverName+"."+fluentdS.namespace+".svc:24224")

			g.By("check logs in fluentd server")
			fluentdS.checkData(oc, true, "app.log")
			fluentdS.checkData(oc, true, "audit.log")
			fluentdS.checkData(oc, true, "infra.log")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Medium-60926-vector Forward logs to fluentd over http - http", func() {
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("deploy fluentd server")
			oc.SetupProject()
			fluentdProj := oc.Namespace()
			fluentdS := fluentdServer{
				serverName:   "fluentdtest",
				namespace:    fluentdProj,
				serverAuth:   false,
				clientAuth:   false,
				loggingNS:    fluentdProj,
				inPluginType: "http",
			}
			defer fluentdS.remove(oc)
			fluentdS.deploy(oc)

			g.By("create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-60926",
				namespace:                 fluentdProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "http-output.yaml"),
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=http://"+fluentdS.serverName+"."+fluentdS.namespace+".svc:24224")

			g.By("check logs in fluentd server")
			fluentdS.checkData(oc, true, "app.log")
			fluentdS.checkData(oc, true, "audit.log")
			fluentdS.checkData(oc, true, "infra.log")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-Medium-60936-vector Forward logs to fluentd over http - TLSSkipVerify", func() {
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("deploy fluentd server")
			oc.SetupProject()
			fluentdProj := oc.Namespace()
			fluentdS := fluentdServer{
				serverName:   "fluentdtest",
				namespace:    fluentdProj,
				serverAuth:   true,
				clientAuth:   false,
				secretName:   "to-fluentd-60936",
				loggingNS:    fluentdProj,
				inPluginType: "http",
			}
			defer fluentdS.remove(oc)
			fluentdS.deploy(oc)

			//Create a fake secret from root ca which is used for TLSSkipVerify
			fakeSecret := resource{"secret", "fake-bundle-60936", fluentdProj}
			defer fakeSecret.clear(oc)
			dirname := "/tmp/60936-keys"
			defer os.RemoveAll(dirname)
			err = os.MkdirAll(dirname, 0777)
			o.Expect(err).NotTo(o.HaveOccurred())
			_, err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("cm/kube-root-ca.crt", "-n", loggingNS, "--confirm", "--to="+dirname).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", fakeSecret.name, "-n", fakeSecret.namespace, "--from-file=ca-bundle.crt="+dirname+"/ca.crt").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-60936",
				namespace:                 fluentdProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "https-output-skipverify.yaml"),
				secretName:                fakeSecret.name,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=https://"+fluentdS.serverName+"."+fluentdS.namespace+".svc:24224")

			g.By("check logs in fluentd server")
			fluentdS.checkData(oc, true, "app.log")
			fluentdS.checkData(oc, true, "audit.log")
			fluentdS.checkData(oc, true, "infra.log")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-High-61567-Collector-External HTTP output sink Fluentd complies with the tlsSecurityProfile configuration.[Slow][Disruptive]", func() {
			compat_otp.By("Check if the current tlsSecurityProfile is the expected one")
			if !compareExpectedTLSConfigWithCurrent(oc, `{"old":{},"type":"Old"}`) {
				g.Skip("Current tlsSecurityProfile is not the expected one, skipping the test...")
			}

			compat_otp.By("Deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("deploy fluentd server")
			oc.SetupProject()
			fluentdProj := oc.Namespace()
			fluentdS := fluentdServer{
				serverName:   "fluentdtest",
				namespace:    fluentdProj,
				serverAuth:   true,
				clientAuth:   false,
				secretName:   "to-fluentd-60933",
				loggingNS:    fluentdProj,
				inPluginType: "http",
			}
			defer fluentdS.remove(oc)
			fluentdS.deploy(oc)

			compat_otp.By("create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:                      "clf-61567",
				namespace:                 fluentdProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "https-61567.yaml"),
				secretName:                fluentdS.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=https://"+fluentdS.serverName+"."+fluentdS.namespace+".svc:24224")

			compat_otp.By("The HTTP sink in Vector config must use the Old tlsSecurityProfile")
			searchString := `[sinks.output_httpout_app.tls]
min_tls_version = "VersionTLS10"
ciphersuites = "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256,ECDHE-ECDSA-AES128-GCM-SHA256,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES256-GCM-SHA384,ECDHE-RSA-AES256-GCM-SHA384,ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,DHE-RSA-AES128-GCM-SHA256,DHE-RSA-AES256-GCM-SHA384,DHE-RSA-CHACHA20-POLY1305,ECDHE-ECDSA-AES128-SHA256,ECDHE-RSA-AES128-SHA256,ECDHE-ECDSA-AES128-SHA,ECDHE-RSA-AES128-SHA,ECDHE-ECDSA-AES256-SHA384,ECDHE-RSA-AES256-SHA384,ECDHE-ECDSA-AES256-SHA,ECDHE-RSA-AES256-SHA,DHE-RSA-AES128-SHA256,DHE-RSA-AES256-SHA256,AES128-GCM-SHA256,AES256-GCM-SHA384,AES128-SHA256,AES256-SHA256,AES128-SHA,AES256-SHA,DES-CBC3-SHA"
ca_file = "/var/run/ocp-collector/secrets/to-fluentd-60933/ca-bundle.crt"`
			result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("check logs in fluentd server")
			fluentdS.checkData(oc, true, "app.log")

			compat_otp.By("Set Intermediate tlsSecurityProfile for the External HTTP output.")
			patch := `[{"op": "add", "path": "/spec/outputs/0/tls/securityProfile", "value": {"type": "Intermediate"}}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			compat_otp.By("The HTTP sink in Vector config must use the Intermediate tlsSecurityProfile")
			searchString = `[sinks.output_httpout_app.tls]
min_tls_version = "VersionTLS12"
ciphersuites = "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256,ECDHE-ECDSA-AES128-GCM-SHA256,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES256-GCM-SHA384,ECDHE-RSA-AES256-GCM-SHA384,ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,DHE-RSA-AES128-GCM-SHA256,DHE-RSA-AES256-GCM-SHA384"
ca_file = "/var/run/ocp-collector/secrets/to-fluentd-60933/ca-bundle.crt"`
			result, err = checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("Check for errors in collector pod logs.")
			e2e.Logf("Wait for a minute before the collector logs are generated.")
			time.Sleep(60 * time.Second)
			collectorLogs, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("-n", clf.namespace, "--selector=app.kubernetes.io/component=collector").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(strings.Contains(collectorLogs, "Error trying to connect")).ShouldNot(o.BeTrue(), "Unable to connect to the external HTTP (Fluentd) server.")

			compat_otp.By("Delete the Fluentdserver pod to recollect logs")
			err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pods", "-n", fluentdProj, "-l", "component=fluentdtest").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			waitForPodReadyWithLabel(oc, fluentdProj, "component=fluentdtest")

			compat_otp.By("check logs in fluentd server")
			fluentdS.checkData(oc, true, "app.log")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-WRS-Critical-65131-V-ICA.02 mCLF Inputs.receiver.http over http with default values", func() {
			oc.SetupProject()
			clfNS := oc.Namespace()
			fluentdNS := clfNS

			g.By("deploy fluentd server")
			keyPassphase := getRandomString()
			fluentdS := fluentdServer{
				serverName:                 "fluentdtest",
				namespace:                  fluentdNS,
				serverAuth:                 true,
				clientAuth:                 true,
				clientPrivateKeyPassphrase: keyPassphase,
				secretName:                 "to-fluentd-65131",
				loggingNS:                  clfNS,
				inPluginType:               "http",
			}
			defer fluentdS.remove(oc)
			fluentdS.deploy(oc)

			g.By("create clusterlogforwarder/instance")
			clf := clusterlogforwarder{
				name:               "http-to-http",
				namespace:          clfNS,
				templateFile:       filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "httpserver-to-httpoutput.yaml"),
				secretName:         fluentdS.secretName,
				serviceAccountName: "clf-" + getRandomString(),
				collectAuditLogs:   false,
				waitForPodReady:    true,
			}
			defer clf.delete(oc)
			clf.create(oc, "URL=https://"+fluentdS.serverName+"."+fluentdS.namespace+".svc:24224")

			g.By("send two records to httpserver")
			o.Expect(postDataToHttpserver(oc, "https://"+clf.name+"-httpserver."+clfNS+".svc:8443", `{"data":"record1"}`)).To(o.BeTrue())
			o.Expect(postDataToHttpserver(oc, "https://"+clf.name+"-httpserver."+clfNS+".svc:8443", `{"data":"record2"}`)).To(o.BeTrue())

			g.By("check auditlogs in fluentd server")
			fluentdS.checkData(oc, true, "audit.log")
		})
	})
})
