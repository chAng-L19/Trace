package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

var u7Text = ReportText{
	ZhCN: "检查已观察到预期标记。复现使用固定命令，结论仅适用于本次观察；清理状态仍为未知，尚无独立影响验证。",
	En:   "The check observed the expected marker. Reproduction uses the recorded command and the conclusion applies only to this observation. Cleanup remains unknown and independent impact validation is still pending.",
}

// Report-only continuations are not the business claim under mutation.
func reportBusinessMessage(t *testing.T, s *Snapshot) *Message {
	t.Helper()
	for i := len(s.Messages) - 1; i >= 0; i-- {
		m := &s.Messages[i]
		if m.Role == "assistant" && !m.ReportOnly && len(m.ToolCalls) == 0 {
			return m
		}
	}
	t.Fatal("no business assistant message")
	return nil
}

func u7Store(t *testing.T, backend string) (*Store, Snapshot) {
	t.Helper()
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if backend == "sqlite" {
		store.Close()
		if err = MigrateStoreToSQLite(root); err != nil {
			t.Fatal(err)
		}
		store, err = NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { store.Close() })
	s, err := store.Create("报告版本核对", "prompt", "prompt-hash")
	if err != nil {
		t.Fatal(err)
	}
	return store, s
}

func u7Read(t *testing.T, store *Store, hash string) string {
	t.Helper()
	f, err := store.OpenArtifact(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Store.Load would reconcile the deliberately stale frontier in these fixtures.
// A historical file read (including a miss) must leave the saved Run alone.
func TestArchivedDocumentReadDoesNotReconcileRun(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, s := u7Store(t, backend)
			ref, err := store.PutArtifact("report", []byte("fixed archive"))
			if err != nil {
				t.Fatal(err)
			}
			ref.Name = "archived.en.md"
			s.Plan.Frontier = []string{"stale-entry"}
			for _, pinned := range []bool{false, true} {
				id := ""
				if pinned {
					id = "saved-version"
					s.Reports = []ReportArchive{{ID: id, Kind: "final", SubjectID: "result", Files: []ArtifactRef{ref}}}
				}
				if err := store.savePreparedLocked(s); err != nil {
					t.Fatal(err)
				}
				_, body, err := store.ReportDocument(s.Run.ID, id, "en")
				if pinned && (err != nil || body != "fixed archive") || !pinned && err == nil {
					t.Fatalf("pinned=%t: body=%q err=%v", pinned, body, err)
				}
				after, err := store.loadRawLocked(s.Run.ID)
				if err != nil || !after.Run.UpdatedAt.Equal(s.Run.UpdatedAt) || !reflect.DeepEqual(after.Plan.Frontier, s.Plan.Frontier) || len(after.Reports) != len(s.Reports) {
					t.Fatalf("document read reconciled or changed the saved Run: %v", err)
				}
			}
		})
	}
}

func TestReportMissingFilesDoNotCreateArchive(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, s := u7Store(t, backend)
			// A verified Load would replace this stale result and produce a final.
			s.Run.Status = StatusCompleted
			s.Result = Result{Status: "incomplete", Summary: "saved result", CleanupStatus: "unknown"}
			s.Messages = append(s.Messages, Message{Role: "assistant", Content: "A completed conversation with no archived Markdown file."})
			s.Reports = []ReportArchive{{ID: "no-files", Kind: "final", SubjectID: "result"}, {ID: "lost-file", Kind: "final", SubjectID: "result", Files: []ArtifactRef{{Hash: strings.Repeat("b", 64), Name: "lost.en.md"}}}}
			if err := store.savePreparedLocked(s); err != nil {
				t.Fatal(err)
			}
			before, err := store.loadRawLocked(s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			view := ReportReceipts(before)
			if len(view) != 2 || len(view[1].Files) != 0 || len(before.Reports[1].Files) != 1 {
				t.Fatal("missing file projected as downloadable or durable receipt lost")
			}
			if _, _, err = store.ReportDocument(s.Run.ID, "", "en"); err == nil || !strings.Contains(err.Error(), "no saved summary report") {
				t.Fatalf("default download did not report missing archive: %v", err)
			}
			if _, _, err = store.ReportDocument(s.Run.ID, "lost-file", "en"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing CAS file: %v", err)
			}
			after, err := store.loadRawLocked(s.Run.ID)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("document read changed stored Run: %v", err)
			}
		})
	}
}

