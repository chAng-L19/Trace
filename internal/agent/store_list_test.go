package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListRunMetadataFollowsColdAndExternalGenerations(t *testing.T) {
	root := t.TempDir()
	one, _ := NewStore(root)
	snap, err := one.Create("session list", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	_, err = one.Update(id, func(s *Snapshot) error {
		s.Messages = []Message{{Role: "user", Content: strings.Repeat("x", 2<<20)}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cold, _ := NewStore(root)
	runs, err := cold.List()
	if err != nil || len(runs) != 1 || runs[0].ID != id {
		t.Fatalf("cold list: %+v %v", runs, err)
	}
	_, err = one.Update(id, func(s *Snapshot) error {
		s.Run.Title = "new title"
		s.Run.Archived = true
		s.Run.Deleted = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err = cold.List()
	if err != nil || len(runs) != 0 {
		t.Fatalf("external hide stale: %+v %v", runs, err)
	}
	runs, err = cold.ListWithDeleted()
	if err != nil || len(runs) != 1 || runs[0].Title != "new title" || !runs[0].Archived {
		t.Fatalf("metadata lost: %+v %v", runs, err)
	}
	// Legacy field order and non-Run JSON files retain their existing handling.
	raw, _ := json.Marshal(map[string]any{"aaa_unknown": []int{1, 2}, "run": runs[0]})
	if err = os.WriteFile(filepath.Join(root, id+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	runs, err = cold.ListWithDeleted()
	if err != nil || len(runs) != 1 {
		t.Fatalf("legacy order: %+v %v", runs, err)
	}
	if err = os.WriteFile(filepath.Join(root, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	runs, err = cold.ListWithDeleted()
	if err != nil || len(runs) != 1 {
		t.Fatalf("non-run listed: %+v %v", runs, err)
	}
	if err = os.WriteFile(filepath.Join(root, id+".json"), []byte("{\"run\":{\"id\":\"wrong\"}}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = cold.List(); err == nil {
		t.Fatal("mismatched identity listed")
	}
}
