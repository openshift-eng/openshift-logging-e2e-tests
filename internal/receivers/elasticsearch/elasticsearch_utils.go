package elasticsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/compat_otp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"

	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/random"
	"github.com/openshift/openshift-logging-e2e-tests/internal/test/certificates"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types"
	"github.com/openshift/openshift-logging-e2e-tests/internal/types/apps/deployment"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
)

type ExternalES struct {
	Namespace                  string
	Version                    string // support 6 and 7
	ServerName                 string // ES cluster name, configmap/sa/deploy/svc name
	HttpSSL                    bool   // `true` means enable `xpack.security.http.ssl`
	ClientAuth                 bool   // `true` means `xpack.security.http.ssl.client_authentication: required`, only can be set to `true` when HttpSSL is `true`
	ClientPrivateKeyPassphrase string // only works when ClientAuth is true
	UserAuth                   bool   // `true` means enable user auth
	Username                   string // shouldn't be empty when `UserAuth: true`
	Password                   string // shouldn't be empty when `UserAuth: true`
	SecretName                 string // the name of the secret for the collector to use, it shouldn't be empty when `HttpSSL: true` or `UserAuth: true`
	LoggingNS                  string // the Namespace where the collector pods deployed in
}

func (es ExternalES) createPipelineSecret(oc *exutil.CLI, keysPath string) {
	// create pipeline secret if needed
	cmd := []string{"secret", "generic", es.SecretName, "-n", es.LoggingNS}
	if es.ClientAuth {
		cmd = append(cmd, "--from-file=tls.key="+keysPath+"/client.key", "--from-file=tls.crt="+keysPath+"/client.crt", "--from-file=ca-bundle.crt="+keysPath+"/ca.crt")
		if es.ClientPrivateKeyPassphrase != "" {
			cmd = append(cmd, "--from-literal=passphrase="+es.ClientPrivateKeyPassphrase)
		}
	} else if es.HttpSSL && !es.ClientAuth {
		cmd = append(cmd, "--from-file=ca-bundle.crt="+keysPath+"/ca.crt")
	}
	if es.UserAuth {
		cmd = append(cmd, "--from-literal=username="+es.Username, "--from-literal=password="+es.Password)
	}

	err := oc.AsAdmin().WithoutNamespace().Run("create").Args(cmd...).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	types.NewResource("secret", es.SecretName, es.LoggingNS).WaitForResourceToAppear(oc)
}

