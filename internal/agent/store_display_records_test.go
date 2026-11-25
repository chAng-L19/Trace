package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func displayRecordFixture(id string, legacy bool) Snapshot {
	snap := Snapshot{Run: Run{ID: id, Status: StatusPaused}, Plan: PlanState{Capacity: 5}}
	for i := 0; i < 105; i++ {
		rid, mid := fmt.Sprint("r", i), fmt.Sprint("m", i)
		text := strings.Repeat("正文𠮷", 1100) + fmt.Sprint(i)
		m := Message{ID: mid, Role: "assistant", RequestID: rid, Content: text, ToolCalls: []ToolCall{{ID: "shared", Name: "exec", Arguments: fmt.Sprint(i)}}}
		snap.Requests = append(snap.Requests, ModelRequest{ID: rid, Messages: []Message{{ID: fmt.Sprint("m", i-1), Content: strings.Repeat("private duplicated context", 1024)}}})
		snap.Responses = append(snap.Responses, ResponseRecord{RequestID: rid, Response: ModelResponse{Content: text, ToolCalls: m.ToolCalls}})
		snap.Actions = append(snap.Actions, Action{RequestID: rid, CallID: "shared", Adapter: "exec", Status: "completed", Result: &ToolResult{Stdout: text}})
		if legacy {
			m.RequestID = ""
		}
		snap.Messages = append(snap.Messages, m)
	}
	return snap
}

