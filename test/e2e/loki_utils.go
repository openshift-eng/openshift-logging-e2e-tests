package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/iam/v1"
	"google.golang.org/api/iterator"
	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

func getMinIOCreds(oc *exutil.CLI, ns string) AWSClientConfig {
	dirname := "/tmp/" + oc.Namespace() + "-creds"
	defer os.RemoveAll(dirname)
	err := os.MkdirAll(dirname, 0777)
	o.Expect(err).NotTo(o.HaveOccurred())

	_, err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("secret/"+minioSecret, "-n", ns, "--confirm", "--to="+dirname).Output()
	o.Expect(err).NotTo(o.HaveOccurred())

	accessKeyID, err := os.ReadFile(dirname + "/access_key_id")
	o.Expect(err).NotTo(o.HaveOccurred())
	secretAccessKey, err := os.ReadFile(dirname + "/secret_access_key")
	o.Expect(err).NotTo(o.HaveOccurred())

	endpoint := "https://" + getRouteAddress(oc, ns, "minio")
	return AWSClientConfig{Endpoint: endpoint, AccessKey: string(accessKeyID), SecretKey: string(secretAccessKey), InsecureSkipTLS: true, Region: "auto"}
}

// createSecretForAWSS3Bucket creates a secret for Loki to connect to s3 bucket
func createSecretForAWSS3Bucket(oc *exutil.CLI, bucketName, secretName, ns string, cred AWSClientConfig) error {
	if len(secretName) == 0 {
		return fmt.Errorf("secret name shouldn't be empty")
	}

	err := loadAccessKeySecretKeyFromFile(context.TODO(), &cred)
	if err != nil {
		return fmt.Errorf("error loading aws credentials")
	}

	endpoint := "https://s3." + cred.Region + ".amazonaws.com"
	return oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", secretName, "--from-literal=access_key_id="+cred.AccessKey, "--from-literal=access_key_secret="+cred.SecretKey, "--from-literal=region="+cred.Region, "--from-literal=bucketnames="+bucketName, "--from-literal=endpoint="+endpoint, "-n", ns).Execute()
}

func createSecretForODFBucket(oc *exutil.CLI, bucketName, secretName, ns string) error {
	if len(secretName) == 0 {
		return fmt.Errorf("secret name shouldn't be empty")
	}
	dirname := "/tmp/" + oc.Namespace() + "-creds"
	err := os.MkdirAll(dirname, 0777)
	o.Expect(err).NotTo(o.HaveOccurred())
	defer os.RemoveAll(dirname)
	_, err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("secret/noobaa-admin", "-n", "openshift-storage", "--confirm", "--to="+dirname).Output()
	o.Expect(err).NotTo(o.HaveOccurred())

	endpoint := "http://s3.openshift-storage.svc:80"
	return oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", secretName, "--from-file=access_key_id="+dirname+"/AWS_ACCESS_KEY_ID", "--from-file=access_key_secret="+dirname+"/AWS_SECRET_ACCESS_KEY", "--from-literal=bucketnames="+bucketName, "--from-literal=endpoint="+endpoint, "-n", ns).Execute()
}

func createSecretForMinIOBucket(oc *exutil.CLI, bucketName, secretName, ns string, cred AWSClientConfig) error {
	if len(secretName) == 0 {
		return fmt.Errorf("secret name shouldn't be empty")
	}
	endpoint := "http://minio.minio-aosqe.svc:9000"
	return oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", secretName, "--from-literal=access_key_id="+cred.AccessKey, "--from-literal=access_key_secret="+cred.SecretKey, "--from-literal=bucketnames="+bucketName, "--from-literal=endpoint="+endpoint, "-n", ns).Execute()
}

func getGCPProjectNumber(projectID string) (string, error) {
	crmService, err := cloudresourcemanager.NewService(context.Background())
	if err != nil {
		return "", err
	}

	project, err := crmService.Projects.Get(projectID).Do()
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(project.ProjectNumber, 10), nil
}

func getGCPAudience(providerName string) (string, error) {
	ctx := context.Background()
	service, err := iam.NewService(ctx)

	if err != nil {
		return "", fmt.Errorf("iam.NewService: %w", err)
	}
	audience, err := service.Projects.Locations.WorkloadIdentityPools.Providers.Get(providerName).Do()
	if err != nil {
		return "", fmt.Errorf("can't get audience: %v", err)
	}
	return audience.Oidc.AllowedAudiences[0], nil

}

func generateServiceAccountNameForGCS(clusterName string) string {
	// Service Account should be between 6-30 characters long
	name := clusterName + getRandomString()
	if len(name) > 30 {
		return (name[0:30])
	}
	return name
}

func createServiceAccountOnGCP(projectID, name string) (*iam.ServiceAccount, error) {
	ctx := context.Background()
	service, err := iam.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("iam.NewService: %w", err)
	}

	request := &iam.CreateServiceAccountRequest{
		AccountId: name,
		ServiceAccount: &iam.ServiceAccount{
			DisplayName: "Service Account for " + name,
		},
	}
	account, err := service.Projects.ServiceAccounts.Create("projects/"+projectID, request).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to create serviceaccount: %w", err)
	}
	e2e.Logf("create serviceaccount: %s successfully", account.Name)
	return account, nil
}

// ref: https://github.com/GoogleCloudPlatform/golang-samples/blob/main/iam/quickstart/quickstart.go
func addBinding(projectID, member, role string) error {
	crmService, err := cloudresourcemanager.NewService(context.Background())
	if err != nil {
		return fmt.Errorf("cloudresourcemanager.NewService: %v", err)
	}

	err = wait.ExponentialBackoffWithContext(context.Background(), wait.Backoff{Steps: 5, Factor: 2, Duration: 5 * time.Second}, func(context.Context) (done bool, err error) {
		policy, err := getPolicy(crmService, projectID)
		if err != nil {
			return false, fmt.Errorf("error getting policy: %v", err)
		}
		// Find the policy binding for role. Only one binding can have the role.
		var binding *cloudresourcemanager.Binding
		for _, b := range policy.Bindings {
			if b.Role == role {
				binding = b
				break
			}
		}
		if binding != nil {
			// If the binding exists, adds the member to the binding
			binding.Members = append(binding.Members, member)
		} else {
			// If the binding does not exist, adds a new binding to the policy
			binding = &cloudresourcemanager.Binding{
				Role:    role,
				Members: []string{member},
			}
			policy.Bindings = append(policy.Bindings, binding)
		}
		err = setPolicy(crmService, projectID, policy)
		if err == nil {
			return true, nil
		}
		/*
			According to https://github.com/hashicorp/terraform-provider-google/issues/8280, deleting another serviceaccount can make 400 error happen, so retry this step when 400 error happens
		*/
		if strings.Contains(err.Error(), `googleapi: Error 409: There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.`) ||
			(strings.Contains(err.Error(), "googleapi: Error 400: Service account") && strings.Contains(err.Error(), "does not exist., badRequest")) {
			e2e.Logf("Hit error: %v, retry the request", err)
			return false, nil
		}
		e2e.Logf("Failed to update polilcy: %v", err)
		return false, err
	})
	if err != nil {
		return fmt.Errorf("failed to add role %s to %s", role, member)
	}
	return nil
}

// removeMember removes the member from the project's IAM policy
func removeMember(projectID, member, role string) error {
	crmService, err := cloudresourcemanager.NewService(context.Background())
	if err != nil {
		return fmt.Errorf("cloudresourcemanager.NewService: %v", err)
	}
	err = wait.ExponentialBackoffWithContext(context.Background(), wait.Backoff{Steps: 5, Factor: 2, Duration: 5 * time.Second}, func(context.Context) (done bool, err error) {
		policy, err := getPolicy(crmService, projectID)
		if err != nil {
			return false, fmt.Errorf("error getting policy: %v", err)
		}
		// Find the policy binding for role. Only one binding can have the role.
		var binding *cloudresourcemanager.Binding
		var bindingIndex int
		for i, b := range policy.Bindings {
			if b.Role == role {
				binding = b
				bindingIndex = i
				break
			}
		}

		if len(binding.Members) == 1 && binding.Members[0] == member {
			// If the member is the only member in the binding, removes the binding
			last := len(policy.Bindings) - 1
			policy.Bindings[bindingIndex] = policy.Bindings[last]
			policy.Bindings = policy.Bindings[:last]
		} else {
			// If there is more than one member in the binding, removes the member
			var memberIndex int
			var exist bool
			for i, mm := range binding.Members {
				if mm == member {
					memberIndex = i
					exist = true
					break
				}
			}
			if exist {
				last := len(policy.Bindings[bindingIndex].Members) - 1
				binding.Members[memberIndex] = binding.Members[last]
				binding.Members = binding.Members[:last]
			}
		}

		err = setPolicy(crmService, projectID, policy)
		if err == nil {
			return true, nil
		}
		if strings.Contains(err.Error(), `googleapi: Error 409: There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.`) ||
			(strings.Contains(err.Error(), "googleapi: Error 400: Service account") && strings.Contains(err.Error(), "does not exist., badRequest")) {
			e2e.Logf("Hit error: %v, retry the request", err)
			return false, nil
		}
		e2e.Logf("Failed to update polilcy: %v", err)
		return false, err
	})
	if err != nil {
		return fmt.Errorf("failed to remove %s", member)
	}
	return nil
}

// getPolicy gets the project's IAM policy
func getPolicy(crmService *cloudresourcemanager.Service, projectID string) (*cloudresourcemanager.Policy, error) {
	request := new(cloudresourcemanager.GetIamPolicyRequest)
	policy, err := crmService.Projects.GetIamPolicy(projectID, request).Do()
	if err != nil {
		return nil, err
	}
	return policy, nil
}

// setPolicy sets the project's IAM policy
func setPolicy(crmService *cloudresourcemanager.Service, projectID string, policy *cloudresourcemanager.Policy) error {
	request := new(cloudresourcemanager.SetIamPolicyRequest)
	request.Policy = policy
	_, err := crmService.Projects.SetIamPolicy(projectID, request).Do()
	return err
}

