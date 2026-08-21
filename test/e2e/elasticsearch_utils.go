package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
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
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2eoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

type externalES struct {
	namespace                  string
	version                    string // support 6 and 7
	serverName                 string // ES cluster name, configmap/sa/deploy/svc name
	httpSSL                    bool   // `true` means enable `xpack.security.http.ssl`
	clientAuth                 bool   // `true` means `xpack.security.http.ssl.client_authentication: required`, only can be set to `true` when httpSSL is `true`
	clientPrivateKeyPassphrase string // only works when clientAuth is true
	userAuth                   bool   // `true` means enable user auth
	username                   string // shouldn't be empty when `userAuth: true`
	password                   string // shouldn't be empty when `userAuth: true`
	secretName                 string //the name of the secret for the collector to use, it shouldn't be empty when `httpSSL: true` or `userAuth: true`
	loggingNS                  string //the namespace where the collector pods deployed in
}

func (es externalES) createPipelineSecret(oc *exutil.CLI, keysPath string) {
	// create pipeline secret if needed
	cmd := []string{"secret", "generic", es.secretName, "-n", es.loggingNS}
	if es.clientAuth {
		cmd = append(cmd, "--from-file=tls.key="+keysPath+"/client.key", "--from-file=tls.crt="+keysPath+"/client.crt", "--from-file=ca-bundle.crt="+keysPath+"/ca.crt")
		if es.clientPrivateKeyPassphrase != "" {
			cmd = append(cmd, "--from-literal=passphrase="+es.clientPrivateKeyPassphrase)
		}
	} else if es.httpSSL && !es.clientAuth {
		cmd = append(cmd, "--from-file=ca-bundle.crt="+keysPath+"/ca.crt")
	}
	if es.userAuth {
		cmd = append(cmd, "--from-literal=username="+es.username, "--from-literal=password="+es.password)
	}

	err := oc.AsAdmin().WithoutNamespace().Run("create").Args(cmd...).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	resource{"secret", es.secretName, es.loggingNS}.WaitForResourceToAppear(oc)
}

