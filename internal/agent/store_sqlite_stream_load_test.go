package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSQLiteStreamLoadMatchesUnmarshal(t *testing.T) {
	s := sqliteTestStore(t)
	created, err := s.Create("stream semantics", "", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.sqliteDB()
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	id := created.Run.ID
	run, err := json.Marshal(Run{ID: id, Goal: "column"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, state, run string }{
		{"object", `{}`, ""},
		{"null", " \r\n\t null \t\r\n", ""},
		{"arrays", `{"messages":[],"requests":null,"events":[{"sequence":7,"data":{"n":2.5}}]}`, ""},
		{"duplicate-elements", `{"messages":[{"id":"first","content":"retained"},{"id":"gone"}],"messages":[{"id":"second"}]}`, ""},
		{"null-array", `{"messages":[{"content":"gone"}],"messages":null}`, ""},
		{"empty-array", `{"messages":[{"content":"gone"}],"messages":[]}`, ""},
		{"case-and-unknown", `{"MESSAGES":[{"content":"中文𠮷"}],"unknown":{"deep":[true,null,2]}}`, ""},
		{"unknown-large-number", `{"future":1e1000,"prompt":"after"}`, ""},
		{"unknown-nested-large-number", `{"future":[1e1000,{"n":-1e1000},2],"events":[{"data":1.25}]}`, ""},
		{"run-merge", `{"run":{"id":"state-id","error":"retained","goal":"state"}}`, ""},
		{"run-null", `{"run":{"id":"` + id + `","goal":"state"}}`, `null`},
		{"run-empty", `{"run":{"id":"` + id + `","goal":"state"}}`, `{}`},
		{"wrong-root", `[]`, ""},
		{"wrong-root-large-number", `1e1000`, ""},
		{"wrong-array", `{"messages":{}}`, ""},
		{"wrong-type", `{"requests":[{"provider_model":42}]}`, ""},
		{"bad-unknown", `{"unknown":[1,]}`, ""},
		{"bad-large-number", `{"future":1e1000x}`, ""},
		{"bad-number-exponent", `{"future":1e+}`, ""},
		{"bad-large-number-separator", `{"future":[1e1000 2]}`, ""},
		{"known-large-number", `{"events":[{"data":1e1000}]}`, ""},
		{"trailing", `{} {}`, ""},
		{"null-trailing", `null true`, ""},
		{"null-incomplete", `nul`, ""},
		{"empty", " \t\n", ""},
		{"wrong-id", `{}`, `{"id":"other"}`},
		{"bad-run", `{}`, `{"id":42}`},
	}
	for _, kind := range []string{"blob", "text"} {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				rawRun := string(run)
				if tc.run != "" {
					rawRun = tc.run
				}
				var state any = tc.state
				if kind == "blob" {
					state = []byte(tc.state)
				}
				if _, err := db.Exec("UPDATE runs SET state=?,run=? WHERE id=?", state, []byte(rawRun), id); err != nil {
					t.Fatal(err)
				}
				var want Snapshot
				wantErr := json.Unmarshal([]byte(tc.state), &want)
				if wantErr == nil {
					wantErr = json.Unmarshal([]byte(rawRun), &want.Run)
				}
				if wantErr == nil && want.Run.ID != id {
					wantErr = errors.New("identity mismatch")
				}
				got, err := s.sqliteSnapshot(id, false)
				if (err == nil) != (wantErr == nil) {
					t.Fatalf("different acceptance from Unmarshal: got=%v want=%v", err, wantErr)
				}
				got.artifactRoot = ""
				if err == nil && !reflect.DeepEqual(got, want) {
					t.Fatalf("different snapshot from Unmarshal: got=%+v want=%+v", got, want)
				}
				// A parse error must release both its Rows and transaction. One
				// connection makes leaked ownership block this next read.
				var stored string
				if err := db.QueryRow("SELECT state FROM runs WHERE id=?", id).Scan(&stored); err != nil || stored != tc.state {
					t.Fatal("read changed source or retained SQL ownership", err)
				}
			})
		}
	}
	if _, err := s.sqliteSnapshot("missing", false); !os.IsNotExist(err) {
		t.Fatalf("missing state changed error: %v", err)
	}
}

