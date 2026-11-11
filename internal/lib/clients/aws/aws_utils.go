package aws

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
	"github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/clients/cluster"
	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/env"
	librandom "github.com/openshift/openshift-logging-e2e-tests/internal/lib/random"
	libstrings "github.com/openshift/openshift-logging-e2e-tests/internal/lib/strings"
	internaltypes "github.com/openshift/openshift-logging-e2e-tests/internal/types"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/viaq"
)

func GetAWSCredentials(oc *exutil.CLI) (AWSClientConfig, error) {
	var region string
	platform := compat_otp.CheckPlatform(oc)
	if platform == "aws" {
		rawRegion, err := compat_otp.GetAWSClusterRegion(oc)
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("can't get AWS region: %w", err)
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
		err := os.MkdirAll(dirname, 0o777)
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("error creating directory %s: %w", dirname, err)
		}
		err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("secret/aws-creds", "-n", "kube-system", "--confirm", "--to="+dirname).Execute()
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("failed to extract secret/aws-creds: %w", err)
		}

		accessKeyID, err := os.ReadFile(dirname + "/aws_access_key_id")
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("failed to read file aws_access_key_id: %w", err)
		}
		secretAccessKey, err := os.ReadFile(dirname + "/aws_secret_access_key")
		if err != nil {
			return AWSClientConfig{}, fmt.Errorf("failed to read file aws_secret_access_key: %w", err)
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

func LoadAccessKeySecretKeyFromFile(ctx context.Context, cfg *AWSClientConfig) error {
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
		proxy := env.Proxy()
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

func CreateS3Bucket(client *s3.Client, bucketName, region string) error {
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
		return EmptyS3Bucket(client, bucketName)
	}

	/*
		Per https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucket.html#API_CreateBucket_RequestBody,
		us-east-1 is the default region and it's not a valid value of LocationConstraint,
		using `LocationConstraint: types.BucketLocationConstraint("us-east-1")` gets error `InvalidLocationConstraint`.
		Here remove the configuration when the region is us-east-1
	*/
	if len(region) == 0 || region == "us-east-1" {
		_, err = client.CreateBucket(context.TODO(), &s3.CreateBucketInput{Bucket: &bucketName})
		return err
	}
	_, err = client.CreateBucket(context.TODO(), &s3.CreateBucketInput{Bucket: &bucketName, CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}})
	return err
}

func DeleteS3Bucket(client *s3.Client, bucketName string) error {
	// empty bucket
	err := EmptyS3Bucket(client, bucketName)
	if err != nil {
		return err
	}
	// delete bucket
	_, err = client.DeleteBucket(context.TODO(), &s3.DeleteBucketInput{Bucket: &bucketName})
	return err
}

func EmptyS3Bucket(client *s3.Client, bucketName string) error {
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
		return EmptyS3Bucket(client, bucketName)
	}

	return nil
}

