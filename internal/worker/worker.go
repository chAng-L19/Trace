// Package worker executes local worker processes. The caller owns durable action
// state: persist intent before Run, persist Result afterwards, and never replay
// a started action whose result was lost.
package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Request struct {
	RequestID string   `json:"request_id,omitempty"`
	RunID     string   `json:"run_id,omitempty"`
	CallID    string   `json:"call_id,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Command   []string `json:"command"`
	// ShellCommand is set only by the exec shell adapter. Ordinary local argv
	// requests leave it empty and retain the platform's normal argv quoting.
	ShellCommand string `json:"-"`
	Dir          string `json:"dir,omitempty"`
	// Env entries supplement the host environment; the last duplicate wins.
	Env []string `json:"env,omitempty"`
	// Stdin is sent verbatim then closed. JSONL workers receive one JSON value
	// per line; protocol parsing and session interpretation belong to the driver.
	Stdin string `json:"stdin,omitempty"`
	// CancelGraceSeconds opts in to cooperative Unix cancellation. The root
	// receives SIGTERM before hard termination, allowing owned detached helpers
	// to be cleaned up. Zero preserves immediate process-tree cancellation.
	CancelGraceSeconds int `json:"cancel_grace_seconds,omitempty"`
	// CaptureDir streams output to files instead of memory. The caller owns
	// Result.StdoutPath/StderrPath and must archive/remove them, including errors.
	CaptureDir string                       `json:"-"`
	OnOutput   func(stream, content string) `json:"-"`
	Resources  *ResourceLimits              `json:"resources,omitempty"`
}

type Status string

const (
	StatusNotStarted Status = "not_started"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
	StatusTimedOut   Status = "timed_out"
	StatusUncertain  Status = "uncertain"
)

type Result struct {
	RunID            string    `json:"run_id,omitempty"`
	CallID           string    `json:"call_id,omitempty"`
	SessionID        string    `json:"session_id,omitempty"`
	PID              int       `json:"pid,omitempty"`
	Status           Status    `json:"status"`
	StdoutPath       string    `json:"-"`
	StderrPath       string    `json:"-"`
	Stdout           string    `json:"stdout"`
	Stderr           string    `json:"stderr"`
	ExitCode         *int      `json:"exit_code"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	FinishedAt       time.Time `json:"finished_at"`
	OutcomeUncertain bool      `json:"outcome_uncertain"`
	Error            string    `json:"error,omitempty"`
	CleanupError     string    `json:"cleanup_error,omitempty"`
	ResourceOwner    string    `json:"resource_owner,omitempty"`
	ResourceLimit    string    `json:"resource_limit,omitempty"`
}

