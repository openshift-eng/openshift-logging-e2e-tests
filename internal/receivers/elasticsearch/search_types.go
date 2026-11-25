package elasticsearch

import (
	"encoding/json"
	"fmt"

	"github.com/openshift/openshift-logging-e2e-tests/internal/types/viaq"
)

// SearchResult example
/*
{
  "took" : 75,
  "timed_out" : false,
  "_shards" : {
    "total" : 14,
    "successful" : 14,
    "skipped" : 0,
    "failed" : 0
  },
  "hits" : {
    "total" : 63767,
    "max_score" : 1.0,
    "hits" : [
      {
        "_index" : "app-centos-logtest-000001",
        "_type" : "_doc",
        "_id" : "ODlhMmYzZDgtMTc4NC00M2Q0LWIyMGQtMThmMGY3NTNlNWYw",
        "_score" : 1.0,
        "_source" : {
          "kubernetes" : {
            "container_image_id" : "quay.io/openshifttest/ocp-logtest@sha256:f23bea6f669d125f2f317e3097a0a4da48e8792746db32838725b45efa6c64a4",
            "container_name" : "centos-logtest",
            "namespace_id" : "c74f42bb-3407-418a-b483-d5f33e08f6a5",
            "flat_labels" : [
              "run=centos-logtest",
              "test=centos-logtest"
            ],
            "host" : "ip-10-0-174-131.us-east-2.compute.internal",
            "master_url" : "https://kubernetes.default.svc",
            "pod_id" : "242e7eb4-47ca-4708-9993-db0390d18268",
            "namespace_labels" : {
              "kubernetes_io/metadata_name" : "e2e-test--lg56q"
            },
            "container_image" : "quay.io/openshifttest/ocp-logtest@sha256:f23bea6f669d125f2f317e3097a0a4da48e8792746db32838725b45efa6c64a4",
            "namespace_name" : "e2e-test--lg56q",
            "pod_name" : "centos-logtest-vnwjn"
          },
          "viaq_msg_id" : "ODlhMmYzZDgtMTc4NC00M2Q0LWIyMGQtMThmMGY3NTNlNWYw",
          "level" : "unknown",
          "message" : "{\"message\": \"MERGE_JSON_LOG=true\", \"level\": \"debug\",\"Layer1\": \"layer1 0\", \"layer2\": {\"name\":\"Layer2 1\", \"tips\":\"Decide by PRESERVE_JSON_LOG\"}, \"StringNumber\":\"10\", \"Number\": 10,\"foo.bar\":\"Dot Item\",\"{foobar}\":\"Brace Item\",\"[foobar]\":\"Bracket Item\", \"foo:bar\":\"Colon Item\",\"foo bar\":\"Space Item\" }",
          "docker" : {
            "container_id" : "b3b84d9f11cefa8abf335e8257e394414133b853dc65c8bc1d50120fc3f86da5"
          },
          "hostname" : "ip-10-0-174-131.us-east-2.compute.internal",
          "@timestamp" : "2021-07-09T01:57:44.400169+00:00",
          "pipeline_metadata" : {
            "collector" : {
              "received_at" : "2021-07-09T01:57:44.688935+00:00",
              "name" : "fluentd",
              "inputname" : "fluent-plugin-systemd",
              "version" : "1.7.4 1.6.0",
              "ipaddr4" : "10.0.174.131"
            }
          },
          "structured" : {
            "foo:bar" : "Colon Item",
            "foo.bar" : "Dot Item",
            "Number" : 10,
            "level" : "debug",
            "{foobar}" : "Brace Item",
            "foo bar" : "Space Item",
            "StringNumber" : "10",
            "layer2" : {
              "name" : "Layer2 1",
              "tips" : "Decide by PRESERVE_JSON_LOG"
            },
            "message" : "MERGE_JSON_LOG=true",
            "Layer1" : "layer1 0",
            "[foobar]" : "Bracket Item"
          }
        }
      }
    ]
  }
}
*/
// esHitsTotal is the "total" field of the "hits" object in an Elasticsearch
// search response. ES 6.x reports a plain number, e.g. "total": 63767, while
// ES 7+ reports an object, e.g. "total": {"value": 63767, "relation": "eq"}.
type esHitsTotal int64

