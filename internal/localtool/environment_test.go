package localtool

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestManifestRunEnvironmentIsolation(t *testing.T) {
	// The operator template must override inherited defaults while leaving model
	// argv/stdin literal. Separate actions retain their Run identity.
	t.Setenv("TRACE_BROWSER_RUN_ID", "shared-host-default")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Name: "session-fixture", Command: binary, Mode: ModeStdio,
		Args: []string{"-test.run=^TestLocalToolFixture$", "--", "{{argv...}}"},
		Env:  map[string]string{"TRACE_LOCAL_FIXTURE": "set", "TRACE_BROWSER_RUN_ID": "trace-{{run_id}}"}}
	arguments := json.RawMessage(`{"argv":["fill","{{run_id}}","two words & $(literal)"]}`)
	for _, runID := range []string{"run-a", "run-b", "run-a"} {
		result, err := m.Execute(context.Background(), runID, "call", arguments)
		if err != nil {
			t.Fatalf("execution: %+v %v", result, err)
		}
		var observed struct {
			Args           []string
			Session, Stdin string
		}
		if err := json.Unmarshal([]byte(result.Stdout), &observed); err != nil {
			t.Fatal(err)
		}
		if observed.Session != "trace-"+runID || observed.Stdin != string(arguments)+"\n" || !reflect.DeepEqual(observed.Args, []string{"fill", "{{run_id}}", "two words & $(literal)"}) {
			t.Fatalf("isolation or literal argv: %+v", observed)
		}
	}
	if m.Env["TRACE_BROWSER_RUN_ID"] != "trace-{{run_id}}" {
		t.Fatal("reusable manifest mutated")
	}
	if _, err := m.Request("", "call", arguments); err == nil {
		t.Fatal("empty run ID accepted for opted-in template")
	}
	m.Env["TRACE_BROWSER_RUN_ID"] = "operator-fixed-session"
	if request, err := m.Request("", "call", arguments); err != nil || !reflect.DeepEqual(request.Env, []string{"TRACE_BROWSER_RUN_ID=operator-fixed-session", "TRACE_LOCAL_FIXTURE=set"}) {
		t.Fatalf("non-template environment changed: %+v %v", request, err)
	}
}
