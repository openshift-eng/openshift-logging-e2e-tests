package openshift

import (
	"context"
	"fmt"
	"time"

	librandom "github.com/openshift/openshift-logging-e2e-tests/internal/lib/random"
	exutil "github.com/openshift/origin/test/extended/util"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// ProcessTemplate uses the oc client to process an OpenShift template with the given parameters
func ProcessTemplate(oc *exutil.CLI, parameters ...string) (string, error) {
	var configFile string
	filename := librandom.String() + ".json"
	err := wait.PollUntilContextTimeout(context.Background(), 3*time.Second, 15*time.Second, true, func(context.Context) (done bool, err error) {
		output, err := oc.AsAdmin().Run("process").Args(parameters...).OutputToFile(filename)
		if err != nil {
			e2e.Logf("the err:%v, and try next round", err)
			return false, nil
		}
		configFile = output
		return true, nil
	})
	if err != nil {
		return configFile, fmt.Errorf("failed to process template with the provided parameters")
	}
	return configFile, nil
}
