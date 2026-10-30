package receivers

// Receiver is a sink for log records. It accepts records forwarded by a
// ClusterLogForwarder writes them to the receiver by a configured output.
//
//	type Receiver struct {
//		name      string // the name of the receiver, it's also used to name the cm/deployment/svc
//		namespace string // the namespace where the receiver is deployed in
//	}
type Receiver interface {

	// Deploy the receiver
	Deploy()

	// Remove the deployed receiver
	Remove()

	// PodName of the deployed receiver
	PodName() string

	// ExecCmd runs a shell command in the receiver pod and returns its output.
	// Transient exec transport failures are retried because the verification polls
	// depend on it and must not flake on a single failed roundtrip.
	ExecCmd(cmd string) (string, error)

	Namespace() string
	Name() string

	//ContainerLogs retrieves the logs from the given namespace
	ContainerLogs(namespace string) ([]string, error)

	JournalLogs() ([]string, error)
	AuditLogs() ([]string, error)
}
