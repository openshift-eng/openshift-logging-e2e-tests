package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/clients/cluster"
	libstrings "github.com/openshift/openshift-logging-e2e-tests/internal/lib/strings"
	internaltypes "github.com/openshift/openshift-logging-e2e-tests/internal/types"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/viaq"
)

// CloudwatchSpec the basic object which describe all common test options
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

// Init Set the default values to the CloudwatchSpec Object, you need to change the default in It if needs
func (cw *CloudwatchSpec) Init(oc *exutil.CLI) {
	cred, err := Credentials(oc)
	if err != nil {
		g.Skip("Skip since no AWS credetials.")
	}
	factory, err := NewClientFactory(context.TODO(), cred)
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
			accountID, _ := Account(stsClient)
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
	cw.AwsRoleArn = IamCreateRole(cw.IamClient, trustPolicy, cw.AwsRoleName)
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

// CreateClfSecret Cloudwatch Secret. note: use credential files can avoid leak in output
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

// DeleteGroups trigger DeleteLogGroup. sometimes, the api return success, but the resource are still there. now wait up to 3 minutes to make the delete success as more as possible.
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
func (cw *CloudwatchSpec) GetLogGroupNames(groupPrefix string) ([]string, error) {
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
			if !slices.Contains(logStreams, streamName) {
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
