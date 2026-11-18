//go:build !windows

package worker

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func crashFixture() {
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return false
	}
	// Linux may leave an adopted zombie until init reaps it; it is not running.
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
		if len(fields) > 0 && fields[0] == "Z" {
			return false
		}
	}
	return true
}
