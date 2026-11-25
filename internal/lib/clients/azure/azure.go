package azure

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// NewDefaultAzureCredential creates a new default Azure credential
func NewDefaultAzureCredential() *azidentity.DefaultAzureCredential {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to obtain a credential")
	return cred
}

type azureCredentials struct {
	SubscriptionID string `json:"subscriptionId"`
	ClientID       string `json:"clientId"`
	ClientSecret   string `json:"clientSecret"`
	TenantID       string `json:"tenantId"`
}

// ReadAzureCredentials reads the Azure subscription json file from local disk.
// It also injects the ENV vars needed to perform certain operations on Managed Identities.
func ReadAzureCredentials() bool {
	var (
		azureCredFile string
		azureCred     azureCredentials
	)
	authFile, present := os.LookupEnv("AZURE_AUTH_LOCATION")
	if present {
		azureCredFile = authFile
	} else {
		envDir, present := os.LookupEnv("CLUSTER_PROFILE_DIR")
		if present {
			azureCredFile = filepath.Join(envDir, "osServicePrincipal.json")
		}
	}
	if len(azureCredFile) > 0 {
		fileContent, err := os.ReadFile(azureCredFile)
		if err != nil {
			e2e.Logf("can't read file %s: %v", azureCredFile, err)
			return false
		}
		if err := json.Unmarshal(fileContent, &azureCred); err != nil {
			e2e.Logf("can't parse azure credentials file %s: %v", azureCredFile, err)
			return false
		}
		os.Setenv("AZURE_SUBSCRIPTION_ID", azureCred.SubscriptionID)
		os.Setenv("AZURE_TENANT_ID", azureCred.TenantID)
		os.Setenv("AZURE_CLIENT_ID", azureCred.ClientID)
		os.Setenv("AZURE_CLIENT_SECRET", azureCred.ClientSecret)
		return true
	}
	return false
}

// CloudName checks out the cloudType of this cluster's platform
func CloudName(oc *exutil.CLI) string {
	cloudName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.azure.cloudName}").Output()
	if err == nil && len(cloudName) > 0 {
		return strings.ToLower(cloudName)
	}
	return ""
}

// ClusterResourceGroup returns the resource group of the cluster
func ClusterResourceGroup(oc *exutil.CLI) (string, error) {
	resourceGroup, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructures", "cluster", "-o=jsonpath={.status.platformStatus.azure.resourceGroupName}").Output()
	return resourceGroup, err
}

// ClusterRegion gets the region/location of the cluster running on Azure Cloud
func ClusterRegion(oc *exutil.CLI) (string, error) {
	return oc.AsAdmin().WithoutNamespace().Run("get").Args("node", `-ojsonpath={.items[].metadata.labels.topology\.kubernetes\.io/region}`).Output()
}

// StorageAccountEnvAndDomain returns the Azure environment and storage account URI suffixes
func StorageAccountEnvAndDomain(oc *exutil.CLI) (string, string) {
	// To return account URI suffix and env
	cloudName, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.azure.cloudName}").Output()
	storageAccountURISuffix := ".blob.core.windows.net"
	environment := "AzureGlobal"
	// Currently we don't have template support for STS/WIF on Azure Government
	// The below code should be ok to run when support is added for WIF
	if strings.ToLower(cloudName) == "azureusgovernmentcloud" {
		storageAccountURISuffix = ".blob.core.usgovcloudapi.net"
		environment = "AzureUSGovernment"
	}
	if strings.ToLower(cloudName) == "azurechinacloud" {
		storageAccountURISuffix = ".blob.core.chinacloudapi.cn"
		environment = "AzureChinaCloud"
	}
	if strings.ToLower(cloudName) == "azuregermancloud" {
		environment = "AzureGermanCloud"
		storageAccountURISuffix = ".blob.core.cloudapi.de"
	}
	return environment, storageAccountURISuffix
}

// CreateResourceGroup creates a resource group.
func CreateResourceGroup(resourceGroupName, subscriptionId, location string, credential azcore.TokenCredential) (armresources.ResourceGroupsClientCreateOrUpdateResponse, error) {
	rgClient, _ := armresources.NewResourceGroupsClient(subscriptionId, credential, nil)

	param := armresources.ResourceGroup{
		Location: &location,
	}

	return rgClient.CreateOrUpdate(context.Background(), resourceGroupName, param, nil)
}

// DeleteResourceGroup deletes a resource group.
func DeleteResourceGroup(resourceGroupName, subscriptionId string, credential azcore.TokenCredential) error {
	rgClient, _ := armresources.NewResourceGroupsClient(subscriptionId, credential, nil)

	poller, err := rgClient.BeginDelete(context.Background(), resourceGroupName, nil)
	if err != nil {
		return err
	}
	if _, err := poller.PollUntilDone(context.Background(), nil); err != nil {
		return err
	}
	e2e.Logf("Successfully deleted resource group: %s", resourceGroupName)
	return nil
}
