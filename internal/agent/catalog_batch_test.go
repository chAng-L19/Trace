package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestChainParentAdjacentSourcesRevalidateCurrentCAS(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			_, store, source, artifact := catalogFixture(t)
			if backend == "sqlite" {
				if err := MigrateStoreToSQLite(store.root); err != nil {
					t.Fatal(err)
				}
				migrated, err := NewStore(store.root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { migrated.Close() })
				store = migrated
			}
			refs := []EvidenceChainReference{}
			for _, name := range []string{"first parent", "adjacent parent"} {
				entry, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: name, Summary: "same source ledger", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
				if err != nil {
					t.Fatal(err)
				}
				refs = append(refs, EvidenceChainReference{SourceRunID: source.Run.ID, ChainID: entry.Chain.ID, ChainHash: entry.Chain.ContentHash})
			}
			read := func() string {
				unlock, err := store.acquireStoreLock()
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				return store.chainParentStatusLocked(refs)
			}
			if got := read(); got != "verified" {
				t.Fatal("valid adjacent parents:", got)
			}
			path := filepath.Join(store.root, "artifacts", artifact.Hash)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("corrupt current CAS"), 0600); err != nil {
				t.Fatal(err)
			}
			if got := read(); got != "invalid" {
				t.Fatal("corrupt CAS accepted:", got)
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if got := read(); got != "verified" {
				t.Fatal("restored CAS not revalidated:", got)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if got := read(); got != "invalid" {
				t.Fatal("missing CAS accepted:", got)
			}
		})
	}
}

func TestEvidenceCatalogBatchMatchesDetailSources(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := Snapshot{
		Run:     Run{ID: "owner", Goal: "catalog source identities", CreatedAt: now},
		Events:  []Event{{Sequence: 7, CreatedAt: now.Add(time.Second)}, {Sequence: 7, CreatedAt: now.Add(2 * time.Second)}, {Sequence: 8}},
		Actions: []Action{{RequestID: "r1", CallID: "same", Name: "first"}, {RequestID: "r1", CallID: "same", Name: "duplicate"}, {RequestID: "r2", CallID: "same", Name: "other request"}},
		Evidence: []Evidence{
			{ID: "first", RequestID: "r1", CallID: "same", SourceEventSequence: 7},
			{ID: "other", RequestID: "r2", CallID: "same", SourceEventSequence: 8},
			{ID: "missing", RequestID: "absent", CallID: "same", SourceEventSequence: 99},
		},
	}
	for len(s.Evidence) < 17 {
		s.Evidence = append(s.Evidence, s.Evidence[len(s.Evidence)%3])
	}
	for _, n := range []int{0, 1, 3, len(s.Evidence)} {
		input := s
		input.Evidence = s.Evidence[:n]
		entries := catalogEntries(input)
		if len(entries) != n {
			t.Fatalf("entries=%d want=%d", len(entries), n)
		}
		for i, e := range input.Evidence {
			want := catalogEntry(input, e)
			if !reflect.DeepEqual(entries[i], want) {
				t.Fatalf("entry %d: got=%+v want=%+v", i, entries[i], want)
			}
			gotJSON, _ := json.Marshal(entries[i])
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("entry JSON differs: %s != %s", gotJSON, wantJSON)
			}
		}
	}
	if got := catalogEntries(Snapshot{}); len(got) != 0 {
		t.Fatal("empty catalog gained entries")
	}
}

// Both arms use this fixture; the baseline test-only batch helper reproduces
// its original catalogEntry loop. Measures assembly, not provenance or disk I/O.
func BenchmarkEvidenceCatalogEntries(b *testing.B) {
	for _, count := range []int{10, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := Snapshot{Run: Run{ID: "benchmark", Goal: "catalog", CreatedAt: time.Unix(1700000000, 0)}}
			for i := 0; i < count*10; i++ {
				s.Events = append(s.Events, Event{Sequence: i, CreatedAt: s.Run.CreatedAt})
			}
			for i := 0; i < count; i++ {
				call := fmt.Sprint(i)
				s.Actions = append(s.Actions, Action{RequestID: "request", CallID: call})
				s.Evidence = append(s.Evidence, Evidence{ID: call, RequestID: "request", CallID: call, SourceEventSequence: count*9 + i})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if len(catalogEntries(s)) != count {
					b.Fatal("incomplete catalog")
				}
			}
		})
	}
}
