package daemonset

import (
	"context"
	"fmt"
	"strings"
	"time"

	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/compat_otp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	Kind = "daemonset"
)

// WaitForPodsToBeReady waits for all the pods controlled by the ds to be ready
func WaitForPodsToBeReady(oc *exutil.CLI, ns string, name string) {
	var selectors map[string]string
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		daemonset, err := oc.AdminKubeClient().AppsV1().DaemonSets(ns).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				e2e.Logf("Waiting for daemonset/%s to appear\n", name)
				return false, nil
			}
			return false, err
		}
		selectors = daemonset.Spec.Selector.MatchLabels
		if daemonset.Status.DesiredNumberScheduled > 0 && daemonset.Status.NumberReady == daemonset.Status.DesiredNumberScheduled && daemonset.Status.UpdatedNumberScheduled == daemonset.Status.DesiredNumberScheduled {
			e2e.Logf("Daemonset/%s is available (%d/%d)\n", name, daemonset.Status.NumberReady, daemonset.Status.DesiredNumberScheduled)
			return true, nil
		}
		e2e.Logf("Waiting for full availability of %s daemonset (%d/%d)\n", name, daemonset.Status.NumberReady, daemonset.Status.DesiredNumberScheduled)
		return false, nil
	})
	if err != nil && len(selectors) > 0 {
		var labels []string
		for k, v := range selectors {
			labels = append(labels, k+"="+v)
		}
		label := strings.Join(labels, ",")
		_ = oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", ns, "-l", label).Execute()
		podStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", ns, "-l", label, "-ojsonpath={.items[*].status.conditions}").Output()
		containerStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", ns, "-l", label, "-ojsonpath={.items[*].status.containerStatuses}").Output()
		e2e.Failf("daemonset %s is not ready:\nconditions: %s\ncontainer status: %s", name, podStatus, containerStatus)
	}
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Daemonset %s is not available", name))
}
