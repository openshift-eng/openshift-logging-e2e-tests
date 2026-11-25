package azure

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azarm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcloud "github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	azpolicy "github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/monitor/query/azlogs"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/operationalinsights/armoperationalinsights"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// MonitorLog wraps a Log Analytics workspace used to validate logs forwarded to Azure Monitor
type MonitorLog struct {
	azCred            *azidentity.DefaultAzureCredential
	clientOpts        azpolicy.ClientOptions
	CustomerID        string
	Host              string
	location          string
	primaryKey        string
	resourceGroupName string
	secondaryKey      string
	subscriptionID    string
	TPrefixOrName     string // Depend on how we defined the logType in CLF template, it can be the table name or the table name name prefix.
	WorkspaceID       string
	workspaceName     string
}

// GetSourceGroupLocation looks up the location of the resource group the workspace lives in
func (azLog *MonitorLog) GetSourceGroupLocation() error {
	resourceGroupClient, err := armresources.NewResourceGroupsClient(azLog.subscriptionID, azLog.azCred,
		&azarm.ClientOptions{
			ClientOptions: azLog.clientOpts,
		},
	)
	if err != nil {
		return err
	}

	ctx := context.Background()
	resourceGroupGetResponse, err := resourceGroupClient.Get(
		ctx,
		azLog.resourceGroupName,
		nil,
	)
	if err != nil {
		return err
	}
	azLog.location = *resourceGroupGetResponse.Location
	return nil
}

// CreateLogWorkspace creates or updates the Log Analytics workspace
func (azLog *MonitorLog) CreateLogWorkspace() error {
	e2e.Logf("Creating workspace")
	workspacesClient, err := armoperationalinsights.NewWorkspacesClient(azLog.subscriptionID, azLog.azCred,
		&azarm.ClientOptions{
			ClientOptions: azLog.clientOpts,
		},
	)
	if err != nil {
		return err
	}
	ctx := context.Background()
	pollerResp, err := workspacesClient.BeginCreateOrUpdate(
		ctx,
		azLog.resourceGroupName,
		azLog.workspaceName,
		armoperationalinsights.Workspace{
			Location:   &azLog.location,
			Properties: &armoperationalinsights.WorkspaceProperties{},
		},
		nil,
	)
	if err != nil {
		return err
	}
	workspace, err := pollerResp.PollUntilDone(ctx, nil)
	if err != nil {
		return err
	}
	azLog.WorkspaceID = *workspace.ID
	azLog.workspaceName = *workspace.Name
	azLog.CustomerID = *workspace.Properties.CustomerID

	shareKeyClient, err := armoperationalinsights.NewSharedKeysClient(azLog.subscriptionID, azLog.azCred,
		&azarm.ClientOptions{
			ClientOptions: azLog.clientOpts,
		},
	)
	if err != nil {
		return err
	}
	resp, err := shareKeyClient.GetSharedKeys(ctx, azLog.resourceGroupName, azLog.workspaceName, nil)
	if err != nil {
		return err
	}
	azLog.primaryKey = *resp.PrimarySharedKey
	azLog.secondaryKey = *resp.SecondarySharedKey
	return nil
}

