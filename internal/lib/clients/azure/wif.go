package azure

import (
	"context"
	"fmt"
	"os"
	"time"

	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	"github.com/openshift/openshift-logging-e2e-tests/internal/types/observability"
)

// PatchLokiConfigIntoLokiSubscription patches CLIENT_ID, SUBSCRIPTION_ID, TENANT_ID AND REGION into
// the Loki subscription on Azure WIF clusters
func PatchLokiConfigIntoLokiSubscription(oc *exutil.CLI, azureSubscriptionID, identityClientID, region string) {
	patchConfig := `{
		"spec": {
			"config": {
				"env": [
					{
						"name": "CLIENTID",
						"value": "%s"
					},
					{
						"name": "TENANTID",
						"value": "%s"
					},
					{
						"name": "SUBSCRIPTIONID",
						"value": "%s"
					},
					{
						"name": "REGION",
						"value": "%s"
					}
				]
			}
		}
	}`

	err := oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("patch").Args("sub", "loki-operator", "-n", observability.NamespaceLokiOperator, "-p", fmt.Sprintf(patchConfig, identityClientID, os.Getenv("AZURE_TENANT_ID"), azureSubscriptionID, region), "--type=merge").Execute()
	o.Expect(err).NotTo(o.HaveOccurred(), "Patching Loki Operator failed...")
	waitForPodReadyWithLabel(oc, observability.NamespaceLokiOperator, "name=loki-operator-controller-manager")
}

// PerformManagedIdentityAndSecretSetupForWIF performs creation of Managed Identity, Associated
// Federated credentials, Role assignment to the managed identity and object storage creation on Azure
func PerformManagedIdentityAndSecretSetupForWIF(oc *exutil.CLI, lokistackName, lokiStackNS, azureContainerName, lokiStackStorageSecretName string) {
	region, err := ClusterRegion(oc)
	o.Expect(err).NotTo(o.HaveOccurred())
	serviceAccountIssuer, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("authentication.config", "cluster", "-o=jsonpath={.spec.serviceAccountIssuer}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	resourceGroup, err := ClusterResourceGroup(oc)
	o.Expect(err).NotTo(o.HaveOccurred())

	azureSubscriptionID := os.Getenv("AZURE_SUBSCRIPTION_ID")
	cred := NewDefaultAzureCredential()

	identityClientID, identityPrincipalID := CreateManagedIdentity(cred, azureSubscriptionID, lokistackName, resourceGroup, region)
	CreateFederatedCredentialforLoki(cred, azureSubscriptionID, lokistackName, lokistackName, lokiStackNS, "openshift-logging-"+lokistackName, serviceAccountIssuer, resourceGroup)
	CreateFederatedCredentialforLoki(cred, azureSubscriptionID, lokistackName, lokistackName+"-ruler", lokiStackNS, "openshift-logging-"+lokistackName+"-ruler", serviceAccountIssuer, resourceGroup)
	AssignRoleToManagedIdentity(cred, azureSubscriptionID, identityPrincipalID)
	PatchLokiConfigIntoLokiSubscription(oc, azureSubscriptionID, identityClientID, region)
	storageAccountName := CreateStorageAccount(cred, azureSubscriptionID, resourceGroup, region)
	environment, storageAccountURISuffix := StorageAccountEnvAndDomain(oc)
	CreateBlobContainer(cred, storageAccountName, storageAccountURISuffix, azureContainerName)
	err = CreateLokiObjectStorageSecretForWIF(oc, lokiStackNS, lokiStackStorageSecretName, environment, azureContainerName, storageAccountName)
	o.Expect(err).NotTo(o.HaveOccurred())
}

// waitForPodReadyWithLabel waits until all pods matching the label in the namespace are running and ready
func waitForPodReadyWithLabel(oc *exutil.CLI, ns string, label string) {
	var count int
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		pods, err := oc.AdminKubeClient().CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: label})
		if err != nil {
			return false, err
		}
		count = len(pods.Items)
		if count == 0 {
			e2e.Logf("Waiting for pod with label %s to appear\n", label)
			return false, nil
		}
		ready := true
		for _, pod := range pods.Items {
			if pod.Status.Phase != "Running" {
				ready = false
			}
		}
		return ready, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "Pods with label %s are not ready", label)
}
