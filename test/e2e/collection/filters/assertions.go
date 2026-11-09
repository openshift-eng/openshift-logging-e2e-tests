package filters

import (
	"fmt"

	"github.com/openshift/openshift-logging-e2e-tests/internal/receivers"
)

func VerifyNoRecordsOlderThanCutoff(fn func() (bool, error)) error {
	exist, err := fn()
	if err != nil {
		return err
	}
	if exist {
		return fmt.Errorf("received records older than expected")
	}
	return nil
}

// VerifyPostCutoffRecordsArrived asserts the receiver keeps getting fresh records of
// every log type: application records from the log generator project, infrastructure
// container records from an infrastructure namespace, journal records and audit records
func VerifyPostCutoffRecordsArrived(receiver receivers.Receiver, namespace string) error {
	exist, err := receiver.HasJournalLogs()
	if err != nil {
		return err
	}
	if !exist {
		return fmt.Errorf("journal logs should exist") // Eventually retries
	}
	exist, err = receiver.HasAuditLogs()
	if err != nil {
		return err
	}
	if !exist {
		return fmt.Errorf("audit logs should exist") // Eventually retries
	}

	exist, err = receiver.HasContainerLogsWith(namespace, "MERGE_JSON_LOG")
	if err != nil {
		return err
	}
	if !exist {
		return fmt.Errorf("application records from project %s do not contain the logtest marker", namespace)
	}
	return nil
}