func TestReportFinalViewTracksCurrentChildSource(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, root := u7Store(t, backend)
			child, err := store.createInitialized("child source", "prompt", "prompt-hash", func(s *Snapshot) error {
				s.Run.ParentRunID, s.Run.ParentTaskID = root.Run.ID, "part"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			child, err = store.Load(child.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			w := AgentWorker{ID: "worker", RunID: child.Run.ID, TaskID: "part", Visit: 1}
			latest := newChildReport(root.Run.ID, w, &child, "completed")
			older := latest
			older.SourceHash = "superseded"
			root, err = store.Update(root.Run.ID, func(s *Snapshot) error {
				s.Run.Status = StatusCompleted
				s.Workers = []AgentWorker{w}
				s.ChildReports = []ChildReport{older, latest}
				return nil
			})
			if err != nil || len(root.Reports) != 1 {
				t.Fatalf("root archive: %v", err)
			}
			id := root.Reports[0].ID
			ref, fixed, err := store.ReportDocument(root.Run.ID, id, "en")
			if err != nil {
				t.Fatal(err)
			}
			check := func(want string) {
				t.Helper()
				view := root
				view.ChildReports, err = store.ChildReportsFrom(root, 0)
				if err != nil {
					t.Fatal(err)
				}
				got := ReportReceipts(view)
				if len(got) != 1 || got[0].ID != id || got[0].SourceStatus != want {
					t.Fatalf("got report %+v, want source %s", got, want)
				}
			}
			check("current")
			if _, body, err := store.ReportDocument(root.Run.ID, "", "en"); err != nil || body != fixed {
				t.Fatalf("default download lost current archived file: %v", err)
			}
			if _, err = store.Update(child.Run.ID, func(s *Snapshot) error { s.Run.Goal += " changed"; return nil }); err != nil {
				t.Fatal(err)
			}
			check("changed")
			root.Plan.Frontier = []string{"stale-frontier"}
			if err := store.savePreparedLocked(root); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.ReportDocument(root.Run.ID, "", "en"); err == nil || !strings.Contains(err.Error(), "current source") {
				t.Fatalf("default download presented changed source as current: %v", err)
			}
			after, err := store.loadRawLocked(root.Run.ID)
			if err != nil || !reflect.DeepEqual(after.Plan.Frontier, root.Plan.Frontier) || !after.Run.UpdatedAt.Equal(root.Run.UpdatedAt) || !reflect.DeepEqual(after.Reports, root.Reports) {
				t.Fatalf("selecting the current document rewrote root: %v", err)
			}
			got, body, err := store.ReportDocument(root.Run.ID, id, "en")
			if err != nil || body != fixed || got.Hash != ref.Hash {
				t.Fatalf("live source change rewrote pinned report: %v", err)
			}
		})
	}
}

