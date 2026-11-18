package main

import (
	"os"
	"path/filepath"
	"testing"
	"trace/internal/agent"
)

func TestStoreSQLiteMaintenanceCommands(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TRACE_DATA_DIR", root)
	s, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Create("maintenance", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"store-migrate", "invalid"}); err == nil {
		t.Fatal("invalid backend accepted")
	}
	if _, err = os.Stat(filepath.Join(root, "store.sqlite")); !os.IsNotExist(err) {
		t.Fatal("invalid command migrated")
	}
	if err = run([]string{"store-migrate", "sqlite"}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "snapshot-export")
	if err = run([]string{"store-export", destination}); err != nil {
		t.Fatal(err)
	}
	restored, err := agent.NewStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.Load(snap.Run.ID)
	if err != nil || got.Run.Goal != "maintenance" {
		t.Fatalf("export: %v", err)
	}
	if err = run([]string{"store-export", destination}); err == nil {
		t.Fatal("export overwrote existing directory")
	}
}
