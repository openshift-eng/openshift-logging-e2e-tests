package http

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	o "github.com/onsi/gomega"
	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/env"
	liburl "github.com/openshift/openshift-logging-e2e-tests/internal/lib/net/url"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

func DoRequest(header http.Header, address, path, query, method string, quiet bool, attempts int, requestBody io.Reader, expectedStatusCode int) ([]byte, error) {
	us, err := liburl.Build(address, path, query)
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
	proxy := env.Proxy()
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
