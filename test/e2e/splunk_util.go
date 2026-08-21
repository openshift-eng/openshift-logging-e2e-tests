package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

func (s *splunkPodServer) checkLogs(query string) bool {
	e2e.Logf("Splunk query: %s", query)
	err := wait.PollUntilContextTimeout(context.Background(), 30*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
		searchID, err := s.requestSearchTask(query)
		if err != nil {
			e2e.Logf("error getting search ID: %v", err)
			return false, nil
		}
		searchResult, err := s.getSearchResult(searchID)
		if err != nil {
			e2e.Logf("hit error when querying logs with %s: %v, try next round", query, err)
			return false, nil
		}
		if searchResult == nil || len(searchResult.Results) == 0 {
			e2e.Logf("no logs found for the query: %s, try next round", query)
			return false, nil
		}
		e2e.Logf("found records for the query: %s", query)
		return true, nil
	})

	return err == nil
}

func (s *splunkPodServer) auditLogFound() bool {
	return s.checkLogs("log_type=audit|head 1")
}

func (s *splunkPodServer) anyLogFound() bool {
	for _, logType := range []string{"infrastructure", "application", "audit"} {
		if s.checkLogs("log_type=" + logType + "|head 1") {
			return true
		}
	}
	return false
}

func (s *splunkPodServer) allQueryFound(queries []string) bool {
	if len(queries) == 0 {
		queries = []string{
			"log_type=application|head 1",
			"log_type=\"infrastructure\" SYSTEMD_INVOCATION_ID |head 1",
			"log_type=\"infrastructure\" container_image|head 1",
			"log_type=\"audit\" .linux-audit.log|head 1",
			"log_type=\"audit\" .ovn-audit.log|head 1",
			"log_type=\"audit\" .k8s-audit.log|head 1",
			"log_type=\"audit\" .openshift-audit.log|head 1",
		}
	}
	//return false if any query fail
	foundAll := true
	for _, query := range queries {
		if !s.checkLogs(query) {
			foundAll = false
		}
	}
	return foundAll
}

func (s *splunkPodServer) allTypeLogsFound() bool {
	queries := []string{
		"log_type=\"infrastructure\" SYSTEMD_INVOCATION_ID |head 1",
		"log_type=\"infrastructure\" container_image|head 1",
		"log_type=application|head 1",
		"log_type=audit|head 1",
	}
	return s.allQueryFound(queries)
}

func (s *splunkPodServer) getSearchResult(searchID string) (*splunkSearchResult, error) {
	h := make(http.Header)
	h.Add("Content-Type", "application/json")
	h.Add(
		"Authorization",
		"Basic "+base64.StdEncoding.EncodeToString([]byte(s.adminUser+":"+s.adminPassword)),
	)
	params := url.Values{}
	params.Add("output_mode", "json")

	var searchResult *splunkSearchResult

	resp, err1 := doHTTPRequest(h, "https://"+s.splunkdRoute, "/services/search/jobs/"+searchID+"/results", params.Encode(), "GET", true, 5, nil, 200)
	if err1 != nil {
		return nil, fmt.Errorf("failed to get response: %v", err1)
	}

	err2 := json.Unmarshal(resp, &searchResult)
	if err2 != nil {
		return nil, fmt.Errorf("failed to unmarshal splunk response: %v", err2)
	}
	return searchResult, nil
}

func (s *splunkPodServer) searchLogs(query string) (*splunkSearchResult, error) {
	searchID, err := s.requestSearchTask(query)
	if err != nil {
		return nil, fmt.Errorf("error getting search ID: %v", err)
	}
	return s.getSearchResult(searchID)
}

func (s *splunkPodServer) requestSearchTask(query string) (string, error) {
	h := make(http.Header)
	h.Add("Content-Type", "application/json")
	h.Add(
		"Authorization",
		"Basic "+base64.StdEncoding.EncodeToString([]byte(s.adminUser+":"+s.adminPassword)),
	)
	params := url.Values{}
	params.Set("search", "search "+query)

	resp, err := doHTTPRequest(h, "https://"+s.splunkdRoute, "/services/search/jobs", "", "POST", true, 2, strings.NewReader(params.Encode()), 201)
	if err != nil {
		return "", err
	}

	resmap := splunkSearchResp{}
	err = xml.Unmarshal(resp, &resmap)
	if err != nil {
		return "", err
	}
	return resmap.Sid, nil
}

