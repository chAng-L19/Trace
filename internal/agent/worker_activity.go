package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
	"trace/internal/worker"
)

// WorkerActivity is a read-only display projection. No token/lease heartbeat
// counts as a result, and silence never changes execution or cleanup state.
type WorkerActivity struct {
	Calls                  []WorkerCallActivity `json:"calls,omitempty"`
	CallTotal              int                  `json:"call_total,omitempty"`
	WorkerID               string               `json:"worker_id"`
	RunID                  string               `json:"run_id"`
	Available              bool                 `json:"available"`
	RunStatus              Status               `json:"run_status,omitempty"`
	Phase                  string               `json:"phase"`
	RequestID              string               `json:"request_id,omitempty"`
	Tools                  []string             `json:"tools,omitempty"`
	LastModelResponse      time.Time            `json:"last_model_response,omitempty"`
	LastToolResult         time.Time            `json:"last_tool_result,omitempty"`
	CleanupStatus          string               `json:"cleanup_status"`
	BudgetRemainingSeconds *int64               `json:"budget_remaining_seconds,omitempty"`
	PendingMessages        int                  `json:"pending_messages"`
	OpenHelpIDs            []string             `json:"open_help_ids,omitempty"`
	OpenHelpTotal          int                  `json:"open_help_total"`
}

func (s *Store) WorkerActivities(parent Snapshot) []WorkerActivity {
	return s.collectWorkerActivities(context.Background(), parent, nil)
}

// ActivityWorkerActivities retains full-inbox counts from the semantic view,
// although the visible message bodies are only a bounded history window.
func (s *Store) ActivityWorkerActivities(p ActivityProjection) []WorkerActivity {
	return s.ActivityWorkerActivitiesContext(context.Background(), p)
}

func (s *Store) ActivityWorkerActivitiesContext(ctx context.Context, p ActivityProjection) []WorkerActivity {
	return s.collectWorkerActivities(ctx, p.Snapshot, p.WorkerInbox)
}

func (s *Store) collectWorkerActivities(ctx context.Context, parent Snapshot, inbox map[string]activityWorkerInbox) []WorkerActivity {
	out := make([]WorkerActivity, 0, len(parent.Workers))
	var helps []WorkerHelp
	if inbox == nil {
		helps = WorkerHelps(parent.WorkerMessages)
	}
	for _, worker := range parent.Workers {
		if ctx.Err() != nil {
			return out
		}
		row := WorkerActivity{Phase: "unavailable"}
		if cached, err := s.workerActivityContext(ctx, worker.RunID); err == nil && !cached.deleted && cached.parentID == parent.Run.ID && cached.taskID == worker.TaskID {
			row = cached.row
			row.Tools = append([]string(nil), cached.row.Tools...)
			if cached.maxSeconds > 0 {
				run := Run{Budget: Budget{MaxSeconds: cached.maxSeconds}, ElapsedMillis: cached.elapsedMillis, BudgetActiveSince: cached.activeSince}
				remaining := max(int64(0), remainingBudget(&Snapshot{Run: run}).Milliseconds()/1000)
				row.BudgetRemainingSeconds = &remaining
			}
		}
		// Parent inbox and cleanup can change without a child-file generation.
		row.WorkerID, row.RunID, row.CleanupStatus = worker.ID, worker.RunID, worker.CleanupStatus
		for _, m := range parent.WorkerMessages {
			if m.To == worker.ID && !m.Delivered {
				row.PendingMessages++
			}
		}
		for _, h := range helps {
			if h.ResolutionID == "" && (h.Message.From == worker.ID || h.Message.To == worker.ID) {
				row.OpenHelpIDs = append(row.OpenHelpIDs, h.Message.ID)
			}
		}
		row.OpenHelpTotal = len(row.OpenHelpIDs)
		if inbox != nil {
			counts := inbox[worker.ID]
			row.PendingMessages = counts.PendingMessages
			row.OpenHelpTotal = counts.OpenHelpTotal
			row.OpenHelpIDs = append([]string(nil), counts.OpenHelpIDs...)
		}
		out = append(out, row)
	}
	return out
}

