package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"trace/internal/agent"
)

func TestCreateRequirementsFailureLeavesNoRun(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			store, err := agent.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			a, err := agent.New(store, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "requirements.json")
			if kind != "missing" {
				body := "["
				if kind == "invalid" {
					body = `[{"id":"proof"}]`
				}
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TRACE_REQUIREMENTS_FILE", path)
			if err := create(a, "must carry its acceptance contract"); err == nil {
				t.Fatal("invalid requirements accepted")
			}
			runs, err := store.List()
			if err != nil {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(root, "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 0 || len(files) != 0 {
				t.Fatal("failed creation left a run without its contract")
			}
		})
	}
}

func TestCreatePublishesRequirementsWithRun(t *testing.T) {
	store, err := agent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "requirements.json")
	body := `[{"id":"proof","description":"exact result","category":"goal","predicate":{"kind":"equals","value":"observed"}}]`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACE_REQUIREMENTS_FILE", path)
	if err := create(a, "atomic requirements"); err != nil {
		t.Fatal(err)
	}
	runs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	snap, err := store.Load(runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Requirements) != 1 || snap.Requirements[0].ID != "proof" {
		t.Fatal("requirements were not persisted")
	}
	if len(snap.Events) != 2 || snap.Events[0].Type != "run_created" || snap.Events[0].Sequence != 1 ||
		snap.Events[1].Type != "recon_prepared" || snap.Events[1].Sequence != 2 || snap.Run.EventSeq != 2 {
		t.Fatal("creation did not publish exactly the run and prepared recon events")
	}
	if snap.Run.Status != agent.StatusCreated || len(snap.Requests) != 0 || len(snap.Actions) != 0 {
		t.Fatal("creation performed execution before publishing the acceptance contract")
	}
	if snap.RootContract == nil || snap.RootContract.Goal != snap.Run.Goal ||
		!reflect.DeepEqual(snap.RootContract.RequirementIDs, []string{"proof"}) {
		t.Fatal("creation omitted the fixed root acceptance contract")
	}
	contract, err := json.Marshal(snap.RootContract)
	if err != nil {
		t.Fatal(err)
	}
	var canonical map[string]any
	if err := json.Unmarshal(contract, &canonical); err != nil {
		t.Fatal(err)
	}
	contract, err = json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Recon == nil || snap.Recon.SourceRunID != snap.Run.ID || snap.Recon.Digest == "" ||
		snap.Recon.RootContractHash != fmt.Sprintf("%x", sha256.Sum256(contract)) {
		t.Fatal("creation recon was not bound to the published root contract")
	}
	data, err := json.Marshal(snap.Events[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Requirements []agent.VerificationRequirement `json:"requirements"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(event.Requirements, snap.Requirements) {
		t.Fatal("creation event omitted the acceptance contract")
	}
	data, err = json.Marshal(snap.Events[1].Data)
	if err != nil {
		t.Fatal(err)
	}
	var reconEvent struct {
		Digest           string `json:"digest"`
		RootContractHash string `json:"root_contract_hash"`
	}
	if err := json.Unmarshal(data, &reconEvent); err != nil {
		t.Fatal(err)
	}
	if reconEvent.Digest != snap.Recon.Digest || reconEvent.RootContractHash != snap.Recon.RootContractHash {
		t.Fatal("creation event omitted the prepared recon identity")
	}
}