// UnmarshalJSON accepts both the ES 6 number form and the ES 7+ object form.
func (t *esHitsTotal) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*t = esHitsTotal(n)
		return nil
	}
	var obj struct {
		Value int64 `json:"value"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("not a number and not an object with a value field: %w", err)
	}
	*t = esHitsTotal(obj.Value)
	return nil
}

type SearchResult struct {
	Took     int64 `json:"took"`
	TimedOut bool  `json:"timed_out"`
	Shards   struct {
		Total      int64 `json:"total"`
		Successful int64 `json:"successful"`
		Skipped    int64 `json:"skipped"`
		Failed     int64 `json:"failed"`
	} `json:"_shards"`
	Hits struct {
		Total    esHitsTotal `json:"total"`
		MaxScore float32     `json:"max_score"`
		DataHits []struct {
			Index  string         `json:"_index"`
			Type   string         `json:"_type"`
			ID     string         `json:"_id"`
			Score  float32        `json:"_score"`
			Source viaq.LogEntity `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
	Aggregations struct {
		LoggingAggregations struct {
			DocCount          int64 `json:"doc_count,omitempty"`
			InnerAggregations struct {
				DocCountErrorUpperBound int64 `json:"doc_count_error_upper_bound,omitempty"`
				SumOtherDocCount        int64 `json:"sum_other_doc_count,omitempty"`
				Buckets                 []struct {
					Key      string `json:"key,omitempty"`
					DocCount int64  `json:"doc_count,omitempty"`
				} `json:"buckets,omitempty"`
			} `json:"inner_aggregations,omitzero"`
		} `json:"logging_aggregations,omitzero"`
	} `json:"aggregations,omitzero"`
}

/*
The aggregation query string must be set as:
{
    "aggs" : {
        "logging_aggregations": {
            "filter": {
                "exists": {
                    "field":"kubernetes"
                }
            },
            "aggs" : {
                "inner_aggregations": {
                    "terms" : {
                        "field" : "hostname"
                    }
                }
            }
        }
    }
}
AggregationResult example
{
	"aggregations": {
		"logging_aggregations": {
		    "doc_count": 13089,
		    "inner_aggregations": {
			    "doc_count_error_upper_bound": 0,
			    "sum_other_doc_count": 0,
			    "buckets": [
			        {
				        "key": "ip-10-0-202-141",
				    	"doc_count": 3250
			  		},
			  		{
						"key": "ip-10-0-147-235",
						"doc_count": 3064
			  		},
			  		{
						"key": "ip-10-0-210-50",
						"doc_count": 2515
			  		},
			  		{
						"key": "ip-10-0-167-109",
						"doc_count": 1832
			  		},
			  		{
						"key": "ip-10-0-186-71",
						"doc_count": 1321
			 		},
			  		{
						"key": "ip-10-0-143-89",
						"doc_count": 1107
			 		 }
				]
		  	}
		}
	}
}
*/

// CountResult example
/*
{
  "count" : 453558,
  "_shards" : {
    "total" : 39,
    "successful" : 39,
    "skipped" : 0,
    "failed" : 0
  }
}
*/
type CountResult struct {
	Count  int64 `json:"count"`
	Shards struct {
		Total      int64 `json:"total"`
		Successful int64 `json:"successful"`
		Skipped    int64 `json:"skipped"`
		Failed     int64 `json:"failed"`
	} `json:"_shards"`
}

// ESIndex example
/*
  {
    "health": "green",
    "status": "open",
    "index": "infra-000015",
    "uuid": "uHqlf91RQAqit072gI9LaA",
    "pri": "3",
    "rep": "1",
    "docs.count": "37323",
    "docs.deleted": "0",
    "store.size": "58.8mb",
    "pri.store.size": "29.3mb"
  }
*/
type ESIndex struct {
	Health       string `json:"health"`
	Status       string `json:"status"`
	Index        string `json:"index"`
	UUID         string `json:"uuid"`
	PrimaryCount string `json:"pri"`
	ReplicaCount string `json:"rep"`
	DocsCount    string `json:"docs.count"`
	DocsDeleted  string `json:"docs.deleted"`
	StoreSize    string `json:"store.size"`
	PriStoreSize string `json:"pri.store.size"`
}
