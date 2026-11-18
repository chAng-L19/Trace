package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"trace/internal/localtool"
)

func TestSourceDirectoryDoesNotConfuseInterpreterAndProject(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "venv", "bin", "python")
	script := filepath.Join(root, "browser-project", "browser.py")
	for _, file := range []string{interpreter, script} {
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		name, local, dir, command string
		args                      []string
		want                      string
	}{
		{name: "interpreter only", command: interpreter},
		{name: "workdir alone", dir: root, command: interpreter},
		{name: "module", command: interpreter, args: []string{"-m", "fixture", script}},
		{name: "script", command: interpreter, args: []string{"-u", script}, want: filepath.Dir(script)},
		{name: "relative script", dir: filepath.Dir(script), command: interpreter, args: []string{"browser.py"}, want: filepath.Dir(script)},
		{name: "script command", command: script, want: filepath.Dir(script)},
		{name: "real bin source", local: filepath.Join(root, "bin"), command: interpreter, want: filepath.Join(root, "bin")},
	} {
		t.Run(item.name, func(t *testing.T) {
			if got := sourceDirectory(item.local, item.dir, item.command, item.args); got != item.want {
				t.Fatalf("source=%q want %q", got, item.want)
			}
		})
	}
	s, _ := fixture(t, nil)
	s.config.LocalTools = []localtool.Manifest{{Name: "browser", Command: interpreter}, {Name: "other", Command: interpreter}}
	c := s.configWithSources()
	for _, tool := range c.LocalTools {
		if tool.LocalPath != "" {
			t.Fatalf("shared interpreter became a project: %s %s", tool.Name, tool.LocalPath)
		}
	}
}

func TestSourceIdentityDoesNotInferFromUnrelatedGitWorkdirOrInterpreter(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init").CombinedOutput(); err != nil {
		t.Fatalf("git fixture: %s %v", output, err)
	}
	interpreter := filepath.Join(root, "venv", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(interpreter), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(interpreter, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s, _ := fixture(t, nil)
	s.config.LocalTools = []localtool.Manifest{
		{Name: "shared-interpreter", Command: interpreter},
		{Name: "unrelated-workdir", Command: "python", Dir: root, Args: []string{"-m", "other"}},
		{Name: "explicit-source", Command: interpreter, LocalPath: root},
	}
	projected := s.configWithSources()
	for _, tool := range projected.LocalTools {
		if tool.Name == "explicit-source" {
			if !samePath(tool.LocalPath, root) {
				t.Fatalf("explicit source lost: %+v", tool)
			}
		} else if tool.LocalPath != "" || tool.GitHubURL != "" {
			t.Fatalf("execution directory became source: %+v", tool)
		}
	}
}
