package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func finiteWork(id string) WorkItem {
	return WorkItem{ID: id, Goal: "verify object " + id, Identity: EvidencePredicate{Kind: "json_equals", Pointer: "/object", Expected: id}, Requirement: VerificationRequirement{ID: "proof-" + id, Description: "object is verified", Category: "goal", ToolID: "exec", Predicate: EvidencePredicate{Kind: "json_equals", Pointer: "/ok", Expected: true}}, NextAction: "inspect object " + id}
}

func TestFiniteCoverageBindsIdentityAndOutcomeToSameObservation(t *testing.T) {
	source := Snapshot{Run: Run{ID: "child", ParentTaskID: "stage"}, artifactRoot: t.TempDir()}
	task := PlanTask{ID: "stage", Status: "completed", ChildRunID: "child"}
	item := finiteWork("A")
	resultValidationSource(t, &source, "a-failed", "exec", "exec", `{"object":"A","ok":false}`)
	resultValidationSource(t, &source, "b-pass", "exec", "exec", `{"object":"B","ok":true}`)
	if proof := workItemProof(&source, task, item); len(proof) != 0 {
		t.Fatal("joined identity and outcome from different observations")
	}
	resultValidationSource(t, &source, "a-pass", "exec", "exec", `{"object":"A","ok":true}`)
	if proof := workItemProof(&source, task, item); len(proof) != 1 || proof[0] != "child/a-pass" {
		t.Fatalf("missing original proof: %v", proof)
	}
	source.Actions[len(source.Actions)-1].Status = "uncertain"
	if proof := workItemProof(&source, task, item); len(proof) != 0 {
		t.Fatal("uncertain action proved success")
	}
	source.Actions[len(source.Actions)-1].Status = "completed"
	source.Run.ParentTaskID = "foreign"
	if proof := workItemProof(&source, task, item); len(proof) != 0 {
		t.Fatal("foreign child proved coverage")
	}
	duplicate := finiteWork("B")
	duplicate.Identity = item.Identity
	if err := validateTaskWork(PlanTask{ID: "duplicate", WorkItems: []WorkItem{item, duplicate}}); err == nil {
		t.Fatal("one identity declared as two objects")
	}

}

func TestWorkPlanPreservesCoverageAndContinuationIdentity(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	source := PlanTask{ID: "stage", Goal: "initial stage", WorkItems: []WorkItem{finiteWork("A"), finiteWork("B")}, ResourceKeys: []string{"shared"}}
	if _, err := a.SetPlan(id, []PlanTask{source}); err != nil {
		t.Fatal(err)
	}
	weakened := source
	weakened.WorkItems = weakened.WorkItems[:1]
	if _, err := a.SetPlan(id, []PlanTask{weakened}); err == nil {
		t.Fatal("finite coverage was weakened")
	}
	if _, err := a.SetPlan(id, nil); err == nil {
		t.Fatal("finite scope was removed")
	}
	item := source.WorkItems[1]
	next := PlanTask{ID: "next", Goal: item.Goal, Requirements: []VerificationRequirement{item.Requirement}, Dependencies: []string{source.ID}, ResourceKeys: source.ResourceKeys, Continues: []WorkReference{{TaskID: source.ID, ItemID: item.ID}}}
	if _, err := a.SetPlan(id, []PlanTask{source, next}); err != nil {
		t.Fatal(err)
	}
	duplicate := next
	duplicate.ID = "duplicate"
	if _, err := a.SetPlan(id, []PlanTask{source, next, duplicate}); err == nil {
		t.Fatal("same obligation linked twice")
	}
	for _, status := range []string{"blocked", "abandoned", "superseded"} {
		duplicate.Status = status
		if err := validateWorkLinks([]PlanTask{next, duplicate, source}); err != nil {
			t.Fatalf("disposed %s conflicted with the live successor or source order: %v", status, err)
		}
	}
	next.Requirements = nil
	if _, err := a.SetPlan(id, []PlanTask{source, next}); err == nil {
		t.Fatal("continuation dropped acceptance")
	}
	snap, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	text, through := plannerContextFrom(&snap)
	snap.Plan.ReasonedThrough = through
	again, _ := plannerContextFrom(&snap)
	if !strings.Contains(text, "work_progress") || !strings.Contains(again, `"item_id":"B"`) {
		t.Fatal("consumed observations hid remaining work")
	}
}

