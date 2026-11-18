package worker

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"sync"
)

// ResourceOwner is stable across crashes and must be persisted before Run.
func ResourceOwner(runID, requestID, callID string) string {
	hash := sha256.Sum256([]byte(runID + "\x00" + requestID + "\x00" + callID))
	return fmt.Sprintf("trace-exec-%x", hash[:16])
}

const (
	ResourceModeCgroup       = "cgroup"
	ResourceModeProcessGroup = "process-group"
	// ProcessGroupResourceRoot is a backend tag, never a filesystem pathname.
	// Persist it before execution so recovery never reinterprets a cgroup action.
	ProcessGroupResourceRoot = "process-group://trace-exec/v1"
)

// ResourceLimits applies only to explicitly managed exec calls, not protocol
// hosts (pi), local adapters, or persistent browser owners. No solving deadline.
type ResourceLimits struct {
	Mode        string `json:"mode,omitempty"`
	CgroupRoot  string `json:"-"`
	MemoryBytes int64  `json:"memory_bytes"` // Enforced only by cgroup mode.
	PIDs        int64  `json:"pids"`         // Enforced only by cgroup mode.
	OutputBytes int64  `json:"output_bytes"`
}

func execResourceMode() (string, error) {
	switch mode := os.Getenv("TRACE_EXEC_RESOURCE_MODE"); mode {
	case "", ResourceModeCgroup:
		return ResourceModeCgroup, nil
	case ResourceModeProcessGroup:
		return mode, nil
	default:
		return "", fmt.Errorf("TRACE_EXEC_RESOURCE_MODE must be cgroup or process-group")
	}
}

// ExecResourceRoot returns the root to persist with ResourceRootIdentity before
// starting an action. Empty mode preserves the historical strict cgroup backend.
func ExecResourceRoot() (string, error) {
	mode, err := execResourceMode()
	if err != nil {
		return "", err
	}
	if mode == ResourceModeProcessGroup {
		return ProcessGroupResourceRoot, nil
	}
	return os.Getenv("TRACE_EXEC_CGROUP_ROOT"), nil
}

type resourceSetupCleanupError struct{ err error }

func (e *resourceSetupCleanupError) Error() string {
	return "exec resource setup cleanup: " + e.err.Error()
}
func (e *resourceSetupCleanupError) Unwrap() error { return e.err }

// ExecResourceLimits is fail-closed: missing delegation is diagnosed by Run
// before starting the target. Environment belongs to the service operator.
func ExecResourceLimits() (*ResourceLimits, error) {
	mode, err := execResourceMode()
	if err != nil {
		return nil, err
	}
	limits := &ResourceLimits{Mode: mode, CgroupRoot: os.Getenv("TRACE_EXEC_CGROUP_ROOT"), MemoryBytes: 256 << 20, PIDs: 64, OutputBytes: 8 << 20}
	for _, item := range []struct {
		name  string
		value *int64
	}{
		{"TRACE_EXEC_MEMORY_BYTES", &limits.MemoryBytes},
		{"TRACE_EXEC_PIDS", &limits.PIDs},
		{"TRACE_EXEC_OUTPUT_BYTES", &limits.OutputBytes},
	} {
		if value, ok := os.LookupEnv(item.name); ok {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%s must be a positive integer", item.name)
			}
			*item.value = n
		}
	}
	return limits, nil
}

type outputBudget struct {
	mu        sync.Mutex
	remaining int64
	exceeded  bool
	cancel    func()
}

func (b *outputBudget) take(n int) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if int64(n) <= b.remaining {
		b.remaining -= int64(n)
		return n, false
	}
	allowed := int(b.remaining)
	b.remaining = 0
	b.exceeded = true
	b.cancel()
	return allowed, true
}

func (b *outputBudget) hit() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exceeded
}
