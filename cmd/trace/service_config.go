package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"trace/internal/agent"
	"trace/internal/localtool"
	"trace/internal/web"
)

func serviceConfigPath() string {
	if path := os.Getenv("TRACE_CONFIG_FILE"); path != "" {
		return path
	}
	root := os.Getenv("TRACE_DATA_DIR")
	if root == "" {
		root = "state"
	}
	return filepath.Join(root, "service", "web-config.json")
}

// Saved service configuration is authoritative for both entrypoints. Environment
// settings bootstrap a missing file only; malformed bootstrap values cannot
// invalidate a saved configuration. Budget/success overrides remain per-run.
// Reference: Muteki 144df766 apps/web/worker_config.py resolve_worker_backend;
// pi 8a7b0c03 core/settings-manager.ts centralizes loading and precedence.
func loadServiceConfig(path, dataDir string) (web.ConfigLoadResult, error) {
	result, err := web.LoadConfig(path, web.Config{}, dataDir)
	if err != nil {
		return result, err
	}
	if result.IsFirstBoot {
		result.Config, err = initialWebConfig()
		if err != nil {
			return result, err
		}
	}
	// Keep explicitly saved entries, including disabled entries and arguments.
	presets, _ := localtool.Presets(context.Background())
	mergeWebPresets(&result.Config, presets)
	return result, nil
}

func cliServiceConfig() (web.ConfigLoadResult, error) {
	dataDir := os.Getenv("TRACE_DATA_DIR")
	if dataDir == "" {
		dataDir = "state"
	}
	return loadServiceConfig(serviceConfigPath(), dataDir)
}

func cliContextOverride(result web.ConfigLoadResult) bool {
	if !result.IsFirstBoot {
		return true
	}
	for _, name := range []string{"TRACE_CONTEXT_TOKENS", "TRACE_MODEL_CONTEXT_TOKENS", "TRACE_OUTPUT_RESERVE", "TRACE_KEEP_RECENT_TOKENS", "TRACE_COMPACTION_MODE"} {
		if _, ok := os.LookupEnv(name); ok {
			return true
		}
	}
	return false
}

// Existing runs retain their persisted prompt even when the configured prompt
// file has since moved. New runs and the prompt command load the shared path.
func configuredRuntimeAgent(ctx context.Context, store *agent.Store) (*agent.Agent, error) {
	result, err := cliServiceConfig()
	if err != nil {
		return nil, err
	}
	result.Config.PromptPath = ""
	return buildConfiguredAgent(ctx, store, result.Config, true, cliContextOverride(result))
}

// Discovery uses the shared saved connection settings and registry, without
// interpreting run budgets or loading a prompt that directory listing never uses.
func configuredDirectoryAgent(ctx context.Context, store *agent.Store) (*agent.Agent, error) {
	result, err := cliServiceConfig()
	if err != nil {
		return nil, err
	}
	runner, err := agent.New(store, agent.MockProvider{}, "")
	if err != nil {
		return nil, err
	}
	if err := registerConfiguredTools(ctx, runner, result.Config); err != nil {
		return nil, errors.Join(err, runner.CloseMCP())
	}
	return runner, nil
}
