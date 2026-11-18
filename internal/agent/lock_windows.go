//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
var unlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")

// Lock the stable file, never unlink it: replacing a locked inode permits two owners.
func acquireFileLock(path string, wait bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	flags := uintptr(2) // LOCKFILE_EXCLUSIVE_LOCK
	if !wait {
		flags |= 1
	} // LOCKFILE_FAIL_IMMEDIATELY
	overlap := &syscall.Overlapped{}
	ok, _, callErr := lockFileEx.Call(f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(overlap)))
	if ok == 0 {
		f.Close()
		if !wait && errors.Is(callErr, syscall.Errno(33)) { // ERROR_LOCK_VIOLATION
			return nil, fmt.Errorf("%w: %w", ErrRunLockBusy, callErr)
		}
		return nil, fmt.Errorf("lock %s: %w", path, callErr)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(overlap)))
			f.Close()
		})
	}, nil
}
