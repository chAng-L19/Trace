//go:build linux

package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func isolatedRequest(t *testing.T, mode string) Request {
	t.Helper()
	root := os.Getenv("TRACE_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("requires an isolated delegated cgroup via TRACE_TEST_CGROUP_ROOT")
	}
	return Request{RunID: t.Name(), RequestID: "request-1", CallID: mode, Command: []string{os.Args[0], "-test.run=^TestResourceFixture$", "--", mode},
		Resources: &ResourceLimits{CgroupRoot: root, MemoryBytes: 128 << 20, PIDs: 48, OutputBytes: 64 << 10},
		Env:       []string{"GORACE=atexit_sleep_ms=0"}, CaptureDir: t.TempDir()}
}

func cgroupCounter(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || n < 0 {
		t.Fatalf("invalid cgroup counter %s: %q %v", path, raw, err)
	}
	return n
}

func memoryOOMCounters(t *testing.T, root string) map[string]int64 {
	t.Helper()
	// Local counters distinguish a protected parent's OOM from a child's OOM.
	raw, err := os.ReadFile(filepath.Join(root, "memory.events.local"))
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]int64)
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			n, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			result[f[0]] = n
		}
	}
	for _, key := range []string{"oom", "oom_kill"} {
		if _, ok := result[key]; !ok {
			t.Fatalf("missing %s in %s/memory.events.local", key, root)
		}
	}
	return result
}

// Fixtures are explicitly bounded. Never use the production recursive command.
func TestResourceFixture(t *testing.T) {
	var mode string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	if mode == "" {
		return
	}
	switch mode {
	case "output":
		for i := 0; i < 64; i++ {
			fmt.Fprint(os.Stdout, strings.Repeat("o", 4096))
			fmt.Fprint(os.Stderr, strings.Repeat("e", 4096))
		}
	case "memory":
		blocks := make([][]byte, 0, 48)
		for i := 0; i < 48; i++ {
			b := make([]byte, 8<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1
			}
			blocks = append(blocks, b)
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Println(len(blocks))
		runtime.KeepAlive(blocks)
	case "pids":
		children := make([]*exec.Cmd, 0, 80)
		for i := 0; i < 80; i++ {
			c := exec.CommandContext(context.Background(), "/bin/sleep", "20")
			if err := c.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				break
			}
			children = append(children, c)
		}
		fmt.Println(len(children))
		// Root exits with all descendants redirected; the owned domain must reap them.
	case "block":
		time.Sleep(30 * time.Second)
	case "ok":
		fmt.Print("survived")
	case "crash":
		crashFixture()
	default:
		os.Exit(4)
	}
	os.Exit(0)
}

