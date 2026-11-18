package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// Keeps B9's history size while isolating durable telemetry commit cost.
// This diagnostic does not replace source-to-DOM browser acceptance.
func BenchmarkTelemetryCommit50k(b *testing.B) {
	s, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	snap, err := s.Create("telemetry profiling", "prompt", "hash")
	if err != nil {
		b.Fatal(err)
	}
	events := make([]Event, 50000)
	for i := range events {
		events[i] = Event{Type: "model_stream", Data: map[string]any{"request_id": "archived", "delta": StreamDelta{Kind: "text", Text: strings.Repeat("x", 700)}}}
	}
	if err = s.AppendEvents(snap.Run.ID, events); err != nil {
		b.Fatal(err)
	}
	release := s.HoldEventView(snap.Run.ID)
	defer release()
	if _, _, err = s.EventBatch(snap.Run.ID, 50000); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err = s.AppendEvents(snap.Run.ID, []Event{{Type: "model_stream", Data: map[string]any{"request_id": "live", "delta": StreamDelta{Kind: "text", Text: "frame"}}}}); err != nil {
			b.Fatal(err)
		}
	}
}

// Same immutable history and validation in both cases; only the committed
// sequence headers differ. This is a component profile, never a B9 result.
func BenchmarkSemanticEventIndex50k(b *testing.B) {
	events := make([]Event, 50000)
	for i := range events {
		events[i] = Event{Sequence: i + 1, Type: "model_stream", Data: strings.Repeat("x", 700)}
	}
	raw, err := json.Marshal(events)
	if err != nil {
		b.Fatal(err)
	}
	for _, known := range []bool{false, true} {
		name := "cold_headers"
		if known {
			name = "committed_headers"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				v := &storedView{fields: map[string]json.RawMessage{"events": raw}}
				if known {
					v.sequences = make([]int, len(events))
					for i, event := range events {
						v.sequences[i] = event.Sequence
					}
				}
				v.indexEvents()
				if v.eventsErr != nil || !v.eventsOrdered || v.sequences[len(events)-1] != 50000 {
					b.Fatalf("invalid index: %v", v.eventsErr)
				}
			}
		})
	}
}
