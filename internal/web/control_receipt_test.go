package web

import (
	"encoding/json"
	"strings"
	"testing"

	"trace/internal/agent"
)

func TestPauseCancelReturnSmallDurableReceipt(t *testing.T) {
	for _, op := range []string{"pause", "cancel"} {
		t.Run(op, func(t *testing.T) {
			s, a := fixture(t, nil)
			snap, err := a.Create("control")
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Store().Update(snap.Run.ID, func(v *agent.Snapshot) error {
				v.Run.Status = agent.StatusRunning
				v.Messages = []agent.Message{{Role: "user", Content: strings.Repeat("private archive ", 1000)}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			w := request(t, s.Handler(), "POST", "/api/runs/"+snap.Run.ID+"/"+op, map[string]any{}, 200)
			if w.Body.Len() > 512 || strings.Contains(w.Body.String(), "private archive") {
				t.Fatal("control returned historical snapshot")
			}
			var receipt struct {
				Accepted bool         `json:"accepted"`
				ID       string       `json:"id"`
				Status   agent.Status `json:"status"`
				Sequence int          `json:"event_seq"`
			}
			if err = json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
			stored, err := a.Store().Observe(snap.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !receipt.Accepted || receipt.ID != snap.Run.ID || receipt.Status != stored.Run.Status || receipt.Sequence != stored.Run.EventSeq {
				t.Fatal("receipt did not identify committed control", receipt)
			}
		})
	}
}
