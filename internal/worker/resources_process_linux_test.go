//go:build linux

package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func processRequest(t *testing.T, command ...string) Request {
	t.Helper()
	return Request{RunID: t.Name(), RequestID: "request-1", CallID: "call-1", Command: command,
		Resources: &ResourceLimits{Mode: ResourceModeProcessGroup, MemoryBytes: 1, PIDs: 1, OutputBytes: 1024},
		Env:       []string{"GORACE=atexit_sleep_ms=0"}}
}

func TestProcessResourceIdentityAndDefaultIsolation(t *testing.T) {
	first, err := ResourceRootIdentity(ProcessGroupResourceRoot)
	if err != nil || !strings.HasPrefix(first, "process-group-v1:") {
		t.Fatalf("process identity: %q %v", first, err)
	}
	t.Setenv("TRACE_EXEC_RESOURCE_MODE", "cgroup")
	t.Setenv("TRACE_EXEC_CGROUP_ROOT", "/missing/exec")
	again, err := ResourceRootIdentity(ProcessGroupResourceRoot)
	if err != nil || first != again {
		t.Fatalf("persisted backend identity changed with environment: %q %v", again, err)
	}
	for _, root := range []string{"process-group://trace-exec/v2", "process-group://trace-exec/v1/", "/missing/exec"} {
		if _, err := ResourceRootIdentity(root); err == nil {
			t.Fatalf("invalid resource root accepted: %s", root)
		}
	}
	request := processRequest(t, "/bin/sh", "-c", "printf started")
	request.Resources.Mode = ""
	result, err := Run(context.Background(), request)
	if err == nil || result.Status != StatusNotStarted || result.PID != 0 || result.Stdout != "" {
		t.Fatalf("default silently fell back to process mode: %+v %v", result, err)
	}
	request.Resources.Mode = "auto"
	result, err = Run(context.Background(), request)
	if err == nil || result.PID != 0 {
		t.Fatalf("invalid direct mode started: %+v %v", result, err)
	}
}

func TestProcessResourceExecutionAndSharedOutput(t *testing.T) {
	t.Setenv("TRACE_EXEC_CGROUP_ROOT", "/missing/strict/exec")
	request := processRequest(t, "/bin/sh", "-c", "printf '%s' \"$TRACE_EXEC_PROCESS_OWNER\"")
	request.Env = append(request.Env, processOwnerEnv+"=caller-forged-owner")
	result, err := Run(context.Background(), request)
	if err != nil || result.Status != StatusCompleted || result.CleanupError != "" || !strings.Contains(result.Stdout, ResourceOwner(request.RunID, request.RequestID, request.CallID)) || strings.Contains(result.Stdout, "caller-forged") {
		t.Fatalf("compat execution/owner failed: %+v %v", result, err)
	}
	for _, capture := range []bool{false, true} {
		t.Run(strconv.FormatBool(capture), func(t *testing.T) {
			request := processRequest(t, fixtureArgs("flood")...)
			if capture {
				request.CaptureDir = t.TempDir()
			}
			result, err := Run(context.Background(), request)
			if err == nil || result.Status != StatusUncertain || !result.OutcomeUncertain || result.ResourceLimit != "output_bytes" || result.CleanupError != "" {
				t.Fatalf("output cap did not retain uncertain semantics: %+v %v", result, err)
			}
			size := int64(len(result.Stdout) + len(result.Stderr))
			if capture {
				for _, path := range []string{result.StdoutPath, result.StderrPath} {
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					size += info.Size()
				}
			}
			if size != request.Resources.OutputBytes {
				t.Fatalf("shared output cap: got %d want %d", size, request.Resources.OutputBytes)
			}
		})
	}
}