func TestRemainingWorkSurvivesClaimsAndRestart(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := newTestAgent(t, MockProvider{})
			if backend == "sqlite" {
				store = sqliteTestStore(t)
				var err error
				a, err = New(store, MockProvider{}, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, []PlanTask{{ID: "source", Goal: "first stage"}}); err != nil {
				t.Fatal(err)
			}
			child := Snapshot{Run: Run{ParentTaskID: "source"}, Result: Result{Remaining: []RemainingWork{{ID: "open", Goal: "research remaining object", NextAction: "read existing state"}}}}
			if _, err := store.Update(id, func(s *Snapshot) error {
				if !mergeRemainingWork(&s.Plan.Tasks[0], &child) {
					t.Fatal("work not received")
				}
				child.Result.Summary = "new report visit"
				if mergeRemainingWork(&s.Plan.Tasks[0], &child) {
					t.Fatal("new receipt duplicated obligation")
				}
				child.Result.Remaining = nil
				mergeRemainingWork(&s.Plan.Tasks[0], &child)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			snap, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Plan.Tasks[0].Remaining) != 1 || len(workProgressGaps(&snap)) != 1 {
				t.Fatal("unresolved work disappeared")
			}
			original := snap.Plan.Tasks[0]
			next := PlanTask{ID: "next", Goal: original.Remaining[0].Goal, Dependencies: []string{"source"}, SuccessContains: "verified remaining object", Continues: []WorkReference{{TaskID: "source", ItemID: "open"}}}
			if _, err = a.SetPlan(id, []PlanTask{{ID: "source", Goal: "first stage"}, next}); err != nil {
				t.Fatal(err)
			}
			snap, err = store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Plan.Tasks[0].Remaining) != 1 || snap.Plan.WorkProgress[0].Status != "scheduled" {
				t.Fatal("source rewrite erased remaining work")
			}
			if _, err = a.ClaimTask(id, "next", "worker", 30); err == nil {
				t.Fatal("unfinished source allowed continuation")
			}
			// Persisted plan, not the report summary, is authoritative after reopen.
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := reopened.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Plan.Tasks[0].Remaining) != 1 {
				t.Fatal("restart lost obligation")
			}
		})
	}
}

