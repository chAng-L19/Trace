package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWorkerReportDeliveryReauditsInvalidatedBusinessSource(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, _ := u7Store(t, backend)
			path := filepath.Join(t.TempDir(), "proof.txt")
			const marker = "report-worker-proof"
			if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
			var a *Agent
			var parentID, actionsHash, oldDigest string
			var calls atomic.Int32
			var audited atomic.Bool
			provider := settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
				n := calls.Add(1)
				if n == 1 {
					args, _ := json.Marshal(map[string]string{"path": path})
					return ModelResponse{ToolCalls: []ToolCall{{ID: "read-proof", Name: "read", Arguments: string(args)}}}, nil
				}
				if n == 2 {
					var result ToolResult
					if err := json.Unmarshal([]byte(lastConversationMessage(request).Content), &result); err != nil {
						return ModelResponse{}, err
					}
					claim := Result{Status: "success", Summary: "已读取固定验证标记，原始读取来源保留，双语文件报告尚待交付。", EvidenceIDs: []string{result.EvidenceID}, Answers: []Answer{{Value: marker, EvidenceIDs: []string{result.EvidenceID}}}}
					raw, _ := json.Marshal(map[string]any{"trace_result": claim})
					return ModelResponse{Content: string(raw)}, nil
				}
				parent, err := store.Load(parentID)
				if err != nil {
					return ModelResponse{}, err
				}
				w := parent.Workers[0]
				child, err := store.Load(w.RunID)
				if err != nil {
					return ModelResponse{}, err
				}
				switch n {
				case 3:
					// Invalidate the source during the first live visit, before any
					// settlement or report-only resume/watchdog boundary exists.
					if w.Visit != 1 || w.SettledVisit != nil || child.WorkerSettlement != nil || parent.Plan.Tasks[0].Status != "running" || !reportBusinessReady(&child) || len(request.Tools) != 0 {
						return ModelResponse{}, fmt.Errorf("not the first live report delivery: worker=%+v run=%+v", w, child.Run)
					}
					actionsHash, oldDigest = valueHash(child.Actions), child.ReportDelivery.DocumentsDigest
					if _, err := store.Update(w.RunID, func(s *Snapshot) error {
						s.Evidence[0].Content += " changed"
						return nil
					}); err != nil {
						return ModelResponse{}, err
					}
					return u7TranslationResponse(request) // The stale digest must be rejected.
				case 4:
					if len(request.Tools) != 0 || child.Result.Status != "incomplete" || child.ReportDelivery.DocumentsDigest == oldDigest {
						return ModelResponse{}, fmt.Errorf("source loss did not refresh report delivery: %+v", child.Result)
					}
					return u7TranslationResponse(request) // Bind both languages to the new digest.
				case 5:
					feedback := lastConversationMessage(request).Content
					if len(request.Tools) == 0 || !strings.HasPrefix(feedback, "Runtime completion audit:") || !strings.Contains(feedback, "evidence_source_") || reportDeliveryPending(&child) {
						return ModelResponse{}, fmt.Errorf("report completion did not return to the existing business audit: %s", feedback)
					}
					audited.Store(true)
					// Stop only the fixture after observing correction; this is an
					// explicit operator pause, not a production retry/time budget.
					_, err := a.Pause(w.RunID)
					return ModelResponse{Content: "Fixture stopped after the completion audit."}, err
				default:
					return ModelResponse{}, fmt.Errorf("unexpected fixture model request %d", n)
				}
			})
			var err error
			a, err = New(store, provider, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.StopWorkers(context.Background()); a.CloseMCP() })
			a.SetRequireVerifiedCompletion(true)
			parent, err := a.Create("Read the marker and deliver substantive Chinese and English file reports.")
			if err != nil {
				t.Fatal(err)
			}
			parentID = parent.Run.ID
			if _, err := a.SetPlan(parentID, []PlanTask{{ID: "work", Goal: "Read " + path, SuccessContains: marker}}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(parentID, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
				t.Fatal(err)
			}
			w, err := a.DispatchTask(context.Background(), parentID, "work")
			if err != nil {
				t.Fatal(err)
			}
			parent, err = waitAllTestWorkers(a, context.Background(), parentID, 10)
			if err != nil {
				t.Fatal(err)
			}
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if !audited.Load() || calls.Load() != 5 || child.Run.Status != StatusPaused || child.Run.PauseReason != "operator_pause" || parent.Workers[0].Status != "paused" || parent.Workers[0].CleanupStatus != "completed" || occupiedWorkerSlots(&parent) != 0 {
				var resumeErr error
				if child.Run.Status == StatusCompleted {
					_, resumeErr = a.ResumeWorker(context.Background(), parentID, w.ID)
				}
				t.Fatalf("invalid source escaped completion audit: audited=%t calls=%d run=%+v result=%+v worker=%+v occupied=%d resume=%v", audited.Load(), calls.Load(), child.Run, child.Result, parent.Workers[0], occupiedWorkerSlots(&parent), resumeErr)
			}
			if valueHash(child.Actions) != actionsHash || len(child.Actions) != 1 || child.Evidence[0].Verification == "verified" || child.Result.Status == "success" || taskDone(parent.Plan.Tasks[0]) || child.Run.Budget != (Budget{}) {
				t.Fatalf("correction replayed work, accepted revoked proof or changed the budget: run=%+v result=%+v", child.Run, child.Result)
			}
		})
	}
}
