package logging

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

func getAWSCredentials(oc *exutil.CLI) (AWSClientConfig, error) {
	var region string
	platform := compat_otp.CheckPlatform(oc)
	if platform == "aws" {
		rawRegion, err := compat_otp.GetAWSClusterRegion(oc)
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("can't get AWS region: %v", err)
		}
		region = rawRegion
	} else {
		region = "us-east-2"
	}

	prowConfigDir, present := os.LookupEnv("CLUSTER_PROFILE_DIR")
	if present {
		awsCredFile := filepath.Join(prowConfigDir, ".awscred")
		if _, err := os.Stat(awsCredFile); err == nil {
			e2e.Logf("use CLUSTER_PROFILE_DIR/.awscred")
			return AWSClientConfig{CredsFilePath: awsCredFile, Region: region}, nil
		}
	}

	awsSharedCredentialsFile, present := os.LookupEnv("AWS_SHARED_CREDENTIALS_FILE")
	if present {
		e2e.Logf("use AWS_SHARED_CREDENTIALS_FILE")
		return AWSClientConfig{CredsFilePath: awsSharedCredentialsFile, Region: region}, nil
	}

	accessKey, accessKeyExist := os.LookupEnv("AWS_ACCESS_KEY_ID")
	secretKey, secretKeyExist := os.LookupEnv("AWS_SECRET_ACCESS_KEY")
	if accessKeyExist && secretKeyExist {
		e2e.Logf("use AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
		return AWSClientConfig{AccessKey: accessKey, SecretKey: secretKey, Region: region}, nil
	}

	if platform == "aws" && !compat_otp.IsWorkloadIdentityCluster(oc) {
		dirname := "/tmp/" + oc.Namespace() + "-creds"
		defer os.RemoveAll(dirname)
		err := os.MkdirAll(dirname, 0777)
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("error creating directory %s: %v", dirname, err)
		}
		err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("secret/aws-creds", "-n", "kube-system", "--confirm", "--to="+dirname).Execute()
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("failed to extract secret/aws-creds: %v", err)
		}

		accessKeyID, err := os.ReadFile(dirname + "/aws_access_key_id")
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("failed to read file aws_access_key_id: %v", err)
		}
		secretAccessKey, err := os.ReadFile(dirname + "/aws_secret_access_key")
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("failed to read file aws_secret_access_key: %v", err)
		}
		e2e.Logf("use credentials from cluster")
		return AWSClientConfig{AccessKey: string(accessKeyID), SecretKey: string(secretAccessKey), Region: region}, nil
	}

	home, _ := os.UserHomeDir()
	if _, err := os.Stat(home + "/.aws/credentials"); err == nil {
		e2e.Logf("use $HOME/.aws/credentials")
		return AWSClientConfig{CredsFilePath: home + "/.aws/credentials", Profile: "default", Region: region}, nil
	}
	return AWSClientConfig{}, fmt.Errorf("can't get aws credentials")
}

func loadAccessKeySecretKeyFromFile(ctx context.Context, cfg *AWSClientConfig) error {
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		e2e.Logf("access key and secret key are already set")
		return nil
	}
	loadOptions := []func(*config.LoadOptions) error{}
	if cfg.CredsFilePath != "" {
		loadOptions = append(loadOptions, config.WithSharedCredentialsFiles([]string{cfg.CredsFilePath}))
	}
	if cfg.Profile != "" {
		loadOptions = append(loadOptions, config.WithSharedConfigProfile(cfg.Profile))
	}

	tempCfg, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return fmt.Errorf("failed to load SDK config: %w", err)
	}
	creds, err := tempCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("failed to retrieve credentials: %w", err)
	}
	cfg.AccessKey = creds.AccessKeyID
	cfg.SecretKey = creds.SecretAccessKey

	return nil
}

func NewAWSClientFactory(ctx context.Context, cfg AWSClientConfig) (*AWSClientFactory, error) {
	var loadOptions []func(*config.LoadOptions) error

	if cfg.Region != "" {
		loadOptions = append(loadOptions, config.WithRegion(cfg.Region))
	}

	// For ODF and Minio, they're deployed in OCP clusters
	// In some clusters, we can't connect it without proxy, here add proxy settings to s3 client when there has http_proxy or https_proxy in the env var
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipTLS},
		},
	}
	if len(cfg.Endpoint) > 0 {
		proxy := getProxyFromEnv()
		if len(proxy) > 0 {
			proxyURL, err := url.Parse(proxy)
			o.Expect(err).NotTo(o.HaveOccurred())
			httpClient = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipTLS},
					Proxy:           http.ProxyURL(proxyURL),
				},
			}
		}
	}
	loadOptions = append(loadOptions, config.WithHTTPClient(httpClient))

	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		loadOptions = append(loadOptions, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	} else {
		if cfg.CredsFilePath != "" {
			loadOptions = append(loadOptions, config.WithSharedCredentialsFiles([]string{cfg.CredsFilePath}))
		}
		if cfg.Profile != "" {
			loadOptions = append(loadOptions, config.WithSharedConfigProfile(cfg.Profile))
		}
	}
	/*
		loadOptions = append(loadOptions, config.WithAssumeRoleCredentialOptions(func(o *stscreds.AssumeRoleOptions) {
			o.TokenProvider = stscreds.StdinTokenProvider // Prompts terminal for MFA
		}))
	*/

	// Load the SDK Configuration
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &AWSClientFactory{
		Config:         awsCfg,
		CustomEndpoint: cfg.Endpoint,
	}, nil
}

