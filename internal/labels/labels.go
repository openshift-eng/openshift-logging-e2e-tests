package labels

const (
	ComponentCLO = "component:clo"
	ExecSlow     = "exec:slow"

	// Collector Feature Labels

	FilterDrop = "filter:drop"
)

// Jira returns a label referencing a Jira issue, e.g. jira:LOG-9876.
func Jira(key string) string { return "jira:" + key }