func grantPermissionsToGCPServiceAccount(poolID, projectID, projectNumber, lokiNS, lokiStackName, serviceAccountEmail string) error {
	gcsRoles := []string{
		"roles/iam.workloadIdentityUser",
		"roles/storage.objectAdmin",
	}
	subjects := []string{
		"system:serviceaccount:" + lokiNS + ":" + lokiStackName,
		"system:serviceaccount:" + lokiNS + ":" + lokiStackName + "-ruler",
	}

	for _, role := range gcsRoles {
		err := addBinding(projectID, "serviceAccount:"+serviceAccountEmail, role)
		if err != nil {
			return fmt.Errorf("error adding role %s to %s: %v", role, serviceAccountEmail, err)
		}
		for _, sub := range subjects {
			err := addBinding(projectID, "principal://iam.googleapis.com/projects/"+projectNumber+"/locations/global/workloadIdentityPools/"+poolID+"/subject/"+sub, role)
			if err != nil {
				return fmt.Errorf("error adding role %s to %s: %v", role, sub, err)
			}
		}
	}
	return nil
}

func removePermissionsFromGCPServiceAccount(poolID, projectID, projectNumber, lokiNS, lokiStackName, serviceAccountEmail string) error {
	gcsRoles := []string{
		"roles/iam.workloadIdentityUser",
		"roles/storage.objectAdmin",
	}
	subjects := []string{
		"system:serviceaccount:" + lokiNS + ":" + lokiStackName,
		"system:serviceaccount:" + lokiNS + ":" + lokiStackName + "-ruler",
	}

	for _, role := range gcsRoles {
		err := removeMember(projectID, "serviceAccount:"+serviceAccountEmail, role)
		if err != nil {
			return fmt.Errorf("error removing role %s from %s: %v", role, serviceAccountEmail, err)
		}
		for _, sub := range subjects {
			err := removeMember(projectID, "principal://iam.googleapis.com/projects/"+projectNumber+"/locations/global/workloadIdentityPools/"+poolID+"/subject/"+sub, role)
			if err != nil {
				return fmt.Errorf("error removing role %s from %s: %v", role, sub, err)
			}
		}
	}
	return nil
}

func removeServiceAccountFromGCP(name string) error {
	ctx := context.Background()
	service, err := iam.NewService(ctx)
	if err != nil {
		return fmt.Errorf("iam.NewService: %w", err)
	}
	_, err = service.Projects.ServiceAccounts.Delete(name).Do()
	if err != nil {
		return fmt.Errorf("can't remove service account: %v", err)
	}
	return nil
}

func createSecretForGCSBucketWithSTS(oc *exutil.CLI, namespace, secretName, bucketName string) error {
	return oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", "-n", namespace, secretName, "--from-literal=bucketname="+bucketName).Execute()
}

// creates a secret for Loki to connect to gcs bucket
func createSecretForGCSBucket(oc *exutil.CLI, bucketName, secretName, ns string) error {
	if len(secretName) == 0 {
		return fmt.Errorf("secret name shouldn't be empty")
	}

	//get gcp-credentials from env var GOOGLE_APPLICATION_CREDENTIALS
	gcsCred := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	return oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", secretName, "-n", ns, "--from-literal=bucketname="+bucketName, "--from-file=key.json="+gcsCred).Execute()
}

// creates a secret for Loki to connect to azure container
func createSecretForAzureContainer(oc *exutil.CLI, bucketName, secretName, ns string) error {
	environment := "AzureGlobal"
	cloudName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.azure.cloudName}").Output()
	if err != nil {
		return fmt.Errorf("can't get azure cluster type  %v", err)
	}
	if strings.ToLower(cloudName) == "azureusgovernmentcloud" {
		environment = "AzureUSGovernment"
	}
	if strings.ToLower(cloudName) == "azurechinacloud" {
		environment = "AzureChinaCloud"
	}
	if strings.ToLower(cloudName) == "azuregermancloud" {
		environment = "AzureGermanCloud"
	}

	accountName, accountKey, err1 := compat_otp.GetAzureStorageAccountFromCluster(oc)
	if err1 != nil {
		return fmt.Errorf("can't get azure storage account from cluster: %v", err1)
	}
	return oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", "-n", ns, secretName, "--from-literal=environment="+environment, "--from-literal=container="+bucketName, "--from-literal=account_name="+accountName, "--from-literal=account_key="+accountKey).Execute()
}

func createSecretForSwiftContainer(oc *exutil.CLI, containerName, secretName, ns string, cred *compat_otp.OpenstackCredentials) error {
	userID, domainID := compat_otp.GetOpenStackUserIDAndDomainID(cred)
	err := oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", "-n", ns, secretName,
		"--from-literal=auth_url="+cred.Clouds.Openstack.Auth.AuthURL,
		"--from-literal=username="+cred.Clouds.Openstack.Auth.Username,
		"--from-literal=user_domain_name="+cred.Clouds.Openstack.Auth.UserDomainName,
		"--from-literal=user_domain_id="+domainID,
		"--from-literal=user_id="+userID,
		"--from-literal=password="+cred.Clouds.Openstack.Auth.Password,
		"--from-literal=domain_id="+domainID,
		"--from-literal=domain_name="+cred.Clouds.Openstack.Auth.UserDomainName,
		"--from-literal=container_name="+containerName,
		"--from-literal=project_id="+cred.Clouds.Openstack.Auth.ProjectID,
		"--from-literal=project_name="+cred.Clouds.Openstack.Auth.ProjectName,
		"--from-literal=project_domain_id="+domainID,
		"--from-literal=project_domain_name="+cred.Clouds.Openstack.Auth.UserDomainName).Execute()
	return err
}

// checkODF check if the ODF is installed in the cluster or not
// here only checks the sc/ocs-storagecluster-ceph-rbd and svc/s3
func checkODF(oc *exutil.CLI) bool {
	svcFound := false
	expectedSC := []string{"openshift-storage.noobaa.io"}
	var scInCluster []string
	scs, err := oc.AdminKubeClient().StorageV1().StorageClasses().List(context.Background(), metav1.ListOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())

	for _, sc := range scs.Items {
		scInCluster = append(scInCluster, sc.Name)
	}

	for _, s := range expectedSC {
		if !contain(scInCluster, s) {
			return false
		}
	}

	_, err = oc.AdminKubeClient().CoreV1().Services("openshift-storage").Get(context.Background(), "s3", metav1.GetOptions{})
	if err == nil {
		svcFound = true
	}
	return svcFound
}

func createObjectBucketClaim(oc *exutil.CLI, ns, name string) error {
	template := testdata.FixturePath("logging", "odf", "objectBucketClaim.yaml")
	obc := resource{"objectbucketclaims", name, ns}

	err := obc.applyFromTemplate(oc, "-f", template, "-n", ns, "-p", "NAME="+name, "NAMESPACE="+ns)
	if err != nil {
		return err
	}
	obc.WaitForResourceToAppear(oc)
	resource{"objectbuckets", "obc-" + ns + "-" + name, ns}.WaitForResourceToAppear(oc)
	assertResourceStatus(oc, "objectbucketclaims", name, ns, "{.status.phase}", "Bound")
	return nil
}

func deleteObjectBucketClaim(oc *exutil.CLI, ns, name string) error {
	obc := resource{"objectbucketclaims", name, ns}
	err := obc.clear(oc)
	if err != nil {
		return err
	}
	return obc.WaitUntilResourceIsGone(oc)
}

// checkMinIO
func checkMinIO(oc *exutil.CLI, ns string) (bool, error) {
	podReady, svcFound := false, false
	pod, err := oc.AdminKubeClient().CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "app=minio"})
	if err != nil {
		return false, err
	}
	if len(pod.Items) > 0 && pod.Items[0].Status.Phase == "Running" {
		podReady = true
	}
	_, err = oc.AdminKubeClient().CoreV1().Services(ns).Get(context.Background(), "minio", metav1.GetOptions{})
	if err == nil {
		svcFound = true
	}
	return podReady && svcFound, err
}

func useExtraObjectStorage(oc *exutil.CLI) string {
	if checkODF(oc) {
		e2e.Logf("use the existing ODF storage service")
		return "odf"
	}
	ready, err := checkMinIO(oc, minioNS)
	if ready {
		e2e.Logf("use existing MinIO storage service")
		return "minio"
	}
	if strings.Contains(err.Error(), "No resources found") || strings.Contains(err.Error(), "not found") {
		e2e.Logf("deploy MinIO and use this MinIO as storage service")
		deployMinIO(oc)
		return "minio"
	}
	return ""
}

func patchLokiOperatorWithAWSRoleArn(oc *exutil.CLI, subNamespace, roleArn string) {
	roleArnPatchConfig := `{
		"spec": {
		  "config": {
			"env": [
			  {
				"name": "ROLEARN",
				"value": "%s"
			  }
			]
		  }
		}
	  }`

	subName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("sub", "-n", subNamespace, `-ojsonpath={.items[?(@.spec.name=="loki-operator")].metadata.name}`).Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(subName).ShouldNot(o.BeEmpty())
	err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("patch").Args("sub", subName, "-n", subNamespace, "-p", fmt.Sprintf(roleArnPatchConfig, roleArn), "--type=merge").Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	waitForPodReadyWithLabel(oc, loNS, "name=loki-operator-controller-manager")
}

// return the storage type per different platform
func getStorageType(oc *exutil.CLI) string {
	platform := compat_otp.CheckPlatform(oc)
	switch platform {
	case "aws":
		{
			return "s3"
		}
	case "gcp":
		{
			return "gcs"
		}
	case "azure":
		{
			return "azure"
		}
	case "openstack":
		{
			return "swift"
		}
	default:
		{
			return useExtraObjectStorage(oc)
		}
	}
}

// lokiStack contains the configurations of loki stack
type lokiStack struct {
	name          string // lokiStack name
	namespace     string // lokiStack namespace
	tSize         string // size
	storageType   string // the backend storage type, currently support s3, gcs, azure, swift, ODF and minIO
	storageSecret string // the secret name for loki to use to connect to backend storage
	storageClass  string // storage class name
	bucketName    string // the butcket or the container name where loki stores it's data in
	template      string // the file used to create the loki stack
}

