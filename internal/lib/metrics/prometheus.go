package metrics

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"

	o "github.com/onsi/gomega"
	libnethttp "github.com/openshift/openshift-logging-e2e-tests/internal/lib/net/http"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/core/serviceaccount"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/openshift"
	exutil "github.com/openshift/origin/test/extended/util"
)

var (
	//go:embed role.yaml
	RbacPrometheus string
)

// metric the prometheus metric
type Metric struct {
	Metric struct {
		Name              string `json:"__name__"`
		Cluster           string `json:"cluster,omitempty"`
		Container         string `json:"container,omitempty"`
		ContainerName     string `json:"containername,omitempty"`
		Endpoint          string `json:"endpoint,omitempty"`
		Instance          string `json:"instance,omitempty"`
		Job               string `json:"job,omitempty"`
		Namespace         string `json:"namespace,omitempty"`
		Path              string `json:"path,omitempty"`
		Pod               string `json:"pod,omitempty"`
		PodName           string `json:"podname,omitempty"`
		Service           string `json:"service,omitempty"`
		ExportedNamespace string `json:"exported_namespace,omitempty"`
		State             string `json:"state,omitempty"`
	} `json:"metric"`
	Value []interface{} `json:"value"`
}

// alert the pending/firing alert
type Alert struct {
	Labels struct {
		AlertName string `json:"alertname,omitempty"`
		Condition string `json:"condition,omitempty"`
		Endpoint  string `json:"endpoint,omitempty"`
		Namespace string `json:"namespace,omitempty"`
		Pod       string `json:"pod,omitempty"`
		Instance  string `json:"instance,omitempty"`
		Severity  string `json:"severity,omitempty"`
	} `json:"labels,omitempty"`
	Annotations struct {
		Message    string `json:"message,omitempty"`
		RunBookURL string `json:"runbook_url,omitempty"`
		Summary    string `json:"summary,omitempty"`
	} `json:"annotations,omitempty"`
	State    string `json:"state,omitempty"`
	ActiveAt string `json:"activeAt,omitempty"`
	Value    string `json:"value,omitempty"`
}

type PrometheusQueryResult struct {
	Data struct {
		Result     []Metric `json:"result"`
		ResultType string   `json:"resultType"`
		Alerts     []Alert  `json:"alerts,omitempty"`
	} `json:"data"`
	Status string `json:"status"`
}

// QueryPrometheus returns the promtheus metrics which match the query string
// token: the user token used to run the http request, if it's not specified, it will use the token of sa/prometheus-k8s in openshift-monitoring project
// path: the api path, for example: /api/v1/query?
// query: the metric/alert you want to search, e.g.: es_index_namespaces_total
// action: it can be "GET", "get", "Get", "POST", "post", "Post"
func QueryPrometheus(oc *exutil.CLI, token string, path string, query string, action string) (*PrometheusQueryResult, error) {
	var bearerToken string
	var err error
	if token == "" {
		bearerToken = serviceaccount.Token(oc, "prometheus-k8s", "openshift-monitoring")
	} else {
		bearerToken = token
	}
	address := "https://" + openshift.RouteAddress(oc, "openshift-monitoring", "prometheus-k8s")

	h := make(http.Header)
	h.Add("Content-Type", "application/json")
	h.Add("Authorization", "Bearer "+bearerToken)

	params := url.Values{}
	if len(query) > 0 {
		params.Add("query", query)
	}

	var p PrometheusQueryResult
	resp, err := libnethttp.DoRequest(h, address, path, params.Encode(), action, true, 5, nil, 200)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(resp, &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Get metrics by querying prometheus
func Get(oc *exutil.CLI, token, query string) ([]Metric, error) {
	res, err := QueryPrometheus(oc, token, "/api/v1/query", query, "GET")
	if err != nil {
		return []Metric{}, err
	}
	return res.Data.Result, nil
}

// EnableClusterMonitoring add label `openshift.io/cluster-monitoring: "true"` to the project, and create role/prometheus-k8s rolebinding/prometheus-k8s in the namespace
func EnableClusterMonitoring(oc *exutil.CLI, namespace string) {
	err := oc.AsAdmin().WithoutNamespace().Run("label").Args("ns", namespace, "openshift.io/cluster-monitoring=true").Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	err = oc.AsAdmin().WithoutNamespace().InputString(RbacPrometheus).Run("apply").Args("-n", namespace, "-f", "-").Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}