// S3 returns an S3 client. Enables PathStyle if a custom endpoint is used.
func (f *AWSClientFactory) S3() *s3.Client {
	return s3.NewFromConfig(f.Config, func(o *s3.Options) {
		if f.CustomEndpoint != "" {
			o.BaseEndpoint = aws.String(f.CustomEndpoint)
			o.UsePathStyle = true
		}
	})
}

// IAM returns an IAM client.
func (f *AWSClientFactory) IAM() *iam.Client {
	return iam.NewFromConfig(f.Config, func(o *iam.Options) {
		if f.CustomEndpoint != "" {
			o.BaseEndpoint = aws.String(f.CustomEndpoint)
		}
	})
}

// CloudWatchLogs returns a CloudWatchLogs client.
func (f *AWSClientFactory) CloudWatchLogs() *cloudwatchlogs.Client {
	return cloudwatchlogs.NewFromConfig(f.Config, func(o *cloudwatchlogs.Options) {
		if f.CustomEndpoint != "" {
			o.BaseEndpoint = aws.String(f.CustomEndpoint)
		}
	})
}

// STS returns an STS client.
func (f *AWSClientFactory) STS() *sts.Client {
	return sts.NewFromConfig(f.Config, func(o *sts.Options) {
		if f.CustomEndpoint != "" {
			o.BaseEndpoint = aws.String(f.CustomEndpoint)
		}
	})
}

func createS3Bucket(client *s3.Client, bucketName, region string) error {
	// check if the bucket exists or not
	// if exists, clear all the objects in the bucket
	// if not, create the bucket
	exist := false
	buckets, err := client.ListBuckets(context.TODO(), &s3.ListBucketsInput{})
	o.Expect(err).NotTo(o.HaveOccurred())
	for _, bu := range buckets.Buckets {
		if *bu.Name == bucketName {
			exist = true
			break
		}
	}
	// clear all the objects in the bucket
	if exist {
		return emptyS3Bucket(client, bucketName)
	}

	/*
		Per https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucket.html#API_CreateBucket_RequestBody,
		us-east-1 is the default region and it's not a valid value of LocationConstraint,
		using `LocationConstraint: types.BucketLocationConstraint("us-east-1")` gets error `InvalidLocationConstraint`.
		Here remove the configration when the region is us-east-1
	*/
	if len(region) == 0 || region == "us-east-1" {
		_, err = client.CreateBucket(context.TODO(), &s3.CreateBucketInput{Bucket: &bucketName})
		return err
	}
	_, err = client.CreateBucket(context.TODO(), &s3.CreateBucketInput{Bucket: &bucketName, CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}})
	return err
}

func deleteS3Bucket(client *s3.Client, bucketName string) error {
	// empty bucket
	err := emptyS3Bucket(client, bucketName)
	if err != nil {
		return err
	}
	// delete bucket
	_, err = client.DeleteBucket(context.TODO(), &s3.DeleteBucketInput{Bucket: &bucketName})
	return err
}

func emptyS3Bucket(client *s3.Client, bucketName string) error {
	// List objects in the bucket
	objects, err := client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
		Bucket: &bucketName,
	})
	if err != nil {
		return err
	}

	// Delete objects in the bucket
	if len(objects.Contents) > 0 {
		objectIdentifiers := make([]types.ObjectIdentifier, len(objects.Contents))
		for i, object := range objects.Contents {
			objectIdentifiers[i] = types.ObjectIdentifier{Key: object.Key}
		}

		quiet := true
		_, err = client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
			Bucket: &bucketName,
			Delete: &types.Delete{
				Objects: objectIdentifiers,
				Quiet:   &quiet,
			},
		})
		if err != nil {
			return err
		}
	}

	// Check if there are more objects to delete and handle pagination
	if *objects.IsTruncated {
		return emptyS3Bucket(client, bucketName)
	}

	return nil
}

// getAwsAccount returns the account ID and the AWS ARN associated with the calling entity.
func getAwsAccount(stsClient *sts.Client) (string, string) {
	e2e.Logf("Running getAwsAccount")
	result, err := stsClient.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
	o.Expect(err).NotTo(o.HaveOccurred())
	return aws.ToString(result.Account), aws.ToString(result.Arn)
}

