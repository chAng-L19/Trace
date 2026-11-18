//go:build !linux

package worker

import (
	"fmt"
	"os/exec"
)

type resourceDomain struct{ owner string }

func ResourceRootIdentity(root string) (string, error) {
	return "", fmt.Errorf("exec resource identity requires Linux")
}

func prepareResourceDomain(_ *exec.Cmd, _ Request) (*resourceDomain, error) {
	return nil, fmt.Errorf("exec resource backends require Linux; target not started")
}
func (d *resourceDomain) close() error            { return nil }
func (d *resourceDomain) finish() (string, error) { return "", nil }
func (d *resourceDomain) kill() error             { return nil }
func ReconcileResourceOwner(root, runID, requestID, callID string) error {
	return fmt.Errorf("exec resource reconciliation requires Linux")
}
