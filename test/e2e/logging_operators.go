package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"encoding/json"
	"fmt"
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
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease vector-loki Upgrade testing loki-operator", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("logging-loki-upgrade")
		loggingBaseDir string
	)

	g.BeforeEach(func() {
		if len(getStorageType(oc)) == 0 {
			g.Skip("Current cluster doesn't have a proper object storage for this test!")
		}
		if !validateInfraForLoki(oc) {
			g.Skip("Current platform not supported!")
		}

		// skip upgrade cases when operator is installed by operator-sdk
		for _, operator := range []string{"cluster-logging", "loki-operator"} {
			channel, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("sub", "--all-namespaces", `-ojsonpath={.items[?(@.spec.name=="`+operator+`")].spec.channel}`).Output()
			if strings.Contains(channel, "operator-sdk-run-bundle") {
				g.Skip("Skip the case for operator " + operator + " is installed via operator-sdk.")
			}
		}

		var oh OperatorHub
		g.By("check source/redhat-operators status in operatorhub")
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("operatorhub/cluster", "-ojson").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		json.Unmarshal([]byte(output), &oh)
		var disabled bool
		for _, source := range oh.Status.Sources {
			if source.Name == "redhat-operators" {
				disabled = source.Disabled
				break
			}
		}
		if disabled {
			g.Skip("source/redhat-operators is disabled, skip this case.")
		}

		loggingBaseDir = testdata.FixturePath("logging")
		clo := SubscriptionObjects{
			OperatorName: "cluster-logging-operator",
			Namespace:    cloNS,
			PackageName:  "cluster-logging",
		}
		lo := SubscriptionObjects{
			OperatorName: "loki-operator-controller-manager",
			Namespace:    "openshift-operators-redhat",
			PackageName:  "loki-operator",
		}
		g.By("uninstall CLO and LO")
		clo.uninstallOperator(oc)
		lo.uninstallOperator(oc)
		for _, crd := range []string{"alertingrules.loki.grafana.com", "lokistacks.loki.grafana.com", "recordingrules.loki.grafana.com", "rulerconfigs.loki.grafana.com"} {
			_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("crd", crd).Execute()
		}
	})
	/*
		g.AfterEach(func() {
			for _, crd := range []string{"alertingrules.loki.grafana.com", "lokistacks.loki.grafana.com", "recordingrules.loki.grafana.com", "rulerconfigs.loki.grafana.com"} {
				_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("crd", crd).Execute()
			}
		})
	*/

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-Longduration-Critical-53407-Upgrade with Vector as collector - minor version.[Serial][Slow]", func() {
		g.Skip("skip the case for logging 6.6 is not released")
		var targetchannel = "stable-6.6"
		g.By(fmt.Sprintf("Subscribe operators to %s channel", targetchannel))
		source := CatalogSourceObjects{
			Channel:         targetchannel,
			SourceName:      "redhat-operators",
			SourceNamespace: "openshift-marketplace",
		}
		subTemplate := filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml")
		preCLO := SubscriptionObjects{
			OperatorName:  "cluster-logging-operator",
			Namespace:     cloNS,
			PackageName:   "cluster-logging",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			CatalogSource: source,
		}
		preLO := SubscriptionObjects{
			OperatorName:  "loki-operator-controller-manager",
			Namespace:     loNS,
			PackageName:   "loki-operator",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			CatalogSource: source,
		}
		defer preCLO.uninstallOperator(oc)
		preCLO.SubscribeOperator(oc)
		defer preLO.uninstallOperator(oc)
		preLO.SubscribeOperator(oc)

		g.By("Deploy lokistack")
		sc, err := getStorageClassName(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls := lokiStack{
			name:          "loki-53407",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-53407",
			storageClass:  sc,
			bucketName:    "logging-loki-53407-" + getInfrastructureName(oc),
			template:      filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml"),
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
			name:                      "instance",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret",
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			waitForPodReady:           true,
			enableMonitoring:          true,
		}
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer resource{"secret", clf.secretName, clf.namespace}.clear(oc)
		ls.createSecretFromGateway(oc, clf.secretName, clf.namespace, "")
		defer clf.delete(oc)
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace)

		compat_otp.By("deploy logfilesmetricexporter")
		lfme := logFileMetricExporter{
			name:          "instance",
			namespace:     loggingNS,
			template:      filepath.Join(loggingBaseDir, "logfilemetricexporter", "lfme.yaml"),
			waitPodsReady: true,
		}
		defer lfme.delete(oc)
		lfme.create(oc)

		//get current csv version
		preCloCSV := preCLO.getInstalledCSV(oc)
		preLoCSV := preLO.getInstalledCSV(oc)

		// get currentCSV in packagemanifests
		currentCloCSV := getCurrentCSVFromPackage(oc, "cluster-logging-operator-registry", targetchannel, preCLO.PackageName)
		currentLoCSV := getCurrentCSVFromPackage(oc, "loki-operator-registry", targetchannel, preLO.PackageName)
		var upgraded = false
		//change source to ART catsrc if needed, and wait for the new operators to be ready
		if preCloCSV != currentCloCSV {
			g.By(fmt.Sprintf("upgrade CLO to %s", currentCloCSV))
			err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("-n", preCLO.Namespace, "sub/"+preCLO.PackageName, "-p", "{\"spec\": {\"source\": \"cluster-logging-operator-registry\"}}", "--type=merge").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			checkResource(oc, true, true, currentCloCSV, []string{"sub", preCLO.PackageName, "-n", preCLO.Namespace, "-ojsonpath={.status.currentCSV}"})
			WaitForDeploymentPodsToBeReady(oc, preCLO.Namespace, preCLO.OperatorName)
			upgraded = true
		}
		if preLoCSV != currentLoCSV {
			g.By(fmt.Sprintf("upgrade LO to %s", currentLoCSV))
			err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("-n", preLO.Namespace, "sub/"+preLO.PackageName, "-p", "{\"spec\": {\"source\": \"loki-operator-registry\"}}", "--type=merge").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			checkResource(oc, true, true, currentLoCSV, []string{"sub", preLO.PackageName, "-n", preLO.Namespace, "-ojsonpath={.status.currentCSV}"})
			WaitForDeploymentPodsToBeReady(oc, preLO.Namespace, preLO.OperatorName)
			upgraded = true
		}

		if upgraded {
			g.By("waiting for the Loki and Vector pods to be ready after upgrade")
			ls.waitForLokiStackToBeReady(oc)
			clf.waitForCollectorPodsReady(oc)
			WaitForDaemonsetPodsToBeReady(oc, lfme.namespace, "logfilesmetricexporter")
			// In upgrade testing, sometimes a pod may not be ready but the deployment/statefulset might be ready
			// here add a step to check the pods' status
			waitForPodReadyWithLabel(oc, ls.namespace, "app.kubernetes.io/instance="+ls.name)

			g.By("checking if the collector can collect logs after upgrading")
			oc.SetupProject()
			appProj := oc.Namespace()
			defer removeClusterRoleFromServiceAccount(oc, appProj, "default", "cluster-admin")
			addClusterRoleToServiceAccount(oc, appProj, "default", "cluster-admin")
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			bearerToken := getSAToken(oc, "default", appProj)
			route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
			lc := newLokiClient(route).withToken(bearerToken).retry(5)
			err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				res, err := lc.searchByNamespace("application", appProj)
				if err != nil {
					e2e.Logf("\ngot err when getting application logs: %v, continue\n", err)
					return false, nil
				}
				if len(res.Data.Result) > 0 {
					return true, nil
				}
				e2e.Logf("\n len(res.Data.Result) not > 0, continue\n")
				return false, nil
			})
			compat_otp.AssertWaitPollNoErr(err, "application logs are not found")

			compat_otp.By("Check if the cm/grafana-dashboard-cluster-logging is created or not after upgrading")
			resource{"configmap", "grafana-dashboard-cluster-logging", "openshift-config-managed"}.WaitForResourceToAppear(oc)
		}
	})

	// author: qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-Longduration-Critical-53404-Upgrade with Vector as collector - major version LokiStack [Serial][Slow]", func() {
		// for 6.6, test upgrade from 6.5 to 6.6
		preSource := CatalogSourceObjects{"stable-6.5", "redhat-operators", "openshift-marketplace"}
		g.By(fmt.Sprintf("Subscribe operators to %s channel", preSource.Channel))
		subTemplate := filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml")
		preCLO := SubscriptionObjects{
			OperatorName:  "cluster-logging-operator",
			Namespace:     cloNS,
			PackageName:   "cluster-logging",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			CatalogSource: preSource,
		}
		preLO := SubscriptionObjects{
			OperatorName:  "loki-operator-controller-manager",
			Namespace:     loNS,
			PackageName:   "loki-operator",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			CatalogSource: preSource,
		}
		defer preCLO.uninstallOperator(oc)
		preCLO.SubscribeOperator(oc)
		defer preLO.uninstallOperator(oc)
		preLO.SubscribeOperator(oc)

		g.By("Deploy lokistack")
		sc, err := getStorageClassName(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls := lokiStack{
			name:          "loki-53404",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-53404",
			storageClass:  sc,
			bucketName:    "logging-loki-53404-" + getInfrastructureName(oc),
			template:      filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml"),
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
			name:                      "instance",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret",
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			waitForPodReady:           true,
			enableMonitoring:          true,
		}
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer resource{"secret", clf.secretName, clf.namespace}.clear(oc)
		ls.createSecretFromGateway(oc, clf.secretName, clf.namespace, "")
		defer clf.delete(oc)
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace)

		compat_otp.By("Check if the cm/grafana-dashboard-cluster-logging is created or not before upgrading")
		clDashboard := resource{"configmap", "grafana-dashboard-cluster-logging", "openshift-config-managed"}
		clDashboard.WaitForResourceToAppear(oc)

		version := "6.6"
		g.By("upgrade CLO&LO to stable-6.6")
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("-n", preCLO.Namespace, "sub/"+preCLO.PackageName, "-p", "{\"spec\": {\"channel\": \"stable-6.6\", \"source\": \"cluster-logging-operator-registry\", \"sourceNamespace\": \"openshift-marketplace\"}}", "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("-n", preLO.Namespace, "sub/"+preLO.PackageName, "-p", "{\"spec\": {\"channel\": \"stable-6.6\", \"source\": \"loki-operator-registry\", \"sourceNamespace\": \"openshift-marketplace\"}}", "--type=merge").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		checkResource(oc, true, false, version, []string{"sub", preCLO.PackageName, "-n", preCLO.Namespace, "-ojsonpath={.status.currentCSV}"})
		cloCurrentCSV, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("sub", "-n", preCLO.Namespace, preCLO.PackageName, "-ojsonpath={.status.currentCSV}").Output()
		resource{"csv", cloCurrentCSV, preCLO.Namespace}.WaitForResourceToAppear(oc)
		checkResource(oc, true, true, "Succeeded", []string{"csv", cloCurrentCSV, "-n", preCLO.Namespace, "-ojsonpath={.status.phase}"})
		WaitForDeploymentPodsToBeReady(oc, preCLO.Namespace, preCLO.OperatorName)

		checkResource(oc, true, false, version, []string{"sub", preLO.PackageName, "-n", preLO.Namespace, "-ojsonpath={.status.currentCSV}"})
		loCurrentCSV, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("sub", "-n", preLO.Namespace, preLO.PackageName, "-ojsonpath={.status.currentCSV}").Output()
		resource{"csv", loCurrentCSV, preLO.Namespace}.WaitForResourceToAppear(oc)
		checkResource(oc, true, true, "Succeeded", []string{"csv", loCurrentCSV, "-n", preLO.Namespace, "-ojsonpath={.status.phase}"})
		WaitForDeploymentPodsToBeReady(oc, preLO.Namespace, preLO.OperatorName)

		ls.waitForLokiStackToBeReady(oc)
		clf.waitForCollectorPodsReady(oc)
		WaitForDaemonsetPodsToBeReady(oc, lfme.namespace, "logfilesmetricexporter")
		// In upgrade testing, sometimes a pod may not be ready but the deployment/statefulset might be ready
		// here add a step to check the pods' status
		waitForPodReadyWithLabel(oc, ls.namespace, "app.kubernetes.io/instance="+ls.name)

		g.By("checking if the collector can collect logs after upgrading")
		oc.SetupProject()
		appProj := oc.Namespace()
		defer removeClusterRoleFromServiceAccount(oc, appProj, "default", "cluster-admin")
		addClusterRoleToServiceAccount(oc, appProj, "default", "cluster-admin")
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", appProj)
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)
		err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
			res, err := lc.searchByNamespace("application", appProj)
			if err != nil {
				e2e.Logf("\ngot err when getting application logs: %v, continue\n", err)
				return false, nil
			}
			if len(res.Data.Result) > 0 {
				return true, nil
			}
			e2e.Logf("\n len(res.Data.Result) not > 0, continue\n")
			return false, nil
		})
		compat_otp.AssertWaitPollNoErr(err, "application logs are not found")

		// Creating cluster roles to allow read access from LokiStack
		defer deleteLokiClusterRolesForReadAccess(oc)
		createLokiClusterRolesForReadAccess(oc)

		g.By("checking if regular user can view his logs after upgrading")
		err = oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "add-cluster-role-to-user", "cluster-logging-application-view", oc.Username(), "-n", appProj).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		userToken, err := oc.Run("whoami").Args("-t").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		lc0 := newLokiClient(route).withToken(userToken).retry(5)
		err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
			res, err := lc0.searchByNamespace("application", appProj)
			if err != nil {
				e2e.Logf("\ngot err when getting application logs: %v, continue\n", err)
				return false, nil
			}
			if len(res.Data.Result) > 0 {
				return true, nil
			}
			e2e.Logf("\n len(res.Data.Result) not > 0, continue\n")
			return false, nil
		})
		compat_otp.AssertWaitPollNoErr(err, "can't get application logs with normal user")

		compat_otp.By("Check if the cm/grafana-dashboard-cluster-logging is created or not after upgrading")
		clDashboard.WaitForResourceToAppear(oc)
	})
})

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease OperatorDeployment", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("logging-operators")
		loggingBaseDir string
	)

	g.BeforeEach(func() {
		loggingBaseDir = testdata.FixturePath("logging")
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:anli-Low-65518-deploy cluster-logging-operator after Datadog-Agent is deployed [Serial]", func() {

		// skip the case when operator is installed by operator-sdk

		channel, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("sub", "--all-namespaces", `-ojsonpath={.items[?(@.spec.name=="cluster-logging")].spec.channel}`).Output()
		if strings.Contains(channel, "operator-sdk-run-bundle") {
			g.Skip("Skip the case for cluster-logging-operator is installed via operator-sdk.")
		}

		oc.SetupProject()
		datadogNS := oc.Namespace()
		subTemplate := filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml")
		ogPath := filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml")
		podLabel := "app.kubernetes.io/name=datadog-operator"

		g.By("Make the datadog operator ready")
		sourceCert := CatalogSourceObjects{
			Channel:         "stable",
			SourceName:      "certified-operators",
			SourceNamespace: "openshift-marketplace",
		}
		subDog := SubscriptionObjects{
			OperatorName:       "datadog-operator-certified",
			PackageName:        "datadog-operator-certified",
			Namespace:          datadogNS,
			Subscription:       subTemplate,
			OperatorPodLabel:   podLabel,
			OperatorGroup:      ogPath,
			CatalogSource:      sourceCert,
			SkipCaseWhenFailed: true,
		}

		subDog.SubscribeOperator(oc)

		g.By("Delete cluster-logging operator if exist")
		sourceQE := CatalogSourceObjects{
			Channel:         "stable-6.6",
			SourceName:      "cluster-logging-operator-registry",
			SourceNamespace: "openshift-marketplace",
		}
		subCLO := SubscriptionObjects{
			OperatorName:  "cluster-logging-operator",
			Namespace:     cloNS,
			PackageName:   "cluster-logging",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			CatalogSource: sourceQE,
		}
		subCLO.uninstallOperator(oc)

		g.By("deploy cluster-logging operator")
		subCLO.SubscribeOperator(oc)
	})
})

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease multi-mode testing", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("logging-multiple-mode")
		loggingBaseDir string
	)

	g.BeforeEach(func() {
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
		g.By("deploy CLO and LO")
		CLO.SubscribeOperator(oc)
		LO.SubscribeOperator(oc)
		oc.SetupProject()
	})

	// author: qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-Medium-64147-Deploy LFME as an independent pod[Serial]", func() {
		template := filepath.Join(loggingBaseDir, "logfilemetricexporter", "lfme.yaml")
		lfme := logFileMetricExporter{
			name:          "instance",
			namespace:     loggingNS,
			template:      template,
			waitPodsReady: true,
		}
		defer lfme.delete(oc)
		lfme.create(oc)
		token := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")

		g.By("check metrics exposed by logfilemetricexporter")
		checkMetric(oc, token, "{job=\"logfilesmetricexporter\"}", 5)

		sc, err := getStorageClassName(oc)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Deploying LokiStack CR")
		ls := lokiStack{
			name:          "loki-64147",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   getStorageType(oc),
			storageSecret: "storage-64147",
			storageClass:  sc,
			bucketName:    "logging-loki-64147-" + getInfrastructureName(oc),
			template:      filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml"),
		}
		defer ls.removeObjectStorage(oc)
		err = ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)
		e2e.Logf("LokiStack deployed")

		compat_otp.By("create a CLF to test forward to lokistack")
		clf := clusterlogforwarder{
			name:                      "instance",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret",
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

		g.By("Remove clusterlogforwarder")
		clf.delete(oc)

		g.By("Check LFME pods, they should not be removed")
		WaitForDaemonsetPodsToBeReady(oc, lfme.namespace, "logfilesmetricexporter")

		g.By("Remove LFME, the pods should be removed")
		lfme.delete(oc)

		g.By("Create LFME with invalid name")
		lfmeInvalidName := resource{
			kind:      "logfilemetricexporters.logging.openshift.io",
			name:      "test-lfme-64147",
			namespace: loggingNS,
		}
		defer lfmeInvalidName.clear(oc)
		err = lfmeInvalidName.applyFromTemplate(oc, "-f", template, "-p", "NAME="+lfmeInvalidName.name, "-p", "NAMESPACE="+lfmeInvalidName.namespace)
		o.Expect(strings.Contains(err.Error(), "metadata.name: Unsupported value: \""+lfmeInvalidName.name+"\": supported values: \"instance\"")).Should(o.BeTrue())

		g.By("Create LFME with invalid namespace")
		lfmeInvalidNamespace := logFileMetricExporter{
			name:      "instance",
			namespace: oc.Namespace(),
			template:  filepath.Join(loggingBaseDir, "logfilemetricexporter", "lfme.yaml"),
		}
		defer lfmeInvalidNamespace.delete(oc)
		lfmeInvalidNamespace.create(oc)
		checkResource(oc, true, false, "validation failed: Invalid namespace name \""+lfmeInvalidNamespace.namespace+"\", instance must be in \"openshift-logging\" namespace", []string{"lfme/" + lfmeInvalidNamespace.name, "-n", lfmeInvalidNamespace.namespace, "-ojsonpath={.status.conditions[*].message}"})
	})

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-Medium-65407-ClusterLogForwarder validation for the serviceaccount[Slow]", func() {
		clfNS := oc.Namespace()
		compat_otp.By("Deploy ES server")
		ees := externalES{
			namespace:  clfNS,
			version:    "8",
			serverName: "elasticsearch-server",
			loggingNS:  clfNS,
		}
		defer ees.remove(oc)
		ees.deploy(oc)

		logFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		compat_otp.By("create pod to generate logs")
		oc.SetupProject()
		proj := oc.Namespace()
		err := oc.WithoutNamespace().Run("new-app").Args("-n", proj, "-f", logFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		compat_otp.By("Create clusterlogforwarder with a non-existing serviceaccount")
		clf := clusterlogforwarder{
			name:         "collector-65407",
			namespace:    clfNS,
			templateFile: filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch.yaml"),
		}
		defer clf.delete(oc)
		clf.create(oc, "ES_URL=http://"+ees.serverName+"."+ees.namespace+".svc:9200", "ES_VERSION="+ees.version, "SERVICE_ACCOUNT_NAME=logcollector", "INPUT_REFS=[\"application\"]")
		checkResource(oc, true, false, `ServiceAccount "logcollector" not found`, []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})

		ds := resource{
			kind:      "daemonset",
			name:      clf.name,
			namespace: clf.namespace,
		}
		dsErr := ds.WaitUntilResourceIsGone(oc)
		o.Expect(dsErr).NotTo(o.HaveOccurred())

		compat_otp.By("Create the serviceaccount and create rolebinding to bind clusterrole to the serviceaccount")
		sa := resource{
			kind:      "serviceaccount",
			name:      "logcollector",
			namespace: clfNS,
		}
		defer sa.clear(oc)
		err = createServiceAccount(oc, sa.namespace, sa.name)
		o.Expect(err).NotTo(o.HaveOccurred(), "get error when creating serviceaccount "+sa.name)
		defer oc.AsAdmin().WithoutNamespace().Run("policy").Args("remove-role-from-user", "collect-application-logs", fmt.Sprintf("system:serviceaccount:%s:%s", sa.namespace, sa.name), "-n", sa.namespace).Execute()
		err = oc.AsAdmin().WithoutNamespace().Run("policy").Args("add-role-to-user", "collect-application-logs", fmt.Sprintf("system:serviceaccount:%s:%s", sa.namespace, sa.name), "-n", sa.namespace).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		// wait for 2 minutes for CLO to update the status in CLF
		time.Sleep(2 * time.Minute)
		checkResource(oc, true, false, `insufficient permissions on service account, not authorized to collect ["application"] logs`, []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})
		dsErr = ds.WaitUntilResourceIsGone(oc)
		o.Expect(dsErr).NotTo(o.HaveOccurred())

		compat_otp.By("Create clusterrolebinding to bind clusterrole to the serviceaccount")
		defer removeClusterRoleFromServiceAccount(oc, sa.namespace, sa.name, "collect-application-logs")
		addClusterRoleToServiceAccount(oc, sa.namespace, sa.name, "collect-application-logs")
		// wait for 2 minutes for CLO to update the status in CLF
		time.Sleep(2 * time.Minute)
		checkResource(oc, true, false, "True", []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[?(@.reason == \"ValidationSuccess\")].status}"})
		compat_otp.By("Collector pods should be deployed and logs can be forwarded to external log store")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
		ees.waitForIndexAppear(oc, "app")

		compat_otp.By("Delete the serviceaccount, the collector pods should be removed")
		err = sa.clear(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		checkResource(oc, true, false, "ServiceAccount \""+sa.name+"\" not found", []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})
		dsErr = ds.WaitUntilResourceIsGone(oc)
		o.Expect(dsErr).NotTo(o.HaveOccurred())

		compat_otp.By("Recreate the sa and add proper clusterroles to it, the collector pods should be recreated")
		err = createServiceAccount(oc, sa.namespace, sa.name)
		o.Expect(err).NotTo(o.HaveOccurred(), "get error when creating serviceaccount "+sa.name)
		addClusterRoleToServiceAccount(oc, sa.namespace, sa.name, "collect-application-logs")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

		compat_otp.By("Remove spec.serviceAccount from CLF")
		msg, err := clf.patch(oc, `[{"op": "remove", "path": "/spec/serviceAccount"}]`)
		o.Expect(err).To(o.HaveOccurred())
		o.Expect(strings.Contains(msg, "spec.serviceAccount: Required value")).To(o.BeTrue())
	})

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-Medium-65408-ClusterLogForwarder validation when roles don't match.[PRGate][CLO]", func() {
		clfNS := oc.Namespace()
		loki := externalLoki{"loki-server", clfNS}
		defer loki.remove(oc)
		loki.deployLoki(oc)

		compat_otp.By("Create ClusterLogForwarder with a serviceaccount which doesn't have proper clusterroles")
		clf := clusterlogforwarder{
			name:               "collector-65408",
			namespace:          clfNS,
			templateFile:       filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "loki.yaml"),
			serviceAccountName: "clf-collector",
		}
		defer clf.delete(oc)
		clf.create(oc, "URL=http://"+loki.name+"."+loki.namespace+".svc:3100")
		checkResource(oc, true, false, `insufficient permissions on service account, not authorized to collect ["application" "audit" "infrastructure"] logs`, []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})
		ds := resource{
			kind:      "daemonset",
			name:      clf.name,
			namespace: clf.namespace,
		}
		dsErr := ds.WaitUntilResourceIsGone(oc)
		o.Expect(dsErr).NotTo(o.HaveOccurred())

		compat_otp.By("Create a new sa and add clusterrole/collect-application-logs to the new sa, then update the CLF to use the new sa")
		err := oc.AsAdmin().WithoutNamespace().Run("create").Args("sa", "collect-application-logs", "-n", clf.namespace).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, "collect-application-logs", "collect-application-logs")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, "collect-application-logs", "collect-application-logs")
		o.Expect(err).NotTo(o.HaveOccurred())
		clf.update(oc, "", "{\"spec\": {\"serviceAccount\": {\"name\": \"collect-application-logs\"}}}", "--type=merge")
		checkResource(oc, true, false, `insufficient permissions on service account, not authorized to collect ["audit" "infrastructure"] logs`, []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})
		dsErr = ds.WaitUntilResourceIsGone(oc)
		o.Expect(dsErr).NotTo(o.HaveOccurred())

		compat_otp.By("Create a new sa and add clusterrole/collect-infrastructure-logs to the new sa, then update the CLF to use the new sa")
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("sa", "collect-infrastructure-logs", "-n", clf.namespace).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, "collect-infrastructure-logs", "collect-infrastructure-logs")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, "collect-infrastructure-logs", "collect-infrastructure-logs")
		o.Expect(err).NotTo(o.HaveOccurred())
		clf.update(oc, "", "{\"spec\": {\"serviceAccount\": {\"name\": \"collect-infrastructure-logs\"}}}", "--type=merge")
		checkResource(oc, true, false, `insufficient permissions on service account, not authorized to collect ["application" "audit"] logs`, []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})
		dsErr = ds.WaitUntilResourceIsGone(oc)
		o.Expect(dsErr).NotTo(o.HaveOccurred())

		compat_otp.By("Create a new sa and add clusterrole/collect-audit-logs to the new sa, then update the CLF to use the new sa")
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("sa", "collect-audit-logs", "-n", clf.namespace).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, "collect-audit-logs", "collect-audit-logs")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, "collect-audit-logs", "collect-audit-logs")
		o.Expect(err).NotTo(o.HaveOccurred())
		clf.update(oc, "", "{\"spec\": {\"serviceAccount\": {\"name\": \"collect-audit-logs\"}}}", "--type=merge")
		checkResource(oc, true, false, `insufficient permissions on service account, not authorized to collect ["application" "infrastructure"] logs`, []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})
		dsErr = ds.WaitUntilResourceIsGone(oc)
		o.Expect(dsErr).NotTo(o.HaveOccurred())

		compat_otp.By("Create a new sa and add all clusterroles to the new sa, then update the CLF to use the new sa")
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("sa", "collect-all-logs", "-n", clf.namespace).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, logType := range []string{"application", "infrastructure", "audit"} {
			role := "collect-" + logType + "-logs"
			defer removeClusterRoleFromServiceAccount(oc, clf.namespace, "collect-all-logs", role)
			err = addClusterRoleToServiceAccount(oc, clf.namespace, "collect-all-logs", role)
			o.Expect(err).NotTo(o.HaveOccurred())
		}
		clf.update(oc, "", "{\"spec\": {\"serviceAccount\": {\"name\": \"collect-all-logs\"}}}", "--type=merge")
		checkResource(oc, true, false, "True", []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[?(@.reason == \"ValidationSuccess\")].status}"})
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

		compat_otp.By("Remove clusterrole from the serviceaccount, the collector pods should be removed")
		err = removeClusterRoleFromServiceAccount(oc, clf.namespace, "collect-all-logs", "collect-audit-logs")
		o.Expect(err).NotTo(o.HaveOccurred())
		checkResource(oc, true, false, `insufficient permissions on service account, not authorized to collect ["audit"] logs`, []string{"clf/" + clf.name, "-n", clf.namespace, "-ojsonpath={.status.conditions[*].message}"})
		dsErr = ds.WaitUntilResourceIsGone(oc)
		o.Expect(dsErr).NotTo(o.HaveOccurred())
	})

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-65685-Deploy CLO to all namespaces and verify prometheusrule/collector and cm/grafana-dashboard-cluster-logging are created along with the CLO.[PRGate][CLO]", func() {
		csvs, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("csv", "-n", "default", "-oname").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(strings.Contains(csvs, "cluster-logging")).Should(o.BeTrue())

		prometheusrule := resource{
			kind:      "prometheusrule",
			name:      "collector",
			namespace: loggingNS,
		}
		prometheusrule.WaitForResourceToAppear(oc)

		configmap := resource{
			kind:      "configmap",
			name:      "grafana-dashboard-cluster-logging",
			namespace: "openshift-config-managed",
		}
		configmap.WaitForResourceToAppear(oc)
	})

})

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease rapidast scan", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("logging-dast")
		loggingBaseDir string
	)
	g.BeforeEach(func() {
		loggingBaseDir = testdata.FixturePath("logging")
		nodes, err := oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "kubernetes.io/os=linux,kubernetes.io/arch=amd64"})
		if err != nil || len(nodes.Items) == 0 {
			g.Skip("Skip for the cluster doesn't have amd64 node")
		}
	})
	// author anli@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:anli-Critical-75070-clo operator should pass DAST", func() {
		CLO := SubscriptionObjects{
			OperatorName:  "cluster-logging-operator",
			Namespace:     cloNS,
			PackageName:   "cluster-logging",
			Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}
		CLO.SubscribeOperator(oc)
		oc.SetupProject()
		proj := oc.Namespace()
		configFile := filepath.Join(loggingBaseDir, "rapidast/data_rapidastconfig_observability_v1.yaml")
		policyFile := filepath.Join(loggingBaseDir, "rapidast/customscan.policy")
		_, err1 := rapidastScan(oc, proj, configFile, policyFile, "observability.openshift.io_v1")

		configFile = filepath.Join(loggingBaseDir, "rapidast/data_rapidastconfig_logging_v1.yaml")
		_, err2 := rapidastScan(oc, proj, configFile, policyFile, "logging.openshift.io_v1")

		configFile = filepath.Join(loggingBaseDir, "rapidast/data_rapidastconfig_logging_v1alpha1.yaml")
		_, err3 := rapidastScan(oc, proj, configFile, policyFile, "logging.openshift.io_v1alpha1")

		if err1 != nil || err2 != nil || err3 != nil {
			e2e.Failf("rapidast test failed, please check the result for more detail")
		}
	})
	// author anli@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:anli-Critical-67424-loki-operator should pass DAST test", func() {
		LO := SubscriptionObjects{
			OperatorName:  "loki-operator-controller-manager",
			Namespace:     loNS,
			PackageName:   "loki-operator",
			Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}
		LO.SubscribeOperator(oc)
		oc.SetupProject()
		proj := oc.Namespace()
		configFile := filepath.Join(loggingBaseDir, "rapidast/data_rapidastconfig_loki_v1.yaml")
		policyFile := filepath.Join(loggingBaseDir, "rapidast/customscan.policy")
		_, err := rapidastScan(oc, proj, configFile, policyFile, "loki.grafana.com_v1")
		o.Expect(err).NotTo(o.HaveOccurred())
	})
})

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease must-gather", func() {
	defer g.GinkgoRecover()

	var (
		oc = exutil.NewCLIWithoutNamespace("logging-must-gather")
		loggingBaseDir, s, sc string
	)

	g.BeforeEach(func() {
		s = getStorageType(oc)
		if len(s) == 0 {
			g.Skip("Current cluster doesn't have a proper object storage for this test!")
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
		COO := SubscriptionObjects{
			OperatorName:  "cluster-observability-operator",
			Namespace:     "openshift-cluster-observability-operator",
			PackageName:   "cluster-observability-operator",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			CatalogSource: CatalogSourceObjects{
				SourceName:      "redhat-operators",
				SourceNamespace: "openshift-marketplace",
				Channel:         "stable",
			},
			OperatorPodLabel: "app.kubernetes.io/name=observability-operator",
		}
		compat_otp.By("deploy CLO, LO and COO")
		CLO.SubscribeOperator(oc)
		LO.SubscribeOperator(oc)
		COO.SubscribeOperator(oc)
	})

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-75632-oc adm must-gather can collect logging data[Serial]", func() {

		oc.SetupProject()
		clfNS := oc.Namespace()

		compat_otp.By("Create external Elasticsearch")
		esProj := oc.Namespace()
		ees := externalES{
			namespace:  esProj,
			version:    "7",
			serverName: "elasticsearch-server",
			httpSSL:    true,
			userAuth:   true,
			username:   "user1",
			password:   getRandomString(),
			secretName: "ees-75632",
			loggingNS:  clfNS,
		}
		defer ees.remove(oc)
		ees.deploy(oc)

		compat_otp.By("Create CLF to forward to ES")
		clfES := clusterlogforwarder{
			name:                      "clf-75632-es",
			namespace:                 clfNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch-userauth-https.yaml"),
			secretName:                ees.secretName,
			waitForPodReady:           false,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			serviceAccountName:        "test-clf-" + getRandomString(),
		}
		defer clfES.delete(oc)
		clfES.create(oc, "ES_URL=https://"+ees.serverName+"."+esProj+".svc:9200", "ES_VERSION="+ees.version)

		compat_otp.By("deploy lokistack")
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
		ls := lokiStack{
			name:          "loki-75632",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   s,
			storageSecret: "storage-secret-75632",
			storageClass:  sc,
			bucketName:    "logging-loki-75632-" + getInfrastructureName(oc),
			template:      lokiStackTemplate,
		}
		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		//ls.waitForLokiStackToBeReady(oc)
		resource{"configmap", ls.name + "-gateway-ca-bundle", ls.namespace}.WaitForResourceToAppear(oc)

		compat_otp.By("deploy logfilesmetricexporter")
		lfme := logFileMetricExporter{
			name:          "instance",
			namespace:     loggingNS,
			template:      filepath.Join(loggingBaseDir, "logfilemetricexporter", "lfme.yaml"),
			waitPodsReady: false,
		}
		defer lfme.delete(oc)
		lfme.create(oc)

		compat_otp.By("create a CLF to test forward to lokistack")
		clf := clusterlogforwarder{
			name:                      "clf-75632-lokistack",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector-75632",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret-75632",
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			waitForPodReady:           false,
		}
		clf.createServiceAccount(oc)
		defer removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "logging-collector-logs-writer")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer resource{"secret", clf.secretName, clf.namespace}.clear(oc)
		ls.createSecretFromGateway(oc, clf.secretName, clf.namespace, "")
		defer clf.delete(oc)
		clf.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace)

		compat_otp.By("create UIPlugin")
		uiPluginTemplate := filepath.Join(loggingBaseDir, "UIPlugin", "UIPlugin.yaml")
		file, err := processTemplate(oc, "-f", uiPluginTemplate, "-p", "LOKISTACK_NAME="+ls.name)
		defer os.Remove(file)
		compat_otp.AssertWaitPollNoErr(err, "Can not process uiPluginTemplate")
		err = oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", file).Execute()
		defer oc.AsAdmin().WithoutNamespace().Run("delete").Args("UIPlugin", "logging").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		files, err := runLoggingMustGather(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(files) > 0).Should(o.BeTrue())
		for _, expectFile := range []string{
			"cluster-scoped-resources/observability.openshift.io/uiplugins/logging.yaml",
			"cluster-logging/namespaces/" + clfES.namespace + "/" + "configmap_" + clfES.name + "-config_vector.toml",
			"cluster-logging/namespaces/" + clf.namespace + "/" + "configmap_" + clf.name + "-config_vector.toml",
			"namespaces/openshift-logging/logging.openshift.io/logfilemetricexporters/instance.yaml",
			"namespaces/" + ls.namespace + "/loki.grafana.com/lokistacks/" + ls.name + ".yaml",
			"namespaces/" + clf.namespace + "/observability.openshift.io/clusterlogforwarders/" + clf.name + ".yaml",
			"namespaces/" + clfES.namespace + "/observability.openshift.io/clusterlogforwarders/" + clfES.name + ".yaml",
		} {
			o.Expect(containSubstring(files, expectFile)).Should(o.BeTrue(), "can't find file "+expectFile)
		}
	})
})

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease NetworkPolicy", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("logging-np")
		loggingBaseDir, subTemplate string
	)

	g.BeforeEach(func() {
		loggingBaseDir = testdata.FixturePath("logging")
		subTemplate = filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml")
		CLO := SubscriptionObjects{
			OperatorName:  "cluster-logging-operator",
			Namespace:     cloNS,
			PackageName:   "cluster-logging",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}

		g.By("deploy CLO")
		CLO.SubscribeOperator(oc)
		oc.SetupProject()
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-84892-High-84897-NetworkPolicy for LFME and ClusterLogForwarder LokiStack output.[PRGate][CLO][Serial]", func() {
		sc, _ := getStorageClassName(oc)
		if len(sc) == 0 {
			g.Skip("The cluster doesn't have a storage class for this test!")
		}
		LO := SubscriptionObjects{
			OperatorName:  "loki-operator-controller-manager",
			Namespace:     loNS,
			PackageName:   "loki-operator",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}
		LO.SubscribeOperator(oc)
		compat_otp.By("Deploying LokiStack")
		ls := lokiStack{
			name:          "logging-loki",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-84892",
			storageClass:  sc,
			bucketName:    "logging-loki-84892-" + getInfrastructureName(oc),
			template:      filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml"),
		}
		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("Deploy logfilesmetricexporter")
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
			name:                      "clf-ls-84892",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector-84892",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "lokistack.yaml"),
			secretName:                "lokistack-secret-84892",
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

		//check logs in loki stack
		g.By("check logs in loki")
		defer removeClusterRoleFromServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, oc.Namespace(), "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", oc.Namespace())
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)
		for _, logType := range []string{"infrastructure", "audit"} {
			lc.waitForLogsAppearByKey(logType, "log_type", logType)
		}

		g.By("check network policy, no network policy should be created")
		np, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkpolicy", "-n", loggingNS, "-oname").Output()
		o.Expect(np).Should(o.BeEmpty())

		g.By("update LFME and CLF to enable network policy")
		o.Expect(lfme.updateNetworkPolicyRuleSet(oc, "AllowIngressMetrics")).NotTo(o.HaveOccurred())
		o.Expect(clf.updateNetworkPolicyRuleSet(oc, "RestrictIngressEgress")).NotTo(o.HaveOccurred())
		lfme.checkNetworkPolicy(oc)
		clf.checkNetworkPolicy(oc)
		o.Expect(clf.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"8080", "TCP"}}, nil)).NotTo(o.HaveOccurred())
		o.Expect(lfme.validateNetworkPolicy(oc, "AllowIngressMetrics")).NotTo(o.HaveOccurred())

		g.By("sleep 1 minute for network policy to be applied")
		time.Sleep(1 * time.Minute)

		g.By("create an app to generate some logs")
		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("ensure metrics can still be collected after enabling network policy")
		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		g.By("check metrics exposed by collector")
		for _, job := range []string{clf.name, "logfilesmetricexporter"} {
			checkMetric(oc, promToken, "{job=\""+job+"\", namespace=\""+loggingNS+"\"}", 3)
		}
		for _, metric := range []string{`vector_component_received_events_total{job="` + clf.name + `", namespace="` + clf.namespace + `"}`, `log_logged_bytes_total{job="logfilesmetricexporter", namespace="` + lfme.namespace + `"}`} {
			checkMetric(oc, promToken, metric, 3)
		}

		g.By("ensure collector pods still can send logs to lokistack after enabling network policy")
		lc.waitForLogsAppearByProject("application", appProj)

		g.By("update network policy ruleSet for LFME and CLF")
		lfme.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
		clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
		g.By("sleep 10 seconds for CLO to update the networkpolicies")
		time.Sleep(10 * time.Second)
		o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())
		o.Expect(lfme.validateNetworkPolicy(oc, "AllowAllIngressEgress")).NotTo(o.HaveOccurred())

		g.By("disable network policy in LFME and CLF, networkpolicies should be removed")
		lfme.updateNetworkPolicyRuleSet(oc, "")
		clf.updateNetworkPolicyRuleSet(oc, "")
		err = resource{"networkpolicy", "lfme-logfilesmetricexporter", lfme.namespace}.WaitUntilResourceIsGone(oc)
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/lfme-logfilesmetricexporter in project/%s is not deleted", lfme.namespace))
		err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-85402-NetworkPolicy for ClusterLogForwarder OTLP output.[PRGate][CLO]", func() {
		var (
			expectedCSV       string
			operatorInstalled bool
		)
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("csv", "-n", "openshift-operators", "-ojsonpath={.items[*].metadata.name}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		for csv := range strings.SplitSeq(output, " ") {
			if strings.Contains(csv, "opentelemetry-operator.v") {
				expectedCSV = csv
				break
			}
		}
		if len(expectedCSV) > 0 {
			output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("csv", "-n", "openshift-operators", expectedCSV, "-ojsonpath={.status.phase}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			if output == "Succeeded" {
				operatorInstalled = true
			}
		}
		if !operatorInstalled {
			compat_otp.By("Deploy opentelemetry-operator")
			otelOperator := SubscriptionObjects{
				OperatorName:  "opentelemetry-operator",
				Namespace:     "openshift-opentelemetry-operator",
				PackageName:   "opentelemetry-product",
				Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
				OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
				CatalogSource: CatalogSourceObjects{
					Channel:    "stable",
					SourceName: "redhat-operators",
				},
				OperatorPodLabel: "app.kubernetes.io/name=opentelemetry-operator",
			}
			otelOperator.SubscribeOperator(oc)
		}
		g.By("Deploy OTEL collector")
		otelTemplate := filepath.Join(loggingBaseDir, "external-log-stores", "otel", "otel-collector.yaml")
		otel := resource{
			kind:      "opentelemetrycollectors",
			name:      "otel",
			namespace: oc.Namespace(),
		}
		defer otel.clear(oc)
		err = otel.applyFromTemplate(oc, "-f", otelTemplate, "-n", otel.namespace, "-p", "NAMESPACE="+otel.namespace, "-p", "NAME="+otel.name)
		o.Expect(err).NotTo(o.HaveOccurred())
		waitForPodReadyWithLabel(oc, otel.namespace, "app.kubernetes.io/component=opentelemetry-collector")
		svc := "http://" + otel.name + "-collector." + otel.namespace + ".svc:4318"

		g.By("Deploy clusterlogforwarder with network policy enabled")
		clf := clusterlogforwarder{
			name:                      "otlp-85402",
			namespace:                 oc.Namespace(),
			serviceAccountName:        "logcollector-85402",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "otlp.yaml"),
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			enableMonitoring:          true,
		}
		defer clf.delete(oc)
		clf.create(oc, "URL="+svc, `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
		//exclude logs from  project otel.namespace because the OTEL collector writes received logs to stdout
		patch := `[{"op": "add", "path": "/spec/inputs", "value": [{"name": "new-app", "type": "application", "application": {"excludes": [{"namespace":"` + otel.namespace + `"}]}}]},{"op": "replace", "path": "/spec/pipelines/0/inputRefs", "value": ["new-app", "infrastructure", "audit"]}]`
		clf.update(oc, "", patch, "--type=json")
		clf.waitForCollectorPodsReady(oc)
		clf.checkNetworkPolicy(oc)
		o.Expect(clf.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"4318", "TCP"}}, nil)).NotTo(o.HaveOccurred())

		g.By("check log data in OTEL collector")
		time.Sleep(1 * time.Minute)
		otelCollector, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", otel.namespace, "pod", "-l", "app.kubernetes.io/component=opentelemetry-collector", "-ojsonpath={.items[0].metadata.name}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		logs, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("-n", otel.namespace, otelCollector, "--tail=60").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(strings.Contains(logs, "LogRecord")).Should(o.BeTrue())

		g.By("check metrics exposed by collector")
		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
		checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

		g.By("update network policy to AllowAllIngressEgress")
		clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
		g.By("sleep 10 seconds for CLO to update the networkpolicy")
		time.Sleep(10 * time.Second)
		o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

		g.By("disable network policy in CLF, networkpolicy should be removed")
		clf.updateNetworkPolicyRuleSet(oc, "")
		err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
	})

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-85456-NetworkPolicy for ClusterLogForwarder CloudWatch output.", func() {
		platform := compat_otp.CheckPlatform(oc)
		if platform != "aws" {
			g.Skip("Skip for the platform is not AWS.")
		}
		g.By("init Cloudwatch test spec")
		clfNS := oc.Namespace()
		cw := cloudwatchSpec{
			collectorSAName: "cloudwatch-" + getRandomString(),
			secretNamespace: clfNS,
			secretName:      "logging-85456-" + getRandomString(),
			groupName:       "logging-85456-" + getInfrastructureName(oc) + `.{.log_type||"none-typed-logs"}`,
			logTypes:        []string{"infrastructure", "audit"},
		}
		defer cw.deleteResources(oc)
		cw.init(oc)

		g.By("Create clusterlogforwarder")
		var template string
		if cw.stsEnabled {
			template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-iamRole.yaml")
		} else {
			template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "cloudwatch-accessKey.yaml")
		}

		clf := clusterlogforwarder{
			name:                      "clf-85456",
			namespace:                 clfNS,
			templateFile:              template,
			secretName:                cw.secretName,
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			serviceAccountName:        cw.collectorSAName,
			enableMonitoring:          true,
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

		g.By("check network policy, no network policy should be created")
		np, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkpolicy", "-n", clf.namespace, "-oname").Output()
		o.Expect(np).Should(o.BeEmpty())

		g.By("update CLF to enable network policy")
		o.Expect(clf.updateNetworkPolicyRuleSet(oc, "RestrictIngressEgress")).NotTo(o.HaveOccurred())
		clf.checkNetworkPolicy(oc)
		o.Expect(clf.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"443", "TCP"}}, nil)).NotTo(o.HaveOccurred())

		g.By("sleep 1 minute for network policy to be applied")
		time.Sleep(1 * time.Minute)

		g.By("create an app to generate some logs")
		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		cw.selAppNamespaces = append(cw.selAppNamespaces, appProj)

		g.By("check metrics exposed by collector")
		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
		checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

		g.By("ensure vector still can send logs to cloudwatch")
		o.Expect(cw.logsFound()).To(o.BeTrue())

		g.By("update network policy to AllowAllIngressEgress")
		clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
		g.By("sleep 10 seconds for CLO to update the networkpolicy")
		time.Sleep(10 * time.Second)
		o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

		g.By("disable network policy in CLF, networkpolicy should be removed")
		clf.updateNetworkPolicyRuleSet(oc, "")
		err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
	})

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-85744-NetworkPolicy for ClusterLogForwarder s3 output.", func() {
		platform := compat_otp.CheckPlatform(oc)
		if platform != "aws" {
			g.Skip("Skip for the platform is not AWS.")
		}
		g.By("init S3 bucket test spec")
		clfNS := oc.Namespace()
		s3 := S3Output{
			BucketName:              "logging-s3-85744-" + getInfrastructureName(oc),
			KeyPrefix:               `logging-s3-output-{.log_type||"none-typed-logs"}-`,
			SecretName:              "s3-output",
			CollectorNamespace:      clfNS,
			CollectorServiceAccount: "collector-s3-output",
		}
		defer s3.Destroy(oc)
		o.Expect(s3.Init(oc)).NotTo(o.HaveOccurred())

		g.By("Create clusterlogforwarder")
		var template string
		if s3.StsEnabled {
			template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "s3-iamRole.yaml")
		} else {
			template = filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "s3-accessKey.yaml")
		}

		clf := clusterlogforwarder{
			name:                      "clf-85744",
			namespace:                 clfNS,
			templateFile:              template,
			secretName:                s3.SecretName,
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			serviceAccountName:        s3.CollectorServiceAccount,
			enableMonitoring:          true,
		}
		defer clf.delete(oc)
		clf.createServiceAccount(oc)
		clf.create(oc, "REGION="+s3.Region, "KEY_PREFIX="+s3.KeyPrefix, "BUCKET_NAME="+s3.BucketName)

		g.By("Check logs in S3 Bucket")
		validatesIfLogsArePushedToS3Bucket(s3.Client, s3.BucketName, []string{"application", "infrastructure", "audit"})

		g.By("check network policy, no network policy should be created")
		np, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkpolicy", "-n", clf.namespace, "-oname").Output()
		o.Expect(np).Should(o.BeEmpty())

		g.By("update CLF to enable network policy")
		o.Expect(clf.updateNetworkPolicyRuleSet(oc, "RestrictIngressEgress")).NotTo(o.HaveOccurred())
		clf.checkNetworkPolicy(oc)
		o.Expect(clf.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"443", "TCP"}}, nil)).NotTo(o.HaveOccurred())

		g.By("sleep 1 minute for network policy to be applied")
		time.Sleep(1 * time.Minute)

		g.By("create an app to generate some logs")
		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("check metrics exposed by collector")
		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
		checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

		g.By("ensure vector still can send logs to S3 bucket")
		validatesIfLogsArePushedToS3Bucket(s3.Client, s3.BucketName, []string{"application"})

		g.By("update network policy to AllowAllIngressEgress")
		clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
		g.By("sleep 10 seconds for CLO to update the networkpolicy")
		time.Sleep(10 * time.Second)
		o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

		g.By("disable network policy in CLF, networkpolicy should be removed")
		clf.updateNetworkPolicyRuleSet(oc, "")
		err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
	})

	//author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-ConnectedOnly-High-85410-NetworkPolicy for ClusterLogForwarder GCL output.", func() {
		platform := compat_otp.CheckPlatform(oc)
		if platform != "gcp" {
			g.Skip("Skip for the platform is not GCP.")
		}
		projectID, err := getGCPProjectID(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		gcl := googleCloudLogging{
			projectID: projectID,
			logName:   getInfrastructureName(oc) + "-85410",
		}
		defer gcl.removeLogs()

		g.By("Create log producer")
		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		oc.SetupProject()
		clfNS := oc.Namespace()
		gcpSecret := resource{"secret", "gcp-secret-85410", clfNS}
		defer gcpSecret.clear(oc)
		err = createSecretForGCL(oc, gcpSecret.name, gcpSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())

		clf := clusterlogforwarder{
			name:                      "clf-gcl-85410",
			namespace:                 clfNS,
			secretName:                gcpSecret.name,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "googleCloudLogging.yaml"),
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			enableMonitoring:          true,
			serviceAccountName:        "test-clf-" + getRandomString(),
		}
		defer clf.delete(oc)
		clf.create(oc, "ID_TYPE=project", "ID_VALUE="+gcl.projectID, "LOG_ID="+gcl.logName, `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
		clf.checkNetworkPolicy(oc)
		o.Expect(clf.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"443", "TCP"}}, nil)).NotTo(o.HaveOccurred())

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

		g.By("check metrics exposed by collector")
		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
		checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

		g.By("update network policy to AllowAllIngressEgress")
		clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
		g.By("sleep 10 seconds for CLO to update the networkpolicy")
		time.Sleep(10 * time.Second)
		o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

		g.By("disable network policy in CLF, networkpolicy should be removed")
		clf.updateNetworkPolicyRuleSet(oc, "")
		err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
	})

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-85411-NetworkPolicy for ClusterLogForwarder elasticsearch output.", func() {
		compat_otp.By("Create external Elasticsearch instance")
		esProj := oc.Namespace()
		ees := externalES{
			namespace:  esProj,
			version:    "8",
			serverName: "elasticsearch-server",
			httpSSL:    false,
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
			name:                      "clf-es-85411",
			namespace:                 esProj,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "elasticsearch.yaml"),
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			serviceAccountName:        "test-clf-" + getRandomString(),
			waitForPodReady:           true,
			enableMonitoring:          true,
		}
		defer clf.delete(oc)
		clf.create(oc, "ES_URL=http://"+getRouteAddress(oc, ees.namespace, ees.serverName), "ES_VERSION="+ees.version, "INDEX=logging-85411.{.log_type||\"none-typed-logs\"}-write", `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
		clf.checkNetworkPolicy(oc)
		o.Expect(clf.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"80", "TCP"}}, nil)).NotTo(o.HaveOccurred())

		compat_otp.By("Check logs in ES")
		ees.waitForIndexAppear(oc, "logging-85411.application-write")
		ees.waitForIndexAppear(oc, "logging-85411.infrastructure-write")
		ees.waitForIndexAppear(oc, "logging-85411.audit-write")

		g.By("check metrics exposed by collector")
		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
		checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

		g.By("update network policy to AllowAllIngressEgress")
		clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
		g.By("sleep 10 seconds for CLO to update the networkpolicy")
		time.Sleep(10 * time.Second)
		o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

		g.By("disable network policy in CLF, networkpolicy should be removed")
		clf.updateNetworkPolicyRuleSet(oc, "")
		err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
	})

	// author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-85466-High-85490-High-85465-NetworkPolicy for ClusterLogForwarder inputs.receiver.http and http, splunk output[Serial]", func() {
		nodes, err := oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "kubernetes.io/os=linux,kubernetes.io/arch=amd64"})
		if err != nil || len(nodes.Items) == 0 {
			g.Skip("Skip for the cluster doesn't have amd64 node")
		}
		clfNS := oc.Namespace()
		g.By("Deploy splunk server")
		//define splunk deployment
		sp := splunkPodServer{
			namespace: clfNS,
			name:      "splunk-http",
			authType:  "http",
			version:   "9.0",
		}
		sp.init()
		defer sp.destroy(oc)
		sp.deploy(oc)

		g.By("create CLF to forward logs from http receiver to splunk")
		// The secret used in CLF to splunk server
		clfSecret := toSplunkSecret{
			name:      "splunk-secret-85465",
			namespace: clfNS,
			hecToken:  sp.hecToken,
		}
		defer clfSecret.delete(oc)
		clfSecret.create(oc)
		clfHttpserverToSplunk := clusterlogforwarder{
			name:                      "http-to-splunk-85465",
			namespace:                 clfNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "httpserver-to-splunk.yaml"),
			secretName:                clfSecret.name,
			serviceAccountName:        "clf-" + getRandomString(),
			waitForPodReady:           true,
			collectAuditLogs:          false,
			collectApplicationLogs:    false,
			collectInfrastructureLogs: false,
			enableMonitoring:          true,
		}
		defer clfHttpserverToSplunk.delete(oc)
		clfHttpserverToSplunk.create(oc, "URL=http://"+sp.serviceURL+":8088", `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
		clfHttpserverToSplunk.checkNetworkPolicy(oc)
		o.Expect(clfHttpserverToSplunk.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"8088", "TCP"}}, []Port{{"8081", "TCP"}, {"8082", "TCP"}, {"8083", "TCP"}})).NotTo(o.HaveOccurred())

		g.By("create CLF to foward logs to http server")
		httpCLF := clusterlogforwarder{
			name:                      "forward-to-http-85490",
			namespace:                 clfNS,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "http-output-85490.yaml"),
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			waitForPodReady:           true,
			serviceAccountName:        "test-clf-" + getRandomString(),
			enableMonitoring:          true,
		}
		defer httpCLF.delete(oc)
		httpCLF.create(oc, "URL_APP=https://"+clfHttpserverToSplunk.name+"-httpserver1."+clfHttpserverToSplunk.namespace+".svc:8081", "SECRET_APP="+clfHttpserverToSplunk.name+"-httpserver1",
			"URL_INFRA=https://"+clfHttpserverToSplunk.name+"-httpserver2."+clfHttpserverToSplunk.namespace+".svc:8082", "SECRET_INFRA="+clfHttpserverToSplunk.name+"-httpserver2",
			"URL_AUDIT=https://"+clfHttpserverToSplunk.name+"-httpserver3."+clfHttpserverToSplunk.namespace+".svc:8083", "SECRET_AUDIT="+clfHttpserverToSplunk.name+"-httpserver3",
			"TLS_CA_KEY=tls.crt", `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
		httpCLF.checkNetworkPolicy(oc)
		o.Expect(httpCLF.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"8081", "TCP"}, {"8082", "TCP"}, {"8083", "TCP"}}, nil)).NotTo(o.HaveOccurred())

		g.By("check logs in splunk")
		o.Expect(sp.auditLogFound()).To(o.BeTrue())

		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		for _, clf := range []clusterlogforwarder{clfHttpserverToSplunk, httpCLF} {
			g.By("check metrics exposed by collector")
			checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
			checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

			g.By("update network policy to AllowAllIngressEgress")
			clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
			g.By("sleep 10 seconds for CLO to update the networkpolicy")
			time.Sleep(10 * time.Second)
			o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

			g.By("disable network policy in CLF, networkpolicy should be removed")
			clf.updateNetworkPolicyRuleSet(oc, "")
			err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
		}
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-85469-High-85472-NetworkPolicy for ClusterLogForwarder inputs.receiver.syslog and syslog output.[Serial][Slow]", func() {
		sc, _ := getStorageClassName(oc)
		if len(sc) == 0 {
			g.Skip("The cluster doesn't have a storage class for this test!")
		}
		LO := SubscriptionObjects{
			OperatorName:  "loki-operator-controller-manager",
			Namespace:     loNS,
			PackageName:   "loki-operator",
			Subscription:  subTemplate,
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
		}
		LO.SubscribeOperator(oc)
		lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
		ls := lokiStack{
			name:          "logging-loki-85469",
			namespace:     loggingNS,
			tSize:         "1x.demo",
			storageType:   getStorageType(oc),
			storageSecret: "storage-secret-85469",
			storageClass:  sc,
			bucketName:    "logging-loki-85469-" + getInfrastructureName(oc),
			template:      lokiStackTemplate,
		}

		defer ls.removeObjectStorage(oc)
		err := ls.prepareResourcesForLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer ls.removeLokiStack(oc)
		err = ls.deployLokiStack(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		ls.waitForLokiStackToBeReady(oc)

		compat_otp.By("create a CLF to forward logs to lokistack")
		lsCLF := clusterlogforwarder{
			name:                      "lokistack-85469",
			namespace:                 loggingNS,
			serviceAccountName:        "logcollector-85469",
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "71049.yaml"),
			secretName:                "lokistack-secret-85469",
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			waitForPodReady:           true,
		}
		lsCLF.createServiceAccount(oc)
		defer removeClusterRoleFromServiceAccount(oc, lsCLF.namespace, lsCLF.serviceAccountName, "logging-collector-logs-writer")
		err = addClusterRoleToServiceAccount(oc, lsCLF.namespace, lsCLF.serviceAccountName, "logging-collector-logs-writer")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer resource{"secret", lsCLF.secretName, lsCLF.namespace}.clear(oc)
		ls.createSecretFromGateway(oc, lsCLF.secretName, lsCLF.namespace, "")
		defer lsCLF.delete(oc)
		lsCLF.create(oc, "LOKISTACK_NAME="+ls.name, "LOKISTACK_NAMESPACE="+ls.namespace, `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
		lsCLF.checkNetworkPolicy(oc)
		o.Expect(lsCLF.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"8080", "TCP"}}, []Port{{"6514", "TCP"}})).NotTo(o.HaveOccurred())

		g.By("Create clusterlogforwarder as syslog clinet and forward logs to syslogserver")
		sysCLF := clusterlogforwarder{
			name:                      "syslog-85472",
			namespace:                 oc.Namespace(),
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "rsyslog-serverAuth.yaml"),
			secretName:                "clf-syslog-secret",
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			serviceAccountName:        "clf-" + getRandomString(),
			enableMonitoring:          true,
		}
		g.By("Create secret for collector pods to connect to syslog server")
		tmpDir := "/tmp/" + getRandomString()
		defer exec.Command("rm", "-r", tmpDir).Output()
		err = os.Mkdir(tmpDir, 0755)
		o.Expect(err).NotTo(o.HaveOccurred())
		err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("secret/"+lsCLF.name+"-syslog", "-n", lsCLF.namespace, "--confirm", "--to="+tmpDir).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", sysCLF.secretName, "-n", sysCLF.namespace, "--from-file=ca-bundle.crt="+tmpDir+"/tls.crt").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		defer sysCLF.delete(oc)
		sysCLF.create(oc, "URL=tls://"+lsCLF.name+"-syslog."+lsCLF.namespace+".svc:6514", `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
		sysCLF.checkNetworkPolicy(oc)
		o.Expect(sysCLF.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"6514", "TCP"}}, nil)).NotTo(o.HaveOccurred())

		//check logs in loki stack
		g.By("check logs in loki")
		defer removeClusterRoleFromServiceAccount(oc, sysCLF.namespace, "default", "cluster-admin")
		err = addClusterRoleToServiceAccount(oc, sysCLF.namespace, "default", "cluster-admin")
		o.Expect(err).NotTo(o.HaveOccurred())
		bearerToken := getSAToken(oc, "default", sysCLF.namespace)
		route := "https://" + getRouteAddress(oc, ls.namespace, ls.name)
		lc := newLokiClient(route).withToken(bearerToken).retry(5)
		lc.waitForLogsAppearByKey("infrastructure", "log_type", "infrastructure")
		sysLog, err := lc.searchLogsInLoki("infrastructure", `{log_type = "infrastructure"}|json|facility = "local0"`)
		o.Expect(err).NotTo(o.HaveOccurred())
		sysLogs := extractLogEntities(sysLog)
		o.Expect(len(sysLogs) > 0).Should(o.BeTrue(), "can't find logs from syslog in lokistack")

		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		for _, clf := range []clusterlogforwarder{lsCLF, sysCLF} {
			g.By("check metrics exposed by collector")
			checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
			checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

			g.By("update network policy to AllowAllIngressEgress")
			clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
			g.By("sleep 10 seconds for CLO to update the networkpolicy")
			time.Sleep(10 * time.Second)
			o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

			g.By("disable network policy in CLF, networkpolicy should be removed")
			clf.updateNetworkPolicyRuleSet(oc, "")
			err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
		}
	})

	//author qitang@redhat.com
	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-ConnectedOnly-High-85496-NetworkPolicy for ClusterLogForwarder azureMonitor output.", func() {
		platform := compat_otp.CheckPlatform(oc)
		if platform != "azure" {
			g.Skip("Skip for the platform is not Azure.")
		}
		if compat_otp.IsWorkloadIdentityCluster(oc) {
			g.Skip("Skip on the workload identity enabled cluster!")
		}
		var (
			resourceGroupName string
			location          string
		)
		infraName := getInfrastructureName(oc)
		cloudName := getAzureCloudName(oc)
		if !(cloudName == "azurepubliccloud" || cloudName == "azureusgovernmentcloud") {
			g.Skip("The case can only be running on Azure Public and Azure US Goverment now!")
		}
		resourceGroupName, _ = compat_otp.GetAzureCredentialFromCluster(oc)

		g.By("Preprare Azure Log Storage environment")
		workSpaceName := infraName + "case85496"
		azLog, err := newAzureLog(oc, location, resourceGroupName, workSpaceName, "case85496")
		defer azLog.deleteWorkspace()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Create log producer")
		clfNS := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err = oc.WithoutNamespace().Run("new-app").Args("-n", clfNS, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Deploy CLF to send logs to Log Analytics")
		azureSecret := resource{"secret", "azure-secret-85496", clfNS}
		defer azureSecret.clear(oc)
		err = azLog.createSecret(oc, azureSecret.name, azureSecret.namespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		clf := clusterlogforwarder{
			name:                      "clf-85496",
			namespace:                 clfNS,
			secretName:                azureSecret.name,
			templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "azureMonitor.yaml"),
			waitForPodReady:           true,
			collectApplicationLogs:    true,
			collectAuditLogs:          true,
			collectInfrastructureLogs: true,
			enableMonitoring:          true,
			serviceAccountName:        "test-clf-" + getRandomString(),
		}
		defer clf.delete(oc)
		clf.create(oc, "PREFIX_OR_NAME="+azLog.tPrefixOrName, "CUSTOMER_ID="+azLog.customerID, "RESOURCE_ID="+azLog.workspaceID, "AZURE_HOST="+azLog.host, `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
		clf.checkNetworkPolicy(oc)
		o.Expect(clf.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"443", "TCP"}}, nil)).NotTo(o.HaveOccurred())

		g.By("Ensure vector can send logs to azure monitor when the RestrictIngressEgress network policy is enabled")
		for _, tableName := range []string{azLog.tPrefixOrName + "infra_log_CL", azLog.tPrefixOrName + "audit_log_CL", azLog.tPrefixOrName + "app_log_CL"} {
			_, err := azLog.getLogByTable(tableName)
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("can't find logs from %s in AzureLogWorkspace", tableName))
		}

		g.By("check metrics exposed by collector")
		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
		checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

		g.By("update network policy to AllowAllIngressEgress")
		clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
		g.By("sleep 10 seconds for CLO to update the networkpolicy")
		time.Sleep(10 * time.Second)
		o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

		g.By("disable network policy in CLF, networkpolicy should be removed")
		clf.updateNetworkPolicyRuleSet(oc, "")
		err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
	})

	// port=unknown - no data in BigQuery last 60 days
	g.It("Author:qitang-High-85491-NetworkPolicy for ClusterLogForwarder Kafka output.", func() {
		g.By("Create log producer")
		appProj := oc.Namespace()
		jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
		err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		kafkaNS := "openshift-kafka-" + getRandomString()
		defer oc.AsAdmin().WithoutNamespace().Run("delete").Args("project", kafkaNS, "--wait=false").Execute()
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("ns", kafkaNS).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		g.By("Deploy zookeeper")
		kafka := kafka{
			namespace:      kafkaNS,
			kafkasvcName:   "kafka",
			zoosvcName:     "zookeeper",
			authtype:       "sasl-plaintext",
			pipelineSecret: "vector-kafka",
			collectorType:  "vector",
			loggingNS:      appProj,
		}
		defer kafka.removeZookeeper(oc)
		kafka.deployZookeeper(oc)
		g.By("Deploy kafka")
		defer kafka.removeKafka(oc)
		kafka.deployKafka(oc)
		kafkaEndpoint := "tcp://" + kafka.kafkasvcName + "." + kafka.namespace + ".svc.cluster.local:9092/clo-topic"

		g.By("Create clusterlogforwarder")
		clf := clusterlogforwarder{
			name:                   "clf-85491",
			namespace:              appProj,
			templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "clf-kafka-with-auth.yaml"),
			secretName:             kafka.pipelineSecret,
			collectApplicationLogs: true,
			enableMonitoring:       true,
			serviceAccountName:     "clf-" + getRandomString(),
		}
		defer clf.delete(oc)
		clf.create(oc, "URL="+kafkaEndpoint, `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
		// Remove tls configuration from CLF as it is not required for this case
		patch := `[{"op": "remove", "path": "/spec/outputs/0/tls"}]`
		clf.update(oc, "", patch, "--type=json")
		WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

		clf.checkNetworkPolicy(oc)
		o.Expect(clf.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"9092", "TCP"}}, nil)).NotTo(o.HaveOccurred())

		g.By("Check app logs in kafka consumer pod")
		consumerPodPodName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pods", "-n", kafka.namespace, "-l", "component=kafka-consumer", "-o", "name").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
			appLogs, err := getDataFromKafkaByNamespace(oc, kafka.namespace, consumerPodPodName, appProj)
			if err != nil {
				return false, err
			}
			return len(appLogs) > 0, nil
		})
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("App logs are not found in %s/%s", kafka.namespace, consumerPodPodName))

		g.By("check metrics exposed by collector")
		promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
		checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
		checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

		g.By("update network policy to AllowAllIngressEgress")
		clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
		g.By("sleep 10 seconds for CLO to update the networkpolicy")
		time.Sleep(10 * time.Second)
		o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

		g.By("disable network policy in CLF, networkpolicy should be removed")
		clf.updateNetworkPolicyRuleSet(oc, "")
		err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
	})

})
