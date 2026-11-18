package web

import "trace/internal/agent"

// This is Web goroutine ownership at read time, not persisted Run state or
// permission to execute. The original Run lock remains the admission authority.
type executionRunView struct {
	agent.Run
	ExecutionActive *bool `json:"execution_active,omitempty"`
}

func (s *Server) executionRun(run agent.Run) executionRunView {
	s.mu.Lock()
	active := s.active[run.ID]
	s.mu.Unlock()
	view := executionRunView{Run: run}
	// Worker lifetimes belong to Agent rather than this Web map. Absence from
	// this map cannot establish that a child is idle.
	if active || run.ParentTaskID == "" {
		view.ExecutionActive = &active
	}
	return view
}

func (s *Server) executionWorkspace(value any) any {
	view := value.(workspaceView)
	return struct {
		workspaceView
		Run executionRunView `json:"run"`
	}{view, s.executionRun(view.Snapshot.Run)}
}
