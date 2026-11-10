package networkpolicy

import (
	"fmt"

	"github.com/openshift/openshift-logging-e2e-tests/internal/types/core"
	exutil "github.com/openshift/origin/test/extended/util"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	Kind = "networkpolicy"
)

// NetworkPolicy defines the complete network policy object
type NetworkPolicy struct {
	metav1.ObjectMeta `yaml:"metadata"`
	Spec              struct {
		Egress []struct {
			Ports []core.Port         `yaml:"ports,omitempty"`
			To    []NetworkPolicyPeer `yaml:"to,omitempty"`
		} `yaml:"egress,omitempty"`
		Ingress []struct {
			Ports []core.Port         `yaml:"ports,omitempty"`
			From  []NetworkPolicyPeer `yaml:"from,omitempty"`
		} `yaml:"ingress,omitempty"`
		PodSelector struct {
			MatchLabels      map[string]string `yaml:"matchLabels,omitempty"`
			MatchExpressions []struct {
				Key      string   `yaml:"key"`
				Operator string   `yaml:"operator"`
				Values   []string `yaml:"values,omitempty"`
			} `yaml:"matchExpressions,omitempty"`
		} `yaml:"podSelector"`
		PolicyTypes []string `yaml:"policyTypes"`
	} `yaml:"spec"`
}

// NetworkPolicyPeer defines the peer in the network policy (for From/To)
type NetworkPolicyPeer struct {
	NamespaceSelector *struct {
		MatchLabels map[string]string `yaml:"matchLabels,omitempty"`
	} `yaml:"namespaceSelector,omitempty"`
	PodSelector *struct {
		MatchLabels      map[string]string `yaml:"matchLabels,omitempty"`
		MatchExpressions []struct {
			Key      string   `yaml:"key"`
			Operator string   `yaml:"operator"`
			Values   []string `yaml:"values,omitempty"`
		} `yaml:"matchExpressions,omitempty"`
	} `yaml:"podSelector,omitempty"`
}

func Get(oc *exutil.CLI, namespace, name string) (*NetworkPolicy, error) {
	var netpol *NetworkPolicy
	np, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(Kind, "-n", namespace, name, "-oyaml").Output()
	if err != nil {
		return netpol, fmt.Errorf("error getting networkpolicy %s: %w", name, err)
	}

	err = yaml.Unmarshal([]byte(np), &netpol)
	if err != nil {
		return netpol, fmt.Errorf("error unmarshalling YAML: %w", err)
	}
	return netpol, nil
}
