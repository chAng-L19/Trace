package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestProcessFixture(t *testing.T) {
	mode := os.Getenv("TRACE_TEST_CHILD")
	if mode == "" {
		return
	}
	root := os.Getenv("TRACE_TEST_ROOT")
	id := os.Getenv("TRACE_TEST_ID")
	switch mode {
	case "pause", "cancel":
		store, err := NewStore(root)
		if err != nil {
			os.Exit(2)
		}
		a, err := New(store, MockProvider{}, "")
		if err != nil {
			os.Exit(3)
		}
		if mode == "pause" {
			_, err = a.Pause(id)
		} else {
			_, err = a.Cancel(id)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
	case "lock":
		store, err := NewStore(root)
		if err != nil {
			os.Exit(2)
		}
		_, err = store.AcquireRunLock(id)
		if err != nil {
			os.Exit(3)
		}
		os.WriteFile(os.Getenv("TRACE_TEST_READY"), []byte("ready"), 0600)
		time.Sleep(time.Minute)
	case "update":
		store, err := NewStore(root)
		if err != nil {
			os.Exit(2)
		}
		for i := 0; i < 20; i++ {
			if _, err := store.Update(id, func(s *Snapshot) error { addEvent(s, "process", "process update", nil); return nil }); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(3)
			}
		}
	case "silent":
		os.Exit(0)
	case "sideeffect":
		path := filepath.Join(root, strconv.Itoa(os.Getpid()))
		os.WriteFile(path, []byte("side effect"), 0600)
		time.Sleep(250 * time.Millisecond)
		fmt.Print("done")
		os.Exit(0)
	case "blocking_tool":
		os.WriteFile(os.Getenv("TRACE_TEST_READY"), []byte("ready"), 0600)
		time.Sleep(time.Minute)
	}
}

func fixtureCommand(mode, root, id, ready string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestProcessFixture$")
	command.Env = append(os.Environ(), "TRACE_TEST_CHILD="+mode, "TRACE_TEST_ROOT="+root, "TRACE_TEST_ID="+id, "TRACE_TEST_READY="+ready)
	return command
}
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture file not ready: %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProcessLockReleaseAfterOwnerDeath(t *testing.T) {
	a, s := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	ready := filepath.Join(t.TempDir(), "ready")
	child := fixtureCommand("lock", s.root, id, ready)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	waitForFile(t, ready)
	if release, err := s.AcquireRunLock(id); err == nil {
		release()
		t.Fatal("stole live child lock")
	}
	child.Process.Kill()
	child.Wait()
	release, err := s.AcquireRunLock(id)
	if err != nil {
		t.Fatalf("dead owner retained OS lock: %v", err)
	}
	release()
}

func TestCrossProcessAndStoreUpdatesAreAtomic(t *testing.T) {
	a, s := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	initial, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	baseEvents := len(initial.Events)
	one := fixtureCommand("update", s.root, id, "")
	two := fixtureCommand("update", s.root, id, "")
	if err := one.Start(); err != nil {
		t.Fatal(err)
	}
	if err := two.Start(); err != nil {
		one.Process.Kill()
		one.Wait()
		t.Fatal(err)
	}
	if err := one.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := two.Wait(); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Events) != baseEvents+40 {
		t.Fatalf("cross-process lost events: %d", len(snap.Events))
	}
	second, _ := NewStore(s.root)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*Store{s, second} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, err := store.Update(id, func(s *Snapshot) error { addEvent(s, "instance", "update", nil); return nil }); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}(store)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	snap, _ = s.Load(id)
	if len(snap.Events) != baseEvents+60 {
		t.Fatalf("cross-store lost events: %d", len(snap.Events))
	}
	for i, event := range snap.Events {
		if event.Sequence != i+1 {
			t.Fatal("event sequence gap")
		}
	}
}

