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