func (es externalES) deploy(oc *exutil.CLI) {
	// create SA
	sa := resource{"serviceaccount", es.serverName, es.namespace}
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args("serviceaccount", sa.name, "-n", sa.namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	sa.WaitForResourceToAppear(oc)

	if es.userAuth {
		o.Expect(es.username).NotTo(o.BeEmpty(), "Please provide username!")
		o.Expect(es.password).NotTo(o.BeEmpty(), "Please provide password!")
	}

	if es.httpSSL || es.clientAuth || es.userAuth {
		o.Expect(es.secretName).NotTo(o.BeEmpty(), "Please provide pipeline secret name!")

		// create a temporary directory
		baseDir := testdata.FixturePath("logging")
		keysPath := filepath.Join(baseDir, "temp"+getRandomString())
		defer exec.Command("rm", "-r", keysPath).Output()
		err = os.MkdirAll(keysPath, 0755)
		o.Expect(err).NotTo(o.HaveOccurred())

		cert := certsConf{es.serverName, es.namespace, es.clientPrivateKeyPassphrase}
		cert.generateCerts(oc, keysPath)
		// create secret for ES if needed
		if es.httpSSL || es.clientAuth {
			r := resource{"secret", es.serverName, es.namespace}
			err = oc.WithoutNamespace().Run("create").Args("secret", "generic", "-n", r.namespace, r.name, "--from-file=elasticsearch.key="+keysPath+"/server.key", "--from-file=elasticsearch.crt="+keysPath+"/server.crt", "--from-file=admin-ca="+keysPath+"/ca.crt").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			r.WaitForResourceToAppear(oc)
		}

		// create pipeline secret in logging project
		es.createPipelineSecret(oc, keysPath)
	}

	// get file path per the configurations
	filePath := []string{"logging", "external-log-stores", "elasticsearch", es.version}
	if es.httpSSL {
		filePath = append(filePath, "https")
	} else {
		o.Expect(es.clientAuth).NotTo(o.BeTrue(), "Unsupported configuration, please correct it!")
		filePath = append(filePath, "http")
	}
	if es.userAuth {
		filePath = append(filePath, "user_auth")
	} else {
		filePath = append(filePath, "no_user")
	}

	// create configmap
	cm := resource{"configmap", es.serverName, es.namespace}
	cmFilePath := append(filePath, "configmap.yaml")
	cmFile := testdata.FixturePath(cmFilePath...)
	cmPatch := []string{"-f", cmFile, "-n", cm.namespace, "-p", "NAMESPACE=" + es.namespace, "-p", "NAME=" + es.serverName}
	if es.userAuth {
		cmPatch = append(cmPatch, "-p", "USERNAME="+es.username, "-p", "PASSWORD="+es.password)
	}
	if es.httpSSL {
		if es.clientAuth {
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

	cm.applyFromTemplate(oc, cmPatch...)

	// create deployment and expose svc
	deploy := resource{"deployment", es.serverName, es.namespace}
	deployFilePath := append(filePath, "deployment.yaml")
	deployFile := testdata.FixturePath(deployFilePath...)
	err = deploy.applyFromTemplate(oc, "-f", deployFile, "-n", es.namespace, "-p", "NAMESPACE="+es.namespace, "-p", "NAME="+es.serverName)
	o.Expect(err).NotTo(o.HaveOccurred())
	WaitForDeploymentPodsToBeReady(oc, es.namespace, es.serverName)
	err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("-n", es.namespace, "deployment", es.serverName, "--name="+es.serverName).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	// expose route
	if es.httpSSL {
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("-n", es.namespace, "route", "passthrough", "--service="+es.serverName, "--port=9200").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
	} else {
		err = oc.AsAdmin().WithoutNamespace().Run("expose").Args("svc/"+es.serverName, "-n", es.namespace, "--port=9200").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
	}
}

func (es externalES) remove(oc *exutil.CLI) {
	resource{"route", es.serverName, es.namespace}.clear(oc)
	resource{"service", es.serverName, es.namespace}.clear(oc)
	resource{"configmap", es.serverName, es.namespace}.clear(oc)
	resource{"deployment", es.serverName, es.namespace}.clear(oc)
	resource{"serviceaccount", es.serverName, es.namespace}.clear(oc)
	if es.httpSSL || es.userAuth {
		resource{"secret", es.secretName, es.loggingNS}.clear(oc)
	}
	if es.httpSSL {
		resource{"secret", es.serverName, es.namespace}.clear(oc)
	}
}

func (es externalES) getPodName(oc *exutil.CLI) string {
	esPods, err := oc.AdminKubeClient().CoreV1().Pods(es.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + es.serverName})
	o.Expect(err).NotTo(o.HaveOccurred())
	var names []string
	for i := 0; i < len(esPods.Items); i++ {
		names = append(names, esPods.Items[i].Name)
	}
	return names[0]
}

func (es externalES) baseCurlString() string {
	curlString := "curl -H \"Content-Type: application/json\""
	if es.userAuth {
		curlString += " -u " + es.username + ":" + es.password
	}
	if es.httpSSL {
		if es.clientAuth {
			curlString += " --cert /usr/share/elasticsearch/config/secret/elasticsearch.crt --key /usr/share/elasticsearch/config/secret/elasticsearch.key"
		}
		curlString += " --cacert /usr/share/elasticsearch/config/secret/admin-ca -s https://localhost:9200/"
	} else {
		curlString += " -s http://localhost:9200/"
	}
	return curlString
}

func (es externalES) getIndices(oc *exutil.CLI) ([]ESIndex, error) {
	cmd := es.baseCurlString() + "_cat/indices?format=JSON"
	stdout, err := e2eoutput.RunHostCmdWithRetries(es.namespace, es.getPodName(oc), cmd, 3*time.Second, 9*time.Second)
	indices := []ESIndex{}
	json.Unmarshal([]byte(stdout), &indices)
	return indices, err
}

func (es externalES) waitForIndexAppear(oc *exutil.CLI, indexName string) {
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		indices, err := es.getIndices(oc)
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

func (es externalES) getDocCount(oc *exutil.CLI, indexName string, queryString string) (int64, error) {
	cmd := es.baseCurlString() + indexName + "*/_count?format=JSON -d '" + queryString + "'"
	stdout, err := e2eoutput.RunHostCmdWithRetries(es.namespace, es.getPodName(oc), cmd, 5*time.Second, 30*time.Second)
	res := CountResult{}
	json.Unmarshal([]byte(stdout), &res)
	return res.Count, err
}

func (es externalES) waitForProjectLogsAppear(oc *exutil.CLI, projectName string, indexName string) {
	query := "{\"query\": {\"match_phrase\": {\"kubernetes.namespace_name\": \"" + projectName + "\"}}}"
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		logCount, err := es.getDocCount(oc, indexName, query)
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

func (es externalES) searchDocByQuery(oc *exutil.CLI, indexName string, queryString string) SearchResult {
	cmd := es.baseCurlString() + indexName + "*/_search?format=JSON -d '" + queryString + "'"
	stdout, err := e2eoutput.RunHostCmdWithRetries(es.namespace, es.getPodName(oc), cmd, 3*time.Second, 30*time.Second)
	o.Expect(err).ShouldNot(o.HaveOccurred())
	res := SearchResult{}
	//data := bytes.NewReader([]byte(stdout))
	//_ = json.NewDecoder(data).Decode(&res)
	json.Unmarshal([]byte(stdout), &res)
	return res
}

func (es externalES) removeIndices(oc *exutil.CLI, indexName string) {
	cmd := es.baseCurlString() + indexName + " -X DELETE"
	_, err := e2eoutput.RunHostCmdWithRetries(es.namespace, es.getPodName(oc), cmd, 3*time.Second, 30*time.Second)
	o.Expect(err).ShouldNot(o.HaveOccurred())
}
