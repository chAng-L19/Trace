//go:build linux

package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecPIDFailureOutcome(t *testing.T) {
	for _, mode := range []string{"pids-failed", "memory", "output", "block"} {
		t.Run(mode, func(t *testing.T) {
			request := isolatedRequest(t, mode)
			deadline := 10 * time.Second
			if mode == "pids-failed" {
				python, err := exec.LookPath("python3")
				if err != nil {
					t.Fatal(err)
				}
				request.Resources.PIDs = 16
				// Keep submitted jobs alive until thread admission fails; finally
				// releases them so Python reports a normal exception/exit 1.
				request.Command = []string{python, "-c", `import concurrent.futures, threading
gate = threading.Event()
with concurrent.futures.ThreadPoolExecutor(max_workers=200) as pool:
    try:
        for _ in range(200):
            pool.submit(gate.wait)
    finally:
        gate.set()
`}
			} else if mode == "block" {
				deadline = 500 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			result, err := Run(ctx, request)
			if err == nil || result.PID == 0 || result.CleanupError != "" {
				t.Fatalf("expected started, cleaned failure: %+v %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(request.Resources.CgroupRoot, result.ResourceOwner)); !os.IsNotExist(err) {
				t.Fatalf("owned domain remains: %v", err)
			}
			if mode == "pids-failed" {
				stderr, readErr := os.ReadFile(result.StderrPath)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if result.Status != StatusFailed || result.OutcomeUncertain || result.ExitCode == nil || *result.ExitCode != 1 || !strings.HasPrefix(result.ResourceLimit, "pids.events:max ") || !strings.Contains(string(stderr), "can't start new thread") || !strings.Contains(result.Error, "exec resource limit reached") {
					t.Fatalf("PID failure lost definite exit or diagnostics: %+v stderr=%s err=%v", result, stderr, err)
				}
			} else if !result.OutcomeUncertain || mode == "block" && result.Status != StatusTimedOut || mode == "memory" && !strings.Contains(result.ResourceLimit, "memory.events:") || mode == "output" && !strings.Contains(result.ResourceLimit, "output_bytes") {
				t.Fatalf("interruption certainty boundary changed: %+v %v", result, err)
			}
		})
	}
}

func TestDefiniteExecFailureRequiresOnlyPIDAdmission(t *testing.T) {
	args := fixtureArgs("failure")
	cmd := exec.Command(args[0], args[1:]...)
	waitErr := cmd.Run()
	if _, ok := waitErr.(*exec.ExitError); !ok {
		t.Fatalf("missing normal exit fixture: %v", waitErr)
	}
	for _, short := range []bool{false, true} {
		w := &outputWriter{buffer: failedPIDOutput{short: short}}
		_, err := w.Write([]byte("output"))
		if err == nil || w.err == nil || definiteExecFailure(waitErr, cmd.ProcessState, w.err, "pids.events:max 1") {
			t.Fatal("output loss hidden by ordinary PID failure", err, w.err)
		}
	}
	for _, tc := range []struct {
		name, limit   string
		wait, cleanup error
		want          bool
	}{
		{"ordinary", "", waitErr, nil, true},
		{"pid-admission", "pids.events:max 1", waitErr, nil, true},
		{"oom", "memory.events:oom 1", waitErr, nil, false},
		{"mixed", "memory.events:oom 1, pids.events:max 1", waitErr, nil, false},
		{"output", "pids.events:max 1 output_bytes", waitErr, nil, false},
		{"cleanup", "pids.events:max 1", waitErr, io.ErrUnexpectedEOF, false},
		{"io-joined", "pids.events:max 1", errors.Join(waitErr, io.ErrUnexpectedEOF), nil, false},
		{"waitdelay-joined", "pids.events:max 1", errors.Join(waitErr, exec.ErrWaitDelay), nil, false},
		{"cancel-joined", "pids.events:max 1", errors.Join(waitErr, context.Canceled), nil, false},
		{"timeout-joined", "pids.events:max 1", errors.Join(waitErr, context.DeadlineExceeded), nil, false},
		{"zero-counter", "pids.events:max 0", waitErr, nil, false},
		{"malformed", "pids.events:max invalid", waitErr, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := definiteExecFailure(tc.wait, cmd.ProcessState, tc.cleanup, tc.limit); got != tc.want {
				t.Fatalf("definite failure = %t, want %t", got, tc.want)
			}
		})
	}
	args = fixtureArgs("crash")
	crash := exec.Command(args[0], args[1:]...)
	crashErr := crash.Run()
	if definiteExecFailure(crashErr, crash.ProcessState, nil, "pids.events:max 1") {
		t.Fatal("signal crash treated as definite PID failure")
	}
}

type failedPIDOutput struct{ short bool }

func (w failedPIDOutput) Write(p []byte) (int, error) {
	if w.short {
		return len(p) / 2, nil
	}
	return 0, io.ErrUnexpectedEOF
}
