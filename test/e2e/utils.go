package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/structpb"
	"gopkg.in/yaml.v3"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

// SubscriptionObjects objects are used to create operators via OLM
type SubscriptionObjects struct {
	OperatorName       string
	Namespace          string
	OperatorGroup      string // the file used to create operator group
	Subscription       string // the file used to create subscription
	PackageName        string
	OperatorPodLabel   string               //The operator pod label which is used to select pod
	CatalogSource      CatalogSourceObjects `json:",omitempty"`
	SkipCaseWhenFailed bool                 // if true, the case will be skipped when operator is not ready, otherwise, the case will be marked as failed
}

// CatalogSourceObjects defines the source used to subscribe an operator
type CatalogSourceObjects struct {
	Channel         string `json:",omitempty"`
	SourceName      string `json:",omitempty"`
	SourceNamespace string `json:",omitempty"`
}

func getRandomString() string {
	chars := "abcdefghijklmnopqrstuvwxyz0123456789"
	seed := rand.New(rand.NewSource(time.Now().UnixNano()))
	buffer := make([]byte, 8)
	for index := range buffer {
		buffer[index] = chars[seed.Intn(len(chars))]
	}
	return string(buffer)
}

// contain checks if b is an elememt of a
func contain(a []string, b string) bool {
	for _, c := range a {
		if c == b {
			return true
		}
	}
	return false
}

// containSubstring checks if b is a's element's substring
func containSubstring(a interface{}, b string) bool {
	switch reflect.TypeOf(a).Kind() {
	case reflect.Slice, reflect.Array:
		s := reflect.ValueOf(a)
		for i := 0; i < s.Len(); i++ {
			if strings.Contains(fmt.Sprintln(s.Index(i)), b) {
				return true
			}
		}
	}
	return false
}

func processTemplate(oc *exutil.CLI, parameters ...string) (string, error) {
	var configFile string
	filename := getRandomString() + ".json"
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

func getProxyFromEnv() string {
	var proxy string
	if os.Getenv("http_proxy") != "" {
		proxy = os.Getenv("http_proxy")
	} else if os.Getenv("http_proxy") != "" {
		proxy = os.Getenv("https_proxy")
	}
	return proxy
}

func getClusterID(oc *exutil.CLI) (string, error) {
	return oc.AsAdmin().WithoutNamespace().Run("get").Args("clusterversion/version", "-ojsonpath={.spec.clusterID}").Output()
}

func isFipsEnabled(oc *exutil.CLI) bool {
	nodes, err := compat_otp.GetSchedulableLinuxWorkerNodes(oc)
	o.Expect(err).NotTo(o.HaveOccurred())
	fips, err := compat_otp.DebugNodeWithChroot(oc, nodes[0].Name, "bash", "-c", "fips-mode-setup --check")
	o.Expect(err).NotTo(o.HaveOccurred())
	return strings.Contains(fips, "FIPS mode is enabled.")
}

func (so *SubscriptionObjects) checkPackagemanifestBySource(oc *exutil.CLI) {
	args := []string{"-n", so.CatalogSource.SourceNamespace, "packagemanifests", "-l", "catalog=" + so.CatalogSource.SourceName}
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		packages, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(args...).Output()
		if err != nil {
			if strings.Contains(err.Error(), "No resources found") || strings.Contains(err.Error(), "NotFound") {
				return false, nil
			}
			return false, err
		}
		if strings.Contains(packages, so.PackageName) {
			return true, nil
		}
		e2e.Logf("Waiting for packagemanifest/%s to appear", so.PackageName)
		return false, nil
	})
	if err != nil {
		if so.SkipCaseWhenFailed {
			g.Skip(fmt.Sprintf("Skip the case for can't find packagemanifest/%s", so.PackageName))
		} else {
			e2e.Failf("Packagemanifest %s is not available", so.PackageName)
		}
	}

	args = append(args, `-ojsonpath={.items[?(@.metadata.name=="`+so.PackageName+`")].status.channels[*].name}`)
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(args...).Output()
	if err != nil {
		e2e.Logf("Error fetching channels: %v", err)
	}
	channels := strings.Fields(output)
	if !contain(channels, so.CatalogSource.Channel) {
		e2e.Logf("Found channels %v from packagemanifest/%s, but wanted %s", channels, so.PackageName, so.CatalogSource.Channel)
		if so.SkipCaseWhenFailed {
			g.Skip(fmt.Sprintf("Skip the case for packagemanifest/%s doesn't have target channel %s", so.PackageName, so.CatalogSource.Channel))
		} else {
			e2e.Failf("Packagemanifest %s doesn't have target channel %s", so.PackageName, so.CatalogSource.Channel)
		}
	}
}

// setCatalogSourceObjects set the default values of channel, source namespace and source name if they're not specified
func (so *SubscriptionObjects) setCatalogSourceObjects(oc *exutil.CLI) {
	// set channel
	if so.CatalogSource.Channel == "" {
		so.CatalogSource.Channel = "stable-6.6"
	}

	// set source namespace
	if so.CatalogSource.SourceNamespace == "" {
		so.CatalogSource.SourceNamespace = "openshift-marketplace"
	}

	// set source and check if the packagemanifest exists or not
	if so.CatalogSource.SourceName != "" {
		so.checkPackagemanifestBySource(oc)
		return
	}

	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("catsrc", "-n", so.CatalogSource.SourceNamespace, "-ojsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		e2e.Logf("can't list catalog source in project %s: %v", so.CatalogSource.SourceNamespace, err)
	}
	catsrcs := strings.Fields(output)
	switch {
	case contain(catsrcs, "auto-release-app-registry"):
		if contain(catsrcs, "redhat-operators") {
			// do not subscribe source auto-release-app-registry as we want to test GAed logging in auto release jobs
			so.CatalogSource.SourceName = "redhat-operators"
			so.checkPackagemanifestBySource(oc)
		} else if so.SkipCaseWhenFailed {
			g.Skip("skip the case because the cluster doesn't have proper catalog source for logging")
		}
	case so.PackageName == "cluster-logging" && contain(catsrcs, "cluster-logging-operator-registry"):
		so.CatalogSource.SourceName = "cluster-logging-operator-registry"
		so.checkPackagemanifestBySource(oc)
	case so.PackageName == "loki-operator" && contain(catsrcs, "loki-operator-registry"):
		so.CatalogSource.SourceName = "loki-operator-registry"
		so.checkPackagemanifestBySource(oc)
	default:
		output, err1 := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", so.CatalogSource.SourceNamespace, "packagemanifests", "-l", "catalog!=community-operators,catalog!=certified-operators,catalog!=redhat-marketplace", "-ojsonpath={.items}").Output()
		if err1 != nil {
			if so.SkipCaseWhenFailed {
				g.Skip(fmt.Sprintf("Skip the case for can't list packagemanifests, hit error: %v", err1))
			} else {
				e2e.Failf("can't list packagemanifests, hit error: %v", err1)
			}
		}
		var list, filtered []PackageManifest
		json.Unmarshal([]byte(output), &list)
		for _, pm := range list {
			if pm.Name == so.PackageName {
				filtered = append(filtered, pm)
			}
		}
		if len(filtered) == 0 {
			if so.SkipCaseWhenFailed {
				g.Skip(fmt.Sprintf("Skip the case for packagemanifest/%s doesn't exist in the cluster", so.PackageName))
			} else {
				e2e.Failf("can't find packagemanifest %s", so.PackageName)
			}
		}
		for _, p := range filtered {
			for _, c := range p.Status.Channels {
				if c.Name == so.CatalogSource.Channel {
					so.CatalogSource.SourceName = p.Status.CatalogSource
					so.CatalogSource.SourceNamespace = p.Status.CatalogSourceNamespace
					break
				}
			}
		}
		if so.CatalogSource.SourceName == "" {
			if so.SkipCaseWhenFailed {
				g.Skip(fmt.Sprintf("packagemanifests %s don't contain channel %s, skip the case", so.PackageName, so.CatalogSource.Channel))
			} else {
				e2e.Failf("packagemanifests %s don't contain channel %s", so.PackageName, so.CatalogSource.Channel)
			}
		}

	}
}

// SubscribeOperator is used to deploy operators
func (so *SubscriptionObjects) SubscribeOperator(oc *exutil.CLI) {
	// check if the namespace exists, if it doesn't exist, create the namespace
	if so.OperatorPodLabel == "" {
		so.OperatorPodLabel = "name=" + so.OperatorName
	}
	_, err := oc.AdminKubeClient().CoreV1().Namespaces().Get(context.Background(), so.Namespace, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			e2e.Logf("The project %s is not found, create it now...", so.Namespace)
			namespaceTemplate := testdata.FixturePath("logging", "subscription", "namespace.yaml")
			namespaceFile, err := processTemplate(oc, "-f", namespaceTemplate, "-p", "NAMESPACE_NAME="+so.Namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			defer os.Remove(namespaceFile)
			err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(context.Context) (done bool, err error) {
				output, err := oc.AsAdmin().Run("apply").Args("-f", namespaceFile).Output()
				if err != nil {
					if strings.Contains(output, "AlreadyExists") {
						return true, nil
					}
					return false, err
				}
				return true, nil
			})
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("can't create project %s", so.Namespace))
		}
	}

	// check the operator group, if no object found, then create an operator group in the project
	og, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", so.Namespace, "og").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	msg := fmt.Sprintf("%v", og)
	if strings.Contains(msg, "No resources found") {
		// create operator group
		ogFile, err := processTemplate(oc, "-n", so.Namespace, "-f", so.OperatorGroup, "-p", "OG_NAME="+so.Namespace, "NAMESPACE="+so.Namespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		defer os.Remove(ogFile)
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(context.Context) (done bool, err error) {
			output, err := oc.AsAdmin().Run("apply").Args("-f", ogFile, "-n", so.Namespace).Output()
			if err != nil {
				if strings.Contains(output, "AlreadyExists") {
					return true, nil
				}
				return false, err
			}
			return true, nil
		})
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("can't create operatorgroup %s in %s project", so.Namespace, so.Namespace))
	}

	// check subscription, if there is no subscription objets, then create one
	installedPackages, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("sub", "-n", so.Namespace, "-ojsonpath={.items[*].spec.name}").Output()
	if err != nil {
		if so.SkipCaseWhenFailed {
			g.Skip("can't get subscriptions: " + err.Error() + ", skip the case")
		} else {
			e2e.Failf("can't get subscriptions: %v", err)
		}
	} else {
		if strings.Contains(installedPackages, so.PackageName) {
			e2e.Logf("operator %s is already installed", so.PackageName)
		} else {
			so.setCatalogSourceObjects(oc)
			e2e.Logf("subscribe operator %s to source %s", so.OperatorName, so.CatalogSource.SourceName)
			//create subscription object
			subscriptionFile, err := processTemplate(oc, "-n", so.Namespace, "-f", so.Subscription, "-p", "PACKAGE_NAME="+so.PackageName, "NAMESPACE="+so.Namespace, "CHANNEL="+so.CatalogSource.Channel, "SOURCE="+so.CatalogSource.SourceName, "SOURCE_NAMESPACE="+so.CatalogSource.SourceNamespace)
			if err != nil {
				if so.SkipCaseWhenFailed {
					g.Skip("hit error when processing subscription template: " + err.Error() + ", skip the case")
				} else {
					e2e.Failf("hit error when processing subscription template: %v", err)
				}
			}
			defer os.Remove(subscriptionFile)
			err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(context.Context) (done bool, err error) {
				output, err := oc.AsAdmin().Run("apply").Args("-f", subscriptionFile, "-n", so.Namespace).Output()
				if err != nil {
					if strings.Contains(output, "AlreadyExists") {
						return true, nil
					}
					return false, err
				}
				return true, nil
			})
			if err != nil {
				if so.SkipCaseWhenFailed {
					g.Skip("hit error when creating subscription, skip the case")
				} else {
					e2e.Failf("hit error when creating subscription")
				}
			}
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("can't create subscription %s in %s project", so.PackageName, so.Namespace))
			// check status in subscription
			err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 120*time.Second, true, func(context.Context) (done bool, err error) {
				output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", so.Namespace, "sub", so.PackageName, `-ojsonpath={.status.state}`).Output()
				if err != nil {
					e2e.Logf("error getting subscription/%s: %v", so.PackageName, err)
					return false, nil
				}
				return strings.Contains(output, "AtLatestKnown"), nil
			})
			if err != nil {
				out, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", so.Namespace, "sub", so.PackageName, `-ojsonpath={.status.conditions}`).Output()
				e2e.Logf("subscription/%s is not ready, conditions: %v", so.PackageName, out)
				if so.SkipCaseWhenFailed {
					g.Skip(fmt.Sprintf("Skip the case for the operator %s is not ready", so.OperatorName))
				} else {
					e2e.Failf("can't deploy operator %s", so.OperatorName)
				}
			}
		}
	}

	// check pod status
	err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 240*time.Second, true, func(context.Context) (done bool, err error) {
		pods, err := oc.AdminKubeClient().CoreV1().Pods(so.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: so.OperatorPodLabel})
		if err != nil {
			e2e.Logf("Hit error %v when getting pods", err)
			return false, nil
		}
		if len(pods.Items) == 0 {
			e2e.Logf("Waiting for pod with label %s to appear\n", so.OperatorPodLabel)
			return false, nil
		}
		ready := true
		for _, pod := range pods.Items {
			if pod.Status.Phase != "Running" {
				ready = false
				e2e.Logf("Pod %s is not running: %v", pod.Name, pod.Status.Phase)
				break
			}
			for _, containerStatus := range pod.Status.ContainerStatuses {
				if !containerStatus.Ready {
					ready = false
					e2e.Logf("Container %s in pod %s is not ready", containerStatus.Name, pod.Name)
					break
				}
			}
		}
		return ready, nil
	})
	if err != nil {
		_ = oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", so.Namespace, "-l", so.OperatorPodLabel).Execute()
		podStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", so.Namespace, "-l", so.OperatorPodLabel, "-ojsonpath={.items[*].status.conditions}").Output()
		containerStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", so.Namespace, "-l", so.OperatorPodLabel, "-ojsonpath={.items[*].status.containerStatuses}").Output()
		e2e.Logf("pod with label %s is not ready:\nconditions: %s\ncontainer status: %s", so.OperatorPodLabel, podStatus, containerStatus)
		if so.SkipCaseWhenFailed {
			g.Skip(fmt.Sprintf("Skip the case for the operator %s is not ready", so.OperatorName))
		} else {
			e2e.Failf("can't deploy operator %s", so.OperatorName)
		}
	}
}

func (so *SubscriptionObjects) uninstallOperator(oc *exutil.CLI) {
	//csv, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", so.Namespace, "sub/"+so.PackageName, "-ojsonpath={.status.installedCSV}").Output()
	resource{"subscription", so.PackageName, so.Namespace}.clear(oc)
	//_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("-n", so.Namespace, "csv", csv).Execute()
	_ = oc.AsAdmin().WithoutNamespace().Run("delete").Args("-n", so.Namespace, "csv", "-l", "operators.coreos.com/"+so.PackageName+"."+so.Namespace+"=").Execute()
	// do not remove namespace openshift-logging and openshift-operators-redhat, and preserve the operatorgroup as there may have several operators deployed in one namespace
	// for example: loki-operator
	if so.Namespace != "openshift-logging" && so.Namespace != "openshift-operators-redhat" && !strings.HasPrefix(so.Namespace, "e2e-test-") {
		deleteNamespace(oc, so.Namespace)
	}
}

func (so *SubscriptionObjects) getInstalledCSV(oc *exutil.CLI) string {
	installedCSV, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", so.Namespace, "sub", so.PackageName, "-ojsonpath={.status.installedCSV}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	return installedCSV
}