// iamCreateRole creates a role on AWS and returns the role ARN
func iamCreateRole(iamClient *iam.Client, trustPolicy string, roleName string) string {
	e2e.Logf("Create iam role %v", roleName)
	result, err := iamClient.CreateRole(context.TODO(), &iam.CreateRoleInput{
		AssumeRolePolicyDocument: aws.String(trustPolicy),
		RoleName:                 aws.String(roleName),
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "couldn't create role "+roleName)
	return aws.ToString(result.Role.Arn)
}

// iamDeleteRole deletes the role from AWS
func iamDeleteRole(iamClient *iam.Client, roleName string) {
	_, err := iamClient.DeleteRole(context.TODO(), &iam.DeleteRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		e2e.Logf("Couldn't delete role %s: %v", roleName, err)
	}
}

// aws iam create-policy
func iamCreatePolicy(iamClient *iam.Client, mgmtPolicy string, policyName string) string {
	e2e.Logf("Create iam policy %v", policyName)
	result, err := iamClient.CreatePolicy(context.TODO(), &iam.CreatePolicyInput{
		PolicyDocument: aws.String(mgmtPolicy),
		PolicyName:     aws.String(policyName),
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "Couldn't create policy"+policyName)
	return aws.ToString(result.Policy.Arn)
}

// aws iam delete-policy
func iamDeletePolicy(iamClient *iam.Client, policyArn string) {
	_, err := iamClient.DeletePolicy(context.TODO(), &iam.DeletePolicyInput{
		PolicyArn: aws.String(policyArn),
	})
	if err != nil {
		e2e.Logf("Couldn't delete policy %v: %v", policyArn, err)
	}
}

// This func creates a IAM role, attaches custom trust policy and managed permission policy
func createIAMRoleOnAWS(iamClient *iam.Client, trustPolicy string, roleName string, policyArn string) string {
	roleArn := iamCreateRole(iamClient, trustPolicy, roleName)
	//Adding managed permission policy if provided
	if policyArn != "" {
		_, err := iamClient.AttachRolePolicy(context.TODO(), &iam.AttachRolePolicyInput{
			PolicyArn: aws.String(policyArn),
			RoleName:  aws.String(roleName),
		})
		o.Expect(err).NotTo(o.HaveOccurred())
	}
	return roleArn
}

// Deletes IAM role and attached policies
func deleteIAMroleonAWS(iamClient *iam.Client, roleName string) {
	// List attached policies of the IAM role
	listAttachedPoliciesOutput, err := iamClient.ListAttachedRolePolicies(context.TODO(), &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		e2e.Logf("Error listing attached policies of IAM role %s", roleName)
	}

	if len(listAttachedPoliciesOutput.AttachedPolicies) == 0 {
		e2e.Logf("No attached policies under IAM role: %s", roleName)
	}

	if len(listAttachedPoliciesOutput.AttachedPolicies) != 0 {
		// Detach attached policy from the IAM role
		for _, policy := range listAttachedPoliciesOutput.AttachedPolicies {
			_, err := iamClient.DetachRolePolicy(context.TODO(), &iam.DetachRolePolicyInput{
				RoleName:  aws.String(roleName),
				PolicyArn: policy.PolicyArn,
			})
			if err != nil {
				e2e.Logf("Error detaching policy: %s", *policy.PolicyName)
			} else {
				e2e.Logf("Detached policy: %s", *policy.PolicyName)
			}
		}
	}

	// Delete the IAM role
	iamDeleteRole(iamClient, roleName)
}

// createIAMRoleForS3Bucket creates role required for s3 bucket on STS clusters and returns the roleArn
func createIAMRoleForS3Bucket(iamClient *iam.Client, oidcName, awsAccountID, partition, serviceAccountNamespace, serviceAccountName, roleName string) string {
	e2e.Logf("Running createIAMRoleForS3Bucket")
	policyArn := "arn:" + partition + ":iam::aws:policy/AmazonS3FullAccess"

	s3BucketTrustPolicy := `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Principal": {
					"Federated": "arn:%s:iam::%s:oidc-provider/%s"
				},
				"Action": "sts:AssumeRoleWithWebIdentity",
				"Condition": {
					"StringEquals": {
						"%s:sub": [
							"system:serviceaccount:%s:%s",
							"system:serviceaccount:%s:%s-ruler"
						]
					}
				}
			}
		]
	}`
	s3BucketTrustPolicy = fmt.Sprintf(s3BucketTrustPolicy, partition, awsAccountID, oidcName, oidcName, serviceAccountNamespace, serviceAccountName, serviceAccountNamespace, serviceAccountName)
	return createIAMRoleOnAWS(iamClient, s3BucketTrustPolicy, roleName, policyArn)
}

// Creates Loki object storage secret on AWS STS cluster
func createObjectStorageSecretOnAWSSTSCluster(oc *exutil.CLI, region, storageSecret, bucketName, namespace string) {
	err := oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", storageSecret, "--from-literal=region="+region, "--from-literal=bucketnames="+bucketName, "--from-literal=audience=openshift", "-n", namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

// Function to check if tenant logs are present under the S3 bucket.
// Returns success if any one of the tenants under tenants[] are found.
func validatesIfLogsArePushedToS3Bucket(s3Client *s3.Client, bucketName string, tenants []string) {
	// Poll to check contents of the s3 bucket
	err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		listObjectsOutput, err := s3Client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
			Bucket: aws.String(bucketName),
		})
		if err != nil {
			return false, err
		}

		for _, object := range listObjectsOutput.Contents {
			for _, tenantName := range tenants {
				if strings.Contains(*object.Key, tenantName) {
					e2e.Logf("Logs %s found under the bucket: %s", *object.Key, bucketName)
					return true, nil
				}
			}
		}
		e2e.Logf("Waiting for data to be available under bucket: %s", bucketName)
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, "Timed out...No data is available under the bucket: "+bucketName)
}

// cloudWatchSpec the basic object which describe all common test options
type cloudwatchSpec struct {
	awsRoleName         string
	awsRoleArn          string
	awsRegion           string
	awsPolicyName       string
	awsPolicyArn        string
	awsPartition        string //The partition in which the resource is located, valid when the cluster is STS, ref: https://docs.aws.amazon.com/IAM/latest/UserGuide/reference-arns.html#arns-syntax
	clusterPlatformType string
	collectorSAName     string // the service account for collector pod to use
	cwClient            *cloudwatchlogs.Client
	groupName           string // the strategy for grouping logstreams, for example: '{.log_type||"none"}'
	hasMaster           bool   // wether the cluster has master nodes or not
	iamClient           *iam.Client
	logTypes            []string //default: "['infrastructure','application', 'audit']"
	nodes               []string // Cluster Nodes Names, required when checking infrastructure/audit logs and strict=true
	ovnEnabled          bool     // if ovn is enabled
	secretName          string   // the name of the secret for the collector to use
	secretNamespace     string   // the namespace where the collector pods to be deployed
	stsEnabled          bool     // Is sts enabled on the cluster
	selAppNamespaces    []string //The app namespaces should be collected and verified
	selNamespacesID     []string // The UUIDs of all app namespaces should be collected
	disAppNamespaces    []string //The namespaces should not be collected and verified
}

