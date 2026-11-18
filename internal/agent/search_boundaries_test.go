package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceSearchAuditReceiptReadBoundaries(t *testing.T) {
	_, store := newTestAgent(t, successProvider{})
	raw := []byte("{\"account\":\"private-account\",\"echo\":\"api-fixture-secret\"}\n")
	ref, err := store.PutArtifact("search_response", raw)
	if err != nil {
		t.Fatal(err)
	}
	// Neither a new reference kind nor configuration loss makes raw bytes public.
	if _, err = store.PutArtifact("stdout", raw); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	a, err := New(reopened, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "receipt.json")
	if err = os.WriteFile(copyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	pathArgs, _ := json.Marshal(FileRequest{Path: filepath.Join(store.root, "artifacts", ref.Hash), Offset: 40, Limit: 1})
	copyArgs, _ := json.Marshal(FileRequest{Path: copyPath, StartLine: 1, LineCount: 1})
	calls := []ToolCall{
		{ID: "whole", Name: "artifact_read", Arguments: fmt.Sprintf(`{"hash":%q}`, ref.Hash)},
		{ID: "split", Name: "artifact_read", Arguments: fmt.Sprintf(`{"hash":%q,"offset":40,"limit":1}`, ref.Hash)},
		{ID: "eof", Name: "artifact_read", Arguments: fmt.Sprintf(`{"hash":%q,"offset":%d,"limit":1}`, ref.Hash, len(raw))},
		{ID: "path", Name: "read", Arguments: string(pathArgs)},
		{ID: "copy", Name: "read", Arguments: string(copyArgs)},
	}
	a.provider = manualProvider(calls...)
	s, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil || len(s.Actions) != len(calls) {
		t.Fatalf("builtin chain: actions=%d err=%v", len(s.Actions), err)
	}
	for _, action := range s.Actions {
		if action.Result == nil || action.Result.ExitCode == 0 || action.Result.Stdout != "" || !strings.Contains(action.Result.Error, "audit-only") {
			t.Fatalf("raw receipt exposed: %+v", action)
		}
	}
	modelMessages, _ := json.Marshal(s.Messages)
	if strings.Contains(string(modelMessages), "api-fixture-secret") || strings.Contains(string(modelMessages), "private-account") || strings.Contains(string(modelMessages), base64.StdEncoding.EncodeToString(raw)) {
		t.Fatal("raw secret entered model messages")
	}
	// Audit/API readers retain byte-faithful pagination, including a split secret.
	page, err := reopened.ReadArtifact(ref.Hash, 40, 1)
	if err != nil || page.Content != string(raw[40:41]) || page.NextOffset != 41 {
		t.Fatalf("audit changed: %+v %v", page, err)
	}
	data, err := base64.StdEncoding.DecodeString(page.DataBase64)
	if err != nil || string(data) != page.Content {
		t.Fatal("audit base64 changed")
	}
	// Treat an upstream account response without a configured key as audit-only.
	account, err := store.PutArtifact("search_response", []byte(`{"account":"private"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.builtin(context.Background(), "", ToolCall{Name: "artifact_read", Arguments: fmt.Sprintf(`{"hash":%q}`, account.Hash)}); err == nil {
		t.Fatal("account receipt exposed")
	}
	// Edit diffs also expose old bytes, so the same gate must run before any
	// mutation or diff construction.
	rawPath := filepath.Join(store.root, "artifacts", ref.Hash)
	if _, err = fileOperation("edit", FileRequest{Path: rawPath, OldText: "private-account", NewText: "redacted"}, store.checkModelArtifactRead); err == nil {
		t.Fatal("audit-only receipt edit exposed old bytes")
	}
	unchanged, err := os.ReadFile(rawPath)
	if err != nil || string(unchanged) != string(raw) {
		t.Fatal("audit-only receipt changed after rejected edit")
	}
}

func TestSourceSearchOrdinaryArtifactsKeepByteAndLineSemantics(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	body := []byte("first\n雪\x00\xfflast\n")
	ref, err := store.PutArtifact("stdout", body)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ordinary.bin")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"read", "artifact_read"} {
		var reconstructed []byte
		for offset := int64(0); ; {
			arguments, _ := json.Marshal(map[string]any{"path": path, "offset": offset, "limit": 2})
			if tool == "artifact_read" {
				arguments, _ = json.Marshal(map[string]any{"hash": ref.Hash, "offset": offset, "limit": 2})
			}
			value, err := a.builtin(context.Background(), "", ToolCall{Name: tool, Arguments: string(arguments)})
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(value)
			var page ArtifactSlice
			if err = json.Unmarshal(encoded, &page); err != nil {
				t.Fatal(err)
			}
			part, err := base64.StdEncoding.DecodeString(page.DataBase64)
			if err != nil || page.Offset != offset || page.NextOffset != offset+int64(len(part)) || page.Bytes != int64(len(body)) || page.Hash != ref.Hash {
				t.Fatalf("page semantics: %+v %v", page, err)
			}
			reconstructed = append(reconstructed, part...)
			if page.EOF {
				break
			}
			if page.NextOffset <= offset {
				t.Fatal("pagination stalled")
			}
			offset = page.NextOffset
		}
		if string(reconstructed) != string(body) {
			t.Fatal("base64 bytes changed")
		}
	}
	for _, tc := range []struct{ line, count, start, end int }{{2, 1, 6, len(body)}, {3, 1, len(body), len(body)}} {
		value, err := fileOperation("read", FileRequest{Path: path, StartLine: tc.line, LineCount: tc.count}, store.checkModelArtifactRead)
		if err != nil {
			t.Fatal(err)
		}
		page := value.(map[string]any)
		if page["offset"] != tc.start || page["next_offset"] != tc.end || page["content"] != string(body[tc.start:tc.end]) {
			t.Fatalf("line page: %+v", page)
		}
	}
	if _, err = fileOperation("read", FileRequest{Path: path, StartLine: 4}, store.checkModelArtifactRead); err == nil {
		t.Fatal("out of range line accepted")
	}
	large := strings.Repeat("x", 1<<20) + "\nlast\n"
	if err = os.WriteFile(path, []byte(large), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := fileOperation("read", FileRequest{Path: path, StartLine: 2, LineCount: 1}, store.checkModelArtifactRead)
	if err != nil {
		t.Fatal(err)
	}
	if value.(map[string]any)["content"] != "last\n" {
		t.Fatal("long line scan truncated source")
	}
}

func TestSourceSearchBranchQuotaAndDeletedAncestry(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	root := createTestRun(t, a)
	source, err := store.Update(root, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "user", Content: "branch point"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	branch, err := a.Branch(root, source.Messages[len(source.Messages)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(root, []PlanTask{{ID: "recon", Goal: "collect"}}); err != nil {
		t.Fatal(err)
	}
	child := registerBudgetChild(t, a, root, "recon", "worker")
	if _, err = a.SetPlan(child, []PlanTask{{ID: "nested", Goal: "collect"}}); err != nil {
		t.Fatal(err)
	}
	nested := registerBudgetChild(t, a, child, "nested", "nested-worker")
	request := SourceSearchRequest{Source: "fofa", PageSize: 100}
	config := SearchSourceConfig{MaxRequests: 2, MaxResults: 200}
	for _, id := range []string{root, nested, branch.Run.ID} {
		if ok, err := a.reserveSearchUsage(id, request, config); err != nil || !ok {
			t.Fatalf("quota %s: %v %v", id, ok, err)
		}
	}
	if ok, err := a.reserveSearchUsage(child, request, config); err != nil || ok {
		t.Fatal("worker escaped shared root quota")
	}
	forged := createTestRun(t, a)
	if _, err = store.Update(forged, func(s *Snapshot) error { s.Run.ParentRunID, s.Run.ParentTaskID = root, "unregistered"; return nil }); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.reserveSearchUsage(forged, request, SearchSourceConfig{MaxRequests: 10, MaxResults: 1000}); err == nil || ok {
		t.Fatal("unregistered ancestry admitted acquisition")
	}
	if _, err = store.Update(root, func(s *Snapshot) error { s.Run.Deleted = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.reserveSearchUsage(branch.Run.ID, request, config); err != nil || !ok {
		t.Fatal("branch depended on deleted source")
	}
	for _, id := range []string{root, child, nested} {
		if ok, err := a.reserveSearchUsage(id, request, SearchSourceConfig{MaxRequests: 10, MaxResults: 1000}); err == nil || ok {
			t.Fatal("deleted ancestor admitted acquisition")
		}
	}
	if _, err = store.Update(child, func(s *Snapshot) error { s.Run.ParentRunID = "missing-parent"; return nil }); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.reserveSearchUsage(nested, request, config); err == nil || ok {
		t.Fatal("missing ancestor admitted acquisition")
	}
	// A branch's historical source is not execution ancestry, even when absent.
	if _, err = store.Update(branch.Run.ID, func(s *Snapshot) error { s.Run.ParentRunID = "missing-history"; return nil }); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.reserveSearchUsage(branch.Run.ID, request, SearchSourceConfig{MaxRequests: 3, MaxResults: 300}); err != nil || !ok {
		t.Fatal("branch depended on missing history")
	}
	s, err := store.Load(root)
	if err != nil || s.SearchUsage["fofa"].Requests != 2 {
		t.Fatal("rejected acquisition changed root usage")
	}
}
