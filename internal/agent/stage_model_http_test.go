package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStageModelSelectionConcurrentHTTPImages(t *testing.T) {
	type observed struct {
		Path, Model, Effort string
		Image               bool
	}
	seen := make(chan observed, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var model, effort string
		if err := json.Unmarshal(body["model"], &model); err != nil {
			t.Error(err)
		}
		if raw := body["reasoning_effort"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &effort); err != nil {
				t.Error(err)
			}
		}
		seen <- observed{r.URL.Path, model, effort, bytes.Contains(body["messages"], []byte("data:image/png;base64,"))}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{\"choices\":[{\"message\":{\"content\":\"complete\"},\"finish_reason\":\"stop\"}]}")
	}))
	defer server.Close()
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "base"})
	providers := map[string]*OpenAIProvider{"vision": {BaseURL: server.URL + "/vision", Model: "vision", ImageInput: true, ReasoningEfforts: []string{"high"}}, "text": {BaseURL: server.URL + "/text", Model: "text"}}
	routes := []ModelRoute{{Provider: "vision", Model: "vision"}, {Provider: "text", Model: "text"}}
	if err := a.ConfigureModels(providers, RoleModels{StageRoutes: routes}); err != nil {
		t.Fatal(err)
	}
	input, err := store.PutInputAttachment("input.png", bytes.NewReader(inputPNG(t)))
	if err != nil {
		t.Fatal(err)
	}
	root, err := a.Create("review image and text independently", input)
	if err != nil {
		t.Fatal(err)
	}
	tasks := []PlanTask{{ID: "image", Goal: "inspect image", InputAttachmentIDs: []string{input.ID}, ModelSelection: &ModelSelection{Provider: "vision", Model: "vision", ReasoningEffort: "high"}}, {ID: "text", Goal: "review text", ModelSelection: &ModelSelection{Provider: "text", Model: "text"}}}
	if _, err = a.SetPlan(root.Run.ID, tasks); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, task := range tasks {
		go func(id string) { _, err := a.DispatchTask(context.Background(), root.Run.ID, id); errs <- err }(task.ID)
	}
	for range tasks {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if _, err = a.WaitWorkers(context.Background(), root.Run.ID, 10); err != nil {
		t.Fatal(err)
	}
	got := map[string]observed{}
	for range tasks {
		select {
		case item := <-seen:
			got[item.Model] = item
		case <-time.After(5 * time.Second):
			t.Fatal("missing HTTP request")
		}
	}
	if got["vision"].Path != "/vision/chat/completions" || got["vision"].Effort != "high" || !got["vision"].Image {
		t.Fatalf("vision wire mismatch: %+v", got)
	}
	if got["text"].Path != "/text/chat/completions" || got["text"].Effort != "" || got["text"].Image {
		t.Fatalf("text wire mismatch: %+v", got)
	}
}