func TestU7ReportArchiveVersionsAndCAS(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, s := u7Store(t, backend)
			claim := Result{Status: "failed", Summary: "本次检查未达到目标，保留已经观察到的事实。", Report: &u7Text, ReproductionSteps: []string{"printf u7-marker"}}
			raw, _ := json.Marshal(map[string]any{"trace_result": claim})
			s, err := store.Update(s.Run.ID, func(s *Snapshot) error {
				s.Run.Status = StatusCompleted
				s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
				return nil
			})
			if err != nil || len(s.Reports) != 1 || s.Result.Status != "failed" {
				t.Fatalf("archive: %+v %v", s, err)
			}
			r := s.Reports[0]
			if r.TranslationStatus != "complete" || len(r.Files) != 2 {
				t.Fatalf("bilingual files: %+v", r)
			}
			zh, en := u7Read(t, store, r.Files[0].Hash), u7Read(t, store, r.Files[1].Hash)
			if !strings.Contains(zh, u7Text.ZhCN) || !strings.Contains(en, u7Text.En) || strings.Contains(en, u7Text.ZhCN) {
				t.Fatal("body language was relabelled")
			}
			for _, body := range []string{zh, en} {
				for _, exact := range []string{r.SourceDigest, "printf u7-marker", "`failed`", "`unknown`"} {
					if !strings.Contains(body, exact) {
						t.Fatalf("immutable value missing: %s", exact)
					}
				}
			}
			s, err = store.Update(s.Run.ID, func(s *Snapshot) error {
				addEvent(s, "tool_output", "internal", map[string]string{"stdout": "INTERNAL-RAW-OUTPUT"})
				return nil
			})
			if err != nil || len(s.Reports) != 1 {
				t.Fatalf("telemetry made a report version: %d %v", len(s.Reports), err)
			}
			if strings.Contains(u7Read(t, store, r.Files[0].Hash), "INTERNAL-RAW-OUTPUT") {
				t.Fatal("tool log became report")
			}
			s, err = store.Update(s.Run.ID, func(s *Snapshot) error { s.Run.Goal += " 新来源版本"; return nil })
			if err != nil || len(s.Reports) != 2 || s.Reports[1].TranslationStatus != "translation_missing" || s.Reports[1].SourceDigest == r.SourceDigest {
				t.Fatalf("source version reused stale translation: %+v %v", s.Reports, err)
			}
			if ReportReceipts(s)[0].SourceStatus != "changed" || u7Read(t, store, r.Files[1].Hash) != en {
				t.Fatal("old report overwritten or still current")
			}
			_, old, err := store.ReportDocument(s.Run.ID, r.ID, "en")
			if err != nil || old != en {
				t.Fatal("fixed old download changed", err)
			}
			loaded, err := store.Load(s.Run.ID)
			if err != nil || !reflect.DeepEqual(loaded.Reports, s.Reports) {
				t.Fatal("restart/read mutated report versions", err)
			}
			activity, err := store.ActivityWorkspace(s.Run.ID)
			if err != nil || activity.Totals["reports"] != 1 || len(activity.Snapshot.Reports) != 1 || activity.Snapshot.Reports[0].ID != s.Reports[1].ID || activity.Snapshot.Reports[0].SourceStatus != "not_checked" {
				t.Fatalf("activity omitted current report or implied source validation: %+v %v", activity, err)
			}
			if _, _, err = store.ReportDocument(s.Run.ID, "missing", "en"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("missing report should be not found", err)
			}
		})
	}
}

