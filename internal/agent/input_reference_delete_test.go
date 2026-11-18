package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInputReferenceDeletionRetainsRawIndexesAndFailsBeforeCommit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, operation := range []string{"evidence", "run"} {
			for _, mode := range []string{"missing", "raw-reference", "directory-error", "file-error"} {
				t.Run(backend+"/"+operation+"/"+mode, func(t *testing.T) {
					_, store, snap, ref := catalogFixture(t)
					if backend == "sqlite" {
						if err := MigrateStoreToSQLite(store.root); err != nil {
							t.Fatal(err)
						}
						var err error
						store, err = NewStore(store.root)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { store.Close() })
					}
					inputs := filepath.Join(store.root, "inputs")
					if _, err := os.Stat(inputs); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("fixture unexpectedly has an input index", err)
					}
					switch mode {
					case "raw-reference", "file-error":
						if err := os.MkdirAll(filepath.Join(inputs, "ignored-subdirectory"), 0o700); err != nil {
							t.Fatal(err)
						}
						if mode == "raw-reference" {
							// Unknown/non-JSON index data must still protect a shared hash.
							if err := os.WriteFile(filepath.Join(inputs, "opaque-index"), []byte("unknown reference: "+ref.Hash), 0o600); err != nil {
								t.Fatal(err)
							}
						} else if err := os.Symlink(filepath.Join(inputs, "absent-target"), filepath.Join(inputs, "broken-index")); err != nil {
							t.Fatal(err)
						}
					case "directory-error":
						if err := os.WriteFile(inputs, []byte("not a directory"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					before, err := store.Load(snap.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					beforeJSON, err := json.Marshal(before)
					if err != nil {
						t.Fatal(err)
					}
					hashes := fixtureCASFiles(t, store)
					var result EvidenceDeleteResult
					if operation == "run" {
						result, err = store.PurgeRun(snap.Run.ID)
					} else {
						result, err = store.DeleteEvidence(snap.Run.ID, snap.Evidence[0].ID)
					}
					if mode == "directory-error" || mode == "file-error" {
						if err == nil {
							t.Fatal("input read failure did not stop deletion")
						}
						var pathErr *os.PathError
						if !errors.As(err, &pathErr) {
							t.Fatal("input read failure lost its cause", err)
						}
						after, err := store.Load(snap.Run.ID)
						if err != nil {
							t.Fatal(err)
						}
						afterJSON, err := json.Marshal(after)
						if err != nil || !bytes.Equal(beforeJSON, afterJSON) {
							t.Fatal("read failure changed durable history", err)
						}
						for _, hash := range hashes {
							if _, err := os.Stat(filepath.Join(store.root, "artifacts", hash)); err != nil {
								t.Fatal("read failure removed CAS", hash, err)
							}
						}
						return
					}
					if err != nil || !result.Deleted {
						t.Fatalf("deletion: %+v %v", result, err)
					}
					_, artifactErr := os.Stat(filepath.Join(store.root, "artifacts", ref.Hash))
					if mode == "raw-reference" {
						if artifactErr != nil || result.ArtifactsRetained < 1 {
							t.Fatal("raw input reference lost shared CAS", result, artifactErr)
						}
					} else if !errors.Is(artifactErr, os.ErrNotExist) {
						t.Fatal("missing inputs prevented exclusive CAS collection", artifactErr)
					}
				})
			}
		}
	}
}
