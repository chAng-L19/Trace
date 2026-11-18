package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Settings-owned routing metadata only; endpoint URLs and credentials never
// enter the model context. This is discovery, not an automatic router.
func (a *Agent) stageModelContext() string {
	if len(a.roleModels.StageRoutes) == 0 && a.roleModels.Worker == nil && a.roleModels.Recon == nil {
		return ""
	}
	describe := func(selection *ModelSelection) map[string]any {
		resolved, provider, err := a.resolveModel(selection, false)
		if err != nil || resolved == nil {
			return map[string]any{"available": false}
		}
		out := map[string]any{"provider": resolved.Provider, "model": resolved.Model, "image_input": resolved.ImageInput, "reasoning_effort": resolved.ReasoningEffort}
		if p, ok := provider.(*OpenAIProvider); ok {
			caps := p.ModelCapabilities[resolved.Model]
			out["reasoning_efforts"] = caps.ReasoningEfforts
		}
		return out
	}
	routes := make([]map[string]any, 0, len(a.roleModels.StageRoutes))
	for _, route := range a.roleModels.StageRoutes {
		routes = append(routes, describe(&ModelSelection{Provider: route.Provider, Model: route.Model}))
	}
	recon := a.roleModels.Recon
	if recon == nil {
		recon = a.roleModels.Worker
	}
	data, _ := json.Marshal(map[string]any{"worker_default": describe(a.roleModels.Worker), "recon_default": describe(recon), "stage_routes": routes})
	return "\nStage model policy (data): execution_role=recon identifies information collection using recon_default (inherits worker_default when unset); empty/worker uses worker_default. Optional model_selection must use an exact allowed provider/model pair. Set reasoning_effort only from that route's declared values; changing routes never inherits another route's effort. Execution role and effective model freeze at first claim; saved bindings survive later default/allowlist changes. Never replay uncertain effects.\n" + string(data)
}

// Stage requests are complete routes, never patches over another route's effort.
// This preflight performs no network or Store calls and is safe inside claim.
func (a *Agent) stageModelSelection(task PlanTask) (*ModelSelection, error) {
	if task.ExecutionRole != "" && task.ExecutionRole != "worker" && task.ExecutionRole != "recon" {
		return nil, fmt.Errorf("task %s execution_role must be worker or recon", task.ID)
	}
	selection := a.roleModels.Worker
	if task.ExecutionRole == "recon" && a.roleModels.Recon != nil {
		selection = a.roleModels.Recon
	}
	if requested := task.ModelSelection; requested != nil && task.ModelBinding == nil {
		if requested.Provider == "" || requested.Model == "" || strings.TrimSpace(requested.Provider) != requested.Provider || strings.TrimSpace(requested.Model) != requested.Model {
			return nil, fmt.Errorf("task %s model_selection requires exact provider and model", task.ID)
		}
		if requested.ProviderID != "" || requested.ImageInput {
			return nil, fmt.Errorf("task %s model_selection identity and image capability are server-owned; use required_image_input", task.ID)
		}
		allowed := false
		for _, route := range a.roleModels.StageRoutes {
			allowed = allowed || route.Provider == requested.Provider && route.Model == requested.Model
		}
		if !allowed {
			return nil, fmt.Errorf("task %s model route is disabled or not in role_models.stage_routes; allowed routes: %v", task.ID, a.roleModels.StageRoutes)
		}
		selection = requested
	}
	restoring := task.ModelBinding != nil
	if restoring {
		selection = task.ModelBinding
	}
	resolved, provider, err := a.resolveModel(selection, restoring)
	if err != nil {
		return nil, fmt.Errorf("task %s model preflight: %w; allowed routes: %v", task.ID, err, a.roleModels.StageRoutes)
	}
	if task.RequiredImageInput {
		capable, ok := provider.(interface{ NativeImageInput() bool })
		if !ok || !capable.NativeImageInput() {
			return nil, fmt.Errorf("task %s requires native image input; selected model does not declare it; allowed routes: %v", task.ID, a.roleModels.StageRoutes)
		}
	}
	return resolved, nil
}

func sameStageModelContract(a, b PlanTask) bool {
	if a.RequiredImageInput != b.RequiredImageInput || a.ExecutionRole != b.ExecutionRole {
		return false
	}
	if a.ModelSelection == nil || b.ModelSelection == nil {
		return a.ModelSelection == nil && b.ModelSelection == nil
	}
	return *a.ModelSelection == *b.ModelSelection
}

// Explicit routing must not silently downgrade image inputs to workspace files.
// Legacy/default tasks retain their existing file-delivery behavior.
func (a *Agent) preflightStageModel(parent *Snapshot, task PlanTask) error {
	inputs, err := taskInputAttachments(parent, task.InputAttachmentIDs)
	if err != nil {
		return err
	}
	if task.ModelSelection != nil {
		for _, input := range inputs {
			if nativeImageMIME(input.MIMEType) {
				task.RequiredImageInput = true
			}
		}
	}
	_, err = a.stageModelSelection(task)
	return err
}
