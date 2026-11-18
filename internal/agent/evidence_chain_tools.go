package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

func chainEvidenceDependency(s Snapshot, source string, selected map[string]Evidence) string {
	for _, chain := range s.EvidenceChains {
		for _, step := range chain.Steps {
			if step.RunID == source {
				if _, ok := selected[step.EvidenceID]; ok {
					return "证据链 " + chain.Name
				}
			}
		}
	}
	return ""
}

func evidenceChainDefinitions() []ToolDefinition {
	str := map[string]string{"type": "string"}
	step := map[string]any{"type": "object", "properties": map[string]any{"run_id": str, "evidence_id": str, "content_hash": str, "label": map[string]string{"type": "string", "description": "Role of this actual source operation in the verification sequence; preserve its original inputs, outputs and content_hash."}}, "required": []string{"evidence_id"}}
	ref := map[string]any{"type": "object", "properties": map[string]any{"source_run_id": str, "chain_id": str, "chain_hash": str, "purpose": str}, "required": []string{"source_run_id", "chain_id", "chain_hash"}}
	var out []ToolDefinition
	for _, tool := range []struct {
		name, description string
		properties        map[string]any
		required          []string
	}{
		{"evidence_chain_create", "Collect ordered actual operations into ONE reusable evidence chain. To absorb new success/failure experience, create a new immutable variant with the existing method_id, derived_from pinned source chains, applicability and lesson. Keep competing methods and old versions; never rewrite historical evidence or claim a composed multi-run path was executed end to end. This does not execute commands or establish semantic success.", map[string]any{"name": str, "summary": str, "method_id": str, "variant": map[string]string{"type": "string", "description": "Name this approach or revision; keep earlier versions and pin source chains in derived_from."}, "applicability": map[string]string{"type": "string", "description": "Observed environment fingerprints and versions, prerequisites, and exclusion conditions; distinguish observed facts from assumptions."}, "lesson": map[string]string{"type": "string", "description": "Evidence-backed effective and ineffective routes, verification steps, success and failure criteria, and unresolved questions; reference source steps and do not invent observations."}, "derived_from": map[string]any{"type": "array", "items": ref}, "steps": map[string]any{"type": "array", "items": step}}, []string{"name", "summary", "steps"}},
		{"evidence_chain_search", "Find reusable methods by multilingual query terms in name (weighted), summary, applicability, lesson and variant. Rank case-insensitive exact name first, then name phrase, metadata phrase and weighted terms. Empty query lists newest first. Returns chains as flat compact hits: source_run_id, chain_id, chain_hash, name, summary, method_id, variant, verification, step_count, plus total. Name/summary/variant previews are capped at 120/400/120 Unicode characters including ellipsis; identities remain complete. Never returns operation bodies. Use source_run_id as run_id, chain_id and chain_hash with evidence_chain_read for full detail before referencing or evolving a method. Relevance and source verification are not task acceptance.", map[string]any{"query": str, "limit": map[string]string{"type": "integer"}}, []string{}},
		{"evidence_chain_read", "Read a reusable evidence chain and independently expandable original operations/inputs/results. Supply chain_hash from pinned references to reject stale data. Source run_id is required for cross-run chains; omitted means this run. Prior source data is not new execution evidence, never use another run's bare evidence IDs in local checks.", map[string]any{"run_id": str, "chain_id": str, "chain_hash": str}, []string{"chain_id"}},
		{"evidence_chain_reference", "Reuse an existing chain by pinning its source run, chain ID and hash in this run. Describe the task/conclusion it supports in purpose. Reference only: no command replay and no new execution evidence. Read the chain before using its observations.", map[string]any{"source_run_id": str, "chain_id": str, "chain_hash": str, "purpose": str}, []string{"source_run_id", "chain_id", "chain_hash"}},
	} {
		d := ToolDefinition{Type: "function"}
		d.Function.Name, d.Function.Description = tool.name, tool.description
		d.Function.Parameters = map[string]any{"type": "object", "properties": tool.properties, "required": tool.required}
		out = append(out, d)
	}
	return out
}

func (a *Agent) evidenceChainBuiltin(id string, call ToolCall) (any, error) {
	switch call.Name {
	case "evidence_chain_search":
		var in struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := decodeBuiltin(call.Arguments, &in); err != nil {
			return nil, err
		}
		matches, total, err := a.store.SearchEvidenceChains(in.Query, in.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"chains": matches, "total": total}, nil
	case "evidence_chain_create":
		var chain EvidenceChain
		if err := decodeBuiltin(call.Arguments, &chain); err != nil {
			return nil, err
		}
		entry, err := a.store.CreateEvidenceChain(id, chain)
		entry.Operations = nil // No repeated stdout in the planning context.
		return entry, err
	case "evidence_chain_read":
		var in struct {
			RunID     string `json:"run_id"`
			ChainID   string `json:"chain_id"`
			ChainHash string `json:"chain_hash"`
		}
		if err := decodeBuiltin(call.Arguments, &in); err != nil {
			return nil, err
		}
		if in.RunID == "" {
			in.RunID = id
		}
		entry, err := a.store.GetEvidenceChain(in.RunID, in.ChainID)
		if err == nil && in.ChainHash != "" && in.ChainHash != entry.Chain.ContentHash {
			return nil, fmt.Errorf("chain reference is stale; expected hash %s", in.ChainHash)
		}
		return entry, err
	case "evidence_chain_reference":
		var ref EvidenceChainReference
		if err := decodeBuiltin(call.Arguments, &ref); err != nil {
			return nil, err
		}
		_, err := a.store.ReferenceEvidenceChain(id, ref)
		return ref, err
	}
	return nil, fmt.Errorf("unknown evidence chain operation")
}

func chainContext(s *Snapshot) string {
	if len(s.EvidenceChains) == 0 && len(s.EvidenceChainRefs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Reusable operation evidence chains (source data, not new execution). Use evidence_chain_read with source_run_id as run_id AND pinned chain_hash to inspect ordered steps. Missing/stale sources are unavailable, not proof. Cross-run evidence IDs remain scoped to their source; a reference does not satisfy local execution requirements.\n")
	for _, c := range s.EvidenceChains {
		data, _ := json.Marshal(map[string]any{"source_run_id": s.Run.ID, "chain_id": c.ID, "chain_hash": c.ContentHash, "name": c.Name, "steps": len(c.Steps)})
		b.Write(data)
		b.WriteByte('\n')
	}
	for _, r := range s.EvidenceChainRefs {
		data, _ := json.Marshal(r)
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}