func (l lokiStack) setTSize(size string) lokiStack {
	l.tSize = size
	return l
}

// prepareResourcesForLokiStack creates buckets/containers in backend storage provider, and creates the secret for Loki to use
func (l lokiStack) prepareResourcesForLokiStack(oc *exutil.CLI) error {
	var err error
	if len(l.bucketName) == 0 {
		return fmt.Errorf("the bucketName should not be empty")
	}
	switch l.storageType {
	case "s3":
		{
			cred, err1 := getAWSCredentials(oc)
			if err1 != nil {
				g.Skip("Skip since no AWS credetial! No Env AWS_SHARED_CREDENTIALS_FILE, Env CLUSTER_PROFILE_DIR  or $HOME/.aws/credentials file")
			}
			factory, err2 := NewAWSClientFactory(context.TODO(), cred)
			if err2 != nil {
				e2e.Failf("error loading aws config: %v", err2)
			}
			if compat_otp.IsWorkloadIdentityCluster(oc) {
				partition := "aws"
				if strings.HasPrefix(cred.Region, "us-gov") {
					partition = "aws-us-gov"
				}
				iamClient := factory.IAM()
				stsClient := factory.STS()
				awsAccountID, _ := getAwsAccount(stsClient)
				oidcName, err := getOIDC(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				lokiIAMRoleName := l.name + "-" + compat_otp.GetRandomString()
				roleArn := createIAMRoleForS3Bucket(iamClient, oidcName, awsAccountID, partition, l.namespace, l.name, lokiIAMRoleName)
				os.Setenv("LOKI_ROLE_NAME_ON_STS", lokiIAMRoleName)
				patchLokiOperatorWithAWSRoleArn(oc, loNS, roleArn)
				createObjectStorageSecretOnAWSSTSCluster(oc, cred.Region, l.storageSecret, l.bucketName, l.namespace)
			} else {
				err = createSecretForAWSS3Bucket(oc, l.bucketName, l.storageSecret, l.namespace, cred)
				if err != nil {
					return fmt.Errorf("failed to create secret for aws s3 buctke: %v", err)
				}
			}
			s3Client := factory.S3()
			return createS3Bucket(s3Client, l.bucketName, cred.Region)
		}
	case "azure":
		{
			if compat_otp.IsWorkloadIdentityCluster(oc) {
				if !readAzureCredentials() {
					g.Skip("Azure Credentials not found. Skip case!")
				} else {
					performManagedIdentityAndSecretSetupForAzureWIF(oc, l.name, l.namespace, l.bucketName, l.storageSecret)
				}
			} else {
				accountName, accountKey, err1 := compat_otp.GetAzureStorageAccountFromCluster(oc)
				if err1 != nil {
					return fmt.Errorf("can't get azure storage account from cluster: %v", err1)
				}
				client, err2 := compat_otp.NewAzureContainerClient(oc, accountName, accountKey, l.bucketName)
				if err2 != nil {
					return err2
				}
				err = compat_otp.CreateAzureStorageBlobContainer(client)
				if err != nil {
					return err
				}
				return createSecretForAzureContainer(oc, l.bucketName, l.storageSecret, l.namespace)
			}
		}
	case "gcs":
		{
			projectID, errGetID := compat_otp.GetGcpProjectID(oc)
			o.Expect(errGetID).NotTo(o.HaveOccurred())
			err = compat_otp.CreateGCSBucket(projectID, l.bucketName)
			if err != nil {
				return err
			}
			if compat_otp.IsWorkloadIdentityCluster(oc) {
				clusterName := getInfrastructureName(oc)
				gcsSAName := generateServiceAccountNameForGCS(clusterName)
				os.Setenv("LOGGING_GCS_SERVICE_ACCOUNT_NAME", gcsSAName)
				projectNumber, err1 := getGCPProjectNumber(projectID)
				if err1 != nil {
					return fmt.Errorf("can't get GCP project number: %v", err1)
				}
				poolID, err2 := getPoolID(oc)
				if err2 != nil {
					return fmt.Errorf("can't get pool ID: %v", err2)
				}
				sa, err3 := createServiceAccountOnGCP(projectID, gcsSAName)
				if err3 != nil {
					return fmt.Errorf("can't create service account: %v", err3)
				}
				os.Setenv("LOGGING_GCS_SERVICE_ACCOUNT_EMAIL", sa.Email)
				err4 := grantPermissionsToGCPServiceAccount(poolID, projectID, projectNumber, l.namespace, l.name, sa.Email)
				if err4 != nil {
					return fmt.Errorf("can't add roles to the serviceaccount: %v", err4)
				}

				patchLokiOperatorOnGCPSTSforCCO(oc, loNS, projectNumber, poolID, sa.Email)

				return createSecretForGCSBucketWithSTS(oc, l.namespace, l.storageSecret, l.bucketName)
			} else {
				return createSecretForGCSBucket(oc, l.bucketName, l.storageSecret, l.namespace)
			}
		}
	case "swift":
		{
			cred, err1 := compat_otp.GetOpenStackCredentials(oc)
			o.Expect(err1).NotTo(o.HaveOccurred())
			client := compat_otp.NewOpenStackClient(cred, "object-store")
			err = compat_otp.CreateOpenStackContainer(client, l.bucketName)
			if err != nil {
				return err
			}
			return createSecretForSwiftContainer(oc, l.bucketName, l.storageSecret, l.namespace, cred)
		}
	case "odf":
		{
			err = createObjectBucketClaim(oc, l.namespace, l.bucketName)
			if err != nil {
				return err
			}
			return createSecretForODFBucket(oc, l.bucketName, l.storageSecret, l.namespace)
		}
	case "minio":
		{
			cred := getMinIOCreds(oc, minioNS)
			factory, err := NewAWSClientFactory(context.TODO(), cred)
			if err != nil {
				e2e.Failf("error loading aws config: %v", err)
			}
			client := factory.S3()
			err = createS3Bucket(client, l.bucketName, "")
			if err != nil {
				return err
			}
			return createSecretForMinIOBucket(oc, l.bucketName, l.storageSecret, l.namespace, cred)
		}
	}
	return nil
}

// deployLokiStack creates the lokiStack CR with basic settings: name, namespace, size, storage.secret.name, storage.secret.type, storageClassName
// optionalParameters is designed for adding parameters to deploy lokiStack with different tenants or some other settings
func (l lokiStack) deployLokiStack(oc *exutil.CLI, optionalParameters ...string) error {
	e2e.Logf("Running deployLokiStack")

	var storage string
	if l.storageType == "odf" || l.storageType == "minio" {
		storage = "s3"
	} else {
		storage = l.storageType
	}

	lokistackTemplate := l.template
	if GetIPVersionStackType(oc) == "ipv6single" {
		lokistackTemplate = strings.ReplaceAll(l.template, ".yaml", "-ipv6.yaml")
	}
	//Add Proxy CA for object storage
	caBundle := getProxyCaBundle(oc)
	if caBundle != "" {
		e2e.Logf("Enable caBundle for lokistack storage")
		lokistackTemplate = strings.ReplaceAll(lokistackTemplate, ".yaml", "-tls.yaml")
		cmProxyCa := resource{"configmap", l.name + "-proxy-ca", l.namespace}
		err := oc.AsAdmin().WithoutNamespace().Run("create").Args("-n", cmProxyCa.namespace, cmProxyCa.kind, cmProxyCa.name, "--from-literal=ca-bundle.crt="+caBundle).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		optionalParameters = append(optionalParameters, "CA_NAME="+cmProxyCa.name)
		optionalParameters = append(optionalParameters, "CA_KEY_NAME=ca-bundle.crt")
	}
	parameters := []string{"-f", lokistackTemplate, "-n", l.namespace, "-p", "NAME=" + l.name, "NAMESPACE=" + l.namespace, "SIZE=" + l.tSize, "SECRET_NAME=" + l.storageSecret, "STORAGE_TYPE=" + storage, "STORAGE_CLASS=" + l.storageClass}

	if len(optionalParameters) != 0 {
		parameters = append(parameters, optionalParameters...)
	}

	file, err := processTemplate(oc, parameters...)
	defer os.Remove(file)
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Can not process %v", parameters))
	err = oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", file, "-n", l.namespace).Execute()
	ls := resource{"lokistack", l.name, l.namespace}
	ls.WaitForResourceToAppear(oc)
	return err
}

func (l lokiStack) waitForLokiStackToBeReady(oc *exutil.CLI) {
	// below wait is temp fix for LOG-8579. Remove it once the bug is fixed.
	if compat_otp.IsWorkloadIdentityCluster(oc) {
		currentPlatform := compat_otp.CheckPlatform(oc)
		switch currentPlatform {
		case "aws", "azure", "gcp":
			time.Sleep(180 * time.Second)
		}
	}
	for _, deploy := range []string{l.name + "-gateway", l.name + "-distributor", l.name + "-querier", l.name + "-query-frontend"} {
		WaitForDeploymentPodsToBeReady(oc, l.namespace, deploy)
	}
	for _, ss := range []string{l.name + "-index-gateway", l.name + "-compactor", l.name + "-ruler", l.name + "-ingester"} {
		waitForStatefulsetReady(oc, l.namespace, ss)
	}
	if compat_otp.IsWorkloadIdentityCluster(oc) {
		currentPlatform := compat_otp.CheckPlatform(oc)
		switch currentPlatform {
		case "aws", "azure", "gcp":
			validateCredentialsRequestGenerationOnSTS(oc, l.name, l.namespace)
		}
	}
}

/*
// update existing lokistack CR
// if template is specified, then run command `oc process -f template -p patches | oc apply -f -`
// if template is not specified, then run command `oc patch lokistack/${l.name} -p patches`
// if use patch, should add `--type=` in the end of patches
func (l lokiStack) update(oc *exutil.CLI, template string, patches ...string) {
	var err error
	if template != "" {
		parameters := []string{"-f", template, "-p", "NAME=" + l.name, "NAMESPACE=" + l.namespace}
		if len(patches) > 0 {
			parameters = append(parameters, patches...)
		}
		file, processErr := processTemplate(oc, parameters...)
		defer os.Remove(file)
		if processErr != nil {
			e2e.Failf("error processing file: %v", processErr)
		}
		err = oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", file, "-n", l.namespace).Execute()
	} else {
		parameters := []string{"lokistack/" + l.name, "-n", l.namespace, "-p"}
		parameters = append(parameters, patches...)
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args(parameters...).Execute()
	}
	if err != nil {
		e2e.Failf("error updating lokistack: %v", err)
	}
}
*/

func (l lokiStack) removeLokiStack(oc *exutil.CLI) {
	resource{"lokistack", l.name, l.namespace}.clear(oc)
	_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("pvc", "-n", l.namespace, "-l", "app.kubernetes.io/instance="+l.name).Execute()
	_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("configmap", "-n", l.namespace, l.name+"-proxy-ca").Execute()
}

func (l lokiStack) removeObjectStorage(oc *exutil.CLI) {
	e2e.Logf("Remove Object Storage")
	resource{"secret", l.storageSecret, l.namespace}.clear(oc)
	var err error
	switch l.storageType {
	case "s3":
		{
			cred, _ := getAWSCredentials(oc)
			factory, err1 := NewAWSClientFactory(context.TODO(), cred)
			if err1 != nil {
				e2e.Failf("error loading aws config: %v", err1)
			}
			if compat_otp.IsWorkloadIdentityCluster(oc) {
				iamClient := factory.IAM()
				deleteIAMroleonAWS(iamClient, os.Getenv("LOKI_ROLE_NAME_ON_STS"))
				os.Unsetenv("LOKI_ROLE_NAME_ON_STS")
				subName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("sub", "-n", loNS, `-ojsonpath={.items[?(@.spec.name=="loki-operator")].metadata.name}`).Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(subName).ShouldNot(o.BeEmpty())
				err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("sub", subName, "-n", loNS, "-p", `[{"op": "remove", "path": "/spec/config"}]`, "--type=json").Execute()
				o.Expect(err).NotTo(o.HaveOccurred())
				waitForPodReadyWithLabel(oc, loNS, "name=loki-operator-controller-manager")
			}
			client := factory.S3()
			err = deleteS3Bucket(client, l.bucketName)
		}
	case "azure":
		{
			if compat_otp.IsWorkloadIdentityCluster(oc) {
				resourceGroup, err := getAzureResourceGroupFromCluster(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				azureSubscriptionID := os.Getenv("AZURE_SUBSCRIPTION_ID")
				cred := createNewDefaultAzureCredential()
				deleteManagedIdentityOnAzure(cred, azureSubscriptionID, resourceGroup, l.name)
				deleteAzureStorageAccount(cred, azureSubscriptionID, resourceGroup, os.Getenv("LOKI_OBJECT_STORAGE_STORAGE_ACCOUNT"))
				os.Unsetenv("LOKI_OBJECT_STORAGE_STORAGE_ACCOUNT")
			} else {
				accountName, accountKey, err1 := compat_otp.GetAzureStorageAccountFromCluster(oc)
				o.Expect(err1).NotTo(o.HaveOccurred())
				client, err2 := compat_otp.NewAzureContainerClient(oc, accountName, accountKey, l.bucketName)
				o.Expect(err2).NotTo(o.HaveOccurred())
				err = compat_otp.DeleteAzureStorageBlobContainer(client)
			}
		}
	case "gcs":
		{
			if compat_otp.IsWorkloadIdentityCluster(oc) {
				sa := os.Getenv("LOGGING_GCS_SERVICE_ACCOUNT_NAME")
				if sa == "" {
					e2e.Logf("LOGGING_GCS_SERVICE_ACCOUNT_NAME is not set, no need to delete the serviceaccount")
				} else {
					os.Unsetenv("LOGGING_GCS_SERVICE_ACCOUNT_NAME")
					email := os.Getenv("LOGGING_GCS_SERVICE_ACCOUNT_EMAIL")
					if email == "" {
						e2e.Logf("LOGGING_GCS_SERVICE_ACCOUNT_EMAIL is not set, no need to delete the policies")
					} else {
						os.Unsetenv("LOGGING_GCS_SERVICE_ACCOUNT_EMAIL")
						projectID, errGetID := compat_otp.GetGcpProjectID(oc)
						o.Expect(errGetID).NotTo(o.HaveOccurred())
						projectNumber, _ := getGCPProjectNumber(projectID)
						poolID, _ := getPoolID(oc)
						err = removePermissionsFromGCPServiceAccount(poolID, projectID, projectNumber, l.namespace, l.name, email)
						o.Expect(err).NotTo(o.HaveOccurred())
						err = removeServiceAccountFromGCP("projects/" + projectID + "/serviceAccounts/" + email)
						o.Expect(err).NotTo(o.HaveOccurred())
					}
				}
			}
			err = compat_otp.DeleteGCSBucket(l.bucketName)
		}
	case "swift":
		{
			cred, err1 := compat_otp.GetOpenStackCredentials(oc)
			o.Expect(err1).NotTo(o.HaveOccurred())
			client := compat_otp.NewOpenStackClient(cred, "object-store")
			err = compat_otp.DeleteOpenStackContainer(client, l.bucketName)
		}
	case "odf":
		{
			err = deleteObjectBucketClaim(oc, l.namespace, l.bucketName)
		}
	case "minio":
		{
			cred := getMinIOCreds(oc, minioNS)
			factory, err1 := NewAWSClientFactory(context.TODO(), cred)
			if err1 != nil {
				e2e.Failf("error loading aws config: %v", err1)
			}
			client := factory.S3()
			err = deleteS3Bucket(client, l.bucketName)
		}
	}
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (l lokiStack) createSecretFromGateway(oc *exutil.CLI, name, namespace, token string) {
	dirname := "/tmp/" + oc.Namespace() + getRandomString()
	defer os.RemoveAll(dirname)
	err := os.MkdirAll(dirname, 0777)
	o.Expect(err).NotTo(o.HaveOccurred())

	err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("cm/"+l.name+"-gateway-ca-bundle", "-n", l.namespace, "--keys=service-ca.crt", "--confirm", "--to="+dirname).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	if token != "" {
		err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", name, "-n", namespace, "--from-file=ca-bundle.crt="+dirname+"/service-ca.crt", "--from-literal=token="+token).Execute()
	} else {
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", name, "-n", namespace, "--from-file=ca-bundle.crt="+dirname+"/service-ca.crt").Execute()
	}
	o.Expect(err).NotTo(o.HaveOccurred())

}

// TODO: add an option to provide TLS config
type lokiClient struct {
	username        string //Username for HTTP basic auth.
	password        string //Password for HTTP basic auth
	address         string //Server address.
	orgID           string //adds X-Scope-OrgID to API requests for representing tenant ID. Useful for requesting tenant data when bypassing an auth gateway.
	bearerToken     string //adds the Authorization header to API requests for authentication purposes.
	bearerTokenFile string //adds the Authorization header to API requests for authentication purposes.
	retries         int    //How many times to retry each query when getting an error response from Loki.
	queryTags       string //adds X-Query-Tags header to API requests.
	quiet           bool   //Suppress query metadata.
}

// newLokiClient initializes a lokiClient with server address
func newLokiClient(routeAddress string) *lokiClient {
	client := &lokiClient{}
	client.address = routeAddress
	client.retries = 5
	client.quiet = true
	return client
}

// retry sets how many times to retry each query
func (c *lokiClient) retry(retry int) *lokiClient {
	nc := *c
	nc.retries = retry
	return &nc
}

// withToken sets the token used to do query
func (c *lokiClient) withToken(bearerToken string) *lokiClient {
	nc := *c
	nc.bearerToken = bearerToken
	return &nc
}

func (c *lokiClient) withBasicAuth(username string, password string) *lokiClient {
	nc := *c
	nc.username = username
	nc.password = password
	return &nc
}

/*
func (c *lokiClient) withTokenFile(bearerTokenFile string) *lokiClient {
	nc := *c
	nc.bearerTokenFile = bearerTokenFile
	return &nc
}
*/

func (c *lokiClient) getHTTPRequestHeader() (http.Header, error) {
	h := make(http.Header)
	if c.username != "" && c.password != "" {
		h.Set(
			"Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.password)),
		)
	}
	h.Set("User-Agent", "loki-logcli")

	if c.orgID != "" {
		h.Set("X-Scope-OrgID", c.orgID)
	}

	if c.queryTags != "" {
		h.Set("X-Query-Tags", c.queryTags)
	}

	if (c.username != "" || c.password != "") && (len(c.bearerToken) > 0 || len(c.bearerTokenFile) > 0) {
		return nil, fmt.Errorf("at most one of HTTP basic auth (username/password), bearer-token & bearer-token-file is allowed to be configured")
	}

	if len(c.bearerToken) > 0 && len(c.bearerTokenFile) > 0 {
		return nil, fmt.Errorf("at most one of the options bearer-token & bearer-token-file is allowed to be configured")
	}

	if c.bearerToken != "" {
		h.Set("Authorization", "Bearer "+c.bearerToken)
	}

	if c.bearerTokenFile != "" {
		b, err := os.ReadFile(c.bearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read authorization credentials file %s: %s", c.bearerTokenFile, err)
		}
		bearerToken := strings.TrimSpace(string(b))
		h.Set("Authorization", "Bearer "+bearerToken)
	}
	return h, nil
}

func (c *lokiClient) doRequest(path, query string, out interface{}) error {
	h, err := c.getHTTPRequestHeader()
	if err != nil {
		return err
	}

	resp, err := doHTTPRequest(h, c.address, path, query, "GET", c.quiet, c.retries, nil, 200)
	if err != nil {
		return err
	}
	return json.Unmarshal(resp, out)
}

func (c *lokiClient) doQuery(path string, query string) (*lokiQueryResponse, error) {
	var err error
	var r lokiQueryResponse

	if err = c.doRequest(path, query, &r); err != nil {
		return nil, err
	}

	return &r, nil
}

// query uses the /api/v1/query endpoint to execute an instant query
// lc.query("application", "sum by(kubernetes_namespace_name)(count_over_time({kubernetes_namespace_name=\"multiple-containers\"}[5m]))", 30, false, time.Now())
func (c *lokiClient) query(tenant string, queryStr string, limit int, forward bool, time time.Time) (*lokiQueryResponse, error) {
	direction := func() string {
		if forward {
			return "FORWARD"
		}
		return "BACKWARD"
	}
	qsb := newQueryStringBuilder()
	qsb.setString("query", queryStr)
	qsb.setInt("limit", int64(limit))
	qsb.setInt("time", time.UnixNano())
	qsb.setString("direction", direction())
	var logPath string
	if len(tenant) > 0 {
		logPath = apiPath + tenant + queryRangePath
	} else {
		logPath = queryRangePath
	}
	return c.doQuery(logPath, qsb.encode())
}

// queryRange uses the /api/v1/query_range endpoint to execute a range query
// tenant: application, infrastructure, audit
// queryStr: string to filter logs, for example: "{kubernetes_namespace_name="test"}"
// limit: max log count
// start: Start looking for logs at this absolute time(inclusive), e.g.: time.Now().Add(time.Duration(-1)*time.Hour) means 1 hour ago
// end: Stop looking for logs at this absolute time (exclusive)
// forward: true means scan forwards through logs, false means scan backwards through logs
func (c *lokiClient) queryRange(tenant string, queryStr string, limit int, start, end time.Time, forward bool) (*lokiQueryResponse, error) {
	direction := func() string {
		if forward {
			return "FORWARD"
		}
		return "BACKWARD"
	}
	params := newQueryStringBuilder()
	params.setString("query", queryStr)
	params.setInt32("limit", limit)
	params.setInt("start", start.UnixNano())
	params.setInt("end", end.UnixNano())
	params.setString("direction", direction())
	var logPath string
	if len(tenant) > 0 {
		logPath = apiPath + tenant + queryRangePath
	} else {
		logPath = queryRangePath
	}

	return c.doQuery(logPath, params.encode())
}

func (c *lokiClient) searchLogsInLoki(tenant, query string) (*lokiQueryResponse, error) {
	res, err := c.queryRange(tenant, query, 5, time.Now().Add(time.Duration(-1)*time.Hour), time.Now(), false)
	return res, err
}

func (c *lokiClient) waitForLogsAppearByQuery(tenant, query string) error {
	return wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		logs, err := c.searchLogsInLoki(tenant, query)
		if err != nil {
			e2e.Logf("\ngot err when searching logs: %v, retrying...\n", err)
			return false, nil
		}
		if len(logs.Data.Result) > 0 {
			e2e.Logf(`find logs by %s`, query)
			return true, nil
		}
		return false, nil
	})
}

func (c *lokiClient) searchByKey(tenant, key, value string) (*lokiQueryResponse, error) {
	res, err := c.searchLogsInLoki(tenant, "{"+key+"=\""+value+"\"}")
	return res, err
}

func (c *lokiClient) waitForLogsAppearByKey(tenant, key, value string) {
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		logs, err := c.searchByKey(tenant, key, value)
		if err != nil {
			e2e.Logf("\ngot err when searching logs: %v, retrying...\n", err)
			return false, nil
		}
		if len(logs.Data.Result) > 0 {
			e2e.Logf(`find logs by {%s="%s"}`, key, value)
			return true, nil
		}
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf(`can't find logs by {%s="%s"} in last 5 minutes`, key, value))
}

