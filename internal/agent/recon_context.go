package agent

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ReconContext is a bounded index into the existing Store, never another
// knowledge store or acceptance gate. Unknowns are reasons to inspect only
// when material to the goal; they do not mandate network collection.
type ReconContext struct {
	Digest           string         `json:"digest"`
	SourceRunID      string         `json:"source_run_id"`
	RootContractHash string         `json:"root_contract_hash,omitempty"`
	ExecutionRole    string         `json:"execution_role,omitempty"`
	ObjectVersion    *EvidenceScope `json:"object_version,omitempty"`
	Sources          []ReconSource  `json:"sources,omitempty"`
	Tools            []string       `json:"tools,omitempty"`
	ToolCount        int            `json:"tool_count"`
	SearchSources    []string       `json:"search_sources,omitempty"`
	Gaps             []string       `json:"gaps,omitempty"`
	Truncated        bool           `json:"truncated,omitempty"`
	PreparedAt       time.Time      `json:"prepared_at"`
}

type ReconSource struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Hash   string `json:"hash"`
	Status string `json:"status"`
	Label  string `json:"label,omitempty"`
	Value  string `json:"value,omitempty"`
}

func (a *Agent) redactConfiguredSecrets(text string) string {
	for _, provider := range a.modelProviders {
		if provider.APIKey != "" {
			text = strings.ReplaceAll(text, provider.APIKey, "[redacted]")
		}
	}
	if a.searchSources != nil {
		for _, config := range a.searchSources.configs {
			for _, secret := range []string{config.APIKey, config.Email} {
				if secret != "" {
					text = strings.ReplaceAll(text, secret, "[redacted]")
				}
			}
		}
	}
	return text
}

func (a *Agent) reconText(text string) string {
	text = a.redactConfiguredSecrets(text)
	if u, err := url.Parse(text); err == nil && u.Host != "" {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		text = u.String()
	}
	return excerpt(text, 512)
}

func (a *Agent) collectKnownInputs(s *Snapshot, task *PlanTask) *ReconContext {
	c := &ReconContext{SourceRunID: s.Run.ID}
	if s.RootContract != nil {
		c.RootContractHash = valueHash(s.RootContract)
	}
	if task != nil {
		c.ExecutionRole, c.ObjectVersion = task.ExecutionRole, task.EvidenceScope
	} else if s.WorkerContext != nil {
		c.ExecutionRole, c.ObjectVersion = s.WorkerContext.ExecutionRole, s.WorkerContext.EvidenceScope
	}
	if c.ObjectVersion != nil {
		scope := *c.ObjectVersion
		c.ObjectVersion = &scope
	}
	add := func(source ReconSource) {
		if len(c.Sources) >= 32 {
			c.Truncated = true
			return
		}
		source.Label, source.Value = a.reconText(source.Label), a.reconText(source.Value)
		c.Sources = append(c.Sources, source)
	}
	seen := map[string]bool{}
	for _, message := range s.Messages {
		for _, attachment := range message.Attachments {
			if seen[attachment.ID] || task != nil && !hasString(task.InputAttachmentIDs, attachment.ID) {
				continue
			}
			seen[attachment.ID] = true
			add(ReconSource{Kind: "attachment", ID: attachment.ID, Hash: attachment.Hash, Label: attachment.Name, Status: "user_input_not_evidence"})
		}
	}
	for _, asset := range s.Assets {
		if task != nil && task.ExecutionRole != "recon" && !hasString(task.AssetIDs, asset.ID) && !hasString(task.RequiredAssetIDs, asset.ID) {
			continue
		}
		add(ReconSource{Kind: "asset", ID: asset.ID, Hash: workerContextHash(asset), Label: asset.Kind, Value: asset.Value, Status: "declared_" + asset.Coverage})
	}
	// Keep a small tail of source references, not a copy of outputs/history.
	count := 0
	for i := len(s.Evidence) - 1; i >= 0 && count < 8; i-- {
		evidence := s.Evidence[i]
		if task != nil && !hasString(task.ContextEvidenceIDs, evidence.ID) {
			continue
		}
		status := "unverified_source"
		if verifyEvidence(s, &evidence) == "verified" {
			status = "source_verified_not_target_verified"
		}
		label := ""
		for _, action := range s.Actions {
			if action.CallID == evidence.CallID && action.RequestID == evidence.RequestID {
				label = action.Name
				break
			}
		}
		add(ReconSource{Kind: "evidence", ID: evidence.ID, Hash: evidence.ContentHash, Label: label, Status: status})
		count++
	}
	for _, tool := range a.registry.Tools() {
		c.ToolCount++
		name := tool.Definition.Function.Name
		if hasString([]string{"read", "artifact_read", "tool_search", "assets", "search_sources", "source_search", "exec", "write", "edit"}, name) || task != nil && hasString(task.RequiredToolIDs, name) {
			c.Tools = append(c.Tools, name)
		}
	}
	sort.Strings(c.Tools)
	for _, status := range a.SearchSourceStatus() {
		if status.Enabled && status.Configured {
			c.SearchSources = append(c.SearchSources, status.Source)
		}
	}
	if c.ObjectVersion == nil {
		c.Gaps = append(c.Gaps, "Object/version is unspecified; inspect only if needed for this task. Do not infer versions from search hits.")
	}
	for _, action := range s.Actions {
		if action.Status == "uncertain" || action.Status == "started" {
			c.Gaps = append(c.Gaps, "Unsettled action "+action.CallID+" ("+action.Name+"): reconcile its existing receipt; never automatically repeat it.")
			if len(c.Gaps) >= 5 {
				c.Truncated = true
				break
			}
		}
	}
	if c.Truncated {
		c.Gaps = append(c.Gaps, "Input index is bounded; use existing assets, artifact_read and tool_search to inspect omitted sources when relevant.")
	}
	c.Digest = valueHash(c)
	return c
}

