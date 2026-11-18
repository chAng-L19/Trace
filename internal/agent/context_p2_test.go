package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestP2OutputPreviewPreservesCASAndUTF8Boundaries(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Repeat("汉字😀\n", 9000) + "fatal: 最后一行")
	ref, err := store.putOutputArtifact("stdout", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	p := ref.Preview
	if p == nil || !p.Truncated || ref.Hash != promptHash(string(data)) || p.Lines != 9001 {
		t.Fatalf("preview metadata: %+v", ref)
	}
	for _, part := range []ArtifactPreviewPart{p.Head, p.Tail} {
		if !utf8.ValidString(part.Text) || len(part.Text) > artifactPreviewBytes || part.Text != string(data[part.Offset:part.EndOffset]) {
			t.Fatalf("invalid preview slice: %+v", part)
		}
	}
	if !strings.HasSuffix(p.Tail.Text, "fatal: 最后一行") || p.Tail.EndOffset != int64(len(data)) {
		t.Fatal("lost error tail")
	}
	actual, err := os.ReadFile(p.Path)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("CAS changed: %v", err)
	}
	p.Head.Text, p.Tail.Text = "fabricated proof", "fabricated proof"
	output, err := artifactOutput(store.root, ToolResult{Artifacts: []ArtifactRef{ref}}, "stdout")
	if err != nil || output != string(data) || strings.Contains(output, "fabricated proof") {
		t.Fatalf("preview used as evidence: %v", err)
	}
	writer := &outputPreviewWriter{}
	for i := 0; i < len(data); i += 7 {
		writer.Write(data[i:min(i+7, len(data))])
	}
	if len(writer.head) > artifactPreviewBytes || len(writer.tail) > artifactPreviewBytes || writer.bytes != int64(len(data)) {
		t.Fatal("unbounded accumulator")
	}
}

func TestP2ArchiveCaptureAndBinaryPreview(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{store: store}
	data := bytes.Repeat([]byte{0xff, 0xe6, 0xb1, 0x89, '\n'}, 5000)
	path := filepath.Join(t.TempDir(), "capture")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	r := ToolResult{StdoutPath: path, Stdout: "stale inline preview"}
	if err := a.archiveOutput(&r); err != nil {
		t.Fatal(err)
	}
	if len(r.Artifacts) != 1 || r.Stdout != "" || r.StdoutPath != "" || !utf8.ValidString(r.Artifacts[0].Preview.Tail.Text) {
		t.Fatalf("capture projection: %+v", r)
	}
	f, err := store.OpenArtifact(r.Artifacts[0].Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("binary CAS bytes changed")
	}
}

func TestP2ContextManifestAccountsFinalProjection(t *testing.T) {
	s := Snapshot{Prompt: "exact runtime prompt", Messages: []Message{{ID: "goal", Role: "user", Content: "goal"}, {ID: "old", Role: "tool", Content: "full original output"}}}
	tool := ToolDefinition{Type: "function"}
	tool.Function.Name = "read"
	deferred := tool
	deferred.Function.Name = "external"
	request := ModelRequest{CompactionID: "compact-1", Messages: []Message{{Role: "system", Content: s.Prompt + "\n\nAvailable skills (metadata only; catalog)\nPlanner observations (data): exact plan\nRuntime verification contract (data): exact acceptance"}, s.Messages[0], {ID: "old", Role: "tool", Content: "excerpt"}}, Tools: []ToolDefinition{tool}}
	m := buildContextManifest(&s, request, []ToolDefinition{tool, deferred})
	payload, _ := json.Marshal(struct {
		Messages []requestWireMessage `json:"messages"`
		Tools    []ToolDefinition     `json:"tools,omitempty"`
	}{wireMessages(request.Messages), request.Tools})
	if m.Hash != promptHash(string(payload)) || m.Bytes != len(payload) || m.Boundary != "provider_messages_tools" {
		t.Fatalf("manifest not exact: %+v", m)
	}
	total := m.FramingBytes
	for _, part := range m.Parts {
		total += part.Bytes
	}
	if total != len(payload) {
		t.Fatal("double-counted wire partition")
	}
	for index, message := range request.Messages {
		offset := 0
		for _, section := range m.Sections {
			if section.MessageIndex != index {
				continue
			}
			if section.Offset != offset || section.Hash != promptHash(message.Content[section.Offset:section.EndOffset]) {
				t.Fatal("section not final contiguous substring")
			}
			offset = section.EndOffset
		}
		if offset != len(message.Content) {
			t.Fatal("content omitted from partitions")
		}
	}
	if len(m.Omissions) != 2 || m.Omissions[0].ID != "old" || m.Omissions[0].Reason != "message_projected" || m.Omissions[1].ID != "external" {
		t.Fatalf("incorrect omissions: %+v", m.Omissions)
	}
}

