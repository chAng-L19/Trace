package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPlannerProjectionKeepsOneWorkProgressLedger(t *testing.T) {
	progress := WorkProgress{WorkReference: WorkReference{TaskID: "source", ItemID: "remaining"}, Goal: "inspect remaining scope", Status: "pending"}
	snapshot := Snapshot{Run: Run{EventSeq: 1}, Events: []Event{{Sequence: 1, Type: "asset_added"}}, Plan: PlanState{Tasks: []PlanTask{{ID: "source", Goal: "inspect scope", Status: "pending"}}, WorkProgress: []WorkProgress{progress}}}
	for round := 0; round < 2; round++ {
		observation, through := plannerContextFrom(&snapshot)
		_, body, ok := strings.Cut(observation, "\n")
		if !ok {
			t.Fatal("missing planner observation")
		}
		var view struct {
			Facts []Event `json:"new_facts"`
			Plan  struct {
				Progress []WorkProgress `json:"work_progress"`
			} `json:"plan"`
		}
		if err := json.Unmarshal([]byte(body), &view); err != nil {
			t.Fatal(err)
		}
		if strings.Count(body, `"work_progress"`) != 1 || len(view.Plan.Progress) != 1 || !reflect.DeepEqual(view.Plan.Progress[0], progress) {
			t.Fatalf("work obligations duplicated or omitted: %s", body)
		}
		if len(view.Facts) != 1-round || through != 1 {
			t.Fatalf("planner watermark did not consume observations: %+v through=%d", view.Facts, through)
		}
		snapshot.Requests = append(snapshot.Requests, ModelRequest{ID: "seen", PlannerThrough: through})
		snapshot.Responses = append(snapshot.Responses, ResponseRecord{RequestID: "seen"})
	}
}
