package azure

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	azRuntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	azto "github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	librandom "github.com/openshift/openshift-logging-e2e-tests/internal/lib/random"
)

// CreateStorageAccount creates an Azure storage account
func CreateStorageAccount(defaultAzureCred *azidentity.DefaultAzureCredential, azureSubscriptionID, resourceGroup, region string) string {
	storageAccountName := "aosqelogging" + librandom.String()
	// Create the storage account
	storageClient, err := armstorage.NewAccountsClient(azureSubscriptionID, defaultAzureCred, nil)
	o.Expect(err).NotTo(o.HaveOccurred())
	result, err := storageClient.BeginCreate(context.Background(), resourceGroup, storageAccountName, armstorage.AccountCreateParameters{
		Location: &region,
		SKU: &armstorage.SKU{
			Name: azto.Ptr(armstorage.SKUNameStandardLRS),
		},
		Kind: azto.Ptr(armstorage.KindStorageV2),
	}, nil)
	o.Expect(err).NotTo(o.HaveOccurred())

	// Poll until the Storage account is ready
	_, err = result.PollUntilDone(context.Background(), &azRuntime.PollUntilDoneOptions{
		Frequency: 10 * time.Second,
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "Storage account is not ready...")
	os.Setenv("LOKI_OBJECT_STORAGE_STORAGE_ACCOUNT", storageAccountName)
	return storageAccountName
}

// CreateBlobContainer creates a blob container under the provided storageAccount
func CreateBlobContainer(defaultAzureCred *azidentity.DefaultAzureCredential, storageAccountName, storageAccountURISuffix, containerName string) {
	blobServiceClient, err := azblob.NewClient(fmt.Sprintf("https://%s%s", storageAccountName, storageAccountURISuffix), defaultAzureCred, nil)
	o.Expect(err).NotTo(o.HaveOccurred())
	_, err = blobServiceClient.CreateContainer(context.Background(), containerName, nil)
	o.Expect(err).NotTo(o.HaveOccurred())
	e2e.Logf("%s container created successfully: ", containerName)
}

// DeleteStorageAccount deletes a storage account in Microsoft Azure
func DeleteStorageAccount(defaultAzureCred *azidentity.DefaultAzureCredential, azureSubscriptionID, resourceGroupName, storageAccountName string) {
	clientFactory, err := armstorage.NewClientFactory(azureSubscriptionID, defaultAzureCred, nil)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to create instance of ClientFactory for storage account deletion")

	_, err = clientFactory.NewAccountsClient().Delete(context.Background(), resourceGroupName, storageAccountName, nil)
	if err != nil {
		e2e.Logf("Error while deleting storage account: %s", err.Error())
	} else {
		e2e.Logf("storage account deleted successfully..")
	}
}

// ValidatesIfLogsArePushedToAzureContainer checks if tenant logs are present under the Azure blob Container.
// Use StorageAccountEnvAndDomain() to get the storage account URI suffix.
// Returns success if any one of the tenants under tenants[] are found.
func ValidatesIfLogsArePushedToAzureContainer(storageAccountURISuffix, storageAccountName, containerName string, tenants []string) {
	cred := NewDefaultAzureCredential()
	// Create a new Blob service client
	serviceClient, err := azblob.NewClient("https://"+storageAccountName+storageAccountURISuffix, cred, nil)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to create service client..")

	// Poll to check log streams are flushed to container referenced under loki object storage secret
	err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		// Create a client to interact with the container and List blobs in the container
		pager := serviceClient.NewListBlobsFlatPager(containerName, nil)
		for pager.More() {
			// advance to the next page
			page, err := pager.NextPage(context.TODO())
			o.Expect(err).NotTo(o.HaveOccurred())

			// check the blob names for this page
			for _, blob := range page.Segment.BlobItems {
				for _, tenantName := range tenants {
					if strings.Contains(*blob.Name, tenantName) {
						e2e.Logf("Logs %s found under the container: %s", *blob.Name, containerName)
						return true, nil
					}
				}
			}
		}
		e2e.Logf("Waiting for data to be available under container: %s", containerName)
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, "Timed out...No data is available under the container: "+containerName)
}

// CreateLokiObjectStorageSecretForWIF creates the Loki object storage secret required on Azure STS/WIF clusters
func CreateLokiObjectStorageSecretForWIF(oc *exutil.CLI, lokiStackNS, objectStorageSecretName, environment, containerName, storageAccountName string) error {
	return oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", "-n", lokiStackNS, objectStorageSecretName, "--from-literal=environment="+environment, "--from-literal=container="+containerName, "--from-literal=account_name="+storageAccountName).Execute()
}
