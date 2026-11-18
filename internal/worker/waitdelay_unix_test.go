//go:build !windows

package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestInheritedPipeKeepsRealWaitDelayError(t *testing.T) {
	result, err := Run(context.Background(), Request{Command: []string{"/bin/sh", "-c", "sleep 30 & printf '%s' $!"}, CaptureDir: t.TempDir()})
	defer syscall.Kill(-result.PID, syscall.SIGKILL)
	if !errors.Is(err, exec.ErrWaitDelay) || result.ExitCode == nil || *result.ExitCode != 0 || !result.OutcomeUncertain || result.Status != StatusUncertain {
		t.Fatalf("inherited pipe error suppressed: %+v %v", result, err)
	}
	if result.CleanupError != "" {
		t.Fatalf("cleanup not verified: %s", result.CleanupError)
	}
	raw, readErr := os.ReadFile(result.StdoutPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	child, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if parseErr != nil || child <= 0 {
		t.Fatalf("child identity missing: %q %v", raw, parseErr)
	}
	if alive, err := processGroupAlive(result.PID); err != nil || alive {
		t.Fatalf("live inherited-pipe child %d in group %d: %v", child, result.PID, err)
	}
}

func TestSuccessfulParentCleansRedirectedBackgroundGroup(t *testing.T) {
	result, err := Run(context.Background(), Request{Command: []string{"/bin/sh", "-c", "sleep 30 >/dev/null 2>&1 & printf '%s' $!"}})
	defer syscall.Kill(-result.PID, syscall.SIGKILL)
	if err != nil || result.CleanupError != "" || result.Status != StatusCompleted {
		t.Fatalf("normal root cleanup: %+v %v", result, err)
	}
	child, parseErr := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if parseErr != nil || child <= 0 {
		t.Fatalf("child identity missing: %q %v", result.Stdout, parseErr)
	}
	if alive, err := processGroupAlive(result.PID); err != nil || alive {
		t.Fatalf("redirected child %d remains in group %d: %v", child, result.PID, err)
	}
}