// WaitForDeploymentPodsToBeReady waits for the specific deployment to be ready
func WaitForDeploymentPodsToBeReady(oc *exutil.CLI, namespace string, name string) {
	var selectors map[string]string
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		deployment, err := oc.AdminKubeClient().AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				e2e.Logf("Waiting for deployment/%s to appear\n", name)
				return false, nil
			}
			return false, err
		}
		selectors = deployment.Spec.Selector.MatchLabels
		if deployment.Status.AvailableReplicas == *deployment.Spec.Replicas && deployment.Status.UpdatedReplicas == *deployment.Spec.Replicas {
			e2e.Logf("Deployment %s available (%d/%d)\n", name, deployment.Status.AvailableReplicas, *deployment.Spec.Replicas)
			return true, nil
		}
		e2e.Logf("Waiting for full availability of %s deployment (%d/%d)\n", name, deployment.Status.AvailableReplicas, *deployment.Spec.Replicas)
		return false, nil
	})
	if err != nil && len(selectors) > 0 {
		var labels []string
		for k, v := range selectors {
			labels = append(labels, k+"="+v)
		}
		label := strings.Join(labels, ",")
		_ = oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", namespace, "-l", label).Execute()
		podStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", namespace, "-l", label, "-ojsonpath={.items[*].status.conditions}").Output()
		containerStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", namespace, "-l", label, "-ojsonpath={.items[*].status.containerStatuses}").Output()
		e2e.Failf("deployment %s is not ready:\nconditions: %s\ncontainer status: %s", name, podStatus, containerStatus)
	}
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("deployment %s is not available", name))
}

func waitForStatefulsetReady(oc *exutil.CLI, namespace string, name string) {
	var selectors map[string]string
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		ss, err := oc.AdminKubeClient().AppsV1().StatefulSets(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				e2e.Logf("Waiting for statefulset/%s to appear\n", name)
				return false, nil
			}
			return false, err
		}
		selectors = ss.Spec.Selector.MatchLabels
		if ss.Status.ReadyReplicas == *ss.Spec.Replicas && ss.Status.UpdatedReplicas == *ss.Spec.Replicas {
			e2e.Logf("statefulset %s available (%d/%d)\n", name, ss.Status.ReadyReplicas, *ss.Spec.Replicas)
			return true, nil
		}
		e2e.Logf("Waiting for full availability of %s statefulset (%d/%d)\n", name, ss.Status.ReadyReplicas, *ss.Spec.Replicas)
		return false, nil
	})
	if err != nil && len(selectors) > 0 {
		var labels []string
		for k, v := range selectors {
			labels = append(labels, k+"="+v)
		}
		label := strings.Join(labels, ",")
		_ = oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", namespace, "-l", label).Execute()
		podStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", namespace, "-l", label, "-ojsonpath={.items[*].status.conditions}").Output()
		containerStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", namespace, "-l", label, "-ojsonpath={.items[*].status.containerStatuses}").Output()
		e2e.Failf("statefulset %s is not ready:\nconditions: %s\ncontainer status: %s", name, podStatus, containerStatus)
	}
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("statefulset %s is not available", name))
}

// WaitForDaemonsetPodsToBeReady waits for all the pods controlled by the ds to be ready
func WaitForDaemonsetPodsToBeReady(oc *exutil.CLI, ns string, name string) {
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

func waitForPodReadyWithLabel(oc *exutil.CLI, ns string, label string) {
	var count int
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		pods, err := oc.AdminKubeClient().CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: label})
		if err != nil {
			return false, err
		}
		count = len(pods.Items)
		if count == 0 {
			e2e.Logf("Waiting for pod with label %s to appear\n", label)
			return false, nil
		}
		ready := true
		for _, pod := range pods.Items {
			if pod.Status.Phase != "Running" {
				ready = false
				break
			}
			for _, containerStatus := range pod.Status.ContainerStatuses {
				if !containerStatus.Ready {
					ready = false
					break
				}
			}
		}
		if !ready {
			e2e.Logf("Waiting for pod with label %s to be ready...\n", label)
		}
		return ready, nil
	})
	if err != nil && count != 0 {
		_ = oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", ns, "-l", label).Execute()
		podStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", ns, "-l", label, "-ojsonpath={.items[*].status.conditions}").Output()
		containerStatus, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", ns, "-l", label, "-ojsonpath={.items[*].status.containerStatuses}").Output()
		e2e.Failf("pod with label %s is not ready:\nconditions: %s\ncontainer status: %s", label, podStatus, containerStatus)
	}
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("pod with label %s is not ready", label))
}

func getPodNames(oc *exutil.CLI, ns, label string) ([]string, error) {
	var names []string
	pods, err := oc.AdminKubeClient().CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: label})
	if err != nil {
		return names, err
	}
	if len(pods.Items) == 0 {
		return names, fmt.Errorf("no pod(s) match label %s in namespace %s", label, ns)
	}
	for _, pod := range pods.Items {
		names = append(names, pod.Name)
	}
	return names, nil
}

type resource struct {
	kind      string
	name      string
	namespace string
}

// WaitUntilResourceIsGone waits for the resource to be removed cluster
func (r resource) WaitUntilResourceIsGone(oc *exutil.CLI) error {
	err := wait.PollUntilContextTimeout(context.Background(), 3*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", r.namespace, r.kind, r.name).Output()
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
		return fmt.Errorf("can't remove %s/%s in %s project", r.kind, r.name, r.namespace)
	}
	return nil
}

// delete the objects in the cluster
func (r resource) clear(oc *exutil.CLI) error {
	msg, err := oc.AsAdmin().WithoutNamespace().Run("delete").Args("-n", r.namespace, r.kind, r.name).Output()
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

func (r resource) WaitForResourceToAppear(oc *exutil.CLI) {
	err := wait.PollUntilContextTimeout(context.Background(), 3*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		e2e.Logf("wait %s %s ready ... ", r.kind, r.name)
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", r.namespace, r.kind, r.name).Output()
		if err != nil {
			msg := fmt.Sprintf("%v", output)
			if strings.Contains(msg, "NotFound") {
				return false, nil
			}
			return false, err
		}
		e2e.Logf("found %s %s", r.kind, r.name)
		return true, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("resource %s/%s is not appear", r.kind, r.name))
}

func (r resource) applyFromTemplate(oc *exutil.CLI, parameters ...string) error {
	parameters = append(parameters, "-n", r.namespace)
	file, err := processTemplate(oc, parameters...)
	defer os.Remove(file)
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Can not process %v", parameters))
	output, err := oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", file, "-n", r.namespace).Output()
	if err != nil {
		return fmt.Errorf("can't apply resource: %s", output)
	}
	r.WaitForResourceToAppear(oc)
	return nil
}

type clusterlogforwarder struct {
	collectApplicationLogs    bool // optional, if true, will add cluster-role/collect-application-logs to the serviceAccount
	collectAuditLogs          bool // optional, if true, will add cluster-role/collect-audit-logs to the serviceAccount
	collectInfrastructureLogs bool // optional, if true, will add cluster-role/collect-infrastructure-logs to the serviceAccount
	enableMonitoring          bool // optional, if true, will add label `openshift.io/cluster-monitoring: "true"` to the project, and create role/prometheus-k8s rolebinding/prometheus-k8s in the namespace, works when when !(clf.namespace == "openshift-operators-redhat" || clf.namespace == "openshift-logging")
	name                      string
	namespace                 string
	serviceAccountName        string
	templateFile              string // the template used to create clusterlogforwarder, no default value
	secretName                string // optional, if it's specified, when creating CLF, the parameter `"SECRET_NAME="+clf.secretName` will be added automatically
	waitForPodReady           bool   // optional, if true, will check daemonset stats
}

// create clusterlogforwarder CR from a template
func (clf *clusterlogforwarder) create(oc *exutil.CLI, optionalParameters ...string) {
	//parameters := []string{"-f", clf.templateFile, "--ignore-unknown-parameters=true", "-p", "NAME=" + clf.name, "NAMESPACE=" + clf.namespace}
	if clf.namespace != "openshift-logging" {
		compat_otp.SetNamespacePrivileged(oc, clf.namespace)
	}

	if clf.namespace != cloNS && clf.namespace != loNS && clf.enableMonitoring {
		enableClusterMonitoring(oc, clf.namespace)
	}

	parameters := []string{"-f", clf.templateFile, "-p", "NAME=" + clf.name, "NAMESPACE=" + clf.namespace}
	if clf.secretName != "" {
		parameters = append(parameters, "SECRET_NAME="+clf.secretName)
	}

	if clf.serviceAccountName != "" {
		clf.createServiceAccount(oc)
		parameters = append(parameters, "SERVICE_ACCOUNT_NAME="+clf.serviceAccountName)
	}

	if len(optionalParameters) > 0 {
		parameters = append(parameters, optionalParameters...)
	}

	file, processErr := processTemplate(oc, parameters...)
	defer os.Remove(file)
	if processErr != nil {
		e2e.Failf("error processing file: %v", processErr)
	}
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args("-f", file, "-n", clf.namespace).Execute()
	if err != nil {
		e2e.Failf("error creating clusterlogforwarder: %v", err)
	}
	resource{"clusterlogforwarders.observability.openshift.io", clf.name, clf.namespace}.WaitForResourceToAppear(oc)
	if clf.waitForPodReady {
		clf.waitForCollectorPodsReady(oc)
	}
}

// createServiceAccount creates the serviceaccount and add the required clusterroles to the serviceaccount
func (clf *clusterlogforwarder) createServiceAccount(oc *exutil.CLI) {
	_, err := oc.AdminKubeClient().CoreV1().ServiceAccounts(clf.namespace).Get(context.Background(), clf.serviceAccountName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		err = createServiceAccount(oc, clf.namespace, clf.serviceAccountName)
		if err != nil {
			e2e.Failf("can't create the serviceaccount: %v", err)
		}
	}
	if clf.collectApplicationLogs {
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-application-logs")
		o.Expect(err).NotTo(o.HaveOccurred())
	}
	if clf.collectInfrastructureLogs {
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-infrastructure-logs")
		o.Expect(err).NotTo(o.HaveOccurred())
	}
	if clf.collectAuditLogs {
		err = addClusterRoleToServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-audit-logs")
		o.Expect(err).NotTo(o.HaveOccurred())
	}
}

func createServiceAccount(oc *exutil.CLI, namespace, name string) error {
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args("serviceaccount", name, "-n", namespace).Execute()
	return err
}

func addClusterRoleToServiceAccount(oc *exutil.CLI, namespace, serviceAccountName, clusterRole string) error {
	return oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "add-cluster-role-to-user", clusterRole, fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName)).Execute()
}

func removeClusterRoleFromServiceAccount(oc *exutil.CLI, namespace, serviceAccountName, clusterRole string) error {
	return oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "remove-cluster-role-from-user", clusterRole, fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName)).Execute()
}

// update existing clusterlogforwarder CR
// if template is specified, then run command `oc process -f template -p patches | oc apply -f -`
// if template is not specified, then run command `oc patch clusterlogforwarder/${clf.name} -p patches`
// if use patch, should add `--type=` in the end of patches
func (clf *clusterlogforwarder) update(oc *exutil.CLI, template string, patches ...string) {
	var err error
	if template != "" {
		//parameters := []string{"-f", template, "--ignore-unknown-parameters=true", "-p", "NAME=" + clf.name, "NAMESPACE=" + clf.namespace}
		parameters := []string{"-f", template, "-p", "NAME=" + clf.name, "NAMESPACE=" + clf.namespace}
		if clf.secretName != "" {
			parameters = append(parameters, "SECRET_NAME="+clf.secretName)
		}
		parameters = append(parameters, "SERVICE_ACCOUNT_NAME="+clf.serviceAccountName)

		if len(patches) > 0 {
			parameters = append(parameters, patches...)
		}
		file, processErr := processTemplate(oc, parameters...)
		defer os.Remove(file)
		if processErr != nil {
			e2e.Failf("error processing file: %v", processErr)
		}
		err = oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", file, "-n", clf.namespace).Execute()
	} else {
		parameters := []string{"clusterlogforwarders.observability.openshift.io/" + clf.name, "-n", clf.namespace, "-p"}
		parameters = append(parameters, patches...)
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args(parameters...).Execute()
	}
	if err != nil {
		e2e.Failf("error updating clusterlogforwarder: %v", err)
	}
}

// patch existing clusterlogforwarder CR and return the output,
// return patch_output and error
func (clf *clusterlogforwarder) patch(oc *exutil.CLI, patch_string string) (string, error) {
	parameters := []string{"clusterlogforwarders.observability.openshift.io/" + clf.name, "-n", clf.namespace, "-p"}
	parameters = append(parameters, patch_string, "--type=json")
	return oc.AsAdmin().WithoutNamespace().Run("patch").Args(parameters...).Output()
}

// delete the clusterlogforwarder CR
func (clf *clusterlogforwarder) delete(oc *exutil.CLI) {
	err := resource{"clusterlogforwarders.observability.openshift.io", clf.name, clf.namespace}.clear(oc)
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("clusterlogforwarder/%s in project/%s is not deleted", clf.name, clf.namespace))
	if len(clf.serviceAccountName) > 0 {
		if clf.collectApplicationLogs {
			removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-application-logs")
		}
		if clf.collectInfrastructureLogs {
			removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-infrastructure-logs")
		}
		if clf.collectAuditLogs {
			removeClusterRoleFromServiceAccount(oc, clf.namespace, clf.serviceAccountName, "collect-audit-logs")
		}
		resource{"serviceaccount", clf.serviceAccountName, clf.namespace}.clear(oc)
	}

	err = resource{"daemonset", clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("daemonset/%s in project/%s is not deleted", clf.name, clf.namespace))

}

func (clf *clusterlogforwarder) waitForCollectorPodsReady(oc *exutil.CLI) {
	WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)
}

