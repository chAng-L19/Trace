package localtool

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"trace/internal/worker"
)

func TestLocalToolFixture(t *testing.T) {
	if os.Getenv("TRACE_LOCAL_FIXTURE") == "" {
		return
	}
	stdin, _ := io.ReadAll(os.Stdin)
	dir, _ := os.Getwd()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": os.Args[3:], "stdin": string(stdin), "cwd": dir, "env": os.Getenv("TRACE_LOCAL_FIXTURE"), "session": os.Getenv("TRACE_BROWSER_RUN_ID")})
	os.Exit(0)
}

func TestManifestRunsExactArgvAndStdio(t *testing.T) {
	binary, _ := os.Executable()
	for _, mode := range []Mode{ModeArgv, ModeStdio} {
		t.Run(string(mode), func(t *testing.T) {
			m := Manifest{Name: "fixture", Command: binary, Args: []string{"-test.run=^TestLocalToolFixture$", "--", "{{value}}", "{{list...}}", "{{number}}", "{{flag}}"}, Mode: mode, Dir: t.TempDir(), Env: map[string]string{"TRACE_LOCAL_FIXTURE": "set"}}
			arguments := json.RawMessage(`{"value":"space & $(literal) ; text","list":["one","two words"],"number":3,"flag":true}`)
			result, err := m.Execute(context.Background(), "run", "call", arguments)
			if err != nil || result.Status != worker.StatusCompleted || result.RunID != "run" || result.CallID != "call" {
				t.Fatalf("execute: %+v %v", result, err)
			}
			var observed struct {
				Args            []string
				Stdin, Cwd, Env string
			}
			if err := json.Unmarshal([]byte(result.Stdout), &observed); err != nil {
				t.Fatal(err)
			}
			want := []string{"space & $(literal) ; text", "one", "two words", "3", "true"}
			if !reflect.DeepEqual(observed.Args, want) || observed.Cwd != m.Dir || observed.Env != "set" {
				t.Fatalf("observed: %+v", observed)
			}
			if (mode == ModeStdio && observed.Stdin != string(arguments)+"\n") || (mode == ModeArgv && observed.Stdin != "") {
				t.Fatalf("stdin: %q", observed.Stdin)
			}
		})
	}
}

func TestManifestInvalidInvocation(t *testing.T) {
	m := Manifest{Name: "test", Command: "not-started", Args: []string{"{{value}}"}}
	for _, arguments := range []string{"null", "[]", "bad", `{}`, `{"value":{}}`, `{"value":null}`, `{"value":[]}`} {
		if _, _, err := m.Invocation(json.RawMessage(arguments)); err == nil {
			t.Fatalf("accepted %s", arguments)
		}
	}
	m.Args = []string{"{{value...}}"}
	for _, arguments := range []string{`{"value":3}`, `{"value":null}`, `{"value":[{}]}`} {
		if _, _, err := m.Invocation(json.RawMessage(arguments)); err == nil {
			t.Fatalf("accepted spread %s", arguments)
		}
	}
	m.Args = []string{"{{arguments}}"}
	command, _, err := m.Invocation(json.RawMessage(`{"value":"ok"}`))
	if err != nil || !strings.Contains(command[1], `"value"`) {
		t.Fatal(command, err)
	}
}

func TestManifestCancelGraceIsOptIn(t *testing.T) {
	m := Manifest{Name: "grace", Command: "fixture"}
	for _, seconds := range []int{0, 15} {
		m.CancelGraceSeconds = seconds
		request, err := m.Request("run", "call", json.RawMessage(`{}`))
		if err != nil || request.CancelGraceSeconds != seconds {
			t.Fatalf("grace propagation: %+v, %v", request, err)
		}
	}
	m.CancelGraceSeconds = -1
	if err := m.Validate(); err == nil {
		t.Fatal("negative cancellation grace accepted")
	}
}

func TestManifestTimeoutRejectsDurationOverflow(t *testing.T) {
	// These boundary values require a 64-bit int, as on the deployment server.
	maximum := int64((1<<63 - 1) / time.Second)
	if int64(int(maximum+1)) != maximum+1 {
		t.Skip("timeout overflow boundary exceeds this platform's int")
	}
	m := Manifest{Name: "timeout", Command: "not-started", TimeoutSeconds: int(maximum)}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.TimeoutSeconds++
	if _, err := m.Request("run", "call", json.RawMessage(`{}`)); err == nil {
		t.Fatal("overflowing positive timeout accepted before execution")
	}
}
