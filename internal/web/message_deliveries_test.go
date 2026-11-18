package web

import (
	"encoding/json"
	"testing"

	"trace/internal/agent"
)

func TestU13DeliveryAPIReadOnlyContract(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("delivery API")
	if err != nil {
		t.Fatal(err)
	}
	_, q, _, err := a.QueueMessageOperation(snap.Run.ID, "steer", "visible-op", "user input")
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/" + snap.Run.ID + "/message-deliveries"
	h := s.Handler()
	w := request(t, h, "GET", path+"?limit=1", nil, 200)
	var page agent.MessageDeliveryPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].MessageID != q.ID || page.Items[0].OperationID != "visible-op" || page.Items[0].HistoryStatus != "pending" || page.Items[0].RequestStatus != "not_archived" {
		t.Fatalf("wrong receipt: %s", w.Body.String())
	}
	for _, raw := range []string{"0", "101", "invalid"} {
		request(t, h, "GET", path+"?limit="+raw, nil, 400)
	}
	request(t, h, "GET", path+"?before=absent", nil, 409)
	request(t, h, "GET", "/api/runs/absent/message-deliveries", nil, 404)
	// The existing generic run route returns 404 for unknown write operations.
	request(t, h, "PUT", path, map[string]string{}, 404)
	after, err := s.store.Load(snap.Run.ID)
	if err != nil || !after.Mailbox[0].Pending() || len(after.Requests) != 0 || len(after.Actions) != 0 {
		t.Fatal("read route consumed or executed input", err)
	}
}
