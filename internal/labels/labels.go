package labels

const (
	// Component Labels

	ComponentCLO          = "component:clo"
	ComponentLokiOperator = "component:loki-operator"

	// Scope Labels

	ScopeInterop = "scope:interop"

	// Gate Labels

	GatePreMerge = "gate:premerge"

	// Execution Labels

	ExecSlow       = "exec:slow"
	ExecSerial     = "exec:serial"
	ExecDisruptive = "exec:disruptive"

	// Release Labels

	ReleaseLogging67 = "6.7"

	// Collector Feature Labels

	FilterDrop = "filter:drop"
)

// BracketTags maps each bracket tag to the ginkgo label that must accompany
// it. Suite qualifiers select on the labels while origin's own suites
// (conformance parents, serial/parallel split) still read the tags from test
// names, so the two must agree; the drift guard in
// test/e2e/labels_drift_test.go fails when they do not. Labels without a
// bracket-tag counterpart (jira:*, release, filter:*, requires:*) do not
// belong here.
var BracketTags = map[string]string{
	"[PreMerge]":     GatePreMerge,
	"[CLO]":          ComponentCLO,
	"[LokiOperator]": ComponentLokiOperator,
	"[InterOps]":     ScopeInterop,
	"[Serial]":       ExecSerial,
	"[Disruptive]":   ExecDisruptive,
	"[Slow]":         ExecSlow,
}

// Jira returns a label referencing a Jira issue, e.g. jira:LOG-9876.
func Jira(key string) string { return "jira:" + key }
