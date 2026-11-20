package agent

import (
	"encoding/json"
	"os"
	"reflect"
	"sync"
)

// This cache is a display projection of immutable source bytes, never evidence.
// Telemetry generations share only the stable non-event base. A semantic commit
// or external file replacement constructs a new base and rechecks provenance.
type workspaceBase struct {
	once sync.Once
	data []byte
	err  error
}

type workspaceProjection struct {
	events  []Event
	through int
	current string
	owners  map[string]string
}

func (v *storedView) workspaceSnapshot() (Snapshot, error) {
	b := v.workspaceBase
	b.once.Do(func() {
		var snap Snapshot
		var view struct {
			*Snapshot
			Prompt   snapshotJSON `json:"prompt"`
			Events   snapshotJSON `json:"events"`
			Requests []struct {
				ModelRequest
				Messages snapshotJSON `json:"messages"`
				Tools    snapshotJSON `json:"tools"`
			} `json:"requests"`
		}
		view.Snapshot = &snap
		// Field boundaries were validated by encoding/json when this immutable
		// generation was opened. Omit the event array before parsing the display
		// base so a history page never scans the 50 MB event ledger again.
		data := v.snapshot
		if v.eventSpan.end > v.eventSpan.start {
			data = make([]byte, 0, len(v.snapshot)-(v.eventSpan.end-v.eventSpan.start)+4)
			data = append(data, v.snapshot[:v.eventSpan.start]...)
			data = append(data, "null"...)
			data = append(data, v.snapshot[v.eventSpan.end:]...)
		}
		if b.err = json.Unmarshal(data, &view); b.err != nil {
			return
		}
		legacy := false
		for _, message := range snap.Messages {
			legacy = legacy || message.Role == "assistant" && message.RequestID == ""
		}
		snap.Requests = make([]ModelRequest, len(view.Requests))
		for i, request := range view.Requests {
			snap.Requests[i] = request.ModelRequest
			if !legacy || len(request.Messages) == 0 {
				continue
			}
			var ids []struct {
				ID string `json:"id"`
			}
			if b.err = json.Unmarshal(request.Messages, &ids); b.err != nil {
				return
			}
			for _, message := range ids {
				snap.Requests[i].Messages = append(snap.Requests[i].Messages, Message{ID: message.ID})
			}
		}
		b.data, b.err = json.Marshal(snap)
	})
	if b.err != nil {
		return Snapshot{}, b.err
	}
	var snap Snapshot
	var view struct {
		*Snapshot
		Requests []struct {
			ModelRequest
			Messages json.RawMessage `json:"messages"`
			Tools    json.RawMessage `json:"tools"`
		} `json:"requests"`
	}
	view.Snapshot = &snap
	if err := json.Unmarshal(b.data, &view); err != nil {
		return Snapshot{}, err
	}
	snap.Requests = make([]ModelRequest, len(view.Requests))
	for i, request := range view.Requests {
		snap.Requests[i] = request.ModelRequest
		if len(request.Messages) > 0 {
			if err := json.Unmarshal(request.Messages, &snap.Requests[i].Messages); err != nil {
				return Snapshot{}, err
			}
		}
	}
	// The shared base predates telemetry-only updates to Run.
	if err := json.Unmarshal(v.fields["run"], &snap.Run); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func (v *storedView) workspaceEventProjection(snap Snapshot) ([]Event, error) {
	v.workspaceOnce.Do(func() {
		v.indexEvents()
		if v.eventsErr != nil {
			v.workspaceErr = v.eventsErr
			return
		}
		start := 0
		current := ""
		owners := map[string]string{}
		if prior := v.workspaceReady.Load(); prior != nil && prior.through <= len(v.events) {
			snap.Events = append([]Event(nil), prior.events...)
			start = prior.through
			current = prior.current
			for call, request := range prior.owners {
				owners[call] = request
			}
		}
		for _, raw := range v.events[start:] {
			var event struct {
				Event
				Data snapshotJSON `json:"data"`
			}
			if v.workspaceErr = json.Unmarshal(raw, &event); v.workspaceErr != nil {
				return
			}
			if len(event.Data) > 0 {
				event.Event.Data = json.RawMessage(event.Data)
			}
			var identity struct {
				Request string `json:"request_id"`
				Call    string `json:"call_id"`
			}
			_ = json.Unmarshal(event.Data, &identity)
			if event.Type == "model_request" {
				current = identity.Request
			}
			if event.Type == "action_started" {
				owner := identity.Request
				if owner == "" {
					owner = current
				}
				owners[identity.Call] = owner
			}
			if identity.Request == "" && (identity.Call != "" || event.Type == "model_error") {
				owner := owners[identity.Call]
				if event.Type == "model_error" {
					owner = current
				}
				if owner != "" {
					var data map[string]json.RawMessage
					if json.Unmarshal(event.Data, &data) == nil && len(data) > 0 {
						data["request_id"], _ = json.Marshal(owner)
						event.Event.Data = data
					}
				}
			}
			snap.Events = append(snap.Events, event.Event)
		}
		settled, archived := map[string]bool{}, map[string]bool{}
		for _, response := range snap.Responses {
			settled[response.RequestID] = response.Error == ""
		}
		for _, message := range snap.Messages {
			if message.Role == "assistant" && message.RequestID != "" {
				archived[message.RequestID] = true
			}
		}
		v.workspaceEvents = WorkspaceEvents(snap, settled, archived)
		active := map[string]bool{}
		for _, action := range snap.Actions {
			if action.Result == nil {
				active[action.CallID] = true
			}
		}
		for call := range owners {
			if !active[call] {
				delete(owners, call)
			}
		}
		v.workspaceReady.Store(&workspaceProjection{events: v.workspaceEvents, through: len(v.events), current: current, owners: owners})
	})
	if v.workspaceErr != nil {
		return nil, v.workspaceErr
	}
	// Detach the bounded display tail; callers may mutate maps and RawMessage.
	raw, err := json.Marshal(v.workspaceEvents)
	if err != nil {
		return nil, err
	}
	var events []struct {
		Event
		Data json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(raw, &events); err != nil {
		return nil, err
	}
	result := make([]Event, len(events))
	for i, event := range events {
		result[i] = event.Event
		if len(event.Data) > 0 {
			result[i].Data = event.Data
		}
	}
	return result, nil
}

// Workspace follows an immutable committed generation. Source-bearing records
// still take Load's full artifact/source validation and persisted revocation.
func (s *Store) Workspace(id string) (Snapshot, error) {
	return s.workspace(id, true)
}

// WorkspaceMessages uses the same identity and provenance gates, but a message
// page has no event consumer. Avoid rebuilding the live display projection on
// each historical page while the committed SSE generation is advancing.
func (s *Store) WorkspaceMessages(id string) (Snapshot, error) {
	return s.workspace(id, false)
}

// ActivityWorkspace is display-only committed activity, never acceptance proof.
// It deliberately does not call Load or persist source revocations. Evidence,
// result and coverage are omitted by its HTTP projection; their tabs use Load.
func (s *Store) workspace(id string, includeEvents bool) (Snapshot, error) {
	if s.sqlite {
		return s.sqliteWorkspace(id, includeEvents)
	}
	defer s.ReleaseEventView(id)
	v, err := s.viewFields(id, true)
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := v.workspaceSnapshot()
	if err != nil {
		return Snapshot{}, err
	}
	if snap.Run.Deleted {
		return Snapshot{}, os.ErrNotExist
	}
	if len(snap.Evidence)+len(snap.EvidenceChains)+len(snap.EvidenceChainRefs)+len(snap.Assets)+len(snap.Workers)+len(snap.Plan.Tasks)+len(snap.Requirements)+len(snap.Checks)+len(snap.Findings) > 0 {
		return s.Load(id)
	}
	snap.artifactRoot = s.root
	previous := snap.Result
	refreshFrontier(&snap.Plan)
	refreshResult(&snap)
	if !reflect.DeepEqual(previous, snap.Result) {
		return s.Load(id)
	}
	if includeEvents {
		snap.Events, err = v.workspaceEventProjection(snap)
	}
	return snap, err
}
