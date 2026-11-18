package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactRegisterCapturesDeliverableAndSource(t *testing.T) {
	data := append([]byte("report\n"), 0, 255, 128)
	path := filepath.Join(t.TempDir(), "report.bin")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(ArtifactRegistration{Path: path, Name: "研究报告.bin", Purpose: "交付验证记录"})
	if err != nil {
		t.Fatal(err)
	}
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "deliver", Name: "artifact_register", Arguments: string(args)}))
	id := createTestRun(t, a)
	if _, err := a.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Load(id)
	if err != nil || len(snap.Actions) != 1 || snap.Actions[0].Result == nil {
		t.Fatalf("registration not recorded: %+v %v", snap.Actions, err)
	}
	action := snap.Actions[0]
	if action.Name != "artifact_register" || action.CallID != "deliver" || action.RequestID != snap.Requests[0].ID || action.Arguments != string(args) {
		t.Fatalf("file source detached from execution: %+v", action)
	}
	if len(action.Result.Artifacts) != 1 {
		t.Fatalf("registration did not promote actual reference: %+v", action.Result)
	}
	ref := action.Result.Artifacts[0]
	if ref.Hash != promptHash(string(data)) || ref.Bytes != int64(len(data)) || ref.Kind != "file" || ref.Name != "研究报告.bin" || ref.Purpose != "交付验证记录" || ref.MIMEType != "application/octet-stream" {
		t.Fatalf("file metadata or content hash mismatch: %+v", ref)
	}
	var reported ArtifactRef
	if err := json.Unmarshal([]byte(action.Result.Stdout), &reported); err != nil || reported != ref {
		t.Fatalf("model result differs from persisted reference: %+v %v", reported, err)
	}
	if len(snap.Evidence) != 1 || snap.Evidence[0].RequestID != action.RequestID || snap.Evidence[0].CallID != action.CallID || snap.Evidence[0].Verification != "verified" {
		t.Fatalf("registration source evidence missing: %+v", snap.Evidence)
	}
	// Subsequent workspace edits must not rewrite an already registered blob.
	if err := os.WriteFile(path, []byte("new revision"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := store.OpenArtifact(ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := io.ReadAll(f)
	f.Close()
	if err != nil || !bytes.Equal(captured, data) {
		t.Fatal("workspace mutation changed captured file")
	}
	reloaded, err := store.Load(id)
	if err != nil || reloaded.Evidence[0].ContentHash != snap.Evidence[0].ContentHash || reloaded.Actions[0].Result.Artifacts[0] != ref {
		t.Fatal("reload rewrote historical artifact/evidence")
	}
}

func TestArtifactRegisterRejectsMissingAndNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	for _, path := range []string{missing, root} {
		args, _ := json.Marshal(ArtifactRegistration{Path: path})
		a, store := newTestAgent(t, manualProvider(ToolCall{ID: "missing", Name: "artifact_register", Arguments: string(args)}))
		id := createTestRun(t, a)
		snap, err := a.Run(context.Background(), id)
		if err != nil || len(snap.Actions) != 1 || snap.Actions[0].Result == nil {
			t.Fatalf("failed registration not recorded: %v", err)
		}
		result := snap.Actions[0].Result
		if result.ExitCode == 0 || result.Error == "" || len(result.Artifacts) != 0 {
			t.Fatalf("failed registration fabricated reference: %+v", result)
		}
		saved, err := store.Load(id)
		if err != nil || len(saved.Actions) != 1 || saved.Actions[0].Result == nil || len(saved.Actions[0].Result.Artifacts) != 0 {
			t.Fatalf("failed registration persisted a file reference: %+v %v", saved.Actions, err)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatalf("failed registration created the missing source file: %v", err)
		}
	}
}

func TestArtifactMetadataPreservesLegacyJSON(t *testing.T) {
	legacy := `{"hash":"old-hash","kind":"stdout","bytes":12}`
	var ref ArtifactRef
	if err := json.Unmarshal([]byte(legacy), &ref); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(ref)
	if err != nil || string(encoded) != legacy {
		t.Fatalf("old artifact bytes changed: %s %v", encoded, err)
	}
}