func TestRemainingResultValidationAndSourceReceipt(t *testing.T) {
	s := Snapshot{}
	if err := validateRemainingWork(&s, []RemainingWork{{ID: "x", Goal: "goal"}}); err == nil {
		t.Fatal("blank next action accepted")
	}
	work := RemainingWork{ID: "x", Goal: "goal", NextAction: "observe", EvidenceIDs: []string{"invented"}}
	if err := validateRemainingWork(&s, []RemainingWork{work}); err == nil {
		t.Fatal("invented source accepted")
	}
	work.EvidenceIDs = nil
	s.Result = Result{Remaining: []RemainingWork{work}}
	s.Run.ParentTaskID = "task"
	r := newChildReport("parent", AgentWorker{TaskID: "task", RunID: "child"}, &s, "completed")
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"remaining"`) || len(r.Unresolved) == 0 {
		t.Fatal("receipt lost next action")
	}
}

// Exercise actual dispatch, execution evidence, settlement and a reopened Store.
func TestFiniteWorkContinuationEndToEnd(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			provider := settlementProviderFunc(func(_ context.Context, r ModelRequest) (ModelResponse, error) {
				object, requirement := "A", "first-proof"
				for _, m := range r.Messages {
					if strings.Contains(m.Content, `"task_id":"next-b"`) {
						object, requirement = "B", "proof-B"
					}
					if strings.Contains(m.Content, `"task_id":"next-c"`) {
						object, requirement = "C", "proof-C"
					}
				}
				last := lastConversationMessage(r)
				if last.Role != "tool" {
					args, _ := json.Marshal(map[string]string{"command": "printf '{\"object\":\"" + object + "\",\"ok\":true}'"})
					return ModelResponse{ToolCalls: []ToolCall{{ID: "probe", Name: "exec", Arguments: string(args)}}}, nil
				}
				var evidenceID string
				for _, m := range r.Messages {
					if m.ToolCallID == "probe" {
						var out ToolResult
						if json.Unmarshal([]byte(m.Content), &out) == nil {
							evidenceID = out.EvidenceID
						}
					}
				}
				if last.ToolCallID == "probe" {
					args, _ := json.Marshal(map[string]any{"requirement_id": requirement, "evidence_ids": []string{evidenceID}})
					return ModelResponse{ToolCalls: []ToolCall{{ID: "check", Name: "evidence_check", Arguments: string(args)}}}, nil
				}
				claim := Result{Status: "success", EvidenceIDs: []string{evidenceID}, Summary: "verified " + object}
				if object == "A" {
					claim.Remaining = []RemainingWork{{ID: "C", Goal: "verify object C", NextAction: "inspect object C", EvidenceIDs: []string{evidenceID}}}
				}
				body, _ := json.Marshal(map[string]Result{"trace_result": claim})
				return ModelResponse{Content: fixtureReport(string(body))}, nil
			})
			a, store := newTestAgent(t, provider)
			if backend == "sqlite" {
				store = sqliteTestStore(t)
				var err error
				a, err = New(store, provider, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			defer a.CloseMCP()
			id := createTestRun(t, a)
			source := PlanTask{ID: "stage", Goal: "first stage", Requirements: []VerificationRequirement{taskRequirement("first-proof")}, WorkItems: []WorkItem{finiteWork("A"), finiteWork("B")}}
			earlyA := PlanTask{ID: "early-a", Goal: finiteWork("A").Goal, Requirements: []VerificationRequirement{finiteWork("A").Requirement}, Dependencies: []string{"stage"}, Continues: []WorkReference{{TaskID: "stage", ItemID: "A"}}}
			if _, err := a.SetPlan(id, []PlanTask{source, earlyA}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.DispatchFrontier(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			partial, err := waitAllTestWorkers(a, context.Background(), id, 15)
			if err != nil {
				state, _ := store.Load(id)
				for _, w := range state.Workers {
					child, _ := store.Load(w.RunID)
					t.Logf("child status=%s result=%+v last_messages=%+v", child.Run.Status, child.Result, child.Messages[max(0, len(child.Messages)-3):])
				}
				t.Fatal(err)
			}
			if !taskDone(partial.Plan.Tasks[0]) || len(partial.Plan.Tasks[0].Remaining) != 1 || len(workProgressGaps(&partial)) != 2 {
				t.Fatalf("partial source lost remaining coverage: %+v %+v", partial.Plan, partial.Workers)
			}
			if _, err := a.DispatchTask(context.Background(), id, "early-a"); err == nil {
				t.Fatal("resolved obligation dispatched from an old pending link")
			} else {
				var resolved *WorkAlreadyResolvedError
				if !errors.As(err, &resolved) {
					t.Fatalf("resolved continuation returned untyped error: %v", err)
				}
			}
			duplicate := earlyA
			duplicate.ID = "duplicate-a"
			if _, err := a.SetPlan(id, []PlanTask{source, earlyA, duplicate}); err == nil {
				t.Fatal("resolved obligation accepted a new link")
			}
			probe := partial
			resultValidationSource(t, &probe, "root", "exec", "exec", "root-stage-proof")
			resultValidationClaim(t, &probe, Result{Status: "success", EvidenceIDs: []string{"root"}})
			if probe.Result.Status == "success" {
				t.Fatal("partial coverage completed root")
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			a.store = reopened
			restored, err := reopened.Load(id)
			if err != nil || len(workProgressGaps(&restored)) != 2 {
				t.Fatalf("restart lost remaining: %v", err)
			}
			nextB := PlanTask{ID: "next-b", Goal: finiteWork("B").Goal, Requirements: []VerificationRequirement{finiteWork("B").Requirement}, Dependencies: []string{"stage"}, Continues: []WorkReference{{TaskID: "stage", ItemID: "B"}}}
			nextC := PlanTask{ID: "next-c", Goal: "verify object C", Requirements: []VerificationRequirement{finiteWork("C").Requirement}, Dependencies: []string{"stage"}, Continues: []WorkReference{{TaskID: "stage", ItemID: "C"}}}
			if _, err := a.SetPlan(id, []PlanTask{source, earlyA, nextB, nextC}); err != nil {
				t.Fatal(err)
			}
			launched, err := a.DispatchFrontier(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if len(launched) != 2 {
				t.Fatalf("obsolete ready link blocked independent successors: %+v", launched)
			}
			pending, err := reopened.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			obsolete, err := planTask(&pending, "early-a")
			if err != nil || obsolete.Status != "pending" || obsolete.Started || obsolete.WorkerID != "" || obsolete.ChildRunID != "" {
				t.Fatalf("resolved continuation executed: %+v %v", obsolete, err)
			}
			if _, err := a.SetPlan(id, []PlanTask{source, nextB, nextC}); err != nil {
				t.Fatal(err)
			}
			done, err := waitAllTestWorkers(a, context.Background(), id, 15)
			if err != nil {
				t.Fatal(err)
			}
			if gaps := workProgressGaps(&done); len(gaps) > 0 {
				t.Fatalf("successor lost source-bound proof: %v; %+v", gaps, done.Plan)
			}
			resultValidationSource(t, &done, "root", "exec", "exec", "root-stage-proof")
			resultValidationClaim(t, &done, Result{Status: "success", EvidenceIDs: []string{"root"}})
			if done.Result.Status != "success" {
				t.Fatalf("complete work blocked root: %+v", done.Result)
			}
			if _, err := reopened.Update(done.Plan.Tasks[1].ChildRunID, func(child *Snapshot) error { child.Evidence[0].Content += "tampered"; return nil }); err != nil {
				t.Fatal(err)
			}
			revoked, err := reopened.Load(id)
			if err != nil || len(workProgressGaps(&revoked)) == 0 {
				t.Fatal("changed source retained finite coverage", err)
			}
		})
	}
}

func TestFiniteWorkAcceptsExistingSameContractReplacement(t *testing.T) {
	item := finiteWork("A")
	original := PlanTask{ID: "old", Goal: "verify A", Status: "superseded", ReplacementTaskID: "new", WorkItems: []WorkItem{item}}
	next := original
	next.ID = "new"
	next.Status = "completed"
	next.ReplacementTaskID = ""
	next.ChildRunID = "child"
	child := Snapshot{Run: Run{ID: "child", ParentTaskID: "new"}, artifactRoot: t.TempDir()}
	resultValidationSource(t, &child, "proof", "exec", "exec", `{"object":"A","ok":true}`)
	parent := Snapshot{Run: Run{ID: "parent"}, Plan: PlanState{Tasks: []PlanTask{original, next}}}
	bindWorkSource(&parent, 1, &child)
	refreshWorkProgress(&parent, func(id string) *Snapshot {
		if id == "child" {
			return &child
		}
		return nil
	})
	if gaps := workProgressGaps(&parent); len(gaps) > 0 {
		t.Fatalf("same-contract replacement lost proof: %v", gaps)
	}
	if taskDone(original) {
		t.Fatal("retired ID became a successful dependency")
	}
	parent.Plan.Tasks[1].WorkItems = nil
	refreshWorkProgress(&parent, func(id string) *Snapshot {
		if id == "child" {
			return &child
		}
		return nil
	})
	if len(workProgressGaps(&parent)) == 0 {
		t.Fatal("weakened replacement covered original work")
	}
}

func bindWorkSource(parent *Snapshot, index int, child *Snapshot) {
	task := &parent.Plan.Tasks[index]
	visit := uint64(1)
	w := AgentWorker{ID: "worker-" + task.ID, TaskID: task.ID, RunID: child.Run.ID, Visit: visit, SettledVisit: &visit, Status: "completed", CleanupStatus: "completed"}
	parent.Workers = append(parent.Workers, w)
	task.WorkerID, task.EvidenceRunID = w.ID, child.Run.ID
	child.Run.ParentRunID, child.Run.ParentTaskID, child.Run.Responsibility, child.Run.Status = parent.Run.ID, task.ID, ResponsibilityWorker, StatusCompleted
	child.WorkerContext = &WorkerContext{RunID: child.Run.ID, ParentRunID: parent.Run.ID, TaskID: task.ID, WorkerID: w.ID}
	child.delegatedDescendantsRetired = true
	child.Result.Status = "success"
	child.WorkerSettlement = &WorkerSettlement{ParentRunID: parent.Run.ID, TaskID: task.ID, WorkerID: w.ID, RunID: child.Run.ID, Visit: visit, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
	child.WorkerSettlement.BusinessSourceHash = workerBusinessSourceHash(child)
}
func TestWorkCoverageRejectsForeignContextAndVisit(t *testing.T) {
	task := PlanTask{ID: "task", Status: "completed", ChildRunID: "child", WorkItems: []WorkItem{finiteWork("A")}}
	child := Snapshot{Run: Run{ID: "child"}, artifactRoot: t.TempDir()}
	resultValidationSource(t, &child, "proof", "exec", "exec", `{"object":"A","ok":true}`)
	parent := Snapshot{Run: Run{ID: "parent"}, Plan: PlanState{Tasks: []PlanTask{task}}}
	bindWorkSource(&parent, 0, &child)
	refresh := func() {
		refreshWorkProgress(&parent, func(id string) *Snapshot {
			if id == "child" {
				return &child
			}
			return nil
		})
	}
	refresh()
	if len(workProgressGaps(&parent)) != 0 {
		t.Fatal("valid settled binding rejected")
	}
	child.WorkerContext.WorkerID = "foreign"
	refresh()
	if len(workProgressGaps(&parent)) == 0 {
		t.Fatal("foreign context retained coverage")
	}
	child.WorkerContext.WorkerID = parent.Workers[0].ID
	parent.Workers[0].Visit++
	refresh()
	if len(workProgressGaps(&parent)) == 0 {
		t.Fatal("old visit retained coverage")
	}
}
func TestRemainingDeclarationsSurviveUnfinishedAndFailedAudits(t *testing.T) {
	for _, status := range []Status{StatusRunning, StatusPaused, StatusFailed, StatusCompleted} {
		t.Run(string(status), func(t *testing.T) {
			s := Snapshot{Run: Run{ID: "child", ParentTaskID: "source", Status: status}, Plan: PlanState{Tasks: []PlanTask{{ID: "unfinished", Goal: "still running", Status: "pending"}}}}
			body, _ := json.Marshal(map[string]Result{"trace_result": {Status: "incomplete", Remaining: []RemainingWork{{ID: "B", Goal: "research B", NextAction: "inspect original state", EvidenceIDs: []string{"unavailable"}}}}})
			s.Messages = []Message{{Role: "assistant", Content: string(body)}, {Role: "assistant", Content: `{"trace_result":{"status":"incomplete"}}`}}
			refreshResult(&s)
			if len(s.Result.Remaining) != 1 || s.Result.Status == "success" {
				t.Fatal("completion gate erased remaining declaration")
			}
			source := PlanTask{ID: "source"}
			if !mergeRemainingWork(&source, &s) || len(source.Remaining) != 1 {
				t.Fatal("unverified reference erased obligation")
			}
		})
	}
}
func TestDynamicWorkDoesNotAcceptOriginReplacement(t *testing.T) {
	old := PlanTask{ID: "old", Goal: "stage A", Status: "superseded", ReplacementTaskID: "new", Remaining: []RemainingWork{{ID: "B", Goal: "goal B", NextAction: "inspect B"}}}
	next := PlanTask{ID: "new", Goal: "stage A", Status: "completed", ChildRunID: "child"}
	parent := Snapshot{Run: Run{ID: "parent"}, Plan: PlanState{Tasks: []PlanTask{old, next}}}
	child := Snapshot{Run: Run{ID: "child"}}
	bindWorkSource(&parent, 1, &child)
	refreshWorkProgress(&parent, func(id string) *Snapshot {
		if id == "child" {
			return &child
		}
		return nil
	})
	if len(workProgressGaps(&parent)) != 1 {
		t.Fatal("original stage replacement erased independent B")
	}
}

func TestContinuationUnknownActionKeepsAdmissionAndResourceFence(t *testing.T) {
	a, store := newTestAgent(t, taskContractProvider{})
	defer a.CloseMCP()
	id := createTestRun(t, a)
	source := PlanTask{ID: "first", Goal: "first target", Requirements: []VerificationRequirement{taskRequirement("first-proof")}, ResourceKeys: []string{"object-write"}}
	if _, err := a.SetPlan(id, []PlanTask{source}); err != nil {
		t.Fatal(err)
	}
	workers, err := a.DispatchFrontier(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitAllTestWorkers(a, context.Background(), id, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Plan.Tasks[0].Remaining = []RemainingWork{{ID: "B", Goal: "remaining B", NextAction: "inspect saved state"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	next := PlanTask{ID: "next", Goal: "remaining B", SuccessContains: "B verified", Dependencies: []string{"first"}, ResourceKeys: source.ResourceKeys, Continues: []WorkReference{{TaskID: "first", ItemID: "B"}}}
	other := PlanTask{ID: "other", Goal: "independent same object operation", ResourceKeys: source.ResourceKeys}
	if _, err := a.SetPlan(id, []PlanTask{source, next, other}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(workers[0].RunID, func(child *Snapshot) error {
		resultValidationSource(t, child, "unknown", "remote", "mcp", "request sent, response unknown")
		child.Actions[len(child.Actions)-1].Status = "uncertain"
		child.Actions[len(child.Actions)-1].Result.OutcomeUncertain = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "next", "manual", 30); err == nil {
		t.Fatal("new task bypassed original unknown Action")
	}
	if _, err := a.ClaimTask(id, "other", "manual", 30); err == nil {
		t.Fatal("completed task released unknown object occupancy")
	}
	parent, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.Workers) != 1 {
		t.Fatal("blocked continuation spawned a Worker")
	}
}
func TestRetiredStageContinuationKeepsDependencySemantics(t *testing.T) {
	source := PlanTask{ID: "source", Goal: "first method", Stage: true, Status: "blocked", DispositionReason: "method not applicable", ChildRunID: "child", Remaining: []RemainingWork{{ID: "B", Goal: "research B", NextAction: "use saved state"}}}
	next := PlanTask{ID: "next", Goal: "research B", SuccessContains: "verified B", Continues: []WorkReference{{TaskID: "source", ItemID: "B"}}}
	if err := validateWorkLinks([]PlanTask{source, next}); err != nil {
		t.Fatal(err)
	}
	parent := Snapshot{Run: Run{ID: "parent"}, RootContract: &RootContract{Version: 2}, Plan: PlanState{Tasks: []PlanTask{source, next}}}
	child := Snapshot{Run: Run{ID: "child"}}
	bindWorkSource(&parent, 0, &child)
	if !continuationSourceReady(&parent, parent.Plan.Tasks[0], &child) {
		t.Fatal("settled source with no unknown effects blocked independent obligation")
	}
	if taskDone(parent.Plan.Tasks[0]) {
		t.Fatal("retired ID became a successful dependency")
	}
	child.Actions = []Action{{Status: "uncertain"}}
	if continuationSourceReady(&parent, parent.Plan.Tasks[0], &child) {
		t.Fatal("retired source waived original unknown effect")
	}
}

func TestWorkCoverageUsesOneSourceScanForFiniteBatch(t *testing.T) {
	task := PlanTask{ID: "stage", Status: "completed", ChildRunID: "child", WorkItems: []WorkItem{finiteWork("A"), finiteWork("B")}}
	child := Snapshot{Run: Run{ID: "child"}, artifactRoot: t.TempDir()}
	for _, id := range []string{"A", "B"} {
		resultValidationSource(t, &child, id, "exec", "exec", `{"object":"`+id+`","ok":true}`)
	}
	parent := Snapshot{Run: Run{ID: "parent"}, Plan: PlanState{Tasks: []PlanTask{task}}}
	bindWorkSource(&parent, 0, &child)
	reads := 0
	refreshWorkProgress(&parent, func(id string) *Snapshot {
		reads++
		if id == "child" {
			return &child
		}
		return nil
	})
	if gaps := workProgressGaps(&parent); len(gaps) > 0 || reads != 1 {
		t.Fatalf("batch projection reads=%d gaps=%v", reads, gaps)
	}
}

func TestRemainingDeclarationsPreserveValidSiblingsAndRepair(t *testing.T) {
	for _, bad := range []struct{ name, entry string }{
		{"missing-next-action", `{"id":"C","goal":"goal C"}`},
		{"wrong-field-type", `{"id":"C","goal":"goal C","next_action":17}`},
		{"duplicate", `{"id":"B","goal":"goal B","next_action":"inspect B"}`},
	} {
		t.Run(bad.name, func(t *testing.T) {
			s := Snapshot{Run: Run{ID: "root"}, artifactRoot: t.TempDir()}
			resultValidationSource(t, &s, "proof", "exec", "exec", "observed")
			initial := `{"trace_result":{"status":"success","evidence_ids":["proof"],"remaining":[{"id":"B","goal":"goal B","next_action":"inspect B"},` + bad.entry + `]}}`
			s.Messages = append(s.Messages, Message{Role: "assistant", Content: initial})
			resultValidationClaim(t, &s, Result{Status: "success", EvidenceIDs: []string{"proof"}})
			if s.Result.Status == "success" || len(s.Result.Remaining) == 0 || s.Result.Remaining[0].ID != "B" {
				t.Fatalf("mixed declaration disappeared: %+v", s.Result)
			}
			resultValidationIssue(t, &s, "invalid_remaining_work", "trace_result.remaining", "", -1, true)
			parent := Snapshot{Run: Run{ID: "parent"}, Plan: PlanState{Tasks: []PlanTask{{ID: "stage", Status: "completed", ChildRunID: s.Run.ID}}}}
			bindWorkSource(&parent, 0, &s)
			if !mergeRemainingWork(&parent.Plan.Tasks[0], &s) {
				t.Fatal("valid sibling was not retained by the parent")
			}
			refreshWorkProgress(&parent, func(string) *Snapshot { return &s })
			if len(workProgressGaps(&parent)) < 2 {
				t.Fatalf("parent lost obligation or raw structure diagnostic: %+v", parent.Plan.WorkProgress)
			}
			repair := Result{Status: "success", EvidenceIDs: []string{"proof"}, Remaining: []RemainingWork{{ID: "B", Goal: "goal B", NextAction: "inspect B"}}}
			if bad.name != "duplicate" {
				repair.Remaining = append(repair.Remaining, RemainingWork{ID: "C", Goal: "goal C", NextAction: "inspect C"})
			}
			resultValidationClaim(t, &s, repair)
			for _, issue := range s.Result.ValidationIssues {
				if issue.Code == "invalid_remaining_work" {
					t.Fatalf("matching repair retained shape error: %+v", issue)
				}
			}
			if s.Result.Status != "success" {
				t.Fatalf("child local success rejected after structure repair: %+v", s.Result)
			}
			s.Messages = append(s.Messages, Message{Role: "assistant", Content: `{"trace_result":{"remaining":[{"id":"B","goal":"changed B","next_action":"inspect B"}]}}`})
			resultValidationClaim(t, &s, Result{Status: "success", EvidenceIDs: []string{"proof"}})
			if s.Result.Status == "success" || s.Result.Remaining[0].Goal != "goal B" {
				t.Fatalf("changed goal laundered declaration: %+v", s.Result)
			}
		})
	}
}

func TestRemainingDeclarationAnonymousErrorsNeedFullRestatement(t *testing.T) {
	full := `{"trace_result":{"remaining":[{"id":"B","goal":"goal B","next_action":"inspect B"},{"id":"C","goal":"goal C","next_action":"inspect C"}]}}`
	for _, raw := range []string{`{"trace_result":{"remaining":"not-an-array"}}`, `{"trace_result":{"remaining":null}}`, `{"trace_result":{"remaining":[{"goal":"unidentified","next_action":"inspect"}]}}`} {
		s := Snapshot{Run: Run{ID: "root"}, Messages: []Message{{Role: "assistant", Content: full}, {Role: "assistant", Content: raw}}}
		for _, later := range []Message{
			{Role: "assistant", Content: `{"trace_result":{"status":"success"}}`},
			{Role: "assistant", Content: `{"trace_result":{"remaining":[]}}`},
			{Role: "assistant", Content: `{"trace_result":{"remaining":[{"id":"B","goal":"goal B","next_action":"inspect B"}]}}`},
			{Role: "assistant", ReportOnly: true, Content: full},
		} {
			s.Messages = append(s.Messages, later)
			if items, issues := collectRemainingDeclarations(&s); len(issues) != 1 || len(items) != 2 {
				t.Fatalf("partial or unrelated claim erased structure error or obligations: %+v %+v", items, issues)
			}
		}
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: full})
		if items, issues := collectRemainingDeclarations(&s); len(issues) != 0 || len(items) != 2 {
			t.Fatalf("explicit full restatement did not repair structure error: %+v %+v", items, issues)
		}
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: `{"trace_result":{"status":"success"}}`})
		if items, issues := collectRemainingDeclarations(&s); len(issues) != 0 || len(items) != 2 {
			t.Fatal("later claim erased repaired sticky obligations")
		}
	}
}

func TestRootRemainingNeedsExplicitContinuation(t *testing.T) {
	root := Snapshot{Run: Run{ID: "root"}, artifactRoot: t.TempDir()}
	resultValidationSource(t, &root, "proof", "exec", "exec", "observed")
	resultValidationClaim(t, &root, Result{Status: "success", EvidenceIDs: []string{"proof"}, Remaining: []RemainingWork{{ID: "B", Goal: "goal B", NextAction: "inspect B"}}})
	if root.Result.Status == "success" || len(workProgressGaps(&root)) != 1 {
		t.Fatalf("unplanned root declaration completed: %+v", root.Result)
	}
	text, _ := plannerContextFrom(&root)
	if text == "" {
		t.Fatal("root without tasks hid remaining obligations from planner")
	}
	ref := WorkReference{TaskID: rootWorkTaskID, ItemID: "B"}
	source, err := continuationSourceTask(&root, ref)
	if err != nil || source.ID != rootWorkTaskID || source.EvidenceRunID != root.Run.ID || len(root.Plan.Tasks) != 0 {
		t.Fatalf("root source was not ephemeral: %+v %v", source, err)
	}
	next := PlanTask{ID: "next", Goal: "goal B", SuccessContains: "done", Continues: []WorkReference{ref}}
	if err := validateRootWorkLinks(&root, []PlanTask{next}); err != nil {
		t.Fatal(err)
	}
	changed := next
	changed.Goal = "another goal"
	if err := validateRootWorkLinks(&root, []PlanTask{changed}); err == nil {
		t.Fatal("root continuation changed goal")
	}
	changed = next
	changed.SuccessContains = ""
	if err := validateRootWorkLinks(&root, []PlanTask{changed}); err == nil {
		t.Fatal("root continuation dropped acceptance")
	}
	if _, err := continuationSourceTask(&root, WorkReference{TaskID: rootWorkTaskID, ItemID: "unknown"}); err == nil {
		t.Fatal("unknown root work accepted")
	}
	root.Plan.Tasks = []PlanTask{{ID: "independent", Status: "completed"}}
	root.Actions = append(root.Actions, Action{PlanTaskID: "independent", Status: "uncertain"})
	if continuationSourceReady(&root, source, &root) {
		t.Fatal("root source bypassed an independent local task's uncertain Action")
	}
	root.Actions = root.Actions[:len(root.Actions)-1]
	root.Plan = PlanState{}
	child := root
	child.Run.ParentRunID = "parent"
	child.Run.ParentTaskID = "task"
	child.Plan = PlanState{}
	refreshResult(&child)
	if child.Result.Status != "success" || len(workProgressGaps(&child)) != 0 {
		t.Fatalf("root gate affected child stage success: %+v", child.Result)
	}
	if _, err := continuationSourceTask(&child, ref); err == nil {
		t.Fatal("child forged root reference")
	}
	root.Run.Goal = "root goal"
	contract := pinRootContract(&root, RootContract{Version: 2, Goal: root.Run.Goal})
	root.RootContract = &contract
	next.Stage, next.Status, next.DispositionReason = true, "blocked", "method not applicable"
	root.Plan.Tasks = []PlanTask{next}
	refreshWorkProgress(&root, func(string) *Snapshot { return &root })
	refreshResult(&root)
	if root.Result.Status != "success" || len(root.Result.Remaining) != 0 {
		t.Fatalf("explicit root continuation disposition did not converge: %+v", root.Result)
	}
}

func TestRootRemainingContinuationEndToEnd(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, taskContractProvider{})
			defer a.CloseMCP()
			id := createTestRun(t, a)
			if _, err := store.Update(id, func(s *Snapshot) error {
				resultValidationSource(t, s, "root-proof", "exec", "exec", "root observed")
				resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{"root-proof"}, Remaining: []RemainingWork{{ID: "B", Goal: "verify remaining B", NextAction: "inspect B"}}})
				s.Run.Status = StatusCreated
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			ref := WorkReference{TaskID: rootWorkTaskID, ItemID: "B"}
			next := PlanTask{ID: "next", Goal: "verify remaining B", Requirements: []VerificationRequirement{taskRequirement("second-proof")}, Continues: []WorkReference{ref}}
			if _, err := a.SetPlan(id, []PlanTask{next}); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			a.store = reopened
			restored, err := reopened.Load(id)
			if err != nil || len(workProgressGaps(&restored)) != 1 {
				t.Fatalf("restart lost root declaration: %+v %v", restored.Plan, err)
			}
			linked, err := continuationSourceTask(&restored, ref)
			if err != nil || linked.ID != rootWorkTaskID {
				t.Fatalf("restart lost root source: %+v %v", linked, err)
			}
			launched, err := a.DispatchFrontier(context.Background(), id)
			if err != nil || len(launched) != 1 {
				t.Fatalf("root continuation dispatch: %+v %v", launched, err)
			}
			done, err := waitAllTestWorkers(a, context.Background(), id, 15)
			if err != nil {
				t.Fatal(err)
			}
			if gaps := workProgressGaps(&done); len(gaps) != 0 {
				t.Fatalf("root remaining never converged: %v %+v", gaps, done.Plan)
			}
			if _, err := reopened.Update(id, func(s *Snapshot) error {
				resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{"root-proof"}})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			completed, err := reopened.Load(id)
			if err != nil || completed.Result.Status != "success" || len(completed.Result.Remaining) != 0 {
				t.Fatalf("completed root retained remaining: %+v %v", completed.Result, err)
			}
			if len(remainingDeclarations(&completed)) != 1 {
				t.Fatal("resolved root history was deleted")
			}
			duplicate := next
			duplicate.ID = "duplicate"
			if _, err := a.UpdatePlan(id, []PlanTask{duplicate}, "append"); err == nil {
				t.Fatal("resolved root accepted another continuation")
			}
			if _, err := reopened.Update(launched[0].RunID, func(s *Snapshot) error {
				s.Actions[0].Status = "uncertain"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			revoked, err := reopened.Load(id)
			if err != nil || revoked.Result.Status == "success" || len(revoked.Result.Remaining) != 1 || len(workProgressGaps(&revoked)) == 0 {
				t.Fatalf("revoked successor retained root success: %+v %v", revoked.Result, err)
			}
		})
	}
}

func TestDynamicWorkCachesBoundSourcesBeforeLoading(t *testing.T) {
	var items []RemainingWork
	for _, id := range []string{"one", "two", "three"} {
		items = append(items, RemainingWork{ID: id, Goal: "same goal", NextAction: "inspect"})
	}
	original := PlanTask{ID: "stage", Status: "completed", Remaining: items}
	first := PlanTask{ID: "first", Goal: "same goal", Status: "completed", ChildRunID: "a"}
	second := PlanTask{ID: "second", Goal: "same goal", Status: "completed", ChildRunID: "b"}
	for _, item := range items {
		first.Continues = append(first.Continues, WorkReference{TaskID: original.ID, ItemID: item.ID})
		second.Continues = append(second.Continues, WorkReference{TaskID: original.ID, ItemID: item.ID})
	}
	parent := Snapshot{Run: Run{ID: "root"}, Plan: PlanState{Tasks: []PlanTask{original, first, second}}}
	a := Snapshot{Run: Run{ID: "a"}}
	b := Snapshot{Run: Run{ID: "b"}}
	bindWorkSource(&parent, 1, &a)
	bindWorkSource(&parent, 2, &b)
	a.WorkerContext.WorkerID = "foreign"
	b.WorkerContext.WorkerID = "foreign"
	reads := map[string]int{}
	refresh := func() {
		refreshWorkProgress(&parent, func(id string) *Snapshot {
			if id == "" || id == parent.Run.ID {
				return &parent
			}
			reads[id]++
			if id == "a" {
				return &a
			}
			if id == "b" {
				return &b
			}
			return nil
		})
	}
	refresh()
	if len(workProgressGaps(&parent)) != 3 || reads["a"] != 1 || reads["b"] != 1 {
		t.Fatalf("interleaved invalid sources were reloaded or accepted: reads=%v progress=%+v", reads, parent.Plan.WorkProgress)
	}
	b.WorkerContext.WorkerID = parent.Workers[1].ID
	reads = map[string]int{}
	refresh()
	if gaps := workProgressGaps(&parent); len(gaps) != 0 || reads["a"] != 1 || reads["b"] != 1 {
		t.Fatalf("cached binding failed valid B after invalid A: reads=%v gaps=%v", reads, gaps)
	}
}