func (clf *clusterlogforwarder) getCollectorNodeNames(oc *exutil.CLI) ([]string, error) {
	var nodes []string
	pods, err := oc.AdminKubeClient().CoreV1().Pods(clf.namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=collector,app.kubernetes.io/instance=" + clf.name})
	for _, pod := range pods.Items {
		nodes = append(nodes, pod.Spec.NodeName)
	}
	return nodes, err
}

func (clf *clusterlogforwarder) getNetworkPolicyRuleSet(oc *exutil.CLI) (string, error) {
	return oc.AsAdmin().WithoutNamespace().Run("get").Args("clusterlogforwarders.observability.openshift.io", "-n", clf.namespace, clf.name, "-ojsonpath={.spec.collector.networkPolicy.ruleSet}").Output()
}

func (clf *clusterlogforwarder) updateNetworkPolicyRuleSet(oc *exutil.CLI, ruleSet string) error {
	var patch string
	if ruleSet == "" {
		e2e.Logf("removing network policy configurations from clf/%s", clf.name)
		patch = `[{"path": "/spec/collector/networkPolicy", "op": "remove"}]`
	} else {
		currentRS, err := clf.getNetworkPolicyRuleSet(oc)
		if err != nil {
			return fmt.Errorf("can't get current networkPolicy ruleSet: %v", err)
		}
		if currentRS == ruleSet {
			e2e.Logf("ruleSet is already set to %s, no action needed", ruleSet)
			return nil
		}
		e2e.Logf("update ruleSet to %s...", ruleSet)
		if currentRS == "" {
			patch = `[{"op": "add", "path": "/spec/collector/networkPolicy", "value": {"ruleSet": "` + ruleSet + `"}}]`
		} else {
			patch = `[{"op": "replace", "path": "/spec/collector/networkPolicy/ruleSet", "value": "` + ruleSet + `"}]`
		}
	}
	return oc.AsAdmin().WithoutNamespace().Run("patch").Args("clusterlogforwarders.observability.openshift.io/"+clf.name, "-n", clf.namespace, "-p", patch, "--type=json").Execute()
}

func (clf *clusterlogforwarder) checkNetworkPolicy(oc *exutil.CLI) {
	resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitForResourceToAppear(oc)
}

func (clf *clusterlogforwarder) validateNetworkPolicy(oc *exutil.CLI, ruleSet string, extraEgressPorts, extraIngressPorts []Port) error {
	np, err := getNetworkPolicy(oc, clf.namespace, "collector-"+clf.name)
	if err != nil {
		return err
	}
	//check labels
	if np.ObjectMeta.Labels["app.kubernetes.io/instance"] != np.Spec.PodSelector.MatchLabels["app.kubernetes.io/instance"] || np.ObjectMeta.Labels["app.kubernetes.io/instance"] != clf.name {
		e2e.Logf("Label: app.kubernetes.io/instance=%s, podSelector: app.kubernetes.io/instance=%s", np.ObjectMeta.Labels["app.kubernetes.io/instance"], np.Spec.PodSelector.MatchLabels["app.kubernetes.io/instance"])
		return fmt.Errorf("label or podSelector doesn't match the pod label")
	}
	if !contain(np.Spec.PolicyTypes, "Ingress") || !contain(np.Spec.PolicyTypes, "Egress") {
		e2e.Logf("policyTypes are: %v", np.Spec.PolicyTypes)
		return fmt.Errorf("policyTypes doesn't have 'Ingress' or 'Egress'")
	}
	//check egress and ingress by ruleSet
	switch ruleSet {
	case "AllowAllIngressEgress":
		{
			var convertedNP NetworkPolicy
			expectedYaml := `
spec:
  egress:
  - {}
  ingress:
  - {}`
			_ = yaml.Unmarshal([]byte(expectedYaml), &convertedNP)
			if !(reflect.DeepEqual(convertedNP.Spec.Egress, np.Spec.Egress)) || !(reflect.DeepEqual(convertedNP.Spec.Ingress, np.Spec.Ingress)) {
				output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkPolicy", "-n", clf.namespace, "collector-"+clf.name, "-oyaml").Output()
				e2e.Logf("np: \n%s\nerror: %v", output, err)
				return fmt.Errorf("can't find expected configurations in networkpolicy/collector-%s", clf.name)
			}
		}
	case "RestrictIngressEgress":
		{
			expectedYaml := `
spec:
  egress:
  - ports:
    - port: dns
      protocol: UDP
    - port: 6443
      protocol: TCP
  ingress:
  - ports:
    - port: 24231
      protocol: TCP
`
			var expectedIngressEgress NetworkPolicy
			_ = yaml.Unmarshal([]byte(expectedYaml), &expectedIngressEgress)
			if len(extraEgressPorts) > 0 {
				expectedIngressEgress.Spec.Egress[0].Ports = append(expectedIngressEgress.Spec.Egress[0].Ports, extraEgressPorts...)
			}
			if len(extraIngressPorts) > 0 {
				expectedIngressEgress.Spec.Ingress[0].Ports = append(expectedIngressEgress.Spec.Ingress[0].Ports, extraIngressPorts...)
			}
			if !containsPortsSubset(np.Spec.Ingress[0].Ports, expectedIngressEgress.Spec.Ingress[0].Ports) || !containsPortsSubset(np.Spec.Egress[0].Ports, expectedIngressEgress.Spec.Egress[0].Ports) {
				output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkPolicy", "-n", clf.namespace, "collector-"+clf.name, "-oyaml").Output()
				e2e.Logf("np: \n%s\nerror: %v", output, err)
				return fmt.Errorf("can't find expected ingress/egress in networkpolicy/collector-%s", clf.name)
			}
		}
	default:
		return fmt.Errorf("unsupported ruleSet %s", ruleSet)
	}
	return nil
}

// hasPort checks if a specific target Port is present in the provided list of Ports.
func hasPort(ports []Port, target Port) bool {
	for _, p := range ports {
		if p.Port == target.Port && p.Protocol == target.Protocol {
			return true
		}
	}
	return false
}

// containsPortsSubset checks if the main configuration contains all Ports
// defined in the subset configuration's egress/ingress rule.
func containsPortsSubset(mainConfig, subsetConfig []Port) bool {
	if len(subsetConfig) == 0 {
		return true
	}
	for _, requiredPort := range subsetConfig {
		if !hasPort(mainConfig, requiredPort) {
			return false
		}
	}
	return true
}

type logFileMetricExporter struct {
	name          string
	namespace     string
	template      string
	waitPodsReady bool
}

func (lfme *logFileMetricExporter) create(oc *exutil.CLI, optionalParameters ...string) {
	if lfme.name == "" {
		lfme.name = "instance"
	}
	if lfme.namespace == "" {
		lfme.namespace = loggingNS
	}
	if lfme.template == "" {
		lfme.template = testdata.FixturePath("logging", "logfilemetricexporter", "lfme.yaml")
	}

	parameters := []string{"-f", lfme.template, "-p", "NAME=" + lfme.name, "NAMESPACE=" + lfme.namespace}
	if len(optionalParameters) > 0 {
		parameters = append(parameters, optionalParameters...)
	}

	file, processErr := processTemplate(oc, parameters...)
	defer os.Remove(file)
	if processErr != nil {
		e2e.Failf("error processing file: %v", processErr)
	}
	err := oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", file, "-n", lfme.namespace).Execute()
	if err != nil {
		e2e.Failf("error creating logfilemetricexporter: %v", err)
	}
	resource{"logfilemetricexporter", lfme.name, lfme.namespace}.WaitForResourceToAppear(oc)
	if lfme.waitPodsReady {
		WaitForDaemonsetPodsToBeReady(oc, lfme.namespace, "logfilesmetricexporter")
	}
}

func (lfme *logFileMetricExporter) delete(oc *exutil.CLI) {
	err := resource{"logfilemetricexporter", lfme.name, lfme.namespace}.clear(oc)
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("logfilemetricexporter/%s in project/%s is not deleted", lfme.name, lfme.namespace))
	err = resource{"daemonset", "logfilesmetricexporter", lfme.namespace}.WaitUntilResourceIsGone(oc)
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("ds/logfilesmetricexporter in project/%s is not deleted", lfme.namespace))
}

func (lfme *logFileMetricExporter) getNetworkPolicyRuleSet(oc *exutil.CLI) (string, error) {
	return oc.AsAdmin().WithoutNamespace().Run("get").Args("logfilemetricexporters.logging.openshift.io", "-n", lfme.namespace, lfme.name, "-ojsonpath={.spec.networkPolicy.ruleSet}").Output()
}

func (lfme *logFileMetricExporter) updateNetworkPolicyRuleSet(oc *exutil.CLI, ruleSet string) error {
	var patch string
	if ruleSet == "" {
		e2e.Logf("removing network policy configurations from lfme/%s", lfme.name)
		patch = `[{"path": "/spec/networkPolicy", "op": "remove"}]`
	} else {
		currentRS, err := lfme.getNetworkPolicyRuleSet(oc)
		if err != nil {
			return fmt.Errorf("can't get current networkPolicy ruleSet: %v", err)
		}
		if currentRS == ruleSet {
			e2e.Logf("ruleSet is already set to %s, no action needed", ruleSet)
			return nil
		}
		e2e.Logf("update ruleSet to %s...", ruleSet)
		if currentRS == "" {
			patch = `[{"op": "add", "path": "/spec/networkPolicy", "value": {"ruleSet": "` + ruleSet + `"}}]`
		} else {
			patch = `[{"op": "replace", "path": "/spec/networkPolicy/ruleSet", "value": "` + ruleSet + `"}]`
		}
	}
	return oc.AsAdmin().WithoutNamespace().Run("patch").Args("logfilemetricexporters.logging.openshift.io/"+lfme.name, "-n", lfme.namespace, "-p", patch, "--type=json").Execute()
}

func (lfme *logFileMetricExporter) checkNetworkPolicy(oc *exutil.CLI) {
	resource{"networkpolicy", "lfme-logfilesmetricexporter", lfme.namespace}.WaitForResourceToAppear(oc)
}

func (lfme *logFileMetricExporter) validateNetworkPolicy(oc *exutil.CLI, ruleSet string) error {
	np, err := getNetworkPolicy(oc, lfme.namespace, "lfme-logfilesmetricexporter")
	if err != nil {
		return err
	}
	//check egress and ingress by ruleSet
	var convertedNP NetworkPolicy
	var expectedYaml string
	switch ruleSet {
	case "AllowAllIngressEgress":
		{
			expectedYaml = `
spec:
  egress:
  - {}
  ingress:
  - {}
  podSelector:
    matchLabels:
      app.kubernetes.io/component: logfilesmetricexporter
      app.kubernetes.io/instance: instance
      app.kubernetes.io/managed-by: cluster-logging-operator
      app.kubernetes.io/name: logfilesmetricexporter
      app.kubernetes.io/part-of: cluster-logging
  policyTypes:
  - Ingress
  - Egress
`
		}
	case "AllowIngressMetrics":
		{
			expectedYaml = `
spec:
  egress:
  - ports:
    - port: 6443
      protocol: TCP
    - port: dns
      protocol: UDP
  ingress:
  - ports:
    - port: 2112
      protocol: TCP
  podSelector:
    matchLabels:
      app.kubernetes.io/component: logfilesmetricexporter
      app.kubernetes.io/instance: instance
      app.kubernetes.io/managed-by: cluster-logging-operator
      app.kubernetes.io/name: logfilesmetricexporter
      app.kubernetes.io/part-of: cluster-logging
  policyTypes:
  - Ingress
  - Egress
`
		}
	default:
		return fmt.Errorf("unsupported ruleSet %s", ruleSet)
	}
	_ = yaml.Unmarshal([]byte(expectedYaml), &convertedNP)
	if !(reflect.DeepEqual(convertedNP.Spec, np.Spec)) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkPolicy", "-n", lfme.namespace, "lfme-logfilesmetricexporter", "-oyaml").Output()
		e2e.Logf("np: \n%s\nerror: %v", output, err)
		return fmt.Errorf("can't find expected configurations in networkpolicy/lfme-logfilesmetricexporter")
	}
	return nil
}

func deleteNamespace(oc *exutil.CLI, ns string) {
	err := oc.AdminKubeClient().CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			err = nil
		}
	}
	o.Expect(err).NotTo(o.HaveOccurred())
	err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		_, err = oc.AdminKubeClient().CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Namespace %s is not deleted in 3 minutes", ns))
}

func getStorageClassName(oc *exutil.CLI) (string, error) {
	scs, err := oc.AdminKubeClient().StorageV1().StorageClasses().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	if len(scs.Items) == 0 {
		return "", fmt.Errorf("there is no storageclass in the cluster")
	}
	for _, sc := range scs.Items {
		if sc.ObjectMeta.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return sc.Name, nil
		}
	}
	return scs.Items[0].Name, nil
}

// Assert the status of a resource
func assertResourceStatus(oc *exutil.CLI, kind, name, namespace, jsonpath, exptdStatus string) {
	parameters := []string{kind, name, "-o", "jsonpath=" + jsonpath}
	if namespace != "" {
		parameters = append(parameters, "-n", namespace)
	}
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		status, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(parameters...).Output()
		if err != nil {
			return false, err
		}
		if strings.Compare(status, exptdStatus) != 0 {
			return false, nil
		}
		return true, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("%s/%s value for %s is not %s", kind, name, jsonpath, exptdStatus))
}

func getRouteAddress(oc *exutil.CLI, ns, routeName string) string {
	route, err := oc.AdminRouteClient().RouteV1().Routes(ns).Get(context.Background(), routeName, metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())
	return route.Spec.Host
}

func getSAToken(oc *exutil.CLI, name, ns string) string {
	token, err := oc.AsAdmin().WithoutNamespace().Run("create").Args("token", name, "-n", ns).Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	return token
}