// Bounded display-only cache. Retain neither canonical snapshots nor event
// arrays; file replacement invalidates each entry, including external writers.
const workerActivityCacheLimit = 128

type workerActivityCache struct {
	info                      os.FileInfo
	parentID, taskID          string
	deleted                   bool
	maxSeconds, elapsedMillis int64
	activeSince               time.Time
	row                       WorkerActivity
}

type workerActionHeader struct {
	CallID                string    `json:"call_id"`
	RequestID             string    `json:"request_id"`
	Name                  string    `json:"name"`
	Status                string    `json:"status"`
	StartedAt             time.Time `json:"started_at"`
	FinishedAt            time.Time `json:"finished_at"`
	TimeoutSeconds        int       `json:"timeout_seconds"`
	Adapter               string    `json:"adapter,omitempty"`
	ResourceCleanupStatus string    `json:"resource_cleanup_status,omitempty"`
	ResourceCleanupError  string    `json:"resource_cleanup_error,omitempty"`
	Result                *struct {
		WorkerStatus           string          `json:"worker_status"`
		WorkerOutcomeUncertain bool            `json:"worker_outcome_uncertain"`
		OutcomeUncertain       bool            `json:"outcome_uncertain"`
		WorkerCleanupError     string          `json:"worker_cleanup_error"`
		Diagnostic             *ToolDiagnostic `json:"diagnostic,omitempty"`
		Artifacts              []struct {
			Kind string `json:"kind"`
		} `json:"artifacts,omitempty"`
	} `json:"result"`
}

func (s *Store) workerActivity(id string) (*workerActivityCache, error) {
	return s.workerActivityContext(context.Background(), id)
}

