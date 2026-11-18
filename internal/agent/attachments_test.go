package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func inputPNG(t *testing.T) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestInputAttachmentNativeWirePersistenceAndFallback(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "workspace_file", true: "native_image"}[native], func(t *testing.T) {
			var wire struct {
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"observed"}}]}`))
			}))
			defer server.Close()
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			data := inputPNG(t)
			input, err := store.PutInputAttachment("original.png", bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			a, err := New(store, &OpenAIProvider{BaseURL: server.URL, Model: "fixture", ImageInput: native}, "")
			if err != nil {
				t.Fatal(err)
			}
			s, err := a.Create("inspect the input", input)
			if err != nil {
				t.Fatal(err)
			}
			s, err = a.Run(context.Background(), s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(wire.Messages) < 2 {
				t.Fatalf("no wire input: %+v", wire)
			}
			content := wire.Messages[1].Content
			if native {
				var parts []struct {
					Type     string `json:"type"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				}
				if err := json.Unmarshal(content, &parts); err != nil {
					t.Fatal(err)
				}
				if len(parts) != 2 || parts[1].Type != "image_url" || parts[1].ImageURL.URL != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data) {
					t.Fatalf("native image lost: %s", content)
				}
			} else {
				var text string
				if err := json.Unmarshal(content, &text); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(text, "workspace_file") || strings.Contains(text, "image_url") {
					t.Fatalf("incorrect file delivery: %s", text)
				}
				b, err := os.ReadFile(s.Messages[0].Attachments[0].Path)
				if err != nil || !bytes.Equal(b, data) {
					t.Fatalf("file not readable: %v", err)
				}
			}
			s, err = store.Load(s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			original := s.Messages[0].Attachments[0]
			if original.Hash != input.Hash || original.SourceRunID != s.Run.ID || original.SourceMessageID != s.Messages[0].ID || len(s.Evidence) != 0 {
				t.Fatalf("source identity changed: %+v", original)
			}
			raw, err := os.ReadFile(filepath.Join(store.root, s.Run.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(data))) {
				t.Fatal("base64 persisted in run")
			}
			if len(s.Requests[0].Messages[1].Attachments) != 1 {
				t.Fatal("request lost attachment identity")
			}
			branch, err := a.Branch(s.Run.ID, s.Messages[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if branch.Messages[0].Attachments[0].SourceRunID != s.Run.ID {
				t.Fatal("branch relabeled original input")
			}
			if _, err = a.Run(context.Background(), branch.Run.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInputAttachmentTamperMissingAndRoles(t *testing.T) {
	for _, mode := range []string{"missing", "changed", "metadata", "role", "origin"} {
		t.Run(mode, func(t *testing.T) {
			store, _ := NewStore(t.TempDir())
			a, _ := New(store, successProvider{}, "")
			input, err := store.PutInputAttachment("sample.txt", strings.NewReader("input-original"))
			if err != nil {
				t.Fatal(err)
			}
			s, err := a.Create("inspect", input)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				err = os.Remove(filepath.Join(store.root, "artifacts", input.Hash))
			case "changed":
				err = os.WriteFile(filepath.Join(store.root, "artifacts", input.Hash), []byte("input-modified"), 0600)
			case "metadata":
				_, err = store.Update(s.Run.ID, func(s *Snapshot) error { s.Messages[0].Attachments[0].Name = "forged"; return nil })
			case "role":
				_, err = store.Update(s.Run.ID, func(s *Snapshot) error { s.Messages[0].Role = "tool"; return nil })
			case "origin":
				_, err = store.Update(s.Run.ID, func(s *Snapshot) error { s.Messages[0].Attachments[0].SourceMessageID = "forged-origin"; return nil })
			}
			if err != nil {
				t.Fatal(err)
			}
			s, err = a.Run(context.Background(), s.Run.ID)
			if err == nil || s.Run.Status != StatusFailed || len(s.Requests) != 0 || s.Run.Error == "" {
				t.Fatalf("bad attachment reached provider: %+v", s.Run)
			}
		})
	}
}

func TestInputAttachmentQueueProjectionAndChildSelection(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	a, _ := New(store, successProvider{}, "")
	first, _ := store.PutInputAttachment("first.txt", strings.NewReader("first"))
	second, _ := store.PutInputAttachment("second.txt", strings.NewReader("second"))
	s, err := a.Create("goal", first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.QueueMessage(s.Run.ID, "steer", "next", second); err != nil {
		t.Fatal(err)
	}
	// Reconstructing the Agent simulates an operator resume after process exit.
	a, _ = New(store, successProvider{}, "")
	s, err = a.Run(context.Background(), s.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages[1].Attachments) != 1 || s.Messages[1].Attachments[0].SourceMessageID != s.Mailbox[0].ID {
		t.Fatal("queue lost input identity")
	}
	if !strings.Contains(contextSummary(s.Messages, 4096), first.Hash) {
		t.Fatal("projection summary lost attachment reference")
	}
	inputs, err := taskInputAttachments(&s, []string{second.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].ID != second.ID {
		t.Fatal("child received unrelated input")
	}
	if _, err = taskInputAttachments(&s, []string{"unrelated"}); err == nil {
		t.Fatal("unknown parent input accepted")
	}
	child, err := a.Create("child goal", inputs...)
	if err != nil {
		t.Fatal(err)
	}
	if child.Messages[0].Attachments[0].SourceRunID != s.Run.ID {
		t.Fatal("child changed origin")
	}
	if _, err = a.SetPlan(s.Run.ID, []PlanTask{{ID: "input-task", Goal: "inspect selected input", InputAttachmentIDs: []string{second.ID}}}); err != nil {
		t.Fatal(err)
	}
	// Parent has completed, so create a runnable branch for dispatch.
	branch, err := a.Branch(s.Run.ID, s.Messages[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(branch.Run.ID, []PlanTask{{ID: "input-task", Goal: "inspect selected input", InputAttachmentIDs: []string{second.ID}}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), branch.Run.ID, "input-task")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.StopWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	child, err = store.Load(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Messages[0].Attachments) != 1 || child.Messages[0].Attachments[0].ID != second.ID || child.Messages[0].Attachments[0].SourceRunID != s.Run.ID {
		t.Fatal("dispatch lost selected input origin")
	}
}
