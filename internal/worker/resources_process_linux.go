//go:build linux

package worker

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	processOwnerEnv  = "TRACE_EXEC_PROCESS_OWNER"
	processAnchorEnv = "TRACE_EXEC_PROCESS_GROUP_ANCHOR"
)

// This explicitly selected compatibility backend has no per-call memory/PID
// containment. Setpgid still separates normal command groups, while inherited
// ownership tags cover observable setsid/double-fork descendants after a crash.
// Commands that remove the tag and detach, change UID, or hide from /proc are
// outside that cleanup contract. An empty scan proves only tagged-owner absence;
// it never establishes the business outcome of an interrupted action.
type processResourceDomain struct {
	marker string
	owner  string
	anchor *exec.Cmd
}

type ownedProcess struct {
	pid         int
	start       string
	group       int
	anchor      bool
	groupAnchor *ownedProcess
}

func processResourceRootIdentity() (string, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("process-group boot identity unavailable: %w", err)
	}
	if len(strings.TrimSpace(string(boot))) != 36 {
		return "", fmt.Errorf("invalid process-group boot identity")
	}
	var namespace unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &namespace); err != nil {
		return "", err
	}
	// pidfd_open interprets IDs in the caller's PID namespace. Reject a /proc
	// mount exposing a different namespace rather than signal an unrelated PID.
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return "", err
	}
	pid, err := strconv.Atoi(strings.SplitN(string(raw), " ", 2)[0])
	if err != nil || pid != os.Getpid() {
		return "", fmt.Errorf("process-group requires /proc in the current PID namespace")
	}
	identity := fmt.Sprintf("%s:%x:%x:%d", strings.TrimSpace(string(boot)), namespace.Dev, namespace.Ino, os.Geteuid())
	hash := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("process-group-v1:%x", hash), nil
}

func newProcessResourceDomain(runID, requestID, callID string) (*processResourceDomain, error) {
	if runID == "" || requestID == "" || callID == "" {
		return nil, fmt.Errorf("process-group exec requires persisted run_id, request_id and call_id")
	}
	identity, err := processResourceRootIdentity()
	if err != nil {
		return nil, err
	}
	owner := ResourceOwner(runID, requestID, callID)
	return &processResourceDomain{owner: owner, marker: identity + ":" + owner}, nil
}

func prepareProcessResourceDomain(cmd *exec.Cmd, request Request) (*resourceDomain, error) {
	d, err := newProcessResourceDomain(request.RunID, request.RequestID, request.CallID)
	if err != nil {
		return nil, err
	}
	// No PID-only fallback: a blocked syscall is diagnosed before target start.
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return nil, fmt.Errorf("process-group requires pidfd_open: %w", err)
	}
	err = unix.PidfdSendSignal(fd, 0, nil, 0)
	err = errors.Join(err, unix.Close(fd))
	if err != nil {
		return nil, fmt.Errorf("process-group requires pidfd_send_signal: %w", err)
	}
	existing, err := d.processes()
	if err != nil {
		return nil, err
	}
	if len(existing) != 0 {
		return nil, &resourceSetupCleanupError{err: fmt.Errorf("process-group owner already has live processes; reconciliation required, never replay")}
	}
	// Keep the original group number reserved until cleanup, even when the
	// target exits or replaces its environment. This fixed helper runs no user
	// command; its inherited marker permits recovery after controller death.
	anchor := exec.Command("/bin/sleep", "infinity")
	anchor.Env = append(os.Environ(), processOwnerEnv+"="+d.marker, processAnchorEnv+"="+d.marker)
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := anchor.Start(); err != nil {
		return nil, fmt.Errorf("start process-group anchor: %w", err)
	}
	d.anchor = anchor
	cmd.SysProcAttr.Pgid = anchor.Process.Pid
	// exec.Cmd deduplicates keys using the last value, so request.Env cannot
	// replace the owner marker. The tag is ownership metadata, not a secret.
	cmd.Env = append(cmd.Env, processOwnerEnv+"="+d.marker, processAnchorEnv+"=")
	cmd.Cancel = d.kill
	return &resourceDomain{owner: d.owner, processes: d}, nil
}

func reconcileProcessResourceOwner(runID, requestID, callID string) error {
	d, err := newProcessResourceDomain(runID, requestID, callID)
	if err != nil {
		return err
	}
	return d.finish()
}

func procDisappeared(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH)
}

func processSnapshot(pid int) (ownedProcess, bool, error) {
	process := ownedProcess{pid: pid}
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return process, false, err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return process, false, fmt.Errorf("invalid proc stat for %d", pid)
	}
	fields := strings.Fields(string(raw)[end+1:])
	if len(fields) < 20 {
		return process, false, fmt.Errorf("short proc stat for %d", pid)
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return process, false, fmt.Errorf("invalid proc start time for %d", pid)
	}
	process.start = fields[19]
	process.group, err = strconv.Atoi(fields[2])
	if err != nil {
		return process, false, err
	}
	return process, fields[0] != "Z" && fields[0] != "X", nil
}

func sameProcessUID(pid int) (bool, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) != 5 {
				break
			}
			uid, err := strconv.Atoi(fields[2])
			return uid == os.Geteuid(), err
		}
	}
	return false, fmt.Errorf("missing process uid for %d", pid)
}

