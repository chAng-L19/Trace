//go:build windows

package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// ConfigureCommand hides the console and cancels the worker process tree.
// Call before Start on a command constructed with exec.CommandContext.
func ConfigureCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		kill := exec.Command("taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if treeErr := kill.Run(); treeErr != nil {
			if err := cmd.Process.Kill(); err != nil {
				return errors.Join(treeErr, err)
			}
			// Killing only the root is not proof of descendant cleanup.
			return treeErr
		}
		return nil
	}
}

func configureGracefulCancel(_ *exec.Cmd, _ time.Duration) func() error {
	// Windows keeps taskkill /T /F. A killed root does not prove cleanup of
	// detached descendants; cancellation remains OutcomeUncertain.
	return func() error { return nil }
}

// cmd.exe parses /c with its own quote rules, not CommandLineToArgvW.
// /s removes the one outer quote pair; all operator/code quotes inside are raw.
func configureShellCommand(cmd *exec.Cmd, command string) {
	cmd.SysProcAttr.CmdLine = syscall.EscapeArg(cmd.Path) + ` /d /s /c "` + command + `"`
}

func processCrashed(state *os.ProcessState) bool {
	if state == nil {
		return true
	}
	code := uint32(state.ExitCode())
	// Windows unhandled exceptions conventionally exit with NTSTATUS codes.
	return state.ExitCode() < 0 || code&0xC0000000 == 0xC0000000
}

func cleanupProcessGroup(cmd *exec.Cmd, waitErr error) error {
	// Preserve ordinary successful Windows execution. Without Job Objects this
	// remains the existing root/process contract, not general descendant proof.
	if waitErr == nil {
		return nil
	}
	// Once the root exited, taskkill cannot certify inherited/detached descendants.
	// Keep the resource claim rather than calling a root exit full cleanup proof.
	return fmt.Errorf("process tree cleanup is unverified for exited root %d", cmd.Process.Pid)
}