func TestInvalidLockPathReturnsError(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	if err := os.Remove(filepath.Join(store.root, ".store.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.root, ".store.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("goal", "prompt", "hash"); err == nil {
		t.Fatal("invalid lock path accepted")
	}
}

func TestConcurrentToolSideEffectAndSilentSuccess(t *testing.T) {
	p := manualProvider(ToolCall{ID: "one", Name: "external", Arguments: "{}"})
	a, s := newTestAgent(t, p)
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	markers := t.TempDir()
	t.Setenv("TRACE_TEST_CHILD", "sideeffect")
	t.Setenv("TRACE_TEST_ROOT", markers)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := a.ExecuteTool(context.Background(), id, "one", []string{os.Args[0], "-test.run=^TestProcessFixture$"})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	failures := 0
	for i := 0; i < 2; i++ {
		if <-errs != nil {
			failures++
		}
	}
	entries, _ := os.ReadDir(markers)
	if len(entries) != 1 || failures != 1 {
		t.Fatalf("duplicate side effects: markers=%d failures=%d", len(entries), failures)
	}
	snap, _ := s.Load(id)
	if len(snap.Actions) != 1 || snap.Actions[0].Result == nil {
		t.Fatal("action result not durable")
	}
	t.Setenv("TRACE_TEST_CHILD", "silent")
	p = manualProvider(ToolCall{ID: "silent", Name: "external", Arguments: "{}"})
	a, _ = newTestAgent(t, p)
	id = createTestRun(t, a)
	waitTestRun(t, a, id)
	snap, err := a.ExecuteTool(context.Background(), id, "silent", []string{os.Args[0], "-test.run=^TestProcessFixture$"})
	if err != nil {
		t.Fatal(err)
	}
	result := snap.Actions[0].Result
	if result == nil || result.ExitCode != 0 || result.Stdout != "" || result.Stderr != "" {
		t.Fatalf("silent result invalid: %+v", result)
	}
}

func TestControlInterruptsRunningToolAndPersistsResult(t *testing.T) {
	for _, mode := range []string{"pause", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			p := manualProvider(ToolCall{ID: "one", Name: "external", Arguments: "{}"})
			a, s := newTestAgent(t, p)
			id := createTestRun(t, a)
			waitTestRun(t, a, id)
			ready := filepath.Join(t.TempDir(), "ready")
			t.Setenv("TRACE_TEST_CHILD", "blocking_tool")
			t.Setenv("TRACE_TEST_READY", ready)
			done := make(chan error, 1)
			go func() {
				_, err := a.ExecuteTool(context.Background(), id, "one", []string{os.Args[0], "-test.run=^TestProcessFixture$"})
				done <- err
			}()
			waitForFile(t, ready)
			before, _ := s.Load(id)
			if len(before.Actions) != 1 || before.Actions[0].Status != "started" {
				t.Fatal("action start was not committed before side effect")
			}
			var err error
			expected := StatusPaused
			if mode == "pause" {
				_, err = a.Pause(id)
			} else {
				_, err = a.Cancel(id)
				expected = StatusCancelled
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("tool not interrupted")
			}
			snap, _ := s.Load(id)
			if snap.Run.Status != expected || snap.Actions[0].Result == nil || !snap.Actions[0].Result.Interrupted {
				t.Fatalf("interrupted result missing: %+v", snap)
			}
		})
	}
}

func TestCrossProcessControlCancelsProvider(t *testing.T) {
	for _, mode := range []string{"pause", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			provider := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
			a, s := newTestAgent(t, provider)
			id := createTestRun(t, a)
			done := make(chan error, 1)
			go func() { _, err := a.Run(context.Background(), id); done <- err }()
			<-provider.entered
			output, err := fixtureCommand(mode, s.root, id, "").CombinedOutput()
			if err != nil {
				close(provider.release)
				t.Fatalf("controller: %v %s", err, output)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				close(provider.release)
				t.Fatal("cross-process control ignored")
			}
			snap, _ := s.Load(id)
			expected := StatusPaused
			if mode == "cancel" {
				expected = StatusCancelled
			}
			if snap.Run.Status != expected {
				t.Fatalf("status=%s", snap.Run.Status)
			}
		})
	}
}