func (d *processResourceDomain) tags(pid int) (marked, anchor bool, err error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return false, false, err
	}
	want := []byte(processOwnerEnv + "=" + d.marker)
	anchorTag := []byte(processAnchorEnv + "=" + d.marker)
	for _, item := range bytes.Split(raw, []byte{0}) {
		if bytes.Equal(item, want) {
			marked = true
		}
		if bytes.Equal(item, anchorTag) {
			anchor = true
		}
	}
	return marked, marked && anchor, nil
}

func (d *processResourceDomain) processes() ([]ownedProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var owned, unmarked []ownedProcess
	anchors := make(map[int]ownedProcess)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		process, alive, err := processSnapshot(pid)
		if procDisappeared(err) || err == nil && !alive {
			continue
		}
		if err != nil {
			return nil, err
		}
		sameUID, err := sameProcessUID(pid)
		if procDisappeared(err) || err == nil && !sameUID {
			continue
		}
		if err != nil {
			return nil, err
		}
		marked, anchor, err := d.tags(pid)
		if procDisappeared(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect process-group ownership for pid %d: %w", pid, err)
		}
		if marked {
			process.anchor = anchor && process.pid == process.group
			owned = append(owned, process)
			if process.anchor {
				anchors[process.group] = process
			}
		} else {
			unmarked = append(unmarked, process)
		}
	}
	for _, process := range unmarked {
		if anchor, ok := anchors[process.group]; ok {
			process.groupAnchor = &anchor
			owned = append(owned, process)
		}
	}
	return owned, nil
}

func (d *processResourceDomain) anchorAlive(anchor ownedProcess) (bool, error) {
	current, alive, err := processSnapshot(anchor.pid)
	if procDisappeared(err) || err == nil && (!alive || current.start != anchor.start || current.group != anchor.pid) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	marked, isAnchor, err := d.tags(anchor.pid)
	if procDisappeared(err) {
		return false, nil
	}
	return marked && isAnchor, err
}

func (d *processResourceDomain) signal(process ownedProcess) error {
	fd, err := unix.PidfdOpen(process.pid, 0)
	if procDisappeared(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// Fix the kernel process object first, then revalidate the observed lifetime
	// and marker. A recycled numeric PID can never receive this owner's signal.
	current, alive, err := processSnapshot(process.pid)
	if procDisappeared(err) || err == nil && (!alive || current.start != process.start) {
		return nil
	}
	if err != nil {
		return err
	}
	if process.groupAnchor != nil {
		if current.group != process.group || current.group != process.groupAnchor.pid {
			return nil
		}
		// Validate the anchor after observing this candidate's group. A dead or
		// recycled anchor never authorizes signalling an untagged numeric PGID.
		alive, err := d.anchorAlive(*process.groupAnchor)
		if err != nil {
			return err
		}
		if !alive {
			return fmt.Errorf("process-group anchor lost; untagged ownership unresolved")
		}
	} else {
		marked, _, err := d.tags(process.pid)
		if procDisappeared(err) || err == nil && !marked {
			return nil
		}
		if err != nil {
			return err
		}
	}
	err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	if procDisappeared(err) {
		return nil
	}
	return err
}

func (d *processResourceDomain) kill() error {
	owned, err := d.processes()
	if err != nil {
		return err
	}
	var errs []error
	for _, process := range owned {
		if process.anchor {
			continue
		}
		if err := d.signal(process); err != nil {
			errs = append(errs, fmt.Errorf("terminate process-group owner %s pid %d: %w", d.owner, process.pid, err))
		}
	}
	if d.anchor != nil {
		// This direct child has never been waited/reaped, so its PID (and PGID)
		// cannot be reused even if the command has killed the anchor itself.
		if err := cleanupProcessGroup(d.anchor, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (d *processResourceDomain) finish() (finishErr error) {
	if d.anchor != nil {
		defer func() {
			// Reuse the existing group/tree cleanup while the unreaped anchor
			// still reserves its numeric group identity. Reap only afterwards.
			finishErr = errors.Join(finishErr, cleanupProcessGroup(d.anchor, nil))
			if err := d.anchor.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				finishErr = errors.Join(finishErr, err)
			}
			waited := make(chan error, 1)
			go func() { waited <- d.anchor.Wait() }()
			select {
			case err := <-waited:
				var exitErr *exec.ExitError
				if err != nil && !errors.As(err, &exitErr) {
					finishErr = errors.Join(finishErr, err)
				}
			case <-time.After(2 * time.Second):
				finishErr = errors.Join(finishErr, fmt.Errorf("process-group anchor reap pending"))
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		owned, err := d.processes()
		if err != nil {
			return err
		}
		if len(owned) == 0 {
			return nil
		}
		var nonAnchors []ownedProcess
		for _, process := range owned {
			if !process.anchor {
				nonAnchors = append(nonAnchors, process)
			}
		}
		if len(nonAnchors) != 0 {
			owned = nonAnchors
		}
		for _, process := range owned {
			if err := d.signal(process); err != nil {
				return fmt.Errorf("cleanup process-group owner %s: %w", d.owner, err)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process-group owner %s still has live tagged processes", d.owner)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
