package agent

// pi compaction.ts (MIT, e65b3b88): retain deterministic file operations beside
// prose summaries. Trace derives them from successful archived actions, never
// from a model's summary, and keeps branch-source identity separate from proof.
import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type ContextLedgerSource struct {
	RunID      string `json:"source_run_id"`
	RequestID  string `json:"request_id"`
	CallID     string `json:"call_id"`
	EvidenceID string `json:"evidence_id,omitempty"`
}
type ContextFileOperation struct {
	ContextLedgerSource
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Hash      string `json:"hash,omitempty"`
}
type ContextLedgerArtifact struct {
	ContextLedgerSource
	ArtifactRef
}
type ContextLedger struct {
	Files        []ContextFileOperation  `json:"files,omitempty"`
	Artifacts    []ContextLedgerArtifact `json:"artifacts,omitempty"`
	Unmet        []string                `json:"unmet_requirements,omitempty"`
	PendingTasks []string                `json:"pending_tasks,omitempty"`
	Issues       []string                `json:"issues,omitempty"`
}

func successfulArchivedAction(action Action) bool {
	r := action.Result
	return action.Status == "completed" && r != nil && r.ExitCode == 0 && r.Error == "" && !r.Interrupted && !r.MCPIsError && !r.OutcomeUncertain && !r.WorkerOutcomeUncertain
}

func actionContextLedger(s *Snapshot, allowed map[string]bool) ContextLedger {
	var out ContextLedger
	files := map[string]int{}
	artifacts := map[string]bool{}
	for _, action := range s.Actions {
		if !successfulArchivedAction(action) || allowed != nil && !allowed[action.RequestID+"\x00"+action.CallID] {
			continue
		}
		source := ContextLedgerSource{RunID: s.Run.ID, RequestID: action.RequestID, CallID: action.CallID, EvidenceID: action.Result.EvidenceID}
		for _, ref := range action.Result.Artifacts {
			key := action.RequestID + "\x00" + action.CallID + "\x00" + ref.Kind + "\x00" + ref.Hash
			if artifacts[key] {
				continue
			}
			artifacts[key] = true
			ref.Preview = nil
			out.Artifacts = append(out.Artifacts, ContextLedgerArtifact{ContextLedgerSource: source, ArtifactRef: ref})
		}
		if action.Adapter != "builtin" || action.Name != "read" && action.Name != "write" && action.Name != "edit" {
			continue
		}
		file, err := archivedFileMetadata(s.artifactRoot, *action.Result)
		if err != nil {
			out.Issues = append(out.Issues, fmt.Sprintf("source %s request %s call %s file metadata unavailable: %s", s.Run.ID, action.RequestID, action.CallID, err))
			continue
		}
		operation := "modified"
		if action.Name == "read" {
			operation = "read"
		}
		entry := ContextFileOperation{ContextLedgerSource: source, Operation: operation, Path: file.Path, Hash: file.Hash}
		key := operation + "\x00" + file.Path
		if index, exists := files[key]; exists {
			out.Files[index] = entry
		} else {
			files[key] = len(out.Files)
			out.Files = append(out.Files, entry)
		}
	}
	return out
}

// Decode file metadata directly from archived JSON, never from its preview.
// CAS still receives the same full integrity pass as other evidence reads.
func archivedFileMetadata(root string, result ToolResult) (struct{ Path, Hash string }, error) {
	var file struct{ Path, Hash string }
	var reader io.Reader = strings.NewReader(result.Stdout)
	for _, ref := range result.Artifacts {
		if ref.Kind != "stdout" {
			continue
		}
		store := Store{root: root}
		stream, err := store.OpenArtifact(ref.Hash)
		if err != nil {
			return file, err
		}
		defer stream.Close()
		reader = stream
		break
	}
	if err := json.NewDecoder(reader).Decode(&file); err != nil {
		return file, err
	}
	if file.Path == "" {
		return file, fmt.Errorf("archived file result has no path")
	}
	return file, nil
}

// A branch inherits context, not actions/evidence. Resolve source ledgers at
// each exact branch message boundary; later parent actions must never leak in.
// Worker children have no ParentMessageID and do not inherit an entire parent.
func (a *Agent) inheritedContextLedger(s *Snapshot) (ContextLedger, error) {
	var out ContextLedger
	seen := map[string]bool{s.Run.ID: true}
	retained := map[string]bool{}
	for _, message := range s.Messages {
		if message.Role == "tool" {
			retained[message.ToolCallID+"\x00"+promptHash(message.Content)] = true
		}
	}
	run := s.Run
	for run.ParentRunID != "" && run.ParentMessageID != "" {
		if seen[run.ParentRunID] {
			return out, fmt.Errorf("branch context ancestry cycle")
		}
		seen[run.ParentRunID] = true
		parent, err := a.store.Load(run.ParentRunID)
		if err != nil {
			return out, fmt.Errorf("branch context source %s: %w", run.ParentRunID, err)
		}
		assignMessageIDs(&parent)
		allowed := map[string]bool{}
		found := false
		for _, message := range parent.Messages {
			if message.Role == "tool" && retained[message.ToolCallID+"\x00"+promptHash(message.Content)] {
				// Legacy tool messages have no RequestID. Their exact archived
				// evidence content disambiguates call IDs reused by later requests.
				for _, evidence := range parent.Evidence {
					if evidence.CallID == message.ToolCallID && evidence.ContentHash == promptHash(message.Content) && evidence.Content == message.Content {
						allowed[evidence.RequestID+"\x00"+evidence.CallID] = true
					}
				}
			}
			if message.ID == run.ParentMessageID {
				found = true
				break
			}
		}
		if !found {
			return out, fmt.Errorf("branch context boundary %s missing", run.ParentMessageID)
		}
		part := actionContextLedger(&parent, allowed)
		out.Files = append(part.Files, out.Files...)
		out.Artifacts = append(part.Artifacts, out.Artifacts...)
		out.Issues = append(part.Issues, out.Issues...)
		run = parent.Run
	}
	return out, nil
}

func contextLedgerContext(s *Snapshot, inherited ContextLedger) string {
	local := actionContextLedger(s, nil)
	inherited.Files = append(inherited.Files, local.Files...)
	inherited.Artifacts = append(inherited.Artifacts, local.Artifacts...)
	inherited.Issues = append(inherited.Issues, local.Issues...)
	inherited.Unmet = unmetRequirements(s)
	for _, task := range s.Plan.Tasks {
		if !planTaskSatisfied(&s.Plan, task, map[string]bool{}) {
			inherited.PendingTasks = append(inherited.PendingTasks, task.ID)
		}
	}
	if len(inherited.Files)+len(inherited.Artifacts)+len(inherited.Unmet)+len(inherited.PendingTasks)+len(inherited.Issues) == 0 {
		return ""
	}
	raw, _ := json.Marshal(inherited)
	return "\nDeterministic context ledger (archived source data, not proof of conclusions). File entries record the latest successful read/modification per path and source run; inherited entries retain original run/request/call identity and are not new branch evidence. Artifact hashes address complete CAS bytes, not previews. Summaries never add entries. Pending acceptance remains governed by the exact verification contract.\n" + string(raw)
}
