package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run through the real pi scheduler: the first report requires translation,
// input arrives during that request, and only the next business turn may read it.
func TestU7ReportDeliveryDefersBusinessInputs(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, input := range []string{"follow_up", "worker_local", "worker_child"} {
			for _, retry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/retry=%t", backend, input, retry), func(t *testing.T) {
					store, _ := u7Store(t, backend)
					dir := t.TempDir()
					original, next := filepath.Join(dir, "original.txt"), filepath.Join(dir, "next.txt")
					for path, text := range map[string]string{original: "u7-original-marker", next: "u7-next-marker"} {
						if err := os.WriteFile(path, []byte(text), 0600); err != nil {
							t.Fatal(err)
						}
					}
					follow := "After the report, read the new file " + next
					steer := "Keep both observations distinct in the next business reply."
					var a *Agent
					var receiver, sender, to string
					calls, nextBusiness := 0, 4
					if retry {
						nextBusiness++
					}
					checkInput := func(delivered bool) error {
						s, err := store.Load(receiver)
						if err != nil {
							return err
						}
						if input == "follow_up" {
							if len(s.Mailbox) != 2 {
								return fmt.Errorf("mailbox lost input: %d", len(s.Mailbox))
							}
							for _, q := range s.Mailbox {
								if q.ConsumedAt.IsZero() == delivered {
									return fmt.Errorf("input prematurely consumed or not consumed: %+v", q)
								}
							}
						} else {
							root, err := store.Observe(sender)
							if err != nil {
								return err
							}
							if len(root.WorkerMessages) != 1 || root.WorkerMessages[0].Delivered != delivered {
								return fmt.Errorf("worker acknowledgement mismatch: %+v", root.WorkerMessages)
							}
						}
						count := 0
						for _, m := range s.Messages {
							if m.Role == "user" && strings.Contains(m.Content, follow) {
								count++
							}
						}
						want := 0
						if delivered {
							want = 1
						}
						if count != want {
							return fmt.Errorf("business input appended %d times, want %d", count, want)
						}
						return nil
					}
					read := func(id, path string) ModelResponse {
						args, _ := json.Marshal(map[string]string{"path": path})
						return ModelResponse{ToolCalls: []ToolCall{{ID: id, Name: "read", Arguments: string(args)}}}
					}
					provider := settlementProviderFunc(func(_ context.Context, r ModelRequest) (ModelResponse, error) {
						calls++
						if calls == 1 {
							return read("original-read", original), nil
						}
						if calls == 2 {
							return ModelResponse{Content: "原始文件已经返回预期标记，保留这次读取的来源与清理状态。"}, nil
						}
						if calls >= 3 && calls < nextBusiness {
							if len(r.Tools) != 0 {
								return ModelResponse{}, fmt.Errorf("report request exposed tools")
							}
							if calls == 3 {
								if input == "follow_up" {
									if _, err := a.QueueMessage(receiver, "steer", steer); err != nil {
										return ModelResponse{}, err
									}
									if _, err := a.QueueMessage(receiver, "follow_up", follow); err != nil {
										return ModelResponse{}, err
									}
								} else if _, err := a.SendWorkerMessage(sender, "parent", to, follow); err != nil {
									return ModelResponse{}, err
								}
							}
							// Exercise an external delivery attempt, too: it must not ack
							// the sender while this receiver is still translating.
							if err := a.DeliverWorkerMessages(receiver); err != nil {
								return ModelResponse{}, err
							}
							if err := checkInput(false); err != nil {
								return ModelResponse{}, err
							}
							for _, m := range r.Messages {
								if m.Role == "user" && strings.Contains(m.Content, follow) {
									return ModelResponse{}, fmt.Errorf("business input leaked into translation request")
								}
							}
							if retry && calls == 3 {
								return ModelResponse{Content: "English translation remains pending."}, nil
							}
							return u7TranslationResponse(r)
						}
						if calls == nextBusiness {
							if len(r.Tools) == 0 {
								return ModelResponse{}, fmt.Errorf("business continuation still has report-only tools")
							}
							if err := checkInput(true); err != nil {
								return ModelResponse{}, err
							}
							s, err := store.Load(receiver)
							if err != nil {
								return ModelResponse{}, err
							}
							complete := false
							for _, report := range s.Reports {
								complete = complete || report.Kind == "final" && report.TranslationStatus == "complete"
							}
							if reportDeliveryPending(&s) || !complete {
								return ModelResponse{}, fmt.Errorf("new business started before report delivery completed")
							}
							seenFollow, seenSteer := false, input != "follow_up"
							for _, m := range r.Messages {
								seenFollow = seenFollow || m.Role == "user" && strings.Contains(m.Content, follow)
								seenSteer = seenSteer || m.Role == "user" && m.Content == steer
							}
							if !seenFollow || !seenSteer {
								return ModelResponse{}, fmt.Errorf("queued inputs did not reach next business request")
							}
							return read("next-read", next), nil
						}
						if calls == nextBusiness+1 {
							body := ReportText{ZhCN: "原文件与新增文件均已读取，分别保留两次观察来源。新增指令在首份报告交付后执行，清理状态仍待独立核对。", En: "Both the original and additional files were read, retaining separate observation sources. The additional instruction ran after delivery of the first report. Cleanup remains independently unverified."}
							raw, _ := json.Marshal(map[string]any{"trace_report": body})
							return ModelResponse{Content: "已读取新增文件并保留原始读取记录。\n\n```json\n" + string(raw) + "\n```"}, nil
						}
						return ModelResponse{}, fmt.Errorf("unexpected request %d", calls)
					})
					var err error
					a, err = New(store, provider, "")
					if err != nil {
						t.Fatal(err)
					}
					a.SetSuccessContains("u7-original-marker")
					run, err := a.Create("Read the original file, then deliver its bilingual report.")
					if err != nil {
						t.Fatal(err)
					}
					receiver, sender, to = run.Run.ID, run.Run.ID, "parent"
					if input == "worker_child" {
						parent, err := a.Create("Parent message routing")
						if err != nil {
							t.Fatal(err)
						}
						sender, to = parent.Run.ID, "worker"
						if _, err = store.Update(receiver, func(s *Snapshot) error {
							s.Run.ParentRunID, s.Run.ParentTaskID = sender, "task"
							s.RootContract = nil
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						if _, err = store.Update(sender, func(s *Snapshot) error {
							s.Workers = []AgentWorker{{ID: to, TaskID: "task", RunID: receiver, Status: "completed", CleanupStatus: "completed"}}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						// This test owns the existing child execution, as a dispatched
						// worker agent does; it does not ask the parent to resume it.
						a.workerRunID = receiver
					}
					s, err := a.Run(context.Background(), receiver)
					if err != nil {
						t.Fatal(err)
					}
					if err != nil || s.Run.Status != StatusCompleted || s.Result.Status != "success" || calls != nextBusiness+1 || len(s.Actions) != 2 || reportDeliveryPending(&s) {
						t.Fatalf("continuation: %+v result=%+v calls=%d actions=%d err=%v", s.Run, s.Result, calls, len(s.Actions), err)
					}
					if err := checkInput(true); err != nil {
						t.Fatal(err)
					}
					for i, id := range []string{"original-read", "next-read"} {
						if s.Actions[i].CallID != id || s.Actions[i].Status != "completed" || s.Actions[i].Result == nil || s.Actions[i].Result.ExitCode != 0 {
							t.Fatalf("read replayed, reordered or failed: %+v", s.Actions)
						}
					}
				})
			}
		}
	}
}

