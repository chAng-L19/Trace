package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoricalToolResultFieldsPreserveEvidenceAndArtifacts(t *testing.T) {
	a, store := newTestAgent(t, resultProvider{})
	snap, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutArtifact("legacy_checkpoint", []byte("historical checkpoint bytes"))
	if err != nil {
		t.Fatal(err)
	}
	snap.Actions[0].Result.Artifacts = append(snap.Actions[0].Result.Artifacts, ref)
	// Historical results predate resource isolation; keep the fixture free of
	// current-only fields before asserting its original serialized bytes.
	snap.Actions[0].Result.WorkerResourceOwner = ""
	snap.Actions[0].Result.WorkerResourceLimit = ""
	base, err := json.Marshal(snap.Actions[0].Result)
	if err != nil {
		t.Fatal(err)
	}
	// Original serialized order, deliberately unlike sorted map keys. Raw nested
	// data remains opaque; retaining it does not reconnect any retired provider.
	prefix := `{"rea_session":{"server":"rea","session_id":"historical","binding":{"z":2,"a":1}},"rea_checkpoint":{"kind":"bundle","artifact_hash":"` + ref.Hash + `"},"rea_restore":{"verified":true,"source":"historical"},`
	content := prefix + string(base[1:])
	if err := json.Unmarshal([]byte(content), snap.Actions[0].Result); err != nil {
		t.Fatal(err)
	}
	for i := range snap.Messages {
		if snap.Messages[i].Role == "tool" {
			snap.Messages[i].Content = content
		}
	}
	snap.Evidence[0].Content = content
	snap.Evidence[0].ContentHash = promptHash(content)
	encoded, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, snap.Run.ID+".json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"load", "update", "reload"} {
		if operation == "update" {
			snap, err = store.Update(snap.Run.ID, func(*Snapshot) error { return nil })
		} else {
			snap, err = store.Load(snap.Run.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
		actual, err := json.Marshal(snap.Actions[0].Result)
		if err != nil || string(actual) != content {
			t.Fatalf("%s changed historical result: %s %v", operation, actual, err)
		}
		if snap.Evidence[0].Verification != "verified" || snap.Result.Status != "success" {
			t.Fatalf("%s invalidated historical proof: %+v", operation, snap.Result)
		}
		artifact, err := store.ReadArtifact(ref.Hash, 0, 100)
		if err != nil || artifact.Content != "historical checkpoint bytes" {
			t.Fatalf("historical artifact lost: %+v %v", artifact, err)
		}
	}
	// Compatibility must not weaken the unchanged evidence validator.
	snap.Actions[0].Result.LegacyREARestore = json.RawMessage(`{"verified":false}`)
	refreshResult(&snap)
	if snap.Evidence[0].Verification == "verified" || snap.Result.Status == "success" {
		t.Fatal("historical metadata tampering accepted")
	}
	plain, _ := json.Marshal(ToolResult{Stdout: "new result"})
	if strings.Contains(string(plain), "rea_") {
		t.Fatal("new results populated retired fields")
	}
}
