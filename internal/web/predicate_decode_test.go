package web

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"trace/internal/agent"
)

func predicateDecodeServer(t *testing.T, root string) *Server {
	t.Helper()
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a, err := agent.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(a, Options{InitialConfig: Config{Context: agent.DefaultContextConfig()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestVerificationPredicateDecodeHTTP(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			if backend == "sqlite" {
				if err := agent.MigrateStoreToSQLite(root); err != nil {
					t.Fatal(err)
				}
			}
			s := predicateDecodeServer(t, root)
			snap, err := s.store.Create("exact predicate decoding", "prompt", "hash")
			if err != nil {
				t.Fatal(err)
			}
			base := "/api/runs/" + snap.Run.ID
			h := s.Handler()
			bad := json.RawMessage(`{"requirements":[{"id":"typo","description":"reject misspelled expected","category":"goal","predicate":{"kind":"json_equals","pointer":"/n","expcted":7}}]}`)
			response := request(t, h, "PUT", base+"/requirements", bad, 400)
			if !strings.Contains(response.Body.String(), `unknown field \"expcted\"`) {
				t.Fatalf("unexpected decode error: %s", response.Body.String())
			}
			snap, err = s.store.Load(snap.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Requirements) != 0 {
				t.Fatal("rejected predicate changed requirements")
			}
			valid := json.RawMessage(`{"requirements":[{"id":"integer","description":"exact integer","category":"goal","predicate":{"kind":"json_equals","pointer":"/n","expected":9007199254740993}},{"id":"null","description":"literal null","category":"goal","predicate":{"kind":"json_equals","pointer":"/n","expected":null}},{"id":"object","description":"arbitrary expected object","category":"goal","predicate":{"kind":"json_equals","pointer":"/n","expected":{"expcted":7,"items":[9007199254740993,null]}}}]}`)
			response = request(t, h, "PUT", base+"/requirements", valid, 200)
			if err := json.Unmarshal(response.Body.Bytes(), &snap); err != nil {
				t.Fatal(err)
			}
			if len(snap.Requirements) != 3 {
				t.Fatalf("requirements: %+v", snap.Requirements)
			}
			wantRequirements := snap.Requirements
			wantExpected := []any{json.Number("9007199254740993"), nil, map[string]any{"expcted": json.Number("7"), "items": []any{json.Number("9007199254740993"), nil}}}
			for i, want := range wantExpected {
				if !reflect.DeepEqual(snap.Requirements[i].Predicate.Expected, want) {
					t.Fatalf("requirement %s: expected %#v, want %#v", snap.Requirements[i].ID, snap.Requirements[i].Predicate.Expected, want)
				}
			}
			// Reconstruct intact archived tool provenance; no command is executed.
			_, err = s.store.Update(snap.Run.ID, func(snap *agent.Snapshot) error {
				for id, stdout := range map[string]string{
					"exact":           `{"n":9007199254740993}`,
					"neighbor":        `{"n":9007199254740992}`,
					"null":            `{"n":null}`,
					"object":          `{"n":{"expcted":7,"items":[9007199254740993,null]}}`,
					"object-neighbor": `{"n":{"expcted":7,"items":[9007199254740992,null]}}`,
					"missing":         `{}`,
				} {
					requestID, callID := "request-"+id, "call-"+id
					call := agent.ToolCall{ID: callID, Name: "exec", Arguments: `{}`}
					result := agent.ToolResult{Stdout: stdout, EvidenceID: id}
					raw, err := json.Marshal(result)
					if err != nil {
						return err
					}
					snap.Requests = append(snap.Requests, agent.ModelRequest{ID: requestID})
					snap.Responses = append(snap.Responses, agent.ResponseRecord{RequestID: requestID, Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{call}}})
					snap.Actions = append(snap.Actions, agent.Action{RequestID: requestID, CallID: callID, Name: "exec", Adapter: "exec", Arguments: call.Arguments, Status: "completed", Result: &result})
					snap.Messages = append(snap.Messages, agent.Message{Role: "tool", ToolCallID: callID, Content: string(raw)})
					snap.Run.EventSeq++
					snap.Events = append(snap.Events, agent.Event{Sequence: snap.Run.EventSeq, Type: "tool_result", Data: map[string]string{"call_id": callID, "evidence_id": id}, CreatedAt: snap.Run.UpdatedAt})
					snap.Evidence = append(snap.Evidence, agent.Evidence{ID: id, RequestID: requestID, CallID: callID, SourceEventSequence: snap.Run.EventSeq, Content: string(raw), ContentHash: fmt.Sprintf("%x", sha256.Sum256(raw)), Type: "tool_execution"})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			verify := func(h http.Handler) {
				t.Helper()
				response := request(t, h, "GET", base+"/requirements", nil, 200)
				var saved struct {
					Requirements []agent.VerificationRequirement `json:"requirements"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(saved.Requirements, wantRequirements) {
					t.Fatalf("requirements changed: %+v", saved.Requirements)
				}
				for _, tc := range []struct {
					requirement, evidence, status string
				}{
					{"integer", "exact", "pass"},
					{"integer", "neighbor", "fail"},
					{"integer", "null", "fail"},
					{"null", "null", "pass"},
					{"null", "exact", "fail"},
					{"null", "missing", "unknown"},
					{"object", "object", "pass"},
					{"object", "object-neighbor", "fail"},
				} {
					response := request(t, h, "POST", base+"/checks", map[string]any{"requirement_id": tc.requirement, "evidence_ids": []string{tc.evidence}}, 200)
					var checked agent.Snapshot
					if err := json.Unmarshal(response.Body.Bytes(), &checked); err != nil {
						t.Fatal(err)
					}
					if len(checked.Checks) == 0 {
						t.Fatal("missing verification check")
					}
					check := checked.Checks[len(checked.Checks)-1]
					if check.RequirementID != tc.requirement || !reflect.DeepEqual(check.EvidenceIDs, []string{tc.evidence}) || check.Status != tc.status {
						t.Fatalf("%s vs %s: got %s (%s), want %s", tc.requirement, tc.evidence, check.Status, check.Reason, tc.status)
					}
				}
			}
			verify(h)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := predicateDecodeServer(t, root)
			verify(reopened.Handler())
		})
	}
}
