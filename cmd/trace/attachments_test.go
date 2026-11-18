package main

import (
	"os"
	"path/filepath"
	"testing"
	"trace/internal/agent"
)

func TestInputArgumentsKeepsTextAndOriginalBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "original.txt")
	if err := os.WriteFile(path, []byte("input"), 0600); err != nil {
		t.Fatal(err)
	}
	store, _ := agent.NewStore(filepath.Join(dir, "state"))
	text, inputs, err := inputArguments(store, []string{"inspect", "--attach", path, "this"})
	if err != nil || text != "inspect this" || len(inputs) != 1 || inputs[0].Name != "original.txt" {
		t.Fatalf("%s %+v %v", text, inputs, err)
	}
	if _, _, err = inputArguments(store, []string{"goal", "--attach"}); err == nil {
		t.Fatal("missing path accepted")
	}
}
