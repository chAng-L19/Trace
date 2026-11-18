package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"trace/internal/agent"
)

func TestDirectMessageOperationRetryDoesNotStartSecondTurn(t *testing.T) {
	s, a := fixture(t, providerFunc(func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		return agent.ModelResponse{Content: "done"}, nil
	}))
	snap, err := a.Create("original goal")
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/runs/" + snap.Run.ID
	body := map[string]string{"operation_id": "direct-op", "content": "supplement"}
	request(t, s.Handler(), "POST", base+"/message", body, 200)
	s.wg.Wait()
	retry := request(t, s.Handler(), "POST", base+"/message", body, 200)
	if !strings.Contains(retry.Body.String(), `"status":"delivered"`) {
		t.Fatalf("lost receipt: %s", retry.Body.String())
	}
	s.wg.Wait()
	final, err := a.Store().Load(snap.Run.ID)
	if err != nil || len(final.Requests) != 1 || len(final.Mailbox) != 1 || final.Run.Status != agent.StatusCompleted {
		t.Fatalf("retry reran: %+v %v", final.Run, err)
	}
	count := 0
	for _, m := range final.Messages {
		if m.Content == "supplement" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("input appeared %d times", count)
	}
}

func TestSessionMessageReceiptAPIAndQueueConflicts(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("message receipts")
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/runs/" + snap.Run.ID
	body := map[string]any{"operation_id": "stable-operation", "content": "original"}
	first := request(t, s.Handler(), "POST", base+"/steer", body, 200)
	var receipt struct {
		MessageID string `json:"message_id"`
		Revision  int    `json:"revision"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.MessageID == "" || receipt.Revision != 1 || receipt.Status != "queued" {
		t.Fatalf("incomplete receipt: %s", first.Body.String())
	}
	request(t, s.Handler(), "POST", base+"/steer", body, 200)
	lookup := request(t, s.Handler(), "GET", base+"/operations/stable-operation", nil, 200)
	if !strings.Contains(lookup.Body.String(), receipt.MessageID) {
		t.Fatal("lookup lost operation identity")
	}
	request(t, s.Handler(), "POST", base+"/steer", map[string]string{"operation_id": "stable-operation", "content": "different"}, 409)
	request(t, s.Handler(), "PATCH", base+"/mailbox/"+receipt.MessageID, map[string]any{"revision": 1, "content": "edited"}, 200)
	request(t, s.Handler(), "PATCH", base+"/mailbox/"+receipt.MessageID, map[string]any{"revision": 1, "content": "stale"}, 409)
	request(t, s.Handler(), "DELETE", base+"/mailbox/"+receipt.MessageID, map[string]int{"revision": 2}, 200)
	retry := request(t, s.Handler(), "POST", base+"/steer", body, 200)
	if !strings.Contains(retry.Body.String(), `"status":"cancelled"`) {
		t.Fatal("retry resurrected cancelled message")
	}
	request(t, s.Handler(), "PUT", base+"/mailbox/order", map[string]any{"message_ids": []string{}}, 200)
	final, err := a.Store().Load(snap.Run.ID)
	if err != nil || len(final.Mailbox) != 1 || final.Mailbox[0].Content != "edited" || final.Mailbox[0].Pending() {
		t.Fatalf("mailbox receipt corrupt: %+v %v", final.Mailbox, err)
	}
}

func TestSessionMetadataBodySearchAndCreationContractAPI(t *testing.T) {
	s, a := fixture(t, nil)
	w := request(t, s.Handler(), "POST", "/api/runs", map[string]any{"goal": "original goal", "title": "named run", "budget": agent.Budget{MaxModelRequests: 2}, "success_contains": "marker"}, 201)
	var snap agent.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Run.Title != "named run" || snap.Run.SuccessContains != "marker" || !snap.Run.BudgetConfigured {
		t.Fatalf("creation contract omitted: %+v", snap.Run)
	}
	base := "/api/runs/" + snap.Run.ID
	request(t, s.Handler(), "PUT", base+"/requirements", map[string]any{"requirements": []agent.VerificationRequirement{}, "success_contains": "updated"}, 200)
	contract := request(t, s.Handler(), "GET", base+"/requirements", nil, 200)
	if !strings.Contains(contract.Body.String(), `"editable":true`) || !strings.Contains(contract.Body.String(), "updated") {
		t.Fatal("contract inspection omitted editability")
	}
	_, err := a.Store().Update(snap.Run.ID, func(s *agent.Snapshot) error {
		s.Messages = []agent.Message{{ID: "body-id", Role: "assistant", Content: "body-only needle"}}
		s.Requests = []agent.ModelRequest{{ID: "executed"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request(t, s.Handler(), "PUT", base+"/requirements", map[string]any{"requirements": []agent.VerificationRequirement{}}, 409)
	request(t, s.Handler(), "PATCH", base, map[string]any{"title": "renamed", "archived": true}, 200)
	visible := request(t, s.Handler(), "GET", "/api/runs", nil, 200)
	if strings.Contains(visible.Body.String(), snap.Run.ID) {
		t.Fatal("archive shown by default")
	}
	archived := request(t, s.Handler(), "GET", "/api/runs?include_archived=true", nil, 200)
	if !strings.Contains(archived.Body.String(), snap.Run.ID) {
		t.Fatal("archive inaccessible")
	}
	search := request(t, s.Handler(), "GET", "/api/runs/search?q=needle&include_archived=true", nil, 200)
	if !strings.Contains(search.Body.String(), `"message_id":"body-id"`) {
		t.Fatalf("body deep link missing: %s", search.Body.String())
	}
	request(t, s.Handler(), "GET", base+"/messages/body-id", nil, 200)
	request(t, s.Handler(), "PATCH", base, map[string]any{"title": "", "archived": false}, 200)
	final, _ := a.Store().Load(snap.Run.ID)
	if final.Run.Title != "" || final.Run.Archived || final.Run.Goal != "original goal" || final.Run.SuccessContains != "updated" {
		t.Fatalf("metadata overwrote goal/contract: %+v", final.Run)
	}
}