// enableClusterMonitoring add label `openshift.io/cluster-monitoring: "true"` to the project, and create role/prometheus-k8s rolebinding/prometheus-k8s in the namespace
func enableClusterMonitoring(oc *exutil.CLI, namespace string) {
	err := oc.AsAdmin().WithoutNamespace().Run("label").Args("ns", namespace, "openshift.io/cluster-monitoring=true").Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	file := testdata.FixturePath("logging", "prometheus-k8s-rbac.yaml")
	err = oc.AsAdmin().WithoutNamespace().Run("apply").Args("-n", namespace, "-f", file).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

// queryPrometheus returns the promtheus metrics which match the query string
// token: the user token used to run the http request, if it's not specified, it will use the token of sa/prometheus-k8s in openshift-monitoring project
// path: the api path, for example: /api/v1/query?
// query: the metric/alert you want to search, e.g.: es_index_namespaces_total
// action: it can be "GET", "get", "Get", "POST", "post", "Post"
func queryPrometheus(oc *exutil.CLI, token string, path string, query string, action string) (*prometheusQueryResult, error) {
	var bearerToken string
	var err error
	if token == "" {
		bearerToken = getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
	} else {
		bearerToken = token
	}
	address := "https://" + getRouteAddress(oc, "openshift-monitoring", "prometheus-k8s")

	h := make(http.Header)
	h.Add("Content-Type", "application/json")
	h.Add("Authorization", "Bearer "+bearerToken)

	params := url.Values{}
	if len(query) > 0 {
		params.Add("query", query)
	}

	var p prometheusQueryResult
	resp, err := doHTTPRequest(h, address, path, params.Encode(), action, true, 5, nil, 200)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(resp, &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func getMetric(oc *exutil.CLI, token, query string) ([]metric, error) {
	res, err := queryPrometheus(oc, token, "/api/v1/query", query, "GET")
	if err != nil {
		return []metric{}, err
	}
	return res.Data.Result, nil
}

func checkMetric(oc *exutil.CLI, token, query string, timeInMinutes int) {
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, time.Duration(timeInMinutes)*time.Minute, true, func(context.Context) (done bool, err error) {
		metrics, err := getMetric(oc, token, query)
		if err != nil {
			return false, err
		}
		if len(metrics) == 0 {
			e2e.Logf("no metrics found by query: %s, try next time", query)
			return false, nil
		}
		return true, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("can't find metrics by %s in %d minutes", query, timeInMinutes))
}

func findMetric(oc *exutil.CLI, token, query string, timeInMinutes int) bool {
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, time.Duration(timeInMinutes)*time.Minute, true, func(context.Context) (done bool, err error) {
		metrics, err := getMetric(oc, token, query)
		if err != nil {
			return false, err
		}
		if len(metrics) == 0 {
			e2e.Logf("no metric match query: %s, try next time", query)
			return false, nil
		}
		return true, nil
	})
	return err == nil
}

func checkCLOMetric(oc *exutil.CLI, token string) {
	//metrics exposed by cluster-logging-operator
	metrics := []string{"openshift_logging:log_forwarder_pipelines:sum", "openshift_logging:log_forwarders:sum", "openshift_logging:log_forwarder_input_type:sum", "openshift_logging:log_forwarder_output_type:sum", "container_file_descriptors", "kubelet_running_containers", "container_cpu_usage_seconds_total"}
	//merics need more time
	slowMetrics := []string{"cluster:log_logged_bytes_total:sum", "collector:received_events:sum_rate", "openshift_logging:vector_component_received_bytes_total:rate5m", "vector_buffer_byte_size{buffer_type=\"disk\"}", "vector_component_sent_bytes_total", "vector_open_files", "vector_component_received_event_bytes_total"}
	var missMetics []string
	for _, metricName := range metrics {
		if !findMetric(oc, token, metricName, 3) {
			missMetics = append(missMetics, metricName)
		}
	}
	for _, metricName := range slowMetrics {
		if !findMetric(oc, token, metricName, 5) {
			missMetics = append(missMetics, metricName)
		}
	}
	o.Expect(len(missMetics) == 0).Should(o.BeTrue(), fmt.Sprintf("can't find metrics %v in given time", missMetics))
}

func getAlert(oc *exutil.CLI, token, alertSelector string) ([]alert, error) {
	var al []alert
	alerts, err := queryPrometheus(oc, token, "/api/v1/alerts", "", "GET")
	if err != nil {
		return al, err
	}
	for i := 0; i < len(alerts.Data.Alerts); i++ {
		if alerts.Data.Alerts[i].Labels.AlertName == alertSelector {
			al = append(al, alerts.Data.Alerts[i])
		}
	}
	return al, nil
}

func checkAlert(oc *exutil.CLI, token, alertName, status string, timeInMinutes int) {
	err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, time.Duration(timeInMinutes)*time.Minute, true, func(context.Context) (done bool, err error) {
		alerts, err := getAlert(oc, token, alertName)
		if err != nil {
			return false, err
		}
		for _, alert := range alerts {
			if strings.Contains(status, alert.State) {
				return true, nil
			}
		}
		e2e.Logf("Waiting for alert %s to be in state %s...", alertName, status)
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("%s alert is not %s in %d minutes", alertName, status, timeInMinutes))
}

// WaitUntilPodsAreGone waits for pods selected with labelselector to be removed
func WaitUntilPodsAreGone(oc *exutil.CLI, namespace string, labelSelector string) {
	err := wait.PollUntilContextTimeout(context.Background(), 3*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pods", "--selector="+labelSelector, "-n", namespace).Output()
		if err != nil {
			return false, err
		}
		errstring := fmt.Sprintf("%v", output)
		if strings.Contains(errstring, "No resources found") {
			return true, nil
		}
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Error waiting for pods to be removed using label selector %s", labelSelector))
}

// Check logs from resource
func checkLogsFromRs(oc *exutil.CLI, kind, name, namespace, containerName, expected string) {
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args(kind+`/`+name, "-n", namespace, "-c", containerName).Output()
		if err != nil {
			e2e.Logf("Can't get logs from resource, error: %s. Trying again", err)
			return false, nil
		}
		if matched, _ := regexp.Match(expected, []byte(output)); !matched {
			e2e.Logf("Can't find the expected string\n")
			return false, nil
		}
		e2e.Logf("Check the logs succeed!!\n")
		return true, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("%s is not expected for %s", expected, name))
}

func getCurrentCSVFromPackage(oc *exutil.CLI, source, channel, packagemanifest string) string {
	var currentCSV string
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("packagemanifest", "-n", "openshift-marketplace", "-l", "catalog="+source, "-ojsonpath={.items}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	packMS := []PackageManifest{}
	json.Unmarshal([]byte(output), &packMS)
	for _, pm := range packMS {
		if pm.Name == packagemanifest {
			for _, channels := range pm.Status.Channels {
				if channels.Name == channel {
					currentCSV = channels.CurrentCSV
					break
				}
			}
		}
	}
	return currentCSV
}

func checkNetworkType(oc *exutil.CLI) string {
	output, err := oc.WithoutNamespace().AsAdmin().Run("get").Args("network.operator", "cluster", "-o=jsonpath={.spec.defaultNetwork.type}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	return strings.ToLower(output)
}

func getAppDomain(oc *exutil.CLI) (string, error) {
	subDomain, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("ingresses.config/cluster", "-ojsonpath={.spec.domain}").Output()
	if err != nil {
		return "", err
	}
	return subDomain, nil
}

type certsConf struct {
	serverName string
	namespace  string
	passPhrase string //client private key passphrase
}

func (certs certsConf) generateCerts(oc *exutil.CLI, keysPath string) {
	generateCertsSH := testdata.FixturePath("logging", "external-log-stores", "cert_generation.sh")
	domain, err := getAppDomain(oc)
	o.Expect(err).NotTo(o.HaveOccurred())
	cmd := []string{generateCertsSH, keysPath, certs.namespace, certs.serverName, domain}
	if certs.passPhrase != "" {
		cmd = append(cmd, certs.passPhrase)
	}
	err = exec.Command("sh", cmd...).Run()
	o.Expect(err).NotTo(o.HaveOccurred())
}

// expect: true means we want the resource contain/compare with the expectedContent, false means the resource is expected not to compare with/contain the expectedContent;
// compare: true means compare the expectedContent with the resource content, false means check if the resource contains the expectedContent;
// args are the arguments used to execute command `oc.AsAdmin.WithoutNamespace().Run("get").Args(args...).Output()`;
func checkResource(oc *exutil.CLI, expect bool, compare bool, expectedContent string, args []string) {
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(args...).Output()
		if err != nil {
			if strings.Contains(output, "NotFound") {
				return false, nil
			}
			return false, err
		}
		if compare {
			res := strings.Compare(output, expectedContent)
			if (res == 0 && expect) || (res != 0 && !expect) {
				return true, nil
			}
			return false, nil
		}
		res := strings.Contains(output, expectedContent)
		if (res && expect) || (!res && !expect) {
			return true, nil
		}
		return false, nil
	})
	if expect {
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("The content doesn't match/contain %s", expectedContent))
	} else {
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("The %s still exists in the resource", expectedContent))
	}
}

type rsyslog struct {
	serverName          string //the name of the rsyslog server, it's also used to name the svc/cm/sa/secret
	namespace           string //the namespace where the rsyslog server deployed in
	tls                 bool
	secretName          string //the name of the secret for the collector to use
	loggingNS           string //the namespace where the collector pods deployed in
	clientKeyPassphrase string //client private key passphrase
}

func (r rsyslog) createPipelineSecret(oc *exutil.CLI, keysPath string) {
	secret := resource{"secret", r.secretName, r.loggingNS}
	cmd := []string{"secret", "generic", secret.name, "-n", secret.namespace, "--from-file=ca-bundle.crt=" + keysPath + "/ca.crt"}
	if r.clientKeyPassphrase != "" {
		cmd = append(cmd, "--from-file=tls.key="+keysPath+"/client.key", "--from-file=tls.crt="+keysPath+"/client.crt", "--from-literal=passphrase="+r.clientKeyPassphrase)
	}

	err := oc.AsAdmin().WithoutNamespace().Run("create").Args(cmd...).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	secret.WaitForResourceToAppear(oc)
}

func (r rsyslog) deploy(oc *exutil.CLI) {
	// create SA
	sa := resource{"serviceaccount", r.serverName, r.namespace}
	err := oc.WithoutNamespace().Run("create").Args("serviceaccount", sa.name, "-n", sa.namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	sa.WaitForResourceToAppear(oc)
	err = oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "add-scc-to-user", "privileged", fmt.Sprintf("system:serviceaccount:%s:%s", r.namespace, r.serverName), "-n", r.namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	filePath := []string{"logging", "external-log-stores", "rsyslog"}
	// create secrets if needed
	if r.tls {
		o.Expect(r.secretName).NotTo(o.BeEmpty())
		// create a temporary directory
		baseDir := testdata.FixturePath("logging")
		keysPath := filepath.Join(baseDir, "temp"+getRandomString())
		defer exec.Command("rm", "-r", keysPath).Output()
		err = os.MkdirAll(keysPath, 0755)
		o.Expect(err).NotTo(o.HaveOccurred())

		cert := certsConf{r.serverName, r.namespace, r.clientKeyPassphrase}
		cert.generateCerts(oc, keysPath)
		// create pipelinesecret
		r.createPipelineSecret(oc, keysPath)
		// create secret for rsyslog server
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", r.serverName, "-n", r.namespace, "--from-file=server.key="+keysPath+"/server.key", "--from-file=server.crt="+keysPath+"/server.crt", "--from-file=ca_bundle.crt="+keysPath+"/ca.crt").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		filePath = append(filePath, "secure")
	} else {
		filePath = append(filePath, "insecure")
	}

	// create configmap/deployment/svc
	cm := resource{"configmap", r.serverName, r.namespace}
	cmFilePath := append(filePath, "configmap.yaml")
	cmFile := testdata.FixturePath(cmFilePath...)
	err = cm.applyFromTemplate(oc, "-f", cmFile, "-n", r.namespace, "-p", "NAMESPACE="+r.namespace, "-p", "NAME="+r.serverName)
	o.Expect(err).NotTo(o.HaveOccurred())

	deploy := resource{"deployment", r.serverName, r.namespace}
	deployFilePath := append(filePath, "deployment.yaml")
	deployFile := testdata.FixturePath(deployFilePath...)
	err = deploy.applyFromTemplate(oc, "-f", deployFile, "-n", r.namespace, "-p", "NAMESPACE="+r.namespace, "-p", "NAME="+r.serverName)
	o.Expect(err).NotTo(o.HaveOccurred())
	WaitForDeploymentPodsToBeReady(oc, r.namespace, r.serverName)

	svc := resource{"svc", r.serverName, r.namespace}
	svcFilePath := append(filePath, "svc.yaml")
	svcFile := testdata.FixturePath(svcFilePath...)
	err = svc.applyFromTemplate(oc, "-f", svcFile, "-n", r.namespace, "-p", "NAMESPACE="+r.namespace, "-p", "NAME="+r.serverName)
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (r rsyslog) remove(oc *exutil.CLI) {
	resource{"serviceaccount", r.serverName, r.namespace}.clear(oc)
	if r.tls {
		resource{"secret", r.serverName, r.namespace}.clear(oc)
		resource{"secret", r.secretName, r.loggingNS}.clear(oc)
	}
	resource{"configmap", r.serverName, r.namespace}.clear(oc)
	resource{"deployment", r.serverName, r.namespace}.clear(oc)
	resource{"svc", r.serverName, r.namespace}.clear(oc)
}

func (r rsyslog) getPodName(oc *exutil.CLI) string {
	pods, err := oc.AdminKubeClient().CoreV1().Pods(r.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "component=" + r.serverName})
	o.Expect(err).NotTo(o.HaveOccurred())
	var names []string
	for i := 0; i < len(pods.Items); i++ {
		names = append(names, pods.Items[i].Name)
	}
	return names[0]
}

func (r rsyslog) checkData(oc *exutil.CLI, expect bool, filename string) {
	cmd := "ls -l /var/log/clf/" + filename
	if expect {
		err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(context.Context) (done bool, err error) {
			stdout, err := e2eoutput.RunHostCmdWithRetries(r.namespace, r.getPodName(oc), cmd, 3*time.Second, 15*time.Second)
			if err != nil {
				if strings.Contains(err.Error(), "No such file or directory") {
					return false, nil
				}
				return false, err
			}
			return strings.Contains(stdout, filename), nil
		})
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("The %s doesn't exist", filename))
	} else {
		err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(context.Context) (done bool, err error) {
			stdout, err := e2eoutput.RunHostCmdWithRetries(r.namespace, r.getPodName(oc), cmd, 3*time.Second, 15*time.Second)
			if err != nil {
				if strings.Contains(err.Error(), "No such file or directory") {
					return true, nil
				}
				return false, err
			}
			return strings.Contains(stdout, "No such file or directory"), nil
		})
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("The %s exists", filename))
	}
}

func (r rsyslog) checkDataContent(oc *exutil.CLI, expect bool, filename string, expected string) {
	re := regexp.MustCompile(`.*` + expected + `.*`)
	cmd := "tail -10 /var/log/clf/" + filename
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(context.Context) (done bool, err error) {
		stdout, err := e2eoutput.RunHostCmdWithRetries(r.namespace, r.getPodName(oc), cmd, 3*time.Second, 15*time.Second)
		if err != nil {
			return false, err
		}
		return re.MatchString(stdout), nil
	})
	if expect {
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("The file %s not exist or the file doesn't contain %s", filename, expected))
	} else {
		compat_otp.AssertWaitPollWithErr(err, fmt.Sprintf("The file %s shouldn't include %s", filename, expected))
	}
}

type fluentdServer struct {
	serverName                 string //the name of the fluentd server, it's also used to name the svc/cm/sa/secret
	namespace                  string //the namespace where the fluentd server deployed in
	serverAuth                 bool
	clientAuth                 bool   // only can be set when serverAuth is true
	clientPrivateKeyPassphrase string //only can be set when clientAuth is true
	sharedKey                  string //if it's not empty, means the shared_key is set, only works when serverAuth is true
	secretName                 string //the name of the secret for the collector to use
	loggingNS                  string //the namespace where the collector pods deployed in
	inPluginType               string //forward or http
}

func (f fluentdServer) createPipelineSecret(oc *exutil.CLI, keysPath string) {
	secret := resource{"secret", f.secretName, f.loggingNS}
	cmd := []string{"secret", "generic", secret.name, "-n", secret.namespace, "--from-file=ca-bundle.crt=" + keysPath + "/ca.crt"}
	if f.clientAuth {
		cmd = append(cmd, "--from-file=tls.key="+keysPath+"/client.key", "--from-file=tls.crt="+keysPath+"/client.crt")
	}
	if f.clientPrivateKeyPassphrase != "" {
		cmd = append(cmd, "--from-literal=passphrase="+f.clientPrivateKeyPassphrase)
	}
	if f.sharedKey != "" {
		cmd = append(cmd, "--from-literal=shared_key="+f.sharedKey)
	}
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args(cmd...).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	secret.WaitForResourceToAppear(oc)
}

