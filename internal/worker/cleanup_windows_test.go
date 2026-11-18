//go:build windows

package worker

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestWindowsPostWaitCleanupDoesNotSignalReusedPID(t *testing.T) {
	called := false
	cmd := &exec.Cmd{Process: &os.Process{Pid: 12345}, Cancel: func() error { called = true; return nil }}
	if err := cleanupProcessGroup(cmd, nil); err != nil {
		t.Fatalf("ordinary success regressed: %v", err)
	}
	if err := cleanupProcessGroup(cmd, exec.ErrWaitDelay); err == nil {
		t.Fatal("inherited-pipe cleanup falsely verified")
	}
	if err := cleanupProcessGroup(cmd, errors.New("cancel failure")); err == nil {
		t.Fatal("failed cleanup falsely verified")
	}
	if called {
		t.Fatal("post-Wait cleanup signalled a potentially reused PID")
	}
}
