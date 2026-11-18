package agent

import (
	"fmt"
	"os"
	"time"
)

type RequestDiagnostic struct {
	RequestID           string            `json:"request_id"`
	RequestedModel      string            `json:"requested_model"`
	ProviderID          string            `json:"provider_id"`
	ReasoningEffort     string            `json:"reasoning_effort"`
	Purpose             string            `json:"purpose"`
	RequestedAt         time.Time         `json:"requested_at"`
	RespondedAt         time.Time         `json:"responded_at"`
	ReportedModel       string            `json:"reported_model"`
	ProviderResponseID  string            `json:"provider_response_id"`
	ReportedServiceTier string            `json:"reported_service_tier"`
	IdentityStatus      string            `json:"identity_status"`
	ResponseStatus      string            `json:"response_status"`
	Timing              *ModelDiagnostics `json:"timing"`
	Cost                CostEstimate      `json:"cost"`
	ReportedCost        *ProviderCost     `json:"reported_cost"`
	ToolActions         int               `json:"tool_actions"`
	// These existing wall-clock records are exposed as such; no cross-restart
	// elapsed time is fabricated from their timestamps.
	ToolStartedAt  time.Time `json:"tool_started_at"`
	ToolFinishedAt time.Time `json:"tool_finished_at"`
	UncertainTools int       `json:"uncertain_tools"`
}

type RunDiagnostics struct {
	RunID         string              `json:"run_id"`
	TotalRequests int                 `json:"total_requests"`
	Requests      []RequestDiagnostic `json:"requests"`
	Truncated     bool                `json:"truncated"`
	IdentityBasis string              `json:"identity_basis"`
	TimingBasis   string              `json:"timing_basis"`
}

// Diagnostics reuses the body-free usage projection and never revalidates,
// repairs, resumes or writes a Run. Response size is bounded; disk reads still
// parse the existing snapshot rather than introducing a second ledger/index.
func (s *Store) Diagnostics(id string, currency string, limit int) (RunDiagnostics, error) {
	if limit < 1 || limit > 100 {
		return RunDiagnostics{}, fmt.Errorf("diagnostic limit must be 1..100")
	}
	snapshot, err := s.readUsageSnapshot(id)
	if err != nil {
		return RunDiagnostics{}, err
	}
	if snapshot.Run.Deleted {
		return RunDiagnostics{}, os.ErrNotExist
	}
	result := RunDiagnostics{RunID: id, TotalRequests: len(snapshot.Requests), Requests: []RequestDiagnostic{}, IdentityBasis: "unverified_provider_report; missing remains unknown", TimingBasis: "process_monotonic_ms; prepare includes archive transaction and context projection; response includes sink backpressure but excludes final drain; first byte means first body byte; missing remains unknown"}
	seen := map[string]bool{}
	for _, r := range snapshot.Requests {
		if r.ID == "" || seen[r.ID] {
			return RunDiagnostics{}, fmt.Errorf("ambiguous request identity")
		}
		seen[r.ID] = true
	}
	responses := map[string]usageResponse{}
	for _, r := range snapshot.Responses {
		if _, exists := responses[r.RequestID]; exists || !seen[r.RequestID] {
			return RunDiagnostics{}, fmt.Errorf("ambiguous response identity")
		}
		responses[r.RequestID] = r
	}
	start := len(snapshot.Requests) - limit
	if start < 0 {
		start = 0
	}
	result.Truncated = start > 0
	indexes := map[string]int{}
	for _, request := range snapshot.Requests[start:] {
		row := RequestDiagnostic{RequestID: request.ID, RequestedModel: request.ProviderModel, ProviderID: request.ProviderID, ReasoningEffort: request.ReasoningEffort, Purpose: request.Purpose, RequestedAt: request.CreatedAt, IdentityStatus: "unknown", ResponseStatus: "pending", Cost: estimateRequestCost(request, nil, currency)}
		if record, ok := responses[request.ID]; ok {
			response := record.Response
			row.RespondedAt, row.ResponseStatus = record.CreatedAt, "received"
			if record.Error != "" {
				row.ResponseStatus = "error"
			}
			row.ReportedModel, row.ProviderResponseID, row.ReportedServiceTier = response.ReportedModel, response.ProviderResponseID, response.ReportedServiceTier
			if response.ReportedModel != "" {
				row.IdentityStatus = "reported"
			}
			if response.IdentityConflict {
				row.IdentityStatus = "conflict"
			}
			row.Timing, row.ReportedCost = response.Diagnostics, response.ReportedCost
			row.Cost = estimateRequestCost(request, response.Usage, currency)
		}
		indexes[request.ID] = len(result.Requests)
		result.Requests = append(result.Requests, row)
	}
	for _, action := range snapshot.Actions {
		index, ok := indexes[action.RequestID]
		if !ok {
			continue
		}
		row := &result.Requests[index]
		row.ToolActions++
		if action.Status == "uncertain" {
			row.UncertainTools++
		}
		if !action.StartedAt.IsZero() && (row.ToolStartedAt.IsZero() || action.StartedAt.Before(row.ToolStartedAt)) {
			row.ToolStartedAt = action.StartedAt
		}
		if action.FinishedAt.After(row.ToolFinishedAt) {
			row.ToolFinishedAt = action.FinishedAt
		}
	}
	return result, nil
}
