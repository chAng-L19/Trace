//go:build windows

package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"trace/internal/localtool"
)

type shellFixtureProvider struct {
	command   string
	tool      string
	arguments string
}

func (p shellFixtureProvider) Complete(_ context.Context, r ModelRequest) (ModelResponse, error) {
	if r.Messages[len(r.Messages)-1].Role == "tool" {
		return ModelResponse{Content: "shell fixture complete"}, nil
	}
	name, args := p.tool, p.arguments
	if name == "" {
		name = "exec"
		raw, _ := json.Marshal(map[string]string{"command": p.command})
		args = string(raw)
	}
	return ModelResponse{ToolCalls: []ToolCall{{ID: "shell-fixture", Name: name, Arguments: args}}}, nil
}

func TestWindowsExecShellQuoting(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("Python is required for the real shell regression")
	}
	quotedPython := `"` + python + `"`
	cases := []struct {
		name, command, stdout, stderr string
		code                          int
	}{
		{"python_quotes", `python -c "print('TRACE_UI_MARKER')"`, "TRACE_UI_MARKER", "", 0},
		{"quoted_executable", quotedPython + ` -c "print('QUOTED_EXEC')"`, "QUOTED_EXEC", "", 0},
		{"stdout_stderr", quotedPython + ` -c "import sys; print('OUT WITH SPACES'); print('ERR WITH SPACES', file=sys.stderr)"`, "OUT WITH SPACES", "ERR WITH SPACES", 0},
		{"nonzero", quotedPython + ` -c "import sys; print('PARTIAL'); print('FAILURE', file=sys.stderr); sys.exit(7)"`, "PARTIAL", "FAILURE", 7},
		{"echo", "echo TRACE_ECHO", "TRACE_ECHO", "", 0},
		{"operators", "echo FIRST&&echo SECOND&echo DIAGNOSTIC 1>&2", "FIRST\r\nSECOND", "DIAGNOSTIC", 0},
		{"pipe", "echo TRACE_PIPE|findstr TRACE_PIPE", "TRACE_PIPE", "", 0},
		{"operator_inside_quotes", quotedPython + ` -c "print('A&B|C')"`, "A&B|C", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a, err := New(store, shellFixtureProvider{command: tc.command}, "")
			if err != nil {
				t.Fatal(err)
			}
			defer a.CloseMCP()
			s, err := a.Create(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			s, err = a.Run(context.Background(), s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Actions) != 1 || s.Actions[0].Result == nil {
				t.Fatalf("missing action: %+v", s.Actions)
			}
			result := s.Actions[0].Result
			t.Logf("command=%q stdout=%q stderr=%q exit=%d", tc.command, result.Stdout, result.Stderr, result.ExitCode)
			if !strings.Contains(result.Stdout, tc.stdout) || !strings.Contains(result.Stderr, tc.stderr) || result.ExitCode != tc.code {
				t.Fatalf("shell semantics changed: %+v", result)
			}
			found := false
			for _, event := range s.Events {
				if event.Type == "tool_output" {
					found = true
				}
			}
			if !found {
				t.Fatal("tool_output preview was not persisted")
			}
		})
	}
}

func TestWindowsLocalPythonArgvUnchanged(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("Python is required for the real argv regression")
	}
	manifest := localtool.Manifest{Name: "python-argv", Command: python, Args: []string{"-c", `import sys; print(sys.argv[1]); print('LOCAL STDERR', file=sys.stderr)`, "{{value}}"}, InputSchema: json.RawMessage(`{"type":"object"}`)}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	value := `literal "quotes" & | > % spaces`
	args, _ := json.Marshal(map[string]string{"value": value})
	a, err := New(store, shellFixtureProvider{tool: manifest.StableID(), arguments: string(args)}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.CloseMCP()
	if err = a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	s, err := a.Create("literal argv")
	if err != nil {
		t.Fatal(err)
	}
	s, err = a.Run(context.Background(), s.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Actions) != 1 || s.Actions[0].Result == nil {
		t.Fatal("missing local action")
	}
	result := s.Actions[0].Result
	if strings.TrimSpace(result.Stdout) != value || strings.TrimSpace(result.Stderr) != "LOCAL STDERR" || result.ExitCode != 0 {
		t.Fatalf("local argv changed: %+v", result)
	}
}
