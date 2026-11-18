package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixtureArgs(mode string, args ...string) []string {
	return append([]string{os.Args[0], "-test.run=^TestWorkerFixture$", "--", mode}, args...)
}

func TestWorkerFixture(t *testing.T) {
	index := 0
	for i, arg := range os.Args {
		if arg == "--" {
			index = i + 1
			break
		}
	}
	if index == 0 || index >= len(os.Args) {
		return
	}
	args := os.Args[index:]
	switch args[0] {
	case "echo":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		cwd, _ := os.Getwd()
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"stdin": string(input), "cwd": cwd, "env": os.Getenv("TRACE_WORKER_TEST"), "args": args[1:],
		})
		fmt.Fprint(os.Stderr, "worker diagnostic")
	case "failure":
		fmt.Fprint(os.Stdout, "partial output")
		fmt.Fprint(os.Stderr, "expected failure")
		os.Exit(7)
	case "flood":
		done := make(chan struct{})
		go func() {
			fmt.Fprint(os.Stderr, strings.Repeat("e", 256*1024))
			close(done)
		}()
		fmt.Fprint(os.Stdout, strings.Repeat("o", 256*1024))
		<-done
	case "block":
		os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0600)
		time.Sleep(time.Minute)
	case "tree":
		childArgs := fixtureArgs("block", args[1]+".child")
		child := exec.Command(childArgs[0], childArgs[1:]...)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		os.WriteFile(args[1], []byte(fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)), 0600)
		child.Wait()
	case "crash":
		crashFixture()
	default:
		os.Exit(4)
	}
	os.Exit(0)
}

func TestRunJSONLArgvEnvironmentAndDirectory(t *testing.T) {
	dir := t.TempDir()
	input := "{\"id\":1}\n{\"id\":2}\n"
	args := []string{"literal ; & | $HOME", "spaces remain one arg"}
	result, err := Run(context.Background(), Request{
		RunID: "run-1", CallID: "call-1", SessionID: "session-1",
		Command: fixtureArgs("echo", args...), Dir: dir, Stdin: input,
		Env: []string{"TRACE_WORKER_TEST=first", "TRACE_WORKER_TEST=second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Stdin string
		Cwd   string
		Env   string
		Args  []string
	}
	if err := json.Unmarshal([]byte(result.Stdout), &output); err != nil {
		t.Fatal(err)
	}
	if output.Stdin != input || !strings.EqualFold(output.Cwd, dir) || output.Env != "second" || strings.Join(output.Args, "\n") != strings.Join(args, "\n") {
		t.Fatalf("argv/stdin/env/cwd mismatch: %+v", output)
	}
	if result.RunID != "run-1" || result.CallID != "call-1" || result.SessionID != "session-1" || result.PID == 0 {
		t.Fatalf("identity mismatch: %+v", result)
	}
	if result.Status != StatusCompleted || result.ExitCode == nil || *result.ExitCode != 0 || result.OutcomeUncertain || result.Stderr != "worker diagnostic" {
		t.Fatalf("result mismatch: %+v", result)
	}
	if result.StartedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
		t.Fatalf("missing lifecycle timing: %+v", result)
	}
}

func TestRunFailureCrashAndStartFailure(t *testing.T) {
	result, err := Run(context.Background(), Request{Command: fixtureArgs("failure")})
	if err == nil || result.Status != StatusFailed || result.ExitCode == nil || *result.ExitCode != 7 || result.OutcomeUncertain || result.Stdout != "partial output" || result.Stderr != "expected failure" {
		t.Fatalf("normal failed exit: %+v err=%v", result, err)
	}
	result, err = Run(context.Background(), Request{Command: fixtureArgs("crash")})
	if err == nil || result.Status != StatusUncertain || !result.OutcomeUncertain {
		t.Fatalf("crash must be uncertain: %+v err=%v", result, err)
	}
	for _, command := range [][]string{nil, {""}, {filepath.Join(t.TempDir(), "nonexistent-worker")}} {
		result, err = Run(context.Background(), Request{Command: command})
		if err == nil || result.Status != StatusNotStarted || result.ExitCode != nil || !result.StartedAt.IsZero() || result.OutcomeUncertain {
			t.Fatalf("start failure: %+v err=%v", result, err)
		}
	}
}

func TestRunDrainsBothOutputStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := Run(ctx, Request{Command: fixtureArgs("flood")})
	if err != nil || result.Status != StatusCompleted || result.Stdout != strings.Repeat("o", 256*1024) || result.Stderr != strings.Repeat("e", 256*1024) {
		t.Fatalf("output draining failed: status=%s stdout=%d stderr=%d err=%v", result.Status, len(result.Stdout), len(result.Stderr), err)
	}
}

func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture not ready: %s", path)
	return nil
}

func TestRunCancellationKillsProcessTree(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			}
			defer cancel()
			ready := filepath.Join(t.TempDir(), "ready")
			type outcome struct {
				result Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := Run(ctx, Request{Command: fixtureArgs("tree", ready)})
				done <- outcome{result, err}
			}()
			pids := strings.Fields(string(waitFile(t, ready)))
			waitFile(t, ready+".child")
			for _, value := range pids {
				pid, _ := strconv.Atoi(value)
				defer func() {
					p, _ := os.FindProcess(pid)
					if p != nil {
						p.Kill()
						p.Release()
					}
				}()
			}
			expected, expectedErr := StatusCancelled, context.Canceled
			if mode == "cancel" {
				cancel()
			} else {
				expected, expectedErr = StatusTimedOut, context.DeadlineExceeded
			}
			select {
			case outcome := <-done:
				if outcome.result.Status != expected || !errors.Is(outcome.err, expectedErr) || !outcome.result.OutcomeUncertain || outcome.result.ExitCode == nil {
					t.Fatalf("cancel result: %+v err=%v", outcome.result, outcome.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("worker cancellation did not finish")
			}
			for _, value := range pids {
				pid, _ := strconv.Atoi(value)
				deadline := time.Now().Add(3 * time.Second)
				alive := processAlive(pid)
				for alive && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
					alive = processAlive(pid)
				}
				if alive {
					t.Fatalf("process survived cancellation: %d", pid)
				}
			}
		})
	}
}

func TestCancelledContextDoesNotStartWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := Run(ctx, Request{Command: fixtureArgs("echo")})
	if !errors.Is(err, context.Canceled) || result.Status != StatusNotStarted || result.PID != 0 || result.ExitCode != nil || result.OutcomeUncertain {
		t.Fatalf("pre-cancelled request: %+v err=%v", result, err)
	}
}