func TestProcessResourceDetachedFixture(t *testing.T) {
	index := 0
	for i, arg := range os.Args {
		if arg == "--" {
			index = i + 1
			break
		}
	}
	if index == 0 {
		return
	}
	mode, ready := os.Args[index], os.Args[index+1]
	if mode == "leaf" {
		if err := os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(3)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestProcessResourceDetachedFixture$", "--", "leaf", ready)
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		os.Exit(4)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(5)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mode == "wait" {
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestProcessResourceCancellationAndDetachedExit(t *testing.T) {
	for _, mode := range []string{"exit", "wait"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "child")
			request := processRequest(t, os.Args[0], "-test.run=^TestProcessResourceDetachedFixture$", "--", mode, ready)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type outcome struct {
				result Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := Run(ctx, request)
				done <- outcome{result, err}
			}()
			pid, err := strconv.Atoi(string(waitFile(t, ready)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if mode == "wait" {
				cancel()
			}
			select {
			case got := <-done:
				if mode == "exit" && (got.err != nil || got.result.Status != StatusCompleted) {
					t.Fatalf("root exit: %+v %v", got.result, got.err)
				}
				if mode == "wait" && (!errors.Is(got.err, context.Canceled) || got.result.Status != StatusCancelled || !got.result.OutcomeUncertain) {
					t.Fatalf("cancel: %+v %v", got.result, got.err)
				}
				if got.result.CleanupError != "" || processAlive(pid) {
					t.Fatalf("detached child survived: %+v", got.result)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("process cleanup stalled")
			}
		})
	}
}

func TestProcessResourceClearsUntaggedOriginalGroup(t *testing.T) {
	for _, mode := range []string{"exit", "cancel", "anchor-killed"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "child")
			command := "env -i /bin/sh -c 'echo $$ > \"$1\"; exec /bin/sleep infinity' sh \"$1\" & while [ ! -s \"$1\" ]; do sleep 0.01; done; "
			switch mode {
			case "exit":
				command += "exit 0"
			case "cancel":
				command += "wait"
			case "anchor-killed":
				// The process group leader is the fixed anchor. Its unreaped PID
				// still reserves the group even if a command kills it directly.
				command += "kill -9 $(ps -o pgid= -p $$); exit 0"
			}
			request := processRequest(t, "/bin/sh", "-c", command, "sh", ready)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type outcome struct {
				result Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := Run(ctx, request)
				done <- outcome{result, err}
			}()
			pid, err := strconv.Atoi(strings.TrimSpace(string(waitFile(t, ready))))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if mode == "cancel" {
				cancel()
			}
			select {
			case got := <-done:
				if got.result.CleanupError != "" || processAlive(pid) {
					t.Fatalf("untagged original group survived: %+v %v", got.result, got.err)
				}
				if mode == "cancel" && got.result.Status != StatusCancelled {
					t.Fatalf("cancellation lost: %+v %v", got.result, got.err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("untagged group cleanup stalled")
			}
		})
	}
}

func startMarkedProcess(t *testing.T, d *processResourceDomain) *exec.Cmd {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessResourceDetachedFixture$", "--", "leaf", ready)
	cmd.Env = append(os.Environ(), processOwnerEnv+"="+d.marker, "GORACE=atexit_sleep_ms=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	waitFile(t, ready)
	return cmd
}

func TestProcessResourceRecoveryPreservesSiblingAndLifetime(t *testing.T) {
	request := processRequest(t, "/bin/sh", "-c", "printf replayed")
	d, err := newProcessResourceDomain(request.RunID, request.RequestID, request.CallID)
	if err != nil {
		t.Fatal(err)
	}
	owned := startMarkedProcess(t, d)
	sibling, err := newProcessResourceDomain(request.RunID, "different-request", request.CallID)
	if err != nil {
		t.Fatal(err)
	}
	other := startMarkedProcess(t, sibling)
	result, err := Run(context.Background(), request)
	if err == nil || result.PID != 0 || result.CleanupError == "" || !result.OutcomeUncertain || !processAlive(owned.Process.Pid) {
		t.Fatalf("existing owner replayed or cleaned implicitly: %+v %v", result, err)
	}
	start, _, err := processStart(owned.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.signal(ownedProcess{pid: owned.Process.Pid, start: "0"}); err != nil || !processAlive(owned.Process.Pid) {
		t.Fatalf("stale lifetime signalled: %s %v", start, err)
	}
	otherStart, _, err := processStart(other.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.signal(ownedProcess{pid: other.Process.Pid, start: otherStart}); err != nil || !processAlive(other.Process.Pid) {
		t.Fatalf("sibling marker ignored: %v", err)
	}
	t.Setenv("TRACE_EXEC_RESOURCE_MODE", "cgroup")
	t.Setenv("TRACE_EXEC_CGROUP_ROOT", "/wrong/current/root")
	for i := 0; i < 2; i++ {
		if err := ReconcileResourceOwner(ProcessGroupResourceRoot, request.RunID, request.RequestID, request.CallID); err != nil {
			t.Fatal(err)
		}
	}
	if processAlive(owned.Process.Pid) || !processAlive(other.Process.Pid) {
		t.Fatal("recovery left owner alive or killed sibling")
	}
	if err := ReconcileResourceOwner(ProcessGroupResourceRoot, "", "r", "c"); err == nil {
		t.Fatal("missing ownership accepted")
	}
	if got := result.ResourceOwner; got != d.owner {
		t.Fatalf("owner mismatch: %s", got)
	}
}

func TestProcessResourceRecoveryClearsUntaggedAnchoredGroup(t *testing.T) {
	d, err := newProcessResourceDomain(t.Name(), "request", "call")
	if err != nil {
		t.Fatal(err)
	}
	anchor := exec.Command("/bin/sleep", "infinity")
	anchor.Env = append(os.Environ(), processOwnerEnv+"="+d.marker, processAnchorEnv+"="+d.marker)
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := anchor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = anchor.Process.Kill(); _ = anchor.Wait() })
	child := exec.Command("/bin/sleep", "infinity")
	child.Env = []string{}
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: anchor.Process.Pid}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	current, alive, err := processSnapshot(child.Process.Pid)
	if err != nil || !alive {
		t.Fatalf("untagged child missing: %v", err)
	}
	anchorState, alive, err := processSnapshot(anchor.Process.Pid)
	if err != nil || !alive {
		t.Fatalf("anchor missing: %v", err)
	}
	staleAnchor := anchorState
	staleAnchor.start = "0"
	current.groupAnchor = &staleAnchor
	if err := d.signal(current); err == nil || !processAlive(child.Process.Pid) {
		t.Fatalf("stale group anchor authorized signal: %v", err)
	}
	foreign, err := newProcessResourceDomain(t.Name(), "foreign", "call")
	if err != nil {
		t.Fatal(err)
	}
	current.groupAnchor = &anchorState
	if err := foreign.signal(current); err == nil || !processAlive(child.Process.Pid) {
		t.Fatalf("foreign group anchor authorized signal: %v", err)
	}
	// No in-memory anchor handle is supplied to reconciliation: ownership must
	// come entirely from the surviving tagged leader and persisted call key.
	if err := ReconcileResourceOwner(ProcessGroupResourceRoot, t.Name(), "request", "call"); err != nil {
		t.Fatal(err)
	}
	if processAlive(anchor.Process.Pid) || processAlive(child.Process.Pid) {
		t.Fatal("recovery missed untagged original group")
	}
}

func TestProcessResourceTargetStartFailureReapsAnchor(t *testing.T) {
	// Include zombies: an empty tagged-live scan alone does not prove that the
	// helper was reaped. Go may fork from any runtime thread, so read each task.
	children := func() map[string]bool {
		t.Helper()
		tasks, err := os.ReadDir("/proc/self/task")
		if err != nil {
			t.Fatal(err)
		}
		pids := make(map[string]bool)
		for _, task := range tasks {
			raw, err := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "children"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, pid := range strings.Fields(string(raw)) {
				pids[pid] = true
			}
		}
		return pids
	}
	request := processRequest(t, filepath.Join(t.TempDir(), "nonexistent-target"))
	domain, err := newProcessResourceDomain(request.RunID, request.RequestID, request.CallID)
	if err != nil {
		t.Fatal(err)
	}
	before := children()
	result, err := Run(context.Background(), request)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), request.Command[0]) {
		t.Fatalf("expected target Start failure after resource preparation: %+v %v", result, err)
	}
	if result.Status != StatusNotStarted || result.PID != 0 || !result.StartedAt.IsZero() || result.ExitCode != nil || result.OutcomeUncertain || result.CleanupError != "" || result.ResourceOwner != domain.owner {
		t.Fatalf("target start failure cleanup result: %+v", result)
	}
	owned, err := domain.processes()
	if err != nil || len(owned) != 0 {
		t.Fatalf("target start failure left owned processes: %+v %v", owned, err)
	}
	for pid := range children() {
		if !before[pid] {
			t.Fatalf("target start failure left an unreaped child: %s", pid)
		}
	}
}

func processStart(pid int) (string, bool, error) {
	process, alive, err := processSnapshot(pid)
	return process.start, alive, err
}
