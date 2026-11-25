// Package web exposes the existing Agent and Store to a local browser.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"trace/internal/agent"
	"trace/internal/mcp"
)

//go:embed static/*.js static/style-unified.css static/polish.css static/provider-settings.css static/index.html static/mark.svg
var static embed.FS

type Options struct {
	ConfigPath    string
	InitialConfig Config
	BuildAgent    func(context.Context, Config) (*agent.Agent, error)
	// Host is the listening address. Browser Host validation prevents DNS rebinding.
	Host string
}

type Server struct {
	mu               sync.Mutex
	a                *agent.Agent
	store            *agent.Store // Shared across configuration rebuilds; reads never race agent replacement.
	options          Options
	config           Config
	active           map[string]bool // In-process goroutine ownership only; Store remains authoritative.
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	configuring      bool
	closed           bool
	closeOnce        sync.Once
	closeErr         error
	mcpAttempts      map[string]mcp.Status
	mcpProbeSequence map[string]uint64
	diagnostics      diagnosticCounters
}

func New(a *agent.Agent, options Options) (*Server, error) {
	if a == nil {
		return nil, errors.New("agent is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Invalidate pre-restart editors, including after out-of-process config edits.
	revision, err := newConfigRevision()
	if err != nil {
		cancel()
		return nil, err
	}
	options.InitialConfig.Revision = revision
	return &Server{a: a, store: a.Store(), options: options, config: options.InitialConfig, active: map[string]bool{}, ctx: ctx, cancel: cancel}, nil
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		a := s.a
		for id := range s.active {
			_, _ = a.Pause(id)
		}
		s.cancel()
		s.mu.Unlock()
		s.wg.Wait()
		workerErr := a.StopWorkers(context.Background())
		s.closeErr = errors.Join(workerErr, a.CloseMCP())
	})
	return s.closeErr
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("POST /api/runs", s.createRun)
	mux.HandleFunc("POST /api/inputs", s.uploadInput)
	mux.HandleFunc("GET /api/inputs/{input}", s.downloadInput)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("GET /api/runs/{id}/events/history", s.eventHistory)
	mux.HandleFunc("GET /api/runs/{id}/requests/{request}", s.getModelRequest)
	mux.HandleFunc("PUT /api/runs/{id}/budget", s.putRunBudget)
	mux.HandleFunc("PUT /api/runs/{id}/tree-budget", s.putTreeBudget)
	mux.HandleFunc("DELETE /api/runs/{id}", s.deleteRun)
	mux.HandleFunc("POST /api/runs/{id}/tool-observations", s.recordToolObservation)
	mux.HandleFunc("GET /api/evidence", s.listEvidence)
	mux.HandleFunc("GET /api/evidence-chains", s.listEvidenceChains)
	mux.HandleFunc("GET /api/evidence-chains/{run}/{chain}", s.getEvidenceChain)
	mux.HandleFunc("DELETE /api/evidence-chains/{run}/{chain}", s.deleteEvidenceChain)
	mux.HandleFunc("POST /api/runs/{id}/evidence-chains", s.createEvidenceChain)
	mux.HandleFunc("POST /api/runs/{id}/evidence-chain-references", s.referenceEvidenceChain)
	mux.HandleFunc("DELETE /api/runs/{id}/evidence-chain-references", s.referenceEvidenceChain)
	mux.HandleFunc("GET /api/evidence/{run}/{evidence}", s.getEvidence)
	mux.HandleFunc("DELETE /api/evidence/{run}/{evidence}", s.deleteEvidence)
	mux.HandleFunc("POST /api/runs/{id}/{op}", s.controlRun)
	mux.HandleFunc("GET /api/runs/{id}/events", s.events)
	mux.HandleFunc("GET /api/runs/{id}/export", s.exportRun)
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.putConfig)
	mux.HandleFunc("POST /api/provider/probe", s.probeProvider)
	mux.HandleFunc("POST /api/search-sources/{source}/probe", s.probeSearchSource)
	mux.HandleFunc("GET /api/mcp/status", s.getMCPStatus)
	mux.HandleFunc("POST /api/mcp/probe", s.probeMCP)
	mux.HandleFunc("GET /api/tools", s.getTools)
	mux.HandleFunc("PUT /api/runs/{id}/tools", s.selectTools)
	mux.HandleFunc("GET /api/skills", s.getSkills)
	mux.HandleFunc("GET /api/skills/{name}", s.getSkill)
	mux.HandleFunc("PUT /api/skills/{name}/availability", s.putSkillAvailability)
	mux.HandleFunc("GET /api/runs/{id}/usage", s.getUsageReport)
	mux.HandleFunc("GET /api/runs/{id}/diagnostics", s.getRunDiagnostics)
	mux.HandleFunc("GET /api/diagnostics", s.getDiagnostics)
	s.registerPlannerRoutes(mux)
	s.registerVerificationRoutes(mux)
	s.registerIntegrationRoutes(mux)
	s.registerSessionRoutes(mux)
	s.registerMessageDeliveryRoutes(mux)
	mux.Handle("/", staticHandler())
	return s.diagnosticHandler(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; frame-src blob:; object-src 'none'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if !s.allowedHost(r.Host) {
			writeError(w, http.StatusForbidden, errors.New("host does not match local listener"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				writeError(w, http.StatusForbidden, errors.New("cross-origin request rejected"))
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, http.StatusForbidden, errors.New("cross-site request rejected"))
			return
		}
		inputUpload := r.Method == "POST" && r.URL.Path == "/api/inputs" && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;")
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Content-Type") != "application/json" && !inputUpload {
			writeError(w, http.StatusUnsupportedMediaType, errors.New("use application/json"))
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

func (s *Server) allowedHost(host string) bool {
	if s.options.Host == "" {
		return true
	} // httptest / an embedding application's own host policy.
	want, port, err := net.SplitHostPort(s.options.Host)
	if err != nil {
		return host == s.options.Host
	}
	got, gotPort, err := net.SplitHostPort(host)
	if err != nil || gotPort != port {
		return false
	}
	if want == "127.0.0.1" || want == "localhost" || want == "::1" {
		return got == "127.0.0.1" || got == "localhost" || got == "::1"
	}
	return got == want
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, err error) {
	body := map[string]any{"error": err.Error()}
	var blocked *agent.TaskDispositionError
	if errors.As(err, &blocked) {
		body["diagnostic"] = agent.ToolDiagnostic{Code: "task_disposition_blocked", Summary: blocked.Reason, NextAction: blocked.NextAction, Blockers: blocked}
	}
	writeJSON(w, status, body)
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		writeError(w, 400, err)
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeError(w, 400, errors.New("request must contain one JSON value"))
		return false
	}
	return true
}
func respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		status := 409
		if errors.Is(err, os.ErrNotExist) {
			status = 404
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, 200, value)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	var runs []agent.Run
	var err error
	if r.URL.Query().Get("include_deleted") == "true" {
		runs, err = s.store.ListWithDeleted()
	} else {
		runs, err = s.store.List()
	}
	if runs == nil {
		runs = []agent.Run{}
	}
	if r.URL.Query().Get("include_archived") != "true" {
		visible := make([]agent.Run, 0, len(runs))
		for _, run := range runs {
			if !run.Archived {
				visible = append(visible, run)
			}
		}
		runs = visible
	}
	views := make([]executionRunView, 0, len(runs))
	for _, run := range runs {
		views = append(views, s.executionRun(run))
	}
	respond(w, views, err)
}
func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("view") == "workspace" {
		if r.URL.Query().Get("tab") == "planner" {
			projection, err := s.store.PlannerWorkspace(r.Context(), r.PathValue("id"))
			if err != nil {
				respond(w, nil, err)
				return
			}
			view, err := projectWorkspacePage(projection.Snapshot, agent.MessagePageQuery{}, true)
			if err == nil {
				p := view.(workspaceView)
				p.Projection, p.Totals = "planner", projection.Totals
				p.HelpRequests = projection.HelpRequests
				view = s.executionWorkspace(p)
			}
			respond(w, view, err)
			return
		}
		if r.URL.Query().Get("activity") == "true" || r.URL.Query().Get("tab") == "events" {
			snap, err := s.store.ActivityWorkspaceContext(r.Context(), r.PathValue("id"))
			if err != nil {
				respond(w, nil, err)
				return
			}
			view, err := s.activityWorkspaceContext(r.Context(), snap, r.URL.Query().Get("workers") != "false" && r.URL.Query().Get("tab") != "events")
			if err == nil {
				view = s.executionWorkspace(view)
			}
			respond(w, view, err)
			return
		}
		snap, err := s.store.Workspace(r.PathValue("id"))
		if err != nil {
			respond(w, nil, err)
			return
		}
		snap.ChildReports, err = s.store.ChildReportsFrom(snap, 0)
		if err != nil {
			respond(w, nil, err)
			return
		}
		respond(w, s.executionWorkspace(workspaceSnapshot(snap, s.store.WorkerActivities(snap))), nil)
		return
	}
	snap, err := s.store.Load(r.PathValue("id"))
	if err == nil && snap.Run.Deleted {
		err = os.ErrNotExist
	}
	respond(w, struct {
		agent.Snapshot
		Run      executionRunView      `json:"run"`
		Coverage agent.CoverageSummary `json:"coverage"`
	}{snap, s.executionRun(snap.Run), agent.Coverage(snap)}, err)
}
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CompletionCriteria string                          `json:"completion_criteria,omitempty"`
		RootContract       *agent.RootContract             `json:"root_contract,omitempty"`
		Exec               *agent.ExecConfig               `json:"exec,omitempty"`
		ToolSchema         agent.ToolSchemaConfig          `json:"tool_schema,omitempty"`
		ModelSelection     *agent.ModelSelection           `json:"model_selection,omitempty"`
		Requirements       []agent.VerificationRequirement `json:"requirements"`
		Attachments        []string                        `json:"attachments,omitempty"`
		Goal               string                          `json:"goal"`
		Start              bool                            `json:"start"`
		SuccessContains    *string                         `json:"success_contains,omitempty"`
		Budget             *agent.Budget                   `json:"budget,omitempty"`
		TreeBudget         *agent.TreeBudget               `json:"tree_budget,omitempty"`
		Title              string                          `json:"title,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		writeError(w, 503, errors.New("server is stopping"))
		return
	}
	if s.configuring {
		writeError(w, 409, errors.New("integration operation is in progress"))
		return
	}
	attachments, err := s.inputIDs(in.Attachments)
	if err != nil {
		respond(w, nil, err)
		return
	}
	snap, err := s.a.CreateConfigured(in.Goal, agent.RunConfiguration{CompletionCriteria: in.CompletionCriteria, Requirements: in.Requirements, SuccessContains: in.SuccessContains, Budget: in.Budget, TreeBudget: in.TreeBudget, Title: in.Title, ModelSelection: in.ModelSelection, Exec: in.Exec, ToolSchema: in.ToolSchema, RootContract: in.RootContract}, attachments...)
	if err == nil && in.Start {
		err = s.startLocked(snap.Run.ID, "", "")
	}
	if err != nil {
		respond(w, nil, err)
		return
	}
	writeJSON(w, 201, snap)
}

func (s *Server) startLocked(id, operation, content string, attachments ...agent.InputAttachment) error {
	if s.configuring {
		return errors.New("integration operation is in progress")
	}
	if s.closed {
		return errors.New("server is stopping")
	}
	if s.active[id] {
		return errors.New("run is already active")
	}
	snap, err := s.a.Store().Load(id)
	if err != nil {
		return err
	}
	if snap.Run.Deleted {
		return errors.New("会话已从档案删除")
	}
	if operation != "message" && (snap.Run.Status == agent.StatusCompleted || snap.Run.Status == agent.StatusCancelled) {
		return fmt.Errorf("run is %s", snap.Run.Status)
	}
	if operation == "message" {
		if strings.TrimSpace(content) == "" {
			return errors.New("message is required")
		}
		switch snap.Run.Status {
		case agent.StatusPaused:
			return errors.New("run is paused; queue a steer message and resume the run")
		case agent.StatusCancelled, agent.StatusRunning, agent.StatusWaitingTool:
			return fmt.Errorf("run is %s", snap.Run.Status)
		}
	}
	s.active[id] = true
	a := s.a
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var runErr error
		if operation == "message" {
			_, runErr = a.Message(s.ctx, id, content, attachments...)
		} else {
			_, runErr = a.Run(s.ctx, id)
		}
		if errors.Is(runErr, agent.ErrRunAdmission) {
			fmt.Fprintf(os.Stderr, "web run %s: %v\n", id, runErr)
		} else if runErr != nil {
			_, saveErr := s.store.Update(id, func(snap *agent.Snapshot) error {
				if snap.Run.Status != agent.StatusFailed {
					snap.Run.Error = runErr.Error()
				}
				snap.Run.EventSeq++
				snap.Events = append(snap.Events, agent.Event{Sequence: snap.Run.EventSeq, Type: "web_operation_error", Summary: "运行请求执行失败", Data: map[string]string{"operation": operation, "error": runErr.Error()}, CreatedAt: time.Now().UTC()})
				return nil
			})
			if saveErr != nil {
				fmt.Fprintf(os.Stderr, "web run %s: %v; persist error: %v\n", id, runErr, saveErr)
			}
		}
		s.mu.Lock()
		delete(s.active, id)
		s.mu.Unlock()
	}()
	return nil
}

func (s *Server) controlRun(w http.ResponseWriter, r *http.Request) {
	id, op := r.PathValue("id"), r.PathValue("op")
	var in struct {
		Content     string   `json:"content"`
		MessageID   string   `json:"message_id"`
		OperationID string   `json:"operation_id,omitempty"`
		Attachments []string `json:"attachments,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		writeError(w, 503, errors.New("server is stopping"))
		return
	}
	var value any
	var err error
	attachments, err := s.inputIDs(in.Attachments)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if in.OperationID != "" && (op == "message" || op == "steer" || op == "follow-up") {
		kind := op
		if kind == "follow-up" {
			kind = "follow_up"
		}
		_, receipt, created, queueErr := s.a.QueueMessageOperation(id, kind, in.OperationID, in.Content, attachments...)
		if queueErr != nil {
			respond(w, nil, queueErr)
			return
		}
		value := messageReceipt(id, receipt)
		if created && op == "message" {
			if startErr := s.startLocked(id, "", ""); startErr != nil {
				value["start_error"] = startErr.Error()
			}
		}
		respond(w, value, nil)
		return
	}
	switch op {
	case "run", "resume":
		err = s.startLocked(id, "", "")
		value = map[string]any{"accepted": err == nil, "id": id}
	case "message":
		err = s.startLocked(id, "message", in.Content, attachments...)
		value = map[string]any{"accepted": err == nil, "id": id}
	case "pause":
		var snap agent.Snapshot
		snap, err = s.a.Pause(id)
		value = map[string]any{"accepted": err == nil, "id": id, "status": snap.Run.Status, "pause_reason": snap.Run.PauseReason, "event_seq": snap.Run.EventSeq}
	case "cancel":
		var snap agent.Snapshot
		snap, err = s.a.Cancel(id)
		value = map[string]any{"accepted": err == nil, "id": id, "status": snap.Run.Status, "event_seq": snap.Run.EventSeq}
	case "steer":
		value, err = s.a.QueueMessage(id, "steer", in.Content, attachments...)
		if err == nil {
			value = map[string]any{"accepted": true, "id": id, "status": "queued"}
		}
	case "follow-up":
		value, err = s.a.QueueMessage(id, "follow_up", in.Content, attachments...)
		if err == nil {
			value = map[string]any{"accepted": true, "id": id, "status": "queued"}
		}
	case "branch":
		value, err = s.a.Branch(id, in.MessageID)
	default:
		writeError(w, 404, errors.New("unknown run operation"))
		return
	}
	respond(w, value, err)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	afterText := r.URL.Query().Get("after")
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		afterText = h
	}
	after := 0
	if afterText != "" {
		n, err := strconv.Atoi(afterText)
		if err != nil || n < 0 {
			writeError(w, 400, errors.New("invalid event cursor"))
			return
		}
		after = n
	}
	store := s.store
	release := store.HoldEventView(id)
	defer release()
	// Bind the committed generation before advertising the connection. An
	// indexed empty cursor needs no retained full-history prefix.
	changed := store.EventChanges(id)
	readStarted := time.Now()
	_, batch, err := store.EventBatch(id, after)
	s.diagnostics.stage("sse_event_batch", readStarted)
	if err != nil {
		respond(w, nil, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, errors.New("streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		writeStarted := time.Now()
		for _, raw := range batch {
			var event struct {
				Sequence int `json:"sequence"`
			}
			if json.Unmarshal(raw, &event) != nil {
				return
			}
			encoded, encodeErr := json.Marshal(raw)
			if encodeErr != nil {
				return
			}
			if _, err = fmt.Fprintf(w, "id: %d\nevent: trace\ndata: %s\n\n", event.Sequence, encoded); err != nil {
				return
			}
			after = event.Sequence
		}
		flusher.Flush()
		s.diagnostics.stage("sse_write_flush", writeStarted)
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-changed:
		case <-tick.C:
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
		// Subscribe before reading to avoid missing a commit between read and wait.
		changed = store.EventChanges(id)
		readStarted = time.Now()
		_, batch, err = store.EventBatch(id, after)
		s.diagnostics.stage("sse_event_batch", readStarted)
		if err != nil {
			return
		}
	}
}

func (s *Server) exportRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	format := r.URL.Query().Get("format")
	language := r.URL.Query().Get("language")
	if (format == "report_bundle" || format == "report") && language != "" && language != "zh-CN" && language != "en" {
		writeError(w, 400, errors.New("language must be zh-CN or en"))
		return
	}
	if format == "report_bundle" {
		if r.URL.Query().Get("source_hash") != "" {
			writeError(w, 400, errors.New("source_hash selects a legacy fixed report; use report_id for bundles"))
			return
		}
		filename, file, err := s.store.ReportBundle(id, r.URL.Query().Get("report_id"), language)
		if err != nil {
			respond(w, nil, err)
			return
		}
		defer func() {
			file.Close()
			os.Remove(file.Name())
		}()
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		http.ServeContent(w, r, filename, time.Time{}, file)
		return
	}
	if format == "report" {
		if r.URL.Query().Get("source_hash") != "" && (r.URL.Query().Get("report_id") != "" || r.URL.Query().Get("language") == "en") {
			writeError(w, 400, errors.New("source_hash selects a legacy fixed report; use report_id for bilingual files"))
			return
		}
		var body, filename string
		var err error
		if hash := r.URL.Query().Get("source_hash"); hash != "" {
			body, err = s.store.ReportVersion(id, hash)
			filename = "trace-" + id + ".md"
		} else {
			var ref agent.ArtifactRef
			ref, body, err = s.store.ReportDocument(id, r.URL.Query().Get("report_id"), r.URL.Query().Get("language"))
			filename = ref.Name
		}
		if err != nil {
			respond(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		fmt.Fprint(w, body)
		return
	}
	if format != "" && format != "json" && format != "evidence" {
		writeError(w, 400, errors.New("format must be report, report_bundle, json or evidence"))
		return
	}
	snap, err := s.store.Load(id)
	if err == nil && snap.Run.Deleted {
		err = os.ErrNotExist
	}
	if err != nil {
		respond(w, nil, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="trace-`+snap.Run.ID+`.json"`)
	if format == "evidence" {
		writeJSON(w, 200, snap.Evidence)
	} else {
		writeJSON(w, 200, snap)
	}
}

func (s *Server) getTools(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, s.a.Registry().Tools())
}
func (s *Server) getSkills(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, s.skillsWithSources())
}
func (s *Server) getSkill(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.a.ReadSkill(r.PathValue("name"))
	respond(w, map[string]string{"content": v}, err)
}

// ParseAddress requires an explicit loopback listener; remote deployments need an authentication layer.
func ParseAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return fmt.Errorf("serve requires a loopback address, got %q", address)
	}
	if strings.TrimSpace(address) == "" {
		return errors.New("listen address is required")
	}
	return nil
}