func TestU7ReportMilestonesAndTranslationNegativeControls(t *testing.T) {
	store, s := u7Store(t, "json")
	s, err := store.Update(s.Run.ID, func(s *Snapshot) error {
		s.Findings = []Finding{{ID: "finding-one", Title: "响应检查", RootCause: "当前观察尚待进一步复测确认。", Report: &u7Text}}
		s.WorkerCheckpoint = &WorkerCheckpoint{WorkerHandoff: WorkerHandoff{Progress: "当前阶段已完成文件检查，继续核对剩余来源。", NextAction: "read remaining source", Report: &u7Text}, Summary: "阶段调查记录"}
		return nil
	})
	if err != nil || len(s.Reports) != 2 {
		t.Fatalf("milestones were not saved: %+v %v", s.Reports, err)
	}
	for _, r := range s.Reports {
		if r.TranslationStatus != "complete" {
			t.Fatalf("milestone missing content: %+v", r)
		}
	}
	for _, tc := range []ReportText{{ZhCN: u7Text.ZhCN, En: "# English report"}, {ZhCN: u7Text.ZhCN, En: u7Text.ZhCN}, {ZhCN: "# 中文总结", En: u7Text.En}} {
		_, missing := (reportCandidate{source: reportSource{}, text: &tc}).bodies()
		if len(missing) == 0 {
			t.Fatal("heading/copy-only content accepted")
		}
	}
	before := s.Reports[0]
	c := reportCandidates(&s)[0]
	bad := reportTranslation{ReportText: ReportText{SourceDigest: "stale", ZhCN: u7Text.ZhCN + "错误译文", En: u7Text.En}, Kind: "finding", SubjectID: "finding-one"}
	raw, _ := json.Marshal(map[string]any{"trace_report": reportSubmission{Documents: []reportTranslation{bad}}})
	s, err = store.Update(s.Run.ID, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw), ReportOnly: true})
		return nil
	})
	if err != nil || len(s.Reports) != 2 || s.Reports[0].ID != before.ID {
		t.Fatal("stale source translation was applied", err)
	}
	bad.SourceDigest = c.digest()
	raw, _ = json.Marshal(map[string]any{"trace_report": reportSubmission{Documents: []reportTranslation{bad}}})
	s, err = store.Update(s.Run.ID, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw), ReportOnly: true})
		return nil
	})
	if err != nil || len(s.Reports) != 3 || s.Reports[2].SourceDigest != before.SourceDigest || s.Reports[2].DocumentDigest == before.DocumentDigest {
		t.Fatal("translation-only new version missing", err)
	}
}

func u7TranslationResponse(request ModelRequest) (ModelResponse, error) {
	for i := len(request.Messages) - 1; i >= 0; i-- {
		m := request.Messages[i]
		at := strings.Index(m.Content, "Pending source-bound documents: ")
		if at < 0 {
			continue
		}
		// JSON uses source/subject snake-case, decode those explicitly.
		var docs []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(m.Content[at+len("Pending source-bound documents: "):]), &docs); err != nil {
			return ModelResponse{}, err
		}
		body := u7Text
		for _, doc := range docs {
			var kind string
			json.Unmarshal(doc["kind"], &kind)
			if kind == "final" {
				json.Unmarshal(doc["source_digest"], &body.SourceDigest)
			}
		}
		if body.SourceDigest == "" {
			return ModelResponse{}, fmt.Errorf("missing final digest")
		}
		raw, _ := json.Marshal(map[string]any{"trace_report": body})
		return ModelResponse{Content: string(raw)}, nil
	}
	return ModelResponse{}, fmt.Errorf("missing delivery request")
}

