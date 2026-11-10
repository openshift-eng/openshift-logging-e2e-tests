package viaq

// LogEntity the entity of log data
type LogEntity struct {
	Kubernetes struct {
		Annotations       map[string]string `json:"annotations,omitempty"`
		ContainerID       string            `json:"container_id,omitempty"`
		ContainerImage    string            `json:"container_image"`
		ContainerImageID  string            `json:"container_image_id,omitempty"`
		ContainerIOStream string            `json:"container_iostream,omitempty"`
		ContainerName     string            `json:"container_name"`
		FlatLabels        []string          `json:"flat_labels"`
		Host              string            `json:"host"`
		Lables            map[string]string `json:"labels,omitempty"`
		MasterURL         string            `json:"master_url,omitempty"`
		NamespaceID       string            `json:"namespace_id"`
		NamespaceLabels   map[string]string `json:"namespace_labels,omitempty"`
		NamespaceName     string            `json:"namespace_name"`
		PodID             string            `json:"pod_id"`
		PodIP             string            `json:"pod_ip,omitempty"`
		PodName           string            `json:"pod_name"`
		PodOwner          string            `json:"pod_owner"`
	} `json:"kubernetes,omitzero"`
	Systemd struct {
		SystemdT struct {
			SystemdInvocationID string `json:"SYSTEMD_INVOCATION_ID"`
			BootID              string `json:"BOOT_ID"`
			GID                 string `json:"GID"`
			CmdLine             string `json:"CMDLINE"`
			PID                 string `json:"PID"`
			SystemSlice         string `json:"SYSTEMD_SLICE"`
			SelinuxContext      string `json:"SELINUX_CONTEXT"`
			UID                 string `json:"UID"`
			StreamID            string `json:"STREAM_ID"`
			Transport           string `json:"TRANSPORT"`
			Comm                string `json:"COMM"`
			EXE                 string
			SystemdUnit         string `json:"SYSTEMD_UNIT"`
			CapEffective        string `json:"CAP_EFFECTIVE"`
			MachineID           string `json:"MACHINE_ID"`
			SystemdCgroup       string `json:"SYSTEMD_CGROUP"`
		} `json:"t"`
		SystemdU struct {
			SyslogIdntifier string `json:"SYSLOG_IDENTIFIER"`
			SyslogFacility  string `json:"SYSLOG_FACILITY"`
		} `json:"u"`
	} `json:"systemd,omitzero"`
	ViaqMsgID string `json:"viaq_msg_id,omitempty"`
	Level     string `json:"level"`
	LogSource string `json:"log_source"`
	LogType   string `json:"log_type,omitempty"`
	Message   string `json:"message"`
	Docker    struct {
		ContainerID string `json:"container_id"`
	} `json:"docker,omitzero"`
	HostName  string `json:"hostname"`
	TimeStamp string `json:"@timestamp"`
	File      string `json:"file,omitempty"`
	OpenShift struct {
		ClusterID string            `json:"cluster_id,omitempty"`
		Sequence  int64             `json:"sequence"`
		Labels    map[string]string `json:"labels,omitempty"`
	} `json:"openshift,omitzero"`
	PipelineMetadata struct {
		Collector struct {
			ReceivedAt string `json:"received_at"`
			Name       string `json:"name"`
			InputName  string `json:"inputname"`
			Version    string `json:"version"`
			IPaddr4    string `json:"ipaddr4"`
		} `json:"collector"`
	} `json:"pipeline_metadata,omitzero"`
	Structured struct {
		Level        string `json:"level,omitempty"`
		StringNumber string `json:"StringNumber,omitempty"`
		Message      string `json:"message,omitempty"`
		Number       int    `json:"Number,omitempty"`
		Layer1       string `json:"Layer1,omitempty"`
		FooColonBar  string `json:"foo:bar,omitempty"`
		FooDotBar    string `json:"foo.bar,omitempty"`
		BraceItem    string `json:"{foobar},omitempty"`
		BracketItem  string `json:"[foobar],omitempty"`
		Layer2       struct {
			Name string `json:"name,omitempty"`
			Tips string `json:"tips,omitempty"`
		} `json:"layer2,omitzero"`
	} `json:"structured,omitzero"`
}