func (c *lokiClient) searchByNamespace(tenant, projectName string) (*lokiQueryResponse, error) {
	res, err := c.searchLogsInLoki(tenant, "{kubernetes_namespace_name=\""+projectName+"\"}")
	return res, err
}

func (c *lokiClient) waitForLogsAppearByProject(tenant, projectName string) {
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		logs, err := c.searchByNamespace(tenant, projectName)
		if err != nil {
			e2e.Logf("\ngot err when searching logs: %v, retrying...\n", err)
			return false, nil
		}
		if len(logs.Data.Result) > 0 {
			e2e.Logf("find logs from %s project", projectName)
			return true, nil
		}
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("can't find logs from %s project in last 5 minutes", projectName))
}

// extractLogEntities extract the log entities from loki query response, designed for checking the content of log data in Loki
func extractLogEntities(lokiQueryResult *lokiQueryResponse) []LogEntity {
	var lokiLogs []LogEntity
	for _, res := range lokiQueryResult.Data.Result {
		for _, value := range res.Values {
			lokiLog := LogEntity{}
			// only process log data, drop timestamp
			json.Unmarshal([]byte(convertInterfaceToArray(value)[1]), &lokiLog)
			lokiLogs = append(lokiLogs, lokiLog)
		}
	}
	return lokiLogs
}