// Set the default values to the cloudwatchSpec Object, you need to change the default in It if needs
func (cw *cloudwatchSpec) init(oc *exutil.CLI) {
	cred, err := getAWSCredentials(oc)
	if err != nil {
		g.Skip("Skip since no AWS credetials.")
	}
	factory, err := NewAWSClientFactory(context.TODO(), cred)
	if err != nil {
		e2e.Failf("error loading aws config: %v", err)
	}
	cw.awsRegion = cred.Region
	if checkNetworkType(oc) == "ovnkubernetes" {
		cw.ovnEnabled = true
	}
	cw.hasMaster = hasMaster(oc)
	cw.clusterPlatformType = compat_otp.CheckPlatform(oc)
	if cw.clusterPlatformType == "aws" {
		if compat_otp.IsSTSCluster(oc) {
			cw.stsEnabled = true
			//Note: AWS China is not added, and the partition is `aws-cn`.
			if strings.HasPrefix(cw.awsRegion, "us-gov") {
				cw.awsPartition = "aws-us-gov"
			} else {
				cw.awsPartition = "aws"
			}
			cw.iamClient = factory.IAM()
			stsClient := factory.STS()
			accountID, _ := getAwsAccount(stsClient)
			oidcProvider, e := getOIDC(oc)
			o.Expect(e).NotTo(o.HaveOccurred())
			//Create IAM roles for cloudwatch
			cw.createIAMCloudwatchRole(oc, accountID, oidcProvider)
		}
	}
	cw.cwClient = factory.CloudWatchLogs()
	if !cw.stsEnabled {
		err = loadAccessKeySecretKeyFromFile(context.TODO(), &cred)
		if err != nil {
			e2e.Failf("can't get aws_access_key_id and aws_secret_access_key: %v", err)
		}
		os.Setenv("AWS_ACCESS_KEY_ID", cred.AccessKey)
		os.Setenv("AWS_SECRET_ACCESS_KEY", cred.SecretKey)
	}
	e2e.Logf("Init cloudwatchSpec done")
}

func (cw *cloudwatchSpec) setGroupName(groupName string) {
	cw.groupName = groupName
}

func (cw *cloudwatchSpec) newIamRole(accountID, oidcProvider string) {
	trustPolicy := `{
"Version": "2012-10-17",
 "Statement": [
   {
     "Effect": "Allow",
     "Principal": {
       "Federated": "arn:%s:iam::%s:oidc-provider/%s"
     },
     "Action": "sts:AssumeRoleWithWebIdentity",
     "Condition": {
       "StringEquals": {
         "%s:sub": "system:serviceaccount:%s:%s"
       }
     }
   }
 ]
}`
	trustPolicy = fmt.Sprintf(trustPolicy, cw.awsPartition, accountID, oidcProvider, oidcProvider, cw.secretNamespace, cw.collectorSAName)
	cw.awsRoleArn = iamCreateRole(cw.iamClient, trustPolicy, cw.awsRoleName)
}

func (cw *cloudwatchSpec) newIamPolicy() {
	mgmtPolicy := `{
"Version": "2012-10-17",
"Statement": [
     {
         "Effect": "Allow",
         "Action": [
            "logs:CreateLogGroup",
            "logs:CreateLogStream",
            "logs:DescribeLogGroups",
            "logs:DescribeLogStreams",
            "logs:PutLogEvents",
            "logs:PutRetentionPolicy"
         ],
         "Resource": "arn:%s:logs:*:*:*"
     }
   ]
}`
	cw.awsPolicyArn = iamCreatePolicy(cw.iamClient, fmt.Sprintf(mgmtPolicy, cw.awsPartition), cw.awsPolicyName)
}

