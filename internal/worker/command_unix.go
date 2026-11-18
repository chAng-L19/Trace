//go:build !windows

package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ConfigureCommand gives a CommandContext its own process group and cancels the
// whole group. Call before Start; no shell or background daemon is introduced.
func ConfigureCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

func configureGracefulCancel(cmd *exec.Cmd, grace time.Duration) func() error {
	if grace <= 0 {
		return func() error { return nil }
	}
	cmd.WaitDelay = grace
	cmd.Cancel = func() error {
		// Signal only the owner: its AbortController must retain the opportunity
		// to clean detached helpers before Go's WaitDelay kills the root.
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return func() error {
		// WaitDelay kills only the root. Reap any still-running members of its
		// original group after Wait, including children retaining output pipes.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
}

func configureShellCommand(_ *exec.Cmd, _ string) {}

func processCrashed(state *os.ProcessState) bool {
	return state == nil || state.ExitCode() < 0
}

func cleanupProcessGroup(cmd *exec.Cmd, _ error) error {
	pid := cmd.Process.Pid
	alive, err := processGroupAlive(pid)
	if err != nil {
		return fmt.Errorf("inspect owned process group %d: %w", pid, err)
	}
	if !alive {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate owned process group %d: %w", pid, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		alive, err := processGroupAlive(pid)
		if err != nil {
			return fmt.Errorf("verify owned process group %d: %w", pid, err)
		}
		if !alive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("owned process group %d still has live processes", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
