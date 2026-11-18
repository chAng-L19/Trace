package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// This exercises the real OpenAI-compatible JSON fallback, not the intentionally
// permissive in-process Provider fixture contract. A missing finish reason is
// not proof that a provider completed generation, even with a parseable body.
func TestRuntimeRound3JSONProviderRequiresCompletionBoundary(t *testing.T) {
	for _, reason := range []string{"missing", "null"} {
		for _, output := range []string{"text", "tool"} {
			t.Run(reason+"/"+output, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "must-not-be-written.txt")
				message := map[string]any{"role": "assistant", "content": "partial response"}
				if output == "tool" {
					args, err := json.Marshal(map[string]string{"path": path, "content": "unexpected execution"})
					if err != nil {
						t.Fatal(err)
					}
					message["tool_calls"] = []any{map[string]any{"id": "incomplete-call", "type": "function", "function": map[string]string{"name": "write", "arguments": string(args)}}}
				}
				choice := map[string]any{"index": 0, "message": message}
				if reason == "null" {
					choice["finish_reason"] = nil
				}
				body, err := json.Marshal(map[string]any{"choices": []any{choice}})
				if err != nil {
					t.Fatal(err)
				}
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if requests.Add(1) == 1 {
						_, _ = w.Write(body)
					} else {
						fmt.Fprint(w, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`)
					}
				}))
				defer server.Close()
				a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "round3-fixture"})
				id := createTestRun(t, a)
				_, runErr := a.Run(context.Background(), id)
				s, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if runErr == nil || s.Run.Status != StatusFailed || len(s.Actions) != 0 || requests.Load() != 1 {
					t.Errorf("incomplete JSON advanced runtime: status=%s actions=%d requests=%d error=%v", s.Run.Status, len(s.Actions), requests.Load(), runErr)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("incomplete JSON caused a file write: stat=%v", err)
				}
			})
		}
	}
}

// Losing the model request that owns an action must revoke its proof. Changing
// only the Requests array is a partial-ledger fault, not a forged full Store.
func TestRuntimeRound3MissingRequestRevokesExecutionEvidence(t *testing.T) {
	for _, mutation := range []string{"remove", "change-id"} {
		t.Run(mutation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "observed.txt")
			if err := os.WriteFile(path, []byte("round3-observed-marker"), 0600); err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal(map[string]string{"path": path})
			if err != nil {
				t.Fatal(err)
			}
			a, store := newTestAgent(t, manualProvider(ToolCall{ID: "read-proof", Name: "read", Arguments: string(args)}))
			a.SetSuccessContains("round3-observed-marker")
			id := createTestRun(t, a)
			s, err := a.Run(context.Background(), id)
			if err != nil || s.Result.Status != "success" || len(s.Evidence) != 1 || s.Evidence[0].Verification != "verified" {
				t.Fatalf("positive control failed: result=%s evidence=%+v err=%v", s.Result.Status, s.Evidence, err)
			}
			baseline, err := store.Report(id)
			if err != nil || !strings.Contains(baseline, "状态：成功（success）") || !strings.Contains(baseline, "输出匹配：通过") {
				t.Fatalf("report positive control failed: %s %v", baseline, err)
			}
			s, err = store.Update(id, func(s *Snapshot) error {
				if mutation == "remove" {
					s.Requests = s.Requests[1:]
				} else {
					s.Requests[0].ID = "orphaned-request"
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if s.Result.Status == "success" || s.Evidence[0].Verification == "verified" {
				t.Errorf("orphan execution retained proof: result=%s verification=%s", s.Result.Status, s.Evidence[0].Verification)
			}
			report, err := store.Report(id)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(report, "状态：未完成（incomplete）") || strings.Contains(report, "状态：成功（success）") || strings.Contains(report, "输出匹配：通过") {
				t.Error("report export preserved an orphan execution as verified success")
			}
		})
	}
}

func TestRuntimeRound3PartialSSEToolCallNeverExecutes(t *testing.T) {
	for _, ending := range []string{"missing-done", "missing-finish"} {
		t.Run(ending, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "must-not-be-written.txt")
			args, err := json.Marshal(map[string]string{"path": path, "content": "unexpected execution"})
			if err != nil {
				t.Fatal(err)
			}
			call := map[string]any{"index": 0, "id": "partial", "type": "function", "function": map[string]string{"name": "write", "arguments": string(args)}}
			chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}}}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", chunk)
				if ending == "missing-done" {
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
				} else {
					fmt.Fprint(w, "data: [DONE]\n\n")
				}
			}))
			defer server.Close()
			a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "round3-fixture"})
			a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(3)})
			s, err := a.Run(context.Background(), createTestRun(t, a))
			if err != nil || s.Run.Status != StatusPaused || s.Run.PauseReason != "model request budget exhausted" || len(s.Actions) != 0 || len(s.Responses) != 3 || s.Responses[0].Error == "" {
				t.Fatalf("partial stream advanced execution: status=%s actions=%d responses=%+v err=%v", s.Run.Status, len(s.Actions), s.Responses, err)
			}
			if len(s.Responses[0].Response.ToolCalls) != 1 || s.Responses[0].Response.ToolCalls[0].Arguments != string(args) {
				t.Fatal("partial streamed call was not retained for inspection")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("partial stream caused file side effects: %v", err)
			}
		})
	}
}

func TestRuntimeRound3CASPagingAndPostPreviewLoss(t *testing.T) {
	for _, mutation := range []string{"missing", "changed"} {
		t.Run(mutation, func(t *testing.T) {
			_, store := newTestAgent(t, MockProvider{})
			// Small byte pages deliberately split multibyte UTF-8. Raw bytes are
			// the authority; clients may decode across page boundaries.
			data := []byte(strings.Repeat("中🙂\x00", 100))
			ref, err := store.PutArtifact("stdout", data)
			if err != nil {
				t.Fatal(err)
			}
			var restored []byte
			for offset := int64(0); ; {
				page, err := store.ReadArtifact(ref.Hash, offset, 5)
				if err != nil {
					t.Fatal(err)
				}
				part, err := base64.StdEncoding.DecodeString(page.DataBase64)
				if err != nil || page.Offset != offset || page.NextOffset != offset+int64(len(part)) || len(part) == 0 {
					t.Fatalf("non-progressing or invalid byte page: %+v err=%v", page, err)
				}
				restored = append(restored, part...)
				if page.EOF {
					break
				}
				offset = page.NextOffset
			}
			if !bytes.Equal(restored, data) {
				t.Fatal("pagination changed original UTF-8/binary bytes")
			}
			path := filepath.Join(store.root, "artifacts", ref.Hash)
			if mutation == "missing" {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("corrupt-and-different-length"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReadArtifact(ref.Hash, 5, 5); err == nil {
				t.Error("cached preview hid artifact loss/corruption")
			}
			if file, err := store.OpenArtifact(ref.Hash); err == nil {
				file.Close()
				t.Error("raw download accepted missing/corrupt artifact")
			}
			if _, err := artifactOutput(store.root, ToolResult{Artifacts: []ArtifactRef{ref}}, "stdout"); err == nil {
				t.Error("evidence reconstruction accepted missing/corrupt artifact")
			}
		})
	}
}