func TestDisplayRecordIndexPagesMatchOriginalProjection(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		snap := displayRecordFixture("fixture", legacy)
		raw, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		bodies := map[string]map[int][]byte{}
		index, err := buildDisplayRecordIndex(bytes.NewReader(raw), func(kind string, ordinal int, body []byte) error {
			if bodies[kind] == nil {
				bodies[kind] = map[int][]byte{}
			}
			bodies[kind][ordinal] = append([]byte(nil), body...)
			return nil
		}, func(kind string) error { delete(bodies, kind); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(index.Fields["messages"], []byte("正文")) || bytes.Contains(index.Fields["requests"], []byte("private duplicated context")) || bytes.Contains(index.Fields["actions"], []byte("正文")) {
			t.Fatal("index retained historical bodies")
		}
		for _, query := range []MessagePageQuery{{}, {Before: "m65"}, {After: "m65", Limit: 80}, {Around: "m44", Limit: 5}, {Request: "r20", Call: "shared"}} {
			original, records, err := readActivityFields(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			want, err := buildActivityPage(original.fields, records, query, "messages")
			if err != nil {
				t.Fatal(err)
			}
			trimActivityDisplay(&want)
			reads := map[string][]int{}
			selected := index.records(func(kind string, ordinal int) ([]byte, error) {
				reads[kind] = append(reads[kind], ordinal)
				return bodies[kind][ordinal], nil
			})
			got, err := buildActivityPage(index.Fields, selected, query, "messages")
			if err != nil {
				t.Fatal(err)
			}
			trimActivityDisplay(&got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("indexed projection changed legacy=%v query=%+v", legacy, query)
			}
			if len(reads["messages"]) != got.MessagesPage.End-got.MessagesPage.Start {
				t.Fatal("page read unselected message body")
			}
			for _, ordinal := range reads["messages"] {
				if ordinal < got.MessagesPage.Start || ordinal >= got.MessagesPage.End {
					t.Fatal("body outside requested page")
				}
			}
		}
	}
}

func TestDisplayRecordIndexColdReopenAndLegacyBackfill(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			created, err := s.Create("record index", "", "")
			if err != nil {
				t.Fatal(err)
			}
			fixture := displayRecordFixture(created.Run.ID, false)
			before, err := s.Update(created.Run.ID, func(v *Snapshot) error {
				v.Messages, v.Requests, v.Responses, v.Actions = fixture.Messages, fixture.Requests, fixture.Responses, fixture.Actions
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			root := s.root
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			cold, err := NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			defer cold.Close()
			for legacy := 0; legacy < 2; legacy++ {
				if legacy == 1 {
					if backend == "json" {
						err = os.Remove(filepath.Join(root, created.Run.ID+".display"))
					} else {
						_, err = cold.sqlitePool.Exec("DELETE FROM run_display_index WHERE run_id=?", created.Run.ID)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				p, err := cold.MessageWorkspace(context.Background(), created.Run.ID, MessagePageQuery{Before: "m65"})
				if err != nil || len(p.Snapshot.Messages) != 40 || p.Snapshot.Messages[0].ID != "m25" {
					t.Fatal("cold page/backfill", err)
				}
				if _, err = cold.PlannerWorkspace(context.Background(), created.Run.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = cold.workerActivityContext(context.Background(), created.Run.ID); err != nil {
					t.Fatal(err)
				}
				if backend == "json" {
					f, err := os.Open(filepath.Join(root, created.Run.ID+".json"))
					if err != nil {
						t.Fatal(err)
					}
					generation, err := displayGeneration(f)
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := readJSONDisplayIndex(filepath.Join(root, created.Run.ID+".display"), generation); err != nil {
						t.Fatal("missing committed index", err)
					}
				} else {
					var total, bytes int
					if err := cold.sqlitePool.QueryRow("SELECT count(*),sum(length(body)) FROM run_display_records WHERE run_id=?", created.Run.ID).Scan(&total, &bytes); err != nil {
						t.Fatal(err)
					}
					if total != 315 || bytes > 3<<20 {
						t.Fatal("projection duplicated complete history", total, bytes)
					}
				}
			}
			after, err := cold.Load(created.Run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("display backfill changed authority", err)
			}
		})
	}
}

func TestDisplayRecordIndexJSONDuplicatesCorruptionAndTelemetryOffsets(t *testing.T) {
	for _, eventsFirst := range []bool{false, true} {
		s := activityCacheStore(t, "json")
		created, err := s.Create("offsets", "", "")
		if err != nil {
			t.Fatal(err)
		}
		run, _ := json.Marshal(created.Run)
		prefix := `"run":` + string(run) + `,"events":null`
		if eventsFirst {
			prefix = `"events":null,"run":` + string(run)
		}
		raw := []byte(`{` + prefix + `,"messages":[{"id":"old","role":"user","content":"old"}],"messages":[{"id":"m","role":"user","content":"actual"}],"requests":[],"responses":[]}`)
		path := filepath.Join(s.root, created.Run.ID+".json")
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			if i == 1 {
				if err := os.WriteFile(filepath.Join(s.root, created.Run.ID+".display"), []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p, err := s.MessageWorkspace(context.Background(), created.Run.ID, MessagePageQuery{Around: "m"})
			if err != nil || len(p.Snapshot.Messages) != 1 || p.Snapshot.Messages[0].Content != "actual" {
				t.Fatal("duplicate/corrupt index projection", i, err)
			}
			if err = s.AppendEvents(created.Run.ID, []Event{{Type: "delta", Data: strings.Repeat("x", i*19)}}); err != nil {
				t.Fatal(err)
			}
		}
		// Canonical generations exercise the offset-only update repeatedly.
		if _, err := s.Update(created.Run.ID, func(v *Snapshot) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 12; i++ {
			if err := s.AppendEvents(created.Run.ID, []Event{{Type: "delta", Data: i}}); err != nil {
				t.Fatal(err)
			}
			p, err := s.MessageWorkspace(context.Background(), created.Run.ID, MessagePageQuery{Around: "m"})
			if err != nil || p.Snapshot.Messages[0].Content != "actual" {
				t.Fatal("canonical offset drift", i, err)
			}
		}
	}
}

func TestDisplayRecordIndexSQLiteExternalWritersInvalidate(t *testing.T) {
	s := sqliteTestStore(t)
	created, err := s.Create("external", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fixture := Snapshot{Run: Run{ID: created.Run.ID}, Messages: []Message{{ID: "m", Role: "user", Content: "first"}}}
	raw, _ := json.Marshal(fixture)
	for _, operation := range []string{"update", "replace", "delete_insert", "derived_update"} {
		if operation == "derived_update" {
			_, err = s.sqlitePool.Exec("UPDATE run_display_records SET body=? WHERE run_id=? AND kind='messages'", []byte(`{"id":"forged"}`), created.Run.ID)
		} else if operation == "update" {
			_, err = s.sqlitePool.Exec("UPDATE runs SET state=? WHERE id=?", raw, created.Run.ID)
		} else if operation == "replace" {
			_, err = s.sqlitePool.Exec("INSERT OR REPLACE INTO runs(id,run,state,observation) SELECT id,run,?,observation FROM runs WHERE id=?", raw, created.Run.ID)
		} else {
			conn, e := s.sqlitePool.Conn(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			_, e = conn.ExecContext(context.Background(), "PRAGMA foreign_keys=OFF")
			if e != nil {
				conn.Close()
				t.Fatal(e)
			}
			var run, observation []byte
			if e = conn.QueryRowContext(context.Background(), "SELECT run,observation FROM runs WHERE id=?", created.Run.ID).Scan(&run, &observation); e == nil {
				_, e = conn.ExecContext(context.Background(), "DELETE FROM runs WHERE id=?", created.Run.ID)
			}
			if e == nil {
				_, e = conn.ExecContext(context.Background(), "INSERT INTO runs(id,run,state,observation) VALUES(?,?,?,?)", created.Run.ID, run, raw, observation)
			}
			_, reset := conn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON")
			conn.Close()
			if e != nil {
				t.Fatal(e)
			}
			if reset != nil {
				t.Fatal(reset)
			}
		}
		if err != nil {
			t.Fatal(operation, err)
		}
		var count int
		if err = s.sqlitePool.QueryRow("SELECT count(*) FROM run_display_index WHERE run_id=?", created.Run.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("external write retained index", operation, err)
		}
		p, err := s.MessageWorkspace(context.Background(), created.Run.ID, MessagePageQuery{Around: "m"})
		if err != nil || len(p.Snapshot.Messages) != 1 || p.Snapshot.Messages[0].Content != "first" {
			t.Fatal("external rebuild", operation, err)
		}
	}
}

func TestDisplayRecordIndexCapacityDuplicateSemantics(t *testing.T) {
	for _, raw := range []string{
		`{"run":{"id":"r"},"plan":{"tasks":[{"id":"a","status":"uncertain"}]},"PLAN":{"TASKS":[{"worker_id":"w"}]}}`,
		`{"actions":[{"adapter":"exec","result":{}}],"actions":[{"status":"completed"}]}`,
		`{"actions":[{"adapter":"exec","result":{}}],"actions":[{"result":null}]}`,
		`{"workers":[{"id":"w","status":"uncertain"}],"workers":[],"workers":[{"cleanup_status":"completed"}]}`,
	} {
		index, err := buildDisplayRecordIndex(strings.NewReader(raw), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var want, got Snapshot
		budget := activityProjectionMaxBytes
		if err = readCapacityProjection(context.Background(), strings.NewReader(raw), &want, &budget); err != nil {
			t.Fatal(err)
		}
		budget = activityProjectionMaxBytes
		if err = readCapacityProjection(context.Background(), bytes.NewReader(index.Capacity), &got, &budget); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("capacity duplicate merge changed: got %+v want %+v", got, want)
		}
	}
}

func TestDisplayRecordIndexOversizeDoesNotRejectAuthority(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			created, err := s.Create("oversize display", "", "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Update(created.Run.ID, func(v *Snapshot) error {
				v.Messages = []Message{{ID: "m", Role: "user", Content: strings.Repeat("x", activityProjectionMaxBytes+1)}}
				return nil
			})
			if err != nil {
				t.Fatal("display limit rejected authoritative save", err)
			}
			if _, err = s.ActivityWorkspace(created.Run.ID); !errors.Is(err, errActivityTooLarge) {
				t.Fatal("oversize display escaped original limit", err)
			}
			full, err := s.Load(created.Run.ID)
			if err != nil || len(full.Messages) != 1 || len(full.Messages[0].Content) != activityProjectionMaxBytes+1 {
				t.Fatal("authority lost oversize message", err)
			}
		})
	}
}

func TestDisplayRecordIndexCanceledBuildRollsBack(t *testing.T) {
	s := sqliteTestStore(t)
	created, err := s.Create("cancel", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err = s.sqlitePool.QueryRow("SELECT data FROM run_display_index WHERE run_id=?", created.Run.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx, err := s.sqlitePool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = sqliteWriteDisplayIndex(ctx, tx, created.Run.ID, strings.NewReader(`{"messages":[]}`)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err = s.sqlitePool.QueryRow("SELECT data FROM run_display_index WHERE run_id=?", created.Run.ID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled build published partial index", err)
	}
}

func TestDisplayRecordIndexJSONReplacementSameSizeAndTimestamp(t *testing.T) {
	s := activityCacheStore(t, "json")
	created, err := s.Create("replacement", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(created.Run.ID, func(v *Snapshot) error { v.Messages = []Message{{ID: "m", Role: "user", Content: "old"}}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, created.Run.ID+".json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"content":"old"`), []byte(`"content":"new"`), 1)
	replacement := path + ".replacement"
	if err = os.WriteFile(replacement, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(replacement, time.Now(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	p, err := s.MessageWorkspace(context.Background(), created.Run.ID, MessagePageQuery{Around: "m"})
	if err != nil || p.Snapshot.Messages[0].Content != "new" {
		t.Fatal("inode replacement reused index", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(stored) != sha256.Sum256(raw) {
		t.Fatal("rebuild changed source", err)
	}
}

func TestDisplayRecordIndexJSONBackfillPublicationFence(t *testing.T) {
	for _, scenario := range []string{"publish", "busy", "purged", "replaced", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			s := activityCacheStore(t, "json")
			created, err := s.Create("backfill fence", "", "")
			if err != nil {
				t.Fatal(err)
			}
			id := created.Run.ID
			path := filepath.Join(s.root, id+".json")
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			generation, err := displayGeneration(f)
			if err != nil {
				t.Fatal(err)
			}
			index, err := buildDisplayRecordIndex(f, nil, nil)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			index.Generation = generation
			staged, err := s.stageJSONDisplayIndex(index)
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(staged)
			display := filepath.Join(s.root, id+".display")
			if err = os.Remove(display); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "busy":
				unlock, err := s.acquireStoreLock()
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			case "purged":
				// Pause a reader after staging, let deletion commit, then resume it.
				if _, err = s.PurgeRun(id); err != nil {
					t.Fatal(err)
				}
			case "replaced":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path+".replacement", raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(path+".replacement", path); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			s.publishJSONDisplayIndex(ctx, id, staged, generation, false)
			if scenario == "publish" {
				if _, err = readJSONDisplayIndex(display, generation); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(display)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("index permissions", err)
				}
			} else if _, err = os.Stat(display); !os.IsNotExist(err) {
				t.Fatal("stale or unfenced index was published", err)
			}
			if scenario == "purged" {
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("purge retained authority", err)
				}
				cold, err := NewStore(s.root)
				if err != nil {
					t.Fatal(err)
				}
				defer cold.Close()
				if _, err = cold.Load(id); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("deleted run reopened", err)
				}
				if _, err = os.Stat(display); !os.IsNotExist(err) {
					t.Fatal("reopen retained deleted index", err)
				}
			}

		})
	}
}
