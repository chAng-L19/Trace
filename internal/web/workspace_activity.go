package web

import (
	"context"
	"trace/internal/agent"
)

// Only activity is a read-only projection. Verification/report/graph routes
// still use canonical source validation; no cached verified badge leaks here.
func (s *Server) activityWorkspace(projection agent.ActivityProjection, includeWorkers bool) (any, error) {
	return s.activityWorkspaceContext(context.Background(), projection, includeWorkers)
}

func (s *Server) activityWorkspaceContext(ctx context.Context, projection agent.ActivityProjection, includeWorkers bool) (any, error) {
	snap := projection.Snapshot
	// The parent activity already carries Worker lifecycle and help summaries.
	// Reading child histories is only needed for the expanded detail panel.
	var activity []agent.WorkerActivity
	if includeWorkers {
		activity = s.store.ActivityWorkerActivitiesContext(ctx, projection)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projected, err := projectWorkspacePage(snap, agent.MessagePageQuery{Limit: projection.MessagesPage.Limit}, true, activity)
	if err != nil {
		return nil, err
	}
	view := projected.(workspaceView)
	for i := range view.Messages {
		if original, ok := projection.MessagePreviews[view.Messages[i].ID]; ok {
			view.Messages[i].Preview = view.Messages[i].Preview || original.Preview
			view.Messages[i].ContentBytes = original.ContentBytes
			view.Messages[i].ThinkingBytes = original.ThinkingBytes
		}
	}
	// projectWorkspacePage detached the request slice. Context accounting is
	// read through the existing full-request detail route, not the activity UI.
	for i := range view.Requests {
		view.Requests[i].ContextManifest = nil
	}
	view.Totals = projection.Totals
	view.MessagesPage = projection.MessagesPage
	view.Projection = "activity"
	view.HelpRequests = projection.HelpRequests
	view.Evidence = nil
	view.EvidenceChains = nil
	view.EvidenceChainRefs = nil
	view.Assets = nil
	view.Relations = nil
	view.Checks = nil
	view.Findings = nil
	view.Compactions = nil
	view.WorkerContext = nil
	view.WorkerCheckpoint = nil
	view.WorkerSettlement = nil
	if len(view.WorkerMessages) > 40 {
		view.WorkerMessages = view.WorkerMessages[len(view.WorkerMessages)-40:]
	}
	for i := range view.WorkerMessages {
		view.WorkerMessages[i].Content = workspaceText(view.WorkerMessages[i].Content, 2048)
	}
	if len(view.HelpRequests) > 20 {
		view.HelpRequests = view.HelpRequests[len(view.HelpRequests)-20:]
	}
	return view, nil
}
