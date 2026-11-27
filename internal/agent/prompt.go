package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"trace/prompts"
)

var DefaultPrompt = prompts.Agent

type runtimePrompts struct {
	coordinator string
	worker      string
	recon       string
}

// UsePromptFileForNewRuns defers reading configured templates until creation.
// Existing Runs keep their saved prompt even if the files have moved.
func (a *Agent) UsePromptFileForNewRuns(path string) {
	a.newRunPromptPath = path
}

func loadRuntimePrompts(path string) (runtimePrompts, error) {
	if path == "" {
		worker := prompts.Common + "\n" + prompts.Worker
		return runtimePrompts{DefaultPrompt, worker, worker + "\n" + prompts.Recon}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return runtimePrompts{}, fmt.Errorf("load runtime prompt: %w", err)
	}
	common := string(data)
	if strings.TrimSpace(common) == "" {
		return runtimePrompts{}, fmt.Errorf("runtime prompt is empty")
	}
	// Existing custom single-file prompts keep their exact bytes for every role.
	if !strings.HasPrefix(strings.TrimSpace(common), prompts.RoleMarker) {
		return runtimePrompts{common, common, common}, nil
	}
	var roles runtimePrompts
	for _, role := range []struct {
		name string
		text *string
	}{{"coordinator", &roles.coordinator}, {"worker", &roles.worker}, {"recon", &roles.recon}} {
		rolePath := filepath.Join(filepath.Dir(path), role.name+".md")
		text, err := os.ReadFile(rolePath)
		if err != nil {
			return runtimePrompts{}, fmt.Errorf("load %s runtime prompt: %w", role.name, err)
		}
		if strings.TrimSpace(string(text)) == "" {
			return runtimePrompts{}, fmt.Errorf("%s runtime prompt is empty", role.name)
		}
		*role.text = string(text)
	}
	roles.coordinator = common + "\n" + roles.coordinator
	roles.worker = common + "\n" + roles.worker
	roles.recon = roles.worker + "\n" + roles.recon
	return roles, nil
}