func (cw *cloudwatchSpec) createIAMCloudwatchRole(oc *exutil.CLI, accountID, oidcProvider string) {
	if os.Getenv("AWS_CLOUDWATCH_ROLE_ARN") != "" {
		cw.awsRoleArn = os.Getenv("AWS_CLOUDWATCH_ROLE_ARN")
		return
	}
	cw.awsRoleName = cw.secretName + "-" + getInfrastructureName(oc)
	cw.awsPolicyName = cw.awsRoleName
	e2e.Logf("Creating aws iam role: %s", cw.awsRoleName)
	cw.newIamRole(accountID, oidcProvider)
	cw.newIamPolicy()
	_, err := cw.iamClient.AttachRolePolicy(context.TODO(), &iam.AttachRolePolicyInput{
		PolicyArn: &cw.awsPolicyArn,
		RoleName:  &cw.awsRoleName,
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to attach policy to iam role "+cw.awsRoleName)
}

// Create Cloudwatch Secret. note: use credential files can avoid leak in output
func (cw *cloudwatchSpec) createClfSecret(oc *exutil.CLI) {
	var err error
	if cw.stsEnabled {
		token, _ := oc.AsAdmin().WithoutNamespace().Run("create").Args("token", cw.collectorSAName, "--audience=openshift", "--duration=24h", "-n", cw.secretNamespace).Output()
		err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", cw.secretName, "--from-literal=role_arn="+cw.awsRoleArn, "--from-literal=token="+token, "-n", cw.secretNamespace).Execute()
	} else {
		err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", cw.secretName, "--from-literal=aws_access_key_id="+os.Getenv("AWS_ACCESS_KEY_ID"), "--from-literal=aws_secret_access_key="+os.Getenv("AWS_SECRET_ACCESS_KEY"), "-n", cw.secretNamespace).Execute()
	}
	o.Expect(err).NotTo(o.HaveOccurred())
}

// trigger DeleteLogGroup. sometimes, the api return success, but the resource are still there. now wait up to 3 minutes to make the delete success as more as possible.
func (cw *cloudwatchSpec) deleteGroups(groupPrefix string) {
	wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 90*time.Second, true, func(context.Context) (done bool, err error) {
		logGroupNames, _ := cw.getLogGroupNames(groupPrefix)
		if len(logGroupNames) == 0 {
			return true, nil
		}
		for _, name := range logGroupNames {
			_, err := cw.cwClient.DeleteLogGroup(context.TODO(), &cloudwatchlogs.DeleteLogGroupInput{LogGroupName: &name})
			if err != nil {
				e2e.Logf("Can't delete log group: %s", name)
			} else {
				e2e.Logf("Log group %s is deleted", name)
			}
		}
		return false, nil
	})
}

// clean the Cloudwatch resources
func (cw *cloudwatchSpec) deleteResources(oc *exutil.CLI) {
	resource{"secret", cw.secretName, cw.secretNamespace}.clear(oc)
	cw.deleteGroups("")
	//delete roles when the role is created in case
	if cw.stsEnabled && os.Getenv("AWS_CLOUDWATCH_ROLE_ARN") == "" {
		deleteIAMroleonAWS(cw.iamClient, cw.awsRoleName)
		iamDeletePolicy(cw.iamClient, cw.awsPolicyArn)
	}
}

// Return Cloudwatch GroupNames
func (cw cloudwatchSpec) getLogGroupNames(groupPrefix string) ([]string, error) {
	var (
		groupNames []string
	)
	if groupPrefix == "" {
		if strings.Contains(cw.groupName, "{") {
			groupPrefix = strings.Split(cw.groupName, "{")[0]
		} else {
			groupPrefix = cw.groupName
		}
	}
	logGroupDesc, err := cw.cwClient.DescribeLogGroups(context.TODO(), &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: &groupPrefix,
	})
	if err != nil {
		return groupNames, fmt.Errorf("can't get log groups from cloudwatch: %v", err)
	}
	for _, group := range logGroupDesc.LogGroups {
		groupNames = append(groupNames, *group.LogGroupName)
	}

	nextToken := logGroupDesc.NextToken
	for nextToken != nil {
		logGroupDesc, err = cw.cwClient.DescribeLogGroups(context.TODO(), &cloudwatchlogs.DescribeLogGroupsInput{
			LogGroupNamePrefix: &groupPrefix,
			NextToken:          nextToken,
		})
		if err != nil {
			return groupNames, fmt.Errorf("can't get log groups from cloudwatch: %v", err)
		}
		for _, group := range logGroupDesc.LogGroups {
			groupNames = append(groupNames, *group.LogGroupName)
		}
		nextToken = logGroupDesc.NextToken
	}
	return groupNames, nil
}

func (cw *cloudwatchSpec) waitForLogGroupsAppear(groupPrefix, keyword string) error {
	if groupPrefix == "" {
		if strings.Contains(cw.groupName, "{") {
			groupPrefix = strings.Split(cw.groupName, "{")[0]
		} else {
			groupPrefix = cw.groupName
		}
	}
	err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		groups, err := cw.getLogGroupNames(groupPrefix)
		if err != nil {
			e2e.Logf("error getting log groups: %v", err)
			return false, nil
		}
		if len(groups) == 0 {
			e2e.Logf("no log groups match the prefix: %s", groupPrefix)
			return false, nil
		}
		e2e.Logf("the log group names %v", groups)
		if keyword != "" {
			return containSubstring(groups, keyword), nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("can't find log groups with prefix: %s", groupPrefix)
	}
	return nil
}

// Get Stream names matching the logTypes and project names.
func (cw *cloudwatchSpec) getLogStreamNames(groupName string, streamPrefix string) ([]string, error) {
	var (
		logStreamNames  []string
		err             error
		logStreamDesc   *cloudwatchlogs.DescribeLogStreamsOutput
		logStreamsInput cloudwatchlogs.DescribeLogStreamsInput
	)

	if streamPrefix == "" {
		logStreamsInput = cloudwatchlogs.DescribeLogStreamsInput{
			LogGroupName: &groupName,
		}
	} else {
		logStreamsInput = cloudwatchlogs.DescribeLogStreamsInput{
			LogGroupName:        &groupName,
			LogStreamNamePrefix: &streamPrefix,
		}
	}
	logStreamDesc, err = cw.cwClient.DescribeLogStreams(context.TODO(), &logStreamsInput)
	if err != nil {
		return logStreamNames, fmt.Errorf("can't get log streams: %v", err)
	}
	for _, stream := range logStreamDesc.LogStreams {
		logStreamNames = append(logStreamNames, *stream.LogStreamName)
	}

	nextToken := logStreamDesc.NextToken
	for nextToken != nil {
		if streamPrefix == "" {
			logStreamsInput = cloudwatchlogs.DescribeLogStreamsInput{
				LogGroupName: &groupName,
				NextToken:    nextToken,
			}
		} else {
			logStreamsInput = cloudwatchlogs.DescribeLogStreamsInput{
				LogGroupName:        &groupName,
				LogStreamNamePrefix: &streamPrefix,
				NextToken:           nextToken,
			}
		}
		logStreamDesc, err = cw.cwClient.DescribeLogStreams(context.TODO(), &logStreamsInput)
		if err != nil {
			return logStreamNames, fmt.Errorf("can't get log streams from cloudwatch: %v", err)
		}
		for _, stream := range logStreamDesc.LogStreams {
			logStreamNames = append(logStreamNames, *stream.LogStreamName)
		}
		nextToken = logStreamDesc.NextToken
	}
	return logStreamNames, nil
}

