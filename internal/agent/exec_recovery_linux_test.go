//go:build linux

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/worker"
)

// Only the test helper is killed. Its real exec resources and durable Action are
// reconciled by a fresh Agent, never by a synthetic successful callback.
func TestExecRecoveryCrashFixture(t *testing.T) {
	if os.Getenv("TRACE_EXEC_RECOVERY_HELPER") != "1" {
		return
	}
	store, err := NewStore(os.Getenv("TRACE_EXEC_RECOVERY_STORE"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a, err := New(store, MockProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	id := os.Getenv("TRACE_EXEC_RECOVERY_RUN")
	release, err := store.AcquireRunLock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = a.executeCommand(context.Background(), id, "crash-call", []string{"/bin/sh", "-c", `printf '%s\n' "$$" > "$1.pid"; printf 'launched\n' >> "$1"; exec /bin/sleep 120`, "sh", os.Getenv("TRACE_EXEC_RECOVERY_MARKER")})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecRecoveryRealCrashOwnerNoReplay(t *testing.T) {
	root := os.Getenv("TRACE_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("requires an isolated delegated cgroup via TRACE_TEST_CGROUP_ROOT")
	}
	identity, err := worker.ResourceRootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
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
			marker := filepath.Join(t.TempDir(), "launches")
			owner := worker.ResourceOwner(id, "crash-request", "crash-call")
			ownerPath := filepath.Join(root, owner)
			log, err := os.Create(filepath.Join(t.TempDir(), "helper.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			helper := exec.Command(os.Args[0], "-test.run=^TestExecRecoveryCrashFixture$")
			helper.Env = append(os.Environ(), "TRACE_EXEC_RECOVERY_HELPER=1", "TRACE_EXEC_RECOVERY_STORE="+store.root, "TRACE_EXEC_RECOVERY_RUN="+id, "TRACE_EXEC_RECOVERY_MARKER="+marker, "TRACE_EXEC_RESOURCE_MODE=cgroup", "TRACE_EXEC_CGROUP_ROOT="+root, "TRACE_EXEC_MEMORY_BYTES=134217728", "TRACE_EXEC_PIDS=48", "TRACE_EXEC_OUTPUT_BYTES=65536")
			helper.Stdout, helper.Stderr = log, log
			if err := helper.Start(); err != nil {
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
			deadline := time.Now().Add(10 * time.Second)
			for {
				started, _ := os.ReadFile(marker)
				events, _ := os.ReadFile(filepath.Join(ownerPath, "cgroup.events"))
				if string(started) == "launched\n" && strings.Contains(string(events), "populated 1") {
					break
				}
				if time.Now().After(deadline) {
					raw, _ := os.ReadFile(log.Name())
					t.Fatalf("real helper did not launch: %s", raw)
				}
				time.Sleep(10 * time.Millisecond)
			}
			sibling := worker.Request{RunID: id + "-sibling", RequestID: "r", CallID: "c", Command: []string{"/bin/sleep", "120"}, Resources: &worker.ResourceLimits{CgroupRoot: root, MemoryBytes: 128 << 20, PIDs: 48, OutputBytes: 65536}}
			siblingCtx, cancel := context.WithCancel(context.Background())
			siblingDone := make(chan worker.Result, 1)
			go func() { result, _ := worker.Run(siblingCtx, sibling); siblingDone <- result }()
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
			siblingPath := filepath.Join(root, worker.ResourceOwner(sibling.RunID, sibling.RequestID, sibling.CallID), "cgroup.events")
			deadline = time.Now().Add(5 * time.Second)
			for {
				raw, _ := os.ReadFile(siblingPath)
				if strings.Contains(string(raw), "populated 1") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("sibling did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := helper.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = helper.Wait()
			waited = true
			before, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(before.Actions) != 1 || before.Actions[0].Status != "started" || before.Actions[0].ResourceOwner != owner || before.Actions[0].ResourceRootIdentity != identity || before.Actions[0].Result != nil {
				t.Fatalf("real durable intent missing: %+v", before.Actions)
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
			t.Setenv("TRACE_EXEC_CGROUP_ROOT", t.TempDir()) // Recovery must use persisted root.
			t.Setenv("TRACE_EXEC_RESOURCE_MODE", "process-group")
			if err = fresh.RecoverExecResources(); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(ownerPath); !os.IsNotExist(err) {
				t.Fatalf("owned cgroup remains: %v", err)
			}
			if raw, err := os.ReadFile(siblingPath); err != nil || !strings.Contains(string(raw), "populated 1") {
				t.Fatalf("cross-owner cleanup: %s %v", raw, err)
			}
			after, err := freshStore.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(after.Actions) != 1 || after.Actions[0].Status != "uncertain" || after.Actions[0].ResourceCleanupStatus != "completed" || after.Actions[0].Result != nil || len(after.Evidence) != len(before.Evidence) || after.Run.Status != before.Run.Status {
				t.Fatalf("crash recovery forged outcome: %+v", after.Actions)
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
			if data, err := os.ReadFile(marker); err != nil || string(data) != "launched\n" {
				t.Fatal("external action replayed")
			}
			t.Logf("backend=%s owner=%s cleaned; sibling alive; actions=1 model_calls=1 replay=0", backend, owner)
		})
	}
}
