package agent

import "time"

// Read-only diagnostics: neither silence nor a timeout value settles a call.
type WorkerCallActivity struct {
	CallID           string          `json:"call_id"`
	RequestID        string          `json:"request_id"`
	Name             string          `json:"name"`
	Status           string          `json:"status"`
	StartedAt        time.Time       `json:"started_at"`
	FinishedAt       time.Time       `json:"finished_at,omitempty"`
	TimeoutSeconds   int             `json:"timeout_seconds,omitempty"`
	WorkerStatus     string          `json:"worker_status,omitempty"`
	OutcomeUncertain bool            `json:"outcome_uncertain,omitempty"`
	CleanupError     string          `json:"cleanup_error,omitempty"`
	Diagnostic       *ToolDiagnostic `json:"diagnostic,omitempty"`
}

func projectToolActivity(actions []Action) ([]WorkerCallActivity, int) {
	rows := []WorkerCallActivity{}
	total := 0
	terminalKept := false
	for i := len(actions) - 1; i >= 0; i-- {
		action := actions[i]
		pending := action.FinishedAt.IsZero() || action.Status == "uncertain" || action.Status == "result_ready"
		if !pending && terminalKept {
			continue
		}
		if !pending {
			terminalKept = true
		}
		total++
		if len(rows) == 20 {
			continue
		}
		row := WorkerCallActivity{CallID: action.CallID, RequestID: action.RequestID, Name: action.Name, Status: action.Status, StartedAt: action.StartedAt, FinishedAt: action.FinishedAt, TimeoutSeconds: action.TimeoutSeconds}
		if action.Result != nil {
			row.WorkerStatus = string(action.Result.WorkerStatus)
			row.OutcomeUncertain = action.Result.OutcomeUncertain || action.Result.WorkerOutcomeUncertain
			row.Diagnostic = action.Result.Diagnostic
		}
		rows = append(rows, row)
		if action.Result != nil {
			rows[len(rows)-1].CleanupError = action.Result.WorkerCleanupError
		}
	}
	return rows, total
}
