package web

import (
	"encoding/json"
	"testing"

	"trace/internal/agent"
)

func TestOperationEvidenceChainAPI(t *testing.T) {
	s, a := fixture(t, nil)
	source, _ := a.Create("source path")
	target, _ := a.Create("reuse path")
	_, err := a.Store().Update(source.Run.ID, func(s *agent.Snapshot) error {
		s.Evidence = []agent.Evidence{{ID: "ev-1", Content: "original", ContentHash: "original-hash", Type: "manual_observation"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	path := "/api/runs/" + source.Run.ID + "/evidence-chains"
	request(t, h, "POST", path, map[string]any{"name": "path", "summary": "purpose", "steps": []map[string]string{{"evidence_id": "missing"}}}, 409)
	w := request(t, h, "POST", path, map[string]any{"name": "path", "summary": "purpose", "steps": []map[string]string{{"evidence_id": "ev-1"}}}, 200)
	var entry agent.EvidenceChainEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entry); err != nil || len(entry.Operations) != 1 {
		t.Fatalf("entry: %s %v", w.Body, err)
	}
	request(t, h, "GET", "/api/evidence-chains", nil, 200)
	request(t, h, "GET", "/api/evidence-chains/"+source.Run.ID+"/"+entry.Chain.ID, nil, 200)
	request(t, h, "GET", "/api/evidence-chains/"+source.Run.ID+"/"+entry.Chain.ID+"?hash="+entry.Chain.ContentHash, nil, 200)
	request(t, h, "GET", "/api/evidence-chains/"+source.Run.ID+"/"+entry.Chain.ID+"?hash=changed", nil, 409)
	w = request(t, h, "GET", "/api/evidence-chains?limit=1&run="+source.Run.ID, nil, 200)
	var page agent.EvidenceChainPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key == "" {
		t.Fatalf("bounded page: %s %v", w.Body, err)
	}
	request(t, h, "GET", "/api/evidence-chains?limit=101", nil, 400)
	ref := agent.EvidenceChainReference{SourceRunID: source.Run.ID, ChainID: entry.Chain.ID, ChainHash: "changed"}
	request(t, h, "POST", "/api/runs/"+target.Run.ID+"/evidence-chain-references", ref, 409)
	request(t, h, "POST", path, map[string]any{"name": "path", "summary": "purpose", "steps": []map[string]string{{"evidence_id": "ev-1"}}, "unknown": true}, 400)
}