func (f fluentdServer) deploy(oc *exutil.CLI) {
	// create SA
	sa := resource{"serviceaccount", f.serverName, f.namespace}
	err := oc.WithoutNamespace().Run("create").Args("serviceaccount", sa.name, "-n", sa.namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	sa.WaitForResourceToAppear(oc)
	//err = oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "add-scc-to-user", "privileged", fmt.Sprintf("system:serviceaccount:%s:%s", f.namespace, f.serverName), "-n", f.namespace).Execute()
	//o.Expect(err).NotTo(o.HaveOccurred())
	filePath := []string{"logging", "external-log-stores", "fluentd"}

	// create secrets if needed
	if f.serverAuth {
		o.Expect(f.secretName).NotTo(o.BeEmpty())
		filePath = append(filePath, "secure")
		// create a temporary directory
		baseDir := testdata.FixturePath("logging")
		keysPath := filepath.Join(baseDir, "temp"+getRandomString())
		defer exec.Command("rm", "-r", keysPath).Output()
		err = os.MkdirAll(keysPath, 0755)
		o.Expect(err).NotTo(o.HaveOccurred())
		//generate certs
		cert := certsConf{f.serverName, f.namespace, f.clientPrivateKeyPassphrase}
		cert.generateCerts(oc, keysPath)
		//create pipelinesecret
		f.createPipelineSecret(oc, keysPath)
		//create secret for fluentd server
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", f.serverName, "-n", f.namespace, "--from-file=ca-bundle.crt="+keysPath+"/ca.crt", "--from-file=tls.key="+keysPath+"/server.key", "--from-file=tls.crt="+keysPath+"/server.crt", "--from-file=ca.key="+keysPath+"/ca.key").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

	} else {
		filePath = append(filePath, "insecure")
	}

	// create configmap/deployment/svc
	cm := resource{"configmap", f.serverName, f.namespace}
	//when prefix is http-, the fluentdserver using http inplugin.
	cmFilePrefix := ""
	if f.inPluginType == "http" {
		cmFilePrefix = "http-"
	}

	var cmFileName string
	if !f.serverAuth {
		cmFileName = cmFilePrefix + "configmap.yaml"
	} else {
		if f.clientAuth {
			if f.sharedKey != "" {
				cmFileName = "cm-mtls-share.yaml"
			} else {
				cmFileName = cmFilePrefix + "cm-mtls.yaml"
			}
		} else {
			if f.sharedKey != "" {
				cmFileName = "cm-serverauth-share.yaml"
			} else {
				cmFileName = cmFilePrefix + "cm-serverauth.yaml"
			}
		}
	}
	cmFilePath := append(filePath, cmFileName)
	cmFile := testdata.FixturePath(cmFilePath...)
	cCmCmd := []string{"-f", cmFile, "-n", f.namespace, "-p", "NAMESPACE=" + f.namespace, "-p", "NAME=" + f.serverName}
	if f.sharedKey != "" {
		cCmCmd = append(cCmCmd, "-p", "SHARED_KEY="+f.sharedKey)
	}
	err = cm.applyFromTemplate(oc, cCmCmd...)
	o.Expect(err).NotTo(o.HaveOccurred())

	deploy := resource{"deployment", f.serverName, f.namespace}
	deployFilePath := append(filePath, "deployment.yaml")
	deployFile := testdata.FixturePath(deployFilePath...)
	err = deploy.applyFromTemplate(oc, "-f", deployFile, "-n", f.namespace, "-p", "NAMESPACE="+f.namespace, "-p", "NAME="+f.serverName)
	o.Expect(err).NotTo(o.HaveOccurred())
	WaitForDeploymentPodsToBeReady(oc, f.namespace, f.serverName)

	err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("-n", f.namespace, "deployment", f.serverName, "--name="+f.serverName).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (f fluentdServer) remove(oc *exutil.CLI) {
	resource{"serviceaccount", f.serverName, f.namespace}.clear(oc)
	if f.serverAuth {
		resource{"secret", f.serverName, f.namespace}.clear(oc)
		resource{"secret", f.secretName, f.loggingNS}.clear(oc)
	}
	resource{"configmap", f.serverName, f.namespace}.clear(oc)
	resource{"deployment", f.serverName, f.namespace}.clear(oc)
	resource{"svc", f.serverName, f.namespace}.clear(oc)
}

func (f fluentdServer) getPodName(oc *exutil.CLI) string {
	pods, err := oc.AdminKubeClient().CoreV1().Pods(f.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "component=" + f.serverName})
	o.Expect(err).NotTo(o.HaveOccurred())
	var names []string
	for i := 0; i < len(pods.Items); i++ {
		names = append(names, pods.Items[i].Name)
	}
	return names[0]
}

// check the data in fluentd server
// filename is the name of a file you want to check
// expect true means you expect the file to exist, false means the file is not expected to exist
func (f fluentdServer) checkData(oc *exutil.CLI, expect bool, filename string) {
	cmd := "ls -l /fluentd/log/" + filename
	if expect {
		err := wait.PollUntilContextTimeout(context.Background(), 20*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
			stdout, err := e2eoutput.RunHostCmdWithRetries(f.namespace, f.getPodName(oc), cmd, 3*time.Second, 15*time.Second)
			if err != nil {
				if strings.Contains(err.Error(), "No such file or directory") {
					return false, nil
				}
				return false, err
			}
			return strings.Contains(stdout, filename), nil
		})
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("The %s doesn't exist", filename))
	} else {
		err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 60*time.Second, true, func(context.Context) (done bool, err error) {
			stdout, err := e2eoutput.RunHostCmdWithRetries(f.namespace, f.getPodName(oc), cmd, 3*time.Second, 15*time.Second)
			if err != nil {
				if strings.Contains(err.Error(), "No such file or directory") {
					return true, nil
				}
				return false, err
			}
			return strings.Contains(stdout, "No such file or directory"), nil
		})
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("The %s exists", filename))
	}

}

// return the infrastructureName. For example:  anli922-jglp4
func getInfrastructureName(oc *exutil.CLI) string {
	infrastructureName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure/cluster", "-o=jsonpath={.status.infrastructureName}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	return infrastructureName
}

func getDataFromKafkaConsumerPod(oc *exutil.CLI, kafkaNS, consumerPod string) ([]LogEntity, error) {
	e2e.Logf("get logs from kakfa consumerPod %s", consumerPod)
	var logs []LogEntity
	//wait up to 5 minutes for logs appear in consumer pod
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 300*time.Second, true, func(context.Context) (done bool, err error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("-n", kafkaNS, consumerPod, "--since=30s", "--tail=30").Output()
		if err != nil {
			e2e.Logf("error when oc logs consumer pod, continue")
			return false, nil
		}
		for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
			//exclude those kafka-consumer logs, for exampe:
			//[2024-11-09 07:25:47,953] WARN [Consumer clientId=consumer-console-consumer-99163-1, groupId=console-consumer-99163] Error while fetching metadata with correlation id 165
			//: {topic-logging-app=UNKNOWN_TOPIC_OR_PARTITION} (org.apache.kafka.clients.NetworkClient)
			r, _ := regexp.Compile(`^{"@timestamp":.*}`)
			if r.MatchString(line) {
				var log LogEntity
				err = json.Unmarshal([]byte(line), &log)
				if err != nil {
					continue
				}
				logs = append(logs, log)
			} else {
				continue
			}
		}
		if len(logs) > 0 {
			return true, nil
		} else {
			e2e.Logf("can not find logs in consumerPod %s, continue", consumerPod)
			return false, nil
		}
	})
	if err != nil {
		return logs, fmt.Errorf("can not find consumer logs in 3 minutes")
	}
	return logs, nil
}

func getDataFromKafkaByNamespace(oc *exutil.CLI, kafkaNS, consumerPod, namespace string) ([]LogEntity, error) {
	data, err := getDataFromKafkaConsumerPod(oc, kafkaNS, consumerPod)
	if err != nil {
		return nil, err
	}
	var logs []LogEntity
	for _, log := range data {
		if log.Kubernetes.NamespaceName == namespace {
			logs = append(logs, log)
		}
	}
	return logs, nil
}

type kafka struct {
	namespace      string
	kafkasvcName   string
	zoosvcName     string
	authtype       string //Name the kafka folders under testdata same as the authtype (options: plaintext-ssl, sasl-ssl, sasl-plaintext)
	pipelineSecret string //the name of the secret for collectors to use
	collectorType  string //must be specified when auth type is sasl-ssl/sasl-plaintext
	loggingNS      string //the namespace where the collector pods are deployed in
}

func (k kafka) deployZookeeper(oc *exutil.CLI) {
	zookeeperFilePath := testdata.FixturePath("logging", "external-log-stores", "kafka", "zookeeper")
	//create zookeeper configmap/svc/StatefulSet
	configTemplate := filepath.Join(zookeeperFilePath, "configmap.yaml")
	if k.authtype == "plaintext-ssl" {
		configTemplate = filepath.Join(zookeeperFilePath, "configmap-ssl.yaml")
	}
	err := resource{"configmap", k.zoosvcName, k.namespace}.applyFromTemplate(oc, "-n", k.namespace, "-f", configTemplate, "-p", "NAME="+k.zoosvcName, "NAMESPACE="+k.namespace)
	o.Expect(err).NotTo(o.HaveOccurred())

	zoosvcFile := filepath.Join(zookeeperFilePath, "zookeeper-svc.yaml")
	zoosvc := resource{"Service", k.zoosvcName, k.namespace}
	err = zoosvc.applyFromTemplate(oc, "-n", k.namespace, "-f", zoosvcFile, "-p", "NAME="+k.zoosvcName, "-p", "NAMESPACE="+k.namespace)
	o.Expect(err).NotTo(o.HaveOccurred())

	zoosfsFile := filepath.Join(zookeeperFilePath, "zookeeper-statefulset.yaml")
	zoosfs := resource{"StatefulSet", k.zoosvcName, k.namespace}
	err = zoosfs.applyFromTemplate(oc, "-n", k.namespace, "-f", zoosfsFile, "-p", "NAME="+k.zoosvcName, "-p", "NAMESPACE="+k.namespace, "-p", "SERVICENAME="+zoosvc.name, "-p", "CM_NAME="+k.zoosvcName)
	o.Expect(err).NotTo(o.HaveOccurred())
	waitForPodReadyWithLabel(oc, k.namespace, "app="+k.zoosvcName)
}

func (k kafka) deployKafka(oc *exutil.CLI) {
	kafkaFilePath := testdata.FixturePath("logging", "external-log-stores", "kafka")
	kafkaConfigmapTemplate := filepath.Join(kafkaFilePath, k.authtype, "kafka-configmap.yaml")
	consumerConfigmapTemplate := filepath.Join(kafkaFilePath, k.authtype, "consumer-configmap.yaml")

	var keysPath string
	if k.authtype == "sasl-ssl" || k.authtype == "plaintext-ssl" {
		baseDir := testdata.FixturePath("logging")
		keysPath = filepath.Join(baseDir, "temp"+getRandomString())
		defer exec.Command("rm", "-r", keysPath).Output()
		err := os.MkdirAll(keysPath, 0755)
		o.Expect(err).NotTo(o.HaveOccurred())
		generateCertsSH := filepath.Join(kafkaFilePath, "cert_generation.sh")
		stdout, err := exec.Command("sh", generateCertsSH, keysPath, k.namespace).Output()
		if err != nil {
			e2e.Logf("error generating certs: %s", string(stdout))
			e2e.Failf("error generating certs: %v", err)
		}
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", "kafka-cluster-cert", "-n", k.namespace, "--from-file=ca_bundle.jks="+keysPath+"/ca/ca_bundle.jks", "--from-file=cluster.jks="+keysPath+"/cluster/cluster.jks").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
	}

	pipelineSecret := resource{"secret", k.pipelineSecret, k.loggingNS}
	kafkaClientCert := resource{"secret", "kafka-client-cert", k.namespace}
	//create kafka secrets and confimap
	cmdPipeline := []string{"secret", "generic", pipelineSecret.name, "-n", pipelineSecret.namespace}
	cmdClient := []string{"secret", "generic", kafkaClientCert.name, "-n", kafkaClientCert.namespace}
	switch k.authtype {
	case "sasl-plaintext":
		{
			cmdClient = append(cmdClient, "--from-literal=username=admin", "--from-literal=password=admin-secret")
			cmdPipeline = append(cmdPipeline, "--from-literal=username=admin", "--from-literal=password=admin-secret")
			if k.collectorType == "vector" {
				cmdPipeline = append(cmdPipeline, "--from-literal=sasl.enable=True", "--from-literal=sasl.mechanisms=PLAIN")
			}
		}
	case "sasl-ssl":
		{
			cmdClient = append(cmdClient, "--from-file=ca-bundle.jks="+keysPath+"/ca/ca_bundle.jks", "--from-file=ca-bundle.crt="+keysPath+"/ca/ca_bundle.crt", "--from-file=tls.crt="+keysPath+"/client/client.crt", "--from-file=tls.key="+keysPath+"/client/client.key", "--from-literal=username=admin", "--from-literal=password=admin-secret")
			cmdPipeline = append(cmdPipeline, "--from-file=ca-bundle.crt="+keysPath+"/ca/ca_bundle.crt", "--from-literal=username=admin", "--from-literal=password=admin-secret")
			switch k.collectorType {
			case "fluentd":
				{
					cmdPipeline = append(cmdPipeline, "--from-literal=sasl_over_ssl=true")
				}
			case "vector":
				{
					cmdPipeline = append(cmdPipeline, "--from-literal=sasl.enable=True", "--from-literal=sasl.mechanisms=PLAIN", "--from-file=tls.crt="+keysPath+"/client/client.crt", "--from-file=tls.key="+keysPath+"/client/client.key")
				}
			}
		}
	case "plaintext-ssl":
		{
			cmdClient = append(cmdClient, "--from-file=ca-bundle.jks="+keysPath+"/ca/ca_bundle.jks", "--from-file=ca-bundle.crt="+keysPath+"/ca/ca_bundle.crt", "--from-file=tls.crt="+keysPath+"/client/client.crt", "--from-file=tls.key="+keysPath+"/client/client.key")
			cmdPipeline = append(cmdPipeline, "--from-file=ca-bundle.crt="+keysPath+"/ca/ca_bundle.crt", "--from-file=tls.crt="+keysPath+"/client/client.crt", "--from-file=tls.key="+keysPath+"/client/client.key")
		}
	}
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args(cmdClient...).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	kafkaClientCert.WaitForResourceToAppear(oc)
	err = oc.AsAdmin().WithoutNamespace().Run("create").Args(cmdPipeline...).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	pipelineSecret.WaitForResourceToAppear(oc)

	consumerConfigmap := resource{"configmap", "kafka-client", k.namespace}
	err = consumerConfigmap.applyFromTemplate(oc, "-n", k.namespace, "-f", consumerConfigmapTemplate, "-p", "NAME="+consumerConfigmap.name, "NAMESPACE="+consumerConfigmap.namespace)
	o.Expect(err).NotTo(o.HaveOccurred())

	kafkaConfigmap := resource{"configmap", k.kafkasvcName, k.namespace}
	err = kafkaConfigmap.applyFromTemplate(oc, "-n", k.namespace, "-f", kafkaConfigmapTemplate, "-p", "NAME="+kafkaConfigmap.name, "NAMESPACE="+kafkaConfigmap.namespace)
	o.Expect(err).NotTo(o.HaveOccurred())

	//create ClusterRole and ClusterRoleBinding
	rbacFile := filepath.Join(kafkaFilePath, "kafka-rbac.yaml")
	output, err := oc.AsAdmin().WithoutNamespace().Run("process").Args("-n", k.namespace, "-f", rbacFile, "-p", "NAMESPACE="+k.namespace).OutputToFile(getRandomString() + ".json")
	o.Expect(err).NotTo(o.HaveOccurred())
	oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", output, "-n", k.namespace).Execute()

	//create kafka svc
	svcFile := filepath.Join(kafkaFilePath, "kafka-svc.yaml")
	svc := resource{"Service", k.kafkasvcName, k.namespace}
	err = svc.applyFromTemplate(oc, "-f", svcFile, "-n", svc.namespace, "-p", "NAME="+svc.name, "NAMESPACE="+svc.namespace)
	o.Expect(err).NotTo(o.HaveOccurred())

	//create kafka StatefulSet
	sfsFile := filepath.Join(kafkaFilePath, k.authtype, "kafka-statefulset.yaml")
	sfs := resource{"StatefulSet", k.kafkasvcName, k.namespace}
	err = sfs.applyFromTemplate(oc, "-f", sfsFile, "-n", k.namespace, "-p", "NAME="+sfs.name, "-p", "NAMESPACE="+sfs.namespace, "-p", "CM_NAME="+k.kafkasvcName)
	o.Expect(err).NotTo(o.HaveOccurred())
	waitForStatefulsetReady(oc, sfs.namespace, sfs.name)

	//create kafka-consumer deployment
	deployFile := filepath.Join(kafkaFilePath, k.authtype, "kafka-consumer-deployment.yaml")
	deploy := resource{"deployment", "kafka-consumer-" + k.authtype, k.namespace}
	err = deploy.applyFromTemplate(oc, "-f", deployFile, "-n", deploy.namespace, "-p", "NAMESPACE="+deploy.namespace, "NAME="+deploy.name, "CM_NAME=kafka-client")
	o.Expect(err).NotTo(o.HaveOccurred())
	WaitForDeploymentPodsToBeReady(oc, deploy.namespace, deploy.name)
}

func (k kafka) removeZookeeper(oc *exutil.CLI) {
	resource{"configmap", k.zoosvcName, k.namespace}.clear(oc)
	resource{"svc", k.zoosvcName, k.namespace}.clear(oc)
	resource{"statefulset", k.zoosvcName, k.namespace}.clear(oc)
}

