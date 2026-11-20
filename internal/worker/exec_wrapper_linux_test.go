//go:build linux

package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExecWrapperGoMemoryBudget(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source path unavailable")
	}
	script, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "scripts", "exec-cgroup-isolated.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the actual configuration preflight and child environment without
	// touching cgroups. Full wrapper admission has separate delegated tests.
	configuration, _, ok := strings.Cut(string(script), "relative=$(sed ")
	if !ok {
		t.Fatal("wrapper cgroup boundary missing")
	}
	probe := configuration + `exec /bin/sh -c 'printf "%s|%s" "${GOMEMLIMIT+set}" "${GOMEMLIMIT-}"'`
	for _, tc := range []struct {
		name, controller, explicit, want string
		set, rejected                    bool
	}{
		{name: "default", want: "set|536870912"},
		{name: "scaled", controller: "805306368", want: "set|402653184"},
		{name: "maximum_integer", controller: "999999999999999", want: "set|499999999999999"},
		{name: "explicit", explicit: "384MiB", set: true, want: "set|384MiB"},
		{name: "explicit_off", explicit: "off", set: true, want: "set|off"},
		{name: "explicit_empty", set: true, want: "set|"},
		{name: "invalid_controller", controller: "-1", rejected: true},
		{name: "integer_out_of_range", controller: "1000000000000000", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", probe, "wrapper-config-test", "target")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GOMEMLIMIT=") && !strings.HasPrefix(entry, "TRACE_CONTROLLER_MEMORY_BYTES=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			if tc.controller != "" {
				cmd.Env = append(cmd.Env, "TRACE_CONTROLLER_MEMORY_BYTES="+tc.controller)
			}
			if tc.set {
				cmd.Env = append(cmd.Env, "GOMEMLIMIT="+tc.explicit)
			}
			out, err := cmd.CombinedOutput()
			if tc.rejected {
				if err == nil || !strings.Contains(string(out), "controller_memory") || !strings.Contains(string(out), "target not started") {
					t.Fatalf("invalid controller reached target: %q %v", out, err)
				}
				return
			}
			if err != nil || string(out) != tc.want {
				t.Fatalf("child GOMEMLIMIT = %q, want %q: %v", out, tc.want, err)
			}
		})
	}
}

func TestExecWrapperManagedHTTP(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "scripts", "exec-cgroup-isolated.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, tail, ok := strings.Cut(string(data), "# Managed HTTP command")
	if !ok {
		t.Fatal("managed HTTP setup absent")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "wrapper.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nset -eu\nfail() { echo \"$*\" >&2; exit 2; }\n# Managed HTTP command"+tail), 0755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "runtime", "bin", "curl")
	for _, state := range []string{"missing-directory", "missing-entry", "not-executable", "ready"} {
		if state == "missing-entry" {
			if err := os.MkdirAll(filepath.Dir(entry), 0755); err != nil {
				t.Fatal(err)
			}
		}
		if state == "not-executable" {
			if err := os.WriteFile(entry, []byte("#!/bin/sh\nprintf managed"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if state == "ready" {
			if err := os.Chmod(entry, 0755); err != nil {
				t.Fatal(err)
			}
		}
		out, err := exec.Command("/bin/sh", script, "/bin/sh", "-c", "command -v curl").CombinedOutput()
		if state == "ready" {
			if err != nil || strings.TrimSpace(string(out)) != entry {
				t.Fatalf("managed PATH: %q %v", out, err)
			}
		} else if err == nil || !strings.Contains(string(out), "managed curl-cffi entry missing") {
			t.Fatalf("%s fell through to native curl: %q %v", state, out, err)
		}
	}
}
