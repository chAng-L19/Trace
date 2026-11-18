package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"trace/internal/localtool"
)

func TestFindIntegrationSourceHead(t *testing.T) {
	for _, name := range []string{"branch", "detached", "unborn", "missing-commit", "non-commit"} {
		t.Run(name, func(t *testing.T) {
			root, url := gitFixture(t)
			head := mustGit(t, root, "rev-parse", "HEAD")
			wantRef, wantStatus := "refs/heads/main", "up_to_date"
			invalid := false
			switch name {
			case "detached":
				mustGit(t, root, "checkout", "--detach", head)
				wantRef, wantStatus = head, "pinned"
			case "unborn":
				mustGit(t, root, "symbolic-ref", "HEAD", "refs/heads/unborn")
				invalid = true
			case "missing-commit":
				writeFixture(t, root, ".git/HEAD", strings.Repeat("1", len(head))+"\n")
				invalid = true
			case "non-commit":
				blob := mustGit(t, root, "hash-object", "-w", "skills/one/SKILL.md")
				writeFixture(t, root, ".git/HEAD", blob+"\n")
				invalid = true
			}
			s := integrationFixture(t)
			s.config.LocalTools = []localtool.Manifest{{Name: "source", Command: "fixture", LocalPath: root, GitHubURL: url}}
			in := integrationRequest{Kind: "local", Name: "source"}
			entry, err := s.findIntegration(in)
			wantCode := 200
			if invalid {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "git rev-parse:") || entry.Revision != "" || entry.Ref != "" {
					t.Fatalf("invalid HEAD accepted or Git error lost: entry=%+v err=%v", entry, err)
				}
				wantCode = 400
			} else if err != nil || entry.Revision != head || entry.Ref != wantRef {
				t.Fatalf("source HEAD: entry=%+v err=%v; want revision=%s ref=%s", entry, err, head, wantRef)
			}
			previous := s.a
			for _, op := range []string{"check", "update"} {
				response := request(t, s.Handler(), "POST", "/api/integrations/"+op, in, wantCode)
				if !invalid {
					var status integrationStatus
					if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
						t.Fatal(err)
					}
					if status.Status != wantStatus || status.CurrentRevision != head || status.Integration.Ref != wantRef {
						t.Fatalf("%s source status: %+v", op, status)
					}
				}
			}
			if s.a != previous || len(s.config.Integrations) != 0 || s.configuring {
				t.Fatal("source lookup or no-op update changed the live configuration")
			}
		})
	}
}

// Keep repository operations real; intercept only the Git command under test.
func integrationSourceGitFault(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Git fault injection uses the validation server's POSIX shell")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFixture(t, dir, "git", "#!/bin/sh\n"+body+"\nexec \"$TRACE_INTEGRATION_SOURCE_GIT\" \"$@\"\n")
	if err := os.Chmod(filepath.Join(dir, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACE_INTEGRATION_SOURCE_GIT", realGit)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestFindIntegrationSourceGitFailures(t *testing.T) {
	for _, item := range []struct {
		name, condition, command string
	}{
		{"revision", `[ "$1" = rev-parse ] && [ "$2" != --show-toplevel ]`, "rev-parse"},
		{"symbolic-ref", `[ "$1" = symbolic-ref ]`, "symbolic-ref"},
	} {
		t.Run(item.name, func(t *testing.T) {
			root, url := gitFixture(t)
			s := integrationFixture(t)
			s.config.LocalTools = []localtool.Manifest{{Name: "source", Command: "fixture", LocalPath: root, GitHubURL: url}}
			if err := saveConfig(s.options.ConfigPath, s.config); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.options.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			previous, revision := s.a, s.config.Revision
			integrationSourceGitFault(t, "if "+item.condition+"; then echo 'fixture Git infrastructure failure' >&2; exit 128; fi")
			in := integrationRequest{Kind: "local", Name: "source"}
			entry, err := s.findIntegration(in)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128 || !strings.Contains(err.Error(), "git "+item.command+":") || !strings.Contains(err.Error(), "fixture Git infrastructure failure") || entry.Ref != "" {
				t.Fatalf("Git failure became a pinned source or lost its cause: entry=%+v err=%v", entry, err)
			}
			for _, op := range []string{"check", "update"} {
				response := request(t, s.Handler(), "POST", "/api/integrations/"+op, in, 400)
				if !strings.Contains(response.Body.String(), "fixture Git infrastructure failure") {
					t.Fatalf("%s hid the Git error: %s", op, response.Body.String())
				}
			}
			after, err := os.ReadFile(s.options.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || s.a != previous || s.config.Revision != revision || len(s.config.Integrations) != 0 || s.configuring {
				t.Fatal("failed source discovery changed saved configuration or the live Agent")
			}
		})
	}
}

func TestFindIntegrationSourceGitCancellation(t *testing.T) {
	root, url := gitFixture(t)
	s := integrationFixture(t)
	s.config.LocalTools = []localtool.Manifest{{Name: "source", Command: "fixture", LocalPath: root, GitHubURL: url}}
	marker := filepath.Join(t.TempDir(), "symbolic-ref-started")
	t.Setenv("TRACE_INTEGRATION_SOURCE_MARKER", marker)
	integrationSourceGitFault(t, "if [ \"$1\" = symbolic-ref ]; then : > \"$TRACE_INTEGRATION_SOURCE_MARKER\"; exec sleep 20; fi")
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	s.ctx = ctx
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	entry, err := s.findIntegration(integrationRequest{Kind: "local", Name: "source"})
	<-done
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("fixture did not cancel symbolic-ref: %v", ctx.Err())
	}
	var exitErr *exec.ExitError
	if err == nil || entry.Ref != "" || !strings.Contains(err.Error(), "git symbolic-ref:") || (!errors.As(err, &exitErr) && !errors.Is(err, context.Canceled)) {
		t.Fatalf("cancelled Git command became pinned or lost its cause: entry=%+v err=%v", entry, err)
	}
}
