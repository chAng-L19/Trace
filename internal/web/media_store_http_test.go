package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"trace/internal/agent"
)

// Real registration through Agent -> Store/CAS -> reopened Store -> HTTP socket.
// Browser rendering and blob lifecycle are verified separately, not mocked here.
func TestMediaMultiSourceStoreReopenHTTP(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jGZkAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	file := filepath.Join(t.TempDir(), "pixel.png")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := providerFunc(func(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
		calls++
		if calls <= 2 {
			name := []string{"first.png", "second.png"}[calls-1]
			args, _ := json.Marshal(agent.ArtifactRegistration{Path: file, Name: name, Purpose: "media persistence verification"})
			return agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "shared-call", Name: "artifact_register", Arguments: string(args)}}}, nil
		}
		return agent.ModelResponse{Content: "captured media"}, nil
	})
	a, err := agent.New(store, provider, "")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := a.Create("register the same bytes in two requests")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Run(context.Background(), initial.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Actions) != 2 {
		t.Fatalf("actions: %+v", snap.Actions)
	}
	if snap.Actions[0].RequestID == snap.Actions[1].RequestID {
		t.Fatal("distinct request provenance lost")
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	for _, action := range snap.Actions {
		if action.Result == nil || len(action.Result.Artifacts) != 1 {
			t.Fatalf("registration: %+v", action)
		}
		ref := action.Result.Artifacts[0]
		if ref.Hash != hash || ref.MIMEType != "image/png" {
			t.Fatalf("media metadata: %+v", ref)
		}
	}
	reopened, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := agent.New(reopened, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(reader, Options{ConfigPath: filepath.Join(t.TempDir(), "config.json"), InitialConfig: Config{Context: agent.DefaultContextConfig()}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	get := func(path string, want int) []byte {
		t.Helper()
		res, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("GET %s: %d %s", path, res.StatusCode, body)
		}
		return body
	}
	var detail agent.Snapshot
	if err := json.Unmarshal(get("/api/runs/"+snap.Run.ID, 200), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Actions) != 2 {
		t.Fatal("reopen lost source action")
	}
	for i, action := range detail.Actions {
		if action.RequestID != snap.Actions[i].RequestID || action.CallID != "shared-call" || action.Result.Artifacts[0] != snap.Actions[i].Result.Artifacts[0] {
			t.Fatalf("reopened source: %+v", action)
		}
	}
	downloaded := get("/api/artifacts/"+hash+"/download", 200)
	if !bytes.Equal(downloaded, data) || sha256.Sum256(downloaded) != digest {
		t.Fatal("download original bytes changed")
	}
	get("/api/artifacts/"+string(bytes.Repeat([]byte("0"), 64))+"/download", 404)
	get("/delivery.js", 200)
	if os.Getenv("TRACE_MEDIA_BROWSER") == "1" {
		fixture, _ := json.Marshal(map[string]any{"base": httpServer.URL, "run": snap.Run.ID, "hash": hash, "requests": []string{snap.Actions[0].RequestID, snap.Actions[1].RequestID}})
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", "media_store_browser.mjs")
		cmd.Env = append(os.Environ(), "TRACE_MEDIA_FIXTURE="+string(fixture), "CLOAKBROWSER_AUTO_UPDATE=false")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Store/CAS headed browser: %v %s", err, output)
		} else {
			t.Log(string(output))
		}
	}
}