func TestU7PiGoalOnlyBilingualDeliveryAndToolGate(t *testing.T) {
	for _, mode := range []string{"translate", "missing", "tool", "provider_error"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			p := settlementProviderFunc(func(_ context.Context, r ModelRequest) (ModelResponse, error) {
				calls++
				switch calls {
				case 1:
					return ModelResponse{ToolCalls: []ToolCall{{ID: "proof", Name: "exec", Arguments: `{"command":"printf u7-proof"}`}}}, nil
				case 2:
					return ModelResponse{Content: "观察已经返回预期结果，清理状态尚未获得独立确认。"}, nil
				default:
					if len(r.Tools) != 0 {
						t.Error("delivery exposed tools")
					}
					if mode == "provider_error" {
						return ModelResponse{}, errors.New("invalid report provider credentials")
					}
					if mode == "missing" && calls == 3 {
						return ModelResponse{Content: "translation still pending"}, nil
					}
					if mode == "tool" && calls == 3 {
						return ModelResponse{ToolCalls: []ToolCall{{ID: "forbidden", Name: "exec", Arguments: `{"command":"printf should-not-run"}`}}}, nil
					}
					return u7TranslationResponse(r)
				}
			})
			a, store := newTestAgent(t, p)
			a.SetSuccessContains("u7-proof")
			id := createTestRun(t, a)
			s, err := a.Run(context.Background(), id)
			wantCalls := 3
			if mode == "missing" || mode == "tool" {
				wantCalls = 4
			}
			if err != nil && mode != "provider_error" || calls != wantCalls || len(s.Actions) != 1 || len(pendingToolCalls(&s)) != 0 || s.Result.Status != "success" || s.Result.CleanupStatus != "unknown" {
				t.Fatalf("delivery changed execution/business: calls=%d actions=%d result=%+v err=%v", calls, len(s.Actions), s.Result, err)
			}
			if mode != "provider_error" {
				if s.Run.Status != StatusCompleted || s.Reports[len(s.Reports)-1].TranslationStatus != "complete" {
					t.Fatalf("goal-only bilingual delivery: %+v", s)
				}
			} else {
				if s.Run.Status != StatusFailed || s.Run.PauseReason != "" || !s.ReportDelivery.Pending {
					t.Fatalf("missing content claimed completion: %+v", s.Run)
				}
				a.provider = settlementProviderFunc(func(_ context.Context, r ModelRequest) (ModelResponse, error) {
					if len(r.Tools) > 0 {
						t.Error("resume exposed tools")
					}
					return u7TranslationResponse(r)
				})
				s, err = a.Run(context.Background(), id)
				if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 1 || s.Result.Status != "success" {
					t.Fatalf("report-only resume: %+v %v", s.Run, err)
				}
			}
			s, err = store.Update(id, func(s *Snapshot) error { s.Evidence[0].Content += "tampered"; return nil })
			if err != nil || s.Result.Status == "success" || s.Evidence[0].Verification == "verified" || s.Reports[len(s.Reports)-1].TranslationStatus == "complete" {
				t.Fatal("translation hid a changed execution source", err)
			}
		})
	}
}

func TestU7MissingTranslationPreservesBusinessFailureAndPlainChat(t *testing.T) {
	s := Snapshot{Run: Run{Status: StatusCompleted}, Messages: []Message{{Role: "assistant", Content: "简单对话回复"}}}
	if continueForReport(&s) || s.Run.Status != StatusCompleted {
		t.Fatal("plain chat forced an extra round")
	}
	raw, _ := json.Marshal(map[string]any{"trace_result": Result{Status: "failed", Summary: "执行未达到预期结果，保持失败结论。"}})
	s = Snapshot{Run: Run{Status: StatusCompleted}, Messages: []Message{{Role: "assistant", Content: string(raw)}}, Actions: []Action{{Name: "exec", Adapter: "exec", Status: "completed", Result: &ToolResult{ExitCode: 1}}}}
	if !continueForReport(&s) {
		t.Fatal("missing report not requested")
	}
	s.Run.Status = StatusCompleted
	s.Messages = append(s.Messages, Message{Role: "assistant", ReportOnly: true, Content: `{"trace_result":{"status":"success","summary":"fake success"}}`})
	if !continueForReport(&s) {
		t.Fatal("missing translation silently completed")
	}
	refreshResult(&s)
	if s.Result.Status != "failed" || s.Run.Status != StatusRunning || s.Run.PauseReason != "" || strings.Contains(s.Result.Summary, "fake") {
		t.Fatalf("translation replaced business result: %+v", s.Result)
	}
}

func TestU7SourceSearchTriggersReportDelivery(t *testing.T) {
	for _, code := range []int{0, 1} {
		s := Snapshot{Run: Run{Status: StatusCompleted}, Messages: []Message{{Role: "assistant", Content: "检索已经返回部分结果，相关条件仍需继续核对。"}}, Actions: []Action{{Name: "source_search", Adapter: "builtin", Result: &ToolResult{ExitCode: code}}}}
		if !continueForReport(&s) || !reportDeliveryPending(&s) {
			t.Fatalf("search result omitted delivery: exit=%d", code)
		}
	}
}

