package aws

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/random"
)

// minioStorageRequested reports whether LOGGING_TEST_STORAGE=minio asks for the MinIO of the cluster as the
// object storage, whatever the platform is.
func minioStorageRequested() bool {
	return os.Getenv("LOGGING_TEST_STORAGE") == "minio"
}

func CreateS3Bucket(client *s3.Client, bucketName, region string) error {
	// check if the bucket exists or not
	// if exists, clear all the objects in the bucket
	// if not, create the bucket
	exist := false
	// The route of a MinIO that was just deployed can answer 503 (the HTML page of the router) for a while after
	// its pod is ready, so the first calls are retried. Other storage is not warming up, so its error is final.
	var buckets *s3.ListBucketsOutput
	var err error
	for i := 1; i <= 18; i++ {
		buckets, err = client.ListBuckets(context.TODO(), &s3.ListBucketsInput{})
		if err == nil || !minioStorageRequested() {
			break
		}
		e2e.Logf("ListBuckets failed (try %d/18): %v", i, err)
		time.Sleep(10 * time.Second)
	}
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

	// Delete objects one by one. DeleteObjects needs a checksum header, and some MinIO versions
	// reject the CRC32 checksum that the SDK sends (MissingContentMD5).
	for _, object := range objects.Contents {
		_, err = client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
			Bucket: &bucketName,
			Key:    object.Key,
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
	cred, err := Credentials(oc)
	if err != nil {
		g.Skip("Skip since no AWS credetials.")
	}
	factory, err := NewClientFactory(context.TODO(), cred)
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
			awsAccountID, _ := Account(s3.STSClient)
			oidcName, err := cluster.OIDCProvider(oc)
			if err != nil {
				return err
			}
			s3.RoleName = s3.BucketName + "-" + random.String()
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

// TODO move me?
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