func (a *Agent) prepareRecon(s *Snapshot) {
	c := a.collectKnownInputs(s, nil)
	if s.Recon != nil && s.Recon.Digest == c.Digest {
		return
	}
	c.PreparedAt = time.Now().UTC()
	s.Recon = c
	addEvent(s, "recon_prepared", "已整理已知输入、对象版本、工具与待核缺口", map[string]any{"digest": c.Digest, "sources": len(c.Sources), "root_contract_hash": c.RootContractHash})
}

func (a *Agent) reconContext(s *Snapshot) string {
	if s.Recon == nil {
		return ""
	}
	data, _ := json.Marshal(s.Recon)
	return "\nPrepared inputs (source data): root intent is already fixed. Use these existing attachments/facts/version references and available tools before new collection. Unknowns are not a prerequisite or a reason to scan. Simple input-only tasks need no external calls. If a material gap remains, use read/HTTP/browser/tool_search or configured source_search, and record only relevant sourced facts. For an independent collection stage use plan execution_role=recon within the same worker pool; pass selected asset_ids/context_evidence_ids/input_attachment_ids to later tasks. Discovery candidates never establish ownership/current target state or satisfy acceptance.\n" + a.redactConfiguredSecrets(string(data))
}

func (a *Agent) reprojectKnownInputs(parent *Snapshot, pinned *ReconContext) *ReconContext {
	if pinned == nil {
		return nil
	}
	raw, _ := json.Marshal(pinned)
	var projected ReconContext
	_ = json.Unmarshal(raw, &projected)
	for i := range projected.Sources {
		ref := &projected.Sources[i]
		currentHash := ""
		switch ref.Kind {
		case "attachment":
			for _, message := range parent.Messages {
				for _, attachment := range message.Attachments {
					if attachment.ID == ref.ID {
						currentHash = attachment.Hash
					}
				}
			}
		case "asset":
			if asset := contextAsset(parent, ref.ID); asset != nil {
				currentHash = workerContextHash(asset)
			}
		case "evidence":
			for _, evidence := range parent.Evidence {
				if evidence.ID == ref.ID {
					currentHash = evidence.ContentHash
					if verifyEvidence(parent, &evidence) != "verified" {
						ref.Status = "unverified_source"
					}
				}
			}
		}
		if currentHash == "" {
			ref.Status, ref.Value = "missing", ""
		} else if currentHash != ref.Hash {
			ref.Status, ref.Value = "changed", ""
		}
	}
	return &projected
}
