package logging

import (
	"encoding/json"
	"testing"
)

// One fixture per datamodel, plus the response a query with no match returns.
// The ViaQ fixture follows the documented example in types.go: every line is
// the JSON envelope LogEntity describes. The OTLP fixture was captured from a
// live LokiStack gateway: the line is the raw log body (here logfmt) and the
// Kubernetes metadata lives in the stream labels.
const viaqQueryResponse = `{"status":"success","data":{"resultType":"streams","result":[
 {"stream":{"kubernetes_container_name":"centos-logtest","kubernetes_namespace_name":"test","kubernetes_pod_name":"centos-logtest-qt6pz","log_type":"application"},"values":[["1637005500907904677","{\"docker\":{\"container_id\":\"da3cf8c0493625dc4f42c8592954aad95f3f4ce2a2098ab97ab6a4ad58276617\"},\"kubernetes\":{\"container_name\":\"centos-logtest\",\"namespace_name\":\"test\",\"pod_name\":\"centos-logtest-qt6pz\",\"pod_id\":\"d77cae4f-2b8a-4c30-8142-417756aa3daf\",\"host\":\"ip-10-0-161-168.us-east-2.compute.internal\"},\"message\":\"{\\\"message\\\": \\\"MERGE_JSON_LOG=true\\\"}\",\"level\":\"unknown\",\"hostname\":\"ip-10-0-161-168.us-east-2.compute.internal\",\"log_type\":\"application\"}"]]}
]}}`

const otelQueryResponse = `{"status":"success","data":{"resultType":"streams","result":[
 {"stream":{"k8s_container_name":"registry","k8s_namespace_name":"openshift-image-registry","k8s_pod_name":"image-registry-54ffcfb7cd-9ntx8","log_source":"container","openshift_log_type":"infrastructure"},"values":[["1794402815106234553","time=\"2026-11-11T13:13:35.106179832Z\" level=info msg=\"authorized request\" http.request.method=GET openshift.auth.user=\"system:serviceaccount:openshift-monitoring:prometheus-k8s\""]]},
 {"stream":{"k8s_container_name":"registry","k8s_namespace_name":"openshift-image-registry","k8s_pod_name":"image-registry-54ffcfb7cd-9ntx8","log_source":"container","openshift_log_type":"infrastructure"},"values":[["1794402815107224713","time=\"2026-11-11T13:13:35.107180622Z\" level=info msg=\"response completed\" http.response.status=200 http.response.written=2859"]]}
]}}`

const emptyQueryResponse = `{"status":"success","data":{"resultType":"streams","result":[]}}`

// TestLokiQueryResultMatchesLines validates what the e2e tests rely on when
// they assert on logs.Data.Result: it is non-empty exactly when the query
// matched at least one line, for both datamodels. Loki answers a range query
// with one entry per matching stream, and every returned stream carries at
// least one value.
func TestLokiQueryResultMatchesLines(t *testing.T) {
	testCases := []struct {
		name      string
		response  string
		wantEmpty bool
	}{
		{name: "viaq with matched lines", response: viaqQueryResponse, wantEmpty: false},
		{name: "viaq without matched lines", response: emptyQueryResponse, wantEmpty: true},
		{name: "otel with matched lines", response: otelQueryResponse, wantEmpty: false},
		{name: "otel without matched lines", response: emptyQueryResponse, wantEmpty: true},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			var resp lokiQueryResponse
			if err := json.Unmarshal([]byte(test.response), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if got := len(resp.Data.Result) == 0; got != test.wantEmpty {
				t.Errorf("Data.Result empty = %v, want %v", got, test.wantEmpty)
			}

			lines := 0
			for _, stream := range resp.Data.Result {
				lines += len(stream.Values)
			}
			if got := lines == 0; got != test.wantEmpty {
				t.Errorf("matched lines = %d, want empty = %v", lines, test.wantEmpty)
			}
		})
	}
}