func (es ExternalES) Deploy(oc *exutil.CLI) {
	// create SA
	sa := types.Resource{Kind: "serviceaccount", Name: es.ServerName, Namespace: es.Namespace}
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args("serviceaccount", sa.Name, "-n", sa.Namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	sa.WaitForResourceToAppear(oc)

	if es.UserAuth {
		o.Expect(es.Username).NotTo(o.BeEmpty(), "Please provide Username!")
		o.Expect(es.Password).NotTo(o.BeEmpty(), "Please provide Password!")
	}

	if es.HttpSSL || es.ClientAuth || es.UserAuth {
		o.Expect(es.SecretName).NotTo(o.BeEmpty(), "Please provide pipeline secret name!")

		// create a temporary directory
		baseDir := testdata.FixturePath("logging")
		keysPath := filepath.Join(baseDir, "temp"+random.String())
		defer exec.Command("rm", "-r", keysPath).Output()
		err = os.MkdirAll(keysPath, 0o755)
		o.Expect(err).NotTo(o.HaveOccurred())

		cert := certificates.New(es.ServerName, es.Namespace, es.ClientPrivateKeyPassphrase)
		cert.Generate(oc, keysPath)
		// create secret for ES if needed
		if es.HttpSSL || es.ClientAuth {
			r := types.Resource{Kind: "secret", Name: es.ServerName, Namespace: es.Namespace}
			err = oc.WithoutNamespace().Run("create").Args("secret", "generic", "-n", r.Namespace, r.Name, "--from-file=elasticsearch.key="+keysPath+"/server.key", "--from-file=elasticsearch.crt="+keysPath+"/server.crt", "--from-file=admin-ca="+keysPath+"/ca.crt").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			r.WaitForResourceToAppear(oc)
		}

		// create pipeline secret in logging project
		es.createPipelineSecret(oc, keysPath)
	}

	// get file path per the configurations
	filePath := []string{"logging", "external-log-stores", "elasticsearch", es.Version}
	if es.HttpSSL {
		filePath = append(filePath, "https")
	} else {
		o.Expect(es.ClientAuth).NotTo(o.BeTrue(), "Unsupported configuration, please correct it!")
		filePath = append(filePath, "http")
	}
	if es.UserAuth {
		filePath = append(filePath, "user_auth")
	} else {
		filePath = append(filePath, "no_user")
	}

	// create configmap
	cm := types.Resource{Kind: "configmap", Name: es.ServerName, Namespace: es.Namespace}
	cmFilePath := append(filePath, "configmap.yaml")
	cmFile := testdata.FixturePath(cmFilePath...)
	cmPatch := []string{"-f", cmFile, "-n", cm.Namespace, "-p", "NAMESPACE=" + es.Namespace, "-p", "NAME=" + es.ServerName}
	if es.UserAuth {
		cmPatch = append(cmPatch, "-p", "USERNAME="+es.Username, "-p", "PASSWORD="+es.Password)
	}
	if es.HttpSSL {
		if es.ClientAuth {
			cmPatch = append(cmPatch, "-p", "CLIENT_AUTH=required")
		} else {
			cmPatch = append(cmPatch, "-p", "CLIENT_AUTH=none")
		}
	}

	// set xpack.ml.enable to false when the architecture is not amd64
	nodes, err := compat_otp.GetSchedulableLinuxWorkerNodes(oc)
	o.Expect(err).NotTo(o.HaveOccurred())
	for _, node := range nodes {
		if node.Status.NodeInfo.Architecture != "amd64" {
			cmPatch = append(cmPatch, "-p", "MACHINE_LEARNING=false")
			break
		}
	}

	o.Expect(cm.ApplyFromTemplate(oc, cmPatch...)).To(o.Succeed())

	// create deployment and expose svc
	deploy := types.Resource{Kind: "deployment", Name: es.ServerName, Namespace: es.Namespace}
	deployFilePath := append(filePath, "deployment.yaml")
	deployFile := testdata.FixturePath(deployFilePath...)
	err = deploy.ApplyFromTemplate(oc, "-f", deployFile, "-n", es.Namespace, "-p", "NAMESPACE="+es.Namespace, "-p", "NAME="+es.ServerName)
	o.Expect(err).NotTo(o.HaveOccurred())
	deployment.WaitForPodsToBeReady(oc, es.Namespace, es.ServerName)
	es.waitForServer(oc)
	err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("-n", es.Namespace, "deployment", es.ServerName, "--name="+es.ServerName).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	// expose route
	if es.HttpSSL {
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("-n", es.Namespace, "route", "passthrough", "--service="+es.ServerName, "--port=9200").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
	} else {
		err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("svc/"+es.ServerName, "-n", es.Namespace, "--port=9200").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
	}
}

// waitForServer waits until Elasticsearch accepts connections on port 9200.
// The readiness probe of the deployment only runs add-user.sh, so a ready pod
// can still be starting its JVM: curl then fails with exit code 7 (connection
// refused), which would abort the polls in waitForIndexAppear and friends.
func (es ExternalES) waitForServer(oc *exutil.CLI) {
	podName := es.getPodName(oc)
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 180*time.Second, true, func(context.Context) (bool, error) {
		if _, err := e2eoutput.RunHostCmd(es.Namespace, podName, es.baseCurlString()+"_cluster/health"); err != nil {
			e2e.Logf("elasticsearch %s is not accepting connections yet: %v", es.ServerName, err)
			return false, nil
		}
		return true, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Elasticsearch %s didn't accept connections in last 3 minutes", es.ServerName))
}

func (es ExternalES) Remove(oc *exutil.CLI) {
	_ = types.NewResource("route", es.ServerName, es.Namespace).Clear(oc)
	_ = types.NewResource("service", es.ServerName, es.Namespace).Clear(oc)
	_ = types.NewResource("configmap", es.ServerName, es.Namespace).Clear(oc)
	_ = types.NewResource("deployment", es.ServerName, es.Namespace).Clear(oc)
	_ = types.NewResource("serviceaccount", es.ServerName, es.Namespace).Clear(oc)
	if es.HttpSSL || es.UserAuth {
		_ = types.NewResource("secret", es.SecretName, es.LoggingNS).Clear(oc)
	}
	if es.HttpSSL {
		_ = types.NewResource("secret", es.ServerName, es.Namespace).Clear(oc)
	}
}

func (es ExternalES) getPodName(oc *exutil.CLI) string {
	esPods, err := oc.AdminKubeClient().CoreV1().Pods(es.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + es.ServerName})
	o.Expect(err).NotTo(o.HaveOccurred())
	var names []string
	for i := 0; i < len(esPods.Items); i++ {
		names = append(names, esPods.Items[i].Name)
	}
	return names[0]
}

func (es ExternalES) baseCurlString() string {
	curlString := "curl -H \"Content-Type: application/json\""
	if es.UserAuth {
		curlString += " -u " + es.Username + ":" + es.Password
	}
	if es.HttpSSL {
		if es.ClientAuth {
			curlString += " --cert /usr/share/elasticsearch/config/secret/elasticsearch.crt --key /usr/share/elasticsearch/config/secret/elasticsearch.key"
		}
		curlString += " --cacert /usr/share/elasticsearch/config/secret/admin-ca -s https://localhost:9200/"
	} else {
		curlString += " -s http://localhost:9200/"
	}
	return curlString
}

func (es ExternalES) GetIndices(oc *exutil.CLI) ([]ESIndex, error) {
	cmd := es.baseCurlString() + "_cat/indices?format=JSON"
	stdout, err := e2eoutput.RunHostCmdWithRetries(es.Namespace, es.getPodName(oc), cmd, 3*time.Second, 9*time.Second)
	if err != nil {
		return nil, err
	}
	indices := []ESIndex{}
	if err := json.Unmarshal([]byte(stdout), &indices); err != nil {
		return nil, err
	}
	return indices, nil
}

func (es ExternalES) WaitForIndexAppear(oc *exutil.CLI, indexName string) {
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		indices, err := es.GetIndices(oc)
		count := 0
		for _, index := range indices {
			if strings.Contains(index.Index, indexName) {
				if index.Health != "red" {
					docCount, _ := strconv.Atoi(index.DocsCount)
					count += docCount
				}
			}
		}
		if count > 0 && err == nil {
			return true, nil
		}
		return false, err
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("Index %s didn't appear or the doc count is 0 in last 3 minutes.", indexName))
}

