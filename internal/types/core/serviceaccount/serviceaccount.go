package serviceaccount

import (
	"fmt"

	exutil "github.com/openshift/origin/test/extended/util"
)

const Kind = "serviceaccount"

func Create(oc *exutil.CLI, namespace, name string) error {
	err := oc.AsAdmin().WithoutNamespace().Run("create").Args(Kind, name, "-n", namespace).Execute()
	return err
}

func AddClusterRole(oc *exutil.CLI, namespace, serviceAccountName, clusterRole string) error {
	return oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "add-cluster-role-to-user", clusterRole, fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName)).Execute()
}

func RemoveClusterRole(oc *exutil.CLI, namespace, serviceAccountName, clusterRole string) error {
	return oc.AsAdmin().WithoutNamespace().Run("adm").Args("policy", "remove-cluster-role-from-user", clusterRole, fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName)).Execute()
}