// In this function, verify if the infra container logs are forwarded to Cloudwatch or not
func (cw *cloudwatchSpec) checkInfraContainerLogs(strict bool) bool {
	var (
		infraLogGroupNames []string
		logStreams         []string
	)
	logGroupNames, err := cw.getLogGroupNames("")
	o.Expect(err).NotTo(o.HaveOccurred())
	if len(logGroupNames) == 0 {
		return false
	}
	if strings.Contains(cw.groupName, "{.log_type") {
		for _, e := range logGroupNames {
			r, _ := regexp.Compile(`.*\.infrastructure$`)
			match := r.MatchString(e)
			if match {
				infraLogGroupNames = append(infraLogGroupNames, e)
			}
		}
	} else {
		infraLogGroupNames = logGroupNames
	}
	e2e.Logf("the possible log group names for infra container logs are %v", infraLogGroupNames)

	// get all the log streams under the log groups
	for _, group := range infraLogGroupNames {
		streams, _ := cw.getLogStreamNames(group, "")
		for _, stream := range streams {
			if strings.Contains(stream, ".openshift-") {
				logStreams = append(logStreams, stream)
			}
		}
	}

	// when strict=true, return ture if we can find podLogStream for all nodes
	if strict {
		if len(cw.nodes) == 0 {
			e2e.Logf("node name is empty, please get node names at first")
			return false
		}
		for _, node := range cw.nodes {
			if !containSubstring(logStreams, node+".openshift-") {
				e2e.Logf("can't find log stream %s", node+".openshift-")
				return false
			}
		}
		return true
	} else {
		return len(logStreams) > 0
	}
}

// list streams, check streams, provide the log streams in this function?
// In this function, verify the system logs present on Cloudwatch
func (cw *cloudwatchSpec) checkInfraNodeLogs(strict bool) bool {
	var (
		infraLogGroupNames []string
		logStreams         []string
	)
	logGroupNames, err := cw.getLogGroupNames("")
	if err != nil || len(logGroupNames) == 0 {
		return false
	}
	if strings.Contains(cw.groupName, ".log_type") {
		for _, group := range logGroupNames {
			r, _ := regexp.Compile(`.*\.infrastructure$`)
			match := r.MatchString(group)
			if match {
				infraLogGroupNames = append(infraLogGroupNames, group)
			}
		}
	} else {
		infraLogGroupNames = logGroupNames
	}
	e2e.Logf("the infra node log group names are %v", infraLogGroupNames)

	// get all the log streams under the log groups
	for _, group := range infraLogGroupNames {
		streams, _ := cw.getLogStreamNames(group, "")
		for _, stream := range streams {
			if strings.Contains(stream, ".journal.system") {
				logStreams = append(logStreams, stream)
			}
		}
	}
	e2e.Logf("the infrastructure node log streams: %v", logStreams)
	// when strict=true, return ture if we can find log streams from all nodes
	if strict {
		var expectedStreamNames []string
		if len(cw.nodes) == 0 {
			e2e.Logf("node name is empty, please get node names at first")
			return false
		}
		//stream name: ip-10-0-152-69.journal.system
		if cw.clusterPlatformType == "aws" {
			for _, node := range cw.nodes {
				expectedStreamNames = append(expectedStreamNames, strings.Split(node, ".")[0])
			}
		} else {
			expectedStreamNames = append(expectedStreamNames, cw.nodes...)
		}
		for _, name := range expectedStreamNames {
			streamName := name + ".journal.system"
			if !contain(logStreams, streamName) {
				e2e.Logf("can't find log stream %s", streamName)
				return false
			}
		}
		return true
	} else {
		return len(logStreams) > 0
	}
}

// In this function, verify the system logs present on Cloudwatch
func (cw *cloudwatchSpec) infrastructureLogsFound(strict bool) bool {
	return cw.checkInfraContainerLogs(strict) && cw.checkInfraNodeLogs(strict)
}

/*
In this function, verify all type of audit logs can be found.
when strict=false, test pass when all type of audit logs are found
when strict=true,  test pass if any audit log is found.
stream:
ip-10-0-90-156.us-east-2.compute.internal
*/
func (cw *cloudwatchSpec) auditLogsFound(strict bool) bool {
	var (
		auditLogGroupNames []string
		logStreams         []string
	)

	if len(cw.nodes) == 0 {
		e2e.Logf("node name is empty, please get node names at first")
		return false
	}

	logGroupNames, err := cw.getLogGroupNames("")
	if err != nil || len(logGroupNames) == 0 {
		return false
	}
	if strings.Contains(cw.groupName, ".log_type") {
		for _, e := range logGroupNames {
			r, _ := regexp.Compile(`.*\.audit$`)
			match := r.MatchString(e)
			if match {
				auditLogGroupNames = append(auditLogGroupNames, e)
			}
		}
	} else {
		auditLogGroupNames = logGroupNames
	}
	e2e.Logf("the possible log group names for audit logs are %v", auditLogGroupNames)

	// stream name: ip-10-0-74-46.us-east-2.compute.internal
	// get all the log streams under the log groups
	for _, group := range auditLogGroupNames {
		streams, _ := cw.getLogStreamNames(group, "")
		logStreams = append(logStreams, streams...)
	}
	// when strict=true, return ture if we can find podLogStream for all nodes
	if strict {
		for _, node := range cw.nodes {
			if !containSubstring(logStreams, node) {
				e2e.Logf("can't find log stream from node: %s", node)
				return false
			}
		}
		return true
	} else {
		for _, node := range cw.nodes {
			if containSubstring(logStreams, node) {
				return true
			}
		}
	}
	return false
}

