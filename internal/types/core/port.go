package core

// Ports defines a single entry in the 'ports' list under 'egress' and 'ingress'.
type Port struct {
	Port     string `yaml:"port"`
	Protocol string `yaml:"protocol"`
}
