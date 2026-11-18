//go:build linux

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"trace/internal/worker"
)

func execRecoveryPIDAlive(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(raw)[strings.LastIndexByte(string(raw), ')')+1:])
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

func execRecoveryWaitPID(t *testing.T, path string, log *os.File) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(path)
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err == nil && pid > 0 && execRecoveryPIDAlive(pid) {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := os.ReadFile(log.Name())
	t.Fatalf("process-group helper did not launch: %s", raw)
	return 0
}

func TestExecRecoveryProcessGroupCrashNoReplay(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("TRACE_EXEC_RESOURCE_MODE", "process-group")
			a, store := newTestAgent(t, MockProvider{})
			if backend == "sqlite" {
				store = sqliteTestStore(t)
				var err error
				a, err = New(store, MockProvider{}, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Run.Status = StatusWaitingTool
				s.Requests = []ModelRequest{{ID: "crash-request"}}
				s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "crash-call", Name: "exec", Arguments: `{}`}}})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			root, err := worker.ExecResourceRoot()
			if err != nil {
				t.Fatal(err)
			}
			identity, err := worker.ResourceRootIdentity(root)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "launches")
			log, err := os.Create(filepath.Join(t.TempDir(), "helper.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			helper := exec.Command(os.Args[0], "-test.run=^TestExecRecoveryCrashFixture$")
			helper.Env = append(os.Environ(), "TRACE_EXEC_RECOVERY_HELPER=1", "TRACE_EXEC_RECOVERY_STORE="+store.root, "TRACE_EXEC_RECOVERY_RUN="+id, "TRACE_EXEC_RECOVERY_MARKER="+marker, "TRACE_EXEC_RESOURCE_MODE=process-group", "TRACE_EXEC_OUTPUT_BYTES=65536")
			helper.Stdout, helper.Stderr = log, log
			if err = helper.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = helper.Process.Kill()
					_ = helper.Wait()
				}
				if err := worker.ReconcileResourceOwner(root, id, "crash-request", "crash-call"); err != nil {
					t.Error(err)
				}
			}()
			pid := execRecoveryWaitPID(t, marker+".pid", log)
			// Both a different managed owner and an unmarked same-UID process
			// must survive recovery of the crashed controller's action.
			siblingPIDPath := filepath.Join(t.TempDir(), "sibling.pid")
			siblingCtx, cancel := context.WithCancel(context.Background())
			siblingDone := make(chan worker.Result, 1)
			go func() {
				result, _ := worker.Run(siblingCtx, worker.Request{RunID: id + "-sibling", RequestID: "r", CallID: "c", Command: []string{"/bin/sh", "-c", `printf '%s\n' "$$" > "$1"; exec /bin/sleep 120`, "sh", siblingPIDPath}, Resources: &worker.ResourceLimits{Mode: worker.ResourceModeProcessGroup, MemoryBytes: 128 << 20, PIDs: 48, OutputBytes: 65536}})
				siblingDone <- result
			}()
			defer func() {
				cancel()
				select {
				case result := <-siblingDone:
					if result.CleanupError != "" {
						t.Error(result.CleanupError)
					}
				case <-time.After(5 * time.Second):
					t.Error("sibling cleanup did not settle")
				}
			}()
			siblingPID := execRecoveryWaitPID(t, siblingPIDPath, log)
			outsider := exec.Command("/bin/sleep", "120")
			if err = outsider.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = outsider.Process.Kill(); _ = outsider.Wait() }()
			if err = helper.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = helper.Wait()
			waited = true
			before, err := store.Load(id)
			if err != nil || len(before.Actions) != 1 {
				t.Fatalf("missing durable action: %+v %v", before.Actions, err)
			}
			action := before.Actions[0]
			if action.Status != "started" || action.ResourceRoot != worker.ProcessGroupResourceRoot || action.ResourceRootIdentity != identity || action.ResourceOwner != worker.ResourceOwner(id, "crash-request", "crash-call") || action.Result != nil {
				t.Fatalf("incorrect process-group intent: %+v", action)
			}
			freshStore, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer freshStore.Close()
			provider := &scriptedProvider{responses: []ModelResponse{{Content: "inspect recovered tool outcome"}}}
			fresh, err := New(freshStore, provider, "")
			if err != nil {
				t.Fatal(err)
			}
			// Operator mode changes must never reinterpret old resource owners.
			t.Setenv("TRACE_EXEC_RESOURCE_MODE", "cgroup")
			t.Setenv("TRACE_EXEC_CGROUP_ROOT", t.TempDir())
			if err = fresh.RecoverExecResources(); err != nil {
				t.Fatal(err)
			}
			if execRecoveryPIDAlive(pid) || !execRecoveryPIDAlive(siblingPID) || !execRecoveryPIDAlive(outsider.Process.Pid) {
				t.Fatal("recovery failed to isolate the persisted owner")
			}
			after, err := freshStore.Load(id)
			if err != nil || len(after.Actions) != 1 || after.Actions[0].Status != "uncertain" || after.Actions[0].ResourceCleanupStatus != "completed" || after.Actions[0].Result != nil || len(after.Evidence) != len(before.Evidence) || after.Run.Status != before.Run.Status {
				t.Fatalf("recovery forged outcome: %+v %v", after.Actions, err)
			}
			events := len(after.Events)
			if err = fresh.RecoverExecResources(); err != nil {
				t.Fatal(err)
			}
			after, err = freshStore.Load(id)
			if err != nil || len(after.Events) != events {
				t.Fatal("repeat recovery wrote duplicate cleanup")
			}
			after, err = fresh.Run(context.Background(), id)
			if err != nil || after.Run.Status != StatusCompleted || len(provider.requests) != 1 || after.Actions[0].Result != nil || after.Actions[0].Status != "uncertain" || len(after.Evidence) != len(before.Evidence)+1 || after.Evidence[len(after.Evidence)-1].Type != "runtime_observation" {
				t.Fatalf("resume replayed or lost model observation: %s %v", after.Run.Status, err)
			}
			if raw, err := os.ReadFile(marker); err != nil || string(raw) != "launched\n" {
				t.Fatalf("external action replayed: %q %v", raw, err)
			}
			t.Logf("backend=%s persisted process-group owner cleaned after mode switch; sibling/unmarked process alive; model_calls=1 replay=0", backend)
		})
	}
}