// check if the container logs are grouped by namespace_id
func (cw *cloudwatchSpec) checkLogGroupByNamespaceID() bool {
	var (
		groupPrefix string
	)

	if strings.Contains(cw.groupName, ".kubernetes.namespace_id") {
		groupPrefix = strings.Split(cw.groupName, "{")[0]
	} else {
		e2e.Logf("the group name doesn't contain .kubernetes.namespace_id, no need to call this function")
		return false
	}
	for _, namespaceID := range cw.selNamespacesID {
		groupErr := cw.waitForLogGroupsAppear(groupPrefix, namespaceID)
		if groupErr != nil {
			e2e.Logf("can't find log group named %s", namespaceID)
			return false
		}
	}
	return true
}

// check if the container logs are grouped by namespace_name
func (cw *cloudwatchSpec) checkLogGroupByNamespaceName() bool {
	var (
		groupPrefix string
	)
	if strings.Contains(cw.groupName, ".kubernetes.namespace_name") {
		groupPrefix = strings.Split(cw.groupName, "{")[0]
	} else {
		e2e.Logf("the group name doesn't contain .kubernetes.namespace_name, no need to call this function")
		return false
	}
	for _, namespaceName := range cw.selAppNamespaces {
		groupErr := cw.waitForLogGroupsAppear(groupPrefix, namespaceName)
		if groupErr != nil {
			e2e.Logf("can't find log group named %s", namespaceName)
			return false
		}
	}
	for _, ns := range cw.disAppNamespaces {
		groups, err := cw.getLogGroupNames(groupPrefix)
		if err != nil {
			return false
		}
		if containSubstring(groups, ns) {
			return false
		}
	}
	return true
}

func (cw *cloudwatchSpec) getApplicationLogStreams() ([]string, error) {
	var (
		appLogGroupNames []string
		logStreams       []string
	)

	logGroupNames, err := cw.getLogGroupNames("")
	if err != nil || len(logGroupNames) == 0 {
		return logStreams, err
	}
	if strings.Contains(cw.groupName, "{.log_type") {
		for _, e := range logGroupNames {
			r, _ := regexp.Compile(`.*\.application$`)
			match := r.MatchString(e)
			if match {
				appLogGroupNames = append(appLogGroupNames, e)
			}
		}
	} else {
		appLogGroupNames = logGroupNames
	}
	e2e.Logf("the possible log group names for application logs are %v", appLogGroupNames)

	for _, group := range appLogGroupNames {
		streams, _ := cw.getLogStreamNames(group, "")
		for _, stream := range streams {
			if !strings.Contains(stream, "ip-10-0") {
				logStreams = append(logStreams, stream)
			}
		}
	}
	return logStreams, nil
}

// The index to find application logs
// GroupType
//
//	logType: anli48022-gwbb4.application
//	namespaceName:  anli48022-gwbb4.aosqe-log-json-1638788875
//	namespaceUUID:   anli48022-gwbb4.0471c739-e38c-4590-8a96-fdd5298d47ae,uuid.audit,uuid.infrastructure
func (cw *cloudwatchSpec) applicationLogsFound() bool {
	if (len(cw.selAppNamespaces) > 0 || len(cw.disAppNamespaces) > 0) && strings.Contains(cw.groupName, ".kubernetes.namespace_id") {
		return cw.checkLogGroupByNamespaceName()
	}
	if len(cw.selNamespacesID) > 0 {
		return cw.checkLogGroupByNamespaceID()
	}

	logStreams, err := cw.getApplicationLogStreams()
	if err != nil || len(logStreams) == 0 {
		return false
	}
	for _, ns := range cw.selAppNamespaces {
		if !containSubstring(logStreams, ns) {
			e2e.Logf("can't find logs from project %s", ns)
			return false
		}
	}
	for _, ns := range cw.disAppNamespaces {
		if containSubstring(logStreams, ns) {
			e2e.Logf("find logs from project %s, this is not expected", ns)
			return false
		}
	}
	return true
}

// The common function to verify if logs can be found or not. In general, customized the cloudwatchSpec before call this function
func (cw *cloudwatchSpec) logsFound() bool {
	var (
		appLogSuccess   = true
		infraLogSuccess = true
		auditLogSuccess = true
	)

	for _, logType := range cw.logTypes {
		switch logType {
		case "infrastructure":
			err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				return cw.infrastructureLogsFound(true), nil
			})
			if err != nil {
				e2e.Logf("can't find infrastructure in given time")
				infraLogSuccess = false
			}
		case "audit":
			err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				return cw.auditLogsFound(false), nil
			})
			if err != nil {
				e2e.Logf("can't find audit logs in given time")
				auditLogSuccess = false
			}
		case "application":
			err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				return cw.applicationLogsFound(), nil
			})
			if err != nil {
				e2e.Logf("can't find application logs in given time")
				appLogSuccess = false
			}
		}
	}
	return infraLogSuccess && auditLogSuccess && appLogSuccess
}

