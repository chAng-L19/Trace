//go:build windows

package worker

import (
	"os"
	"syscall"
)

func crashFixture() {
	code := uint32(0xC0000005)
	os.Exit(int(code))
}

func processAlive(pid int) bool {
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return err != syscall.Errno(87)
	}
	defer syscall.CloseHandle(handle)
	state, err := syscall.WaitForSingleObject(handle, 0)
	return err != nil || state != syscall.WAIT_OBJECT_0
}