func (es ExternalES) GetDocCount(oc *exutil.CLI, indexName string, queryString string) (int64, error) {
	cmd := es.baseCurlString() + indexName + "*/_count?format=JSON -d '" + queryString + "'"
	stdout, err := e2eoutput.RunHostCmdWithRetries(es.Namespace, es.getPodName(oc), cmd, 5*time.Second, 30*time.Second)
	if err != nil {
		return 0, err
	}
	res := CountResult{}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		return 0, err
	}
	return res.Count, nil
}

func (es ExternalES) WaitForProjectLogsAppear(oc *exutil.CLI, projectName string, indexName string) {
	query := "{\"query\": {\"match_phrase\": {\"kubernetes.namespace_name\": \"" + projectName + "\"}}}"
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		logCount, err := es.GetDocCount(oc, indexName, query)
		if err != nil {
			return false, err
		}
		if logCount > 0 {
			return true, nil
		}
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("The logs of project %s didn't collected to index %s in last 180 seconds.", projectName, indexName))
}

func (es ExternalES) SearchDocByQuery(oc *exutil.CLI, indexName string, queryString string) SearchResult {
	cmd := es.baseCurlString() + indexName + "*/_search?format=JSON -d '" + queryString + "'"
	stdout, err := e2eoutput.RunHostCmdWithRetries(es.Namespace, es.getPodName(oc), cmd, 3*time.Second, 30*time.Second)
	o.Expect(err).ShouldNot(o.HaveOccurred())
	res := SearchResult{}
	o.Expect(json.Unmarshal([]byte(stdout), &res)).ShouldNot(o.HaveOccurred())
	return res
}

func (es ExternalES) RemoveIndices(oc *exutil.CLI, indexName string) {
	cmd := es.baseCurlString() + indexName + " -X DELETE"
	_, err := e2eoutput.RunHostCmdWithRetries(es.Namespace, es.getPodName(oc), cmd, 3*time.Second, 30*time.Second)
	o.Expect(err).ShouldNot(o.HaveOccurred())
}
