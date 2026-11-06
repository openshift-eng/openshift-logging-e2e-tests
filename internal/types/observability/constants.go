package observability

const (
	NamespaceClusterLoggingOperator = "openshift-logging"
	NamespaceLokiOperator           = "openshift-operators-redhat"

	PackageClusterLogging = "cluster-logging"
	OperatorName          = "cluster-logging-operator"

	ApiGroupClusterLogForwarder = "clusterlogforwarders.observability.openshift.io"

	ClusterRoleCollectApplicationLogs    = "collect-application-logs"
	ClusterRoleCollectAuditLogs          = "collect-audit-logs"
	ClusterRoleCollectInfrastructureLogs = "collect-infrastructure-logs"
)
