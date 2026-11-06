package receivers

// Receiver is a sink for log records. It accepts records forwarded by a
// ClusterLogForwarder writes them to the receiver by a configured output.
//
//	type Receiver struct {
//		name      string // the name of the receiver, it's also used to name the cm/deployment/svc
//		namespace string // the namespace where the receiver is deployed in
//	}
type Receiver interface {
	// Deploy the receiver. Optional template parameters override the
	// receiver's template defaults, e.g. Deploy("OLDER_THAN=<RFC3339>").
	Deploy(optionalParameters ...string)

	// Remove the deployed receiver
	Remove()

	// PodName of the first deployed pod
	PodName() string

	// ExecCmd runs a shell command in the receiver pod and returns its output.
	// Transient exec transport failures are retried because the verification polls
	// depend on it and must not flake on a single failed roundtrip.
	ExecCmd(cmd string) (string, error)

	// Namespace of the receiver
	Namespace() string

	// Name of the receiver deployment
	Name() string

	// ContainerLogs retrieves the logs from the given namespace
	ContainerLogs(namespace string) ([]string, error)

	// ContainerRecordCount returns the number of records received from the
	// given namespace. The count runs in the receiver pod, so the file content
	// is never transferred. Cheap to poll.
	ContainerRecordCount(namespace string) (int, error)

	// HasContainerLogsWith reports whether any record received from the given
	// namespace matches the regex pattern, a POSIX extended regular expression
	// (ERE). The match runs in the receiver pod, so the file content is never
	// transferred. Cheap to poll.
	HasContainerLogsWith(namespace, regexPattern string) (bool, error)

	// JournalLogs ingested by the receiver
	JournalLogs() ([]string, error)

	// HasJournalLogs reports whether the journal file exists and is non-empty,
	// without reading its content. Cheap to poll.
	HasJournalLogs() (bool, error)

	// AuditLogs ingested by the receiver
	AuditLogs() ([]string, error)

	// HasAuditLogs reports whether the audit file exists and is non-empty,
	// without reading its content. Cheap to poll.
	HasAuditLogs() (bool, error)
}