// Force the observation/commit interleaving without sleeps: the caller holds a
// pre-delivery sender projection while the receiver enters report-only mode.
func TestU7WorkerInboxStaleObservationDoesNotAcknowledge(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, _ := u7Store(t, backend)
			a, err := New(store, successProvider{}, "")
			if err != nil {
				t.Fatal(err)
			}
			parent, err := a.Create("sender")
			if err != nil {
				t.Fatal(err)
			}
			child, err := a.Create("receiver")
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update(parent.Run.ID, func(s *Snapshot) error { s.Workers = []AgentWorker{{ID: "worker", RunID: child.Run.ID}}; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.SendWorkerMessage(parent.Run.ID, "parent", "worker", "read the next file"); err != nil {
				t.Fatal(err)
			}
			stale, err := store.Observe(parent.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update(child.Run.ID, func(s *Snapshot) error {
				s.ReportDelivery = &ReportDeliveryState{Pending: true}
				s.Run.Status = StatusRunning
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = a.deliverWorkerInbox(child.Run.ID, parent.Run.ID, "worker", stale); err != nil {
				t.Fatal(err)
			}
			root, _ := store.Observe(parent.Run.ID)
			receiver, _ := store.Load(child.Run.ID)
			if root.WorkerMessages[0].Delivered || len(receiver.Messages) != 0 {
				t.Fatal("stale delivery acknowledged a receiver that deferred input")
			}
			_, err = store.Update(child.Run.ID, func(s *Snapshot) error { s.ReportDelivery.Pending = false; return nil })
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err = a.deliverWorkerInbox(child.Run.ID, parent.Run.ID, "worker", stale); err != nil {
					t.Fatal(err)
				}
			}
			root, _ = store.Observe(parent.Run.ID)
			receiver, _ = store.Load(child.Run.ID)
			if !root.WorkerMessages[0].Delivered || len(receiver.Messages) != 1 {
				t.Fatal("post-report delivery was lost or duplicated")
			}
		})
	}
}
