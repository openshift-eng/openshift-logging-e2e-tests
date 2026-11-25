package certificates

import (
	"os/exec"

	exutil "github.com/openshift/origin/test/extended/util"

	"github.com/openshift/openshift-logging-e2e-tests/internal/lib/clients/cluster"
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"

	. "github.com/onsi/gomega"
)

type Conf struct {
	ServerName string
	Namespace  string
	PassPhrase string // client private key passphrase
}

func New(serverName, namespace, passPhrase string) *Conf {
	return &Conf{ServerName: serverName, Namespace: namespace, PassPhrase: passPhrase}
}

func (certs *Conf) Generate(oc *exutil.CLI, keysPath string) {
	generateCertsSH := testdata.FixturePath("logging", "external-log-stores", "cert_generation.sh")
	domain, err := cluster.Domain(oc)
	Expect(err).NotTo(HaveOccurred())
	cmd := []string{generateCertsSH, keysPath, certs.Namespace, certs.ServerName, domain}
	if certs.PassPhrase != "" {
		cmd = append(cmd, certs.PassPhrase)
	}
	err = exec.Command("sh", cmd...).Run()
	Expect(err).NotTo(HaveOccurred())
}
