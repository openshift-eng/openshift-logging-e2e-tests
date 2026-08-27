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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease scheduler", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("log-scheduler")
		loggingBaseDir string
	)

	g.BeforeEach(func() {
		loggingBaseDir = testdata.FixturePath("logging")
		CLO := SubscriptionObjects{
			OperatorName:  "cluster-logging-operator",
			Namespace:     cloNS,
			PackageName:   "cluster-logging",
			Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}

		LO := SubscriptionObjects{
			OperatorName:       "loki-operator-controller-manager",
			Namespace:          loNS,
			PackageName:        "loki-operator",
			Subscription:       filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
			OperatorGroup:      filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			SkipCaseWhenFailed: true,
		}

		g.By("Deploy CLO &LO")
		CLO.SubscribeOperator(oc)
		LO.SubscribeOperator(oc)
		oc.SetupProject()
	})
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-Critical-74398-Manage logging collector pods via CLF[PRGate][CLO][Serial]", func() {
		compat_otp.By("deploy loki stack")
		s := getStorageType(oc)
		sc, err := getStorageClassName(oc)
		if err != nil || len(sc) == 0 {
			g.Skip("can't get storageclass from cluster, skip this case")
		}

		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
		ls := lokiStack{
			name:          "loki-74398",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   s,
			storageSecret: "storage-secret-74398",
			storageClass:  sc,
			bucketName:    "logging-loki-74398-" + getInfrastructureName(oc),
			template:      lokiStackTemplate,
		}
		defer ls.removeObjectStorage(oc)
		err = ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("create a CLF to test forward to lokistack")
		clf := clusterlogforwarder{
			name:                      "clf-74398",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector-74398",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret-74398",
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			waitForPodReady:           true,
		}
		clf.createServiceAccount(oc)
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer resource{"secret", clf.secretName, clf.namespace}.clear(oc)
		ls.createSecretFromGateway(oc, clf.secretName, clf.namespace, "")
		defer clf.delete(oc)
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace)

		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)
		for _, logType := range []string{"infrastructure", "audit"} {
			lc.waitForLogsAppearByKey(logType, "log_type", logType)
		}

		compat_otp.By("check configurations in collector pods")
		checkResource(oc, true, true, `{"limits":{"cpu":"6","memory":"2Gi"},"requests":{"cpu":"500m","memory":"64Mi"}}`, []string{"daemonset", clf.name, "-n", clf.namespace, "-ojsonpath={.spec.template.spec.containers[].resources}"})
		checkResource(oc, true, true, `{"kubernetes.io/os":"linux"}`, []string{"daemonset", clf.name, "-n", clf.namespace, "-ojsonpath={.spec.template.spec.nodeSelector}"})
		checkResource(oc, true, true, `[{"effect":"NoSchedule","key":"node-role.kubernetes.io/master","operator":"Exists"},{"effect":"NoSchedule","key":"node.kubernetes.io/disk-pressure","operator":"Exists"}]`, []string{"daemonset", clf.name, "-n", clf.namespace, "-ojsonpath={.spec.template.spec.tolerations}"})

		compat_otp.By("update collector configurations in CLF")
		patch := `[{"op":"add","path":"/spec/collector","value":{"nodeSelector":{"logging":"test"},"resources":{"limits":{"cpu":1,"memory":"3Gi"},"requests":{"cpu":1,"memory":"1Gi","ephemeral-storage":"2Gi"}},"tolerations":[{"effect":"NoExecute","key":"test","operator":"Equal","tolerationSeconds":3000,"value":"logging"}]}},{"op":"add","path":"/metadata/annotations","value":{"observability.openshift.io/max-unavailable-rollout": "10%", "observability.openshift.io/use-apiserver-cache": "true"}}]`
		clf.update(oc, "", patch, "--type=json")
		WaitUntilPodsAreGone(oc, clf.namespace, "app.kubernetes.io/component=collector")
		checkResource(oc, true, true, `{"limits":{"cpu":"1","memory":"3Gi"},"requests":{"cpu":"1","ephemeral-storage":"2Gi","memory":"1Gi"}}`, []string{"daemonset", clf.name, "-n", clf.namespace, "-ojsonpath={.spec.template.spec.containers[].resources}"})
		checkResource(oc, true, true, `{"rollingUpdate":{"maxSurge":0,"maxUnavailable":"10%"},"type":"RollingUpdate"}`, []string{"daemonset", clf.name, "-n", clf.namespace, "-ojsonpath={.spec.updateStrategy}"})
		checkResource(oc, true, true, `{"kubernetes.io/os":"linux","logging":"test"}`, []string{"daemonset", clf.name, "-n", clf.namespace, "-ojsonpath={.spec.template.spec.nodeSelector}"})
		checkResource(oc, true, true, `[{"effect":"NoSchedule","key":"node-role.kubernetes.io/master","operator":"Exists"},{"effect":"NoSchedule","key":"node.kubernetes.io/disk-pressure","operator":"Exists"},{"effect":"NoExecute","key":"test","operator":"Equal","tolerationSeconds":3000,"value":"logging"}]`, []string{"daemonset", clf.name, "-n", clf.namespace, "-ojsonpath={.spec.template.spec.tolerations}"})

		//check vector.toml, use use_apiserver_cache are added into application logs inputs
		searchString1 := ` rotate_wait_secs = 5
use_apiserver_cache = true

[transforms.input_infrastructure_container_meta]`
		searchString2 := ` rotate_wait_secs = 5
use_apiserver_cache = true

[transforms.input_infrastructure_container_meta]`

		_, err = checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString1, searchString2)
		o.Expect(err).NotTo(o.HaveOccurred())

		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		compat_otp.By("remove the nodeSelector, collector pods should be deployed")
		patch = `[{"op": "remove", "path": "/spec/collector/nodeSelector"}]`
		clf.update(oc, "", patch, "--type=json")
		clf.waitForCollectorPodsReady(oc)
		lc.waitForLogsAppearByProject("application", appProj)
	})
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:anli-High-81398-set collector deamoset affinity/anti-affinity", func() {
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
			name:                      "clf-81398",
			namespace:                 syslogProj,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "affinity-81398.yaml"),
			waitForPodReady:           false,
			collectApplicationLogs:    true,
			collectAuditLogs:          false,
			collectInfrastructureLogs: false,
			serviceAccountName:        "test-clf-" + getRandomString(),
		}
		defer clf.delete(oc)
		clf.create(oc, "URL=udp://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:514", "NAMESPACE_PATTERN="+appProj, "INPUT_REFS=[\"app-input-namespace\"]")
		clfAffinity := `{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"preference":{"matchExpressions":[{"key":"label-1","operator":"Exists"}]},"weight":1},{"preference":{"matchExpressions":[{"key":"label-2","operator":"In","values":["key-2"]}]},"weight":50}],"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/os","operator":"In","values":["linux"]}]},{"matchExpressions":[{"key":"node-role.kubernetes.io/worker","operator":"Exists"}]}]}},"podAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"podAffinityTerm":{"labelSelector":{"matchExpressions":[{"key":"qe-test","operator":"In","values":["value1"]}]},"topologyKey":"kubernetes.io/hostname"},"weight":50}],"requiredDuringSchedulingIgnoredDuringExecution":[{"labelSelector":{"matchExpressions":[{"key":"run","operator":"In","values":["centos-logtest"]}]},"namespaceSelector":{},"topologyKey":"kubernetes.io/hostname"}]},"podAntiAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"podAffinityTerm":{"labelSelector":{"matchExpressions":[{"key":"security","operator":"In","values":["S2"]}]},"topologyKey":"topology.kubernetes.io/zone"},"weight":100}]}}`

		g.By("Check app-container.log in rsyslog server")
		rsyslog.checkData(oc, true, "app-container.log")

		g.By("Verfy there is affinity in the damonset")
		dsAffinity, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("ds", clf.name, "-n", clf.namespace, `-ojsonpath={.spec.template.spec.affinity}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		g.By("Verfy affinity is expected")
		o.Expect(dsAffinity == clfAffinity).Should(o.BeTrue(), "The affanity in damonset: %v", dsAffinity)

		g.By("Verify there is running collector pod binding to pod run=centos-logtest")
		pods, _ := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: "run=centos-logtest"})
		loggenNodeIP, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", appProj, "pod", pods.Items[0].Name, `-ojsonpath={.status.hostIP}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		collectorRunNodeIPs, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", clf.namespace, "pod", "-l app.kubernetes.io/instance="+clf.name, `-ojsonpath={.items[?(@.status.phase=="Running")].status.hostIP}}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(strings.Contains(collectorRunNodeIPs, loggenNodeIP)).Should(o.BeTrue())
	})
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:anli-High-81397-set collector deployment affinity/anti-affinity", func() {
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
			name:                      "clf-81397",
			namespace:                 syslogProj,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "affinity-81397.yaml"),
			waitForPodReady:           false,
			collectApplicationLogs:    false,
			collectAuditLogs:          false,
			collectInfrastructureLogs: false,
			serviceAccountName:        "test-clf-" + getRandomString(),
		}
		defer clf.delete(oc)
		clf.create(oc, "URL=udp://"+rsyslog.serverName+"."+rsyslog.namespace+".svc:514")

		//Note: Some pods may in pending status as no affinity pod can be found, so we only validate one pod is running here
		g.By("Wait the collector deployment one pod is ready")
		var deploymentReplicas int32 = 0
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
			deployment, err := oc.AdminKubeClient().AppsV1().Deployments(clf.namespace).Get(context.Background(), clf.name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					e2e.Logf("Waiting for deployment/%s to appear\n", clf.name)
					return false, nil
				}
				return false, err
			}
			deploymentReplicas = *deployment.Spec.Replicas
			//Exist the AvailableReplicas is not zero
			if deployment.Status.AvailableReplicas != 0 {
				return true, nil
			}
			return false, nil
		})
		compat_otp.AssertWaitPollNoErr(err, "the collector deployment is not available")
		o.Expect(deploymentReplicas == 2).Should(o.BeTrue(), "The deployment.Spec.Replicas is not 2")

		g.By("Verfy there is affinity in the deployment")
		dpAffinity, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("deployment", clf.name, "-n", clf.namespace, `-ojsonpath={.spec.template.spec.affinity}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		clfAffinity := `{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"preference":{"matchExpressions":[{"key":"label-1","operator":"Exists"}]},"weight":1},{"preference":{"matchExpressions":[{"key":"label-2","operator":"In","values":["key-2"]}]},"weight":50}],"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/os","operator":"In","values":["linux"]}]},{"matchExpressions":[{"key":"node-role.kubernetes.io/worker","operator":"Exists"}]}]}},"podAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"podAffinityTerm":{"labelSelector":{"matchExpressions":[{"key":"qe-test","operator":"In","values":["value1"]}]},"topologyKey":"kubernetes.io/hostname"},"weight":50}],"requiredDuringSchedulingIgnoredDuringExecution":[{"labelSelector":{"matchExpressions":[{"key":"run","operator":"In","values":["centos-logtest"]}]},"namespaceSelector":{},"topologyKey":"kubernetes.io/hostname"}]},"podAntiAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"podAffinityTerm":{"labelSelector":{"matchExpressions":[{"key":"security","operator":"In","values":["S2"]}]},"topologyKey":"topology.kubernetes.io/zone"},"weight":100}]}}`
		g.By("Verfy affinity is expected")
		o.Expect(dpAffinity == clfAffinity).Should(o.BeTrue(), "The affanity in deployment: %v", dpAffinity)

		g.By("Verify there is running collector pod binding to pod run=centos-logtest")
		pods, _ := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: "run=centos-logtest"})
		loggenNodeIP, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", appProj, "pod", pods.Items[0].Name, `-ojsonpath={.status.hostIP}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		collectorRunNodeIPs, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", clf.namespace, "pod", "-l app.kubernetes.io/instance="+clf.name, `-ojsonpath={.items[?(@.status.phase=="Running")].status.hostIP}}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(strings.Contains(collectorRunNodeIPs, loggenNodeIP)).Should(o.BeTrue())

		g.By("Verify record from https are send as audit logs")
		o.Expect(postDataToHttpserver(oc, "https://"+clf.name+"-collector-receiver."+clf.namespace+".svc:8443", `{"data":"record1"}`)).To(o.BeTrue())
		rsyslog.checkData(oc, true, "audit-kubeAPI.log")
	})
})
