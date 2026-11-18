package agent

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// ModelSelection pins a Run to a credential-free provider endpoint and model.
type ModelSelection struct {
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ProviderID      string `json:"provider_id,omitempty"`
	ImageInput      bool   `json:"image_input,omitempty"`
}

type RoleModels struct {
	Orchestrator *ModelSelection `json:"orchestrator,omitempty"`
	Worker       *ModelSelection `json:"worker,omitempty"`
	Recon        *ModelSelection `json:"recon,omitempty"`
	StageRoutes  []ModelRoute    `json:"stage_routes,omitempty"`
}

// ModelRoute is settings-owned; an empty list disables task-selected routes.
type ModelRoute struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type ModelCapabilities struct {
	ImageInput       bool     `json:"image_input,omitempty"`
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
}

func copyModelProvider(p *OpenAIProvider) *OpenAIProvider {
	copy := *p
	copy.ReasoningEfforts = append([]string(nil), p.ReasoningEfforts...)
	copy.ModelCapabilities = make(map[string]ModelCapabilities, len(p.ModelCapabilities)+1)
	for model, caps := range p.ModelCapabilities {
		caps.ReasoningEfforts = append([]string(nil), caps.ReasoningEfforts...)
		copy.ModelCapabilities[model] = caps
	}
	if _, ok := copy.ModelCapabilities[p.Model]; !ok {
		copy.ModelCapabilities[p.Model] = ModelCapabilities{ImageInput: p.ImageInput, ReasoningEfforts: copy.ReasoningEfforts}
	}
	return &copy
}

func providerIdentity(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", fmt.Errorf("provider URL must be an absolute HTTP(S) URL with a host")
	}
	if strings.ContainsAny(u.Host, "[]") || strings.Contains(u.Hostname(), ":") {
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil || !ip.Is6() || !strings.HasPrefix(u.Host, "[") {
			return "", fmt.Errorf("provider URL has an invalid host")
		}
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return "", fmt.Errorf("provider URL has an invalid port")
		}
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return promptHash(strings.TrimSuffix(u.String(), "/chat/completions")), nil
}

// ConfigureModels runs before Agent publication. Provider/default copies prevent
// later caller mutations and concurrent role selection from changing a route.
func (a *Agent) ConfigureModels(providers map[string]*OpenAIProvider, roles RoleModels) error {
	candidate := *a
	candidate.modelProviders = make(map[string]*OpenAIProvider, len(providers)+1)
	if p, ok := a.provider.(*OpenAIProvider); ok {
		if p == nil {
			return fmt.Errorf("invalid default provider")
		}
		if _, err := providerIdentity(p.BaseURL); err != nil {
			return fmt.Errorf("invalid default provider: %w", err)
		}
		candidate.modelProviders["default"] = copyModelProvider(p)
	}
	for name, p := range providers {
		if strings.TrimSpace(name) != name || name == "" || name == "default" || p == nil || p.BaseURL == "" || p.Model == "" {
			return fmt.Errorf("invalid named provider %q", name)
		}
		if _, err := providerIdentity(p.BaseURL); err != nil {
			return fmt.Errorf("invalid named provider %q: %w", name, err)
		}
		candidate.modelProviders[name] = copyModelProvider(p)
	}
	candidate.roleModels = RoleModels{}
	for _, item := range []struct {
		source *ModelSelection
		target **ModelSelection
	}{{roles.Orchestrator, &candidate.roleModels.Orchestrator}, {roles.Worker, &candidate.roleModels.Worker}, {roles.Recon, &candidate.roleModels.Recon}} {
		if item.source == nil {
			continue
		}
		selected, _, err := candidate.resolveModel(item.source, false)
		if err != nil {
			return err
		}
		*item.target = selected
	}
	seen := map[ModelRoute]bool{}
	for _, route := range roles.StageRoutes {
		if strings.TrimSpace(route.Provider) != route.Provider || route.Provider == "" || strings.TrimSpace(route.Model) != route.Model || route.Model == "" || seen[route] {
			return fmt.Errorf("invalid or duplicate stage model route")
		}
		if _, _, err := candidate.resolveModel(&ModelSelection{Provider: route.Provider, Model: route.Model}, false); err != nil {
			return err
		}
		seen[route] = true
		candidate.roleModels.StageRoutes = append(candidate.roleModels.StageRoutes, route)
	}
	a.modelProviders, a.roleModels = candidate.modelProviders, candidate.roleModels
	return nil
}

// WithModelConfiguration returns a new immutable routing snapshot while
// retaining the same Store, tools and worker/MCP lifetimes. Active executions
// keep the Agent they captured. Neither copy owns a separate MCP connection,
// so discarding or replacing a model-only copy must never call CloseMCP.
func (a *Agent) WithModelConfiguration(provider Provider, providers map[string]*OpenAIProvider, roles RoleModels) (*Agent, error) {
	if provider == nil {
		provider = MockProvider{}
	}
	if p, ok := provider.(*OpenAIProvider); ok && p != nil {
		provider = copyModelProvider(p)
	}
	candidate := *a
	candidate.provider = provider
	if err := candidate.ConfigureModels(providers, roles); err != nil {
		return nil, err
	}
	return &candidate, nil
}

func (a *Agent) resolveModel(selection *ModelSelection, restoring bool) (*ModelSelection, Provider, error) {
	selected := ModelSelection{Provider: "default"}
	if selection != nil {
		selected = *selection
	}
	if selected.Provider == "" {
		selected.Provider = "default"
	}
	p := a.modelProviders[selected.Provider]
	if p == nil && selected.Provider == "default" {
		p, _ = a.provider.(*OpenAIProvider)
	}
	if p == nil {
		if selection == nil {
			return nil, a.provider, nil
		}
		return nil, nil, fmt.Errorf("model provider %q is unavailable", selected.Provider)
	}
	copy := *p
	if selected.Model == "" {
		if selection != nil {
			return nil, nil, fmt.Errorf("model is required")
		}
		selected.Model = p.Model
	}
	identity, err := providerIdentity(p.BaseURL)
	if err != nil {
		return nil, nil, err
	}
	caps, declared := p.ModelCapabilities[selected.Model]
	if !declared && selected.Model == p.Model {
		caps = ModelCapabilities{ImageInput: p.ImageInput, ReasoningEfforts: p.ReasoningEfforts}
		declared = true
	}
	if !declared && p.ImageInput && !restoring {
		return nil, nil, fmt.Errorf("image capability is not declared for model %q", selected.Model)
	}
	if restoring && (selected.ProviderID == "" || selected.ProviderID != identity || selected.ImageInput != caps.ImageInput) {
		return nil, nil, fmt.Errorf("saved model provider identity or image capability changed")
	}
	if selected.ReasoningEffort != "" && !hasString(caps.ReasoningEfforts, selected.ReasoningEffort) {
		return nil, nil, fmt.Errorf("provider %q model %q does not support reasoning effort %q", selected.Provider, selected.Model, selected.ReasoningEffort)
	}
	selected.ProviderID, selected.ImageInput = identity, caps.ImageInput
	copy.ImageInput = caps.ImageInput
	copy.Model, copy.ReasoningEffort = selected.Model, selected.ReasoningEffort
	return &selected, &copy, nil
}

func (a *Agent) withRunModel(run Run) (*Agent, error) {
	if run.ModelSelection == nil {
		return a, nil
	} // Legacy runs retain legacy behavior.
	_, provider, err := a.resolveModel(run.ModelSelection, true)
	if err != nil {
		return nil, err
	}
	copy := *a
	copy.provider = provider
	return &copy, nil
}
