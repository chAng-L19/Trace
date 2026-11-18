package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFullStoreReadsRejectNonRunAndMismatchedIdentityWithoutWrites(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	valid, err := store.Create("protected run", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	protectedPath := filepath.Join(store.root, valid.Run.ID+".json")
	protected, err := os.ReadFile(protectedPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		id, body string
		missing  bool
	}{
		{"web-config", `{"provider":{"model":"preserve-me"}}`, true},
		{"empty-run", `{"run":{}}`, true},
		{"mismatch", `{"run":{"id":"` + valid.Run.ID + `","goal":"overwrite protected run"}}`, false},
	} {
		t.Run(fixture.id, func(t *testing.T) {
			path := filepath.Join(store.root, fixture.id+".json")
			if err := os.WriteFile(path, []byte(fixture.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := store.Load(fixture.id)
			if err == nil || fixture.missing != os.IsNotExist(err) {
				t.Fatalf("unexpected identity result: %v", err)
			}
			called := false
			_, err = store.Update(fixture.id, func(s *Snapshot) error { called = true; s.Run.Goal = "mutated"; return nil })
			if err == nil || called {
				t.Fatal("invalid snapshot reached mutation callback")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != fixture.body {
				t.Fatal("invalid input rewritten")
			}
			after, err = os.ReadFile(protectedPath)
			if err != nil || !bytes.Equal(after, protected) {
				t.Fatal("another run was overwritten")
			}
			if _, err := os.Stat(filepath.Join(store.root, ".json")); !os.IsNotExist(err) {
				t.Fatal("zero-ID snapshot created")
			}
		})
	}
}
