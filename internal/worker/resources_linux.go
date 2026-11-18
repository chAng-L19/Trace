//go:build linux

package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// This is an accidental-resource-exhaustion boundary, not a privilege sandbox.
// The administrator provisions sibling controller/exec domains under an empty,
// finite parent. Both siblings have finite memory+pids and zero swap; their
// combined caps leave parent headroom. This is not host-wide reservation.
// No fallback starts the command outside that boundary.
const (
	parentMemoryHeadroom    = int64(256 << 20)
	parentPIDsHeadroom      = int64(64)
	aggregateMemoryHeadroom = int64(64 << 20)
	aggregatePIDsHeadroom   = int64(8)
)

type resourceDomain struct {
	owner     string
	path      string
	fd        *os.File
	processes *processResourceDomain
}

func readLimit(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s requires a finite positive limit", path)
	}
	return n, nil
}

func readResourceCounter(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s requires a nonnegative counter", path)
	}
	return n, nil
}

// Retired cgroups can leave charged cache behind. Conservatively count all
// current aggregate usage in addition to live maximum reservations: this may
// reject busy capacity early, but never sells existing charges as free space.
func aggregateAvailable(root, name string, cap, margin int64) (int64, error) {
	if cap <= margin {
		return 0, fmt.Errorf("exec aggregate %s lacks headroom", name)
	}
	available := cap - margin
	if name == "memory.max" {
		current, err := readResourceCounter(filepath.Join(root, "memory.current"))
		if err != nil {
			return 0, err
		}
		if current >= available {
			return 0, fmt.Errorf("exec resource capacity unavailable: existing aggregate memory charges consume headroom")
		}
		available -= current
	}
	return available, nil
}

// ResourceRootIdentity binds durable ownership to this particular cgroup, not
// merely its pathname. Recreated directories are different resource roots.
func ResourceRootIdentity(root string) (string, error) {
	if root == ProcessGroupResourceRoot {
		return processResourceRootIdentity()
	}
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("resource root requires an absolute cgroup-v2 path")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var fs syscall.Statfs_t
	if err := syscall.Fstatfs(int(file.Fd()), &fs); err != nil {
		return "", err
	}
	if fs.Type != 0x63677270 {
		return "", fmt.Errorf("resource root is not cgroup v2")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
		return "", err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return "", fmt.Errorf("resource root is not a directory")
	}
	return fmt.Sprintf("%x:%x", stat.Dev, stat.Ino), nil
}

func requireCgroupValue(path, expected string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) != expected {
		return fmt.Errorf("%s requires %q", path, expected)
	}
	return nil
}

func requireResourceControllers(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		return err
	}
	fields := " " + strings.Join(strings.Fields(string(raw)), " ") + " "
	if !strings.Contains(fields, " memory ") || !strings.Contains(fields, " pids ") {
		return fmt.Errorf("%s requires delegated memory and pids controllers", root)
	}
	return nil
}

// validateResourceLayout checks the actual calling process, not an environment
// assertion about its placement. A manually provisioned unsafe layout fails
// before the target is created. Concurrent operator reconfiguration is outside
// this accidental-exhaustion boundary; operators must stop admission first.
func validateResourceLayout(root string) error {
	parent := filepath.Dir(root)
	controller := filepath.Join(parent, "controller")
	if filepath.Base(root) != "exec" {
		return fmt.Errorf("resource aggregate must be the exec sibling of controller")
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	current := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "0::/") {
			current = filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::/"))
			break
		}
	}
	actual, err := filepath.EvalSymlinks(current)
	if err != nil || actual != controller {
		return fmt.Errorf("exec controller is not resident in the separate bounded controller sibling")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "controller" && entry.Name() != "exec" {
			return fmt.Errorf("resource parent has an unaccounted sibling: %s", entry.Name())
		}
	}
	for _, path := range []string{parent, root} {
		if err := requireCgroupValue(filepath.Join(path, "cgroup.procs"), ""); err != nil {
			return err
		}
		if err := requireResourceControllers(path); err != nil {
			return err
		}
	}
	for _, path := range []string{parent, controller, root} {
		if err := requireCgroupValue(filepath.Join(path, "memory.swap.max"), "0"); err != nil {
			return err
		}
	}
	return validateResourceCaps(parent, controller, root)
}

