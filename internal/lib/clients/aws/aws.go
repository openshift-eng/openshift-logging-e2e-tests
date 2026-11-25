package aws

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/compat_otp"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/env"
)

// ClientConfig stores the AWS credentials and client configurations
type ClientConfig struct {
	Endpoint        string // Custom URL for MinIO/ODF
	Region          string
	AccessKey       string
	SecretKey       string
	Profile         string // Profile name in credentials file
	CredsFilePath   string // Path to custom credentials file
	InsecureSkipTLS bool   // For self-signed certs
}

// ClientFactory stores the shared base configuration.
type ClientFactory struct {
	Config         aws.Config
	CustomEndpoint string
}

func Credentials(oc *exutil.CLI) (ClientConfig, error) {
	var region string
	platform := compat_otp.CheckPlatform(oc)
	if platform == "aws" {
		rawRegion, err := compat_otp.GetAWSClusterRegion(oc)
		if err != nil {
			return ClientConfig{}, fmt.Errorf("can't get AWS region: %w", err)
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
			return ClientConfig{CredsFilePath: awsCredFile, Region: region}, nil
		}
	}

	awsSharedCredentialsFile, present := os.LookupEnv("AWS_SHARED_CREDENTIALS_FILE")
	if present {
		e2e.Logf("use AWS_SHARED_CREDENTIALS_FILE")
		return ClientConfig{CredsFilePath: awsSharedCredentialsFile, Region: region}, nil
	}

	accessKey, accessKeyExist := os.LookupEnv("AWS_ACCESS_KEY_ID")
	secretKey, secretKeyExist := os.LookupEnv("AWS_SECRET_ACCESS_KEY")
	if accessKeyExist && secretKeyExist {
		e2e.Logf("use AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
		return ClientConfig{AccessKey: accessKey, SecretKey: secretKey, Region: region}, nil
	}

	if platform == "aws" && !compat_otp.IsWorkloadIdentityCluster(oc) {
		dirname := "/tmp/" + oc.Namespace() + "-creds"
		defer os.RemoveAll(dirname)
		err := os.MkdirAll(dirname, 0o777)
		if err != nil {
			return ClientConfig{}, fmt.Errorf("error creating directory %s: %w", dirname, err)
		}
		err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("secret/aws-creds", "-n", "kube-system", "--confirm", "--to="+dirname).Execute()
		if err != nil {
			return ClientConfig{}, fmt.Errorf("failed to extract secret/aws-creds: %w", err)
		}

		accessKeyID, err := os.ReadFile(dirname + "/aws_access_key_id")
		if err != nil {
			return ClientConfig{}, fmt.Errorf("failed to read file aws_access_key_id: %w", err)
		}
		secretAccessKey, err := os.ReadFile(dirname + "/aws_secret_access_key")
		if err != nil {
			return ClientConfig{}, fmt.Errorf("failed to read file aws_secret_access_key: %w", err)
		}
		e2e.Logf("use credentials from cluster")
		return ClientConfig{AccessKey: string(accessKeyID), SecretKey: string(secretAccessKey), Region: region}, nil
	}

	home, _ := os.UserHomeDir()
	if _, err := os.Stat(home + "/.aws/credentials"); err == nil {
		e2e.Logf("use $HOME/.aws/credentials")
		return ClientConfig{CredsFilePath: home + "/.aws/credentials", Profile: "default", Region: region}, nil
	}
	return ClientConfig{}, fmt.Errorf("can't get aws credentials")
}

func LoadAccessKeySecretKeyFromFile(ctx context.Context, cfg *ClientConfig) error {
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

func NewClientFactory(ctx context.Context, cfg ClientConfig) (*ClientFactory, error) {
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

	loadOptions = append(loadOptions, config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired))

	// Load the SDK Configuration
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &ClientFactory{
		Config:         awsCfg,
		CustomEndpoint: cfg.Endpoint,
	}, nil
}

// S3 returns an S3 client. Enables PathStyle if a custom endpoint is used.
func (f *ClientFactory) S3() *s3.Client {
	return s3.NewFromConfig(f.Config, func(o *s3.Options) {
		if f.CustomEndpoint != "" {
			o.BaseEndpoint = aws.String(f.CustomEndpoint)
			o.UsePathStyle = true
		}
	})
}

// IAM returns an IAM client.
func (f *ClientFactory) IAM() *iam.Client {
	return iam.NewFromConfig(f.Config, func(o *iam.Options) {
		if f.CustomEndpoint != "" {
			o.BaseEndpoint = aws.String(f.CustomEndpoint)
		}
	})
}

// CloudWatchLogs returns a CloudWatchLogs client.
func (f *ClientFactory) CloudWatchLogs() *cloudwatchlogs.Client {
	return cloudwatchlogs.NewFromConfig(f.Config, func(o *cloudwatchlogs.Options) {
		if f.CustomEndpoint != "" {
			o.BaseEndpoint = aws.String(f.CustomEndpoint)
		}
	})
}

// STS returns an STS client.
func (f *ClientFactory) STS() *sts.Client {
	return sts.NewFromConfig(f.Config, func(o *sts.Options) {
		if f.CustomEndpoint != "" {
			o.BaseEndpoint = aws.String(f.CustomEndpoint)
		}
	})
}

// Account returns the account ID and the AWS ARN associated with the calling entity.
func Account(stsClient *sts.Client) (string, string) {
	e2e.Logf("Running Account")
	result, err := stsClient.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
	o.Expect(err).NotTo(o.HaveOccurred())
	return aws.ToString(result.Account), aws.ToString(result.Arn)
}

// IamCreateRole creates a role on AWS and returns the role ARN
func IamCreateRole(iamClient *iam.Client, trustPolicy string, roleName string) string {
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

func IamCreatePolicy(iamClient *iam.Client, mgmtPolicy string, policyName string) string {
	e2e.Logf("Create iam policy %v", policyName)
	result, err := iamClient.CreatePolicy(context.TODO(), &iam.CreatePolicyInput{
		PolicyDocument: aws.String(mgmtPolicy),
		PolicyName:     aws.String(policyName),
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "Couldn't create policy"+policyName)
	return aws.ToString(result.Policy.Arn)
}

// IamDeletePolicy aws iam delete-policy
func IamDeletePolicy(iamClient *iam.Client, policyArn string) {
	_, err := iamClient.DeletePolicy(context.TODO(), &iam.DeletePolicyInput{
		PolicyArn: aws.String(policyArn),
	})
	if err != nil {
		e2e.Logf("Couldn't delete policy %v: %v", policyArn, err)
	}
}

// CreateIAMRoleOnAWS This func creates a IAM role, attaches custom trust policy and managed permission policy
func CreateIAMRoleOnAWS(iamClient *iam.Client, trustPolicy string, roleName string, policyArn string) string {
	roleArn := IamCreateRole(iamClient, trustPolicy, roleName)
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

// DeleteIAMroleonAWS IAM role and attached policies
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
