package aws

import (
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
)

// AWSClientConfig stores the AWS credentials and client configurations
type AWSClientConfig struct {
	Endpoint        string // Custom URL for MinIO/ODF
	Region          string
	AccessKey       string
	SecretKey       string
	Profile         string // Profile name in credentials file
	CredsFilePath   string // Path to custom credentials file
	InsecureSkipTLS bool   // For self-signed certs
}

// AWSClientFactory stores the shared base configuration.
type AWSClientFactory struct {
	Config         awssdk.Config
	CustomEndpoint string
}
