package agent

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPiModelToolsDeltaUsesPreparedSchemaAndParallelBaseline(t *testing.T) {
	initial := execToolDefinition()
	changed := initial
	changed.Function.Description += " revised schema"
	response := ModelResponse{Content: "same durable response"}
	for _, tc := range []struct {
		name     string
		tools    []ToolDefinition
		parallel []string
		changed  bool
	}{
		{"unchanged", []ToolDefinition{initial}, []string{"exec"}, false},
		{"same-name-schema", []ToolDefinition{changed}, []string{"exec"}, true},
		{"parallel-only", []ToolDefinition{initial}, []string{}, true},
		{"null-clear", nil, nil, true},
		{"empty-clear", []ToolDefinition{}, []string{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var transport piContextTransport
			request := ModelRequest{Messages: []Message{{Role: "user", Content: "unchanged history"}}, Tools: []ToolDefinition{initial}}
			parallel := []string{"exec"}
			prepared, err := transport.prepare(request, parallel, 0)
			if err != nil {
				t.Fatal(err)
			}
			// Caller-owned registry slices are not the delivered baseline.
			parallel[0] = "mutated"
			baseline, _ := json.Marshal(prepared)
			canonical := append([]byte(nil), transport.tools...)
			request.Tools = tc.tools
			completed, err := transport.modelResponse(request, response, tc.parallel)
			if err != nil {
				t.Fatal(err)
			}
			_, hasTools := completed["tools"]
			_, hasParallel := completed["parallel_tools"]
			if hasTools != tc.changed || hasParallel != tc.changed || !reflect.DeepEqual(completed["response"], response) {
				t.Fatalf("wrong completion delta: %#v", completed)
			}
			if hasTools && (!reflect.DeepEqual(completed["tools"], tc.tools) || !reflect.DeepEqual(completed["parallel_tools"], tc.parallel)) {
				t.Fatal("completion did not carry actual final request tools")
			}
			if !bytes.Equal(canonical, transport.tools) || transport.revision != 1 || !reflect.DeepEqual(transport.parallel, []string{"exec"}) {
				t.Fatal("model completion advanced canonical prepare baseline")
			}
			if after, _ := json.Marshal(prepared); !bytes.Equal(baseline, after) {
				t.Fatal("model completion mutated the prepared payload")
			}
			next, err := transport.prepare(request, tc.parallel, 1)
			if err != nil {
				t.Fatal(err)
			}
			_, nextHasTools := next["tools"]
			if nextHasTools != !reflect.DeepEqual(tc.tools, []ToolDefinition{initial}) || next["message_prefix"] != 1 {
				t.Fatalf("next prepare lost canonical schema transition: %#v", next)
			}
			stable, err := transport.modelResponse(request, response, tc.parallel)
			if err != nil || len(stable) != 1 {
				t.Fatalf("next prepared state was not reused: %#v, %v", stable, err)
			}
		})
	}
}

func TestPiModelToolsDeltaStableRoundsAndMarshalFailure(t *testing.T) {
	tool := execToolDefinition()
	tool.Function.Description = strings.Repeat("stable schema description ", 128)
	request := ModelRequest{Tools: []ToolDefinition{tool}}
	response := ModelResponse{Content: "continue"}
	var transport piContextTransport
	fullBytes, deltaBytes := 0, 0
	for i := 0; i < 24; i++ {
		if _, err := transport.prepare(request, nil, transport.revision); err != nil {
			t.Fatal(err)
		}
		completed, err := transport.modelResponse(request, response, []string{})
		if err != nil || len(completed) != 1 {
			t.Fatalf("stable round %d repeated schema: %#v, %v", i, completed, err)
		}
		full, _ := json.Marshal(map[string]any{"response": response, "tools": request.Tools, "parallel_tools": []string{}})
		delta, _ := json.Marshal(completed)
		fullBytes += len(full)
		deltaBytes += len(delta)
	}
	if deltaBytes >= fullBytes {
		t.Fatal("stable model replies did not shrink")
	}
	t.Logf("24 stable model replies: schema sets 24 -> 0; JSON bytes %d -> %d (prepare excluded)", fullBytes, deltaBytes)
	invalid := tool
	invalid.Function.Parameters = map[string]any{"unsupported": make(chan int)}
	request.Tools = []ToolDefinition{invalid}
	before := append([]byte(nil), transport.tools...)
	if _, err := transport.modelResponse(request, response, nil); err == nil || !bytes.Equal(before, transport.tools) || transport.revision != 24 {
		t.Fatal("invalid schema was swallowed or changed canonical baseline")
	}
}
