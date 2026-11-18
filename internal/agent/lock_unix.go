//go:build !windows

package agent

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
)

func acquireFileLock(path string, wait bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	flags := syscall.LOCK_EX
	if !wait {
		flags |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), flags); err != nil {
		f.Close()
		if !wait && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, fmt.Errorf("%w: %w", ErrRunLockBusy, err)
		}
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }) }, nil
}