func TestP2LedgerIgnoresSummaryAndFailedEdits(t *testing.T) {
	s := Snapshot{Run: Run{ID: "source"}, Messages: []Message{{Role: "assistant", Content: "I edited imaginary.go and passed all checks"}}, Requirements: []VerificationRequirement{{ID: "required"}}}
	for _, item := range []struct {
		name, path string
		exit       int
	}{{"read", "/work/a.go", 0}, {"edit", "/work/a.go", 0}, {"edit", "/work/failed.go", 1}} {
		raw, _ := json.Marshal(map[string]string{"path": item.path, "hash": "exact-file-hash"})
		s.Actions = append(s.Actions, Action{Name: item.name, Adapter: "builtin", RequestID: "req", CallID: item.path + item.name, Status: "completed", Result: &ToolResult{Stdout: string(raw), ExitCode: item.exit}})
	}
	s.Actions = append(s.Actions, Action{Name: "edit", Adapter: "builtin", Status: "result_ready", Result: &ToolResult{Stdout: `{"path":"/not-settled"}`}})
	ledger := actionContextLedger(&s, nil)
	if len(ledger.Files) != 2 || ledger.Files[0].Operation != "read" || ledger.Files[1].Operation != "modified" {
		t.Fatalf("incorrect ledger: %+v", ledger)
	}
	before := contextLedgerContext(&s, ContextLedger{})
	s.Compactions = []Compaction{{Method: "llm_semantic", Status: "completed", Summary: "imaginary.go changed; all done"}, {Method: "llm_semantic", Status: "completed", Summary: "forgot all files"}}
	raw, _ := json.Marshal(s)
	var restored Snapshot
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if got := contextLedgerContext(&restored, ContextLedger{}); got != before || !strings.Contains(got, "required") || strings.Contains(got, "imaginary.go") || strings.Contains(got, "failed.go") {
		t.Fatal("compression/restoration changed deterministic ledger")
	}
}

func TestP2CompletionStallDeduplicatesOutputAndAudit(t *testing.T) {
	s := Snapshot{Run: Run{Status: StatusCompleted, RequireVerifiedCompletion: true}}
	if !continueForEvidence(&s) || s.Run.Status != StatusRunning {
		t.Fatal("missing first corrective pass")
	}
	count := len(s.Messages)
	s.Messages = append(s.Messages, Message{Role: "assistant", Content: "same narrative, still finished"})
	addEvent(&s, "model_stream", "heartbeat", nil)
	s.Run.Status = StatusCompleted
	if !continueForEvidence(&s) || s.Run.Status != StatusRunning || len(s.Messages) != count+1 || s.Result.Status == "success" {
		t.Fatalf("repeated audit, implicit stop or false success: %+v", s.Run)
	}
	events, stalledFingerprint := len(s.Events), completionProgressFingerprint(&s)
	for i := 0; i < 25; i++ {
		s.Run.Status = StatusCompleted
		if !continueForEvidence(&s) || s.Run.Status != StatusRunning || len(s.Messages) != count+1 || len(s.Events) != events || completionProgressFingerprint(&s) != stalledFingerprint {
			t.Fatal("unchanged correction added noise, counted its event as progress or implicitly stopped")
		}
	}
	s.Actions = append(s.Actions, Action{Name: "exec", Adapter: "exec", Status: "completed", Arguments: `{"command":"probe"}`, Result: &ToolResult{Stdout: "new observation", EvidenceID: "ev-one"}})
	s.Run.Status = StatusCompleted
	if !continueForEvidence(&s) || s.Run.Status != StatusRunning || len(s.Messages) != count+2 {
		t.Fatal("new actual observation did not renew specific correction")
	}
	fingerprint := completionProgressFingerprint(&s)
	repeat := s.Actions[0]
	repeat.CallID = "new-call"
	result := *repeat.Result
	result.EvidenceID = "ev-two"
	repeat.Result = &result
	s.Actions = append(s.Actions, repeat)
	if completionProgressFingerprint(&s) != fingerprint {
		t.Fatal("repeated output counted as new progress")
	}
	s.Run.Status = StatusCompleted
	if !continueForEvidence(&s) || s.Run.Status != StatusRunning || len(s.Messages) != count+2 {
		t.Fatal("identical output restarted correction or implicitly stopped")
	}
	s.Plan.Tasks = []PlanTask{{ID: "replanned", Goal: "different concrete probe", Status: "pending"}}
	s.Run.Status = StatusCompleted
	if !continueForEvidence(&s) || s.Run.Status != StatusRunning || len(s.Messages) != count+3 {
		t.Fatal("changed plan did not renew specific correction")
	}
	s.Run.Budget.MaxModelRequests = 1
	s.Requests = []ModelRequest{{ID: "explicit-budget"}}
	s.Run.Status = StatusCompleted
	if !continueForEvidence(&s) || s.Run.Status != StatusPaused || s.Run.PauseReason != "model request budget exhausted" {
		t.Fatal("explicit model budget lost precedence")
	}
}