func TestU7PendingSourceRefreshAndSharedFailure(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	raw, _ := json.Marshal(map[string]any{"trace_result": Result{Status: "failed", Summary: "已观察到业务失败，保留这一明确结论。"}})
	s, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusCompleted
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
		s.Actions = []Action{{Name: "exec", Adapter: "exec", Status: "completed", Result: &ToolResult{ExitCode: 1}}}
		continueForReport(s)
		return nil
	})
	if err != nil || s.Result.Status != "failed" {
		t.Fatalf("initial delivery: %+v %v", s.Result, err)
	}
	oldDigest, messages := s.ReportDelivery.DocumentsDigest, len(s.Messages)
	s, err = store.Update(id, func(s *Snapshot) error {
		s.Run.Goal += " 新来源"
		continueForReport(s, true)
		return nil
	})
	if err != nil || s.ReportDelivery.DocumentsDigest == oldDigest || len(s.Messages) != messages+1 {
		t.Fatalf("changed source did not refresh request: %+v %v", s.ReportDelivery, err)
	}
	versions := len(s.Reports)
	for i := 0; i < 3; i++ {
		s, err = store.Update(id, func(s *Snapshot) error { continueForReport(s, true); return nil })
		if err != nil || len(s.Messages) != messages+1 || len(s.Reports) != versions {
			t.Fatalf("unchanged delivery grew context or versions: %v", err)
		}
	}
	s, err = a.fail(id, errors.New("pi bridge failed during report delivery"))
	if err == nil || s.Run.Status != StatusFailed || s.Run.PauseReason != "" || s.Result.Status != "failed" || len(s.Actions) != 1 {
		t.Fatalf("shared failure changed business: %+v %+v %v", s.Run, s.Result, err)
	}
}

func TestU7CurrentReportOnlyBusinessReplyWins(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"trace_report": u7Text})
	s := Snapshot{Run: Run{Status: StatusCompleted}, Messages: []Message{{Role: "assistant", Content: "old answer"}, {Role: "assistant", Content: string(raw)}}}
	refreshResult(&s)
	if s.Result.Summary != u7Text.ZhCN {
		t.Fatalf("report-only final selected older business text: %q", s.Result.Summary)
	}
}

func TestU7DefaultDownloadPrefersFinalOverFindingTranslation(t *testing.T) {
	store, s := u7Store(t, "json")
	raw, _ := json.Marshal(map[string]any{"trace_result": Result{Status: "failed", Summary: u7Text.ZhCN, Report: &u7Text}})
	s, err := store.Update(s.Run.ID, func(s *Snapshot) error {
		s.Run.Status = StatusCompleted
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
		s.Findings = []Finding{{ID: "finding", Title: "检查结果", RootCause: u7Text.ZhCN}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	final := preferredReport(s)
	if final == nil || final.Kind != "final" {
		t.Fatal("no final document")
	}
	for _, candidate := range reportCandidates(&s) {
		if candidate.source.Kind != "finding" {
			continue
		}
		doc := reportTranslation{Kind: "finding", SubjectID: "finding", ReportText: u7Text}
		doc.SourceDigest = candidate.digest()
		raw, _ = json.Marshal(map[string]any{"trace_report": reportSubmission{Documents: []reportTranslation{doc}}})
	}
	s, err = store.Update(s.Run.ID, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "assistant", ReportOnly: true, Content: string(raw)})
		return nil
	})
	if err != nil || s.Reports[len(s.Reports)-1].Kind != "finding" {
		t.Fatalf("finding translation not appended: %v", err)
	}
	ref, _, err := store.ReportDocument(s.Run.ID, "", "en")
	if err != nil || ref.Hash != final.Files[1].Hash {
		t.Fatalf("default download chose a finding: %+v %v", ref, err)
	}
}
