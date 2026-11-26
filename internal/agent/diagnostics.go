package agent

import (
	"fmt"
	"os"
	"sync"
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

// Store measurements are bounded, process-local observations, never Run state.
type StoreDiagnostics struct {
	Backend string                 `json:"backend"`
	Since   time.Time              `json:"since"`
	Stages  []StoreStageDiagnostic `json:"stages"`
	Basis   string                 `json:"basis"`
}

type StoreStageDiagnostic struct {
	Stage             string `json:"stage"`
	Samples           int64  `json:"samples"`
	DurationMicros    int64  `json:"duration_us_total"`
	MaxDurationMicros int64  `json:"duration_us_max"`
	BytesTotal        int64  `json:"bytes_total,omitempty"`
	MaxBytes          int64  `json:"bytes_max,omitempty"`
	ByteBasis         string `json:"byte_basis,omitempty"`
}

type storeDiagnosticStage uint8

const (
	storeMutexWait storeDiagnosticStage = iota
	storeFileLockWait
	storeLoadVerify
	storeSave
	storeJSONEncode
	storeJSONWriteSync
	storeJSONPublish
	storeJSONEncodeWriteSync
	storeSQLiteWriteCommit
	storeSQLiteCommit
	storeDiagnosticStageCount
)

var storeDiagnosticNames = [...]string{
	"mutex_wait", "file_lock_wait", "load_verify", "save",
	"json_encode", "json_write_fsync", "json_publish", "json_encode_write_fsync",
	"sqlite_write_commit", "sqlite_commit",
}

type storeDiagnosticCounters struct {
	mu     sync.Mutex
	since  time.Time
	stages [storeDiagnosticStageCount]StoreStageDiagnostic
}

func (d *storeDiagnosticCounters) record(stage storeDiagnosticStage, start time.Time, bytes int64) {
	duration := time.Since(start).Microseconds()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.since.IsZero() {
		d.since = start.UTC()
	}
	row := &d.stages[stage]
	row.Samples++
	row.DurationMicros += duration
	row.MaxDurationMicros = max(row.MaxDurationMicros, duration)
	row.BytesTotal += bytes
	row.MaxBytes = max(row.MaxBytes, bytes)
}

// StoreDiagnostics never acquires execution/Store locks, reads files or saves data.
func (s *Store) StoreDiagnostics() StoreDiagnostics {
	backend := "json"
	if s.sqlite {
		backend = "sqlite"
	}
	s.diagnostics.mu.Lock()
	defer s.diagnostics.mu.Unlock()
	out := StoreDiagnostics{Backend: backend, Since: s.diagnostics.since, Stages: []StoreStageDiagnostic{}, Basis: "process_local_monotonic_us; completed measurements include failed waits/saves; large JSON encode/write samples require successful staging; stages overlap and are not additive; mutex_wait covers create/update/append; file_lock_wait covers blocking Store fence acquisition, excludes finish/recovery and context-polling reads; load_verify may save revocations; save covers prepared semantic snapshots, excludes report archiving; json_publish includes display-index preparation; bytes cover JSON snapshot encoding/staging only, exclude derived indexes; SQLite snapshot bytes and physical disk/WAL writes are unmeasured; not p95"}
	for stage, row := range s.diagnostics.stages {
		if row.Samples == 0 {
			continue
		}
		row.Stage = storeDiagnosticNames[stage]
		switch storeDiagnosticStage(stage) {
		case storeJSONEncode:
			row.ByteBasis = "encoded_snapshot"
		case storeJSONWriteSync, storeJSONEncodeWriteSync:
			row.ByteBasis = "snapshot_file_written_including_discarded_staging"
		}
		out.Stages = append(out.Stages, row)
	}
	return out
}