// GetAwsAccount returns the account ID and the AWS ARN associated with the calling entity.
func GetAwsAccount(stsClient *sts.Client) (string, string) {
	e2e.Logf("Running GetAwsAccount")
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

// IamDeleteRole deletes the role from AWS
func IamDeleteRole(iamClient *iam.Client, roleName string) {
	_, err := iamClient.DeleteRole(context.TODO(), &iam.DeleteRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		e2e.Logf("Couldn't delete role %s: %v", roleName, err)
	}
}

// aws iam create-policy
func IamCreatePolicy(iamClient *iam.Client, mgmtPolicy string, policyName string) string {
	e2e.Logf("Create iam policy %v", policyName)
	result, err := iamClient.CreatePolicy(context.TODO(), &iam.CreatePolicyInput{
		PolicyDocument: aws.String(mgmtPolicy),
		PolicyName:     aws.String(policyName),
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "Couldn't create policy"+policyName)
	return aws.ToString(result.Policy.Arn)
}

// aws iam delete-policy
func IamDeletePolicy(iamClient *iam.Client, policyArn string) {
	_, err := iamClient.DeletePolicy(context.TODO(), &iam.DeletePolicyInput{
		PolicyArn: aws.String(policyArn),
	})
	if err != nil {
		e2e.Logf("Couldn't delete policy %v: %v", policyArn, err)
	}
}

// This func creates a IAM role, attaches custom trust policy and managed permission policy
func CreateIAMRoleOnAWS(iamClient *iam.Client, trustPolicy string, roleName string, policyArn string) string {
	roleArn := iamCreateRole(iamClient, trustPolicy, roleName)
	// Adding managed permission policy if provided
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
func DeleteIAMroleonAWS(iamClient *iam.Client, roleName string) {
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
	IamDeleteRole(iamClient, roleName)
}

// CreateIAMRoleForS3Bucket creates role required for s3 bucket on STS clusters and returns the roleArn
func CreateIAMRoleForS3Bucket(iamClient *iam.Client, oidcName, awsAccountID, partition, serviceAccountNamespace, serviceAccountName, roleName string) string {
	e2e.Logf("Running CreateIAMRoleForS3Bucket")
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
	return CreateIAMRoleOnAWS(iamClient, s3BucketTrustPolicy, roleName, policyArn)
}

// Creates Loki object storage secret on AWS STS cluster
func CreateObjectStorageSecretOnAWSSTSCluster(oc *exutil.CLI, region, storageSecret, bucketName, namespace string) {
	err := oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", storageSecret, "--from-literal=region="+region, "--from-literal=bucketnames="+bucketName, "--from-literal=audience=openshift", "-n", namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

// Function to check if tenant logs are present under the S3 bucket.
// Returns success if any one of the tenants under tenants[] are found.
func ValidatesIfLogsArePushedToS3Bucket(s3Client *s3.Client, bucketName string, tenants []string) {
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
type CloudwatchSpec struct {
	AwsRoleName         string
	AwsRoleArn          string
	AwsRegion           string
	AwsPolicyName       string
	AwsPolicyArn        string
	AwsPartition        string // The partition in which the resource is located, valid when the cluster is STS, ref: https://docs.aws.amazon.com/IAM/latest/UserGuide/reference-arns.html#arns-syntax
	ClusterPlatformType string
	CollectorSAName     string // the service account for collector pod to use
	CwClient            *cloudwatchlogs.Client
	GroupName           string // the strategy for grouping logstreams, for example: '{.log_type||"none"}'
	HasMaster           bool   // whether the cluster has master Nodes or not
	IamClient           *iam.Client
	LogTypes            []string // default: "['infrastructure','application', 'audit']"
	Nodes               []string // Cluster Nodes Names, required when checking infrastructure/audit logs and strict=true
	OvnEnabled          bool     // if ovn is enabled
	SecretName          string   // the name of the secret for the collector to use
	SecretNamespace     string   // the namespace where the collector pods to be deployed
	StsEnabled          bool     // Is sts enabled on the cluster
	SelAppNamespaces    []string // The app namespaces should be collected and verified
	SelNamespacesID     []string // The UUIDs of all app namespaces should be collected
	DisAppNamespaces    []string // The namespaces should not be collected and verified
}

// Set the default values to the CloudwatchSpec Object, you need to change the default in It if needs
func (cw *CloudwatchSpec) Init(oc *exutil.CLI) {
	cred, err := GetAWSCredentials(oc)
	if err != nil {
		g.Skip("Skip since no AWS credetials.")
	}
	factory, err := NewAWSClientFactory(context.TODO(), cred)
	if err != nil {
		e2e.Failf("error loading aws config: %v", err)
	}
	cw.AwsRegion = cred.Region
	if cluster.NetworkType(oc) == "ovnkubernetes" {
		cw.OvnEnabled = true
	}
	cw.HasMaster = cluster.HasMaster(oc)
	cw.ClusterPlatformType = compat_otp.CheckPlatform(oc)
	if cw.ClusterPlatformType == "aws" {
		if compat_otp.IsSTSCluster(oc) {
			cw.StsEnabled = true
			// Note: AWS China is not added, and the partition is `aws-cn`.
			if strings.HasPrefix(cw.AwsRegion, "us-gov") {
				cw.AwsPartition = "aws-us-gov"
			} else {
				cw.AwsPartition = "aws"
			}
			cw.IamClient = factory.IAM()
			stsClient := factory.STS()
			accountID, _ := GetAwsAccount(stsClient)
			oidcProvider, e := cluster.OIDCProvider(oc)
			o.Expect(e).NotTo(o.HaveOccurred())
			// Create IAM roles for cloudwatch
			cw.CreateIAMCloudwatchRole(oc, accountID, oidcProvider)
		}
	}
	cw.CwClient = factory.CloudWatchLogs()
	if !cw.StsEnabled {
		err = LoadAccessKeySecretKeyFromFile(context.TODO(), &cred)
		if err != nil {
			e2e.Failf("can't get aws_access_key_id and aws_secret_access_key: %v", err)
		}
		os.Setenv("AWS_ACCESS_KEY_ID", cred.AccessKey)
		os.Setenv("AWS_SECRET_ACCESS_KEY", cred.SecretKey)
	}
	e2e.Logf("Init CloudwatchSpec done")
}

func (cw *CloudwatchSpec) SetGroupName(groupName string) {
	cw.GroupName = groupName
}

func (cw *CloudwatchSpec) NewIamRole(accountID, oidcProvider string) {
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
	trustPolicy = fmt.Sprintf(trustPolicy, cw.AwsPartition, accountID, oidcProvider, oidcProvider, cw.SecretNamespace, cw.CollectorSAName)
	cw.AwsRoleArn = iamCreateRole(cw.IamClient, trustPolicy, cw.AwsRoleName)
}

func (cw *CloudwatchSpec) NewIamPolicy() {
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
	cw.AwsPolicyArn = IamCreatePolicy(cw.IamClient, fmt.Sprintf(mgmtPolicy, cw.AwsPartition), cw.AwsPolicyName)
}

func (cw *CloudwatchSpec) CreateIAMCloudwatchRole(oc *exutil.CLI, accountID, oidcProvider string) {
	if os.Getenv("AWS_CLOUDWATCH_ROLE_ARN") != "" {
		cw.AwsRoleArn = os.Getenv("AWS_CLOUDWATCH_ROLE_ARN")
		return
	}
	cw.AwsRoleName = cw.SecretName + "-" + cluster.InfrastructureName(oc)
	cw.AwsPolicyName = cw.AwsRoleName
	e2e.Logf("Creating aws iam role: %s", cw.AwsRoleName)
	cw.NewIamRole(accountID, oidcProvider)
	cw.NewIamPolicy()
	_, err := cw.IamClient.AttachRolePolicy(context.TODO(), &iam.AttachRolePolicyInput{
		PolicyArn: &cw.AwsPolicyArn,
		RoleName:  &cw.AwsRoleName,
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to attach policy to iam role "+cw.AwsRoleName)
}

// Create Cloudwatch Secret. note: use credential files can avoid leak in output
func (cw *CloudwatchSpec) CreateClfSecret(oc *exutil.CLI) {
	var err error
	if cw.StsEnabled {
		token, _ := oc.AsAdmin().WithoutNamespace().Run("create").Args("token", cw.CollectorSAName, "--audience=openshift", "--duration=24h", "-n", cw.SecretNamespace).Output()
		err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", cw.SecretName, "--from-literal=role_arn="+cw.AwsRoleArn, "--from-literal=token="+token, "-n", cw.SecretNamespace).Execute()
	} else {
		err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", cw.SecretName, "--from-literal=aws_access_key_id="+os.Getenv("AWS_ACCESS_KEY_ID"), "--from-literal=aws_secret_access_key="+os.Getenv("AWS_SECRET_ACCESS_KEY"), "-n", cw.SecretNamespace).Execute()
	}
	o.Expect(err).NotTo(o.HaveOccurred())
}

// trigger DeleteLogGroup. sometimes, the api return success, but the resource are still there. now wait up to 3 minutes to make the delete success as more as possible.
func (cw *CloudwatchSpec) DeleteGroups(groupPrefix string) {
	wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 90*time.Second, true, func(context.Context) (done bool, err error) {
		logGroupNames, _ := cw.GetLogGroupNames(groupPrefix)
		if len(logGroupNames) == 0 {
			return true, nil
		}
		for _, name := range logGroupNames {
			_, err := cw.CwClient.DeleteLogGroup(context.TODO(), &cloudwatchlogs.DeleteLogGroupInput{LogGroupName: &name})
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
func (cw *CloudwatchSpec) DeleteResources(oc *exutil.CLI) {
	_ = internaltypes.NewResource("secret", cw.SecretName, cw.SecretNamespace).Clear(oc)
	cw.DeleteGroups("")
	// delete roles when the role is created in case
	if cw.StsEnabled && os.Getenv("AWS_CLOUDWATCH_ROLE_ARN") == "" {
		DeleteIAMroleonAWS(cw.IamClient, cw.AwsRoleName)
		IamDeletePolicy(cw.IamClient, cw.AwsPolicyArn)
	}
}

// Return Cloudwatch GroupNames
func (cw CloudwatchSpec) GetLogGroupNames(groupPrefix string) ([]string, error) {
	var groupNames []string
	if groupPrefix == "" {
		if strings.Contains(cw.GroupName, "{") {
			groupPrefix = strings.Split(cw.GroupName, "{")[0]
		} else {
			groupPrefix = cw.GroupName
		}
	}
	logGroupDesc, err := cw.CwClient.DescribeLogGroups(context.TODO(), &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: &groupPrefix,
	})
	if err != nil {
		return groupNames, fmt.Errorf("can't get log groups from cloudwatch: %w", err)
	}
	for _, group := range logGroupDesc.LogGroups {
		groupNames = append(groupNames, *group.LogGroupName)
	}

	nextToken := logGroupDesc.NextToken
	for nextToken != nil {
		logGroupDesc, err = cw.CwClient.DescribeLogGroups(context.TODO(), &cloudwatchlogs.DescribeLogGroupsInput{
			LogGroupNamePrefix: &groupPrefix,
			NextToken:          nextToken,
		})
		if err != nil {
			return groupNames, fmt.Errorf("can't get log groups from cloudwatch: %w", err)
		}
		for _, group := range logGroupDesc.LogGroups {
			groupNames = append(groupNames, *group.LogGroupName)
		}
		nextToken = logGroupDesc.NextToken
	}
	return groupNames, nil
}

func (cw *CloudwatchSpec) WaitForLogGroupsAppear(groupPrefix, keyword string) error {
	if groupPrefix == "" {
		if strings.Contains(cw.GroupName, "{") {
			groupPrefix = strings.Split(cw.GroupName, "{")[0]
		} else {
			groupPrefix = cw.GroupName
		}
	}
	err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		groups, err := cw.GetLogGroupNames(groupPrefix)
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
			return strings.Contains(strings.Join(groups, ","), keyword), nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("can't find log groups with prefix: %s", groupPrefix)
	}
	return nil
}

// Get Stream names matching the LogTypes and project names.
func (cw *CloudwatchSpec) GetLogStreamNames(groupName string, streamPrefix string) ([]string, error) {
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
	logStreamDesc, err = cw.CwClient.DescribeLogStreams(context.TODO(), &logStreamsInput)
	if err != nil {
		return logStreamNames, fmt.Errorf("can't get log streams: %w", err)
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
		logStreamDesc, err = cw.CwClient.DescribeLogStreams(context.TODO(), &logStreamsInput)
		if err != nil {
			return logStreamNames, fmt.Errorf("can't get log streams from cloudwatch: %w", err)
		}
		for _, stream := range logStreamDesc.LogStreams {
			logStreamNames = append(logStreamNames, *stream.LogStreamName)
		}
		nextToken = logStreamDesc.NextToken
	}
	return logStreamNames, nil
}

// In this function, verify if the infra container logs are forwarded to Cloudwatch or not
func (cw *CloudwatchSpec) CheckInfraContainerLogs(strict bool) bool {
	var (
		infraLogGroupNames []string
		logStreams         []string
	)
	logGroupNames, err := cw.GetLogGroupNames("")
	o.Expect(err).NotTo(o.HaveOccurred())
	if len(logGroupNames) == 0 {
		return false
	}
	if strings.Contains(cw.GroupName, "{.log_type") {
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
		streams, _ := cw.GetLogStreamNames(group, "")
		for _, stream := range streams {
			if strings.Contains(stream, ".openshift-") {
				logStreams = append(logStreams, stream)
			}
		}
	}

	// when strict=true, return ture if we can find podLogStream for all Nodes
	if strict {
		if len(cw.Nodes) == 0 {
			e2e.Logf("node name is empty, please get node names at first")
			return false
		}
		for _, node := range cw.Nodes {
			if !libstrings.AnyContainSubstring(logStreams, node+".openshift-") {
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
func (cw *CloudwatchSpec) CheckInfraNodeLogs(strict bool) bool {
	var (
		infraLogGroupNames []string
		logStreams         []string
	)
	logGroupNames, err := cw.GetLogGroupNames("")
	if err != nil || len(logGroupNames) == 0 {
		return false
	}
	if strings.Contains(cw.GroupName, ".log_type") {
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
		streams, _ := cw.GetLogStreamNames(group, "")
		for _, stream := range streams {
			if strings.Contains(stream, ".journal.system") {
				logStreams = append(logStreams, stream)
			}
		}
	}
	e2e.Logf("the infrastructure node log streams: %v", logStreams)
	// when strict=true, return ture if we can find log streams from all Nodes
	if strict {
		var expectedStreamNames []string
		if len(cw.Nodes) == 0 {
			e2e.Logf("node name is empty, please get node names at first")
			return false
		}
		// stream name: ip-10-0-152-69.journal.system
		if cw.ClusterPlatformType == "aws" {
			for _, node := range cw.Nodes {
				expectedStreamNames = append(expectedStreamNames, strings.Split(node, ".")[0])
			}
		} else {
			expectedStreamNames = append(expectedStreamNames, cw.Nodes...)
		}
		for _, name := range expectedStreamNames {
			streamName := name + ".journal.system"
			if !libstrings.AnyContainSubstring(logStreams, streamName) {
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
func (cw *CloudwatchSpec) InfrastructureLogsFound(strict bool) bool {
	return cw.CheckInfraContainerLogs(strict) && cw.CheckInfraNodeLogs(strict)
}

/*
In this function, verify all type of audit logs can be found.
when strict=false, test pass when all type of audit logs are found
when strict=true,  test pass if any audit log is found.
stream:
ip-10-0-90-156.us-east-2.compute.internal
*/
func (cw *CloudwatchSpec) AuditLogsFound(strict bool) bool {
	var (
		auditLogGroupNames []string
		logStreams         []string
	)

	if len(cw.Nodes) == 0 {
		e2e.Logf("node name is empty, please get node names at first")
		return false
	}

	logGroupNames, err := cw.GetLogGroupNames("")
	if err != nil || len(logGroupNames) == 0 {
		return false
	}
	if strings.Contains(cw.GroupName, ".log_type") {
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
		streams, _ := cw.GetLogStreamNames(group, "")
		logStreams = append(logStreams, streams...)
	}
	// when strict=true, return ture if we can find podLogStream for all Nodes
	if strict {
		for _, node := range cw.Nodes {
			if !libstrings.AnyContainSubstring(logStreams, node) {
				e2e.Logf("can't find log stream from node: %s", node)
				return false
			}
		}
		return true
	} else {
		for _, node := range cw.Nodes {
			if libstrings.AnyContainSubstring(logStreams, node) {
				return true
			}
		}
	}
	return false
}

// check if the container logs are grouped by namespace_id
func (cw *CloudwatchSpec) CheckLogGroupByNamespaceID() bool {
	var groupPrefix string

	if strings.Contains(cw.GroupName, ".kubernetes.namespace_id") {
		groupPrefix = strings.Split(cw.GroupName, "{")[0]
	} else {
		e2e.Logf("the group name doesn't contain .kubernetes.namespace_id, no need to call this function")
		return false
	}
	for _, namespaceID := range cw.SelNamespacesID {
		groupErr := cw.WaitForLogGroupsAppear(groupPrefix, namespaceID)
		if groupErr != nil {
			e2e.Logf("can't find log group named %s", namespaceID)
			return false
		}
	}
	return true
}

// check if the container logs are grouped by namespace_name
func (cw *CloudwatchSpec) CheckLogGroupByNamespaceName() bool {
	var groupPrefix string
	if strings.Contains(cw.GroupName, ".kubernetes.namespace_name") {
		groupPrefix = strings.Split(cw.GroupName, "{")[0]
	} else {
		e2e.Logf("the group name doesn't contain .kubernetes.namespace_name, no need to call this function")
		return false
	}
	for _, namespaceName := range cw.SelAppNamespaces {
		groupErr := cw.WaitForLogGroupsAppear(groupPrefix, namespaceName)
		if groupErr != nil {
			e2e.Logf("can't find log group named %s", namespaceName)
			return false
		}
	}
	for _, ns := range cw.DisAppNamespaces {
		groups, err := cw.GetLogGroupNames(groupPrefix)
		if err != nil {
			return false
		}
		if libstrings.AnyContainSubstring(groups, ns) {
			return false
		}
	}
	return true
}

func (cw *CloudwatchSpec) GetApplicationLogStreams() ([]string, error) {
	var (
		appLogGroupNames []string
		logStreams       []string
	)

	logGroupNames, err := cw.GetLogGroupNames("")
	if err != nil || len(logGroupNames) == 0 {
		return logStreams, err
	}
	if strings.Contains(cw.GroupName, "{.log_type") {
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
		streams, _ := cw.GetLogStreamNames(group, "")
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
func (cw *CloudwatchSpec) ApplicationLogsFound() bool {
	if (len(cw.SelAppNamespaces) > 0 || len(cw.DisAppNamespaces) > 0) && strings.Contains(cw.GroupName, ".kubernetes.namespace_id") {
		return cw.CheckLogGroupByNamespaceName()
	}
	if len(cw.SelNamespacesID) > 0 {
		return cw.CheckLogGroupByNamespaceID()
	}

	logStreams, err := cw.GetApplicationLogStreams()
	if err != nil || len(logStreams) == 0 {
		return false
	}
	for _, ns := range cw.SelAppNamespaces {
		if !libstrings.AnyContainSubstring(logStreams, ns) {
			e2e.Logf("can't find logs from project %s", ns)
			return false
		}
	}
	for _, ns := range cw.DisAppNamespaces {
		if libstrings.AnyContainSubstring(logStreams, ns) {
			e2e.Logf("find logs from project %s, this is not expected", ns)
			return false
		}
	}
	return true
}

// The common function to verify if logs can be found or not. In general, customized the CloudwatchSpec before call this function
func (cw *CloudwatchSpec) LogsFound() bool {
	var (
		appLogSuccess   = true
		infraLogSuccess = true
		auditLogSuccess = true
	)

	for _, logType := range cw.LogTypes {
		switch logType {
		case "infrastructure":
			err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				return cw.InfrastructureLogsFound(true), nil
			})
			if err != nil {
				e2e.Logf("can't find infrastructure in given time")
				infraLogSuccess = false
			}
		case "audit":
			err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				return cw.AuditLogsFound(false), nil
			})
			if err != nil {
				e2e.Logf("can't find audit logs in given time")
				auditLogSuccess = false
			}
		case "application":
			err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				return cw.ApplicationLogsFound(), nil
			})
			if err != nil {
				e2e.Logf("can't find application logs in given time")
				appLogSuccess = false
			}
		}
	}
	return infraLogSuccess && auditLogSuccess && appLogSuccess
}

func (cw *CloudwatchSpec) GetLogRecordsByNamespace(limit int32, logGroupName string, namespaceName string) ([]viaq.LogEntity, error) {
	var (
		output *cloudwatchlogs.FilterLogEventsOutput
		logs   []viaq.LogEntity
	)

	streamNames, streamErr := cw.GetLogStreamNames(logGroupName, namespaceName)
	if streamErr != nil {
		return logs, streamErr
	}
	e2e.Logf("the log streams: %v", streamNames)
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		output, err = cw.FilterLogEvents(limit, logGroupName, "", streamNames...)
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
		return nil, fmt.Errorf("the query is not completed in 5 minutes or there is no log record matches the query: %w", err)
	}
	for _, event := range output.Events {
		var log viaq.LogEntity
		if err := json.Unmarshal([]byte(*event.Message), &log); err != nil {
			return nil, fmt.Errorf("can't parse log event from cloudwatch: %w", err)
		}
		logs = append(logs, log)
	}

	return logs, nil
}

// aws logs filter-log-events --log-group-name logging-47052-qitang-fips-zfpgd.application --log-stream-name-prefix=var.log.pods.e2e-test-logfwd-namespace-x8mzw
func (cw *CloudwatchSpec) FilterLogEvents(limit int32, logGroupName, logStreamNamePrefix string, logStreamNames ...string) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	if len(logStreamNamePrefix) > 0 && len(logStreamNames) > 0 {
		return nil, fmt.Errorf("invalidParameterException: logStreamNamePrefix and logStreamNames are specified")
	}
	var (
		err    error
		output *cloudwatchlogs.FilterLogEventsOutput
	)

	if len(logStreamNamePrefix) > 0 {
		output, err = cw.CwClient.FilterLogEvents(context.TODO(), &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:        &logGroupName,
			LogStreamNamePrefix: &logStreamNamePrefix,
			Limit:               &limit,
		})
	} else if len(logStreamNames) > 0 {
		output, err = cw.CwClient.FilterLogEvents(context.TODO(), &cloudwatchlogs.FilterLogEventsInput{
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
	cred, err := GetAWSCredentials(oc)
	if err != nil {
		g.Skip("Skip since no AWS credetials.")
	}
	factory, err := NewAWSClientFactory(context.TODO(), cred)
	if err != nil {
		return fmt.Errorf("error loading aws config: %w", err)
	}
	s3.Region = cred.Region

	if compat_otp.CheckPlatform(oc) == "aws" {
		if compat_otp.IsSTSCluster(oc) {
			s3.StsEnabled = true
			partition := "aws"
			// Note: AWS China is not added, and the partition is `aws-cn`.
			if strings.HasPrefix(s3.Region, "us-gov") {
				partition = "aws-us-gov"
			}
			s3.IAMClient = factory.IAM()
			s3.STSClient = factory.STS()
			awsAccountID, _ := GetAwsAccount(s3.STSClient)
			oidcName, err := cluster.OIDCProvider(oc)
			if err != nil {
				return err
			}
			s3.RoleName = s3.BucketName + "-" + librandom.String()
			s3.RoleArn = CreateIAMRoleForS3Bucket(s3.IAMClient, oidcName, awsAccountID, partition, s3.CollectorNamespace, s3.CollectorServiceAccount, s3.RoleName)
			//token, _ := oc.AsAdmin().WithoutNamespace().Run("create").Args("token", s3.CollectorServiceAccount, "--audience=openshift", "--duration=24h", "-n", s3.CollectorNamespace).Output()
			//err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", s3.SecretName, "--from-literal=role_arn="+s3.RoleArn, "--from-literal=token="+token, "-n", s3.CollectorNamespace).Execute()
			err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", s3.SecretName, "--from-literal=role_arn="+s3.RoleArn, "-n", s3.CollectorNamespace).Execute()
			if err != nil {
				return err
			}
		}
	}
	s3.Client = factory.S3()
	err = CreateS3Bucket(s3.Client, s3.BucketName, s3.Region)
	if err != nil {
		return err
	}
	if !s3.StsEnabled {
		err = LoadAccessKeySecretKeyFromFile(context.TODO(), &cred)
		if err != nil {
			return fmt.Errorf("can't get aws_access_key_id and aws_secret_access_key: %w", err)
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
		DeleteIAMroleonAWS(s3.IAMClient, s3.RoleName)
	}
	o.Expect(DeleteS3Bucket(s3.Client, s3.BucketName)).NotTo(o.HaveOccurred())
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("secret", s3.SecretName, "-n", s3.CollectorNamespace).Execute()
}
