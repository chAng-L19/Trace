package agent

// Muteki context_manifest.py / prompt_assembly.py (AGPL-3.0, c60c8fee):
// derive accounting after projection, not by reselecting original sections.
// This is an index into the single archived request, never a second ledger.
import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

type ContextManifest struct {
	Boundary       string                   `json:"boundary"`
	Hash           string                   `json:"sha256"`
	Bytes          int                      `json:"bytes"`
	FramingBytes   int                      `json:"framing_bytes"`
	MessageCount   int                      `json:"message_count"`
	ToolCount      int                      `json:"tool_count"`
	Parts          []ContextManifestPart    `json:"parts"`
	Sections       []ContextManifestSection `json:"sections"`
	Omissions      []ContextOmission        `json:"omissions,omitempty"`
	CompactionID   string                   `json:"compaction_id,omitempty"`
	PlannerThrough int                      `json:"planner_through,omitempty"`
}
type ContextManifestPart struct {
	Kind  string `json:"kind"`
	Index int    `json:"index"`
	ID    string `json:"id,omitempty"`
	Bytes int    `json:"bytes"`
	Hash  string `json:"sha256"`
}
type ContextManifestSection struct {
	Name         string `json:"name"`
	MessageIndex int    `json:"message_index"`
	Offset       int    `json:"content_byte_offset"`
	EndOffset    int    `json:"content_byte_end"`
	Hash         string `json:"sha256"`
	Required     bool   `json:"required"`
}
type ContextOmission struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	Reason         string `json:"reason"`
	SourceBytes    int    `json:"source_bytes"`
	SourceHash     string `json:"source_sha256"`
	ProjectedBytes int    `json:"projected_bytes,omitempty"`
	ProjectedHash  string `json:"projected_sha256,omitempty"`
}

// boundary covers the exact JSON {messages,tools} projection, not HTTP model/
// stream options. Before hydration image URLs are CAS identities; after hydration
// these same wireMessages bytes include actual image data URLs.
func buildContextManifest(s *Snapshot, request ModelRequest, registered ...[]ToolDefinition) *ContextManifest {
	wire := wireMessages(request.Messages)
	payload, _ := json.Marshal(struct {
		Messages []requestWireMessage `json:"messages"`
		Tools    []ToolDefinition     `json:"tools,omitempty"`
	}{wire, request.Tools})
	out := &ContextManifest{Boundary: "provider_messages_tools", Hash: promptHash(string(payload)), Bytes: len(payload), MessageCount: len(wire), ToolCount: len(request.Tools), CompactionID: request.CompactionID, PlannerThrough: request.PlannerThrough}
	projected := map[string]Message{}
	summarySources := map[string]bool{}
	if request.Purpose == "context_summary" {
		for _, c := range s.Compactions {
			if c.ID == request.CompactionID {
				for _, id := range c.SourceMessageIDs {
					summarySources[id] = true
				}
			}
		}
	}
	for i, message := range request.Messages {
		data, _ := json.Marshal(wire[i])
		out.Parts = append(out.Parts, ContextManifestPart{Kind: "message", Index: i, ID: message.ID, Bytes: len(data), Hash: promptHash(string(data))})
		for _, attachment := range message.Attachments {
			if attachment.Delivery == "native_image" && attachment.dataURL == "" {
				out.Boundary = "canonical_messages_tools_before_image_hydration"
			}
		}
		if message.ID != "" {
			projected[message.ID] = message
		}
		out.Sections = append(out.Sections, manifestMessageSections(s, message, i)...)
	}
	selected := map[string]bool{}
	for i, tool := range request.Tools {
		data, _ := json.Marshal(tool)
		out.Parts = append(out.Parts, ContextManifestPart{Kind: "tool_schema", Index: i, ID: tool.Function.Name, Bytes: len(data), Hash: promptHash(string(data))})
		selected[tool.Function.Name] = true
	}
	out.FramingBytes = out.Bytes
	for _, part := range out.Parts {
		out.FramingBytes -= part.Bytes
	}
	for _, source := range s.Messages {
		got, present := projected[source.ID]
		if present && valueHash(source) == valueHash(got) {
			continue
		}
		data, _ := json.Marshal(source)
		omission := ContextOmission{Kind: "history_message", ID: source.ID, Reason: "history_not_in_request", SourceBytes: len(data), SourceHash: promptHash(string(data))}
		if summarySources[source.ID] {
			omission.Reason = "summary_source_not_individual_message"
		} else if request.CompactionID != "" {
			omission.Reason = "history_represented_by_compaction"
		}
		if present {
			data, _ = json.Marshal(got)
			omission.Reason, omission.ProjectedBytes, omission.ProjectedHash = "message_projected", len(data), promptHash(string(data))
		}
		out.Omissions = append(out.Omissions, omission)
	}
	for _, definitions := range registered {
		for _, tool := range definitions {
			if selected[tool.Function.Name] {
				continue
			}
			data, _ := json.Marshal(tool)
			out.Omissions = append(out.Omissions, ContextOmission{Kind: "tool_schema", ID: tool.Function.Name, Reason: "schema_not_selected", SourceBytes: len(data), SourceHash: promptHash(string(data))})
		}
	}
	// Plan omissions are based on the exact JSON in the final request section,
	// not on rerunning compactPlan against a possibly newer snapshot.
	for _, section := range out.Sections {
		if section.Name != "planner" {
			continue
		}
		text := request.Messages[section.MessageIndex].Content[section.Offset:section.EndOffset]
		line := strings.IndexByte(text, '\n')
		if line < 0 {
			continue
		}
		var projection struct {
			Plan struct {
				Tasks []PlanTask `json:"tasks"`
			} `json:"plan"`
			Facts []Event `json:"new_facts"`
		}
		if json.Unmarshal([]byte(text[line+1:]), &projection) != nil {
			continue
		}
		included := map[string]bool{}
		for _, task := range projection.Plan.Tasks {
			included[task.ID] = true
		}
		for _, task := range s.Plan.Tasks {
			if included[task.ID] {
				continue
			}
			data, _ := json.Marshal(task)
			out.Omissions = append(out.Omissions, ContextOmission{Kind: "plan_task", ID: task.ID, Reason: "outside_compact_planner_view", SourceBytes: len(data), SourceHash: promptHash(string(data))})
		}
		facts := map[int]bool{}
		for _, event := range projection.Facts {
			facts[event.Sequence] = true
		}
		for _, event := range s.Events {
			if event.Sequence <= request.PlannerThrough || facts[event.Sequence] {
				continue
			}
			switch event.Type {
			case "tool_result", "asset_added", "asset_updated", "relation_added", "worker_finished", "verification_invalidated", "task_retry_requested", "task_disposed":
				data, _ := json.Marshal(event)
				out.Omissions = append(out.Omissions, ContextOmission{Kind: "planner_fact", ID: strconv.Itoa(event.Sequence), Reason: "after_request_planner_watermark", SourceBytes: len(data), SourceHash: promptHash(string(data))})
			}
		}
	}
	if s.WorkerContext != nil {
		included := map[string]bool{}
		for _, message := range request.Messages {
			if message.ID != s.Run.ID+"-worker-method-candidates" {
				continue
			}
			_, raw, _ := strings.Cut(message.Content, "\n")
			var candidates []EvidenceChainSearchHit
			if json.Unmarshal([]byte(raw), &candidates) == nil {
				for _, candidate := range candidates {
					included[candidate.SourceRunID+"/"+candidate.ChainID+"/"+candidate.ChainHash] = true
				}
			}
		}
		for _, candidate := range s.WorkerContext.MethodCandidates {
			id := candidate.SourceRunID + "/" + candidate.ChainID + "/" + candidate.ChainHash
			if !included[id] {
				raw, _ := json.Marshal(candidate)
				out.Omissions = append(out.Omissions, ContextOmission{Kind: "method_candidate", ID: id, Reason: "optional_candidate_not_projected", SourceBytes: len(raw), SourceHash: promptHash(string(raw))})
			}
		}
	}
	return out
}