func (k kafka) removeKafka(oc *exutil.CLI) {
	resource{"secret", "kafka-client-cert", k.namespace}.clear(oc)
	resource{"secret", "kafka-cluster-cert", k.namespace}.clear(oc)
	resource{"secret", k.pipelineSecret, k.loggingNS}.clear(oc)
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("clusterrole/kafka-node-reader").Execute()
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("clusterrolebinding/kafka-node-reader").Execute()
	resource{"configmap", k.kafkasvcName, k.namespace}.clear(oc)
	resource{"svc", k.kafkasvcName, k.namespace}.clear(oc)
	resource{"statefulset", k.kafkasvcName, k.namespace}.clear(oc)
	resource{"configmap", "kafka-client", k.namespace}.clear(oc)
	resource{"deployment", "kafka-consumer-" + k.authtype, k.namespace}.clear(oc)
}

// deploy amqstream instance, kafka user for predefined topics
// if amqstreams absent, deploy amqstream operator
func (amqi *amqInstance) deploy(oc *exutil.CLI) {
	e2e.Logf("deploy amq instance")
	//initialize kakfa vars
	if amqi.name == "" {
		amqi.name = "my-cluster"
	}
	if amqi.namespace == "" {
		e2e.Failf("error, please define a namespace for amqstream instance")
	}
	if amqi.user == "" {
		amqi.user = "my-user"
	}
	if amqi.topicPrefix == "" {
		amqi.topicPrefix = "topic-logging"
	}
	if amqi.instanceType == "" {
		amqi.instanceType = "kafka-sasl-cluster"
	}

	loggingBaseDir := testdata.FixturePath("logging")
	operatorDeployed := false
	// Wait csv appears up to 3 minutes
	wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("csv", "-n", "openshift-operators").Output()
		if err != nil {
			return false, err
		}
		if strings.Contains(output, "amqstreams") {
			operatorDeployed = true
			return true, nil
		}
		return false, nil
	})
	if !operatorDeployed {
		e2e.Logf("deploy amqstream operator")
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("operatorhub/cluster", `-ojsonpath='{.status.sources[?(@.name=="redhat-operators")].disabled}'`).Output()
		if err != nil {
			g.Skip("Can not detect the catalog source/redhat-operators status")
		}
		if output == "true" {
			g.Skip("catalog source/redhat-operators is disabled")
		}
		catsrc := CatalogSourceObjects{"amq-streams-2.x", "redhat-operators", "openshift-marketplace"}
		amqs := SubscriptionObjects{
			OperatorName:  "amq-streams-cluster-operator",
			Namespace:     amqi.namespace,
			PackageName:   "amq-streams",
			Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
			OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "singlenamespace-og.yaml"),
			CatalogSource: catsrc,
		}
		amqs.SubscribeOperator(oc)
		if isFipsEnabled(oc) {
			//disable FIPS_MODE due to "java.io.IOException: getPBEAlgorithmParameters failed: PBEWithHmacSHA256AndAES_256 AlgorithmParameters not available"
			err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("sub/"+amqs.PackageName, "-n", amqs.Namespace, "-p", "{\"spec\": {\"config\": {\"env\": [{\"name\": \"FIPS_MODE\", \"value\": \"disabled\"}]}}}", "--type=merge").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
		}
	}
	// before creating kafka, check the existence of crd kafkas.kafka.strimzi.io
	checkResource(oc, true, true, "kafka.strimzi.io", []string{"crd", "kafkas.kafka.strimzi.io", "-ojsonpath={.spec.group}"})
	kafka := resource{"kafka", amqi.name, amqi.namespace}
	kafkaTemplate := filepath.Join(loggingBaseDir, "external-log-stores", "kafka", "amqstreams", amqi.instanceType+".yaml")
	kafka.applyFromTemplate(oc, "-n", kafka.namespace, "-f", kafkaTemplate, "-p", "NAME="+kafka.name)
	// wait for kafka cluster to be ready
	waitForPodReadyWithLabel(oc, kafka.namespace, "app.kubernetes.io/instance="+kafka.name)
	if amqi.instanceType == "kafka-sasl-cluster" {
		e2e.Logf("deploy kafka user")
		kafkaUser := resource{"kafkauser", amqi.user, amqi.namespace}
		kafkaUserTemplate := filepath.Join(loggingBaseDir, "external-log-stores", "kafka", "amqstreams", "kafka-sasl-user.yaml")
		kafkaUser.applyFromTemplate(oc, "-n", kafkaUser.namespace, "-f", kafkaUserTemplate, "-p", "NAME="+amqi.user, "-p", "KAFKA_NAME="+amqi.name, "-p", "TOPIC_PREFIX="+amqi.topicPrefix)
		// get user password from secret my-user
		err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
			secrets, err := oc.AdminKubeClient().CoreV1().Secrets(kafkaUser.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + kafkaUser.name})
			if err != nil {
				e2e.Logf("failed to list secret, continue")
				return false, nil
			}
			count := len(secrets.Items)
			if count == 0 {
				e2e.Logf("canot not find the secret %s, continues", kafkaUser.name)
				return false, nil
			}
			return true, nil
		})
		compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Can not find the kafka user Secret %s", amqi.user))

		e2e.Logf("set kafka user password")
		amqi.password, err = oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("get").Args("secret", amqi.user, "-n", amqi.namespace, "-o", "jsonpath={.data.password}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		temp, err := base64.StdEncoding.DecodeString(amqi.password)
		o.Expect(err).NotTo(o.HaveOccurred())
		amqi.password = string(temp)

		// get extranal route of amqstream kafka
		e2e.Logf("get kafka route")
		amqi.route = getRouteAddress(oc, amqi.namespace, amqi.name+"-kafka-external-bootstrap")
		amqi.route = amqi.route + ":443"

		// get ca for route
		e2e.Logf("get kafka routeCA")
		amqi.routeCA, err = oc.AsAdmin().WithoutNamespace().Run("get").Args("secret", amqi.name+"-cluster-ca-cert", "-n", amqi.namespace, "-o", `jsonpath={.data.ca\.crt}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		temp, err = base64.StdEncoding.DecodeString(amqi.routeCA)
		o.Expect(err).NotTo(o.HaveOccurred())
		amqi.routeCA = string(temp)
	}
	// get internal service URL of amqstream kafka
	amqi.service = amqi.name + "-kafka-bootstrap." + amqi.namespace + ".svc:9092"
	e2e.Logf("amqstream deployed")
}

// try best to delete resources which will block normal deletion
func (amqi *amqInstance) destroy(oc *exutil.CLI) {
	e2e.Logf("delete kakfa resources")
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("job", "--all", "-n", amqi.namespace).Execute()
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("kafkauser", "--all", "-n", amqi.namespace).Execute()
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("kafkatopic", "--all", "-n", amqi.namespace).Execute()
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("kafka", amqi.name, "-n", amqi.namespace).Execute()
}

// Create kafka topic, create consumer pod and return consumer pod name
// Note: the topic name must match the amq.topicPrefix
func (amqi amqInstance) createTopicAndConsumber(oc *exutil.CLI, topicName string) string {
	e2e.Logf("create kakfa topic %s and consume pod", topicName)
	if !strings.HasPrefix(topicName, amqi.topicPrefix) {
		e2e.Failf("error, the topic %s must has prefix %s", topicName, amqi.topicPrefix)
	}
	var (
		consumerPodName string
		loggingBaseDir  = testdata.FixturePath("logging")
		topicTemplate   = filepath.Join(loggingBaseDir, "external-log-stores", "kafka", "amqstreams", "kafka-topic.yaml")
		topic           = resource{"Kafkatopic", topicName, amqi.namespace}
	)
	err := topic.applyFromTemplate(oc, "-n", topic.namespace, "-f", topicTemplate, "-p", "NAMESPACE="+topic.namespace, "-p", "NAME="+topic.name, "CLUSTER_NAME="+amqi.name)
	o.Expect(err).NotTo(o.HaveOccurred())

	if amqi.instanceType == "kafka-sasl-cluster" {
		//create consumers sasl.client.property
		truststorePassword, err := oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("get").Args("secret", amqi.name+"-cluster-ca-cert", "-n", amqi.namespace, "-o", `jsonpath={.data.ca\.password}`).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		temp, err := base64.StdEncoding.DecodeString(truststorePassword)
		o.Expect(err).NotTo(o.HaveOccurred())
		truststorePassword = string(temp)
		consumerConfigTemplate := filepath.Join(loggingBaseDir, "external-log-stores", "kafka", "amqstreams", "kafka-sasl-consumers-config.yaml")
		consumerConfig := resource{"configmap", "client-property-" + amqi.user, amqi.namespace}
		err = consumerConfig.applyFromTemplate(oc.NotShowInfo(), "-n", consumerConfig.namespace, "-f", consumerConfigTemplate, "-p", "NAME="+consumerConfig.name, "-p", "USER="+amqi.user, "-p", "PASSWORD="+amqi.password, "-p", "TRUSTSTORE_PASSWORD="+truststorePassword, "-p", "KAFKA_NAME="+amqi.name)
		o.Expect(err).NotTo(o.HaveOccurred())

		//create consumer pod
		consumerTemplate := filepath.Join(loggingBaseDir, "external-log-stores", "kafka", "amqstreams", "kafka-sasl-consumer-job.yaml")
		consumer := resource{"job", topicName + "-consumer", amqi.namespace}
		err = consumer.applyFromTemplate(oc, "-n", consumer.namespace, "-f", consumerTemplate, "-p", "NAME="+consumer.name, "-p", "CLUSTER_NAME="+amqi.name, "-p", "TOPIC_NAME="+topicName, "-p", "CLIENT_CONFIGMAP_NAME="+consumerConfig.name, "-p", "CA_SECRET_NAME="+amqi.name+"-cluster-ca-cert")
		o.Expect(err).NotTo(o.HaveOccurred())
		waitForPodReadyWithLabel(oc, amqi.namespace, "job-name="+consumer.name)

		consumerPods, err := oc.AdminKubeClient().CoreV1().Pods(amqi.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "job-name=" + topicName + "-consumer"})
		o.Expect(err).NotTo(o.HaveOccurred())
		consumerPodName = consumerPods.Items[0].Name

	}
	if amqi.instanceType == "kafka-no-auth-cluster" {
		//create consumer pod
		consumerTemplate := filepath.Join(loggingBaseDir, "external-log-stores", "kafka", "amqstreams", "kafka-no-auth-consumer-job.yaml")
		consumer := resource{"job", topicName + "-consumer", amqi.namespace}
		err = consumer.applyFromTemplate(oc, "-n", consumer.namespace, "-f", consumerTemplate, "-p", "NAME="+consumer.name, "-p", "CLUSTER_NAME="+amqi.name, "-p", "TOPIC_NAME="+topicName)
		o.Expect(err).NotTo(o.HaveOccurred())
		waitForPodReadyWithLabel(oc, amqi.namespace, "job-name="+consumer.name)

		consumerPods, err := oc.AdminKubeClient().CoreV1().Pods(amqi.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "job-name=" + topicName + "-consumer"})
		o.Expect(err).NotTo(o.HaveOccurred())
		consumerPodName = consumerPods.Items[0].Name
	}
	if consumerPodName == "" {
		e2e.Logf("can not get comsumer pod for the topic %s", topicName)
	} else {
		e2e.Logf("found the comsumer pod %s ", consumerPodName)
	}
	return consumerPodName
}

type eventRouter struct {
	name      string
	namespace string
	template  string
}

func (e eventRouter) deploy(oc *exutil.CLI, optionalParameters ...string) {
	parameters := []string{"-f", e.template, "-l", "app=eventrouter", "-p", "NAME=" + e.name, "NAMESPACE=" + e.namespace}
	if len(optionalParameters) > 0 {
		parameters = append(parameters, optionalParameters...)
	}

	file, processErr := processTemplate(oc, parameters...)
	defer os.Remove(file)
	if processErr != nil {
		e2e.Failf("error processing file: %v", processErr)
	}
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args("-f", file, "-n", e.namespace).Execute()
	if err != nil {
		e2e.Failf("error deploying eventrouter: %v", err)
	}
	resource{"deployment", e.name, e.namespace}.WaitForResourceToAppear(oc)
	WaitForDeploymentPodsToBeReady(oc, e.namespace, e.name)
}

func (e eventRouter) delete(oc *exutil.CLI) {
	resources := []resource{{"deployment", e.name, e.namespace}, {"configmaps", e.name, e.namespace}, {"serviceaccounts", e.name, e.namespace}}
	for _, r := range resources {
		r.clear(oc)
	}
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("clusterrole", e.name+"-reader").Execute()
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("clusterrolebindings", e.name+"-reader-binding").Execute()
}

// createSecretForGCL creates a secret for collector pods to forward logs to Google Cloud Logging
func createSecretForGCL(oc *exutil.CLI, name, namespace string) error {
	// get gcp-credentials from env var GOOGLE_APPLICATION_CREDENTIALS
	gcsCred := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	return oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", name, "-n", namespace, "--from-file=google-application-credentials.json="+gcsCred).Execute()

}

type googleApplicationCredentials struct {
	CredentialType string `json:"type"`
	ProjectID      string `json:"project_id"`
	ClientID       string `json:"client_id"`
}

func getGCPProjectID(oc *exutil.CLI) (string, error) {
	platform := compat_otp.CheckPlatform(oc)
	if platform == "gcp" {
		return compat_otp.GetGcpProjectID(oc)
	}

	credentialFile, present := os.LookupEnv("GOOGLE_APPLICATION_CREDENTIALS")
	if !present {
		g.Skip("Skip for the platform is not GCP and there is no GCP credentials")
	}
	file, err := os.ReadFile(credentialFile)
	if err != nil {
		g.Skip("Skip for the platform is not GCP and can't read google application credentials: " + err.Error())
	}
	var gac googleApplicationCredentials
	err = json.Unmarshal(file, &gac)
	return gac.ProjectID, err
}

type googleCloudLogging struct {
	projectID string
	logName   string
}

// listLogEntries gets the most recent 5 entries
// example: https://cloud.google.com/logging/docs/reference/libraries#list_log_entries
// https://github.com/GoogleCloudPlatform/golang-samples/blob/HEAD/logging/simplelog/simplelog.go
func (gcl googleCloudLogging) listLogEntries(queryString string) ([]*logging.Entry, error) {
	ctx := context.Background()

	adminClient, err := logadmin.NewClient(ctx, gcl.projectID)
	if err != nil {
		e2e.Logf("Failed to create logadmin client: %v", err)
	}
	defer adminClient.Close()

	var entries []*logging.Entry
	lastHour := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)

	filter := fmt.Sprintf(`logName = "projects/%s/logs/%s" AND timestamp > "%s"`, gcl.projectID, gcl.logName, lastHour)
	if len(queryString) > 0 {
		filter += queryString
	}

	iter := adminClient.Entries(ctx,
		logadmin.Filter(filter),
		// Get most recent entries first.
		logadmin.NewestFirst(),
	)

	// Fetch the most recent 5 entries.
	for len(entries) < 5 {
		entry, err := iter.Next()
		if err == iterator.Done {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (gcl googleCloudLogging) getLogByType(logType string) ([]*logging.Entry, error) {
	searchString := " AND jsonPayload.log_type = \"" + logType + "\""
	return gcl.listLogEntries(searchString)
}

func (gcl googleCloudLogging) getLogByNamespace(namespace string) ([]*logging.Entry, error) {
	searchString := " AND jsonPayload.kubernetes.namespace_name = \"" + namespace + "\""
	return gcl.listLogEntries(searchString)
}

func extractGoogleCloudLoggingLogs(gclLogs []*logging.Entry) ([]LogEntity, error) {
	var (
		logs []LogEntity
		log  LogEntity
	)
	for _, item := range gclLogs {
		if value, ok := item.Payload.(*structpb.Struct); ok {
			v, err := value.MarshalJSON()
			if err != nil {
				return nil, err
			}
			//e2e.Logf("\noriginal log:\n%s\n\n", string(v))
			err = json.Unmarshal(v, &log)
			if err != nil {
				return nil, err
			}
			logs = append(logs, log)
		}
	}
	return logs, nil
}

func (gcl googleCloudLogging) removeLogs() error {
	ctx := context.Background()

	adminClient, err := logadmin.NewClient(ctx, gcl.projectID)
	if err != nil {
		e2e.Logf("Failed to create logadmin client: %v", err)
	}
	defer adminClient.Close()

	return adminClient.DeleteLog(ctx, gcl.logName)
}

func (gcl googleCloudLogging) waitForLogsAppearByType(logType string) error {
	return wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		logs, err := gcl.getLogByType(logType)
		if err != nil {
			return false, err
		}
		return len(logs) > 0, nil
	})
}

func (gcl googleCloudLogging) waitForLogsAppearByNamespace(namespace string) error {
	return wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		logs, err := gcl.getLogByNamespace(namespace)
		if err != nil {
			return false, err
		}
		return len(logs) > 0, nil
	})
}

func getExtLokiSecret() (string, string, error) {
	glokiUser := os.Getenv("GLOKIUSER")
	glokiPwd := os.Getenv("GLOKIPWD")
	if glokiUser == "" || glokiPwd == "" {
		return "", "", fmt.Errorf("GLOKIUSER or GLOKIPWD environment variable is not set")
	}
	return glokiUser, glokiPwd, nil
}

func checkCiphers(oc *exutil.CLI, tlsVer string, ciphers []string, server string, caFile string, cloNS string, timeInSec int) error {
	delay := time.Duration(timeInSec) * time.Second
	for _, cipher := range ciphers {
		e2e.Logf("Testing %s...", cipher)

		clPod, err := oc.AdminKubeClient().CoreV1().Pods(cloNS).List(context.Background(), metav1.ListOptions{LabelSelector: "name=cluster-logging-operator"})
		if err != nil {
			return fmt.Errorf("failed to get pods: %w", err)
		}

		cmd := fmt.Sprintf("openssl s_client -%s -cipher %s -CAfile %s -connect %s", tlsVer, cipher, caFile, server)
		result, err := e2eoutput.RunHostCmdWithRetries(cloNS, clPod.Items[0].Name, cmd, 3*time.Second, 30*time.Second)

		if err != nil {
			return fmt.Errorf("failed to run command: %w", err)
		}

		if strings.Contains(string(result), ":error:") {
			errorStr := strings.Split(string(result), ":")[5]
			return fmt.Errorf("error: NOT SUPPORTED (%s)", errorStr)
		} else if strings.Contains(string(result), fmt.Sprintf("Cipher is %s", cipher)) || strings.Contains(string(result), "Cipher    :") {
			e2e.Logf("SUPPORTED")
		} else {
			errorStr := string(result)
			return fmt.Errorf("error: UNKNOWN RESPONSE %s", errorStr)
		}

		time.Sleep(delay)
	}

	return nil
}

func checkTLSVer(oc *exutil.CLI, tlsVer string, server string, caFile string, cloNS string) error {

	e2e.Logf("Testing TLS %s ", tlsVer)

	clPod, err := oc.AdminKubeClient().CoreV1().Pods(cloNS).List(context.Background(), metav1.ListOptions{LabelSelector: "name=cluster-logging-operator"})
	if err != nil {
		return fmt.Errorf("failed to get pods: %w", err)
	}

	cmd := fmt.Sprintf("openssl s_client -%s -CAfile %s -connect %s", tlsVer, caFile, server)
	result, err := e2eoutput.RunHostCmdWithRetries(cloNS, clPod.Items[0].Name, cmd, 3*time.Second, 30*time.Second)

	if err != nil {
		return fmt.Errorf("failed to run command: %w", err)
	}

	if strings.Contains(string(result), ":error:") {
		errorStr := strings.Split(string(result), ":")[5]
		return fmt.Errorf("error: NOT SUPPORTED (%s)", errorStr)
	} else if strings.Contains(string(result), "Cipher is ") || strings.Contains(string(result), "Cipher    :") {
		e2e.Logf("SUPPORTED")
	} else {
		errorStr := string(result)
		return fmt.Errorf("error: UNKNOWN RESPONSE %s", errorStr)
	}

	return nil
}

func checkTLSProfile(oc *exutil.CLI, profile string, algo string, server string, caFile string, cloNS string, timeInSec int) bool {
	var ciphers []string
	var tlsVer string

	if profile == "modern" {
		e2e.Logf("Modern profile is currently not supported, please select from old, intermediate, custom")
		return false
	}

	if isFipsEnabled(oc) {
		switch profile {
		case "old":
			e2e.Logf("Checking old profile with TLS v1.3")
			tlsVer = "tls1_3"
			err := checkTLSVer(oc, tlsVer, server, caFile, cloNS)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking old profile with TLS v1.2")
			switch algo {
			case "ECDSA":
				{
					ciphers = []string{"ECDHE-ECDSA-AES128-GCM-SHA256", "ECDHE-ECDSA-AES256-GCM-SHA384", "ECDHE-ECDSA-CHACHA20-POLY1305", "ECDHE-ECDSA-AES128-SHA256", "ECDHE-ECDSA-AES128-SHA", "ECDHE-ECDSA-AES256-SHA384", "ECDHE-ECDSA-AES256-SHA"}
				}
			case "RSA":
				{
					ciphers = []string{"ECDHE-RSA-AES256-GCM-SHA384", "ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES128-GCM-SHA256"}
				}
			}
			tlsVer = "tls1_2"
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).NotTo(o.HaveOccurred())

		case "intermediate":
			e2e.Logf("Setting alogorith to %s", algo)
			e2e.Logf("Checking intermediate profile with TLS v1.3")
			tlsVer = "tls1_3"
			err := checkTLSVer(oc, tlsVer, server, caFile, cloNS)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking intermediate ciphers with TLS v1.3")
			//  as openssl-3.0.7-24.el9 in CLO pod failed as below, no such issue in openssl-3.0.9-2.fc38.  use TLS 1.3 to test TSL 1.2 here.
			//  openssl s_client -tls1_2 -cipher ECDHE-RSA-AES128-GCM-SHA256 -CAfile /run/secrets/kubernetes.io/serviceaccount/service-ca.crt -connect lokistack-sample-gateway-http:8081
			//  20B4A391FFFF0000:error:1C8000E9:Provider routines:kdf_tls1_prf_derive:ems not enabled:providers/implementations/kdfs/tls1_prf.c:200:
			//  20B4A391FFFF0000:error:0A08010C:SSL routines:tls1_PRF:unsupported:ssl/t1_enc.c:83:
			tlsVer = "tls1_3"
			switch algo {
			case "ECDSA":
				{
					ciphers = []string{"ECDHE-ECDSA-AES128-GCM-SHA256", "ECDHE-ECDSA-AES256-GCM-SHA384"}
				}
			case "RSA":
				{
					ciphers = []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"}
				}
			}
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking intermediate profile with TLS v1.1")
			tlsVer = "tls1_1"
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).To(o.HaveOccurred())

		case "custom":
			e2e.Logf("Setting alogorith to %s", algo)
			e2e.Logf("Checking custom profile with TLS v1.3")
			tlsVer = "tls1_3"
			err := checkTLSVer(oc, tlsVer, server, caFile, cloNS)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking custom profile ciphers with TLS v1.3")
			//  as openssl-3.0.7-24.el9 in CLO pod failed as below, no such issue in openssl-3.0.9-2.fc38.  use TLS 1.3 to test TSL 1.2 here.
			//  openssl s_client -tls1_2 -cipher ECDHE-RSA-AES128-GCM-SHA256 -CAfile /run/secrets/kubernetes.io/serviceaccount/service-ca.crt -connect lokistack-sample-gateway-http:8081
			//  20B4A391FFFF0000:error:1C8000E9:Provider routines:kdf_tls1_prf_derive:ems not enabled:providers/implementations/kdfs/tls1_prf.c:200:
			//  20B4A391FFFF0000:error:0A08010C:SSL routines:tls1_PRF:unsupported:ssl/t1_enc.c:83:
			tlsVer = "tls1_3"
			switch algo {
			case "ECDSA":
				{
					ciphers = []string{"ECDHE-ECDSA-CHACHA20-POLY1305", "ECDHE-ECDSA-AES128-GCM-SHA256"}
				}
			case "RSA":
				{
					ciphers = []string{"ECDHE-RSA-AES128-GCM-SHA256"}
				}
			}
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking ciphers on in custom profile with TLS v1.3")
			tlsVer = "tls1_3"
			switch algo {
			case "ECDSA", "RSA":
				{
					ciphers = []string{"TLS_AES_128_GCM_SHA256"}
				}
			}
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).To(o.HaveOccurred())
		}

	} else {
		switch profile {
		case "old":
			e2e.Logf("Checking old profile with TLS v1.3")
			tlsVer = "tls1_3"
			err := checkTLSVer(oc, tlsVer, server, caFile, cloNS)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking old profile with TLS v1.2")
			switch algo {
			case "ECDSA":
				{
					ciphers = []string{"ECDHE-ECDSA-AES128-GCM-SHA256", "ECDHE-ECDSA-AES256-GCM-SHA384", "ECDHE-ECDSA-CHACHA20-POLY1305", "ECDHE-ECDSA-AES128-SHA256", "ECDHE-ECDSA-AES128-SHA", "ECDHE-ECDSA-AES256-SHA384", "ECDHE-ECDSA-AES256-SHA"}
				}
			case "RSA":
				{
					ciphers = []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES128-SHA256", "ECDHE-RSA-AES128-SHA", "ECDHE-RSA-AES256-SHA", "AES128-GCM-SHA256", "AES256-GCM-SHA384", "AES128-SHA256", "AES128-SHA", "AES256-SHA"}
				}
			}
			tlsVer = "tls1_2"
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking old profile with TLS v1.1")
			//  remove these ciphers as openssl-3.0.7-24.el9  s_client -tls1_1 -cipher <ciphers> failed.
			switch algo {
			case "ECDSA":
				{
					ciphers = []string{"ECDHE-ECDSA-AES128-SHA", "ECDHE-ECDSA-AES256-SHA"}
				}
			case "RSA":
				{
					ciphers = []string{"AES128-SHA", "AES256-SHA"}
				}
			}
			tlsVer = "tls1_1"
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).NotTo(o.HaveOccurred())

		case "intermediate":
			e2e.Logf("Setting alogorith to %s", algo)
			e2e.Logf("Checking intermediate profile with TLS v1.3")
			tlsVer = "tls1_3"
			err := checkTLSVer(oc, tlsVer, server, caFile, cloNS)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking intermediate profile ciphers with TLS v1.2")
			tlsVer = "tls1_2"
			switch algo {
			case "ECDSA":
				{
					ciphers = []string{"ECDHE-ECDSA-AES128-GCM-SHA256", "ECDHE-ECDSA-AES256-GCM-SHA384", "ECDHE-ECDSA-CHACHA20-POLY1305"}
				}
			case "RSA":
				{
					ciphers = []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"}
				}
			}
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking intermediate profile with TLS v1.1")
			// replace checkCiphers with checkTLSVer as we needn't check all v1.1 Ciphers
			tlsVer = "tls1_1"
			err = checkTLSVer(oc, tlsVer, server, caFile, cloNS)
			o.Expect(err).To(o.HaveOccurred())

		case "custom":
			e2e.Logf("Setting alogorith to %s", algo)

			e2e.Logf("Checking custom profile with TLS v1.3")
			tlsVer = "tls1_3"
			err := checkTLSVer(oc, tlsVer, server, caFile, cloNS)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking custom profile with TLS v1.2")
			tlsVer = "tls1_2"
			switch algo {
			case "ECDSA":
				{
					ciphers = []string{"ECDHE-ECDSA-AES128-GCM-SHA256"}
				}
			case "RSA":
				{
					ciphers = []string{"ECDHE-RSA-AES128-GCM-SHA256"}
				}
			}
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Checking ciphers not in custom profile with TLS v1.3")
			tlsVer = "tls1_3"
			switch algo {
			case "ECDSA":
				{
					ciphers = []string{"ECDHE-ECDSA-AES128-GCM-SHA256"}
				}
			case "RSA":
				{
					ciphers = []string{"TLS_AES_128_GCM_SHA256"}
				}
			}
			err = checkCiphers(oc, tlsVer, ciphers, server, caFile, cloNS, timeInSec)
			o.Expect(err).To(o.HaveOccurred())
		}
	}
	return true
}

func checkCollectorConfiguration(oc *exutil.CLI, ns, cmName string, searchStrings ...string) (bool, error) {
	// Parse the vector.toml file
	dirname := "/tmp/" + oc.Namespace() + "-vectortoml"
	defer os.RemoveAll(dirname)
	err := os.MkdirAll(dirname, 0777)
	if err != nil {
		return false, err
	}

	_, err = oc.AsAdmin().WithoutNamespace().Run("extract").Args("configmap/"+cmName, "-n", ns, "--confirm", "--to="+dirname).Output()
	if err != nil {
		return false, err
	}

	filename := filepath.Join(dirname, "vector.toml")
	file, err := os.Open(filename)
	if err != nil {
		return false, err
	}
	defer file.Close()

	content, err := os.ReadFile(filename)
	if err != nil {
		return false, err
	}

	for _, s := range searchStrings {
		if !strings.Contains(string(content), s) {
			e2e.Logf("can't find %s in vector.toml", s)
			return false, nil
		}
	}
	return true, nil
}

func checkOperatorsRunning(oc *exutil.CLI) (bool, error) {
	jpath := `{range .items[*]}{.metadata.name}:{.status.conditions[?(@.type=='Available')].status}{':'}{.status.conditions[?(@.type=='Degraded')].status}{'\n'}{end}`
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("clusteroperators.config.openshift.io", "-o", "jsonpath="+jpath).Output()
	if err != nil {
		return false, fmt.Errorf("failed to execute 'oc get clusteroperators.config.openshift.io' command: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		e2e.Logf("%s", line)
		parts := strings.Split(line, ":")
		available := parts[1] == "True"
		degraded := parts[2] == "False"

		if !available || !degraded {
			return false, nil
		}
	}

	return true, nil
}

func waitForOperatorsRunning(oc *exutil.CLI) {
	e2e.Logf("Wait a minute to allow the cluster to reconcile the config changes.")
	time.Sleep(1 * time.Minute)
	err := wait.PollUntilContextTimeout(context.Background(), 3*time.Minute, 21*time.Minute, true, func(context.Context) (done bool, err error) {
		return checkOperatorsRunning(oc)
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Failed to wait for operators to be running: %v", err))
}

func doHTTPRequest(header http.Header, address, path, query, method string, quiet bool, attempts int, requestBody io.Reader, expectedStatusCode int) ([]byte, error) {
	us, err := buildURL(address, path, query)
	if err != nil {
		return nil, err
	}
	if !quiet {
		e2e.Logf("the URL is: %s", us)
	}

	req, err := http.NewRequest(strings.ToUpper(method), us, requestBody)
	if err != nil {
		return nil, err
	}

	req.Header = header

	var tr *http.Transport
	proxy := getProxyFromEnv()
	if len(proxy) > 0 {
		proxyURL, err := url.Parse(proxy)
		o.Expect(err).NotTo(o.HaveOccurred())
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			Proxy:           http.ProxyURL(proxyURL),
		}
	} else {
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	client := &http.Client{Transport: tr}

	var resp *http.Response
	success := false

	for attempts > 0 {
		attempts--

		resp, err = client.Do(req)
		if err != nil {
			e2e.Logf("error sending request %v", err)
			continue
		}
		if resp.StatusCode != expectedStatusCode {
			buf, _ := io.ReadAll(resp.Body) // nolint
			e2e.Logf("Error response from server: %s %s (%v), attempts remaining: %d", resp.Status, string(buf), err, attempts)
			if err := resp.Body.Close(); err != nil {
				e2e.Logf("error closing body: %v", err)
			}
			// sleep 5 second before doing next request
			time.Sleep(5 * time.Second)
			continue
		}
		success = true
		break
	}
	if !success {
		return nil, fmt.Errorf("run out of attempts while querying the server")
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			e2e.Logf("error closing body: %v", err)
		}
	}()
	return io.ReadAll(resp.Body)
}

// buildURL concats a url `http://foo/bar` with a path `/buzz`.
func buildURL(u, p, q string) (string, error) {
	url, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	url.Path = path.Join(url.Path, p)
	url.RawQuery = q
	return url.String(), nil
}

// GetIPVersionStackType gets IP-version Stack type of the cluster
func GetIPVersionStackType(oc *exutil.CLI) (ipvStackType string) {
	svcNetwork, err := oc.WithoutNamespace().AsAdmin().Run("get").Args("network.operator", "cluster", "-o=jsonpath={.spec.serviceNetwork}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	if strings.Count(svcNetwork, ":") >= 2 && strings.Count(svcNetwork, ".") >= 2 {
		ipvStackType = "dualstack"
	} else if strings.Count(svcNetwork, ":") >= 2 {
		ipvStackType = "ipv6single"
	} else if strings.Count(svcNetwork, ".") >= 2 {
		ipvStackType = "ipv4single"
	}
	e2e.Logf("The test cluster IP-version Stack type is :\"%s\".", ipvStackType)
	return ipvStackType
}

// convertInterfaceToArray converts interface{} to []string
/*
	example of interface{}:
	  [
	    timestamp,
		log data
	  ],
	  [
	    timestamp,
		count
	  ]
*/
func convertInterfaceToArray(t interface{}) []string {
	var data []string
	switch reflect.TypeOf(t).Kind() {
	case reflect.Slice, reflect.Array:
		s := reflect.ValueOf(t)
		for i := 0; i < s.Len(); i++ {
			data = append(data, fmt.Sprint(s.Index(i)))
		}
	}
	return data
}

// send logs over http
func postDataToHttpserver(oc *exutil.CLI, httpURL string, postJsonString string) bool {
	cloPods, err := oc.AdminKubeClient().CoreV1().Pods(cloNS).List(context.Background(), metav1.ListOptions{LabelSelector: "name=cluster-logging-operator"})
	if err != nil || len(cloPods.Items) < 1 {
		e2e.Logf("failed to get pods by label name=cluster-logging-operator")
		return false
	}
	//ToDo, send logs to httpserver using service ca, oc get cm/openshift-service-ca.crt -o json |jq '.data."service-ca.crt"'
	cmd := `curl -s -k -w "%{http_code}" ` + httpURL + " -d '" + postJsonString + "'"
	result, err := e2eoutput.RunHostCmdWithRetries(cloNS, cloPods.Items[0].Name, cmd, 3*time.Second, 30*time.Second)
	if err != nil {
		e2e.Logf("Show more status as data can not be sent to httpserver")
		oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", cloNS, "endpoints").Output()
		oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", cloNS, "pods").Output()
		return false
	}
	if result == "200" {
		return true
	} else {
		e2e.Logf("Show result as return code is not 200")
		e2e.Logf("result=%v", result)
		return false
	}
}

// create job for rapiddast test
// Run a job to do rapiddast, the scan result will be written into pod logs and store in artifactdirPath
func rapidastScan(oc *exutil.CLI, ns, configFile string, scanPolicyFile string, apiGroupName string) (bool, error) {
	//if dastSecretFile exist, the result will be sent to  central storage#2
	dastSecretFile := "/var/run/ocp-logging-team-secrets/dast-gcs-secret"
	dastSecretExist := false
	if _, err := os.Stat(dastSecretFile); err == nil {
		dastSecretExist = true
	}

	//if ARTIFACT_DIR exist, the result will copied into $ARTIFACT_DIR
	artifactAvaiable := true
	artifactdirPath := os.Getenv("ARTIFACT_DIR")
	if artifactdirPath == "" {
		artifactAvaiable = false
	}
	info, err := os.Stat(artifactdirPath)
	if err != nil {
		e2e.Logf("%s doesn't exist", artifactdirPath)
		artifactAvaiable = false
	} else if !info.IsDir() {
		e2e.Logf("%s isn't a directory", artifactdirPath)
		artifactAvaiable = false
	}

	// Load configFile and update the content
	newConfigFile := "/tmp/dast-config-" + getRandomString() + ".yaml"
	data := make(map[string]interface{})
	fileContent, err := os.ReadFile(configFile)
	if err != nil {
		e2e.Failf("Error to open file %s, %v", configFile, err)
	}
	// Unmarshal (Load YAML into the map)
	err = yaml.Unmarshal(fileContent, &data)
	if err != nil {
		e2e.Failf("Error load %s to the map", configFile)
	}

	// delete googleCloudStorage wen dast GCS secret is not ready
	if !dastSecretExist {
		if subMap, ok := data["config"].(map[string]interface{}); ok {
			delete(subMap, "googleCloudStorage")
		}
	}
	// Update the cluster token
	token := getSAToken(oc, "default", ns)
	// Safely navigate to the "parameters" map
	if general, ok := data["general"].(map[string]interface{}); ok {
		if auth, ok := general["authentication"].(map[string]interface{}); ok {
			if params, ok := auth["parameters"].(map[string]interface{}); ok {
				// Update the token
				params["value"] = "Bearer " + token

			}
		}
	}
	// Marshal (Convert map back to YAML bytes)
	updatedYaml, err := yaml.Marshal(&data)
	if err != nil {
		e2e.Failf("Error generating YAML: %v", err)
	}

	// Save back to the file
	err = os.WriteFile(newConfigFile, updatedYaml, 0644)
	if err != nil {
		e2e.Failf("Error generating YAML: %v", err)
	}

	//Ensure the service account has permission to scan CRD
	defer oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "remove-cluster-role-from-user", "cluster-admin", fmt.Sprintf("system:serviceaccount:%s:default", ns)).Execute()
	oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "add-cluster-role-to-user", "cluster-admin", fmt.Sprintf("system:serviceaccount:%s:default", ns)).Execute()

	//Create configmap for job
	jobName := "rapidast-" + getRandomString()
	configMapName := jobName
	err = oc.WithoutNamespace().Run("create").Args("-n", ns, "configmap", configMapName, "--from-file=rapidastconfig.yaml="+newConfigFile, "--from-file=customscan.policy="+scanPolicyFile).Execute()
	if err != nil {
		e2e.Failf("Error create configmap for rapidast jobs, %v", err)
	}
	if dastSecretExist {
		err = oc.WithoutNamespace().Run("set").Args("-n", ns, "data", "configmap/"+configMapName, "--from-file=dast-gcs-secret.json="+dastSecretFile).Execute()
		if err != nil {
			e2e.Failf("Error create configmap for rapidast jobs, %v", err)
		}
	}

	//Create job
	loggingBaseDir := testdata.FixturePath("logging")
	jobTemplate := filepath.Join(loggingBaseDir, "rapidast/job_rapidast.yaml")
	rapidastJob := resource{"job", jobName, ns}
	err = rapidastJob.applyFromTemplate(oc, "-f", jobTemplate, "-n", ns, "-p", "NAME="+jobName)
	if err != nil {
		e2e.Logf("rapidastScan abort! create rapidast job failed")
		e2e.Logf("rapidast result: riskHigh=unknown riskMedium=unknown")
		return false, err
	}
	//Waiting up to 3 minutes until pod Failed or Success
	wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 3*time.Minute, true, func(context.Context) (done bool, err error) {
		jobStatus, err1 := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", ns, "pod", "-l", "job-name="+jobName, "-ojsonpath={.items[0].status.phase}").Output()
		e2e.Logf(" rapidast Job status %s ", jobStatus)
		if err1 != nil {
			return false, nil
		}
		if jobStatus == "Pending" || jobStatus == "Running" {
			return false, nil
		}
		if jobStatus == "Failed" {
			e2e.Logf("rapidast-job %s failed", jobName)
			return true, nil
		}
		if jobStatus == "Succeeded" {
			return true, nil
		}
		return false, nil
	})
	// Get the rapidast pod name
	jobPods, err := oc.AdminKubeClient().CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil {
		e2e.Logf("rapidastScan abort! can not find rapidast scan job ")
		e2e.Logf("rapidast result: riskHigh=unknown riskMedium=unknown")
		return false, err
	}
	podLogs, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("-n", ns, jobPods.Items[0].Name).Output()
	if err != nil {
		e2e.Logf("rapidastScan abort! can not fetch logs from rapidast-scan pod %s", jobPods.Items[0].Name)
		e2e.Logf("rapidast result: riskHigh=unknown riskMedium=unknown")
		return false, err
	}

	if artifactAvaiable {
		rapidastResultsSubDir := artifactdirPath + "/rapiddastresultslogging"
		err = os.MkdirAll(rapidastResultsSubDir, 0755)
		if err != nil {
			e2e.Logf("failed to create %s", rapidastResultsSubDir)
		}
		artifactFile := rapidastResultsSubDir + "/" + apiGroupName + "_rapidast.result.txt"
		e2e.Logf("Write report into %s", artifactFile)
		f1, err := os.Create(artifactFile)
		if err != nil {
			e2e.Logf("failed to create artifactFile %s", artifactFile)
		}
		defer f1.Close()
		_, err = f1.WriteString(podLogs)
		if err != nil {
			e2e.Logf("failed to write logs into artifactFile %s", artifactFile)
		}
	} else {
		// print pod logs if artifactdirPath is not writable
		e2e.Logf("#oc logs -n %s %s \n %s", jobPods.Items[0].Name, ns, podLogs)
	}

	//return false, if high risk is reported
	podLogA := strings.Split(podLogs, "\n")
	riskHigh := 0
	riskMedium := 0
	re1 := regexp.MustCompile(`"riskdesc": .*High`)
	re2 := regexp.MustCompile(`"riskdesc": .*Medium`)
	for _, item := range podLogA {
		if re1.MatchString(item) {
			riskHigh++
		}
		if re2.MatchString(item) {
			riskMedium++
		}
	}
	e2e.Logf("rapidast result: riskHigh=%v riskMedium=%v", riskHigh, riskMedium)

	if riskHigh > 0 {
		return false, fmt.Errorf("high risk alert, please check the scan result report")
	}
	return true, nil
}

