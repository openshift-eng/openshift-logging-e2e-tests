package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	"github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease - LokiStack Performance Test", func() {
	defer g.GinkgoRecover()

	var (
		oc = exutil.NewCLIWithoutNamespace("lokistack-performace-test")
		loggingBaseDir, sc string
		nodes              *corev1.NodeList
		workerNodeCount    int
	)

	g.BeforeEach(func() {

		// Check worker nodes count
		var err error
		nodes, err = oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/worker=,kubernetes.io/os=linux",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		workerNodeCount = len(nodes.Items)
		if workerNodeCount == 0 {
			g.Skip("Skipping test: No worker nodes available in the cluster")
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
		compat_otp.By("deploy CLO and LO")
		CLO.SubscribeOperator(oc)
		LO.SubscribeOperator(oc)
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Longduration-High-84898-Performance-Vector-LokiStack-Performance test from vector to LokiStack using 1x.extra-small t-shirt size and ViaQ datamodel with log loss measurement[Serial][Slow]", func() {
		// Performance test to measure throughput and log loss from vector to LokiStack with 1x.extra-small configuration

		if !validateInfraAndResourcesForLoki(oc, "36Gi", "18") {
			g.Skip("Current platform not supported/resources not available for this test!")
		}

		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")

		ls := lokiStack{
			name:          "lokistack-performance-84898",
			namespace:     loggingNS,
			tSize:         "1x.extra-small",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-performance-84898",
			storageClass:  sc,
			bucketName:    "logging-loki-performance-" + getRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Patch LokiStack with custom ingestion limits for performance testing")
		patchData := `{
				"spec": {
					"limits": {
						"global": {
							"ingestion": {
								"ingestionBurstSize": 50,
								"ingestionRate": 16,
								"maxGlobalStreamsPerTenant": 5000
							}
						}
					}
				}
			}`
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("lokistack", ls.name, "-n", ls.namespace,
			"-p", patchData, "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("LokiStack patched with ingestionRate: 16, ingestionBurstSize: 50, maxGlobalStreamsPerTenant: 5000")
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Create ClusterLogForwarder for performance testing using ViaQ datamodel")
		clf := clusterlogforwarder{
			name:                      "clf-performance-84898",
			namespace:                 loggingNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			serviceAccountName:        "logcollector-performance-84898",
			secretName:                "lokistack-performance-84898",
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
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace, "INPUT_REFS=[\"application\"]")

		var (
			jsonLogFile        = filepath.Join(loggingBaseDir, "generatelog", "logging-performance-app-generator.json")
			targetTotalLogs    = int64(3600000)   // NUM_LINES: 3.6M total logs
			totalRatePerMinute = 120000.0         // RATE: 120K logs/minute total across all pods
			testDuration       = 30 * time.Minute // 30 minutes duration
			logLineLength      = 1000             // LINE_LENGTH: characters per log line
		)

		compat_otp.By("Deploy application pods on all worker nodes")
		// Calculate distribution across pods
		podsPerNode := 8 // 8 pods per worker node for higher log volume
		totalPods := workerNodeCount * podsPerNode
		ratePerPodPerMinute := totalRatePerMinute / float64(totalPods) // Rate per pod per minute
		numLinesPerPod := targetTotalLogs / int64(totalPods)           // Total logs per pod
		totalExpectedLogs := targetTotalLogs
		e2e.Logf("Performance test configuration:")
		e2e.Logf("  - Worker nodes: %d", workerNodeCount)
		e2e.Logf("  - Pods per node: %d", podsPerNode)
		e2e.Logf("  - Total pods: %d", totalPods)
		e2e.Logf("  - Rate per pod: %.1f logs/minute", ratePerPodPerMinute)
		e2e.Logf("  - NUM_LINES per pod: %d", numLinesPerPod)
		e2e.Logf("  - Total rate: %.0f logs/minute", totalRatePerMinute)
		e2e.Logf("  - Target total logs: %d (%.1fM)", targetTotalLogs, float64(targetTotalLogs)/1000000)
		e2e.Logf("  - Test duration: %v", testDuration)
		e2e.Logf("  - Log line length: %d chars", logLineLength)
		e2e.Logf("  - Expected total log size: %.2f GB", float64(targetTotalLogs*int64(logLineLength))/(1024*1024*1024))

		g.By("Create application generator project with pods distributed across all worker nodes")
		oc.SetupProject()
		appProj := oc.Namespace()

		for i, node := range nodes.Items {
			rcName := fmt.Sprintf("logging-centos-logtest-node-%d", i)
			configMapName := fmt.Sprintf("logtest-config-node-%d", i)
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile,
				"-p", fmt.Sprintf("RATE=%.1f", ratePerPodPerMinute),
				"-p", fmt.Sprintf("NUM_LINES=%d", numLinesPerPod),
				"-p", fmt.Sprintf("REPLICAS=%d", podsPerNode),
				"-p", fmt.Sprintf("LINE_LENGTH=%d", logLineLength),
				"-p", fmt.Sprintf("REPLICATIONCONTROLLER=%s", rcName),
				"-p", fmt.Sprintf("CONFIGMAP=%s", configMapName)).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			e2e.Logf("Deployed %d pods on worker node: %s", podsPerNode, node.Name)
		}

		g.By("Wait until all application generator pods are running before log loss measurement")
		const appPodLabelSelector = "run=centos-logtest"
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(_ context.Context) (bool, error) {
			pods, listErr := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: appPodLabelSelector})
			if listErr != nil {
				return false, listErr
			}
			if n := len(pods.Items); n != totalPods {
				e2e.Logf("Waiting for %d pods with label %s, have %d", totalPods, appPodLabelSelector, n)
				return false, nil
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					e2e.Logf("Pod %s is %s, waiting for Running", pod.Name, pod.Status.Phase)
					return false, nil
				}
			}
			return true, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for %d application pods to be running", totalPods)

		// Use the calculated expected logs from pod distribution
		performanceMetrics.LogsSent = totalExpectedLogs

		e2e.Logf("Starting log loss measurement: Expected %d logs over %v from %d pods", totalExpectedLogs, testDuration, totalPods)

		// Record start time for log loss measurement
		measurementStartTime := time.Now()

		g.By("Measure application log performance and count metrics during test period")

		// Wait for the test duration to allow log generation
		time.Sleep(testDuration)
		measurementEndTime := time.Now()

		// Use expected logs from calculation as baseline
		performanceMetrics.LogsSent = totalExpectedLogs

		g.By("Query Loki to count ingested logs within measurement time window")

		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)

		// Build a window equal to the test duration + buffer for late ingestion.
		testDurationMinutes := int(math.Ceil(testDuration.Minutes())) + 5 // 5m buffer
		timeRangeQuery := fmt.Sprintf(
			`sum(count_over_time({log_type="application",kubernetes_namespace_name="%s"}[%dm]))`,
			appProj, testDurationMinutes,
		)
		e2e.Logf("Loki query: %s", timeRangeQuery)

		// Use FORWARD direction so the last sample is the newest, with a reasonable limit
		appLogs, err := lc.queryRange("application", timeRangeQuery, 1000, measurementStartTime, measurementEndTime, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		var logsInLoki int64
		e2e.Logf("Loki query response status: %s", appLogs.Status)

		if appLogs.Status == "success" && len(appLogs.Data.Result) > 0 {
			// sum(...) should return a single time series
			result := appLogs.Data.Result[0]
			e2e.Logf("Samples in result: %d", len(result.Values))
			if n := len(result.Values); n > 0 {
				last := result.Values[n-1] // newest sample (because FORWARD)
				if valueSlice, ok := last.([]any); ok && len(valueSlice) >= 2 {
					if countStr, ok := valueSlice[1].(string); ok {
						if count, err := strconv.ParseInt(countStr, 10, 64); err == nil {
							logsInLoki = count
						} else {
							e2e.Logf("failed to parse count: %v", err)
						}
					}
				}
			}
		} else {
			e2e.Logf("Query failed or returned no results. Status: %s", appLogs.Status)
		}

		performanceMetrics.LogsReceived = logsInLoki
		e2e.Logf("Total logs received (window=%dm): %d", testDurationMinutes, logsInLoki)

		e2e.Logf("Loki query results:")
		e2e.Logf("  - Logs found in Loki: %d", logsInLoki)
		e2e.Logf("  - Expected logs: %d", performanceMetrics.LogsSent)

		// Calculate log loss percentage
		if performanceMetrics.LogsSent > 0 {
			performanceMetrics.LogLossPercentage = float64(performanceMetrics.LogsSent-performanceMetrics.LogsReceived) / float64(performanceMetrics.LogsSent) * 100
		}

		// Log loss validation

		e2e.Logf("Expected Logs: %d", performanceMetrics.LogsSent)
		e2e.Logf("Received Logs: %d", performanceMetrics.LogsReceived)
		e2e.Logf("Log Loss: %.2f%%", performanceMetrics.LogLossPercentage)

		o.Expect(performanceMetrics.LogsReceived).Should(o.BeNumerically(">", 0),
			"LogsReceived should be greater than 0")
		o.Expect(performanceMetrics.LogLossPercentage).Should(o.BeNumerically("<", 10.0),
			"Log loss percentage should be less than 10% for 1x.extra-small LokiStack")
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Longduration-High-84975-Performance-Vector-LokiStack-Performance test from vector to LokiStack using 1x.extra-small t-shirt size and Otel datamodel with log loss measurement[Serial][Slow]", func() {

		if !validateInfraAndResourcesForLoki(oc, "36Gi", "18") {
			g.Skip("Current platform not supported/resources not available for this test!")
		}

		// Performance test to measure throughput and log loss from vector to LokiStack with 1x.extra-small configuration
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")

		ls := lokiStack{
			name:          "lokistack-performance-84975",
			namespace:     loggingNS,
			tSize:         "1x.extra-small",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-performance-84975",
			storageClass:  sc,
			bucketName:    "logging-loki-performance-" + getRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Patch LokiStack with custom ingestion limits for performance testing")
		patchData := `{
				"spec": {
					"limits": {
						"global": {
							"ingestion": {
								"ingestionBurstSize": 50,
								"ingestionRate": 16,
								"maxGlobalStreamsPerTenant": 5000
							}
						}
					}
				}
			}`
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("lokistack", ls.name, "-n", ls.namespace,
			"-p", patchData, "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("LokiStack patched with ingestionRate: 16, ingestionBurstSize: 50, maxGlobalStreamsPerTenant: 5000")
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Create ClusterLogForwarder for performance testing using Otel datamodel")
		clf := clusterlogforwarder{
			name:                      "clf-performance-84975",
			namespace:                 loggingNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			serviceAccountName:        "logcollector-performance-84975",
			secretName:                "lokistack-performance-84975",
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
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace, "DATAMODEL=Otel", "INPUT_REFS=[\"application\"]")

		var (
			jsonLogFile        = filepath.Join(loggingBaseDir, "generatelog", "logging-performance-app-generator.json")
			targetTotalLogs    = int64(3600000)   // NUM_LINES: 3.6M total logs
			totalRatePerMinute = 120000.0         // RATE: 120K logs/minute total across all pods
			testDuration       = 30 * time.Minute // 30 minutes duration
			logLineLength      = 1000             // LINE_LENGTH: characters per log line
		)

		compat_otp.By("Deploy application pods on all worker nodes")
		// Calculate distribution across pods
		podsPerNode := 8 // 8 pods per worker node for higher log volume
		totalPods := workerNodeCount * podsPerNode
		ratePerPodPerMinute := totalRatePerMinute / float64(totalPods) // Rate per pod per minute
		numLinesPerPod := targetTotalLogs / int64(totalPods)           // Total logs per pod
		totalExpectedLogs := targetTotalLogs
		e2e.Logf("Performance test configuration:")
		e2e.Logf("  - Worker nodes: %d", workerNodeCount)
		e2e.Logf("  - Pods per node: %d", podsPerNode)
		e2e.Logf("  - Total pods: %d", totalPods)
		e2e.Logf("  - Rate per pod: %.1f logs/minute", ratePerPodPerMinute)
		e2e.Logf("  - NUM_LINES per pod: %d", numLinesPerPod)
		e2e.Logf("  - Total rate: %.0f logs/minute", totalRatePerMinute)
		e2e.Logf("  - Target total logs: %d (%.1fM)", targetTotalLogs, float64(targetTotalLogs)/1000000)
		e2e.Logf("  - Test duration: %v", testDuration)
		e2e.Logf("  - Log line length: %d chars", logLineLength)
		e2e.Logf("  - Expected total log size: %.2f GB", float64(targetTotalLogs*int64(logLineLength))/(1024*1024*1024))

		g.By("Create application generator project with pods distributed across all worker nodes")
		oc.SetupProject()
		appProj := oc.Namespace()

		for i, node := range nodes.Items {
			rcName := fmt.Sprintf("logging-centos-logtest-node-%d", i)
			configMapName := fmt.Sprintf("logtest-config-node-%d", i)
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile,
				"-p", fmt.Sprintf("RATE=%.1f", ratePerPodPerMinute),
				"-p", fmt.Sprintf("NUM_LINES=%d", numLinesPerPod),
				"-p", fmt.Sprintf("REPLICAS=%d", podsPerNode),
				"-p", fmt.Sprintf("LINE_LENGTH=%d", logLineLength),
				"-p", fmt.Sprintf("REPLICATIONCONTROLLER=%s", rcName),
				"-p", fmt.Sprintf("CONFIGMAP=%s", configMapName)).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			e2e.Logf("Deployed %d pods on worker node: %s", podsPerNode, node.Name)
		}

		g.By("Wait until all application generator pods are running before log loss measurement")
		const appPodLabelSelector = "run=centos-logtest"
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(_ context.Context) (bool, error) {
			pods, listErr := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: appPodLabelSelector})
			if listErr != nil {
				return false, listErr
			}
			if n := len(pods.Items); n != totalPods {
				e2e.Logf("Waiting for %d pods with label %s, have %d", totalPods, appPodLabelSelector, n)
				return false, nil
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					e2e.Logf("Pod %s is %s, waiting for Running", pod.Name, pod.Status.Phase)
					return false, nil
				}
			}
			return true, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for %d application pods to be running", totalPods)

		// Use the calculated expected logs from pod distribution
		performanceMetrics.LogsSent = totalExpectedLogs

		e2e.Logf("Starting log loss measurement: Expected %d logs over %v from %d pods", totalExpectedLogs, testDuration, totalPods)

		// Record start time for log loss measurement
		measurementStartTime := time.Now()

		g.By("Measure application log performance and count metrics during test period")

		// Wait for the test duration to allow log generation
		time.Sleep(testDuration)
		measurementEndTime := time.Now()

		// Use expected logs from calculation as baseline
		performanceMetrics.LogsSent = totalExpectedLogs

		g.By("Query Loki to count ingested logs within measurement time window")

		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)

		// Build a window equal to the test duration + buffer for late ingestion.
		testDurationMinutes := int(math.Ceil(testDuration.Minutes())) + 5 // 5m buffer
		timeRangeQuery := fmt.Sprintf(
			`sum(count_over_time({log_type="application",kubernetes_namespace_name="%s"}[%dm]))`,
			appProj, testDurationMinutes,
		)
		e2e.Logf("Loki query: %s", timeRangeQuery)

		// Use FORWARD direction so the last sample is the newest, with a reasonable limit
		appLogs, err := lc.queryRange("application", timeRangeQuery, 1000, measurementStartTime, measurementEndTime, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		var logsInLoki int64
		e2e.Logf("Loki query response status: %s", appLogs.Status)

		if appLogs.Status == "success" && len(appLogs.Data.Result) > 0 {
			// sum(...) should return a single time series
			result := appLogs.Data.Result[0]
			e2e.Logf("Samples in result: %d", len(result.Values))
			if n := len(result.Values); n > 0 {
				last := result.Values[n-1] // newest sample (because FORWARD)
				if valueSlice, ok := last.([]any); ok && len(valueSlice) >= 2 {
					if countStr, ok := valueSlice[1].(string); ok {
						if count, err := strconv.ParseInt(countStr, 10, 64); err == nil {
							logsInLoki = count
						} else {
							e2e.Logf("failed to parse count: %v", err)
						}
					}
				}
			}
		} else {
			e2e.Logf("Query failed or returned no results. Status: %s", appLogs.Status)
		}

		performanceMetrics.LogsReceived = logsInLoki
		e2e.Logf("Total logs received (window=%dm): %d", testDurationMinutes, logsInLoki)

		e2e.Logf("Loki query results:")
		e2e.Logf("  - Logs found in Loki: %d", logsInLoki)
		e2e.Logf("  - Expected logs: %d", performanceMetrics.LogsSent)

		// Calculate log loss percentage
		if performanceMetrics.LogsSent > 0 {
			performanceMetrics.LogLossPercentage = float64(performanceMetrics.LogsSent-performanceMetrics.LogsReceived) / float64(performanceMetrics.LogsSent) * 100
		}

		// Log loss validation

		e2e.Logf("Expected Logs: %d", performanceMetrics.LogsSent)
		e2e.Logf("Received Logs: %d", performanceMetrics.LogsReceived)
		e2e.Logf("Log Loss: %.2f%%", performanceMetrics.LogLossPercentage)

		o.Expect(performanceMetrics.LogsReceived).Should(o.BeNumerically(">", 0),
			"LogsReceived should be greater than 0")
		o.Expect(performanceMetrics.LogLossPercentage).Should(o.BeNumerically("<", 10.0),
			"Log loss percentage should be less than 10% for 1x.extra-small LokiStack")
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Longduration-High-87993-Performance-Vector-LokiStack-Performance test from vector to LokiStack using 1x.pico t-shirt size and ViaQ datamodel with log loss measurement[Serial][Slow]", func() {

		if !validateInfraAndResourcesForLoki(oc, "18Gi", "8") {
			g.Skip("Current platform not supported/resources not available for this test!")
		}

		// Performance test to measure throughput and log loss from vector to LokiStack with 1x.pico configuration
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")

		ls := lokiStack{
			name:          "lokistack-performance-87993",
			namespace:     loggingNS,
			tSize:         "1x.pico",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-performance-87993",
			storageClass:  sc,
			bucketName:    "logging-loki-performance-" + getRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Patch LokiStack with custom ingestion limits for performance testing")
		patchData := `{
				"spec": {
					"limits": {
						"global": {
							"ingestion": {
								"ingestionBurstSize": 50,
								"ingestionRate": 16,
								"maxGlobalStreamsPerTenant": 5000
							}
						}
					}
				}
			}`
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("lokistack", ls.name, "-n", ls.namespace,
			"-p", patchData, "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("LokiStack patched with ingestionRate: 16, ingestionBurstSize: 50, maxGlobalStreamsPerTenant: 5000")
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Create ClusterLogForwarder for performance testing using ViaQ datamodel")
		clf := clusterlogforwarder{
			name:                      "clf-performance-87993",
			namespace:                 loggingNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			serviceAccountName:        "logcollector-performance-87993",
			secretName:                "lokistack-performance-87993",
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
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace, "INPUT_REFS=[\"application\"]")

		var (
			jsonLogFile        = filepath.Join(loggingBaseDir, "generatelog", "logging-performance-app-generator.json")
			targetTotalLogs    = int64(3600000)   // NUM_LINES: 3.6M total logs
			totalRatePerMinute = 120000.0         // RATE: 120K logs/minute total across all pods
			testDuration       = 30 * time.Minute // 30 minutes duration
			logLineLength      = 500              // LINE_LENGTH: characters per log line
		)

		compat_otp.By("Deploy application pods on all worker nodes")
		// Calculate distribution across pods
		podsPerNode := 8 // 8 pods per worker node for higher log volume
		totalPods := workerNodeCount * podsPerNode
		ratePerPodPerMinute := totalRatePerMinute / float64(totalPods) // Rate per pod per minute
		numLinesPerPod := targetTotalLogs / int64(totalPods)           // Total logs per pod
		totalExpectedLogs := targetTotalLogs
		e2e.Logf("Performance test configuration:")
		e2e.Logf("  - Worker nodes: %d", workerNodeCount)
		e2e.Logf("  - Pods per node: %d", podsPerNode)
		e2e.Logf("  - Total pods: %d", totalPods)
		e2e.Logf("  - Rate per pod: %.1f logs/minute", ratePerPodPerMinute)
		e2e.Logf("  - NUM_LINES per pod: %d", numLinesPerPod)
		e2e.Logf("  - Total rate: %.0f logs/minute", totalRatePerMinute)
		e2e.Logf("  - Target total logs: %d (%.1fM)", targetTotalLogs, float64(targetTotalLogs)/1000000)
		e2e.Logf("  - Test duration: %v", testDuration)
		e2e.Logf("  - Log line length: %d chars", logLineLength)
		e2e.Logf("  - Expected total log size: %.2f GB", float64(targetTotalLogs*int64(logLineLength))/(1024*1024*1024))

		g.By("Create application generator project with pods distributed across all worker nodes")
		oc.SetupProject()
		appProj := oc.Namespace()

		for i, node := range nodes.Items {
			rcName := fmt.Sprintf("logging-centos-logtest-node-%d", i)
			configMapName := fmt.Sprintf("logtest-config-node-%d", i)
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile,
				"-p", fmt.Sprintf("RATE=%.1f", ratePerPodPerMinute),
				"-p", fmt.Sprintf("NUM_LINES=%d", numLinesPerPod),
				"-p", fmt.Sprintf("REPLICAS=%d", podsPerNode),
				"-p", fmt.Sprintf("LINE_LENGTH=%d", logLineLength),
				"-p", fmt.Sprintf("REPLICATIONCONTROLLER=%s", rcName),
				"-p", fmt.Sprintf("CONFIGMAP=%s", configMapName)).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			e2e.Logf("Deployed %d pods on worker node: %s", podsPerNode, node.Name)
		}

		g.By("Wait until all application generator pods are running before log loss measurement")
		const appPodLabelSelector = "run=centos-logtest"
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(_ context.Context) (bool, error) {
			pods, listErr := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: appPodLabelSelector})
			if listErr != nil {
				return false, listErr
			}
			if n := len(pods.Items); n != totalPods {
				e2e.Logf("Waiting for %d pods with label %s, have %d", totalPods, appPodLabelSelector, n)
				return false, nil
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					e2e.Logf("Pod %s is %s, waiting for Running", pod.Name, pod.Status.Phase)
					return false, nil
				}
			}
			return true, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for %d application pods to be running", totalPods)

		// Use the calculated expected logs from pod distribution
		performanceMetrics.LogsSent = totalExpectedLogs

		e2e.Logf("Starting log loss measurement: Expected %d logs over %v from %d pods", totalExpectedLogs, testDuration, totalPods)

		// Record start time for log loss measurement
		measurementStartTime := time.Now()

		g.By("Measure application log performance and count metrics during test period")

		// Wait for the test duration to allow log generation
		time.Sleep(testDuration)
		measurementEndTime := time.Now()

		// Use expected logs from calculation as baseline
		performanceMetrics.LogsSent = totalExpectedLogs

		g.By("Query Loki to count ingested logs within measurement time window")

		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)

		// Build a window equal to the test duration + buffer for late ingestion.
		testDurationMinutes := int(math.Ceil(testDuration.Minutes())) + 5 // 5m buffer
		timeRangeQuery := fmt.Sprintf(
			`sum(count_over_time({log_type="application",kubernetes_namespace_name="%s"}[%dm]))`,
			appProj, testDurationMinutes,
		)
		e2e.Logf("Loki query: %s", timeRangeQuery)

		// Use FORWARD direction so the last sample is the newest, with a reasonable limit
		appLogs, err := lc.queryRange("application", timeRangeQuery, 1000, measurementStartTime, measurementEndTime, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		var logsInLoki int64
		e2e.Logf("Loki query response status: %s", appLogs.Status)

		if appLogs.Status == "success" && len(appLogs.Data.Result) > 0 {
			// sum(...) should return a single time series
			result := appLogs.Data.Result[0]
			e2e.Logf("Samples in result: %d", len(result.Values))
			if n := len(result.Values); n > 0 {
				last := result.Values[n-1] // newest sample (because FORWARD)
				if valueSlice, ok := last.([]any); ok && len(valueSlice) >= 2 {
					if countStr, ok := valueSlice[1].(string); ok {
						if count, err := strconv.ParseInt(countStr, 10, 64); err == nil {
							logsInLoki = count
						} else {
							e2e.Logf("failed to parse count: %v", err)
						}
					}
				}
			}
		} else {
			e2e.Logf("Query failed or returned no results. Status: %s", appLogs.Status)
		}

		performanceMetrics.LogsReceived = logsInLoki
		e2e.Logf("Total logs received (window=%dm): %d", testDurationMinutes, logsInLoki)

		e2e.Logf("Loki query results:")
		e2e.Logf("  - Logs found in Loki: %d", logsInLoki)
		e2e.Logf("  - Expected logs: %d", performanceMetrics.LogsSent)

		// Calculate log loss percentage
		if performanceMetrics.LogsSent > 0 {
			performanceMetrics.LogLossPercentage = float64(performanceMetrics.LogsSent-performanceMetrics.LogsReceived) / float64(performanceMetrics.LogsSent) * 100
		}

		// Log loss validation

		e2e.Logf("Expected Logs: %d", performanceMetrics.LogsSent)
		e2e.Logf("Received Logs: %d", performanceMetrics.LogsReceived)
		e2e.Logf("Log Loss: %.2f%%", performanceMetrics.LogLossPercentage)

		o.Expect(performanceMetrics.LogsReceived).Should(o.BeNumerically(">", 0),
			"LogsReceived should be greater than 0")
		o.Expect(performanceMetrics.LogLossPercentage).Should(o.BeNumerically("<", 10.0),
			"Log loss percentage should be less than 10% for 1x.pico LokiStack")
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Longduration-High-87994-Performance-Vector-LokiStack-Performance test from vector to LokiStack using 1x.pico t-shirt size and Otel datamodel with log loss measurement[Serial][Slow]", func() {

		if !validateInfraAndResourcesForLoki(oc, "18Gi", "8") {
			g.Skip("Current platform not supported/resources not available for this test!")
		}

		// Performance test to measure throughput and log loss from vector to LokiStack with 1x.pico configuration
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")

		ls := lokiStack{
			name:          "lokistack-performance-87994",
			namespace:     loggingNS,
			tSize:         "1x.pico",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-performance-87994",
			storageClass:  sc,
			bucketName:    "logging-loki-performance-" + getRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Patch LokiStack with custom ingestion limits for performance testing")
		patchData := `{
				"spec": {
					"limits": {
						"global": {
							"ingestion": {
								"ingestionBurstSize": 50,
								"ingestionRate": 16,
								"maxGlobalStreamsPerTenant": 5000
							}
						}
					}
				}
			}`
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("lokistack", ls.name, "-n", ls.namespace,
			"-p", patchData, "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("LokiStack patched with ingestionRate: 16, ingestionBurstSize: 50, maxGlobalStreamsPerTenant: 5000")
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Create ClusterLogForwarder for performance testing using Otel datamodel")
		clf := clusterlogforwarder{
			name:                      "clf-performance-87994",
			namespace:                 loggingNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			serviceAccountName:        "logcollector-performance-87994",
			secretName:                "lokistack-performance-87994",
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
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace, "DATAMODEL=Otel", "INPUT_REFS=[\"application\"]")

		var (
			jsonLogFile        = filepath.Join(loggingBaseDir, "generatelog", "logging-performance-app-generator.json")
			targetTotalLogs    = int64(3600000)   // NUM_LINES: 3.6M total logs
			totalRatePerMinute = 120000.0         // RATE: 120K logs/minute total across all pods
			testDuration       = 30 * time.Minute // 30 minutes duration
			logLineLength      = 500              // LINE_LENGTH: characters per log line
		)

		compat_otp.By("Deploy application pods on all worker nodes")
		// Calculate distribution across pods
		podsPerNode := 8 // 8 pods per worker node for higher log volume
		totalPods := workerNodeCount * podsPerNode
		ratePerPodPerMinute := totalRatePerMinute / float64(totalPods) // Rate per pod per minute
		numLinesPerPod := targetTotalLogs / int64(totalPods)           // Total logs per pod
		totalExpectedLogs := targetTotalLogs
		e2e.Logf("Performance test configuration:")
		e2e.Logf("  - Worker nodes: %d", workerNodeCount)
		e2e.Logf("  - Pods per node: %d", podsPerNode)
		e2e.Logf("  - Total pods: %d", totalPods)
		e2e.Logf("  - Rate per pod: %.1f logs/minute", ratePerPodPerMinute)
		e2e.Logf("  - NUM_LINES per pod: %d", numLinesPerPod)
		e2e.Logf("  - Total rate: %.0f logs/minute", totalRatePerMinute)
		e2e.Logf("  - Target total logs: %d (%.1fM)", targetTotalLogs, float64(targetTotalLogs)/1000000)
		e2e.Logf("  - Test duration: %v", testDuration)
		e2e.Logf("  - Log line length: %d chars", logLineLength)
		e2e.Logf("  - Expected total log size: %.2f GB", float64(targetTotalLogs*int64(logLineLength))/(1024*1024*1024))

		g.By("Create application generator project with pods distributed across all worker nodes")
		oc.SetupProject()
		appProj := oc.Namespace()

		for i, node := range nodes.Items {
			rcName := fmt.Sprintf("logging-centos-logtest-node-%d", i)
			configMapName := fmt.Sprintf("logtest-config-node-%d", i)
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile,
				"-p", fmt.Sprintf("RATE=%.1f", ratePerPodPerMinute),
				"-p", fmt.Sprintf("NUM_LINES=%d", numLinesPerPod),
				"-p", fmt.Sprintf("REPLICAS=%d", podsPerNode),
				"-p", fmt.Sprintf("LINE_LENGTH=%d", logLineLength),
				"-p", fmt.Sprintf("REPLICATIONCONTROLLER=%s", rcName),
				"-p", fmt.Sprintf("CONFIGMAP=%s", configMapName)).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			e2e.Logf("Deployed %d pods on worker node: %s", podsPerNode, node.Name)
		}

		g.By("Wait until all application generator pods are running before log loss measurement")
		const appPodLabelSelector = "run=centos-logtest"
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(_ context.Context) (bool, error) {
			pods, listErr := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: appPodLabelSelector})
			if listErr != nil {
				return false, listErr
			}
			if n := len(pods.Items); n != totalPods {
				e2e.Logf("Waiting for %d pods with label %s, have %d", totalPods, appPodLabelSelector, n)
				return false, nil
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					e2e.Logf("Pod %s is %s, waiting for Running", pod.Name, pod.Status.Phase)
					return false, nil
				}
			}
			return true, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for %d application pods to be running", totalPods)

		// Use the calculated expected logs from pod distribution
		performanceMetrics.LogsSent = totalExpectedLogs

		e2e.Logf("Starting log loss measurement: Expected %d logs over %v from %d pods", totalExpectedLogs, testDuration, totalPods)

		// Record start time for log loss measurement
		measurementStartTime := time.Now()

		g.By("Measure application log performance and count metrics during test period")

		// Wait for the test duration to allow log generation
		time.Sleep(testDuration)
		measurementEndTime := time.Now()

		// Use expected logs from calculation as baseline
		performanceMetrics.LogsSent = totalExpectedLogs

		g.By("Query Loki to count ingested logs within measurement time window")

		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)

		// Build a window equal to the test duration + buffer for late ingestion.
		testDurationMinutes := int(math.Ceil(testDuration.Minutes())) + 5 // 5m buffer
		timeRangeQuery := fmt.Sprintf(
			`sum(count_over_time({log_type="application",kubernetes_namespace_name="%s"}[%dm]))`,
			appProj, testDurationMinutes,
		)
		e2e.Logf("Loki query: %s", timeRangeQuery)

		// Use FORWARD direction so the last sample is the newest, with a reasonable limit
		appLogs, err := lc.queryRange("application", timeRangeQuery, 1000, measurementStartTime, measurementEndTime, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		var logsInLoki int64
		e2e.Logf("Loki query response status: %s", appLogs.Status)

		if appLogs.Status == "success" && len(appLogs.Data.Result) > 0 {
			// sum(...) should return a single time series
			result := appLogs.Data.Result[0]
			e2e.Logf("Samples in result: %d", len(result.Values))
			if n := len(result.Values); n > 0 {
				last := result.Values[n-1] // newest sample (because FORWARD)
				if valueSlice, ok := last.([]any); ok && len(valueSlice) >= 2 {
					if countStr, ok := valueSlice[1].(string); ok {
						if count, err := strconv.ParseInt(countStr, 10, 64); err == nil {
							logsInLoki = count
						} else {
							e2e.Logf("failed to parse count: %v", err)
						}
					}
				}
			}
		} else {
			e2e.Logf("Query failed or returned no results. Status: %s", appLogs.Status)
		}

		performanceMetrics.LogsReceived = logsInLoki
		e2e.Logf("Total logs received (window=%dm): %d", testDurationMinutes, logsInLoki)

		e2e.Logf("Loki query results:")
		e2e.Logf("  - Logs found in Loki: %d", logsInLoki)
		e2e.Logf("  - Expected logs: %d", performanceMetrics.LogsSent)

		// Calculate log loss percentage
		if performanceMetrics.LogsSent > 0 {
			performanceMetrics.LogLossPercentage = float64(performanceMetrics.LogsSent-performanceMetrics.LogsReceived) / float64(performanceMetrics.LogsSent) * 100
		}

		// Log loss validation

		e2e.Logf("Expected Logs: %d", performanceMetrics.LogsSent)
		e2e.Logf("Received Logs: %d", performanceMetrics.LogsReceived)
		e2e.Logf("Log Loss: %.2f%%", performanceMetrics.LogLossPercentage)

		o.Expect(performanceMetrics.LogsReceived).Should(o.BeNumerically(">", 0),
			"LogsReceived should be greater than 0")
		o.Expect(performanceMetrics.LogLossPercentage).Should(o.BeNumerically("<", 10.0),
			"Log loss percentage should be less than 10% for 1x.pico LokiStack")
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Longduration-High-87998-Performance-Vector-LokiStack-Performance test from vector to LokiStack using 1x.small t-shirt size and ViaQ datamodel with log loss measurement[Serial][Slow]", func() {

		if !validateInfraAndResourcesForLoki(oc, "84Gi", "42") {
			g.Skip("Current platform not supported/resources not available for this test!")
		}

		// Performance test to measure throughput and log loss from vector to LokiStack with 1x.small configuration
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")

		ls := lokiStack{
			name:          "lokistack-performance-87998",
			namespace:     loggingNS,
			tSize:         "1x.small",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-performance-87998",
			storageClass:  sc,
			bucketName:    "logging-loki-performance-" + getRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Patch LokiStack with custom ingestion limits for performance testing")
		patchData := `{
				"spec": {
					"limits": {
						"global": {
							"ingestion": {
								"ingestionBurstSize": 50,
								"ingestionRate": 16,
								"maxGlobalStreamsPerTenant": 5000
							}
						}
					}
				}
			}`
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("lokistack", ls.name, "-n", ls.namespace,
			"-p", patchData, "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("LokiStack patched with ingestionRate: 16, ingestionBurstSize: 50, maxGlobalStreamsPerTenant: 5000")
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Create ClusterLogForwarder for performance testing using ViaQ datamodel")
		clf := clusterlogforwarder{
			name:                      "clf-performance-87998",
			namespace:                 loggingNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			serviceAccountName:        "logcollector-performance-87998",
			secretName:                "lokistack-performance-87998",
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
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace, "INPUT_REFS=[\"application\"]")

		var (
			jsonLogFile        = filepath.Join(loggingBaseDir, "generatelog", "logging-performance-app-generator.json")
			targetTotalLogs    = int64(3600000)   // NUM_LINES: 3.6M total logs
			totalRatePerMinute = 120000.0         // RATE: 120K logs/minute total across all pods
			testDuration       = 30 * time.Minute // 30 minutes duration
			logLineLength      = 1500             // LINE_LENGTH: characters per log line
		)

		compat_otp.By("Deploy application pods on all worker nodes")
		// Calculate distribution across pods
		podsPerNode := 8 // 8 pods per worker node for higher log volume
		totalPods := workerNodeCount * podsPerNode
		ratePerPodPerMinute := totalRatePerMinute / float64(totalPods) // Rate per pod per minute
		numLinesPerPod := targetTotalLogs / int64(totalPods)           // Total logs per pod
		totalExpectedLogs := targetTotalLogs
		e2e.Logf("Performance test configuration:")
		e2e.Logf("  - Worker nodes: %d", workerNodeCount)
		e2e.Logf("  - Pods per node: %d", podsPerNode)
		e2e.Logf("  - Total pods: %d", totalPods)
		e2e.Logf("  - Rate per pod: %.1f logs/minute", ratePerPodPerMinute)
		e2e.Logf("  - NUM_LINES per pod: %d", numLinesPerPod)
		e2e.Logf("  - Total rate: %.0f logs/minute", totalRatePerMinute)
		e2e.Logf("  - Target total logs: %d (%.1fM)", targetTotalLogs, float64(targetTotalLogs)/1000000)
		e2e.Logf("  - Test duration: %v", testDuration)
		e2e.Logf("  - Log line length: %d chars", logLineLength)
		e2e.Logf("  - Expected total log size: %.2f GB", float64(targetTotalLogs*int64(logLineLength))/(1024*1024*1024))

		g.By("Create application generator project with pods distributed across all worker nodes")
		oc.SetupProject()
		appProj := oc.Namespace()

		for i, node := range nodes.Items {
			rcName := fmt.Sprintf("logging-centos-logtest-node-%d", i)
			configMapName := fmt.Sprintf("logtest-config-node-%d", i)
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile,
				"-p", fmt.Sprintf("RATE=%.1f", ratePerPodPerMinute),
				"-p", fmt.Sprintf("NUM_LINES=%d", numLinesPerPod),
				"-p", fmt.Sprintf("REPLICAS=%d", podsPerNode),
				"-p", fmt.Sprintf("LINE_LENGTH=%d", logLineLength),
				"-p", fmt.Sprintf("REPLICATIONCONTROLLER=%s", rcName),
				"-p", fmt.Sprintf("CONFIGMAP=%s", configMapName)).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			e2e.Logf("Deployed %d pods on worker node: %s", podsPerNode, node.Name)
		}

		g.By("Wait until all application generator pods are running before log loss measurement")
		const appPodLabelSelector = "run=centos-logtest"
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(_ context.Context) (bool, error) {
			pods, listErr := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: appPodLabelSelector})
			if listErr != nil {
				return false, listErr
			}
			if n := len(pods.Items); n != totalPods {
				e2e.Logf("Waiting for %d pods with label %s, have %d", totalPods, appPodLabelSelector, n)
				return false, nil
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					e2e.Logf("Pod %s is %s, waiting for Running", pod.Name, pod.Status.Phase)
					return false, nil
				}
			}
			return true, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for %d application pods to be running", totalPods)

		// Use the calculated expected logs from pod distribution
		performanceMetrics.LogsSent = totalExpectedLogs

		e2e.Logf("Starting log loss measurement: Expected %d logs over %v from %d pods", totalExpectedLogs, testDuration, totalPods)

		// Record start time for log loss measurement
		measurementStartTime := time.Now()

		g.By("Measure application log performance and count metrics during test period")

		// Wait for the test duration to allow log generation
		time.Sleep(testDuration)
		measurementEndTime := time.Now()

		// Use expected logs from calculation as baseline
		performanceMetrics.LogsSent = totalExpectedLogs

		g.By("Query Loki to count ingested logs within measurement time window")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)

		// Build a window equal to the test duration + buffer for late ingestion.
		testDurationMinutes := int(math.Ceil(testDuration.Minutes())) + 5 // 5m buffer
		timeRangeQuery := fmt.Sprintf(
			`sum(count_over_time({log_type="application",kubernetes_namespace_name="%s"}[%dm]))`,
			appProj, testDurationMinutes,
		)
		e2e.Logf("Loki query: %s", timeRangeQuery)
		appLogs, err := lc.queryRange("application", timeRangeQuery, 1000, measurementStartTime, measurementEndTime, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Use FORWARD direction so the last sample is the newest, with a reasonable limit
		var logsInLoki int64
		e2e.Logf("Loki query response status: %s", appLogs.Status)
		if appLogs.Status == "success" && len(appLogs.Data.Result) > 0 {
			result := appLogs.Data.Result[0]
			e2e.Logf("Samples in result: %d", len(result.Values))
			if n := len(result.Values); n > 0 {
				last := result.Values[n-1]
				if valueSlice, ok := last.([]any); ok && len(valueSlice) >= 2 {
					if countStr, ok := valueSlice[1].(string); ok {
						if count, parseErr := strconv.ParseInt(countStr, 10, 64); parseErr == nil {
							logsInLoki = count
						}
					}
				}
			}
		} else {
			e2e.Logf("Query failed or returned no results. Status: %s", appLogs.Status)
		}

		performanceMetrics.LogsReceived = logsInLoki
		e2e.Logf("Total logs received (window=%dm): %d", testDurationMinutes, logsInLoki)
		e2e.Logf("Loki query results: Logs found: %d, Expected: %d", logsInLoki, performanceMetrics.LogsSent)
		// Calculate log loss percentage
		if performanceMetrics.LogsSent > 0 {
			performanceMetrics.LogLossPercentage = float64(performanceMetrics.LogsSent-performanceMetrics.LogsReceived) / float64(performanceMetrics.LogsSent) * 100
		}

		// Log loss validation
		e2e.Logf("Expected Logs: %d, Received Logs: %d, Log Loss: %.2f%%", performanceMetrics.LogsSent, performanceMetrics.LogsReceived, performanceMetrics.LogLossPercentage)
		o.Expect(performanceMetrics.LogsReceived).Should(o.BeNumerically(">", 0), "LogsReceived should be greater than 0")
		o.Expect(performanceMetrics.LogLossPercentage).Should(o.BeNumerically("<", 10.0), "Log loss percentage should be less than 10% for 1x.small LokiStack")
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Longduration-High-87999-Performance-Vector-LokiStack-Performance test from vector to LokiStack using 1x.small t-shirt size and Otel datamodel with log loss measurement[Serial][Slow]", func() {

		if !validateInfraAndResourcesForLoki(oc, "84Gi", "42") {
			g.Skip("Current platform not supported/resources not available for this test!")
		}

		// Performance test to measure throughput and log loss from vector to LokiStack with 1x.small configuration
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")

		ls := lokiStack{
			name:          "lokistack-performance-87999",
			namespace:     loggingNS,
			tSize:         "1x.small",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-performance-87999",
			storageClass:  sc,
			bucketName:    "logging-loki-performance-" + getRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Patch LokiStack with custom ingestion limits for performance testing")
		patchData := `{
				"spec": {
					"limits": {
						"global": {
							"ingestion": {
								"ingestionBurstSize": 50,
								"ingestionRate": 16,
								"maxGlobalStreamsPerTenant": 5000
							}
						}
					}
				}
			}`
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("lokistack", ls.name, "-n", ls.namespace,
			"-p", patchData, "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("LokiStack patched with ingestionRate: 16, ingestionBurstSize: 50, maxGlobalStreamsPerTenant: 5000")
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Create ClusterLogForwarder for performance testing using Otel datamodel")
		clf := clusterlogforwarder{
			name:                      "clf-performance-87999",
			namespace:                 loggingNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			serviceAccountName:        "logcollector-performance-87999",
			secretName:                "lokistack-performance-87999",
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
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace, "DATAMODEL=Otel", "INPUT_REFS=[\"application\"]")

		var (
			jsonLogFile        = filepath.Join(loggingBaseDir, "generatelog", "logging-performance-app-generator.json")
			targetTotalLogs    = int64(3600000)   // NUM_LINES: 3.6M total logs
			totalRatePerMinute = 120000.0         // RATE: 120K logs/minute total across all pods
			testDuration       = 30 * time.Minute // 30 minutes duration
			logLineLength      = 1500             // LINE_LENGTH: characters per log line
		)

		compat_otp.By("Deploy application pods on all worker nodes")
		// Calculate distribution across pods
		podsPerNode := 8 // 8 pods per worker node for higher log volume
		totalPods := workerNodeCount * podsPerNode
		ratePerPodPerMinute := totalRatePerMinute / float64(totalPods) // Rate per pod per minute
		numLinesPerPod := targetTotalLogs / int64(totalPods)           // Total logs per pod
		totalExpectedLogs := targetTotalLogs
		e2e.Logf("Performance test configuration:")
		e2e.Logf("  - Worker nodes: %d", workerNodeCount)
		e2e.Logf("  - Pods per node: %d", podsPerNode)
		e2e.Logf("  - Total pods: %d", totalPods)
		e2e.Logf("  - Rate per pod: %.1f logs/minute", ratePerPodPerMinute)
		e2e.Logf("  - NUM_LINES per pod: %d", numLinesPerPod)
		e2e.Logf("  - Total rate: %.0f logs/minute", totalRatePerMinute)
		e2e.Logf("  - Target total logs: %d (%.1fM)", targetTotalLogs, float64(targetTotalLogs)/1000000)
		e2e.Logf("  - Test duration: %v", testDuration)
		e2e.Logf("  - Log line length: %d chars", logLineLength)
		e2e.Logf("  - Expected total log size: %.2f GB", float64(targetTotalLogs*int64(logLineLength))/(1024*1024*1024))

		g.By("Create application generator project with pods distributed across all worker nodes")
		oc.SetupProject()
		appProj := oc.Namespace()

		for i, node := range nodes.Items {
			rcName := fmt.Sprintf("logging-centos-logtest-node-%d", i)
			configMapName := fmt.Sprintf("logtest-config-node-%d", i)
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile,
				"-p", fmt.Sprintf("RATE=%.1f", ratePerPodPerMinute),
				"-p", fmt.Sprintf("NUM_LINES=%d", numLinesPerPod),
				"-p", fmt.Sprintf("REPLICAS=%d", podsPerNode),
				"-p", fmt.Sprintf("LINE_LENGTH=%d", logLineLength),
				"-p", fmt.Sprintf("REPLICATIONCONTROLLER=%s", rcName),
				"-p", fmt.Sprintf("CONFIGMAP=%s", configMapName)).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			e2e.Logf("Deployed %d pods on worker node: %s", podsPerNode, node.Name)
		}

		g.By("Wait until all application generator pods are running before log loss measurement")
		const appPodLabelSelector = "run=centos-logtest"
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(_ context.Context) (bool, error) {
			pods, listErr := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: appPodLabelSelector})
			if listErr != nil {
				return false, listErr
			}
			if n := len(pods.Items); n != totalPods {
				e2e.Logf("Waiting for %d pods with label %s, have %d", totalPods, appPodLabelSelector, n)
				return false, nil
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					e2e.Logf("Pod %s is %s, waiting for Running", pod.Name, pod.Status.Phase)
					return false, nil
				}
			}
			return true, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for %d application pods to be running", totalPods)

		// Use the calculated expected logs from pod distribution
		performanceMetrics.LogsSent = totalExpectedLogs
		e2e.Logf("Starting log loss measurement: Expected %d logs over %v from %d pods", totalExpectedLogs, testDuration, totalPods)

		// Record start time for log loss measurement
		measurementStartTime := time.Now()

		g.By("Measure application log performance and count metrics during test period")

		// Wait for the test duration to allow log generation
		time.Sleep(testDuration)
		measurementEndTime := time.Now()

		// Use expected logs from calculation as baseline
		performanceMetrics.LogsSent = totalExpectedLogs

		g.By("Query Loki to count ingested logs within measurement time window")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)

		// Build a window equal to the test duration + buffer for late ingestion.
		testDurationMinutes := int(math.Ceil(testDuration.Minutes())) + 5 // 5m buffer
		timeRangeQuery := fmt.Sprintf(
			`sum(count_over_time({log_type="application",kubernetes_namespace_name="%s"}[%dm]))`,
			appProj, testDurationMinutes,
		)
		e2e.Logf("Loki query: %s", timeRangeQuery)

		// Use FORWARD direction so the last sample is the newest, with a reasonable limit
		appLogs, err := lc.queryRange("application", timeRangeQuery, 1000, measurementStartTime, measurementEndTime, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		var logsInLoki int64
		e2e.Logf("Loki query response status: %s", appLogs.Status)
		if appLogs.Status == "success" && len(appLogs.Data.Result) > 0 {
			// sum(...) should return a single time series
			result := appLogs.Data.Result[0]
			e2e.Logf("Samples in result: %d", len(result.Values))
			if n := len(result.Values); n > 0 {
				last := result.Values[n-1] // newest sample (because FORWARD)
				if valueSlice, ok := last.([]any); ok && len(valueSlice) >= 2 {
					if countStr, ok := valueSlice[1].(string); ok {
						if count, parseErr := strconv.ParseInt(countStr, 10, 64); parseErr == nil {
							logsInLoki = count
						} else {
							e2e.Logf("failed to parse count: %v", parseErr)
						}
					}
				}
			}
		} else {
			e2e.Logf("Query failed or returned no results. Status: %s", appLogs.Status)
		}

		performanceMetrics.LogsReceived = logsInLoki
		e2e.Logf("Total logs received (window=%dm): %d", testDurationMinutes, logsInLoki)

		e2e.Logf("Loki query results:")
		e2e.Logf("  - Logs found in Loki: %d", logsInLoki)
		e2e.Logf("  - Expected logs: %d", performanceMetrics.LogsSent)

		// Calculate log loss percentage
		if performanceMetrics.LogsSent > 0 {
			performanceMetrics.LogLossPercentage = float64(performanceMetrics.LogsSent-performanceMetrics.LogsReceived) / float64(performanceMetrics.LogsSent) * 100
		}

		// Log loss validation
		e2e.Logf("Expected Logs: %d, Received Logs: %d, Log Loss: %.2f%%", performanceMetrics.LogsSent, performanceMetrics.LogsReceived, performanceMetrics.LogLossPercentage)
		o.Expect(performanceMetrics.LogsReceived).Should(o.BeNumerically(">", 0), "LogsReceived should be greater than 0")
		o.Expect(performanceMetrics.LogLossPercentage).Should(o.BeNumerically("<", 10.0), "Log loss percentage should be less than 10%% for 1x.small LokiStack")
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Longduration-High-88000-Performance-Vector-LokiStack-Performance test from vector to LokiStack using 1x.medium t-shirt size and ViaQ datamodel with log loss measurement[Serial][Slow]", func() {

		if !validateInfraAndResourcesForLoki(oc, "172Gi", "70") {
			g.Skip("Current platform not supported/resources not available for this test!")
		}

		// Performance test to measure throughput and log loss from vector to LokiStack with 1x.medium configuration
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")

		ls := lokiStack{
			name:          "lokistack-performance-88000",
			namespace:     loggingNS,
			tSize:         "1x.medium",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-performance-88000",
			storageClass:  sc,
			bucketName:    "logging-loki-performance-" + getRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Patch LokiStack with custom ingestion limits for performance testing")
		patchData := `{
				"spec": {
					"limits": {
						"global": {
							"ingestion": {
								"ingestionBurstSize": 50,
								"ingestionRate": 16,
								"maxGlobalStreamsPerTenant": 5000
							}
						}
					}
				}
			}`
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("lokistack", ls.name, "-n", ls.namespace,
			"-p", patchData, "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("LokiStack patched with ingestionRate: 16, ingestionBurstSize: 50, maxGlobalStreamsPerTenant: 5000")
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Create ClusterLogForwarder for performance testing using ViaQ datamodel")
		clf := clusterlogforwarder{
			name:                      "clf-performance-88000",
			namespace:                 loggingNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			serviceAccountName:        "logcollector-performance-88000",
			secretName:                "lokistack-performance-88000",
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
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace, "INPUT_REFS=[\"application\"]")

		var (
			jsonLogFile        = filepath.Join(loggingBaseDir, "generatelog", "logging-performance-app-generator.json")
			targetTotalLogs    = int64(3600000)   // NUM_LINES: 3.6M total logs
			totalRatePerMinute = 120000.0         // RATE: 120K logs/minute total across all pods
			testDuration       = 30 * time.Minute // 30 minutes duration
			logLineLength      = 2000             // LINE_LENGTH: characters per log line
		)

		compat_otp.By("Deploy application pods on all worker nodes")
		// Calculate distribution across pods
		podsPerNode := 8 // 8 pods per worker node for higher log volume
		totalPods := workerNodeCount * podsPerNode
		ratePerPodPerMinute := totalRatePerMinute / float64(totalPods) // Rate per pod per minute
		numLinesPerPod := targetTotalLogs / int64(totalPods)           // Total logs per pod
		totalExpectedLogs := targetTotalLogs
		e2e.Logf("Performance test configuration:")
		e2e.Logf("  - Worker nodes: %d", workerNodeCount)
		e2e.Logf("  - Pods per node: %d", podsPerNode)
		e2e.Logf("  - Total pods: %d", totalPods)
		e2e.Logf("  - Rate per pod: %.1f logs/minute", ratePerPodPerMinute)
		e2e.Logf("  - NUM_LINES per pod: %d", numLinesPerPod)
		e2e.Logf("  - Total rate: %.0f logs/minute", totalRatePerMinute)
		e2e.Logf("  - Target total logs: %d (%.1fM)", targetTotalLogs, float64(targetTotalLogs)/1000000)
		e2e.Logf("  - Test duration: %v", testDuration)
		e2e.Logf("  - Log line length: %d chars", logLineLength)
		e2e.Logf("  - Expected total log size: %.2f GB", float64(targetTotalLogs*int64(logLineLength))/(1024*1024*1024))

		g.By("Create application generator project with pods distributed across all worker nodes")
		oc.SetupProject()
		appProj := oc.Namespace()

		for i, node := range nodes.Items {
			rcName := fmt.Sprintf("logging-centos-logtest-node-%d", i)
			configMapName := fmt.Sprintf("logtest-config-node-%d", i)
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile,
				"-p", fmt.Sprintf("RATE=%.1f", ratePerPodPerMinute),
				"-p", fmt.Sprintf("NUM_LINES=%d", numLinesPerPod),
				"-p", fmt.Sprintf("REPLICAS=%d", podsPerNode),
				"-p", fmt.Sprintf("LINE_LENGTH=%d", logLineLength),
				"-p", fmt.Sprintf("REPLICATIONCONTROLLER=%s", rcName),
				"-p", fmt.Sprintf("CONFIGMAP=%s", configMapName)).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			e2e.Logf("Deployed %d pods on worker node: %s", podsPerNode, node.Name)
		}

		g.By("Wait until all application generator pods are running before log loss measurement")
		const appPodLabelSelector = "run=centos-logtest"
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(_ context.Context) (bool, error) {
			pods, listErr := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: appPodLabelSelector})
			if listErr != nil {
				return false, listErr
			}
			if n := len(pods.Items); n != totalPods {
				e2e.Logf("Waiting for %d pods with label %s, have %d", totalPods, appPodLabelSelector, n)
				return false, nil
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					e2e.Logf("Pod %s is %s, waiting for Running", pod.Name, pod.Status.Phase)
					return false, nil
				}
			}
			return true, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for %d application pods to be running", totalPods)

		// Use the calculated expected logs from pod distribution
		performanceMetrics.LogsSent = totalExpectedLogs
		e2e.Logf("Starting log loss measurement: Expected %d logs over %v from %d pods", totalExpectedLogs, testDuration, totalPods)

		// Record start time for log loss measurement
		measurementStartTime := time.Now()

		// Wait for the test duration to allow log generation
		g.By("Measure application log performance and count metrics during test period")
		time.Sleep(testDuration)
		measurementEndTime := time.Now()

		// Use expected logs from calculation as baseline
		performanceMetrics.LogsSent = totalExpectedLogs

		g.By("Query Loki to count ingested logs within measurement time window")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)

		// Build a window equal to the test duration + buffer for late ingestion.
		testDurationMinutes := int(math.Ceil(testDuration.Minutes())) + 5 // 5m buffer
		timeRangeQuery := fmt.Sprintf(
			`sum(count_over_time({log_type="application",kubernetes_namespace_name="%s"}[%dm]))`,
			appProj, testDurationMinutes,
		)
		e2e.Logf("Loki query: %s", timeRangeQuery)

		// Use FORWARD direction so the last sample is the newest, with a reasonable limit
		appLogs, err := lc.queryRange("application", timeRangeQuery, 1000, measurementStartTime, measurementEndTime, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		var logsInLoki int64
		e2e.Logf("Loki query response status: %s", appLogs.Status)
		if appLogs.Status == "success" && len(appLogs.Data.Result) > 0 {
			// sum(...) should return a single time series
			result := appLogs.Data.Result[0]
			e2e.Logf("Samples in result: %d", len(result.Values))
			if n := len(result.Values); n > 0 {
				last := result.Values[n-1] // newest sample (because FORWARD)
				if valueSlice, ok := last.([]any); ok && len(valueSlice) >= 2 {
					if countStr, ok := valueSlice[1].(string); ok {
						if count, parseErr := strconv.ParseInt(countStr, 10, 64); parseErr == nil {
							logsInLoki = count
						} else {
							e2e.Logf("failed to parse count: %v", parseErr)
						}
					}
				}
			}
		} else {
			e2e.Logf("Query failed or returned no results. Status: %s", appLogs.Status)
		}

		performanceMetrics.LogsReceived = logsInLoki
		e2e.Logf("Total logs received (window=%dm): %d", testDurationMinutes, logsInLoki)

		e2e.Logf("Loki query results:")
		e2e.Logf("  - Logs found in Loki: %d", logsInLoki)
		e2e.Logf("  - Expected logs: %d", performanceMetrics.LogsSent)

		// Calculate log loss percentage
		if performanceMetrics.LogsSent > 0 {
			performanceMetrics.LogLossPercentage = float64(performanceMetrics.LogsSent-performanceMetrics.LogsReceived) / float64(performanceMetrics.LogsSent) * 100
		}

		// Log loss validation
		e2e.Logf("Expected Logs: %d, Received Logs: %d, Log Loss: %.2f%%", performanceMetrics.LogsSent, performanceMetrics.LogsReceived, performanceMetrics.LogLossPercentage)
		o.Expect(performanceMetrics.LogsReceived).Should(o.BeNumerically(">", 0), "LogsReceived should be greater than 0")
		o.Expect(performanceMetrics.LogLossPercentage).Should(o.BeNumerically("<", 10.0), "Log loss percentage should be less than 10%% for 1x.medium LokiStack")
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:kbharti-Longduration-High-88001-Performance-Vector-LokiStack-Performance test from vector to LokiStack using 1x.medium t-shirt size and Otel datamodel with log loss measurement[Serial][Slow]", func() {

		if !validateInfraAndResourcesForLoki(oc, "172Gi", "70") {
			g.Skip("Current platform not supported/resources not available for this test!")
		}

		// Performance test to measure throughput and log loss from vector to LokiStack with 1x.medium configuration
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")

		ls := lokiStack{
			name:          "lokistack-performance-88001",
			namespace:     loggingNS,
			tSize:         "1x.medium",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-performance-88001",
			storageClass:  sc,
			bucketName:    "logging-loki-performance-" + getRandomString(),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Patch LokiStack with custom ingestion limits for performance testing")
		patchData := `{
				"spec": {
					"limits": {
						"global": {
							"ingestion": {
								"ingestionBurstSize": 50,
								"ingestionRate": 16,
								"maxGlobalStreamsPerTenant": 5000
							}
						}
					}
				}
			}`
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("lokistack", ls.name, "-n", ls.namespace,
			"-p", patchData, "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("LokiStack patched with ingestionRate: 16, ingestionBurstSize: 50, maxGlobalStreamsPerTenant: 5000")
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Create ClusterLogForwarder for performance testing using Otel datamodel")
		clf := clusterlogforwarder{
			name:                      "clf-performance-88001",
			namespace:                 loggingNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			serviceAccountName:        "logcollector-performance-88001",
			secretName:                "lokistack-performance-88001",
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
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace, "DATAMODEL=Otel", "INPUT_REFS=[\"application\"]")

		var (
			jsonLogFile        = filepath.Join(loggingBaseDir, "generatelog", "logging-performance-app-generator.json")
			targetTotalLogs    = int64(3600000)   // NUM_LINES: 3.6M total logs
			totalRatePerMinute = 120000.0         // RATE: 120K logs/minute total across all pods
			testDuration       = 30 * time.Minute // 30 minutes duration
			logLineLength      = 2000             // LINE_LENGTH: characters per log line
		)

		compat_otp.By("Deploy application pods on all worker nodes")
		// Calculate distribution across pods
		podsPerNode := 8 // 8 pods per worker node for higher log volume
		totalPods := workerNodeCount * podsPerNode
		ratePerPodPerMinute := totalRatePerMinute / float64(totalPods) // Rate per pod per minute
		numLinesPerPod := targetTotalLogs / int64(totalPods)           // Total logs per pod
		totalExpectedLogs := targetTotalLogs
		e2e.Logf("Performance test configuration:")
		e2e.Logf("  - Worker nodes: %d", workerNodeCount)
		e2e.Logf("  - Pods per node: %d", podsPerNode)
		e2e.Logf("  - Total pods: %d", totalPods)
		e2e.Logf("  - Rate per pod: %.1f logs/minute", ratePerPodPerMinute)
		e2e.Logf("  - NUM_LINES per pod: %d", numLinesPerPod)
		e2e.Logf("  - Total rate: %.0f logs/minute", totalRatePerMinute)
		e2e.Logf("  - Target total logs: %d (%.1fM)", targetTotalLogs, float64(targetTotalLogs)/1000000)
		e2e.Logf("  - Test duration: %v", testDuration)
		e2e.Logf("  - Log line length: %d chars", logLineLength)
		e2e.Logf("  - Expected total log size: %.2f GB", float64(targetTotalLogs*int64(logLineLength))/(1024*1024*1024))

		g.By("Create application generator project with pods distributed across all worker nodes")
		oc.SetupProject()
		appProj := oc.Namespace()

		for i, node := range nodes.Items {
			rcName := fmt.Sprintf("logging-centos-logtest-node-%d", i)
			configMapName := fmt.Sprintf("logtest-config-node-%d", i)
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile,
				"-p", fmt.Sprintf("RATE=%.1f", ratePerPodPerMinute),
				"-p", fmt.Sprintf("NUM_LINES=%d", numLinesPerPod),
				"-p", fmt.Sprintf("REPLICAS=%d", podsPerNode),
				"-p", fmt.Sprintf("LINE_LENGTH=%d", logLineLength),
				"-p", fmt.Sprintf("REPLICATIONCONTROLLER=%s", rcName),
				"-p", fmt.Sprintf("CONFIGMAP=%s", configMapName)).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			e2e.Logf("Deployed %d pods on worker node: %s", podsPerNode, node.Name)
		}

		g.By("Wait until all application generator pods are running before log loss measurement")
		const appPodLabelSelector = "run=centos-logtest"
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(_ context.Context) (bool, error) {
			pods, listErr := oc.AdminKubeClient().CoreV1().Pods(appProj).List(context.Background(), metav1.ListOptions{LabelSelector: appPodLabelSelector})
			if listErr != nil {
				return false, listErr
			}
			if n := len(pods.Items); n != totalPods {
				e2e.Logf("Waiting for %d pods with label %s, have %d", totalPods, appPodLabelSelector, n)
				return false, nil
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					e2e.Logf("Pod %s is %s, waiting for Running", pod.Name, pod.Status.Phase)
					return false, nil
				}
			}
			return true, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for %d application pods to be running", totalPods)

		// Use the calculated expected logs from pod distribution
		performanceMetrics.LogsSent = totalExpectedLogs
		e2e.Logf("Starting log loss measurement: Expected %d logs over %v from %d pods", totalExpectedLogs, testDuration, totalPods)

		// Record start time for log loss measurement
		measurementStartTime := time.Now()

		// Wait for the test duration to allow log generation
		g.By("Measure application log performance and count metrics during test period")
		time.Sleep(testDuration)
		measurementEndTime := time.Now()

		// Use expected logs from calculation as baseline
		performanceMetrics.LogsSent = totalExpectedLogs

		g.By("Query Loki to count ingested logs within measurement time window")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)

		// Build a window equal to the test duration + buffer for late ingestion.
		testDurationMinutes := int(math.Ceil(testDuration.Minutes())) + 5 // 5m buffer
		timeRangeQuery := fmt.Sprintf(
			`sum(count_over_time({log_type="application",kubernetes_namespace_name="%s"}[%dm]))`,
			appProj, testDurationMinutes,
		)
		e2e.Logf("Loki query: %s", timeRangeQuery)

		// Use FORWARD direction so the last sample is the newest, with a reasonable limit
		appLogs, err := lc.queryRange("application", timeRangeQuery, 1000, measurementStartTime, measurementEndTime, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		var logsInLoki int64
		e2e.Logf("Loki query response status: %s", appLogs.Status)
		if appLogs.Status == "success" && len(appLogs.Data.Result) > 0 {
			// sum(...) should return a single time series
			result := appLogs.Data.Result[0]
			e2e.Logf("Samples in result: %d", len(result.Values))
			if n := len(result.Values); n > 0 {
				last := result.Values[n-1] // newest sample (because FORWARD)
				if valueSlice, ok := last.([]any); ok && len(valueSlice) >= 2 {
					if countStr, ok := valueSlice[1].(string); ok {
						if count, parseErr := strconv.ParseInt(countStr, 10, 64); parseErr == nil {
							logsInLoki = count
						} else {
							e2e.Logf("failed to parse count: %v", parseErr)
						}
					}
				}
			}
		} else {
			e2e.Logf("Query failed or returned no results. Status: %s", appLogs.Status)
		}

		performanceMetrics.LogsReceived = logsInLoki
		e2e.Logf("Total logs received (window=%dm): %d", testDurationMinutes, logsInLoki)

		e2e.Logf("Loki query results:")
		e2e.Logf("  - Logs found in Loki: %d", logsInLoki)
		e2e.Logf("  - Expected logs: %d", performanceMetrics.LogsSent)

		// Calculate log loss percentage
		if performanceMetrics.LogsSent > 0 {
			performanceMetrics.LogLossPercentage = float64(performanceMetrics.LogsSent-performanceMetrics.LogsReceived) / float64(performanceMetrics.LogsSent) * 100
		}

		// Log loss validation
		e2e.Logf("Expected Logs: %d, Received Logs: %d, Log Loss: %.2f%%", performanceMetrics.LogsSent, performanceMetrics.LogsReceived, performanceMetrics.LogLossPercentage)
		o.Expect(performanceMetrics.LogsReceived).Should(o.BeNumerically(">", 0), "LogsReceived should be greater than 0")
		o.Expect(performanceMetrics.LogLossPercentage).Should(o.BeNumerically("<", 10.0), "Log loss percentage should be less than 10%% for 1x.medium LokiStack")
	})
})
