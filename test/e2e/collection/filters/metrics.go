package filters

import (
	"context"
	"fmt"
	"strconv"
	"time"

	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	libmetrics "github.com/openshift/openshift-logging-e2e-tests/internal/lib/metrics"
)

// waitDropFilterDiscardedEvents waits until drop filter from at least one collector reports
// transform discarded at least one event, proving historical records were actually read
// and dropped rather than never collected
func waitDropFilterDiscardedEvents(oc *exutil.CLI, token, clfName, clfNamespace string) {
	query := fmt.Sprintf(`vector_component_discarded_events_total{job=%q,namespace=%q,component_id=~".*drop.*"}`, clfName, clfNamespace)
	err := wait.PollUntilContextTimeout(context.Background(), 15*time.Second, 5*time.Minute, true, func(context.Context) (done bool, err error) {
		metrics, err := libmetrics.Get(oc, token, query)
		if err != nil {
			e2e.Logf("error querying %q, try next time: %v", query, err)
			return false, nil
		}
		for _, m := range metrics {
			if len(m.Value) != 2 {
				continue
			}
			if value, ok := m.Value[1].(string); ok {
				if count, err := strconv.ParseFloat(value, 64); err == nil && count > 0 {
					e2e.Logf("drop filter discarded %s events so far", value)
					return true, nil
				}
			}
		}
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, "the drop filter did not discard any event: "+query)
}