func TestSQLiteStreamLoadChunkBoundaryAndTransaction(t *testing.T) {
	s := sqliteTestStore(t)
	created, err := s.Create("old", "", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.sqliteDB()
	if err != nil {
		t.Fatal(err)
	}
	// Split a four-byte UTF-8 rune across the authoritative chunk boundary.
	const prefix = `{"messages":[{"content":"`
	content := strings.Repeat("x", sqliteDisplayChunkBytes-len(prefix)-2) + "𠮷旧正文"
	state := prefix + content + `"}],"requests":[{"id":"archived","context_manifest":{"sha256":"original"}}]}`
	for _, kind := range []string{"blob", "text"} {
		t.Run(kind, func(t *testing.T) {
			var value any = state
			if kind == "blob" {
				value = []byte(state)
			}
			oldRun, err := json.Marshal(Run{ID: created.Run.ID, Goal: "old"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE runs SET state=?,run=? WHERE id=?", value, oldRun, created.Run.ID); err != nil {
				t.Fatal(err)
			}
			tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			// Establish the read snapshot before a separate connection commits.
			var pinned []byte
			if err := tx.QueryRow("SELECT run FROM runs WHERE id=?", created.Run.ID).Scan(&pinned); err != nil {
				t.Fatal(err)
			}
			newRun, err := json.Marshal(Run{ID: created.Run.ID, Goal: "new"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE runs SET state=?,run=? WHERE id=?", []byte(`{"messages":[{"content":"new"}]}`), newRun, created.Run.ID); err != nil {
				t.Fatal(err)
			}
			got, err := sqliteReadState(tx, created.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Run.Goal != "old" || len(got.Messages) != 1 || got.Messages[0].Content != content || len(got.Requests) != 1 || got.Requests[0].ContextManifest == nil || got.Requests[0].ContextManifest.Hash != "original" {
				t.Fatal("state chunks or run escaped the original transaction")
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			latest, err := s.sqliteSnapshot(created.Run.ID, false)
			if err != nil || latest.Run.Goal != "new" || len(latest.Messages) != 1 || latest.Messages[0].Content != "new" {
				t.Fatal("next read failed to observe committed authority", err)
			}
		})
	}
}

func TestSQLiteStreamLoadEventsKeepRawContractAndOwnership(t *testing.T) {
	s := sqliteTestStore(t)
	created, err := s.Create("events", "", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.sqliteDB()
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	state := `{"events":[{"sequence":77,"type":"legacy-state-event"}]}`
	if _, err := db.Exec("UPDATE runs SET state=? WHERE id=?", []byte(state), created.Run.ID); err != nil {
		t.Fatal(err)
	}
	raw := []string{
		` { "sequence": 1, "type":"first", "data":{"text":"` + strings.Repeat("第一条𠮷", 1000) + `","number":1.25} } `,
		`null`,
		`{"sequence":3,"type":"last","data":[null,true,"尾部"]}`,
	}
	// Insert out of order; the raw API orders by SQL sequence and preserves bytes.
	for _, i := range []int{2, 0, 1} {
		if _, err := db.Exec("INSERT INTO events(run_id,sequence,body) VALUES(?,?,?)", created.Run.ID, i+1, []byte(raw[i])); err != nil {
			t.Fatal(err)
		}
	}
	var want Snapshot
	if err := json.Unmarshal([]byte(state), &want); err != nil {
		t.Fatal(err)
	}
	for _, body := range raw {
		var event Event
		if err := json.Unmarshal([]byte(body), &event); err != nil {
			t.Fatal(err)
		}
		want.Events = append(want.Events, event)
	}
	got, err := s.sqliteSnapshot(created.Run.ID, true)
	if err != nil || !reflect.DeepEqual(got.Events, want.Events) {
		t.Fatal("stream changed typed events or state-event append semantics", err)
	}
	_, batch, err := s.sqliteEventBatch(created.Run.ID, 0)
	if err != nil || len(batch) != len(raw) {
		t.Fatal("raw event API changed", err)
	}
	for i := range batch {
		if string(batch[i]) != raw[i] {
			t.Fatal("raw event bytes were normalized")
		}
	}
	if _, err := db.Exec("UPDATE events SET body=? WHERE run_id=? AND sequence=2", []byte(`{"data":`), created.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sqliteSnapshot(created.Run.ID, true); err == nil {
		t.Fatal("accepted malformed event")
	}
	withoutEvents, err := s.sqliteSnapshot(created.Run.ID, false)
	if err != nil || len(withoutEvents.Events) != 1 {
		t.Fatal("error retained rows or no-events read decoded the event table", err)
	}
	if !reflect.DeepEqual(got.Events, want.Events) {
		t.Fatal("returned events retained reused driver bytes")
	}
}
