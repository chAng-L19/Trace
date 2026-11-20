package worker

import (
	"bytes"
	"sync"
	"testing"
)

func TestSharedOutputBudget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	budget := &outputBudget{remaining: 17, cancel: func() {}}
	writers := []*outputWriter{{buffer: &stdout, budget: budget}, {buffer: &stderr, budget: budget}}
	var wg sync.WaitGroup
	for _, w := range writers {
		wg.Add(1)
		go func(w *outputWriter) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if n, err := w.Write([]byte("1234567890")); n != 10 || err != nil {
					t.Errorf("drain: %d %v", n, err)
				}
			}
		}(w)
	}
	wg.Wait()
	if stdout.Len()+stderr.Len() != 17 || !budget.hit() {
		t.Fatalf("combined cap failed: %d/%d", stdout.Len(), stderr.Len())
	}
}

func TestExecResourceConfiguration(t *testing.T) {
	if ResourceOwner("r", "request-1", "c") == ResourceOwner("r", "request-2", "c") {
		t.Fatal("repeated call ID reused owner")
	}
	t.Setenv("TRACE_EXEC_MEMORY_BYTES", "invalid")
	if _, err := ExecResourceLimits(); err == nil {
		t.Fatal("invalid memory accepted")
	}
	if ResourceOwner("ab", "request", "c") == ResourceOwner("a", "request", "bc") {
		t.Fatal("owner separator missing")
	}
	const wantOwner = "trace-exec-ca6015aafee9fd5800aee40439bbbc41"
	if owner := ResourceOwner("r", "request", "c"); owner != wantOwner {
		t.Fatalf("persisted owner changed: got %q, want %q", owner, wantOwner)
	}
}

func TestExecResourceModeConfiguration(t *testing.T) {
	t.Setenv("TRACE_EXEC_CGROUP_ROOT", "/strict/exec")
	for _, name := range []string{"TRACE_EXEC_MEMORY_BYTES", "TRACE_EXEC_PIDS", "TRACE_EXEC_OUTPUT_BYTES"} {
		t.Setenv(name, "1024")
	}
	for _, mode := range []string{"", ResourceModeCgroup, ResourceModeProcessGroup, "auto", "CGROUP", "process-group "} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TRACE_EXEC_RESOURCE_MODE", mode)
			root, rootErr := ExecResourceRoot()
			limits, limitErr := ExecResourceLimits()
			switch mode {
			case "", ResourceModeCgroup:
				if rootErr != nil || limitErr != nil || root != "/strict/exec" || limits.Mode != ResourceModeCgroup {
					t.Fatalf("strict default changed: %s %+v %v %v", root, limits, rootErr, limitErr)
				}
			case ResourceModeProcessGroup:
				if rootErr != nil || limitErr != nil || root != ProcessGroupResourceRoot || limits.Mode != mode || limits.OutputBytes != 1024 {
					t.Fatalf("explicit process mode lost budget/identity: %s %+v %v %v", root, limits, rootErr, limitErr)
				}
			default:
				if rootErr == nil || limitErr == nil || root != "" || limits != nil {
					t.Fatalf("invalid mode accepted: %s %+v %v %v", root, limits, rootErr, limitErr)
				}
			}
		})
	}
}
