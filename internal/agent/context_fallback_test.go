package agent

import "testing"

type countedContextSchema struct{ encodes int }

func (s *countedContextSchema) MarshalJSON() ([]byte, error) {
	s.encodes++
	return []byte(`{"type":"object"}`), nil
}

func TestUsageCalibrationOnlyEncodesFallbackWhenNeeded(t *testing.T) {
	for _, name := range []string{"anchored", "native-image-tail", "tools-changed", "empty", "non-system", "missing-usage", "nil-input", "zero-usage", "failed-response", "edited-prefix", "summary", "purpose"} {
		t.Run(name, func(t *testing.T) {
			schema := &countedContextSchema{}
			tools := []ToolDefinition{{}}
			tools[0].Function.Name = "fixture"
			tools[0].Function.Parameters = schema
			prior := []Message{{Role: "system", Content: "system"}, {Role: "user", Content: "objective"}}
			input := int64(500)
			s := Snapshot{Requests: []ModelRequest{{ID: "request", Messages: prior, Tools: tools}}, Responses: []ResponseRecord{{RequestID: "request", Response: ModelResponse{Usage: &TokenUsage{InputTokens: &input}}}}}
			current := append(append([]Message(nil), prior...), Message{Role: "assistant", Content: "tail"})
			switch name {
			case "empty":
				current = nil
			case "non-system":
				current[0].Role = "user"
			case "missing-usage":
				s.Responses[0].Response.Usage = nil
			case "nil-input":
				s.Responses[0].Response.Usage.InputTokens = nil
			case "zero-usage":
				input = 0
			case "failed-response":
				s.Responses[0].Error = "failed"
			case "edited-prefix":
				current[1].Content = "changed"
			case "summary":
				s.Requests[0].Messages = append([]Message(nil), prior...)
				s.Requests[0].Messages[0].Content = summaryPrompt
			case "purpose":
				s.Requests[0].Purpose = "summary"
			case "native-image-tail":
				current[2].Attachments = []InputAttachment{{Delivery: "native_image"}}
			case "tools-changed":
				tools = append([]ToolDefinition(nil), tools...)
				tools[0].Function.Description = "updated"
			}
			want := EstimateContextTokens(current, tools)
			wantEncodes := 1
			if name == "anchored" || name == "native-image-tail" || name == "tools-changed" {
				want = int(input) + EstimateContextTokens(current[2:], nil)
				wantEncodes = 2 // Existing old/new schema comparison only; no full fallback.
				if name == "tools-changed" {
					want += EstimateContextTokens(nil, tools)
					wantEncodes++
				}
			}
			schema.encodes = 0
			if got := calibratedContextTokens(&s, current, tools); got != want {
				t.Fatalf("estimate=%d want=%d", got, want)
			}
			if schema.encodes != wantEncodes {
				t.Fatalf("schema encodes=%d want=%d", schema.encodes, wantEncodes)
			}
		})
	}
}
