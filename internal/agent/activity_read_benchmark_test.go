package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// This file intentionally uses only the pre-cache Store API so the identical
// fixture and benchmark can be copied into the baseline source tree.
func BenchmarkActivityWorkspaceCommittedRead(b *testing.B) {
	for _, requests := range []int{300, 1000} {
		b.Run(fmt.Sprintf("%dreq_50000events", requests), func(b *testing.B) {
			for _, backend := range []string{"json", "sqlite"} {
				b.Run(backend, func(b *testing.B) {
					root := b.TempDir()
					if backend == "sqlite" {
						if err := MigrateStoreToSQLite(root); err != nil {
							b.Fatal(err)
						}
					}
					store, err := NewStore(root)
					if err != nil {
						b.Fatal(err)
					}
					defer store.Close()
					snap, err := store.Create("activity read benchmark", "", "")
					if err != nil {
						b.Fatal(err)
					}
					snap, err = store.Update(snap.Run.ID, func(v *Snapshot) error {
						manifest := &ContextManifest{Boundary: "provider_messages_tools", Hash: strings.Repeat("a", 64)}
						for i := 0; i < 128; i++ {
							manifest.Parts = append(manifest.Parts, ContextManifestPart{Kind: "message", Index: i, ID: fmt.Sprint("m", i), Bytes: 1024, Hash: strings.Repeat("b", 64)})
						}
						for i := 0; i < requests; i++ {
							id := fmt.Sprint("r", i)
							v.Requests = append(v.Requests, ModelRequest{ID: id, ProviderID: "fixture", ProviderModel: "fixture", ContextManifest: manifest, Messages: []Message{{Content: strings.Repeat("context ", 128)}}})
							if i != 3 {
								v.Responses = append(v.Responses, ResponseRecord{RequestID: id, Response: ModelResponse{Content: "answer", Thinking: strings.Repeat("archived response ", 1024)}})
							}
						}
						v.Messages = nil
						for i := 0; i < 120; i++ {
							v.Messages = append(v.Messages, Message{ID: fmt.Sprint("m", i), Role: "assistant", RequestID: "r0", Content: "answer"})
						}
						v.Actions = []Action{{RequestID: "r1", Status: "uncertain", Result: &ToolResult{OutcomeUncertain: true}}, {RequestID: "r3", Status: "started"}}
						// Display records only: this benchmark never starts real Workers.
						for i := 0; i < 5; i++ {
							v.Workers = append(v.Workers, AgentWorker{ID: fmt.Sprint("fixture-worker-", i), Status: "running"})
						}
						v.Events = nil
						for i := 1; i <= 50000; i++ {
							v.Events = append(v.Events, Event{Sequence: i, Type: "model_stream", Data: map[string]string{"delta": strings.Repeat("stream ", 16)}})
						}
						v.Run.EventSeq = 50000
						return nil
					})
					if err != nil {
						b.Fatal(err)
					}
					raw, err := json.Marshal(snap)
					if err != nil {
						b.Fatal(err)
					}
					sourceBytes := len(raw)
					raw = nil
					for _, mode := range []string{"cold", "warm"} {
						b.Run(mode, func(b *testing.B) {
							read := store
							if mode == "warm" {
								if _, err := read.ActivityWorkspace(snap.Run.ID); err != nil {
									b.Fatal(err)
								}
								read.ReleaseEventView(snap.Run.ID)
							}
							b.ReportAllocs()
							b.ReportMetric(float64(sourceBytes), "source_bytes")
							b.ResetTimer()
							for i := 0; i < b.N; i++ {
								if mode == "cold" {
									b.StopTimer()
									read, err = NewStore(root)
									if err != nil {
										b.Fatal(err)
									}
									b.StartTimer()
								}
								p, e := read.ActivityWorkspace(snap.Run.ID)
								if e != nil || len(p.Snapshot.Messages) != 40 || p.MessagesPage.Total != 120 || p.Totals["requests"] != requests || p.Snapshot.Run.EventSeq != 50000 || len(p.Snapshot.Events) != 0 {
									b.Fatal("invalid activity projection", e)
								}
								read.ReleaseEventView(snap.Run.ID)
								if mode == "cold" {
									b.StopTimer()
									read.Close()
									b.StartTimer()
								}
							}
							b.StopTimer()
						})
					}
				})
			}
		})
	}
}