func validateResourceCaps(parent, controller, root string) error {
	for _, item := range []struct {
		name   string
		margin int64
	}{
		{"memory.max", parentMemoryHeadroom}, {"pids.max", parentPIDsHeadroom},
	} {
		cap, err := readLimit(filepath.Join(parent, item.name))
		if err != nil {
			return err
		}
		controlCap, err := readLimit(filepath.Join(controller, item.name))
		if err != nil {
			return err
		}
		execCap, err := readLimit(filepath.Join(root, item.name))
		if err != nil {
			return err
		}
		// Subtraction avoids overflow even for pathological operator caps.
		if cap <= item.margin || controlCap > cap-item.margin || execCap > cap-item.margin-controlCap {
			return fmt.Errorf("resource parent %s lacks controller + exec + %d headroom", item.name, item.margin)
		}
	}
	return nil
}

func prepareResourceDomain(cmd *exec.Cmd, request Request) (_ *resourceDomain, err error) {
	limits := request.Resources
	if limits == nil || limits.MemoryBytes <= 0 || limits.PIDs <= 0 || limits.OutputBytes <= 0 {
		return nil, fmt.Errorf("invalid exec resource limits")
	}
	switch limits.Mode {
	case ResourceModeProcessGroup:
		return prepareProcessResourceDomain(cmd, request)
	case "", ResourceModeCgroup:
	default:
		return nil, fmt.Errorf("invalid exec resource mode %q", limits.Mode)
	}
	if limits.CgroupRoot == "" || !filepath.IsAbs(limits.CgroupRoot) {
		return nil, fmt.Errorf("exec isolation unavailable: configure TRACE_EXEC_CGROUP_ROOT to an empty delegated cgroup-v2 parent with finite memory.max/pids.max; target not started")
	}
	root, err := filepath.EvalSymlinks(limits.CgroupRoot)
	if err != nil {
		return nil, fmt.Errorf("exec isolation parent: %w", err)
	}
	var stat syscall.Statfs_t
	if err = syscall.Statfs(root, &stat); err != nil {
		return nil, err
	}
	if stat.Type != 0x63677270 {
		return nil, fmt.Errorf("exec isolation parent is not cgroup v2")
	}
	if err := validateResourceLayout(root); err != nil {
		return nil, fmt.Errorf("exec isolation layout: %w", err)
	}
	// Serialize reservations across Trace processes too. Flock on the existing
	// cgroup directory introduces no regular files into the cgroup filesystem.
	lock, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("exec resource admission busy: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, item := range []struct {
		name   string
		limit  int64
		margin int64
	}{{"memory.max", limits.MemoryBytes, aggregateMemoryHeadroom}, {"pids.max", limits.PIDs, aggregatePIDsHeadroom}} {
		cap, readErr := readLimit(filepath.Join(root, item.name))
		if readErr != nil {
			return nil, readErr
		}
		if cap <= item.margin || item.limit > cap-item.margin {
			return nil, fmt.Errorf("exec %s exceeds aggregate parent limit after headroom", item.name)
		}
		available, readErr := aggregateAvailable(root, item.name, cap, item.margin)
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			reserved, readErr := readLimit(filepath.Join(root, entry.Name(), item.name))
			if readErr != nil {
				return nil, fmt.Errorf("unknown resource reservation: %w", readErr)
			}
			if reserved > available {
				return nil, fmt.Errorf("exec resource capacity unavailable: aggregate %s reservations and existing charges exceed admission cap", item.name)
			}
			available -= reserved
		}
		if item.limit > available {
			return nil, fmt.Errorf("exec resource capacity unavailable: %s; existing owners retained", item.name)
		}
	}
	// An empty dedicated aggregate prevents placing the controller in the same
	// resource domain as its commands. Host capacity remains operator-owned.
	raw, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(raw)) != "" {
		return nil, fmt.Errorf("exec isolation parent must be empty and separate from controller processes")
	}
	if request.RunID == "" || request.RequestID == "" || request.CallID == "" {
		return nil, fmt.Errorf("isolated exec requires run_id, request_id and call_id ownership")
	}
	owner := ResourceOwner(request.RunID, request.RequestID, request.CallID)
	domain := &resourceDomain{owner: owner, path: filepath.Join(root, owner)}
	if err = os.Mkdir(domain.path, 0700); err != nil {
		return nil, fmt.Errorf("create owned exec cgroup (existing owners require reconciliation, never replay): %w", err)
	}
	defer func() {
		if err != nil {
			if cleanupErr := errors.Join(domain.close(), os.Remove(domain.path)); cleanupErr != nil {
				err = errors.Join(err, &resourceSetupCleanupError{err: cleanupErr})
			}
		}
	}()
	for _, item := range []struct{ name, value string }{
		{"memory.max", strconv.FormatInt(limits.MemoryBytes, 10)},
		{"memory.swap.max", "0"},
		{"memory.oom.group", "1"},
		{"pids.max", strconv.FormatInt(limits.PIDs, 10)},
	} {
		path := filepath.Join(domain.path, item.name)
		if err = os.WriteFile(path, []byte(item.value), 0600); err != nil {
			return nil, fmt.Errorf("set exec %s before launch: %w", item.name, err)
		}
		var got []byte
		got, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(got)) != item.value {
			return nil, fmt.Errorf("exec %s limit readback mismatch", item.name)
		}
	}
	// Preflight kill availability without writing it or spawning a target.
	killFile, openErr := os.OpenFile(filepath.Join(domain.path, "cgroup.kill"), os.O_WRONLY, 0)
	if openErr != nil {
		return nil, fmt.Errorf("exec cgroup.kill unavailable: %w", openErr)
	}
	if err = killFile.Close(); err != nil {
		return nil, err
	}
	domain.fd, err = os.Open(domain.path)
	if err != nil {
		return nil, err
	}
	// clone3(CLONE_INTO_CGROUP) is atomic: target code never executes before
	// admission. Kernel/seccomp/delegation failures make cmd.Start fail closed.
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(domain.fd.Fd())
	cmd.Cancel = domain.kill
	return domain, nil
}