// listLabelValues uses the /api/v1/label endpoint to list label values
func (c *lokiClient) listLabelValues(tenant, name string, start, end time.Time) (*labelResponse, error) {
	lpath := fmt.Sprintf(labelValuesPath, url.PathEscape(name))
	var labelResponse labelResponse
	params := newQueryStringBuilder()
	params.setInt("start", start.UnixNano())
	params.setInt("end", end.UnixNano())

	path := ""
	if len(tenant) > 0 {
		path = apiPath + tenant + lpath
	} else {
		path = lpath
	}

	if err := c.doRequest(path, params.encode(), &labelResponse); err != nil {
		return nil, err
	}
	return &labelResponse, nil
}

// listLabelNames uses the /api/v1/label endpoint to list label names
func (c *lokiClient) listLabelNames(tenant string, start, end time.Time) (*labelResponse, error) {
	var labelResponse labelResponse
	params := newQueryStringBuilder()
	params.setInt("start", start.UnixNano())
	params.setInt("end", end.UnixNano())
	path := ""
	if len(tenant) > 0 {
		path = apiPath + tenant + labelsPath
	} else {
		path = labelsPath
	}

	if err := c.doRequest(path, params.encode(), &labelResponse); err != nil {
		return nil, err
	}
	return &labelResponse, nil
}

// listLabels gets the label names or values
func (c *lokiClient) listLabels(tenant, labelName string) ([]string, error) {
	var labelResponse *labelResponse
	var err error
	start := time.Now().Add(time.Duration(-2) * time.Hour)
	end := time.Now()
	if len(labelName) > 0 {
		labelResponse, err = c.listLabelValues(tenant, labelName, start, end)
	} else {
		labelResponse, err = c.listLabelNames(tenant, start, end)
	}
	return labelResponse.Data, err
}

func (c *lokiClient) queryRules(tenant, ns string) ([]byte, error) {
	path := apiPath + tenant + rulesPath

	params := url.Values{}
	if ns != "" {
		params.Add("kubernetes_namespace_name", ns)
	}

	h, err := c.getHTTPRequestHeader()
	if err != nil {
		return nil, err
	}

	resp, err := doHTTPRequest(h, c.address, path, params.Encode(), "GET", c.quiet, c.retries, nil, 200)
	if err != nil {
		/*
			Ignore error "unexpected EOF", adding `h.Add("Accept-Encoding", "identity")` doesn't resolve the error.
			This seems to be an issue in lokistack when tenant=application, recording rules are not in the response.
			No error when tenant=infrastructure
		*/
		if strings.Contains(err.Error(), "unexpected EOF") && len(resp) > 0 {
			e2e.Logf("got error %s when reading the response, but ignore it", err.Error())
			return resp, nil
		}
		return nil, err
	}
	return resp, nil

}

type queryStringBuilder struct {
	values url.Values
}

func newQueryStringBuilder() *queryStringBuilder {
	return &queryStringBuilder{
		values: url.Values{},
	}
}

func (b *queryStringBuilder) setString(name, value string) {
	b.values.Set(name, value)
}

func (b *queryStringBuilder) setInt(name string, value int64) {
	b.setString(name, strconv.FormatInt(value, 10))
}

func (b *queryStringBuilder) setInt32(name string, value int) {
	b.setString(name, strconv.Itoa(value))
}

/*
func (b *queryStringBuilder) setStringArray(name string, values []string) {
	for _, v := range values {
		b.values.Add(name, v)
	}
}
func (b *queryStringBuilder) setFloat32(name string, value float32) {
	b.setString(name, strconv.FormatFloat(float64(value), 'f', -1, 32))
}
func (b *queryStringBuilder) setFloat(name string, value float64) {
	b.setString(name, strconv.FormatFloat(value, 'f', -1, 64))
}
*/

// encode returns the URL-encoded query string based on key-value
// parameters added to the builder calling Set functions.
func (b *queryStringBuilder) encode() string {
	return b.values.Encode()
}

// compareClusterResources compares the remaning resource with the requested resource provide by user
func compareClusterResources(oc *exutil.CLI, cpu, memory string) bool {
	nodes, err := compat_otp.GetSchedulableLinuxWorkerNodes(oc)
	o.Expect(err).NotTo(o.HaveOccurred())
	var remainingCPU, remainingMemory int64
	re := compat_otp.GetRemainingResourcesNodesMap(oc, nodes)
	for _, node := range nodes {
		remainingCPU += re[node.Name].CPU
		remainingMemory += re[node.Name].Memory
	}

	requiredCPU, _ := k8sresource.ParseQuantity(cpu)
	requiredMemory, _ := k8sresource.ParseQuantity(memory)
	e2e.Logf("the required cpu is: %d, and the required memory is: %d", requiredCPU.MilliValue(), requiredMemory.MilliValue())
	e2e.Logf("the remaining cpu is: %d, and the remaning memory is: %d", remainingCPU, remainingMemory)
	return remainingCPU > requiredCPU.MilliValue() && remainingMemory > requiredMemory.MilliValue()
}

// validateInfraForLoki checks platform type
// supportedPlatforms the platform types which the case can be executed on, if it's empty, then skip this check
func validateInfraForLoki(oc *exutil.CLI, supportedPlatforms ...string) bool {
	currentPlatform := compat_otp.CheckPlatform(oc)
	if len(supportedPlatforms) > 0 {
		return contain(supportedPlatforms, currentPlatform)
	}
	return true
}