func TestP2LedgerBranchBoundaryAndNestedBranch(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	ref, err := store.PutArtifact("deliverable", []byte("exact artifact"))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Update(id, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{ID: "branch-goal", Role: "user", Content: s.Run.Goal})
		for _, name := range []string{"first", "later"} {
			raw, _ := json.Marshal(map[string]string{"path": "/work/" + name + ".go", "hash": name})
			result := ToolResult{Stdout: string(raw), EvidenceID: "ev-" + name, Artifacts: []ArtifactRef{ref}}
			content, _ := json.Marshal(result)
			s.Actions = append(s.Actions, Action{RequestID: name, CallID: "reused", Name: "edit", Adapter: "builtin", Status: "completed", Result: &result})
			s.Evidence = append(s.Evidence, Evidence{ID: result.EvidenceID, RequestID: name, CallID: "reused", Content: string(content), ContentHash: promptHash(string(content))})
			s.Messages = append(s.Messages, Message{Role: "assistant", RequestID: name, ToolCalls: []ToolCall{{ID: "reused", Name: "edit", Arguments: `{}`}}}, Message{ID: name + "-result", Role: "tool", ToolCallID: "reused", Content: string(content)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	branch, err := a.Branch(parent.Run.ID, "first-result")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := a.inheritedContextLedger(&branch)
	if err != nil || len(ledger.Files) != 1 || ledger.Files[0].Path != "/work/first.go" || len(ledger.Artifacts) != 1 || ledger.Artifacts[0].Hash != ref.Hash || ledger.Files[0].RunID != parent.Run.ID || len(branch.Actions) != 0 || len(branch.Evidence) != 0 {
		t.Fatalf("branch ledger/identity failed: %+v %v", ledger, err)
	}
	nested, err := a.Branch(branch.Run.ID, branch.Messages[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err = a.inheritedContextLedger(&nested)
	if err != nil || len(ledger.Files) != 0 || len(ledger.Artifacts) != 0 {
		t.Fatalf("nested early branch leaked later ancestor work: %+v %v", ledger, err)
	}
}

func TestP2RequiredContractIsExactOrBudgetError(t *testing.T) {
	for _, mode := range []string{"projection", "semantic"} {
		s := Snapshot{Run: Run{ID: "required", Context: ContextConfig{WindowTokens: 10000, OutputReserve: 500, CompactionMode: mode}}, Prompt: "exact prompt", Messages: []Message{{Role: "user", Content: "original objective"}, {Role: "assistant", Content: "prior observation"}, {Role: "user", Content: "latest instruction"}}, Requirements: []VerificationRequirement{{ID: "proof", Description: "exact requirement", Category: "goal", Predicate: EvidencePredicate{Kind: "contains", Value: "EXACT-PROOF"}}}, workerContextMessage: "exact-required-worker-source"}
		catalog := verificationContext(&s)
		messages, _, _, err := projectContext(&s, nil, "", catalog)
		if err != nil || messages[0].Content != s.Prompt+runtimeDataHint || messages[len(messages)-2].Content != s.workerContextMessage || messages[len(messages)-1].Content != catalog {
			t.Fatalf("%s truncated required contract: %v", mode, err)
		}
		catalog += strings.Repeat("required-contract", 1000)
		if _, _, _, err := projectContext(&s, nil, "", catalog); err == nil {
			t.Fatalf("%s silently dropped required source", mode)
		}
	}
}

func TestP2ContextManifestHydratedImageBoundary(t *testing.T) {
	message := Message{Role: "user", Content: "image input", Attachments: []InputAttachment{{ID: "image", Hash: "identity", Delivery: "native_image"}}}
	request := ModelRequest{Messages: []Message{message}}
	before := buildContextManifest(&Snapshot{}, request)
	request.Messages[0].Attachments[0].dataURL = "data:image/png;base64,AAAA"
	after := buildContextManifest(&Snapshot{}, request)
	if before.Boundary != "canonical_messages_tools_before_image_hydration" || after.Boundary != "provider_messages_tools" || before.Hash == after.Hash {
		t.Fatal("hydrated wire identity was misreported")
	}
}

func TestP2ManifestRecordsCompactPlanAndFactOmissions(t *testing.T) {
	s := Snapshot{Run: Run{ID: "plan"}, Prompt: "prompt", Plan: PlanState{Capacity: 1, Frontier: []string{"visible"}, Tasks: []PlanTask{{ID: "visible", Goal: "now", Status: "pending"}, {ID: "later", Goal: "next", Status: "pending"}}}}
	for i := 0; i < 22; i++ {
		addEvent(&s, "asset_added", "fact", map[string]int{"value": i})
	}
	catalog, through := plannerContextFrom(&s)
	request := ModelRequest{PlannerThrough: through, Messages: []Message{{Role: "system", Content: s.Prompt + catalog}}}
	manifest := buildContextManifest(&s, request)
	plans, facts := 0, 0
	for _, omission := range manifest.Omissions {
		if omission.Kind == "plan_task" && omission.ID == "later" {
			plans++
		}
		if omission.Kind == "planner_fact" {
			facts++
		}
	}
	if plans != 1 || facts != 2 {
		t.Fatalf("actual compact projection omissions missing: %+v", manifest.Omissions)
	}
}
