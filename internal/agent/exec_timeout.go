package agent

import (
	"encoding/json"
	"fmt"
)

const defaultExecTimeoutSeconds = 300
const maxExecTimeoutSeconds = 86400

// Nil in a saved Run retains the legacy parent-context-only boundary. New runs
// persist a finite default; changing application defaults never changes a resume.
type ExecConfig struct {
	TimeoutSeconds int `json:"timeout_seconds"`
}

func normalizeExecConfig(c *ExecConfig) (*ExecConfig, error) {
	if c == nil {
		return &ExecConfig{TimeoutSeconds: defaultExecTimeoutSeconds}, nil
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > maxExecTimeoutSeconds {
		return nil, fmt.Errorf("exec timeout_seconds must be 1..%d", maxExecTimeoutSeconds)
	}
	copy := *c
	return &copy, nil
}

type execArguments struct {
	Command        string `json:"command"`
	TimeoutSeconds *int   `json:"timeout_seconds,omitempty"`
}

func execTimeout(run Run, raw string) (int, error) {
	var args execArguments
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return 0, fmt.Errorf("invalid exec arguments: %w", err)
	}
	if args.TimeoutSeconds != nil {
		config, err := normalizeExecConfig(&ExecConfig{TimeoutSeconds: *args.TimeoutSeconds})
		if err != nil {
			return 0, err
		}
		return config.TimeoutSeconds, nil
	}
	if run.Exec == nil {
		return 0, nil
	}
	config, err := normalizeExecConfig(run.Exec)
	if err != nil {
		return 0, err
	}
	return config.TimeoutSeconds, nil
}

type execTimeoutKey struct{}