func (d *resourceDomain) close() error {
	if d.fd == nil {
		return nil
	}
	err := d.fd.Close()
	d.fd = nil
	return err
}
func (d *resourceDomain) kill() error {
	if d.processes != nil {
		return d.processes.kill()
	}
	return os.WriteFile(filepath.Join(d.path, "cgroup.kill"), []byte("1"), 0600)
}

// ReconcileResourceOwner cleans only the persisted run/call owner, never
// restarts its command. Callers keep the interrupted action uncertain even
// after cleanup succeeds: process absence is not external side-effect proof.
func ReconcileResourceOwner(root, runID, requestID, callID string) error {
	if root == ProcessGroupResourceRoot {
		return reconcileProcessResourceOwner(runID, requestID, callID)
	}
	if root == "" || !filepath.IsAbs(root) || runID == "" || requestID == "" || callID == "" {
		return fmt.Errorf("resource reconciliation requires configured root and persisted run/call identity")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(resolved, &stat); err != nil {
		return err
	}
	if stat.Type != 0x63677270 {
		return fmt.Errorf("resource reconciliation root is not cgroup v2")
	}
	owner := ResourceOwner(runID, requestID, callID)
	path := filepath.Join(resolved, owner)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid owned resource domain")
	}
	d := &resourceDomain{owner: owner, path: path}
	_, err = d.finish()
	return err
}

func (d *resourceDomain) finish() (string, error) {
	if d.processes != nil {
		return "", d.processes.finish()
	}
	var events []string
	var errs []error
	for _, name := range []string{"memory.events", "pids.events"} {
		raw, err := os.ReadFile(filepath.Join(d.path, name))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		for _, line := range lines {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			if (name == "pids.events" && f[0] == "max" || name == "memory.events" && (f[0] == "oom" || f[0] == "oom_kill")) && f[1] != "0" {
				events = append(events, name+":"+line)
			}
		}
	}
	if err := d.kill(); err != nil {
		errs = append(errs, fmt.Errorf("kill owned exec cgroup: %w", err))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(d.path, "cgroup.events"))
		if err != nil {
			errs = append(errs, err)
			break
		}
		if strings.Contains("\n"+string(raw), "\npopulated 0\n") {
			break
		}
		if time.Now().After(deadline) {
			errs = append(errs, fmt.Errorf("owned exec cgroup %s remains populated", d.owner))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	errs = append(errs, d.close())
	// Remove only this exact leaf. Never recursively delete an aggregate or
	// another owner's descendants; unexpected subgroups require reconciliation.
	if err := os.Remove(d.path); err != nil {
		errs = append(errs, err)
	}
	return strings.Join(events, "; "), errors.Join(errs...)
}
