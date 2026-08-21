package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease", func() {
	defer g.GinkgoRecover()

	var (
		oc = exutil.NewCLIWithoutNamespace("vector-es")
		loggingBaseDir string
	)

	g.Context("Log Forward to User-Managed-ES", func() {
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
			oc.SetupProject()
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-Critical-49390-Vector Collecting Kubernetes events using event router", func() {
			g.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			es := externalES{
				namespace:  esProj,
				version:    "8",
				serverName: "elasticsearch-server",
				httpSSL:    true,
				clientAuth: true,
				secretName: "ees-49390",
				loggingNS:  esProj,
			}
			defer es.remove(oc)
			es.deploy(oc)

			g.By("Create ClusterLogForwarder instance")
			clf := clusterlogforwarder{
				name:                      "clf-49390",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-mtls.yaml"),
				secretName:                es.secretName,
				waitForPodReady:           true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=https://"+es.serverName+"."+esProj+".svc:9200", "ES_VERSION="+es.version, "INPUT_REFS=[\"infrastructure\"]")

			g.By("Deploy the Event Router")
			evt := eventRouter{
				name:      "logging-eventrouter",
				namespace: cloNS,
				template:  filepath.Join(loggingBaseDir, "eventrouter", "eventrouter.yaml"),
			}
			defer evt.delete(oc)
			evt.deploy(oc)

			g.By("Check event logs in the Event Router pod")
			podList, err := oc.AdminKubeClient().CoreV1().Pods(evt.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "component=eventrouter"})
			o.Expect(err).NotTo(o.HaveOccurred())
			checkLogsFromRs(oc, "pods", podList.Items[0].Name, evt.namespace, "kube-eventrouter", "ADDED")
			checkLogsFromRs(oc, "pods", podList.Items[0].Name, evt.namespace, "kube-eventrouter", "Update")

			g.By("Check for Event Router logs in Elasticsearch")
			checkLog := "{\"sort\": [{\"@timestamp\": {\"order\":\"desc\"}}], \"query\": {\"match_phrase\": {\"kubernetes.pod_name\": \"" + podList.Items[0].Name + "\"}}}"
			err = wait.PollUntilContextTimeout(context.Background(), 20*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				logs := es.searchDocByQuery(oc, "infra", checkLog)
				return len(logs.Hits.DataHits) > 0, nil
			})
			compat_otp.AssertWaitPollNoErr(err, "No Event Router logs found when using vector as log collector.")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-Critical-53995-Vector Collect OVN audit logs", func() {
			g.By("Check the network type for the test")
			networkType := checkNetworkType(oc)
			if !strings.Contains(networkType, "ovnkubernetes") {
				g.Skip("Skip for non-supported network type, type is not OVNKubernetes!!!")
			}

			oc.SetupProject()
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "7",
				serverName: "external-es",
				httpSSL:    true,
				secretName: "clf-53995",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			g.By("create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                      "clf-53995",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-https.yaml"),
				secretName:                ees.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=https://"+ees.serverName+"."+ees.namespace+".svc:9200", "ES_VERSION="+ees.version)

			g.By("Check audit index in ES pod")
			ees.waitForIndexAppear(oc, "audit")

			g.By("Create a test project, enable OVN network log collection on it, add the OVN log app and network policies for the project")
			oc.SetupProject()
			ovnProj := oc.Namespace()
			ovn := resource{"deployment", "ovn-app", ovnProj}
			ovnTemplate := filepath.Join(loggingBaseDir, "generatelog", "42981.yaml")
			err := ovn.applyFromTemplate(oc, "-n", ovn.namespace, "-f", ovnTemplate, "-p", "NAMESPACE="+ovn.namespace)
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
			o.Expect(nodeLogs).Should(o.ContainSubstring(ovnProj), "The OVN logs doesn't contain logs from project %s", ovnProj)

			g.By("Check for the generated OVN audit logs in Elasticsearch")
			err = wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
				query := "{\"query\":{\"query_string\":{\"query\":\"verdict=allow AND severity=alert AND tcp,vlan_tci AND tcp_flags=ack\",\"default_field\":\"message\"}}}"
				res := ees.searchDocByQuery(oc, "audit", query)
				return len(res.Hits.DataHits) > 0, nil
			})
			compat_otp.AssertWaitPollNoErr(err, "The ovn audit logs are not collected")
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-76073-Medium-74944-Send logs from containers in the same pod to separate indices and parse json logs", func() {
			app := oc.Namespace()
			containerName := "log-76073-" + getRandomString()
			multiContainerJSONLog := filepath.Join(loggingBaseDir, "generatelog", "multi_container_json_log_template.yaml")
			err := oc.WithoutNamespace().Run("new-app").Args("-f", multiContainerJSONLog, "-n", app, "-p", "CONTAINER="+containerName).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			oc.SetupProject()
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "7",
				serverName: "external-es",
				httpSSL:    true,
				secretName: "json-log-76073",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)
			eesURL := "https://" + ees.serverName + "." + ees.namespace + ".svc:9200"

			g.By("create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                      "clf-76073",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-https.yaml"),
				secretName:                ees.secretName,
				collectApplicationLogs:    true,
				collectInfrastructureLogs: true,
				collectAuditLogs:          true,
				serviceAccountName:        "clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL="+eesURL, "ES_VERSION="+ees.version, "INDEX={.kubernetes.container_name||.log_type||\"none\"}")
			patch := `[{"op": "add", "path": "/spec/filters", "value": [{"name": "parse-json-logs", "type": "parse"}]}, {"op": "add", "path": "/spec/pipelines/0/filterRefs", "value":["parse-json-logs"]}]`
			clf.update(oc, "", patch, "--type=json")
			clf.waitForCollectorPodsReady(oc)

			// for container logs, they're indexed by container name
			// for non-container logs, they're indexed by log_type
			g.By("check indices in externale ES")
			ees.waitForIndexAppear(oc, containerName+"-0")
			ees.waitForIndexAppear(oc, containerName+"-1")
			ees.waitForIndexAppear(oc, containerName+"-2")
			ees.waitForIndexAppear(oc, "kube-") // infra container logs
			ees.waitForIndexAppear(oc, "infrastructure")
			ees.waitForIndexAppear(oc, "audit")

			compat_otp.By("check logs in ES, json logs should be parsed to json format")
			queryContainerLog := func(container string) string {
				return "{\"size\": 1, \"sort\": [{\"@timestamp\": {\"order\":\"desc\"}}], \"query\": {\"match_phrase\": {\"kubernetes.container_name\": \"" + container + "\"}}}"
			}
			// in index app-$containerName-0, only logs in container $containerName-0 are stored in it
			log0 := ees.searchDocByQuery(oc, containerName+"-0", queryContainerLog(containerName+"-0"))
			o.Expect(len(log0.Hits.DataHits) > 0).To(o.BeTrue())
			o.Expect(log0.Hits.DataHits[0].Source.Structured.Message != "").Should(o.BeTrue())
			log01 := ees.searchDocByQuery(oc, containerName+"-0", queryContainerLog(containerName+"-1"))
			o.Expect(len(log01.Hits.DataHits) == 0).To(o.BeTrue())
			log02 := ees.searchDocByQuery(oc, containerName+"-0", queryContainerLog(containerName+"-2"))
			o.Expect(len(log02.Hits.DataHits) == 0).To(o.BeTrue())

			// in index app-$containerName-1, only logs in container $containerName-1 are stored in it
			log1 := ees.searchDocByQuery(oc, containerName+"-1", queryContainerLog(containerName+"-1"))
			o.Expect(len(log1.Hits.DataHits) > 0).To(o.BeTrue())
			o.Expect(log1.Hits.DataHits[0].Source.Structured.Message != "").Should(o.BeTrue())
			log10 := ees.searchDocByQuery(oc, containerName+"-1", queryContainerLog(containerName+"-0"))
			o.Expect(len(log10.Hits.DataHits) == 0).To(o.BeTrue())
			log12 := ees.searchDocByQuery(oc, containerName+"-1", queryContainerLog(containerName+"-2"))
			o.Expect(len(log12.Hits.DataHits) == 0).To(o.BeTrue())

			// in index app-$app-project, only logs in container $containerName-2 are stored in it
			log2 := ees.searchDocByQuery(oc, containerName+"-2", queryContainerLog(containerName+"-2"))
			o.Expect(len(log2.Hits.DataHits) > 0).To(o.BeTrue())
			o.Expect(log2.Hits.DataHits[0].Source.Structured.Message != "").Should(o.BeTrue())
			log20 := ees.searchDocByQuery(oc, containerName+"-2", queryContainerLog(containerName+"-0"))
			o.Expect(len(log20.Hits.DataHits) == 0).To(o.BeTrue())
			log21 := ees.searchDocByQuery(oc, containerName+"-2", queryContainerLog(containerName+"-1"))
			o.Expect(len(log21.Hits.DataHits) == 0).To(o.BeTrue())

		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-52131-Vector Logs from different projects are forwarded to the same index if the pods have same annotation", func() {
			containerName := "log-52131-" + getRandomString()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			app1 := oc.Namespace()
			err := oc.WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", app1, "-p", "CONTAINER="+containerName).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			oc.SetupProject()
			app2 := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", app2, "-p", "CONTAINER="+containerName).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			oc.SetupProject()
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "7",
				serverName: "external-es",
				httpSSL:    true,
				secretName: "json-log-52131",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)
			eesURL := "https://" + ees.serverName + "." + ees.namespace + ".svc:9200"

			g.By("create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                   "clf-52131",
				namespace:              esProj,
				templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-https.yaml"),
				secretName:             ees.secretName,
				waitForPodReady:        true,
				collectApplicationLogs: true,
				serviceAccountName:     "clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "INDEX={.kubernetes.container_name||\"none-container-logs\"}", "ES_URL="+eesURL, "ES_VERSION="+ees.version, "INPUT_REFS=[\"application\"]")

			g.By("check indices in externale ES")
			ees.waitForIndexAppear(oc, containerName)

			g.By("check data in ES")
			for _, proj := range []string{app1, app2} {
				count, err := ees.getDocCount(oc, containerName, "{\"query\": {\"match_phrase\": {\"kubernetes.namespace_name\": \""+proj+"\"}}}")
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(count > 0).To(o.BeTrue())
			}
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-74947-New filter openshiftLabels testing", func() {
			compat_otp.By("Create Elasticsearch")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "6",
				serverName: "elasticsearch-server",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			compat_otp.By("Create project for app logs and deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", loglabeltemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("Create ClusterLogForwarder")
			clf := clusterlogforwarder{
				name:                      "clf-74947",
				namespace:                 esProj,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch.yaml"),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=http://"+getRouteAddress(oc, ees.namespace, ees.serverName)+":80", "ES_VERSION="+ees.version)

			compat_otp.By("Check logs in ES")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForIndexAppear(oc, "infra")
			ees.waitForIndexAppear(oc, "audit")

			compat_otp.By("Add new filter to the ClusterLogForwarder")
			patch := `[{"op": "add", "path": "/spec/filters", "value": [{"name": "openshift-labels", "type": "openshiftLabels", "openshiftLabels": {"label-test": "ocp-74947", "clf/observability.openshift.io": "logging-74947"}}]}, {"op": "add", "path": "/spec/pipelines/0/filterRefs", "value": ["openshift-labels"]}]`
			clf.update(oc, "", patch, "--type=json")
			clf.waitForCollectorPodsReady(oc)

			compat_otp.By("Check logs with label in ES")
			checkLog := `{"size": 1, "sort": [{"@timestamp": {"order":"desc"}}], "query": {"bool": {"must": [{"match": {"openshift.labels.label-test": "ocp-74947"}},{"match": {"openshift.labels.clf/observability.openshift.io": "logging-74947"}}]}}}`
			indexName := []string{"app", "infra", "audit"}
			for _, index := range indexName {
				err = wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 60*time.Second, true, func(context.Context) (done bool, err error) {
					logs := ees.searchDocByQuery(oc, index, checkLog)
					if logs.Hits.Total > 0 || len(logs.Hits.DataHits) > 0 {
						return true, nil
					}
					return false, nil
				})
				compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("No %s logs found with label in extranl ES", index))
			}

		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-Medium-48593-Vector ClusterLogForwarder Label each message type differently and send all to the same output", func() {

			g.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "6",
				serverName: "elasticsearch-server",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			g.By("Create project for app logs and deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", loglabeltemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Create ClusterLogForwarder instance")
			clf := clusterlogforwarder{
				name:                      "clf-48593",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "48593.yaml"),
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=http://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version)

			g.By("Check logs in external ES")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForIndexAppear(oc, "infra")
			ees.waitForIndexAppear(oc, "audit")

			g.By("Check logs with pipeline label in external ES")
			indexName := []string{"app", "infra", "audit"}
			for i := 0; i < len(indexName); i++ {
				checkLog := "{\"size\": 1, \"sort\": [{\"@timestamp\": {\"order\":\"desc\"}}], \"query\": {\"match\": {\"openshift.labels.logging\": \"" + indexName[i] + "-logs\"}}}"
				err = wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 60*time.Second, true, func(context.Context) (done bool, err error) {
					logs := ees.searchDocByQuery(oc, indexName[i], checkLog)
					if logs.Hits.Total > 0 {
						return true, nil
					}
					return false, nil
				})
				compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("No %s logs found with pipeline label in extranl ES", indexName[i]))
			}

		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-High-46882-High-47061-Vector ClusterLogForwarder forward logs to Elasticsearch insecure forward and metadata check", func() {

			g.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "6",
				serverName: "elasticsearch-server",
				httpSSL:    false,
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			g.By("Create project for app logs and deploy the log generator app")
			oc.SetupProject()
			app := oc.Namespace()
			// to test fix for LOG-3463, add labels to the app project
			_, err := compat_otp.AddLabelsToSpecificResource(oc, "ns/"+app, "", "app=logging-apps", "app.kubernetes.io/instance=logging-apps-test", "app.test=test")
			o.Expect(err).NotTo(o.HaveOccurred())
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_non_json_log_template.json")
			err = oc.WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", app).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			clusterID, err := getClusterID(oc)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Create ClusterLogForwarder")
			clf := clusterlogforwarder{
				name:                      "clf-46882",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch.yaml"),
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=http://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version)

			g.By("Check logs in external ES")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForIndexAppear(oc, "infra")
			ees.waitForIndexAppear(oc, "audit")
			ees.waitForProjectLogsAppear(oc, app, "app")

			appLogs := ees.searchDocByQuery(oc, "app", "{\"query\": {\"match_phrase\": {\"kubernetes.namespace_name\": \""+app+"\"}}}")
			log := appLogs.Hits.DataHits[0].Source
			o.Expect(log.Message == "ㄅㄉˇˋㄓˊ˙ㄚㄞㄢㄦㄆ 中国 883.317µs ā á ǎ à ō ó ▅ ▆ ▇ █ 々").Should(o.BeTrue())
			o.Expect(log.OpenShift.ClusterID == clusterID).Should(o.BeTrue())
			o.Expect(log.OpenShift.Sequence > 0).Should(o.BeTrue())
			o.Expect(log.Kubernetes.NamespaceLabels["app_kubernetes_io_instance"] == "logging-apps-test").Should(o.BeTrue())
			o.Expect(log.Kubernetes.NamespaceLabels["app_test"] == "test").Should(o.BeTrue())
			infraLogs := ees.searchDocByQuery(oc, "infra", "")
			o.Expect(infraLogs.Hits.DataHits[0].Source.OpenShift.ClusterID == clusterID).Should(o.BeTrue())
			auditLogs := ees.searchDocByQuery(oc, "audit", "")
			o.Expect(auditLogs.Hits.DataHits[0].Source.OpenShift.ClusterID == clusterID).Should(o.BeTrue())

			for _, logType := range []string{"app", "infra", "audit"} {
				for _, field := range []string{"@timestamp", "openshift.cluster_id", "openshift.sequence"} {
					count, err := ees.getDocCount(oc, logType, "{\"query\": {\"bool\": {\"must_not\": {\"exists\": {\"field\": \""+field+"\"}}}}}")
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(count == 0).Should(o.BeTrue())
				}
			}

		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-High-55396-AlertRule CollectorNodeDown testing", func() {
			g.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "6",
				serverName: "elasticsearch-server",
				httpSSL:    true,
				secretName: "ees-https",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			g.By("Create project for app logs and deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", loglabeltemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Create ClusterLogForwarder instance")
			clf := clusterlogforwarder{
				name:                      "clf-55396",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-https.yaml"),
				secretName:                ees.secretName,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
				enableMonitoring:          true,
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=https://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version, `SECURITY_PROFILE={"type": "Old"}`)

			g.By("Check logs in external ES")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForIndexAppear(oc, "infra")
			ees.waitForIndexAppear(oc, "audit")

			g.By("Patch the collector Prometheus Rule for alert CollectorNodeDown to set alert firing time to 2m")
			defer oc.AsAdmin().WithoutNamespace().Run("patch").Args("prometheusrules", "collector", "--type=json", "-p", `[{"op": "replace", "path": "/spec/groups/0/rules/0/for", "value":"10m"}]`, "-n", cloNS).Execute()
			er := oc.AsAdmin().WithoutNamespace().Run("patch").Args("prometheusrules", "collector", "--type=json", "-p", `[{"op": "replace", "path": "/spec/groups/0/rules/0/for", "value":"2m"}]`, "-n", cloNS).Execute()
			o.Expect(er).NotTo(o.HaveOccurred())

			g.By("Update clusterlogforwarder to set the cpu and memory for collector pods")
			resource := `[{"op": "replace", "path": "/spec/collector/resources", "value": {"limits": {"memory": "128Mi", "cpu": "10m"}, "requests": {"cpu": "1m", "memory": "2Mi"}}}]`
			clf.update(oc, "", resource, "--type=json")

			g.By("Check the alert CollectorNodeDown is in state firing or pending")
			checkAlert(oc, getSAToken(oc, "prometheus-k8s", "openshift-monitoring"), "CollectorNodeDown", "firing/pending", 5)
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-Medium-55200-Medium-47753-Vector Forward logs to external Elasticsearch with username password HTTP ES 6.x", func() {
			g.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "6",
				serverName: "elasticsearch-server",
				userAuth:   true,
				username:   "user1",
				password:   getRandomString(),
				secretName: "ees-http",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			g.By("Create project for app logs and deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", loglabeltemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Create ClusterLogForwarder instance")
			clf := clusterlogforwarder{
				name:                      "clf-47753",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-userauth.yaml"),
				secretName:                ees.secretName,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=http://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version)

			g.By("Check logs in external ES")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForIndexAppear(oc, "infra")
			ees.waitForIndexAppear(oc, "audit")

		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-Medium-55199-Medium-47755-Vector Forward logs to external Elasticsearch with username password HTTPS ES 7.x", func() {
			oc.SetupProject()
			clfNS := oc.Namespace()

			g.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "7",
				serverName: "elasticsearch-server",
				httpSSL:    true,
				userAuth:   true,
				username:   "user1",
				password:   getRandomString(),
				secretName: "ees-47755",
				loggingNS:  clfNS,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			g.By("Create project for app logs and deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", loglabeltemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Create ClusterLogForwarder instance")
			clf := clusterlogforwarder{
				name:                      "clf-55199",
				namespace:                 clfNS,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-userauth-https.yaml"),
				secretName:                ees.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
				enableMonitoring:          true,
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=https://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version)

			g.By("Check logs in external ES")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForIndexAppear(oc, "infra")
			ees.waitForIndexAppear(oc, "audit")

		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-High-61450-Collector-External Elasticsearch output complies with the tlsSecurityProfile config.[Slow][Disruptive]", func() {
			compat_otp.By("Check if the current tlsSecurityProfile is the expected one")
			expectedTLSConfig := `{"custom":{"ciphers":["ECDHE-ECDSA-CHACHA20-POLY1305","ECDHE-RSA-CHACHA20-POLY1305","ECDHE-RSA-AES128-GCM-SHA256","ECDHE-ECDSA-AES128-GCM-SHA256","TLS_AES_128_GCM_SHA256","TLS_AES_256_GCM_SHA384","TLS_CHACHA20_POLY1305_SHA256","ECDHE-ECDSA-AES256-GCM-SHA384","ECDHE-RSA-AES256-GCM-SHA384","ECDHE-ECDSA-CHACHA20-POLY1305","ECDHE-RSA-CHACHA20-POLY1305","DHE-RSA-AES128-GCM-SHA256","DHE-RSA-AES256-GCM-SHA384","DHE-RSA-CHACHA20-POLY1305","ECDHE-ECDSA-AES128-SHA256","ECDHE-RSA-AES128-SHA256","ECDHE-ECDSA-AES128-SHA","ECDHE-RSA-AES128-SHA","ECDHE-ECDSA-AES256-SHA384","ECDHE-RSA-AES256-SHA384","ECDHE-ECDSA-AES256-SHA","ECDHE-RSA-AES256-SHA","DHE-RSA-AES128-SHA256","DHE-RSA-AES256-SHA256","AES128-GCM-SHA256","AES256-GCM-SHA384","AES128-SHA256","AES256-SHA256"],"minTLSVersion":"VersionTLS10"},"type":"Custom"}`
			if !compareExpectedTLSConfigWithCurrent(oc, expectedTLSConfig) {
				g.Skip("Current tlsSecurityProfile is not the expected one, skipping the test...")
			}

			compat_otp.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "6",
				serverName: "elasticsearch-server",
				httpSSL:    true,
				clientAuth: true,
				secretName: "ees-https",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			compat_otp.By("Create project for app logs and deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", loglabeltemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("Create ClusterLogForwarder instance")
			clf := clusterlogforwarder{
				name:                      "clf-61450",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-mtls.yaml"),
				secretName:                ees.secretName,
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=https://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version)

			compat_otp.By("The Elasticsearch sink in Vector config must use the Custom tlsSecurityProfile")
			searchString := `[sinks.output_es_created_by_user.tls]
min_tls_version = "VersionTLS10"
ciphersuites = "ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES128-GCM-SHA256,TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256,ECDHE-ECDSA-AES256-GCM-SHA384,ECDHE-RSA-AES256-GCM-SHA384,ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,DHE-RSA-AES128-GCM-SHA256,DHE-RSA-AES256-GCM-SHA384,DHE-RSA-CHACHA20-POLY1305,ECDHE-ECDSA-AES128-SHA256,ECDHE-RSA-AES128-SHA256,ECDHE-ECDSA-AES128-SHA,ECDHE-RSA-AES128-SHA,ECDHE-ECDSA-AES256-SHA384,ECDHE-RSA-AES256-SHA384,ECDHE-ECDSA-AES256-SHA,ECDHE-RSA-AES256-SHA,DHE-RSA-AES128-SHA256,DHE-RSA-AES256-SHA256,AES128-GCM-SHA256,AES256-GCM-SHA384,AES128-SHA256,AES256-SHA256"
key_file = "/var/run/ocp-collector/secrets/ees-https/tls.key"
crt_file = "/var/run/ocp-collector/secrets/ees-https/tls.crt"
ca_file = "/var/run/ocp-collector/secrets/ees-https/ca-bundle.crt"`
			result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("Check logs in external ES")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForIndexAppear(oc, "infra")
			ees.waitForIndexAppear(oc, "audit")

			compat_otp.By("Set Old tlsSecurityProfile for the External ES output.")
			patch := `[{"op": "add", "path": "/spec/outputs/0/tls/securityProfile", "value": {"type": "Old"}}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			compat_otp.By("The Elasticsearch sink in Vector config must use the Old tlsSecurityProfile")
			searchString = `[sinks.output_es_created_by_user.tls]
min_tls_version = "VersionTLS10"
ciphersuites = "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256,ECDHE-ECDSA-AES128-GCM-SHA256,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES256-GCM-SHA384,ECDHE-RSA-AES256-GCM-SHA384,ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,DHE-RSA-AES128-GCM-SHA256,DHE-RSA-AES256-GCM-SHA384,DHE-RSA-CHACHA20-POLY1305,ECDHE-ECDSA-AES128-SHA256,ECDHE-RSA-AES128-SHA256,ECDHE-ECDSA-AES128-SHA,ECDHE-RSA-AES128-SHA,ECDHE-ECDSA-AES256-SHA384,ECDHE-RSA-AES256-SHA384,ECDHE-ECDSA-AES256-SHA,ECDHE-RSA-AES256-SHA,DHE-RSA-AES128-SHA256,DHE-RSA-AES256-SHA256,AES128-GCM-SHA256,AES256-GCM-SHA384,AES128-SHA256,AES256-SHA256,AES128-SHA,AES256-SHA,DES-CBC3-SHA"
key_file = "/var/run/ocp-collector/secrets/ees-https/tls.key"
crt_file = "/var/run/ocp-collector/secrets/ees-https/tls.crt"
ca_file = "/var/run/ocp-collector/secrets/ees-https/ca-bundle.crt"`
			result, err = checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("Check for errors in collector pod logs.")
			e2e.Logf("Wait for a minute before the collector logs are generated.")
			time.Sleep(60 * time.Second)
			collectorLogs, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("-n", clf.namespace, "--selector=app.kubernetes.io/component=collector").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(strings.Contains(collectorLogs, "Error trying to connect")).ShouldNot(o.BeTrue(), "Unable to connect to the external Elasticsearch server.")

			compat_otp.By("Delete the Elasticsearch server pod to recollect logs")
			err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pods", "-n", esProj, "-l", "app=elasticsearch-server").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			waitForPodReadyWithLabel(oc, esProj, "app=elasticsearch-server")

			compat_otp.By("Check logs in external ES")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForIndexAppear(oc, "infra")
			ees.waitForIndexAppear(oc, "audit")

		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-71000-Collect or exclude logs by namespace[Slow]", func() {
			compat_otp.By("Deploy Elasticsearch")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "7",
				serverName: "elasticsearch-server-71000",
				httpSSL:    true,
				clientAuth: true,
				secretName: "ees-https-71000",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			compat_otp.By("Deploy CLF to exclude some logs by setting excludeNamespaces")
			clf := clusterlogforwarder{
				name:                   "clf-71000",
				namespace:              esProj,
				templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-mtls.yaml"),
				secretName:             ees.secretName,
				collectApplicationLogs: true,
				serviceAccountName:     "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "INPUT_REFS=[\"application\"]", "ES_URL=https://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version)
			patch := `[{"op": "add", "path": "/spec/inputs", "value": [{"name": "new-app", "type": "application", "application": {"excludes": [{"namespace":"logging-project-71000-2"}]}}]}, {"op": "replace", "path": "/spec/pipelines/0/inputRefs", "value": ["new-app"]}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			compat_otp.By("Create project for app logs and deploy the log generator")
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			for i := 0; i < 3; i++ {
				ns := "logging-project-71000-" + strconv.Itoa(i)
				defer oc.DeleteSpecifiedNamespaceAsAdmin(ns)
				oc.CreateSpecifiedNamespaceAsAdmin(ns)
				err := oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", ns).Execute()
				o.Expect(err).NotTo(o.HaveOccurred())
			}

			appNS := "logging-71000-test-1"
			defer oc.DeleteSpecifiedNamespaceAsAdmin(appNS)
			oc.CreateSpecifiedNamespaceAsAdmin(appNS)
			err := oc.AsAdmin().WithoutNamespace().Run("new-app").Args("-f", jsonLogFile, "-n", appNS).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("Check data in ES, logs from project/logging-project-71000-2 shouldn't be collected")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForProjectLogsAppear(oc, "logging-project-71000-0", "app")
			ees.waitForProjectLogsAppear(oc, "logging-project-71000-1", "app")
			ees.waitForProjectLogsAppear(oc, appNS, "app")
			count, err := ees.getDocCount(oc, "app", "{\"query\": {\"regexp\": {\"kubernetes.namespace_name\": \"logging-project-71000-2\"}}}")
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(count == 0).Should(o.BeTrue())

			compat_otp.By("Update CLF to exclude all namespaces")
			patch = `[{"op": "replace", "path": "/spec/inputs/0/application/excludes/0/namespace", "value": "*"}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			compat_otp.By("Check data in ES, no logs should be collected")
			// sleep 10 seconds for collector pods to send the cached records
			time.Sleep(10 * time.Second)
			ees.removeIndices(oc, "application")
			// sleep 10 seconds for collector pods to work with new configurations
			time.Sleep(10 * time.Second)
			indices, err := ees.getIndices(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			if len(indices) > 0 {
				for _, i := range indices {
					o.Expect(strings.Contains(i.Index, "app")).ShouldNot(o.BeTrue())
				}
			}

			compat_otp.By("Update CLF to set include namespaces")
			patch = `[{"op": "add", "path": "/spec/inputs/0/application/includes", "value": [{"namespace": "logging-project-71000*"}]}, {"op": "replace", "path": "/spec/inputs/0/application/excludes/0/namespace", "value": "logging-project-71000-2"}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			// sleep 10 seconds for collector pods to send the cached records
			time.Sleep(10 * time.Second)
			ees.removeIndices(oc, "application")

			compat_otp.By("Check data in ES, logs from project/logging-project-71000-2 and " + appNS + "shouldn't be collected")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForProjectLogsAppear(oc, "logging-project-71000-0", "app")
			ees.waitForProjectLogsAppear(oc, "logging-project-71000-1", "app")
			for _, ns := range []string{appNS, "logging-project-71000-2"} {
				count, err = ees.getDocCount(oc, "app", "{\"query\": {\"regexp\": {\"kubernetes.namespace_name\": \""+ns+"\"}}}")
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(count == 0).Should(o.BeTrue(), "find logs from project "+ns+", this is not expected")
			}

			compat_otp.By("Remove excludes from CLF")
			patch = `[{"op": "remove", "path": "/spec/inputs/0/application/excludes"}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			// sleep 10 seconds for collector pods to send the cached records
			time.Sleep(10 * time.Second)
			ees.removeIndices(oc, "application")

			compat_otp.By("Check data in ES, logs from logging-project-71000*, other logs shouldn't be collected")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForProjectLogsAppear(oc, "logging-project-71000-0", "app")
			ees.waitForProjectLogsAppear(oc, "logging-project-71000-1", "app")
			ees.waitForProjectLogsAppear(oc, "logging-project-71000-2", "app")
			count, err = ees.getDocCount(oc, "app", "{\"query\": {\"regexp\": {\"kubernetes.namespace_name\": \""+appNS+"\"}}}")
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(count == 0).Should(o.BeTrue(), "find logs from project "+appNS+", this is not expected")

			compat_otp.By("Update CLF to include all namespaces")
			patch = `[{"op": "replace", "path": "/spec/inputs/0/application/includes/0/namespace", "value": "*"}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			compat_otp.By("Check data in ES, all application logs should be collected, but no logs from infra projects")
			// sleep 10 seconds for collector pods to send the cached records
			time.Sleep(10 * time.Second)
			ees.removeIndices(oc, "application")
			for _, ns := range []string{appNS, "logging-project-71000-0", "logging-project-71000-1", "logging-project-71000-2"} {
				ees.waitForProjectLogsAppear(oc, ns, "app")
			}
			count, err = ees.getDocCount(oc, "app", "{\"query\": {\"regexp\": {\"kubernetes.namespace_name\": \"openshift@\"}}}")
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(count == 0).Should(o.BeTrue(), "find logs from project openshift*, this is not expected")

		})

		//author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-High-51740-Vector Preserve k8s Common Labels", func() {
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			labels := map[string]string{
				"app.kubernetes.io/name":       "test",
				"app.kubernetes.io/instance":   "functionaltest",
				"app.kubernetes.io/version":    "123",
				"app.kubernetes.io/component":  "thecomponent",
				"app.kubernetes.io/part-of":    "clusterlogging",
				"app.kubernetes.io/managed-by": "clusterloggingoperator",
				"app.kubernetes.io/created-by": "anoperator",
				"run":                          "test-51740",
				"test":                         "test-logging-51740",
			}
			processedLabels := map[string]string{
				"app_kubernetes_io_name":       "test",
				"app_kubernetes_io_instance":   "functionaltest",
				"app_kubernetes_io_version":    "123",
				"app_kubernetes_io_component":  "thecomponent",
				"app_kubernetes_io_part-of":    "clusterlogging",
				"app_kubernetes_io_managed-by": "clusterloggingoperator",
				"app_kubernetes_io_created-by": "anoperator",
				"run":                          "test-51740",
				"test":                         "test-logging-51740",
			}
			labelJSON, _ := json.Marshal(labels)
			labelStr := string(labelJSON)
			app := oc.Namespace()
			err := oc.WithoutNamespace().Run("new-app").Args("-f", loglabeltemplate, "-n", app, "-p", "LABELS="+labelStr).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			//For this case, we need to cover ES and non-ES, and we need to check the log entity in log store,
			//to make the functions simple, here use external loki as the non-ES log store
			g.By("Create Loki project and deploy Loki Server")
			oc.SetupProject()
			lokiNS := oc.Namespace()
			loki := externalLoki{"loki-server", lokiNS}
			defer loki.remove(oc)
			loki.deployLoki(oc)

			g.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "6",
				serverName: "elasticsearch-server",
				httpSSL:    false,
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			g.By("Create ClusterLogForwarder")
			clf := clusterlogforwarder{
				name:                   "clf-51740",
				namespace:              esProj,
				templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch.yaml"),
				collectApplicationLogs: true,
				serviceAccountName:     "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=http://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version, "INPUT_REFS=[\"application\"]")

			lokiURL := "http://" + loki.name + "." + lokiNS + ".svc:3100"
			patch := `[{"op": "add", "path": "/spec/outputs/-", "value": {"name": "loki-server", "type": "loki", "loki": {"url": "` + lokiURL + `"}}}, {"op": "add", "path": "/spec/pipelines/0/outputRefs/-", "value": "loki-server"}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			g.By("check data in ES")
			ees.waitForIndexAppear(oc, "app")
			ees.waitForProjectLogsAppear(oc, app, "app")
			dataInES := ees.searchDocByQuery(oc, "app", "{\"size\": 1, \"sort\": [{\"@timestamp\": {\"order\":\"desc\"}}], \"query\": {\"match_phrase\": {\"kubernetes.namespace_name\": \""+app+"\"}}}")
			k8sLabelsInES := dataInES.Hits.DataHits[0].Source.Kubernetes.Lables
			o.Expect(len(k8sLabelsInES) > 0).Should(o.BeTrue())
			o.Expect(reflect.DeepEqual(processedLabels, k8sLabelsInES)).Should(o.BeTrue())

			g.By("check data in Loki")
			route := "http://" + getRouteAddress(oc, loki.namespace, loki.name)
			lc := newLokiClient(route)
			err = wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
				appLogs, err := lc.searchByNamespace("", app)
				if err != nil {
					return false, err
				}
				if appLogs.Status == "success" && len(appLogs.Data.Result) > 0 {
					return true, nil
				}
				return false, nil
			})
			compat_otp.AssertWaitPollNoErr(err, "can't find app logs")
			dataInLoki, _ := lc.searchByNamespace("", app)
			lokiLog := extractLogEntities(dataInLoki)
			k8sLabelsInLoki := lokiLog[0].Kubernetes.Lables
			o.Expect(reflect.DeepEqual(processedLabels, k8sLabelsInLoki)).Should(o.BeTrue())
			flatLabelsInLoki := lokiLog[0].Kubernetes.FlatLabels
			o.Expect(len(flatLabelsInLoki) == 0).Should(o.BeTrue())
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Critical-74927-Forward logs to elasticsearch 8.x.", func() {
			compat_otp.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "8",
				serverName: "elasticsearch-server",
				httpSSL:    true,
				clientAuth: true,
				userAuth:   true,
				username:   "user1",
				password:   getRandomString(),
				secretName: "ees-74927",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			compat_otp.By("Create project for app logs and deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", loglabeltemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("Create ClusterLogForwarder")
			clf := clusterlogforwarder{
				name:                      "clf-74927",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-userauth-mtls.yaml"),
				secretName:                ees.secretName,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=https://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version, "INDEX=logging-74927.{.log_type||\"none-typed-logs\"}-write",
				`TUNING={"compression": "zlib", "deliveryMode": "AtLeastOnce", "maxRetryDuration": 30, "maxWrite": "20M", "minRetryDuration": 10}`)
			clf.update(oc, "", `[{"op": "add", "path": "/spec/outputs/0/rateLimit", value: {"maxRecordsPerSecond": 5000}}]`, "--type=json")
			clf.waitForCollectorPodsReady(oc)

			compat_otp.By("Check logs in ES")
			ees.waitForIndexAppear(oc, "logging-74927.application-write")
			ees.waitForIndexAppear(oc, "logging-74927.infrastructure-write")
			ees.waitForIndexAppear(oc, "logging-74927.audit-write")

			compat_otp.By("Check configurations in collector pods")
			expectedConfigs := []string{
				`[transforms.output_es_created_by_user_throttle]
type = "throttle"
inputs = ["pipeline_forward_to_external_es_viaq_0"]
window_secs = 1
threshold = 5000`,
				`compression = "zlib"`,
				`[sinks.output_es_created_by_user.batch]
max_bytes = 20000000`,
				`[sinks.output_es_created_by_user.buffer]
type = "disk"
when_full = "block"
max_size = 268435488`,
				`[sinks.output_es_created_by_user.request]
retry_initial_backoff_secs = 10
retry_max_duration_secs = 30`,
			}
			result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", expectedConfigs...)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).Should(o.BeTrue())
		})
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-High-74322-CLO expose Metrics,AlertRule and Telemetry", func() {
			g.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "8",
				serverName: "elasticsearch-server",
				httpSSL:    true,
				secretName: "ees-https",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			g.By("Create project for app and deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", loglabeltemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Create ClusterLogForwarder instance")
			clf := clusterlogforwarder{
				name:                      "clf-74322",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-https.yaml"),
				secretName:                ees.secretName,
				collectApplicationLogs:    true,
				collectInfrastructureLogs: false,
				collectAuditLogs:          false,
				waitForPodReady:           true,
				serviceAccountName:        "test-clf-" + getRandomString(),
				enableMonitoring:          true,
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=https://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version, `INPUT_REFS=["application"]`, `TUNING={"deliveryMode": "AtLeastOnce"}`)

			g.By("Waiting until logs appears")
			ees.waitForIndexAppear(oc, "app")

			compat_otp.By("deploy logfilesmetricexporter")
			lfme := logFileMetricExporter{
				name:          "instance",
				namespace:     "openshift-logging",
				template:      filepath.Join(loggingBaseDir, "logfilemetricexporter", "lfme.yaml"),
				waitPodsReady: true,
			}
			defer lfme.delete(oc)
			lfme.create(oc)

			compat_otp.By("verify prometheusrules rules collector is defined")
			err = oc.AsAdmin().WithoutNamespace().Run("get").Args("prometheusrules", "collector", "-n", "openshift-logging").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("verify prometheusrules rules cluster-logging-operator-telemetry is defined")
			err = oc.AsAdmin().WithoutNamespace().Run("get").Args("prometheusrules", "cluster-logging-operator-telemetry", "-n", "openshift-logging").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("verify metrics required by collector,cluster-logging-operator are exposed")
			prometheusToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
			checkCLOMetric(oc, prometheusToken)
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-86567-Add custom headers when forwarding to Elasticsearch output.", func() {
			g.By("Create external Elasticsearch instance")
			esProj := oc.Namespace()
			ees := externalES{
				namespace:  esProj,
				version:    "8",
				serverName: "elasticsearch-server",
				httpSSL:    true,
				secretName: "ees-86567",
				loggingNS:  esProj,
			}
			defer ees.remove(oc)
			ees.deploy(oc)

			g.By("Create ClusterLogForwarder")
			clf := clusterlogforwarder{
				name:                      "clf-86567",
				namespace:                 esProj,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-https.yaml"),
				secretName:                ees.secretName,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "ES_URL=https://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version, "INDEX=logging-86567.{.log_type||\"none-typed-logs\"}-write")
			clf.update(oc, "", `[{"op": "add", "path": "/spec/outputs/0/elasticsearch/headers", "value": {"test": "logging", "Accept": "application/json"}}]`, "--type=json")
			clf.waitForCollectorPodsReady(oc)

			g.By("Create project for app logs and deploy the log generator app")
			oc.SetupProject()
			appProj := oc.Namespace()
			loglabeltemplate := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", loglabeltemplate).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Check configurations in collector pods")
			expectedConfigs := []string{
				`[sinks.output_es_created_by_user.request.headers]
Accept = "application/json"
test = "logging"`,
			}
			result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", expectedConfigs...)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).Should(o.BeTrue())

			g.By("Check logs in ES")
			ees.waitForIndexAppear(oc, "logging-86567.infrastructure-write")
			ees.waitForIndexAppear(oc, "logging-86567.audit-write")
			ees.waitForIndexAppear(oc, "logging-86567.application-write")

			g.By("Should raise error when invalid headers are added")
			clf.update(oc, "", `[{"op": "replace", "path": "/spec/outputs/0/elasticsearch/headers", "value": {"Content-Type": "application/json", "Authorization": "AFAETHBDFHG", "Accept": "application/json", "test": "logging"}}]`, "--type=json")
			checkResource(oc, true, false, "invalid headers found: Authorization,Content-Type", []string{"clusterlogforwarders.observability.openshift.io", clf.name, "-n", clf.namespace, `-ojsonpath={.status.outputConditions[?(@.reason=="ValidationFailure")].message}`})

		})

	})

})