// validateInfraAndResourcesForLoki checks cluster remaning resources and platform type
// supportedPlatforms the platform types which the case can be executed on, if it's empty, then skip this check
func validateInfraAndResourcesForLoki(oc *exutil.CLI, reqMemory, reqCPU string, supportedPlatforms ...string) bool {
	return validateInfraForLoki(oc, supportedPlatforms...) && compareClusterResources(oc, reqCPU, reqMemory)
}

type externalLoki struct {
	name      string
	namespace string
}

func (l externalLoki) deployLoki(oc *exutil.CLI) {
	//Create configmap for Loki
	cmTemplate := testdata.FixturePath("logging", "external-log-stores", "loki", "loki-configmap.yaml")
	lokiCM := resource{"configmap", l.name, l.namespace}
	err := lokiCM.applyFromTemplate(oc, "-n", l.namespace, "-f", cmTemplate, "-p", "LOKINAMESPACE="+l.namespace, "-p", "LOKICMNAME="+l.name)
	o.Expect(err).NotTo(o.HaveOccurred())

	//Create Deployment for Loki
	deployTemplate := testdata.FixturePath("logging", "external-log-stores", "loki", "loki-deployment.yaml")
	lokiDeploy := resource{"deployment", l.name, l.namespace}
	err = lokiDeploy.applyFromTemplate(oc, "-n", l.namespace, "-f", deployTemplate, "-p", "LOKISERVERNAME="+l.name, "-p", "LOKINAMESPACE="+l.namespace, "-p", "LOKICMNAME="+l.name)
	o.Expect(err).NotTo(o.HaveOccurred())

	//Expose Loki as a Service
	WaitForDeploymentPodsToBeReady(oc, l.namespace, l.name)
	err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("-n", l.namespace, "deployment", l.name).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	// expose loki route
	err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("-n", l.namespace, "svc", l.name).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (l externalLoki) remove(oc *exutil.CLI) {
	resource{"configmap", l.name, l.namespace}.clear(oc)
	resource{"deployment", l.name, l.namespace}.clear(oc)
	resource{"svc", l.name, l.namespace}.clear(oc)
	resource{"route", l.name, l.namespace}.clear(oc)
}

func deployMinIO(oc *exutil.CLI) {
	// create namespace
	_, err := oc.AdminKubeClient().CoreV1().Namespaces().Get(context.Background(), minioNS, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("namespace", minioNS).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
	}
	// create secret
	_, err = oc.AdminKubeClient().CoreV1().Secrets(minioNS).Get(context.Background(), minioSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", minioSecret, "-n", minioNS, "--from-literal=access_key_id="+getRandomString(), "--from-literal=secret_access_key=passwOOrd"+getRandomString()).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
	}
	// deploy minIO
	clusterDomain, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("ingress.config/cluster", "-o=jsonpath={.spec.domain}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	minioDomain := "logging-s3-minio." + clusterDomain
	deployTemplate := testdata.FixturePath("logging", "minIO", "deploy.yaml")
	deployFile, err := processTemplate(oc, "-n", minioNS, "-f", deployTemplate, "-p", "NAMESPACE="+minioNS, "NAME=minio", "SECRET_NAME="+minioSecret, "MINIO_DOMAIN="+minioDomain)
	defer os.Remove(deployFile)
	o.Expect(err).NotTo(o.HaveOccurred())
	err = oc.AsAdmin().Run("apply").Args("-f", deployFile, "-n", minioNS).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	// wait for minio to be ready
	for _, rs := range []string{"deployment", "svc", "route"} {
		resource{rs, "minio", minioNS}.WaitForResourceToAppear(oc)
	}
	WaitForDeploymentPodsToBeReady(oc, minioNS, "minio")
}

/*
func removeMinIO(oc *exutil.CLI) {
	deleteNamespace(oc, minioNS)
}
*/

// queryAlertManagerForLokiAlerts() queries user-workload alert-manager if isUserWorkloadAM parameter is true.
// All active alerts should be returned when querying Alert Managers
func queryAlertManagerForActiveAlerts(oc *exutil.CLI, token string, isUserWorkloadAM bool, alertName string, timeInMinutes int) {
	var err error
	if !isUserWorkloadAM {
		alertManagerRoute := getRouteAddress(oc, "openshift-monitoring", "alertmanager-main")
		h := make(http.Header)
		h.Add("Content-Type", "application/json")
		h.Add("Authorization", "Bearer "+token)
		params := url.Values{}
		err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, time.Duration(timeInMinutes)*time.Minute, true, func(context.Context) (done bool, err error) {
			resp, err := doHTTPRequest(h, "https://"+alertManagerRoute, "/api/v2/alerts", params.Encode(), "GET", true, 5, nil, 200)
			if err != nil {
				return false, err
			}
			if strings.Contains(string(resp), alertName) {
				return true, nil
			}
			e2e.Logf("Waiting for alert %s to be in Firing state", alertName)
			return false, nil
		})

	} else {
		userWorkloadAlertManagerURL := "https://alertmanager-user-workload.openshift-user-workload-monitoring.svc:9095/api/v2/alerts"
		authBearer := " \"Authorization: Bearer " + token + "\""
		cmd := "curl -k -H" + authBearer + " " + userWorkloadAlertManagerURL
		err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, time.Duration(timeInMinutes)*time.Minute, true, func(context.Context) (done bool, err error) {
			alerts, err := compat_otp.RemoteShPod(oc, "openshift-monitoring", "prometheus-k8s-0", "/bin/sh", "-x", "-c", cmd)
			if err != nil {
				return false, err
			}
			if strings.Contains(string(alerts), alertName) {
				return true, nil
			}
			e2e.Logf("Waiting for alert %s to be in Firing state", alertName)
			return false, nil
		})
	}

	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Alert %s is not firing after %d minutes", alertName, timeInMinutes))
}

// Deletes cluster-monitoring-config and user-workload-monitoring-config if exists and recreates configmaps.
// deleteUserWorkloadManifests() should be called once resources are created by enableUserWorkloadMonitoringForLogging()
func enableUserWorkloadMonitoringForLogging(oc *exutil.CLI) {
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("ConfigMap", "cluster-monitoring-config", "-n", "openshift-monitoring", "--ignore-not-found").Execute()
	clusterMonitoringConfigPath := testdata.FixturePath("logging", "loki-log-alerts", "cluster-monitoring-config.yaml")
	clusterMonitoringConfig := resource{"configmap", "cluster-monitoring-config", "openshift-monitoring"}
	err := clusterMonitoringConfig.applyFromTemplate(oc, "-n", clusterMonitoringConfig.namespace, "-f", clusterMonitoringConfigPath)
	o.Expect(err).NotTo(o.HaveOccurred())

	oc.AsAdmin().WithoutNamespace().Run("delete").Args("ConfigMap", "user-workload-monitoring-config", "-n", "openshift-user-workload-monitoring", "--ignore-not-found").Execute()
	userWorkloadMConfigPath := testdata.FixturePath("logging", "loki-log-alerts", "user-workload-monitoring-config.yaml")
	userworkloadConfig := resource{"configmap", "user-workload-monitoring-config", "openshift-user-workload-monitoring"}
	err = userworkloadConfig.applyFromTemplate(oc, "-n", userworkloadConfig.namespace, "-f", userWorkloadMConfigPath)
	o.Expect(err).NotTo(o.HaveOccurred())
}

func deleteUserWorkloadManifests(oc *exutil.CLI) {
	clusterMonitoringConfig := resource{"configmap", "cluster-monitoring-config", "openshift-monitoring"}
	clusterMonitoringConfig.clear(oc)
	userworkloadConfig := resource{"configmap", "user-workload-monitoring-config", "openshift-user-workload-monitoring"}
	userworkloadConfig.clear(oc)
}

// To check CredentialsRequest is generated by Loki Operator on STS clusters for CCO flow
func validateCredentialsRequestGenerationOnSTS(oc *exutil.CLI, lokiStackName, lokiNamespace string) {
	compat_otp.By("Validate that Loki Operator creates a CredentialsRequest object")
	err := oc.AsAdmin().WithoutNamespace().Run("get").Args("CredentialsRequest", lokiStackName, "-n", lokiNamespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	cloudTokenPath, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("CredentialsRequest", lokiStackName, "-n", lokiNamespace, `-o=jsonpath={.spec.cloudTokenPath}`).Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(cloudTokenPath).Should(o.Equal("/var/run/secrets/storage/serviceaccount/token"))
	serviceAccountNames, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("CredentialsRequest", lokiStackName, "-n", lokiNamespace, `-o=jsonpath={.spec.serviceAccountNames}`).Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(serviceAccountNames).Should(o.Equal(fmt.Sprintf(`["%s","%s-ruler"]`, lokiStackName, lokiStackName)))
}

// Function to check if tenant logs are present under the Google Cloud Storage bucket.
// Returns success if any one of the tenants under tenants[] are found.
func validatesIfLogsArePushedToGCSBucket(bucketName string, tenants []string) {
	// Create a new GCS client
	client, err := storage.NewClient(context.Background())
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create GCS client")

	// Get a reference to the bucket
	bucket := client.Bucket(bucketName)

	// Create a query to list objects in the bucket
	query := &storage.Query{}

	// List objects in the bucket and check for tenant object
	err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		itr := bucket.Objects(context.Background(), query)
		for {
			objAttrs, err := itr.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return false, err
			}
			for _, tenantName := range tenants {
				if strings.Contains(objAttrs.Name, tenantName) {
					e2e.Logf("Logs %s found under the bucket: %s", objAttrs.Name, bucketName)
					return true, nil
				}
			}
		}
		e2e.Logf("Waiting for data to be available under bucket: %s", bucketName)
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, "Timed out...No data is available under the bucket: "+bucketName)
}