// Get OIDC provider for the cluster
func getOIDC(oc *exutil.CLI) (string, error) {
	oidc, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("authentication.config", "cluster", "-o=jsonpath={.spec.serviceAccountIssuer}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(oidc, "https://"), nil
}

func getPoolID(oc *exutil.CLI) (string, error) {
	// pool_id="$(oc get authentication cluster -o json | jq -r .spec.serviceAccountIssuer | sed 's/.*\/\([^\/]*\)-oidc/\1/')"
	issuer, err := getOIDC(oc)
	if err != nil {
		return "", err
	}

	return strings.Split(strings.Split(issuer, "/")[1], "-oidc")[0], nil
}

// Create a linux audit policy to generate audit logs in one schedulable worker
func genLinuxAuditLogsOnWorker(oc *exutil.CLI) (string, error) {
	workerNodes, err := compat_otp.GetSchedulableLinuxWorkerNodes(oc)
	if err != nil || len(workerNodes) == 0 {
		return "", fmt.Errorf("can not find schedulable worker to enable audit policy")
	}
	result, err := compat_otp.DebugNodeWithChroot(oc, workerNodes[0].Name, "bash", "-c", "auditctl -w /var/log/pods/ -p rwa -k logging-qe-test-read-write-pod-logs")
	if err != nil && strings.Contains(result, "Rule exists") {
		//Note: we still provide the nodeName here, the policy will be deleted if `defer deleteLinuxAuditPolicyFromNodes` is called.
		return workerNodes[0].Name, nil
	}
	return workerNodes[0].Name, err
}

// delete the linux audit policy
func deleteLinuxAuditPolicyFromNode(oc *exutil.CLI, nodeName string) error {
	if nodeName == "" {
		return fmt.Errorf("nodeName can not be empty")
	}
	_, err := compat_otp.DebugNodeWithChroot(oc, nodeName, "bash", "-c", "auditctl -W /var/log/pods/ -p rwa -k logging-qe-test-read-write-pod-logs")
	return err
}

func hasMaster(oc *exutil.CLI) bool {
	masterNodes, err := oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/master="})
	if err != nil {
		e2e.Logf("hit error when listing master nodes: %v", err)
	}
	return len(masterNodes.Items) > 0
}

func runLoggingMustGather(oc *exutil.CLI) ([]string, error) {
	// create a temporary directory
	baseDir := testdata.FixturePath("logging")
	mustGatherPath := filepath.Join(baseDir, "temp"+getRandomString())
	defer exec.Command("rm", "-r", mustGatherPath).Output()
	err := os.MkdirAll(mustGatherPath, 0755)
	o.Expect(err).NotTo(o.HaveOccurred())

	image, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("deployment.apps/cluster-logging-operator", `-ojsonpath={.spec.template.spec.containers[?(@.name == "cluster-logging-operator")].image}`, "-n", cloNS).Output()
	if err != nil {
		return []string{}, fmt.Errorf("can't get CLO image: %s", err)
	}

	err = oc.AsAdmin().WithoutNamespace().Run("adm").Args("must-gather", "--image="+image, "--dest-dir="+mustGatherPath).Execute()
	if err != nil {
		return []string{}, fmt.Errorf("can't get logging must-gather: %v", err)
	}

	cloImgID, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", cloNS, "pods", "-l", "name=cluster-logging-operator", "-o", "jsonpath={.items[0].status.containerStatuses[0].imageID}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	replacer := strings.NewReplacer(".", "-", "/", "-", ":", "-", "@", "-")
	cloImgDir := replacer.Replace(cloImgID)
	return listFilesAndDirectories(mustGatherPath + "/" + cloImgDir)
}

func listFilesAndDirectories(dirPath string) ([]string, error) {
	var files []string
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return files, err
	}

	for _, entry := range entries {
		path := filepath.Join(dirPath, entry.Name())
		if entry.IsDir() {
			pathes, err := listFilesAndDirectories(path)
			if err != nil {
				return files, err
			}
			files = append(files, pathes...)
		}
		//fmt.Printf("\n%s\n", path)
		files = append(files, path)
	}

	return files, nil
}