// Run starts exactly the supplied argv, without an implicit shell. A nonzero
// process exit returns both its recorded Result and the underlying exit error.
// Completed means the process exited successfully, not that its task is proven.
func Run(ctx context.Context, request Request) (result Result, runErr error) {
	result = Result{RunID: request.RunID, CallID: request.CallID, SessionID: request.SessionID, Status: StatusNotStarted}
	defer func() {
		result.FinishedAt = time.Now().UTC()
		if runErr != nil {
			result.Error = runErr.Error()
		}
	}()
	if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
		return result, fmt.Errorf("worker command is required")
	}
	if request.CancelGraceSeconds < 0 || int64(request.CancelGraceSeconds) > int64((1<<63-1)/time.Second) {
		return result, fmt.Errorf("invalid worker cancel_grace_seconds")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	processCtx, cancelProcess := context.WithCancel(ctx)
	defer cancelProcess()
	cmd := exec.CommandContext(processCtx, request.Command[0], request.Command[1:]...)
	cmd.Dir = request.Dir
	cmd.Env = append(cmd.Environ(), request.Env...)
	cmd.Stdin = strings.NewReader(request.Stdin)
	var stdout, stderr bytes.Buffer
	var outWriter, errWriter io.Writer = &stdout, &stderr
	var outFile, errFile *os.File
	if request.CaptureDir != "" {
		var err error
		if err = os.MkdirAll(request.CaptureDir, 0700); err != nil {
			return result, err
		}
		outFile, err = os.CreateTemp(request.CaptureDir, ".stdout-*")
		if err != nil {
			return result, err
		}
		errFile, err = os.CreateTemp(request.CaptureDir, ".stderr-*")
		if err != nil {
			outFile.Close()
			os.Remove(outFile.Name())
			return result, err
		}
		result.StdoutPath, result.StderrPath = outFile.Name(), errFile.Name()
		defer outFile.Close()
		defer errFile.Close()
		outWriter, errWriter = outFile, errFile
	}
	var budget *outputBudget
	if request.Resources != nil {
		budget = &outputBudget{remaining: request.Resources.OutputBytes, cancel: cancelProcess}
	}
	stdoutWriter := &outputWriter{buffer: outWriter, stream: "stdout", emit: request.OnOutput, budget: budget}
	stderrWriter := &outputWriter{buffer: errWriter, stream: "stderr", emit: request.OnOutput, budget: budget}
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter
	ConfigureCommand(cmd)
	cleanupCancelled := configureGracefulCancel(cmd, time.Duration(request.CancelGraceSeconds)*time.Second)
	if request.ShellCommand != "" {
		configureShellCommand(cmd, request.ShellCommand)
	}
	var domain *resourceDomain
	if request.Resources != nil {
		var err error
		result.ResourceOwner = ResourceOwner(request.RunID, request.RequestID, request.CallID)
		domain, err = prepareResourceDomain(cmd, request)
		if err != nil {
			var cleanupErr *resourceSetupCleanupError
			if errors.As(err, &cleanupErr) {
				result.CleanupError = cleanupErr.Error()
				result.OutcomeUncertain = true
			}
			return result, err
		}
		result.ResourceOwner = domain.owner
	}
	if err := cmd.Start(); err != nil {
		if domain != nil {
			_, cleanupErr := domain.finish()
			if cleanupErr != nil {
				result.CleanupError = cleanupErr.Error()
			}
			err = errors.Join(err, cleanupErr)
		}
		return result, err
	}
	result.PID = cmd.Process.Pid
	result.StartedAt = time.Now().UTC()
	waitErr := cmd.Wait()
	runErr = waitErr
	outputErr := errors.Join(stdoutWriter.err, stderrWriter.err)
	runErr = errors.Join(runErr, outputErr)
	var cleanupErr error
	if ctx.Err() != nil {
		if domain == nil {
			cleanupErr = cleanupCancelled()
		}
		runErr = errors.Join(runErr, ctx.Err(), cleanupErr)
	}
	// A parent exit does not close inherited pipes or stop its descendants.
	// Preserve the original wait error while independently checking ownership cleanup.
	if domain != nil {
		var domainErr error
		result.ResourceLimit, domainErr = domain.finish()
		cleanupErr = errors.Join(cleanupErr, domainErr)
	} else {
		cleanupErr = errors.Join(cleanupErr, cleanupProcessGroup(cmd, runErr))
	}
	if budget.hit() {
		result.ResourceLimit = strings.TrimSpace(result.ResourceLimit + " output_bytes")
	}
	if result.ResourceLimit != "" {
		runErr = errors.Join(runErr, fmt.Errorf("exec resource limit reached: %s", result.ResourceLimit))
	}
	if cleanupErr != nil {
		result.CleanupError = cleanupErr.Error()
		runErr = errors.Join(runErr, cleanupErr)
	}
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	if cmd.ProcessState != nil {
		exitCode := cmd.ProcessState.ExitCode()
		result.ExitCode = &exitCode
	}
	// Output writer failures also leave outcome unknown; only ordinary nonzero
	// exits with no cleanup failure have a definite failed outcome.
	result.Status = StatusCompleted
	if runErr == nil {
		return result, nil
	}
	result.Status = StatusUncertain
	result.OutcomeUncertain = true
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Status = StatusTimedOut
		runErr = errors.Join(ctx.Err(), runErr)
	} else if errors.Is(ctx.Err(), context.Canceled) {
		result.Status = StatusCancelled
		runErr = errors.Join(ctx.Err(), runErr)
	} else {
		if definiteExecFailure(waitErr, cmd.ProcessState, errors.Join(cleanupErr, outputErr), result.ResourceLimit) {
			result.Status = StatusFailed
			result.OutcomeUncertain = false
		}
	}
	return result, runErr
}

// A PID admission failure does not itself interrupt the process. Preserve an
// observed normal failed exit after ownership cleanup, including its diagnostic.
// Use the original, unjoined Wait error: an ExitError inside a joined I/O or
// WaitDelay error is not proof of an ordinary failure.
func definiteExecFailure(waitErr error, state *os.ProcessState, cleanupErr error, resourceLimit string) bool {
	if _, ok := waitErr.(*exec.ExitError); !ok || state == nil || state.ExitCode() <= 0 || processCrashed(state) || cleanupErr != nil {
		return false
	}
	if resourceLimit == "" {
		return true
	}
	fields := strings.Fields(resourceLimit)
	if len(fields) != 2 || fields[0] != "pids.events:max" {
		return false
	}
	count, err := strconv.ParseUint(fields[1], 10, 64)
	return err == nil && count > 0
}

type outputWriter struct {
	buffer io.Writer
	stream string
	emit   func(string, string)
	budget *outputBudget
	err    error
}

func (w *outputWriter) Write(p []byte) (int, error) {
	original := len(p)
	if w.budget != nil {
		allowed, _ := w.budget.take(len(p))
		p = p[:allowed]
	}
	n, err := w.buffer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil && w.err == nil {
		w.err = err
	}
	if w.emit != nil {
		for offset := 0; offset < n; offset += 4096 {
			w.emit(w.stream, string(p[offset:min(n, offset+4096)]))
		}
	}
	// Drain/discard excess until cancellation completes; preserve the limit cause.
	if err == nil {
		return original, nil
	}
	return n, err
}
