//go:build !windows && !linux

package worker

import (
	"errors"
	"syscall"
)

func processGroupAlive(group int) (bool, error) {
	err := syscall.Kill(-group, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return err == nil, err
}