// Set the default values to the splunkPodServer Object
func (s *splunkPodServer) init() {
	s.adminUser = "admin"
	s.adminPassword = getRandomString()
	s.hecToken = uuid.New().String()
	//https://idelta.co.uk/generate-hec-tokens-with-python/,https://docs.splunk.com/Documentation/SplunkCloud/9.0.2209/Security/Passwordbestpracticesforadministrators
	s.serviceName = s.name + "-0"
	s.serviceURL = s.serviceName + "." + s.namespace + ".svc"
	if s.name == "" {
		s.name = "splunk-default"
	}
	//authType must be one of "http|tls_serveronly|tls_mutual"
	//Note: when authType==http, you can still access splunk via https://${splunk_route}
	if s.authType == "" {
		s.authType = "http"
	}
	if s.version == "" {
		s.version = "9.0"
	}

	//Exit if anyone of caFile, keyFile,CertFile is null
	if s.authType == "tls_clientauth" || s.authType == "tls_serveronly" {
		o.Expect(s.caFile == "").To(o.BeFalse())
		o.Expect(s.keyFile == "").To(o.BeFalse())
		o.Expect(s.certFile == "").To(o.BeFalse())
	}
}

func (s *splunkPodServer) deploy(oc *exutil.CLI) {
	// Get route URL of splunk service
	appDomain, err := getAppDomain(oc)
	o.Expect(err).NotTo(o.HaveOccurred())
	//splunkd route URL
	s.splunkdRoute = s.name + "-splunkd-" + s.namespace + "." + appDomain
	//splunkd hec URL
	s.hecRoute = s.name + "-hec-" + s.namespace + "." + appDomain
	s.webRoute = s.name + "-web-" + s.namespace + "." + appDomain

	err = oc.AsAdmin().WithoutNamespace().Run("label").Args("namespace", s.namespace,
		"pod-security.kubernetes.io/enforce=baseline",
		"security.openshift.io/scc.podSecurityLabelSync=false",
		"--overwrite").Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	err = oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "add-scc-to-user", "nonroot", "-z", "default", "-n", s.namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	// Create secret used by splunk
	switch s.authType {
	case "http":
		s.deployHTTPSplunk(oc)
	case "tls_clientauth":
		s.deployCustomCertClientForceSplunk(oc)
	case "tls_serveronly":
		s.deployCustomCertSplunk(oc)
	default:
		s.deployHTTPSplunk(oc)
	}
	//In general, it take 1 minutes to be started, here wait 30second before call  waitForStatefulsetReady
	time.Sleep(30 * time.Second)
	waitForStatefulsetReady(oc, s.namespace, s.name)
}

