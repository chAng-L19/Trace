//go:build !windows

package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An actual owner/helper pair: cooperative mode owns a detached process group,
// matching the REA adapter's AbortController/owned-process cleanup contract.
func TestGracefulWorkerFixture(t *testing.T) {
	index := 0
	for i, value := range os.Args {
		if value == "--" {
			index = i + 1
			break
		}
	}
	if index == 0 {
		return
	}
	mode, ready := os.Args[index], os.Args[index+1]
	term := make(chan os.Signal, 1)
	if mode == "stubborn" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		signal.Notify(term, syscall.SIGTERM)
	}
	args := fixtureArgs("block", ready+".child")
	child := exec.Command(args[0], args[1:]...)
	if mode == "cooperative" {
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(ready, []byte(fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)), 0600); err != nil {
		os.Exit(4)
	}
	if mode == "stubborn" {
		time.Sleep(time.Minute)
		os.Exit(5)
	}
	<-term
	if mode == "cooperative" {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_ = child.Wait()
		if err := os.WriteFile(ready+".cleaned", []byte("helper reaped"), 0600); err != nil {
			os.Exit(6)
		}
	}
	// A cooperative program may exit 0 after cancellation. It must still be
	// reported cancelled, never completed. leave-group retains inherited pipes.
	os.Exit(0)
}

func TestRunGracefulCancellation(t *testing.T) {
	for _, mode := range []string{"cooperative", "stubborn", "leave-group"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := filepath.Join(t.TempDir(), "ready")
			type outcome struct {
				result Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				// Race builds otherwise sleep one second inside os.Exit, exactly
				// consuming this fixture's grace after cleanup has already finished.
				// Keep race detection enabled; omit only its exit delay in children.
				result, err := Run(ctx, Request{Command: []string{os.Args[0], "-test.run=^TestGracefulWorkerFixture$", "--", mode, ready}, CancelGraceSeconds: 1, Env: []string{"GORACE=" + os.Getenv("GORACE") + " atexit_sleep_ms=0"}})
				done <- outcome{result, err}
			}()
			var pids []int
			for _, text := range strings.Fields(string(waitFile(t, ready))) {
				pid, err := strconv.Atoi(text)
				if err != nil {
					t.Fatal(err)
				}
				pids = append(pids, pid)
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			}
			waitFile(t, ready+".child")
			started := time.Now()
			cancel()
			select {
			case got := <-done:
				if got.result.Status != StatusCancelled || !errors.Is(got.err, context.Canceled) || !got.result.OutcomeUncertain {
					t.Fatalf("cancelled invocation reported incorrectly: %+v, %v", got.result, got.err)
				}
				if mode == "cooperative" {
					if got.result.ExitCode == nil || *got.result.ExitCode != 0 {
						t.Fatalf("owner did not finish cleanup: %+v", got.result)
					}
					if _, err := os.ReadFile(ready + ".cleaned"); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "stubborn" && time.Since(started) < 900*time.Millisecond {
					t.Fatal("grace period was skipped")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("graceful cancellation exceeded bounded fallback")
			}
			for _, pid := range pids {
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