func TestExecResourceIsolation(t *testing.T) {
	request := isolatedRequest(t, "preflight")
	if err := validateResourceLayout(request.Resources.CgroupRoot); err != nil {
		t.Fatal(err)
	}
	controller := filepath.Join(filepath.Dir(request.Resources.CgroupRoot), "controller")
	cap, err := readLimit(filepath.Join(controller, "memory.max"))
	if err != nil {
		t.Fatal(err)
	}
	before := cgroupCounter(t, filepath.Join(controller, "memory.current"))
	const pressureBytes = 96 << 20
	if cap-before < pressureBytes+(64<<20) {
		t.Fatalf("controller profile lacks room for bounded %d-byte pressure plus 64MiB test overhead: current=%d cap=%d", pressureBytes, before, cap)
	}
	// Bounded occupancy in the actual controller while leaf limits are hit.
	// This is not a recursive or open-ended host stress test.
	pressure := make([]byte, pressureBytes)
	for i := 0; i < len(pressure); i += 4096 {
		pressure[i] = 1
	}
	t.Logf("controller bounded pressure: before=%d current=%d cap=%d", before, cgroupCounter(t, filepath.Join(controller, "memory.current")), cap)
	defer runtime.KeepAlive(pressure)
	protected := []string{filepath.Dir(controller), controller, request.Resources.CgroupRoot}
	baseline := make(map[string]map[string]int64)
	for _, path := range protected {
		baseline[path] = memoryOOMCounters(t, path)
	}
	defer func() {
		for _, path := range protected {
			after := memoryOOMCounters(t, path)
			for _, key := range []string{"oom", "oom_kill"} {
				if after[key] != baseline[path][key] {
					t.Errorf("protected domain %s had %s: before=%d after=%d", path, key, baseline[path][key], after[key])
				}
			}
		}
	}()
	for _, mode := range []string{"output", "memory", "pids", "crash"} {
		t.Run(mode, func(t *testing.T) {
			sibling := isolatedRequest(t, "block")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			siblingDone := make(chan Result, 1)
			go func() { r, _ := Run(ctx, sibling); siblingDone <- r }()
			// Observe admission rather than sleeping and assuming the sibling started.
			path := filepath.Join(sibling.Resources.CgroupRoot, ResourceOwner(sibling.RunID, sibling.RequestID, sibling.CallID), "cgroup.events")
			deadline := time.Now().Add(5 * time.Second)
			for {
				raw, _ := os.ReadFile(path)
				if strings.Contains(string(raw), "populated 1") {
					break
				}
				select {
				case r := <-siblingDone:
					t.Fatalf("sibling failed before admission: %+v", r)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("sibling failed to start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			request := isolatedRequest(t, mode)
			runCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			result, err := Run(runCtx, request)
			if err == nil || !result.OutcomeUncertain || result.PID == 0 {
				t.Fatalf("expected isolated interruption: %+v %v", result, err)
			}
			if mode != "crash" && result.ResourceLimit == "" {
				t.Fatalf("missing resource reason: %+v", result)
			}
			if result.CleanupError != "" {
				t.Fatal(result.CleanupError)
			}
			if _, err := os.Stat(filepath.Join(request.Resources.CgroupRoot, result.ResourceOwner)); !os.IsNotExist(err) {
				t.Fatalf("owned domain remains: %v", err)
			}
			if mode == "output" {
				var size int64
				for _, p := range []string{result.StdoutPath, result.StderrPath} {
					s, e := os.Stat(p)
					if e != nil {
						t.Fatal(e)
					}
					size += s.Size()
				}
				if size > request.Resources.OutputBytes {
					t.Fatalf("capture exceeded combined output cap: %d", size)
				}
			}
			select {
			case r := <-siblingDone:
				t.Fatalf("sibling died: %+v", r)
			default:
			}
			if raw, err := os.ReadFile(path); err != nil || !strings.Contains(string(raw), "populated 1") {
				t.Fatalf("sibling no longer populated: %s %v", raw, err)
			}
			control := isolatedRequest(t, "ok")
			r, e := Run(context.Background(), control)
			if e != nil || r.Status != StatusCompleted {
				t.Fatalf("controller/other owner affected: %+v %v", r, e)
			}
			cancel()
			select {
			case r := <-siblingDone:
				if r.CleanupError != "" {
					t.Fatal(r.CleanupError)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancel cleanup blocked")
			}
		})
	}
}

func TestExecIsolationUnavailableDoesNotStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	for _, root := range []string{"", t.TempDir()} {
		result, err := Run(context.Background(), Request{RunID: t.Name(), RequestID: "request-1", CallID: root, Command: []string{"/bin/sh", "-c", "printf started > \"$1\"", "sh", marker}, Resources: &ResourceLimits{CgroupRoot: root, MemoryBytes: 128 << 20, PIDs: 32, OutputBytes: 1024}})
		if err == nil || result.PID != 0 || result.Status != StatusNotStarted {
			t.Fatalf("isolation fell through: %+v %v", result, err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("target executed: %v", err)
		}
	}
}

func TestExecMissingControllerDoesNotStart(t *testing.T) {
	request := isolatedRequest(t, "ok")
	controlFile := filepath.Join(request.Resources.CgroupRoot, "cgroup.subtree_control")
	if err := os.WriteFile(controlFile, []byte("-pids"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(controlFile, []byte("+pids"), 0600); err != nil {
			t.Error(err)
		}
		if err := requireResourceControllers(request.Resources.CgroupRoot); err != nil {
			t.Error(err)
		}
	})
	result, err := Run(context.Background(), request)
	if err == nil || result.PID != 0 || result.Status != StatusNotStarted || !strings.Contains(err.Error(), "requires delegated memory and pids") {
		t.Fatalf("missing controller launched target: %+v %v", result, err)
	}
}

func TestExecReconcileDoesNotReplayOrTouchSibling(t *testing.T) {
	request := isolatedRequest(t, "block")
	cmd := exec.CommandContext(context.Background(), "/bin/sleep", "30")
	ConfigureCommand(cmd)
	domain, err := prepareResourceDomain(cmd, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		domain.finish()
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer func() {
		domain.close()
		ReconcileResourceOwner(request.Resources.CgroupRoot, request.RunID, request.RequestID, request.CallID)
	}()
	sibling := isolatedRequest(t, "sibling")
	siblingCmd := exec.CommandContext(context.Background(), "/bin/sleep", "30")
	ConfigureCommand(siblingCmd)
	siblingDomain, err := prepareResourceDomain(siblingCmd, sibling)
	if err != nil {
		t.Fatal(err)
	}
	if err := siblingCmd.Start(); err != nil {
		siblingDomain.finish()
		t.Fatal(err)
	}
	defer siblingCmd.Wait()
	defer siblingDomain.finish()
	// Simulate the persisted owner surviving loss of its controlling Run.
	retry, e := Run(context.Background(), request)
	if e == nil || retry.PID != 0 {
		t.Fatalf("live owner replayed: %+v %v", retry, e)
	}
	if err := ReconcileResourceOwner(request.Resources.CgroupRoot, request.RunID, request.RequestID, request.CallID); err != nil {
		t.Fatal(err)
	}
	if !processAlive(siblingCmd.Process.Pid) {
		t.Fatal("reconciliation killed another owner")
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatal("reconciled target remains alive")
	}
	if err := ReconcileResourceOwner(request.Resources.CgroupRoot, request.RunID, request.RequestID, request.CallID); err != nil {
		t.Fatalf("reconciliation not idempotent: %v", err)
	}
}

func TestExecAggregateReservationsFailBeforeLaunch(t *testing.T) {
	request := isolatedRequest(t, "ok")
	parentCap, err := readLimit(filepath.Join(request.Resources.CgroupRoot, "memory.max"))
	if err != nil {
		t.Fatal(err)
	}
	request.Resources.MemoryBytes = parentCap - aggregateMemoryHeadroom - cgroupCounter(t, filepath.Join(request.Resources.CgroupRoot, "memory.current")) - (16 << 20)
	if request.Resources.MemoryBytes <= 0 {
		t.Fatal("aggregate profile too small for reservation fixture")
	}
	cmd := exec.CommandContext(context.Background(), "/bin/true")
	ConfigureCommand(cmd)
	held, err := prepareResourceDomain(cmd, request)
	if err != nil {
		t.Fatal(err)
	}
	defer held.finish()
	other := isolatedRequest(t, "other")
	other.Command = []string{"/bin/true"}
	result, err := Run(context.Background(), other)
	if err == nil || result.PID != 0 || !strings.Contains(err.Error(), "capacity unavailable") {
		t.Fatalf("aggregate admission escaped: %+v %v", result, err)
	}
}

func TestExecParentCapsRequireHeadroom(t *testing.T) {
	for _, tc := range []struct {
		name, memory, pids string
		wantError          bool
	}{
		{"exact_margin", "2952790016", "576", false},
		{"memory_one_byte_short", "2952790015", "576", true},
		{"pids_one_task_short", "2952790016", "575", true},
		{"old_2g_profile", "2147483648", "768", true},
		{"unlimited_parent", "max", "768", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			controller, root := filepath.Join(parent, "controller"), filepath.Join(parent, "exec")
			for _, path := range []string{controller, root} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, item := range []struct{ path, value string }{
				{filepath.Join(parent, "memory.max"), tc.memory}, {filepath.Join(parent, "pids.max"), tc.pids},
				{filepath.Join(controller, "memory.max"), "1073741824"}, {filepath.Join(controller, "pids.max"), "128"},
				{filepath.Join(root, "memory.max"), "1610612736"}, {filepath.Join(root, "pids.max"), "384"},
			} {
				if err := os.WriteFile(item.path, []byte(item.value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := validateResourceCaps(parent, controller, root); (err != nil) != tc.wantError {
				t.Fatalf("unexpected caps validation: %v", err)
			}
		})
	}
}

func TestExecAggregateCurrentChargesReduceAdmission(t *testing.T) {
	root := t.TempDir()
	const cap = int64(768 << 20)
	for _, current := range []int64{0, 96 << 20, cap - aggregateMemoryHeadroom, cap} {
		if err := os.WriteFile(filepath.Join(root, "memory.current"), []byte(strconv.FormatInt(current, 10)), 0600); err != nil {
			t.Fatal(err)
		}
		available, err := aggregateAvailable(root, "memory.max", cap, aggregateMemoryHeadroom)
		if current >= cap-aggregateMemoryHeadroom {
			if err == nil {
				t.Fatalf("existing charges admitted: current=%d available=%d", current, available)
			}
		} else if err != nil || available != cap-aggregateMemoryHeadroom-current {
			t.Fatalf("existing charge accounting: current=%d available=%d err=%v", current, available, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "memory.current"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := aggregateAvailable(root, "memory.max", cap, aggregateMemoryHeadroom); err == nil {
		t.Fatal("invalid charge counter accepted")
	}
}

func TestResourceRootIdentityRejectsNonCgroup(t *testing.T) {
	for _, root := range []string{"", "relative", t.TempDir()} {
		if identity, err := ResourceRootIdentity(root); err == nil || identity != "" {
			t.Fatalf("non-cgroup identity accepted: %q %v", identity, err)
		}
	}
}

func TestResourceRootIdentityChangesOnRecreation(t *testing.T) {
	request := isolatedRequest(t, "identity")
	root := filepath.Join(request.Resources.CgroupRoot, "identity-fixture")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(root)
	first, err := ResourceRootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ResourceRootIdentity(root)
	if err != nil || again != first {
		t.Fatalf("identity not stable: %q %q %v", first, again, err)
	}
	if _, err := ResourceRootIdentity(filepath.Join(root, "cgroup.events")); err == nil {
		t.Fatal("control file accepted as root")
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	second, err := ResourceRootIdentity(root)
	if err != nil || second == first {
		t.Fatalf("recreated root retained identity: %q %q %v", first, second, err)
	}
}

func TestExecWrapperRejectsSmallUnitBeforeLaunch(t *testing.T) {
	request := isolatedRequest(t, "wrapper")
	controller := filepath.Join(filepath.Dir(request.Resources.CgroupRoot), "controller")
	cap, err := readLimit(filepath.Join(controller, "memory.max"))
	if err != nil {
		t.Fatal(err)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source path unavailable")
	}
	script := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "exec-cgroup-isolated.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("wrapper source required: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "target-started")
	// Execute the wrapper in this finite controller as a too-small candidate
	// unit. Cap rejection must happen before membership changes or mkdir.
	cmd := exec.Command("/bin/sh", script, "/bin/sh", "-c", "printf started > \"$1\"", "sh", marker)
	cmd.Env = append(os.Environ(),
		"TRACE_CONTROLLER_MEMORY_BYTES="+strconv.FormatInt(cap, 10),
		"TRACE_EXEC_AGGREGATE_MEMORY_BYTES=805306368",
		"TRACE_CONTROLLER_PIDS=128", "TRACE_EXEC_AGGREGATE_PIDS=384")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "MemoryMax cannot contain controller + exec + 256MiB headroom") {
		t.Fatalf("wrapper did not reject small memory before setup: %s %v", out, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("rejected wrapper started target: %v", err)
	}
	for _, name := range []string{"controller", "exec"} {
		if _, err := os.Stat(filepath.Join(controller, name)); !os.IsNotExist(err) {
			t.Fatalf("small-unit guard mutated layout: %s %v", name, err)
		}
	}
}
