package logging

import (
	"encoding/json"
	"testing"
)

// ES 6.x reports hits.total as a plain number; ES 7+ reports an object with a
// value and a relation. Both forms must unmarshal into SearchResult.Hits.Total.
func TestSearchResultUnmarshalHitsTotal(t *testing.T) {
	testCases := []struct {
		name    string
		payload string
		want    int64
	}{
		{
			name:    "es6 number",
			payload: `{"took": 1, "timed_out": false, "hits": {"total": 63767, "max_score": 1.0, "hits": []}}`,
			want:    63767,
		},
		{
			name:    "es7+ object",
			payload: `{"took": 1, "timed_out": false, "hits": {"total": {"value": 63767, "relation": "eq"}, "max_score": 1.0, "hits": []}}`,
			want:    63767,
		},
		{
			name:    "es7+ zero",
			payload: `{"took": 1, "timed_out": false, "hits": {"total": {"value": 0, "relation": "eq"}, "max_score": null, "hits": []}}`,
			want:    0,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			var res SearchResult
			if err := json.Unmarshal([]byte(test.payload), &res); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := int64(res.Hits.Total); got != test.want {
				t.Fatalf("hits.total = %d, want %d", got, test.want)
			}
		})
	}
}

func TestEsHitsTotalRejectsInvalidForms(t *testing.T) {
	for _, payload := range []string{`"63767"`, `true`} {
		var total esHitsTotal
		if err := json.Unmarshal([]byte(payload), &total); err == nil {
			t.Fatalf("expected error for payload %s", payload)
		}
	}
}