func (s *splunkPodServer) deployHTTPSplunk(oc *exutil.CLI) {
	filePath := testdata.FixturePath("logging", "external-log-stores", "splunk")
	//Create secret for splunk Statefulset
	secretTemplate := filepath.Join(filePath, "secret_splunk_template.yaml")
	secret := resource{"secret", s.name, s.namespace}
	err := secret.applyFromTemplate(oc, "-f", secretTemplate, "-p", "NAME="+secret.name, "-p", "HEC_TOKEN="+s.hecToken, "-p", "PASSWORD="+s.adminPassword)
	o.Expect(err).NotTo(o.HaveOccurred())

	//create splunk StatefulSet
	statefulsetTemplate := filepath.Join(filePath, "statefulset_splunk-"+s.version+"_template.yaml")
	splunkSfs := resource{"StatefulSet", s.name, s.namespace}
	err = splunkSfs.applyFromTemplate(oc, "-f", statefulsetTemplate, "-p", "NAME="+s.name)
	o.Expect(err).NotTo(o.HaveOccurred())

	//create route for splunk service
	routeHecTemplate := filepath.Join(filePath, "route-edge_splunk_template.yaml")
	routeHec := resource{"route", s.name + "-hec", s.namespace}
	err = routeHec.applyFromTemplate(oc, "-f", routeHecTemplate, "-p", "NAME="+routeHec.name, "-p", "SERVICE_NAME="+s.serviceName, "-p", "PORT_NAME=http-hec", "-p", "ROUTE_HOST="+s.hecRoute)
	o.Expect(err).NotTo(o.HaveOccurred())

	routeSplunkdTemplate := filepath.Join(filePath, "route-passthrough_splunk_template.yaml")
	routeSplunkd := resource{"route", s.name + "-splunkd", s.namespace}
	err = routeSplunkd.applyFromTemplate(oc, "-f", routeSplunkdTemplate, "-p", "NAME="+routeSplunkd.name, "-p", "SERVICE_NAME="+s.serviceName, "-p", "PORT_NAME=https-splunkd", "-p", "ROUTE_HOST="+s.splunkdRoute)
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (s *splunkPodServer) genHecPemFile(hecFile string) error {
	dat1, err := os.ReadFile(s.certFile)
	if err != nil {
		e2e.Logf("Can not find the certFile %s", s.certFile)
		return err
	}
	dat2, err := os.ReadFile(s.keyFile)
	if err != nil {
		e2e.Logf("Can not find the keyFile %s", s.keyFile)
		return err
	}
	dat3, err := os.ReadFile(s.caFile)
	if err != nil {
		e2e.Logf("Can not find the caFile %s", s.caFile)
		return err
	}

	buf := []byte{}
	buf = append(buf, dat1...)
	buf = append(buf, dat2...)
	buf = append(buf, dat3...)
	err = os.WriteFile(hecFile, buf, 0644)
	return err
}

func (s *splunkPodServer) deployCustomCertSplunk(oc *exutil.CLI) {
	//Create basic secret content for splunk Statefulset
	filePath := testdata.FixturePath("logging", "external-log-stores", "splunk")
	secretTemplate := filepath.Join(filePath, "secret_tls_splunk_template.yaml")
	if s.passphrase != "" {
		secretTemplate = filepath.Join(filePath, "secret_tls_passphrase_splunk_template.yaml")
	}
	secret := resource{"secret", s.name, s.namespace}
	if s.passphrase != "" {
		err := secret.applyFromTemplate(oc, "-f", secretTemplate, "-p", "NAME="+secret.name, "-p", "HEC_TOKEN="+s.hecToken, "-p", "PASSWORD="+s.adminPassword, "-p", "PASSPHASE="+s.passphrase)
		o.Expect(err).NotTo(o.HaveOccurred())
	} else {
		err := secret.applyFromTemplate(oc, "-f", secretTemplate, "-p", "NAME="+secret.name, "-p", "HEC_TOKEN="+s.hecToken, "-p", "PASSWORD="+s.adminPassword)
		o.Expect(err).NotTo(o.HaveOccurred())
	}

	//HEC need all in one PEM file.
	hecPemFile := "/tmp/" + getRandomString() + "hecAllKeys.crt"
	defer os.Remove(hecPemFile)
	err := s.genHecPemFile(hecPemFile)
	o.Expect(err).NotTo(o.HaveOccurred())

	//The secret will be mounted into splunk pods and used in server.conf,inputs.conf
	args := []string{"data", "secret/" + secret.name, "-n", secret.namespace}
	args = append(args, "--from-file=hec.pem="+hecPemFile)
	args = append(args, "--from-file=ca.pem="+s.caFile)
	args = append(args, "--from-file=key.pem="+s.keyFile)
	args = append(args, "--from-file=cert.pem="+s.certFile)
	if s.passphrase != "" {
		args = append(args, "--from-literal=passphrase="+s.passphrase)
	}
	err = oc.AsAdmin().WithoutNamespace().Run("set").Args(args...).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	//create splunk StatefulSet
	statefulsetTemplate := filepath.Join(filePath, "statefulset_splunk-"+s.version+"_template.yaml")
	splunkSfs := resource{"StatefulSet", s.name, s.namespace}
	err = splunkSfs.applyFromTemplate(oc, "-f", statefulsetTemplate, "-p", "NAME="+splunkSfs.name)
	o.Expect(err).NotTo(o.HaveOccurred())

	//create route for splunk service
	routeHecTemplate := filepath.Join(filePath, "route-passthrough_splunk_template.yaml")
	routeHec := resource{"route", s.name + "-hec", s.namespace}
	err = routeHec.applyFromTemplate(oc, "-f", routeHecTemplate, "-p", "NAME="+routeHec.name, "-p", "SERVICE_NAME="+s.serviceName, "-p", "PORT_NAME=http-hec", "-p", "ROUTE_HOST="+s.hecRoute)
	o.Expect(err).NotTo(o.HaveOccurred())

	routeSplunkdTemplate := filepath.Join(filePath, "route-passthrough_splunk_template.yaml")
	routeSplunkd := resource{"route", s.name + "-splunkd", s.namespace}
	err = routeSplunkd.applyFromTemplate(oc, "-f", routeSplunkdTemplate, "-p", "NAME="+routeSplunkd.name, "-p", "SERVICE_NAME="+s.serviceName, "-p", "PORT_NAME=https-splunkd", "-p", "ROUTE_HOST="+s.splunkdRoute)
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (s *splunkPodServer) deployCustomCertClientForceSplunk(oc *exutil.CLI) {
	//Create secret for splunk Statefulset
	filePath := testdata.FixturePath("logging", "external-log-stores", "splunk")
	secretTemplate := filepath.Join(filePath, "secret_tls_splunk_template.yaml")
	if s.passphrase != "" {
		secretTemplate = filepath.Join(filePath, "secret_tls_passphrase_splunk_template.yaml")
	}
	secret := resource{"secret", s.name, s.namespace}
	if s.passphrase != "" {
		err := secret.applyFromTemplate(oc, "-f", secretTemplate, "-p", "NAME="+secret.name, "-p", "HEC_TOKEN="+s.hecToken, "-p", "PASSWORD="+s.adminPassword, "-p", "HEC_CLIENTAUTH=True", "-p", "PASSPHASE="+s.passphrase)
		o.Expect(err).NotTo(o.HaveOccurred())
	} else {
		err := secret.applyFromTemplate(oc, "-f", secretTemplate, "-p", "NAME="+secret.name, "-p", "HEC_TOKEN="+s.hecToken, "-p", "PASSWORD="+s.adminPassword, "-p", "HEC_CLIENTAUTH=True")
		o.Expect(err).NotTo(o.HaveOccurred())
	}

	//HEC need all in one PEM file.
	hecPemFile := "/tmp/" + getRandomString() + "hecAllKeys.crt"
	defer os.Remove(hecPemFile)
	err := s.genHecPemFile(hecPemFile)
	o.Expect(err).NotTo(o.HaveOccurred())

	//The secret will be mounted into splunk pods and used in server.conf,inputs.conf
	secretArgs := []string{"data", "secret/" + secret.name, "-n", secret.namespace}
	secretArgs = append(secretArgs, "--from-file=hec.pem="+hecPemFile)
	secretArgs = append(secretArgs, "--from-file=ca.pem="+s.caFile)
	secretArgs = append(secretArgs, "--from-file=key.pem="+s.keyFile)
	secretArgs = append(secretArgs, "--from-file=cert.pem="+s.certFile)
	if s.passphrase != "" {
		secretArgs = append(secretArgs, "--from-literal=passphrase="+s.passphrase)
	}
	err = oc.AsAdmin().WithoutNamespace().Run("set").Args(secretArgs...).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())

	//create splunk StatefulSet
	statefulsetTemplate := filepath.Join(filePath, "statefulset_splunk-"+s.version+"_template.yaml")
	splunkSfs := resource{"StatefulSet", s.name, s.namespace}
	err = splunkSfs.applyFromTemplate(oc, "-f", statefulsetTemplate, "-p", "NAME="+splunkSfs.name)
	o.Expect(err).NotTo(o.HaveOccurred())

	//create route for splunk service
	routeHecTemplate := filepath.Join(filePath, "route-passthrough_splunk_template.yaml")
	routeHec := resource{"route", s.name + "-hec", s.namespace}
	err = routeHec.applyFromTemplate(oc, "-f", routeHecTemplate, "-p", "NAME="+routeHec.name, "-p", "SERVICE_NAME="+s.serviceName, "-p", "PORT_NAME=http-hec", "-p", "ROUTE_HOST="+s.hecRoute)
	o.Expect(err).NotTo(o.HaveOccurred())

	routeSplunkdTemplate := filepath.Join(filePath, "route-passthrough_splunk_template.yaml")
	routeSplunkd := resource{"route", s.name + "-splunkd", s.namespace}
	err = routeSplunkd.applyFromTemplate(oc, "-f", routeSplunkdTemplate, "-p", "NAME="+routeSplunkd.name, "-p", "SERVICE_NAME="+s.serviceName, "-p", "PORT_NAME=https-splunkd", "-p", "ROUTE_HOST="+s.splunkdRoute)
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (s *splunkPodServer) destroy(oc *exutil.CLI) {
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("route", s.name+"-hec", "-n", s.namespace).Execute()
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("route", s.name+"-splunkd", "-n", s.namespace).Execute()
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("statefulset", s.name, "-n", s.namespace).Execute()
	oc.AsAdmin().WithoutNamespace().Run("delete").Args("secret", s.name, "-n", s.namespace).Execute()
	oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "remove-scc-from-user", "nonroot", "-z", "default", "-n", s.namespace).Execute()
}

// createIndexes adds custom index(es) into splunk
func (s *splunkPodServer) createIndexes(oc *exutil.CLI, indexes ...string) error {
	splunkPod, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("-n", s.namespace, "pod", "-l", "app.kubernetes.io/instance="+s.name, "-ojsonpath={.items[0].metadata.name}").Output()
	if err != nil {
		return fmt.Errorf("error getting splunk pod: %v", err)
	}
	for _, index := range indexes {
		// curl -k -u admin:gjc2t9jx  https://localhost:8089/servicesNS/admin/search/data/indexes -d name=devtutorial
		cmd := "curl -k -u admin:" + s.adminPassword + " https://localhost:8089/servicesNS/admin/search/data/indexes -d name=" + index
		stdout, err := oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("exec").Args("-n", s.namespace, splunkPod, "--", "/bin/sh", "-x", "-c", cmd).Output()
		if err != nil {
			e2e.Logf("query output: %v", stdout)
			return fmt.Errorf("can't create index %s, error: %v", index, err)
		}
	}
	return nil
}

// Create the secret which is used in CLF
func (toSp *toSplunkSecret) create(oc *exutil.CLI) {
	secretArgs := []string{"secret", "generic", toSp.name, "-n", toSp.namespace}

	if toSp.hecToken != "" {
		secretArgs = append(secretArgs, "--from-literal=hecToken="+toSp.hecToken)
	}
	if toSp.caFile != "" {
		secretArgs = append(secretArgs, "--from-file=ca-bundle.crt="+toSp.caFile)
	}
	if toSp.keyFile != "" {
		secretArgs = append(secretArgs, "--from-file=tls.key="+toSp.keyFile)
	}
	if toSp.certFile != "" {
		secretArgs = append(secretArgs, "--from-file=tls.crt="+toSp.certFile)
	}
	if toSp.passphrase != "" {
		secretArgs = append(secretArgs, "--from-literal=passphrase="+toSp.passphrase)
	}
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args(secretArgs...).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

func (toSp *toSplunkSecret) delete(oc *exutil.CLI) {
	s := resource{"secret", toSp.name, toSp.namespace}
	s.clear(oc)
}