// Get Proxy CaBundle from the cluster, return "" if no bundle or the cluster isn't proxy enabled cluster
func getProxyCaBundle(oc *exutil.CLI) (caBundle string) {
	e2e.Logf("Check if proxxy caBundle is enabled")
	configMapName, err := oc.WithoutNamespace().AsAdmin().Run("get").Args("proxy", "cluster", "-o=jsonpath={.spec.trustedCA.name}").Output()
	if configMapName == "" {
		e2e.Logf("GetProxyCaBundle can not find proxy trustedCA, %v", err)
		return ""
	}
	caBundle, err = oc.AsAdmin().WithoutNamespace().Run("get").Args("configmap", configMapName, "-n", "openshift-config", `-ojsonpath={.data.ca-bundle\.crt}`).Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	return caBundle
}

func getNetworkPolicy(oc *exutil.CLI, namespace, name string) (NetworkPolicy, error) {
	var netpol NetworkPolicy
	np, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("networkpolicy", "-n", namespace, name, "-oyaml").Output()
	if err != nil {
		return netpol, fmt.Errorf("error getting networkpolicy %s: %v", name, err)
	}

	err = yaml.Unmarshal([]byte(np), &netpol)
	if err != nil {
		return netpol, fmt.Errorf("error unmarshalling YAML: %v", err)
	}
	return netpol, nil
}
