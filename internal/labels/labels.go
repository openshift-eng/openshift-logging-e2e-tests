package labels

const (
	ComponentCLO = "component:clo"
	ExecSlow     = "exec:slow"

	// Release Labels

	ReleaseLogging67 = "6.7"

	// Collector Feature Labels

	FilterDrop = "filter:drop"
)

// Jira returns a label referencing a Jira issue, e.g. jira:LOG-9876.
func Jira(key string) string { return "jira:" + key }
