package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"trace/internal/agent"
)

type providerFunc func(context.Context, agent.ModelRequest) (agent.ModelResponse, error)

func (p providerFunc) Complete(ctx context.Context, r agent.ModelRequest) (agent.ModelResponse, error) {
	return p(ctx, r)
}

func fixture(t *testing.T, p agent.Provider) (*Server, *agent.Agent) {
	t.Helper()
	store, err := agent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(store, p, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(a, Options{ConfigPath: filepath.Join(t.TempDir(), "config.json"), InitialConfig: Config{Context: agent.DefaultContextConfig()}, BuildAgent: func(_ context.Context, c Config) (*agent.Agent, error) {
		if c.Provider.Model == "invalid" {
			return nil, fmt.Errorf("model fixture rejected")
		}
		return agent.New(store, p, "")
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, a
}

func request(t *testing.T, h http.Handler, method, path string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	var b io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		b = bytes.NewReader(data)
	}
	r := httptest.NewRequest(method, path, b)
	if method != "GET" {
		r.Header.Set("Content-Type", "application/json")
	}
	if configWriteRequest(method, path) {
		current := httptest.NewRecorder()
		h.ServeHTTP(current, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		r.Header.Set("If-Match", current.Header().Get("ETag"))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	return w
}

func configWriteRequest(method, path string) bool {
	return method == http.MethodPut && (path == "/api/config" || strings.HasSuffix(path, "/availability")) ||
		method == http.MethodPost && (path == "/api/integrations/import" || path == "/api/integrations/update")
}

func TestEmbeddedModuleClosure(t *testing.T) {
	s, _ := fixture(t, nil)
	h := s.Handler()
	request(t, h, "GET", "/", nil, 200)
	imports := regexp.MustCompile(`(?:from\s+|import\s*)['"](\./[^'"]+)['"]`)
	seen := map[string]bool{}
	var check func(string)
	check = func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		w := request(t, h, "GET", path, nil, 200)
		if strings.Contains(w.Body.String(), "https://cdn") {
			t.Fatalf("external runtime dependency in %s", path)
		}
		for _, m := range imports.FindAllStringSubmatch(w.Body.String(), -1) {
			check("/" + strings.TrimPrefix(m[1], "./"))
		}
	}
	check("/app.js")
	if !seen["/views.js"] || !seen["/graphs.js"] {
		t.Fatal("view graph missing from module closure")
	}
	request(t, h, "GET", "/style-unified.css", nil, 200)
}

func TestArtifactByteRangesAndDownload(t *testing.T) {
	s, a := fixture(t, nil)
	data := append([]byte("中🙂"), 0, 255, 128, 10)
	ref, err := a.Store().PutArtifact("binary", data)
	if err != nil {
		t.Fatal(err)
	}
	var reconstructed []byte
	for offset := int64(0); offset < int64(len(data)); {
		w := request(t, s.Handler(), "GET", fmt.Sprintf("/api/artifacts/%s?offset=%d&limit=1", ref.Hash, offset), nil, 200)
		var part struct {
			Data string `json:"data_base64"`
			Next int64  `json:"next_offset"`
			EOF  bool   `json:"eof"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &part); err != nil {
			t.Fatal(err)
		}
		b, err := base64.StdEncoding.DecodeString(part.Data)
		if err != nil {
			t.Fatal(err)
		}
		if part.Next != offset+1 {
			t.Fatalf("byte cursor: %d -> %d", offset, part.Next)
		}
		reconstructed = append(reconstructed, b...)
		offset = part.Next
	}
	if !bytes.Equal(data, reconstructed) {
		t.Fatalf("artifact byte loss: %x != %x", data, reconstructed)
	}
	w := request(t, s.Handler(), "GET", "/api/artifacts/"+ref.Hash+"/download", nil, 200)
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("download changed bytes")
	}
	r := httptest.NewRequest("GET", "/api/artifacts/"+ref.Hash+"/download", nil)
	r.Header.Set("Range", "bytes=1-3")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), data[1:4]) {
		t.Fatalf("HTTP Range failed: %d %x", w.Code, w.Body.Bytes())
	}
	request(t, s.Handler(), "GET", "/api/artifacts/"+ref.Hash+"?offset=999", nil, 409)
}

func TestRunControlsAndSSECursor(t *testing.T) {
	s, a := fixture(t, nil)
	h := s.Handler()
	w := request(t, h, "POST", "/api/runs", map[string]any{"goal": "web run fixture", "start": false}, 201)
	var snap agent.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	request(t, h, "POST", "/api/runs/"+id+"/steer", map[string]string{"content": "first"}, 200)
	snap, err := a.Store().Load(id)
	if err != nil {
		t.Fatal(err)
	}
	cursor := snap.Run.EventSeq
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/runs/"+id+"/events?after=0", nil)
	r.Header.Set("Last-Event-ID", fmt.Sprint(cursor))
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	request(t, h, "POST", "/api/runs/"+id+"/follow-up", map[string]string{"content": "second"}, 200)
	scanner := bufio.NewScanner(response.Body)
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			if line != fmt.Sprintf("id: %d", cursor+1) {
				t.Fatalf("replayed cursor: %s", line)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no SSE event: %v", scanner.Err())
	}
	response.Body.Close()
	request(t, h, "POST", "/api/runs/"+id+"/run", map[string]any{}, 200)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, err = a.Store().Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Run.Status == agent.StatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap.Run.Status != agent.StatusCompleted {
		t.Fatalf("run did not finish: %s", snap.Run.Status)
	}
	request(t, h, "POST", "/api/runs/"+id+"/branch", map[string]string{"message_id": snap.Messages[len(snap.Messages)-1].ID}, 200)
	request(t, h, "GET", "/api/runs/"+id+"/export?format=report", nil, 200)
	request(t, h, "GET", "/api/runs/"+id+"/export?format=evidence", nil, 200)
	request(t, h, "GET", "/api/runs/"+id+"/coverage", nil, 200)
}

func TestConfigReplacementConcurrentReads(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("stable shared store")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				for _, path := range []string{"/api/runs", "/api/runs/" + snap.Run.ID, "/api/tools", "/api/skills", "/api/config"} {
					w := httptest.NewRecorder()
					h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
					if w.Code != 200 {
						t.Errorf("concurrent read %s: %d", path, w.Code)
					}
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		request(t, h, "PUT", "/api/config", Config{Provider: ProviderConfig{Model: fmt.Sprint(i), APIKey: "secret"}}, 200)
	}
	wg.Wait()
	w := request(t, h, "GET", "/api/config", nil, 200)
	if strings.Contains(w.Body.String(), `"secret"`) {
		t.Fatal("secret leaked")
	}
	request(t, h, "PUT", "/api/config", Config{Provider: ProviderConfig{URL: "invalid-url", Model: "invalid"}}, 409)
	w = request(t, h, "GET", "/api/config", nil, 200)
	if !strings.Contains(w.Body.String(), `"model":"9"`) {
		t.Fatal("failed build replaced configuration")
	}
}

func TestCloseWaitsForWorkerAndIsIdempotent(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := providerFunc(func(ctx context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
		close(entered)
		<-ctx.Done()
		<-release
		return agent.ModelResponse{}, ctx.Err()
	})
	s, a := fixture(t, p)
	snap, err := a.Create("parent")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.SetPlan(snap.Run.ID, []agent.PlanTask{{ID: "child", Goal: "child goal"}})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	request(t, h, "POST", "/api/runs/"+snap.Run.ID+"/tasks/child/dispatch", map[string]any{}, 200)
	request(t, h, "PUT", "/api/config", Config{}, 409)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("worker did not start")
	}
	closed := make(chan error, 2)
	go func() { closed <- s.Close() }()
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("Close returned before worker cleanup: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Close did not wait/finish")
		}
	}
	if a.HasActiveWorkers() {
		t.Fatal("worker remained active")
	}
	request(t, h, "POST", "/api/runs", map[string]any{"goal": "after close"}, 503)
	request(t, h, "POST", "/api/runs/"+snap.Run.ID+"/tasks/child/dispatch", map[string]any{}, 503)
}

func TestPauseCancelAndBusyConfig(t *testing.T) {
	entered := make(chan struct{}, 2)
	s, a := fixture(t, providerFunc(func(ctx context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return agent.ModelResponse{}, ctx.Err()
	}))
	snap, err := a.Create("pause resume cancel")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	path := "/api/runs/" + snap.Run.ID
	request(t, h, "POST", path+"/run", map[string]any{}, 200)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not start")
	}
	request(t, h, "PUT", "/api/config", Config{}, 409)
	request(t, h, "POST", path+"/run", map[string]any{}, 409)
	request(t, h, "POST", path+"/pause", map[string]any{}, 200)
	s.wg.Wait()
	request(t, h, "POST", path+"/resume", map[string]any{}, 200)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not resume")
	}
	request(t, h, "POST", path+"/cancel", map[string]any{}, 200)
	s.wg.Wait()
	snap, err = a.Store().Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != agent.StatusCancelled {
		t.Fatalf("cancelled status lost: %s", snap.Run.Status)
	}
}

func TestPlannerAssetsMessagesAndWait(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("planner fixture")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	path := "/api/runs/" + snap.Run.ID
	request(t, h, "PUT", path+"/plan", map[string]any{"tasks": []agent.PlanTask{{ID: "inspect", Goal: "inspect fixture"}, {ID: "verify", Goal: "verify fixture", Dependencies: []string{"inspect"}}}}, 200)
	w := request(t, h, "POST", path+"/tasks/inspect/dispatch", map[string]any{}, 200)
	var worker agent.AgentWorker
	if err = json.Unmarshal(w.Body.Bytes(), &worker); err != nil {
		t.Fatal(err)
	}
	request(t, h, "POST", path+"/workers/"+worker.ID+"/message", map[string]string{"content": "check evidence"}, 200)
	request(t, h, "POST", path+"/workers/wait", map[string]int{"seconds": 2}, 200)
	request(t, h, "POST", path+"/assets", agent.Asset{ID: "host", Kind: "host", Value: "local", Coverage: "discovered"}, 200)
	request(t, h, "POST", path+"/assets", agent.Asset{ID: "port", ParentID: "host", Kind: "port", Value: "80", Coverage: "discovered"}, 200)
	request(t, h, "POST", path+"/relations", agent.AssetRelation{From: "host", To: "port", Kind: "contains"}, 200)
	request(t, h, "POST", path+"/assets", agent.Asset{ID: "false-proof", Kind: "host", Value: "unproven", Coverage: "verified"}, 409)
	w = request(t, h, "GET", path, nil, 200)
	var value struct {
		agent.Snapshot
		Coverage agent.CoverageSummary `json:"coverage"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Coverage.Total != 2 || value.Coverage.Uncovered != 2 || value.Coverage.Verified != 0 {
		t.Fatalf("false coverage: %+v", value.Coverage)
	}
	if value.Coverage.ByParent["host"].Total != 2 {
		t.Fatal("hierarchy projection missing")
	}
	if len(value.Plan.Frontier) != 1 || value.Plan.Frontier[0] != "verify" {
		t.Fatalf("frontier did not progress: %+v", value.Plan)
	}
	if len(value.WorkerMessages) < 1 {
		t.Fatal("worker message not persisted")
	}
}
