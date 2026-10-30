package types

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	libopenshift "github.com/openshift/openshift-logging-e2e-tests/internal/types/openshift"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

type Resource struct {
	Kind      string
	Name      string
	Namespace string
}

func NewResource(kind string, name string, namespace string) Resource {
	return Resource{
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
	}
}

func (r Resource) WaitUntilResourceIsGone(oc *exutil.CLI) error {
	err := wait.PollUntilContextTimeout(context.Background(), 3*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", r.Namespace, r.Kind, r.Name).Output()
		if err != nil {
			errstring := fmt.Sprintf("%v", output)
			if strings.Contains(errstring, "NotFound") || strings.Contains(errstring, "the server doesn't have a resource type") {
				return true, nil
			}
			return true, err
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("can't Remove %s/%s in %s project", r.Kind, r.Name, r.Namespace)
	}
	return nil
}

func (r Resource) ApplyFromTemplate(oc *exutil.CLI, parameters ...string) error {
	parameters = append(parameters, "-n", r.Namespace)
	file, err := libopenshift.ProcessTemplate(oc, parameters...)
	defer os.Remove(file)
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Can not process %v", parameters))
	output, err := oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", file, "-n", r.Namespace).Output()
	if err != nil {
		return fmt.Errorf("can't apply resource: %s", output)
	}
	r.WaitForResourceToAppear(oc)
	return nil
}

func (r Resource) WaitForResourceToAppear(oc *exutil.CLI) {
	err := wait.PollUntilContextTimeout(context.Background(), 3*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		e2e.Logf("wait %s %s ready ... ", r.Kind, r.Name)
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", r.Namespace, r.Kind, r.Name).Output()
		if err != nil {
			msg := fmt.Sprintf("%v", output)
			if strings.Contains(msg, "NotFound") {
				return false, nil
			}
			return false, err
		}
		e2e.Logf("found %s %s", r.Kind, r.Name)
		return true, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("resource %s/%s is not appear", r.Kind, r.Name))
}

// Clear delete the objects in the cluster
func (r Resource) Clear(oc *exutil.CLI) error {
	msg, err := oc.AsAdmin().WithoutNamespace().Run("delete").Args("-n", r.Namespace, r.Kind, r.Name).Output()
	if err != nil {
		errstring := fmt.Sprintf("%v", msg)
		if strings.Contains(errstring, "NotFound") || strings.Contains(errstring, "the server doesn't have a resource type") {
			return nil
		}
		return err
	}
	err = r.WaitUntilResourceIsGone(oc)
	return err
}
