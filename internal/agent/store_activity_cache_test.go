package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func activityCacheStore(t *testing.T, backend string) *Store {
	t.Helper()
	if backend == "sqlite" {
		return sqliteTestStore(t)
	}
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestActivityCacheGenerationAndCurrentRun(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			snap, err := s.Create("cache", "private", "hash")
			if err != nil {
				t.Fatal(err)
			}
			snap, err = s.Update(snap.Run.ID, func(v *Snapshot) error {
				v.Run.Title = "old title"
				v.Messages = []Message{{Role: "user", Content: "old"}}
				v.Actions = []Action{{CallID: "uncertain", Status: "uncertain", Result: &ToolResult{OutcomeUncertain: true}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			first, err := s.ActivityWorkspace(snap.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			entry := s.activityCache.entries[snap.Run.ID]
			if entry == nil {
				t.Fatal("small projection was not cached")
			}
			s.ReleaseEventView(snap.Run.ID)
			if _, ok := s.eventViews.Load(snap.Run.ID); ok {
				t.Fatal("cache retained the full event view")
			}
			again, err := s.ActivityWorkspace(snap.Run.ID)
			if err != nil || !reflect.DeepEqual(first, again) || s.activityCache.entries[snap.Run.ID] != entry {
				t.Fatal("same generation was not reused", err)
			}
			again.Snapshot.Messages[0].Content = "caller mutation"
			again.Totals["messages"] = -1
			if err = s.AppendEvents(snap.Run.ID, []Event{{Type: "model_stream", Data: "delta"}}); err != nil {
				t.Fatal(err)
			}
			latest, err := s.ActivityWorkspace(snap.Run.ID)
			if err != nil || latest.Snapshot.Run.EventSeq != snap.Run.EventSeq+1 || latest.Snapshot.Messages[0].Content != "old" || latest.Totals["messages"] != 1 || latest.Snapshot.Actions[0].Status != "uncertain" {
				t.Fatal("telemetry or detached window mismatch", err)
			}
			if backend == "sqlite" && s.activityCache.entries[snap.Run.ID] != entry {
				t.Fatal("unchanged SQLite state rebuilt projection")
			}
			other, err := NewStore(s.root)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			_, err = other.Update(snap.Run.ID, func(v *Snapshot) error {
				v.Run.Title = ""
				v.Messages[0].Content = "replacement"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			latest, err = s.ActivityWorkspace(snap.Run.ID)
			if err != nil || latest.Snapshot.Messages[0].Content != "replacement" || latest.Snapshot.Run.Title != "" {
				t.Fatal("cross-Store generation stayed stale", err)
			}
			// A Run-only change leaves SQLite state unchanged, including omitempty fields.
			_, err = other.Update(snap.Run.ID, func(v *Snapshot) error { v.Run.Title = "temporary"; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ActivityWorkspace(snap.Run.ID); err != nil {
				t.Fatal(err)
			}
			_, err = other.Update(snap.Run.ID, func(v *Snapshot) error { v.Run.Title = ""; return nil })
			if err != nil {
				t.Fatal(err)
			}
			latest, err = s.ActivityWorkspace(snap.Run.ID)
			if err != nil || latest.Snapshot.Run.Title != "" {
				t.Fatal("cached Run retained omitted field", err)
			}
		})
	}
}

func TestActivityCacheExternalMalformedHistoryAndIdentity(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, field := range []string{"context_manifest", "provider_model", "id"} {
			t.Run(backend+"/"+field, func(t *testing.T) {
				s := activityCacheStore(t, backend)
				snap, err := s.Create("external", "", "")
				if err != nil {
					t.Fatal(err)
				}
				snap, err = s.Update(snap.Run.ID, func(v *Snapshot) error {
					v.Requests = []ModelRequest{{ID: "old"}, {ID: "latest"}}
					v.Responses = []ResponseRecord{{RequestID: "old"}, {RequestID: "latest"}}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.ActivityWorkspace(snap.Run.ID); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(snap)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err = json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				fields["requests"] = json.RawMessage(`[{"id":"old","` + field + `":42},{"id":"latest"}]`)
				if field == "id" {
					fields["requests"] = json.RawMessage(`[{"id":42},{"id":"latest"}]`)
				}
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if backend == "sqlite" {
					db, e := s.sqliteDB()
					if e != nil {
						t.Fatal(e)
					}
					if _, err = db.Exec("UPDATE runs SET state=? WHERE id=?", raw, snap.Run.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					path := filepath.Join(s.root, snap.Run.ID+".json")
					info, e := os.Stat(path)
					if e != nil {
						t.Fatal(e)
					}
					tmp := path + ".replacement"
					if err = os.WriteFile(tmp, raw, 0600); err != nil {
						t.Fatal(err)
					}
					if err = os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
					if err = os.Rename(tmp, path); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = s.ActivityWorkspace(snap.Run.ID); err == nil {
					t.Fatal("cached activity hid malformed historical typed metadata")
				}
			})
		}
	}
}

func TestActivityCacheJSONAtomicReplacementSameSizeAndMtime(t *testing.T) {
	s := activityCacheStore(t, "json")
	snap, err := s.Create("replacement", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(snap.Run.ID, func(v *Snapshot) error {
		v.Requests = []ModelRequest{{ID: "old", ProviderModel: "ok"}, {ID: "latest"}}
		v.Responses = []ResponseRecord{{RequestID: "old"}, {RequestID: "latest"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ActivityWorkspace(snap.Run.ID)
	if err != nil || len(p.Snapshot.Requests) != 1 || p.Snapshot.Requests[0].ID != "latest" {
		t.Fatal("historical request must be outside the display window", err)
	}
	entry := s.activityCache.entries[snap.Run.ID]
	if entry == nil {
		t.Fatal("replacement test requires a warm activity cache")
	}
	path := filepath.Join(s.root, snap.Run.ID+".json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Change only a historical field's type, preserving its encoded length.
	old := []byte(`"provider_model":"ok"`)
	if bytes.Count(raw, old) != 1 {
		t.Fatal("expected exactly one historical provider model")
	}
	raw = bytes.Replace(raw, old, []byte(`"provider_model":true`), 1)
	tmp := path + ".replacement"
	if err = os.WriteFile(tmp, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(tmp, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || os.SameFile(before, after) {
		t.Fatal("replacement must preserve size and mtime but change file identity")
	}
	if hit, prior, err := s.cachedJSONActivity(snap.Run.ID); err != nil || hit != nil || prior != entry {
		t.Fatal("changed file identity reused the old activity projection", err)
	}
	_, err = s.ActivityWorkspace(snap.Run.ID)
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) || !strings.HasSuffix(typeErr.Field, "provider_model") {
		t.Fatal("replacement bypassed historical typed validation", err)
	}
}

func TestActivityCacheDeletePurgeAndOversize(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			for _, remove := range []string{"delete", "purge"} {
				snap, err := s.Create(remove, "", "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.ActivityWorkspace(snap.Run.ID); err != nil {
					t.Fatal(err)
				}
				if remove == "delete" {
					err = s.Delete(snap.Run.ID)
				} else {
					_, err = s.PurgeRun(snap.Run.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.ActivityWorkspace(snap.Run.ID); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("cached removed Run remained visible", err)
				}
			}
			snap, err := s.Create("large", "", "")
			if err != nil {
				t.Fatal(err)
			}
			body := strings.Repeat("x", activityCacheMaxEntry+1)
			_, err = s.Update(snap.Run.ID, func(v *Snapshot) error { v.Messages = []Message{{Role: "user", Content: body}}; return nil })
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.ActivityWorkspace(snap.Run.ID)
			if err != nil || len(p.Snapshot.Messages[0].Content) != 4096 || !p.MessagePreviews[p.Snapshot.Messages[0].ID].Preview || p.MessagePreviews[p.Snapshot.Messages[0].ID].ContentBytes != len(body) {
				t.Fatal("oversized message lost explicit preview metadata", err)
			}
			if s.activityCache.entries[snap.Run.ID] == nil {
				t.Fatal("bounded text preview missed cache")
			}
			full, err := s.Load(snap.Run.ID)
			if err != nil || full.Messages[0].Content != body {
				t.Fatal("preview changed original message", err)
			}
			large, err := s.Create(body, "", "")
			if err != nil {
				t.Fatal(err)
			}
			p, err = s.ActivityWorkspace(large.Run.ID)
			if err != nil || p.Snapshot.Run.Goal != body || s.activityCache.entries[large.Run.ID] != nil {
				t.Fatal("non-preview metadata bypassed cache item cap", err)
			}
		})
	}
}

func TestActivityCacheBoundsAndLatePublication(t *testing.T) {
	s := &Store{sqlite: true}
	key := activityCacheKey{state: sha256.Sum256([]byte("state"))}
	for i := 0; i < activityCacheMaxEntries; i++ {
		s.cacheActivity(fmt.Sprint(i), key, nil, []byte("small"), nil)
	}
	if hit, _ := s.cachedActivity("0", key); hit == nil {
		t.Fatal("missing cache entry")
	}
	s.cacheActivity("new", key, nil, []byte("small"), nil)
	if len(s.activityCache.entries) != activityCacheMaxEntries || s.activityCache.entries["0"] == nil || s.activityCache.entries["1"] != nil {
		t.Fatal("count cap or LRU failed")
	}
	for i := 0; i < 10; i++ {
		s.cacheActivity(fmt.Sprint("large", i), key, nil, make([]byte, activityCacheMaxEntry), nil)
	}
	if s.activityCache.bytes > activityCacheMaxBytes || len(s.activityCache.entries) != activityCacheMaxBytes/activityCacheMaxEntry {
		t.Fatal("byte cap failed")
	}
	old := activityCacheKey{state: sha256.Sum256([]byte("old"))}
	s.cacheActivity("late", old, nil, []byte("old"), nil)
	_, prior := s.cachedActivity("late", key)
	s.cacheActivity("late", key, prior, []byte("new"), nil)
	s.cacheActivity("late", old, prior, []byte("late old"), nil)
	if hit, _ := s.cachedActivity("late", key); hit == nil || string(hit.data) != "new" {
		t.Fatal("late build replaced a newer cache entry")
	}
	jsonStore := activityCacheStore(t, "json")
	snap, err := jsonStore.Create("old", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := detachedPublicationView(t, jsonStore, snap.Run.ID)
	_, err = jsonStore.Update(snap.Run.ID, func(v *Snapshot) error { v.Run.Goal = "new"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	jsonStore.cacheActivity(snap.Run.ID, activityCacheKey{info: v.info}, nil, []byte("old"), nil)
	if jsonStore.activityCache.entries[snap.Run.ID] != nil {
		t.Fatal("late JSON build published an obsolete generation")
	}
}

func TestActivityCacheConcurrentSemanticCuts(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			snap, err := s.Create("v0", "", "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Update(snap.Run.ID, func(v *Snapshot) error { v.Messages = []Message{{Role: "user", Content: "v0"}}; return nil })
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for reader := 0; reader < 4; reader++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < 24; i++ {
						p, e := s.ActivityWorkspace(snap.Run.ID)
						if e != nil || len(p.Snapshot.Messages) != 1 || p.Snapshot.Run.Goal != p.Snapshot.Messages[0].Content {
							t.Error("mixed semantic cut", e)
							return
						}
					}
				}()
			}
			for i := 1; i <= 12; i++ {
				_, err = s.Update(snap.Run.ID, func(v *Snapshot) error {
					v.Run.Goal = fmt.Sprint("v", i)
					v.Messages[0].Content = v.Run.Goal
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			wg.Wait()
		})
	}
}

func TestActivityCacheReadOnlyAndFailedCommit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			snap, err := s.Create("committed", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Load(snap.Run.ID); err != nil {
				t.Fatal(err)
			}
			authority := func() []byte {
				if backend == "json" {
					raw, e := os.ReadFile(filepath.Join(s.root, snap.Run.ID+".json"))
					if e != nil {
						t.Fatal(e)
					}
					return raw
				}
				db, e := s.sqliteDB()
				if e != nil {
					t.Fatal(e)
				}
				var state, run []byte
				if e = db.QueryRow("SELECT state,run FROM runs WHERE id=?", snap.Run.ID).Scan(&state, &run); e != nil {
					t.Fatal(e)
				}
				return append(state, run...)
			}
			before := authority()
			for i := 0; i < 8; i++ {
				if _, err = s.ActivityWorkspace(snap.Run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(before, authority()) {
				t.Fatal("display reads changed authoritative bytes")
			}
			entry := s.activityCache.entries[snap.Run.ID]
			var restore func()
			if backend == "json" {
				s.beforePersist = func(string) { restore = dispatchBlockRename(t, s, snap.Run.ID) }
			}
			_, err = s.Update(snap.Run.ID, func(v *Snapshot) error {
				v.Run.Goal = "uncommitted"
				v.Messages = []Message{{Role: "user", Content: "uncommitted"}}
				if backend == "sqlite" {
					addEvent(v, "test", "duplicate sequence rollback", nil)
					v.Events = append(v.Events, v.Events[len(v.Events)-1])
				}
				return nil
			})
			s.beforePersist = nil
			if restore != nil {
				restore()
			}
			if err == nil {
				t.Fatal("injected commit failure succeeded")
			}
			p, err := s.ActivityWorkspace(snap.Run.ID)
			if err != nil || p.Snapshot.Run.Goal != "committed" || s.activityCache.entries[snap.Run.ID] != entry || !bytes.Equal(before, authority()) {
				t.Fatal("failed commit changed activity generation", err)
			}
		})
	}
}

func TestActivityCacheSQLiteRunIdentityOnStateHit(t *testing.T) {
	s := sqliteTestStore(t)
	snap, err := s.Create("identity", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ActivityWorkspace(snap.Run.ID); err != nil {
		t.Fatal(err)
	}
	db, err := s.sqliteDB()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"id":"other"}`, `{"id":42}`, `{"id":"` + snap.Run.ID + `","deleted":true}`} {
		if _, err = db.Exec("UPDATE runs SET run=? WHERE id=?", []byte(raw), snap.Run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ActivityWorkspace(snap.Run.ID); err == nil {
			t.Fatal("state hash hit bypassed Run identity/deletion validation")
		}
	}
}
