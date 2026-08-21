package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"os"
	"path/filepath"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("loki-stack")
		loggingBaseDir string
	)

	g.Context("LokiStack testing", func() {
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

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:kbharti-ConnectedOnly-Critical-48607-High-66088-High-64961-Loki Operator - Verify replica support and PodDisruptionBudget 1x.extra-small, 1x.small and 1x.medium t-shirt size[Serial]", func() {
			// This test needs m5.8xlarge (AWS) instance type and similar instance requirement for other public clouds
			objectStorage := getStorageType(oc)
			if len(objectStorage) == 0 {
				g.Skip("Current cluster doesn't have a proper object storage for this test!")
			}
			if !validateInfraAndResourcesForLoki(oc, "150Gi", "64") {
				g.Skip("Current platform not supported/resources not available for this test!")
			}

			sc, err := getStorageClassName(oc)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Deploying LokiStack CR for 1x.extra-small tshirt size")
			lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
			ls := lokiStack{
				name:          "loki-66088",
				namespace:     cloNS,
				tSize:         "1x.extra-small",
				storageType:   objectStorage,
				storageSecret: "storage-secret-66088",
				storageClass:  sc,
				bucketName:    "logging-loki-66088-" + getInfrastructureName(oc),
				template:      lokiStackTemplate,
			}
			defer ls.removeObjectStorage(oc)
			err = ls.prepareResourcesForLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			defer ls.removeLokiStack(oc)
			err = ls.deployLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			ls.waitForLokiStackToBeReady(oc)
			e2e.Logf("LokiStack deployed")

			g.By("Validate component replicas for 1x.extra-small bucket size")
			lokiComponents := []string{"distributor", "gateway", "index-gateway", "query-frontend", "ingester", "querier", "ruler"}
			for _, component := range lokiComponents {
				if component == "gateway" {
					component = "lokistack-gateway"
				}
				replicaCount, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component="+component)
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(len(replicaCount) == 2).Should(o.BeTrue())
			}
			replicacount, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component=compactor")
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(replicacount) == 1).Should(o.BeTrue())

			g.By("Check PodDisruptionBudgets set for 1x.extra-small bucket size")
			for _, component := range lokiComponents {
				minAvailable, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.spec.minAvailable}").Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(minAvailable == "1").Should(o.BeTrue())
				disruptionsAllowed, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.status.disruptionsAllowed}").Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(disruptionsAllowed == "1").Should(o.BeTrue())
			}

			g.By("Deploying LokiStack CR for 1x.small tshirt size")
			ls.removeLokiStack(oc)
			newls := ls.setTSize("1x.small")
			err = newls.deployLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			newls.waitForLokiStackToBeReady(oc)
			e2e.Logf("LokiStack redeployed")

			g.By("Checking Replica count for 1x.small tshirt size")
			replicacount, err = getPodNames(oc, ls.namespace, "app.kubernetes.io/component=compactor")
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(replicacount) == 1).Should(o.BeTrue())
			for _, component := range lokiComponents {
				if component == "gateway" {
					component = "lokistack-gateway"
				}
				replicaCount, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component="+component)
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(len(replicaCount) == 2).Should(o.BeTrue())
			}

			g.By("Check PodDisruptionBudgets set for 1x.small bucket size")
			for _, component := range lokiComponents {
				minAvailable, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.spec.minAvailable}").Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(minAvailable == "1").Should(o.BeTrue())
				disruptionsAllowed, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.status.disruptionsAllowed}").Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(disruptionsAllowed == "1").Should(o.BeTrue())
			}

			g.By("Redeploying LokiStack with 1x.medium tshirt size")
			ls.removeLokiStack(oc)
			newls = ls.setTSize("1x.medium")
			err = newls.deployLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			newls.waitForLokiStackToBeReady(oc)
			e2e.Logf("LokiStack redeployed")

			g.By("Checking Replica replica for 1x.medium tshirt size")
			replicacount, err = getPodNames(oc, ls.namespace, "app.kubernetes.io/component=compactor")
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(replicacount) == 1).Should(o.BeTrue())
			for _, component := range lokiComponents {
				if component == "gateway" {
					component = "lokistack-gateway"
				}
				replicaCount, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component="+component)
				o.Expect(err).NotTo(o.HaveOccurred())
				if component == "ingester" || component == "querier" {
					o.Expect(len(replicaCount) == 3).Should(o.BeTrue())
				} else {
					o.Expect(len(replicaCount) == 2).Should(o.BeTrue())
				}
			}

			g.By("Check PodDisruptionBudgets set for 1x.medium bucket size")
			for _, component := range lokiComponents {
				if component == "ingester" || component == "querier" {
					minAvailable, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.spec.minAvailable}").Output()
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(minAvailable == "2").Should(o.BeTrue())
					disruptionsAllowed, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.status.disruptionsAllowed}").Output()
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(disruptionsAllowed == "1").Should(o.BeTrue())
				} else {
					minAvailable, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.spec.minAvailable}").Output()
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(minAvailable == "1").Should(o.BeTrue())
					disruptionsAllowed, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.status.disruptionsAllowed}").Output()
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(disruptionsAllowed == "1").Should(o.BeTrue())
				}
			}

		})

		//Author: kbharti@redhat.com (GitHub: kabirbhartiRH)
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:kbharti-ConnectedOnly-High-48608-Loki Operator-Reconcile and re-create objects on accidental user deletes[Serial]", func() {
			objectStorage := getStorageType(oc)
			if len(objectStorage) == 0 {
				g.Skip("Current cluster doesn't have a proper object storage for this test!")
			}

			sc, err := getStorageClassName(oc)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Deploying LokiStack CR for 1x.demo tshirt size")
			lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
			ls := lokiStack{
				name:          "loki-48608",
				namespace:     loggingNS,
				tSize:         "1x.demo",
				storageType:   objectStorage,
				storageSecret: "storage-secret-48608",
				storageClass:  sc,
				bucketName:    "logging-loki-48608-" + getInfrastructureName(oc),
				template:      lokiStackTemplate,
			}
			defer ls.removeObjectStorage(oc)
			err = ls.prepareResourcesForLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			defer ls.removeLokiStack(oc)
			err = ls.deployLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			ls.waitForLokiStackToBeReady(oc)
			e2e.Logf("LokiStack deployed")

			e2e.Logf("Getting List of configmaps managed by Loki Controller")
			lokiCMList, err := oc.AdminKubeClient().CoreV1().ConfigMaps(ls.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/created-by=lokistack-controller"})
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(lokiCMList.Items) == 5).Should(o.BeTrue())

			e2e.Logf("Deleting Loki Configmaps")
			for _, items := range lokiCMList.Items {
				err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("cm/"+items.Name, "-n", ls.namespace).Execute()
				o.Expect(err).NotTo(o.HaveOccurred())
			}

			e2e.Logf("Deleting Loki Distributor deployment")
			distributorPods, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component=distributor")
			o.Expect(err).NotTo(o.HaveOccurred())
			err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("deployment/"+ls.name+"-distributor", "-n", ls.namespace).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			for _, pod := range distributorPods {
				er := resource{"pod", pod, ls.namespace}.WaitUntilResourceIsGone(oc)
				o.Expect(er).NotTo(o.HaveOccurred())
			}

			e2e.Logf("Check to see reconciliation of Loki Distributor by Controller....")
			ls.waitForLokiStackToBeReady(oc)
			podList, err := oc.AdminKubeClient().CoreV1().Pods(ls.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=distributor"})
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(podList.Items) == 1).Should(o.BeTrue())
			e2e.Logf("Distributor deployment reconciled!")

			e2e.Logf("Check to see reconciliation of configmaps by Controller....")
			lokiCMList, err = oc.AdminKubeClient().CoreV1().ConfigMaps(ls.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/created-by=lokistack-controller"})
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(lokiCMList.Items) == 5).Should(o.BeTrue())
			e2e.Logf("Loki Configmaps are reconciled \n")

		})
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:kbharti-ConnectedOnly-High-48679-High-48616-Define limits and overrides per tenant for Loki and restart loki components on config change[Serial]", func() {

			objectStorage := getStorageType(oc)
			if len(objectStorage) == 0 {
				g.Skip("Current cluster doesn't have a proper object storage for this test!")
			}

			sc, err := getStorageClassName(oc)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Deploying LokiStack CR for 1x.demo tshirt size")
			lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
			ls := lokiStack{
				name:          "loki-48679",
				namespace:     cloNS,
				tSize:         "1x.demo",
				storageType:   objectStorage,
				storageSecret: "storage-secret-48679",
				storageClass:  sc,
				bucketName:    "logging-loki-48679-" + getInfrastructureName(oc),
				template:      lokiStackTemplate,
			}
			defer ls.removeObjectStorage(oc)
			err = ls.prepareResourcesForLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			defer ls.removeLokiStack(oc)
			err = ls.deployLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			ls.waitForLokiStackToBeReady(oc)
			e2e.Logf("LokiStack deployed")

			// Get names of some lokistack components before patching
			querierPodNameBeforePatch, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component=querier")
			o.Expect(err).NotTo(o.HaveOccurred())
			queryFrontendPodNameBeforePatch, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component=query-frontend")
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Patching lokiStack with limits and overrides")
			patchConfig := `
spec:
  limits:
    tenants:
      application:
        ingestion:
          ingestionRate: 20
          maxLabelNameLength: 2048
          maxLabelValueLength: 1024
      infrastructure:
        ingestion:
          ingestionRate: 15
      audit:
        ingestion:
          ingestionRate: 10
`
			_, err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("lokistack", ls.name, "-n", ls.namespace, "--type", "merge", "-p", patchConfig).Output()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Check lokistack components are restarted")
			existingPodNames := []string{querierPodNameBeforePatch[0], queryFrontendPodNameBeforePatch[0]}
			for _, podName := range existingPodNames {
				err := resource{"pod", podName, ls.namespace}.WaitUntilResourceIsGone(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
			}
			ls.waitForLokiStackToBeReady(oc)

			g.By("Validate limits and overrides per tenant under runtime-config.yaml")
			dirname := "/tmp/" + oc.Namespace() + "-comp-restart"
			defer os.RemoveAll(dirname)
			err = os.MkdirAll(dirname, 0777)
			o.Expect(err).NotTo(o.HaveOccurred())

			_, err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("cm/"+ls.name+"-config", "-n", ls.namespace, "--confirm", "--to="+dirname).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			_, err = os.Stat(dirname + "/runtime-config.yaml")
			o.Expect(err).NotTo(o.HaveOccurred())
			lokiStackConf, err := os.ReadFile(dirname + "/runtime-config.yaml")
			o.Expect(err).NotTo(o.HaveOccurred())

			runtimeConfig := RuntimeConfig{}
			err = yaml.Unmarshal(lokiStackConf, &runtimeConfig)
			o.Expect(err).NotTo(o.HaveOccurred())
			// validating overrides for application tenant
			o.Expect(*runtimeConfig.Overrides.Application.IngestionRateMb).Should(o.Equal(20))
			o.Expect(*runtimeConfig.Overrides.Application.MaxLabelNameLength).Should(o.Equal(2048))
			o.Expect(*runtimeConfig.Overrides.Application.MaxLabelValueLength).Should(o.Equal(1024))
			//validating overrides for infra tenant
			o.Expect(*runtimeConfig.Overrides.Infrastructure.IngestionRateMb).Should(o.Equal(15))
			o.Expect(runtimeConfig.Overrides.Infrastructure.MaxLabelNameLength).To(o.BeNil())
			o.Expect(runtimeConfig.Overrides.Infrastructure.MaxLabelValueLength).To(o.BeNil())
			//validating overrides for audit tenant
			o.Expect(*runtimeConfig.Overrides.Audit.IngestionRateMb).Should(o.Equal(10))
			o.Expect(runtimeConfig.Overrides.Audit.MaxLabelNameLength).To(o.BeNil())
			o.Expect(runtimeConfig.Overrides.Audit.MaxLabelValueLength).To(o.BeNil())
			e2e.Logf("overrides have been validated!")
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-ConnectedOnly-High-76729-LokiStack 1x.pico Support[Serial]", func() {
			if !validateInfraAndResourcesForLoki(oc, "18Gi", "8") {
				g.Skip("Skip this case for the cluster does't have enough resources")
			}
			objectStorage := getStorageType(oc)
			if len(objectStorage) == 0 {
				g.Skip("Current cluster doesn't have a proper object storage for this test!")
			}
			sc, err := getStorageClassName(oc)
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("Deploying LokiStack CR for 1x.pico tshirt size")
			lokiStackTemplate := filepath.Join(loggingBaseDir, "lokistack", "lokistack-simple.yaml")
			ls := lokiStack{
				name:          "loki-76729",
				namespace:     cloNS,
				tSize:         "1x.pico",
				storageType:   objectStorage,
				storageSecret: "storage-secret-76729",
				storageClass:  sc,
				bucketName:    "logging-loki-76729-" + getInfrastructureName(oc),
				template:      lokiStackTemplate,
			}
			defer ls.removeObjectStorage(oc)
			err = ls.prepareResourcesForLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			defer ls.removeLokiStack(oc)
			err = ls.deployLokiStack(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			ls.waitForLokiStackToBeReady(oc)
			e2e.Logf("LokiStack deployed")

			compat_otp.By("Validate component replicas for 1x.pico bucket size")
			lokiComponents := []string{"distributor", "gateway", "index-gateway", "query-frontend", "querier", "ruler"}
			for _, component := range lokiComponents {
				if component == "gateway" {
					component = "lokistack-gateway"
				}
				replicaCount, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component="+component)
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(len(replicaCount) == 2).Should(o.BeTrue())
			}
			replicacount, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component=compactor")
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(replicacount) == 1).Should(o.BeTrue())
			ingesterReplicaCount, err := getPodNames(oc, ls.namespace, "app.kubernetes.io/component=ingester")
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(ingesterReplicaCount) == 3).Should(o.BeTrue())

			compat_otp.By("Check PodDisruptionBudgets set for 1x.pico bucket size")
			for _, component := range lokiComponents {
				maxUnavailable, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.spec.maxUnavailable}").Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(maxUnavailable == "1").Should(o.BeTrue())
				disruptionsAllowed, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-"+component, "-n", cloNS, "-o=jsonpath={.status.disruptionsAllowed}").Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(disruptionsAllowed == "1").Should(o.BeTrue())
			}

			maxUnavailable, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-ingester", "-n", cloNS, "-o=jsonpath={.spec.maxUnavailable}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(maxUnavailable == "1").Should(o.BeTrue())
			disruptionsAllowed, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("PodDisruptionBudget", ls.name+"-ingester", "-n", cloNS, "-o=jsonpath={.status.disruptionsAllowed}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(disruptionsAllowed == "1").Should(o.BeTrue())

		})

	})

})