// NewAzureLog gets azureMonitoring from Envs and creates the Log Analytics workspace.
func NewAzureLog(oc *exutil.CLI, location, resourceGroupName, workspaceName, tPrefixOrName string) (MonitorLog, error) {
	var (
		azLog MonitorLog
		err   error
	)

	azLog.TPrefixOrName = tPrefixOrName
	azLog.workspaceName = workspaceName
	azLog.resourceGroupName = resourceGroupName
	//  The workspace name must be between 4 and 63 characters.
	//  The workspace name can contain only letters, numbers and '-'. The '-' shouldn't be the first or the last symbol.

	azLog.subscriptionID = os.Getenv("AZURE_SUBSCRIPTION_ID")
	if len(azLog.subscriptionID) == 0 {
		dat, err := oc.AsAdmin().WithoutNamespace().Run("get", "-n", "kube-system", "secret/azure-credentials", "-ojsonpath={.data.azure_subscription_id}").Output()
		if err != nil {
			return azLog, fmt.Errorf("failed to get secret/azure-credentials")
		}
		data, err := base64.StdEncoding.DecodeString(dat)
		if err != nil {
			return azLog, fmt.Errorf("failed to decode subscription_id from secret/azure-credentials")
		}

		azLog.subscriptionID = string(data)
		if len(azLog.subscriptionID) == 0 {
			return azLog, fmt.Errorf("failed as subscriptionID is empty")
		}
	}
	platform := compat_otp.CheckPlatform(oc)
	if platform == "azure" {
		cloudName := CloudName(oc)
		switch cloudName {
		case "azurepubliccloud":
			azLog.clientOpts = azcore.ClientOptions{Cloud: azcloud.AzurePublic}
			azLog.Host = "ods.opinsights.azure.com"
		case "azureusgovernmentcloud":
			azLog.clientOpts = azcore.ClientOptions{Cloud: azcloud.AzureGovernment}
			azLog.Host = "ods.opinsights.azure.us"
		case "azurechinacloud":
			//azLog.clientOpts = azcore.ClientOptions{Cloud: azcloud.AzureChina}
			return azLog, fmt.Errorf("skip on AzureChinaCloud")
		case "azuregermancloud":
			return azLog, fmt.Errorf("skip on AzureGermanCloud")
		case "azurestackcloud":
			return azLog, fmt.Errorf("skip on AzureStackCloud")
		default:
			return azLog, fmt.Errorf("skip on %s", cloudName)
		}
	} else {
		// TODO: get az cloud type from env vars
		azLog.clientOpts = azcore.ClientOptions{Cloud: azcloud.AzurePublic}
		azLog.Host = "ods.opinsights.azure.com"
	}
	azLog.azCred, err = azidentity.NewDefaultAzureCredential(
		&azidentity.DefaultAzureCredentialOptions{ClientOptions: azLog.clientOpts},
	)
	if err != nil {
		return azLog, err
	}

	if location != "" {
		azLog.location = location
	} else {
		err = azLog.GetSourceGroupLocation()
		if err != nil {
			return azLog, err
		}
	}

	err = azLog.CreateLogWorkspace()
	if err != nil {
		return azLog, err
	}
	return azLog, nil
}

// CreateSecret creates a secret for collector pods to forward logs to Log Analytics workspaces.
func (azLog *MonitorLog) CreateSecret(oc *exutil.CLI, name, namespace string) error {
	return oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", name, "-n", namespace, "--from-literal=shared_key="+azLog.primaryKey).Execute()
}

// GetLogByTable queries logs per table in Log Analytics workspaces.
func (azLog *MonitorLog) GetLogByTable(logTable string) ([]azlogs.Row, error) {
	queryString := logTable + "| where TimeGenerated > ago(5m)|top 10 by TimeGenerated"
	e2e.Logf("query %v", queryString)
	var entries []azlogs.Row

	client, err := azlogs.NewClient(azLog.azCred,
		&azlogs.ClientOptions{
			ClientOptions: azLog.clientOpts,
		},
	)
	if err != nil {
		return entries, err
	}

	// https://learn.microsoft.com/en-us/cli/azure/monitor/log-analytics?view=azure-cli-latest
	// https://learn.microsoft.com/en-us/azure/data-explorer/kusto/query/
	err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		res, err1 := client.QueryWorkspace(
			context.TODO(),
			azLog.CustomerID,
			azlogs.QueryBody{
				Query: &queryString,
			},
			nil)
		if err1 != nil {
			e2e.Logf("azlogs QueryWorkspace error: %v. continue", err1)
			return false, nil
		}
		if res.Error != nil {
			e2e.Logf("azlogs QueryWorkspace response error: %v, continue", res.Error)
			return false, nil
		}
		for _, table := range res.Tables {
			entries = append(entries, table.Rows...)
		}
		return len(entries) > 0, nil
	})

	return entries, err
}

// DeleteWorkspace deletes the LogWorkspace
func (azLog *MonitorLog) DeleteWorkspace() error {
	e2e.Logf("Delete workspace %v", azLog.workspaceName)
	ctx := context.Background()
	workspacesClient, err := armoperationalinsights.NewWorkspacesClient(azLog.subscriptionID, azLog.azCred,
		&azarm.ClientOptions{
			ClientOptions: azLog.clientOpts,
		},
	)
	if err != nil {
		return err
	}
	poller, err := workspacesClient.BeginDelete(ctx, azLog.resourceGroupName, azLog.workspaceName, &armoperationalinsights.WorkspacesClientBeginDeleteOptions{Force: new(bool)})
	if err != nil {
		return err
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return err
	}
	return nil
}