func (s *Store) workerActivityContext(ctx context.Context, id string) (*workerActivityCache, error) {
	// Share the display admission gate with parent reads; expanding a Worker
	// must not allocate a second large SQL state beside a cold parent scan.
	if err := s.lockDisplayRead(ctx); err != nil {
		return nil, err
	}
	defer s.activityReadMu.Unlock()
	if s.sqlite {
		tx, run, _, err := s.indexedSQLiteTransaction(ctx, id)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		index, _, err := readSQLiteDisplayIndex(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		v := index.view("worker")
		v.fields["run"] = run
		child, row, err := projectWorkerActivityFields(v.fields)
		if err != nil {
			return nil, err
		}
		if child.ID != id {
			return nil, os.ErrNotExist
		}
		return &workerActivityCache{parentID: child.ParentRunID, taskID: child.ParentTaskID, deleted: child.Deleted, maxSeconds: child.Budget.MaxSeconds, elapsedMillis: child.ElapsedMillis, activeSince: child.BudgetActiveSince, row: row}, nil
	}
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil, os.ErrNotExist
	}
	// Separate from the Store's publication lock. Concurrent board requests
	// share one cold decode, without delaying commits behind large histories.
	s.workerActivityMu.Lock()
	defer s.workerActivityMu.Unlock()
	info, err := os.Stat(filepath.Join(s.root, id+".json"))
	if err != nil {
		delete(s.workerActivities, id)
		return nil, err
	}
	if cached := s.workerActivities[id]; cached != nil && sameGeneration(cached.info, info) {
		return cached, nil
	}
	f, err := os.Open(filepath.Join(s.root, id+".json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	index, _, err := s.jsonDisplayIndex(ctx, id, f, false)
	if err != nil {
		return nil, err
	}
	v := index.view("worker")
	child, row, err := projectWorkerActivityFields(v.fields)
	if err != nil {
		return nil, err
	}
	if child.ID != id {
		return nil, os.ErrNotExist
	}
	cached := &workerActivityCache{info: info, parentID: child.ParentRunID, taskID: child.ParentTaskID, deleted: child.Deleted, maxSeconds: child.Budget.MaxSeconds, elapsedMillis: child.ElapsedMillis, activeSince: child.BudgetActiveSince, row: row}
	if s.workerActivities == nil {
		s.workerActivities = map[string]*workerActivityCache{}
	}
	if len(s.workerActivities) >= workerActivityCacheLimit && s.workerActivities[id] == nil {
		for key := range s.workerActivities {
			delete(s.workerActivities, key)
			break
		}
	}
	s.workerActivities[id] = cached
	return cached, nil
}

// Decode only source identities and lifecycle headers. In particular, old
// model context, response text and tool streams never materialize here.
func projectWorkerActivityFields(fields map[string]json.RawMessage) (Run, WorkerActivity, error) {
	var child Snapshot
	if err := json.Unmarshal(fields["run"], &child.Run); err != nil {
		return Run{}, WorkerActivity{}, err
	}
	for _, name := range []string{"requests", "responses", "actions"} {
		rows, err := activityArray(fields, name)
		if err != nil {
			return Run{}, WorkerActivity{}, err
		}
		for _, raw := range rows {
			switch name {
			case "requests":
				var h struct {
					ID string `json:"id"`
				}
				if err = json.Unmarshal(raw, &h); err != nil {
					return Run{}, WorkerActivity{}, err
				}
				child.Requests = append(child.Requests, ModelRequest{ID: h.ID})
			case "responses":
				var h struct {
					RequestID string    `json:"request_id"`
					CreatedAt time.Time `json:"created_at"`
				}
				if err = json.Unmarshal(raw, &h); err != nil {
					return Run{}, WorkerActivity{}, err
				}
				child.Responses = append(child.Responses, ResponseRecord{RequestID: h.RequestID, CreatedAt: h.CreatedAt})
			case "actions":
				var h workerActionHeader
				if err = json.Unmarshal(raw, &h); err != nil {
					return Run{}, WorkerActivity{}, err
				}
				action := Action{CallID: h.CallID, RequestID: h.RequestID, Name: h.Name, Status: h.Status, StartedAt: h.StartedAt, FinishedAt: h.FinishedAt, TimeoutSeconds: h.TimeoutSeconds}
				if h.Result != nil {
					action.Result = &ToolResult{WorkerStatus: worker.Status(h.Result.WorkerStatus), WorkerOutcomeUncertain: h.Result.WorkerOutcomeUncertain, OutcomeUncertain: h.Result.OutcomeUncertain, WorkerCleanupError: h.Result.WorkerCleanupError, Diagnostic: h.Result.Diagnostic}
				}
				child.Actions = append(child.Actions, action)
			}
		}
	}
	return child.Run, projectWorkerActivity(child), nil
}

func projectWorkerActivity(child Snapshot) WorkerActivity {
	run := child.Run
	row := WorkerActivity{Available: true, RunStatus: run.Status, Phase: string(run.Status)}
	row.Calls, row.CallTotal = projectToolActivity(child.Actions)
	settled := map[string]bool{}
	for _, response := range child.Responses {
		settled[response.RequestID] = true
		if response.CreatedAt.After(row.LastModelResponse) {
			row.LastModelResponse = response.CreatedAt
		}
	}
	active := run.Status == StatusRunning || run.Status == StatusWaitingTool
	if active && len(child.Requests) > 0 && !settled[child.Requests[len(child.Requests)-1].ID] {
		row.Phase, row.RequestID = "model", child.Requests[len(child.Requests)-1].ID
	}
	for _, action := range child.Actions {
		if action.FinishedAt.After(row.LastToolResult) {
			row.LastToolResult = action.FinishedAt
		}
		if active && (action.Status == "started" || action.Status == "running" || action.Status == "result_ready" || action.Status == "waiting_tool") {
			row.Tools = append(row.Tools, action.Name)
		}
	}
	if len(row.Tools) > 0 {
		row.Phase = "tool"
	}
	return row
}

func (a *Agent) compactWorkerActivity(s Snapshot) map[string]any {
	state := compactWorkerState(s)
	s.Workers = currentWorkerView(s.Workers)
	activity := a.store.WorkerActivities(s)
	for i := range activity {
		activity[i].OpenHelpIDs = activity[i].OpenHelpIDs[:min(len(activity[i].OpenHelpIDs), workerViewLimit)]
	}
	state["worker_activity"] = activity
	return state
}
