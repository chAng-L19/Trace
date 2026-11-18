package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillBoundedReadAndRevisionIdentity(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	path := filepath.Join(t.TempDir(), "SKILL.md")
	write := func(body []byte) {
		t.Helper()
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	a.skills = []Skill{{Name: "bounded", Path: path, Source: filepath.Dir(path)}}
	first := "---\nname: bounded\n---\n正文😀"
	write([]byte(first))
	value, err := a.builtin(context.Background(), "", ToolCall{Name: "read_skill", Arguments: `{"name":"bounded"}`})
	if err != nil {
		t.Fatal(err)
	}
	doc := value.(SkillRead)
	digest := sha256.Sum256([]byte(first))
	if doc.Body != first || doc.SHA256 != hex.EncodeToString(digest[:]) || doc.Artifact.Hash != doc.SHA256 || doc.Bytes != len(first) || doc.Path != path {
		t.Fatalf("incorrect loaded identity: %+v", doc)
	}
	write([]byte("new revision"))
	args, _ := json.Marshal(map[string]string{"name": "bounded", "expected_sha256": doc.SHA256})
	if _, err := a.builtin(context.Background(), "", ToolCall{Name: "read_skill", Arguments: string(args)}); err == nil || !strings.Contains(err.Error(), "revision mismatch") {
		t.Fatalf("stale pin accepted: %v", err)
	}
	current, err := a.readModelSkillDocument("bounded", "")
	if err != nil || current.Body != "new revision" || current.SHA256 == doc.SHA256 {
		t.Fatalf("revision not refreshed: %+v %v", current, err)
	}
	stored, err := store.ReadArtifact(doc.SHA256, 0, int64(len(first)))
	if err != nil || stored.Content != first {
		t.Fatalf("old captured revision lost: %+v %v", stored, err)
	}
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"oversize", []byte(strings.Repeat("x", maxSkillBytes+1)), "byte limit"},
		{"utf8", []byte{0xff, 0xfe}, "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(tc.body)
			before, err := os.ReadDir(filepath.Join(store.root, "artifacts"))
			if err != nil {
				t.Fatal(err)
			}
			if body, err := a.ReadSkill("bounded"); err == nil || body != "" || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("management boundary: %d %v", len(body), err)
			}
			if _, err := a.readModelSkillDocument("bounded", ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("model boundary: %v", err)
			}
			after, err := os.ReadDir(filepath.Join(store.root, "artifacts"))
			if err != nil || len(before) != len(after) {
				t.Fatalf("rejected skill created CAS entries: %v", err)
			}
		})
	}
	write([]byte(strings.Repeat("x", maxSkillBytes)))
	if body, err := a.ReadSkill("bounded"); err != nil || len(body) != maxSkillBytes {
		t.Fatalf("exact boundary: %d %v", len(body), err)
	}
	a.skills[0].Disabled = true
	if _, err := a.readModelSkillDocument("bounded", ""); err == nil {
		t.Fatal("disabled model read accepted")
	}
	if _, err := a.ReadSkill("bounded"); err != nil {
		t.Fatalf("disabled management read: %v", err)
	}
	a.skills[0].Path = filepath.Dir(path)
	if _, err := a.ReadSkill("bounded"); err == nil {
		t.Fatal("directory read accepted")
	}
}

func TestSkillReadPersistsRawIdentityInActionLedger(t *testing.T) {
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "read-revision", Name: "read_skill", Arguments: `{"name":"fixture"}`}))
	path := filepath.Join(t.TempDir(), "SKILL.md")
	body := []byte(strings.Repeat("正文😀\n", 2500))
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	a.skills = []Skill{{Name: "fixture", Path: path, Source: filepath.Dir(path)}}
	id := createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil || len(s.Actions) != 1 || s.Actions[0].Result == nil || s.Actions[0].Result.ExitCode != 0 {
		t.Fatalf("read action: %+v %v", s.Actions, err)
	}
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	found, stdout := false, false
	for _, ref := range s.Actions[0].Result.Artifacts {
		if ref.Kind == "skill" && ref.Hash == hash && ref.Bytes == int64(len(body)) {
			found = true
		}
		if ref.Kind == "stdout" && ref.Hash != hash && ref.Bytes > int64(len(body)) {
			stdout = true
		}
	}
	if !found || !stdout || s.Actions[0].Result.Stdout != "" {
		t.Fatalf("raw/serialized revision not separately archived: %+v", s.Actions[0].Result)
	}
	reloaded, err := store.Load(id)
	if err != nil || len(reloaded.Actions[0].Result.Artifacts) < 1 {
		t.Fatalf("identity not persisted: %v", err)
	}
	writeErr := os.WriteFile(path, []byte("replacement"), 0600)
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	raw, err := store.ReadArtifact(hash, 0, int64(len(body)))
	if err != nil || raw.Content != string(body) {
		t.Fatalf("captured bytes changed: %v", err)
	}
}