// Global function to check if logs are pushed to external storage.
// Currently supports Amazon S3, Azure Blob Storage and Google Cloud Storage bucket.
func (l lokiStack) validateExternalObjectStorageForLogs(oc *exutil.CLI, tenants []string) {
	switch l.storageType {
	case "s3":
		{
			cred, err := getAWSCredentials(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			factory, err := NewAWSClientFactory(context.TODO(), cred)
			if err != nil {
				e2e.Failf("error loading aws config: %v", err)
			}
			s3Client := factory.S3()
			o.Expect(err).NotTo(o.HaveOccurred())
			validatesIfLogsArePushedToS3Bucket(s3Client, l.bucketName, tenants)
		}
	case "azure":
		{
			// For Azure Container Storage
			var accountName string
			var err error
			_, storageAccountURISuffix := getStorageAccountURISuffixAndEnvForAzure(oc)
			if compat_otp.IsSTSCluster(oc) {
				accountName = os.Getenv("LOKI_OBJECT_STORAGE_STORAGE_ACCOUNT")
			} else {
				_, err = compat_otp.GetAzureCredentialFromCluster(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				accountName, _, err = compat_otp.GetAzureStorageAccountFromCluster(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
			}
			validatesIfLogsArePushedToAzureContainer(storageAccountURISuffix, accountName, l.bucketName, tenants)
		}
	case "gcs":
		{
			// For Google Cloud Storage Bucket
			validatesIfLogsArePushedToGCSBucket(l.bucketName, tenants)
		}

	case "swift":
		{
			e2e.Logf("Currently swift is not supported")
			// TODO swift code here
		}
	case "minio":
		{
			validatesIfLogsArePushedToMinIOBucket(oc, l.bucketName, tenants)
		}
	default:
		{
			e2e.Logf("Currently %s is not supported", l.storageType)
		}
	}
}

// This function creates the cluster roles 'cluster-logging-application-view', 'cluster-logging-infrastructure-view' and 'cluster-logging-audit-view' introduced
// for fine grained read access to LokiStack logs. The ownership of these roles is moved to Cluster Observability Operator (COO) from Cluster Logging Operator (CLO) in Logging 6.0+
func createLokiClusterRolesForReadAccess(oc *exutil.CLI) {
	rbacFile := testdata.FixturePath("logging", "lokistack", "fine-grained-access-roles.yaml")
	msg, err := oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", rbacFile).Output()
	o.Expect(err).NotTo(o.HaveOccurred(), msg)
}

func deleteLokiClusterRolesForReadAccess(oc *exutil.CLI) {
	roles := []string{"cluster-logging-application-view", "cluster-logging-infrastructure-view", "cluster-logging-audit-view"}
	for _, role := range roles {
		msg, err := oc.AsAdmin().WithoutNamespace().Run("delete").Args("clusterrole", role).Output()
		if err != nil {
			e2e.Logf("Failed to delete Loki RBAC role '%s': %s", role, msg)
		}
	}
}

// Patches Loki Operator running on a GCP WIF cluster. Operator is deployed with CCO mode after patching.
func patchLokiOperatorOnGCPSTSforCCO(oc *exutil.CLI, namespace string, projectNumber string, poolID string, serviceAccount string) {
	patchConfig := `{
    	"spec": {
        	"config": {
            	"env": [
               		{
                    	"name": "PROJECT_NUMBER",
                    	"value": "%s"
                	},
                	{
                    	"name": "POOL_ID",
                    	"value": "%s"
                	},
                	{
                    	"name": "PROVIDER_ID",
                    	"value": "%s"
                	},
                	{
                    	"name": "SERVICE_ACCOUNT_EMAIL",
                    	"value": "%s"
                	}
            	]
        	}
    	}
	}`

	err := oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("patch").Args("sub", "loki-operator", "-n", namespace, "-p", fmt.Sprintf(patchConfig, projectNumber, poolID, poolID, serviceAccount), "--type=merge").Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	waitForPodReadyWithLabel(oc, loNS, "name=loki-operator-controller-manager")
}

// Compare the expected TLS config with the current TLS config on apiserver/cluster. Compares the spec.tlsSecurityProfile attribute.
func compareExpectedTLSConfigWithCurrent(oc *exutil.CLI, expectedTLSConfig string) bool {
	currentTLSConfig, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("apiserver/cluster", "-o", "jsonpath={.spec.tlsSecurityProfile}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())

	return currentTLSConfig == expectedTLSConfig
}

// Function to check if logs are present under the MinIO bucket.
// Returns success if any one of the tenants under tenants[] are found.
func validatesIfLogsArePushedToMinIOBucket(oc *exutil.CLI, bucketName string, tenants []string) {
	// Build an S3 client pointing to the in-cluster MinIO endpoint
	cred := getMinIOCreds(oc, minioNS)
	factory, err := NewAWSClientFactory(context.TODO(), cred)
	if err != nil {
		e2e.Failf("error loading aws config: %v", err)
	}
	s3Client := factory.S3()
	o.Expect(err).NotTo(o.HaveOccurred())

	// Poll to check contents of the MinIO bucket
	err = wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		listObjectsOutput, err := s3Client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
			Bucket: aws.String(bucketName),
		})
		if err != nil {
			return false, err
		}

		for _, object := range listObjectsOutput.Contents {
			for _, tenantName := range tenants {
				if strings.Contains(*object.Key, tenantName) {
					e2e.Logf("Logs %s found under the minio bucket: %s", *object.Key, bucketName)
					return true, nil
				}
			}
		}
		e2e.Logf("Waiting for data to be available under bucket: %s", bucketName)
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, "Timed out...No data is available under the bucket: "+bucketName)
}

// verifyIfLokiStackNetworkPoliciesExist checks if network policies exist for the LokiStack.
// Returns 'false' if no network policies found (disabled), 'true' if network policies are found
func (l lokiStack) verifyIfLokiStackNetworkPoliciesExist(oc *exutil.CLI) bool {
	e2e.Logf("Checking if network policies exist for lokistack: %s in namespace: %s", l.name, l.namespace)
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkpolicy", "-n", l.namespace, "-l", "app.kubernetes.io/instance="+l.name, "-o", "name").Output()
	if err != nil {
		e2e.Logf("Error getting network policies: %v", err)
	}
	if len(output) > 0 {
		e2e.Logf("Network policies found for lokistack %s in namespace %s", l.name, l.namespace)
		return true
	}
	e2e.Logf("No network policies found for lokistack %s in namespace %s", l.name, l.namespace)
	return false
}

// verifyLokiStackNetworkPolicies verifies that all expected network policies are created for the lokistack
// and validates their content including policy types, egress rules, ingress rules, and pod selectors
func (l lokiStack) verifyLokiStackNetworkPolicies(oc *exutil.CLI) {
	// Define expected network policies with their policy types
	expectedPolicies := []LokiStackNetworkPolicy{
		{Name: l.name + "-default-deny", PolicyTypes: []string{"Ingress", "Egress"}},
		{Name: l.name + "-gateway-allow", PolicyTypes: []string{"Egress", "Ingress"}},
		{Name: l.name + "-gateway-allow-metrics", PolicyTypes: []string{"Ingress"}},
		{Name: l.name + "-loki-allow", PolicyTypes: []string{"Egress", "Ingress"}},
		{Name: l.name + "-loki-allow-bucket-egress", PolicyTypes: []string{"Egress"}},
		{Name: l.name + "-loki-allow-gateway-ingress", PolicyTypes: []string{"Ingress"}},
		{Name: l.name + "-loki-allow-metrics", PolicyTypes: []string{"Egress", "Ingress"}},
		{Name: l.name + "-ruler-allow-alert-egress", PolicyTypes: []string{"Egress"}},
	}

	e2e.Logf("Verifying %d network policies for lokistack: %s in namespace: %s", len(expectedPolicies), l.name, l.namespace)

	for _, expected := range expectedPolicies {
		// Get network policy with full content for validation
		np, err := l.getLokiStackNetworkPolicy(oc, expected.Name)
		o.Expect(err).NotTo(o.HaveOccurred(), fmt.Sprintf("Failed to get network policy %s", expected.Name))

		// Validate policy types
		if !validatePolicyTypes(np.Spec.PolicyTypes, expected.PolicyTypes) {
			output, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkpolicy", "-n", l.namespace, expected.Name, "-oyaml").Output()
			e2e.Logf("Network policy %s content:\n%s", expected.Name, output)
			e2e.Failf("Network policy %s has policyTypes: %v, expected: %v", expected.Name, np.Spec.PolicyTypes, expected.PolicyTypes)
		}

		// Validate policy content based on policy name
		err = l.validateLokiStackNetworkPolicyContent(oc, expected.Name)
		o.Expect(err).NotTo(o.HaveOccurred(), fmt.Sprintf("Failed to validate content for %s", expected.Name))

		e2e.Logf("Network policy %s validated with policyTypes: %v", expected.Name, expected.PolicyTypes)
	}
	e2e.Logf("All network policies found and verified for lokistack %s successfully", l.name)
}

// getLokiStackNetworkPolicy gets the entire content of a LokiStack network policy
func (l lokiStack) getLokiStackNetworkPolicy(oc *exutil.CLI, policyName string) (NetworkPolicy, error) {
	var netpol NetworkPolicy
	np, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkpolicy", "-n", l.namespace, policyName, "-oyaml").Output()
	if err != nil {
		return netpol, fmt.Errorf("error getting networkpolicy %s: %v", policyName, err)
	}
	err = yaml.Unmarshal([]byte(np), &netpol)
	if err != nil {
		return netpol, fmt.Errorf("error unmarshalling YAML: %v", err)
	}
	return netpol, nil
}

// validateLokiStackNetworkPolicyContent validates the specific content of each LokiStack network policy
func (l lokiStack) validateLokiStackNetworkPolicyContent(oc *exutil.CLI, policyName string) error {

	np, err := l.getLokiStackNetworkPolicy(oc, policyName)
	o.Expect(err).NotTo(o.HaveOccurred(), fmt.Sprintf("Failed to get network policy %s", policyName))

	switch policyName {
	case l.name + "-default-deny":
		// Should have NO ingress/egress rules (default deny all)
		if len(np.Spec.Ingress) > 0 || len(np.Spec.Egress) > 0 {
			return fmt.Errorf("default-deny policy should have no ingress/egress rules, found ingress: %d, egress: %d",
				len(np.Spec.Ingress), len(np.Spec.Egress))
		}
		// Should match all lokistack pods via matchLabels
		if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"] != "lokistack" {
			return fmt.Errorf("default-deny policy podSelector should match lokistack name label")
		}
		if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/instance"] != l.name {
			return fmt.Errorf("default-deny policy podSelector should match instance label")
		}

	case l.name + "-gateway-allow":
		// Validate podSelector for lokistack-gateway component
		if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "lokistack-gateway" {
			return fmt.Errorf("gateway-allow policy should select lokistack-gateway component")
		}
		// Should have egress rules: DNS (5353), loki (3100), API (6443)
		if len(np.Spec.Egress) < 3 {
			return fmt.Errorf("gateway-allow policy should have at least 3 egress rules (DNS, loki, API)")
		}
		// Verify key ports exist
		ports := make(map[string]bool)
		for _, egress := range np.Spec.Egress {
			for _, port := range egress.Ports {
				ports[port.Port] = true
			}
		}
		requiredPorts := []string{"5353", "3100", "6443"}
		for _, reqPort := range requiredPorts {
			if !ports[reqPort] {
				return fmt.Errorf("gateway-allow policy missing required egress port: %s", reqPort)
			}
		}
		// Should have ingress rule for port 8080
		if len(np.Spec.Ingress) == 0 {
			return fmt.Errorf("gateway-allow policy should have ingress rules")
		}
		hasPort8080 := false
		for _, ingress := range np.Spec.Ingress {
			for _, port := range ingress.Ports {
				if port.Port == "8080" {
					hasPort8080 = true
				}
			}
		}
		if !hasPort8080 {
			return fmt.Errorf("gateway-allow policy should have ingress port 8080")
		}

	case l.name + "-gateway-allow-metrics":
		// Validate podSelector for lokistack-gateway component
		if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "lokistack-gateway" {
			return fmt.Errorf("gateway-allow-metrics policy should select lokistack-gateway component")
		}
		// Should have ingress from prometheus in openshift-monitoring
		if len(np.Spec.Ingress) == 0 {
			return fmt.Errorf("gateway-allow-metrics policy should have ingress rules")
		}
		hasPrometheus := false
		hasPort8081 := false
		hasPort8083 := false
		hasNamespaceCheck := false
		for _, ingress := range np.Spec.Ingress {
			// Check for prometheus selector
			for _, from := range ingress.From {
				if from.PodSelector != nil && from.PodSelector.MatchLabels["app.kubernetes.io/name"] == "prometheus" {
					hasPrometheus = true
				}
				if from.NamespaceSelector != nil && from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "openshift-monitoring" {
					hasNamespaceCheck = true
				}
			}
			// Check for both metrics ports 8081 and 8083
			for _, port := range ingress.Ports {
				if port.Port == "8081" {
					hasPort8081 = true
				}
				if port.Port == "8083" {
					hasPort8083 = true
				}
			}
		}
		if !hasPrometheus {
			return fmt.Errorf("gateway-allow-metrics policy should allow prometheus pods")
		}
		if !hasNamespaceCheck {
			return fmt.Errorf("gateway-allow-metrics policy should allow from openshift-monitoring namespace")
		}
		if !hasPort8081 {
			return fmt.Errorf("gateway-allow-metrics policy missing port 8081")
		}
		if !hasPort8083 {
			return fmt.Errorf("gateway-allow-metrics policy missing port 8083")
		}

	case l.name + "-loki-allow":
		// Should have matchExpressions in podSelector
		if len(np.Spec.PodSelector.MatchExpressions) == 0 {
			return fmt.Errorf("loki-allow policy should use matchExpressions in podSelector")
		}
		// Should have both ingress and egress rules
		if len(np.Spec.Egress) == 0 || len(np.Spec.Ingress) == 0 {
			return fmt.Errorf("loki-allow policy should have both ingress and egress rules")
		}
		// Verify egress ports: DNS (5353), 3100, 3101, 9095, 7946
		egressPorts := make(map[string]bool)
		for _, egress := range np.Spec.Egress {
			for _, port := range egress.Ports {
				egressPorts[port.Port] = true
			}
		}
		requiredEgressPorts := []string{"5353", "3100", "3101", "9095", "7946"}
		for _, reqPort := range requiredEgressPorts {
			if !egressPorts[reqPort] {
				return fmt.Errorf("loki-allow policy missing required egress port: %s", reqPort)
			}
		}
		// Verify ingress ports: 3100, 3101, 9095, 7946
		ingressPorts := make(map[string]bool)
		for _, ingress := range np.Spec.Ingress {
			for _, port := range ingress.Ports {
				ingressPorts[port.Port] = true
			}
		}
		requiredIngressPorts := []string{"3100", "3101", "9095", "7946"}
		for _, reqPort := range requiredIngressPorts {
			if !ingressPorts[reqPort] {
				return fmt.Errorf("loki-allow policy missing required ingress port: %s", reqPort)
			}
		}

	case l.name + "-loki-allow-bucket-egress":
		// Should use matchExpressions in podSelector
		if len(np.Spec.PodSelector.MatchExpressions) == 0 {
			return fmt.Errorf("loki-allow-bucket-egress policy should use matchExpressions in podSelector")
		}
		// Should have egress rule for port 443 (object storage)
		// Currently we only check for external object storage due to bugs for ODF, swift and minIO. We will add checks for ODF, swift and minIO when the bugs are fixed.
		if len(np.Spec.Egress) == 0 {
			return fmt.Errorf("loki-allow-bucket-egress policy should have egress rules")
		}
		hasPort443 := false
		for _, egress := range np.Spec.Egress {
			for _, port := range egress.Ports {
				if port.Port == "443" && port.Protocol == "TCP" {
					hasPort443 = true
				}
			}
		}
		if !hasPort443 {
			return fmt.Errorf("loki-allow-bucket-egress policy should have egress rule for port 443/TCP")
		}

	case l.name + "-loki-allow-gateway-ingress":
		// Should use matchExpressions in podSelector
		if len(np.Spec.PodSelector.MatchExpressions) == 0 {
			return fmt.Errorf("loki-allow-gateway-ingress policy should use matchExpressions in podSelector")
		}
		// Should have ingress from gateway pods on port 3100
		if len(np.Spec.Ingress) == 0 {
			return fmt.Errorf("loki-allow-gateway-ingress policy should have ingress rules")
		}
		hasGateway := false
		hasPort3100 := false
		for _, ingress := range np.Spec.Ingress {
			for _, from := range ingress.From {
				if from.PodSelector != nil && from.PodSelector.MatchLabels["app.kubernetes.io/component"] == "lokistack-gateway" {
					hasGateway = true
				}
			}
			for _, port := range ingress.Ports {
				if port.Port == "3100" && port.Protocol == "TCP" {
					hasPort3100 = true
				}
			}
		}
		if !hasGateway {
			return fmt.Errorf("loki-allow-gateway-ingress policy should allow lokistack-gateway component")
		}
		if !hasPort3100 {
			return fmt.Errorf("loki-allow-gateway-ingress policy should have port 3100")
		}

	case l.name + "-loki-allow-metrics":
		// Should use matchExpressions in podSelector
		if len(np.Spec.PodSelector.MatchExpressions) == 0 {
			return fmt.Errorf("loki-allow-metrics policy should use matchExpressions in podSelector")
		}
		// Should have ingress from prometheus on port 3100
		if len(np.Spec.Ingress) == 0 {
			return fmt.Errorf("loki-allow-metrics policy should have ingress rules")
		}
		hasPrometheus := false
		hasPort3100 := false
		hasNamespaceCheck := false
		for _, ingress := range np.Spec.Ingress {
			for _, from := range ingress.From {
				if from.PodSelector != nil && from.PodSelector.MatchLabels["app.kubernetes.io/name"] == "prometheus" {
					hasPrometheus = true
				}
				if from.NamespaceSelector != nil && from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "openshift-monitoring" {
					hasNamespaceCheck = true
				}
			}
			for _, port := range ingress.Ports {
				if port.Port == "3100" {
					hasPort3100 = true
				}
			}
		}
		if !hasPrometheus {
			return fmt.Errorf("loki-allow-metrics policy should allow prometheus pods")
		}
		if !hasNamespaceCheck {
			return fmt.Errorf("loki-allow-metrics policy should allow from openshift-monitoring namespace")
		}
		if !hasPort3100 {
			return fmt.Errorf("loki-allow-metrics policy should have port 3100")
		}

	case l.name + "-ruler-allow-alert-egress":
		// Should use matchLabels with component: ruler
		if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "ruler" {
			return fmt.Errorf("ruler-allow-alert-egress policy should select ruler component")
		}
		// Should have egress to alertmanager on port 9095
		if len(np.Spec.Egress) == 0 {
			return fmt.Errorf("ruler-allow-alert-egress policy should have egress rules")
		}
		hasAlertmanager := false
		hasPort9095 := false
		for _, egress := range np.Spec.Egress {
			for _, to := range egress.To {
				if to.PodSelector != nil && to.PodSelector.MatchLabels["app.kubernetes.io/name"] == "alertmanager" {
					hasAlertmanager = true
				}
			}
			for _, port := range egress.Ports {
				if port.Port == "9095" && port.Protocol == "TCP" {
					hasPort9095 = true
				}
			}
		}
		if !hasAlertmanager {
			return fmt.Errorf("ruler-allow-alert-egress policy should allow alertmanager pods")
		}
		if !hasPort9095 {
			return fmt.Errorf("ruler-allow-alert-egress policy should have egress port 9095/TCP")
		}
	}

	return nil
}

// validatePolicyTypes checks if the actual policy types match the expected policy types
func validatePolicyTypes(actualTypes, expectedTypes []string) bool {
	if len(actualTypes) != len(expectedTypes) {
		return false
	}

	expectedMap := make(map[string]bool)
	for _, pType := range expectedTypes {
		expectedMap[pType] = true
	}

	for _, pType := range actualTypes {
		if !expectedMap[pType] {
			return false
		}
	}
	return true
}