func (cw *cloudwatchSpec) getLogRecordsByNamespace(limit int32, logGroupName string, namespaceName string) ([]LogEntity, error) {
	var (
		output *cloudwatchlogs.FilterLogEventsOutput
		logs   []LogEntity
	)

	streamNames, streamErr := cw.getLogStreamNames(logGroupName, namespaceName)
	if streamErr != nil {
		return logs, streamErr
	}
	e2e.Logf("the log streams: %v", streamNames)
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		output, err = cw.filterLogEvents(limit, logGroupName, "", streamNames...)
		if err != nil {
			e2e.Logf("get error when filter events in cloudwatch, try next time")
			return false, nil
		}
		if len(output.Events) == 0 {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("the query is not completed in 5 minutes or there is no log record matches the query: %v", err)
	}
	for _, event := range output.Events {
		var log LogEntity
		json.Unmarshal([]byte(*event.Message), &log)
		logs = append(logs, log)
	}

	return logs, nil
}

// aws logs filter-log-events --log-group-name logging-47052-qitang-fips-zfpgd.application --log-stream-name-prefix=var.log.pods.e2e-test-logfwd-namespace-x8mzw
func (cw *cloudwatchSpec) filterLogEvents(limit int32, logGroupName, logStreamNamePrefix string, logStreamNames ...string) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	if len(logStreamNamePrefix) > 0 && len(logStreamNames) > 0 {
		return nil, fmt.Errorf("invalidParameterException: logStreamNamePrefix and logStreamNames are specified")
	}
	var (
		err    error
		output *cloudwatchlogs.FilterLogEventsOutput
	)

	if len(logStreamNamePrefix) > 0 {
		output, err = cw.cwClient.FilterLogEvents(context.TODO(), &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:        &logGroupName,
			LogStreamNamePrefix: &logStreamNamePrefix,
			Limit:               &limit,
		})
	} else if len(logStreamNames) > 0 {
		output, err = cw.cwClient.FilterLogEvents(context.TODO(), &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:   &logGroupName,
			LogStreamNames: logStreamNames,
			Limit:          &limit,
		})
	}
	return output, err
}

type S3Output struct {
	BucketName              string
	StsEnabled              bool   // Is sts enabled on the cluster
	Region                  string // the region where the bucket is located in
	RoleName                string
	RoleArn                 string
	Client                  *s3.Client
	IAMClient               *iam.Client
	STSClient               *sts.Client
	KeyPrefix               string // the S3 key prefix for log objects
	SecretName              string // the name of the secret for the collector to use
	CollectorNamespace      string
	CollectorServiceAccount string
}

func (s3 *S3Output) Init(oc *exutil.CLI) error {
	cred, err := getAWSCredentials(oc)
	if err != nil {
		g.Skip("Skip since no AWS credetials.")
	}
	factory, err := NewAWSClientFactory(context.TODO(), cred)
	if err != nil {
		return fmt.Errorf("error loading aws config: %v", err)
	}
	s3.Region = cred.Region

	if compat_otp.CheckPlatform(oc) == "aws" {
		if compat_otp.IsSTSCluster(oc) {
			s3.StsEnabled = true
			partition := "aws"
			//Note: AWS China is not added, and the partition is `aws-cn`.
			if strings.HasPrefix(s3.Region, "us-gov") {
				partition = "aws-us-gov"
			}
			s3.IAMClient = factory.IAM()
			s3.STSClient = factory.STS()
			awsAccountID, _ := getAwsAccount(s3.STSClient)
			oidcName, err := getOIDC(oc)
			if err != nil {
				return err
			}
			s3.RoleName = s3.BucketName + "-" + getRandomString()
			s3.RoleArn = createIAMRoleForS3Bucket(s3.IAMClient, oidcName, awsAccountID, partition, s3.CollectorNamespace, s3.CollectorServiceAccount, s3.RoleName)
			//token, _ := oc.AsAdmin().WithoutNamespace().Run("create").Args("token", s3.CollectorServiceAccount, "--audience=openshift", "--duration=24h", "-n", s3.CollectorNamespace).Output()
			//err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", s3.SecretName, "--from-literal=role_arn="+s3.RoleArn, "--from-literal=token="+token, "-n", s3.CollectorNamespace).Execute()
			err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", s3.SecretName, "--from-literal=role_arn="+s3.RoleArn, "-n", s3.CollectorNamespace).Execute()
			if err != nil {
				return err
			}
		}
	}
	s3.Client = factory.S3()
	err = createS3Bucket(s3.Client, s3.BucketName, s3.Region)
	if err != nil {
		return err
	}
	if !s3.StsEnabled {
		err = loadAccessKeySecretKeyFromFile(context.TODO(), &cred)
		if err != nil {
			return fmt.Errorf("can't get aws_access_key_id and aws_secret_access_key: %v", err)
		}
		err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", s3.SecretName, "--from-literal=aws_access_key_id="+cred.AccessKey, "--from-literal=aws_secret_access_key="+cred.SecretKey, "-n", s3.CollectorNamespace).Execute()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s3 *S3Output) Destroy(oc *exutil.CLI) {
	if s3.StsEnabled {
		deleteIAMroleonAWS(s3.IAMClient, s3.RoleName)
	}
	o.Expect(deleteS3Bucket(s3.Client, s3.BucketName)).NotTo(o.HaveOccurred())
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("secret", s3.SecretName, "-n", s3.CollectorNamespace).Execute()
}
