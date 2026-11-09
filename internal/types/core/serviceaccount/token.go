package serviceaccount

import (
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
)

// Token returns the token for a serviceaccount
func Token(oc *exutil.CLI, name, ns string) string {
	token, err := oc.AsAdmin().WithoutNamespace().Run("create").Args("token", name, "-n", ns).Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	return token
}