func manifestMessageSections(s *Snapshot, message Message, index int) []ContextManifestSection {
	type start struct {
		offset   int
		name     string
		required bool
	}
	name, required := "history_"+message.Role, false
	if message.Role == "system" {
		name, required = "runtime_prompt", true
	}
	if strings.HasSuffix(message.ID, "-worker-context") {
		name, required = "worker_sources", true
	}
	if strings.HasSuffix(message.ID, "-evidence-chains") {
		name, required = "evidence_chains", true
	}
	if strings.HasSuffix(message.ID, "-worker-method-candidates") {
		name, required = "method_candidates", false
	}
	if strings.HasPrefix(message.Content, "Historical conversation summary") || strings.HasPrefix(message.Content, "Earlier conversation excerpts") {
		name = "history_summary"
	}
	if strings.HasPrefix(message.Content, "Runtime completion audit:") {
		name, required = "completion_correction", true
	}
	if message.Role == "user" && message.ID != "" {
		first, last := "", ""
		for _, source := range s.Messages {
			if source.Role == "user" {
				if first == "" {
					first = source.ID
				}
				last = source.ID
			}
		}
		if message.ID == first {
			name, required = "original_objective", true
		} else if message.ID == last {
			name, required = "latest_instruction", true
		}
	}
	starts := []start{{0, name, required}}
	runtime := message.Role == "user" && message.ID == s.Run.ID+"-runtime-observations"
	if runtime || message.Role == "system" && strings.HasPrefix(message.Content, s.Prompt) {
		floor := 0
		if !runtime {
			floor = len(s.Prompt)
		} else {
			starts[0] = start{0, "runtime_observations", true}
		}
		for _, section := range []struct {
			marker, name string
			required     bool
		}{
			{"\n\nAvailable skills (metadata only;", "skill_catalog", false},
			{"Planner observations (data):", "planner", true},
			{"\nRuntime verification contract (data):", "verification_contract", true},
			{"\nDeterministic context ledger (", "file_artifact_ledger", true},
			{"\n\nExternal tool schemas are loaded on demand.", "tool_directory_notice", true},
		} {
			if offset := strings.Index(message.Content[floor:], section.marker); offset >= 0 {
				starts = append(starts, start{floor + offset, section.name, section.required})
			}
		}
	}
	sort.SliceStable(starts, func(i, j int) bool { return starts[i].offset < starts[j].offset })
	var sections []ContextManifestSection
	for i, at := range starts {
		end := len(message.Content)
		if i+1 < len(starts) {
			end = starts[i+1].offset
		}
		sections = append(sections, ContextManifestSection{Name: at.name, MessageIndex: index, Offset: at.offset, EndOffset: end, Hash: promptHash(message.Content[at.offset:end]), Required: at.required})
	}
	return sections
}
