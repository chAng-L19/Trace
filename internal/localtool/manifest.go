// Package localtool describes one generic local command adapter.
//
// Trace deliberately treats a command as an argv/stdin endpoint.  A manifest
// can therefore wrap any local CLI without a product-specific adapter. argv
// mode maps named arguments into whole argv tokens; stdio mode receives the
// same JSON object as one JSONL line on stdin.
package localtool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"trace/internal/worker"
)

type Mode string

const (
	ModeArgv  Mode = "argv"
	ModeStdio Mode = "stdio"
)

// Manifest is operator-owned configuration. It is never copied into a run
// snapshot; only the model-facing definition and action invocation are saved.
type Manifest struct {
	Name               string            `json:"name"`
	Disabled           bool              `json:"disabled,omitempty"`
	Description        string            `json:"description,omitempty"`
	GitHubURL          string            `json:"github_url,omitempty"`
	LocalPath          string            `json:"local_path,omitempty"`
	InputSchema        json.RawMessage   `json:"input_schema,omitempty"`
	Command            string            `json:"command"`
	Args               []string          `json:"args,omitempty"`
	Mode               Mode              `json:"mode,omitempty"`
	Dir                string            `json:"dir,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	TimeoutSeconds     int               `json:"timeout_seconds,omitempty"`
	CancelGraceSeconds int               `json:"cancel_grace_seconds,omitempty"`
}

type Definition struct {
	ID          string
	Name        string
	Description string
	InputSchema json.RawMessage
}

func (m Manifest) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("local tool name is required")
	}
	if strings.TrimSpace(m.Command) == "" {
		return fmt.Errorf("local tool %s command is required", m.Name)
	}
	if m.Mode == "" {
		m.Mode = ModeArgv
	}
	if m.Mode != ModeArgv && m.Mode != ModeStdio {
		return fmt.Errorf("local tool %s mode must be argv or stdio", m.Name)
	}
	if m.TimeoutSeconds < 0 || int64(m.TimeoutSeconds) > int64((1<<63-1)/time.Second) {
		return fmt.Errorf("local tool %s timeout_seconds is outside the supported duration range", m.Name)
	}
	if m.CancelGraceSeconds < 0 || int64(m.CancelGraceSeconds) > int64((1<<63-1)/time.Second) {
		return fmt.Errorf("local tool %s cancel_grace_seconds is invalid", m.Name)
	}
	if len(m.InputSchema) != 0 {
		var schema map[string]any
		if json.Unmarshal(m.InputSchema, &schema) != nil || schema == nil {
			return fmt.Errorf("local tool %s input_schema must be a JSON object", m.Name)
		}
	}
	return nil
}

func (m Manifest) StableID() string {
	hash := sha256.Sum256([]byte(m.Name))
	return "local_" + hex.EncodeToString(hash[:28])
}

func (m Manifest) Definition() Definition {
	schema := m.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return Definition{ID: m.StableID(), Name: m.Name, Description: m.Description, InputSchema: append(json.RawMessage(nil), schema...)}
}

// Invocation returns the exact argv and stdin for an action. Replacements
// occupy whole tokens; values never undergo shell parsing or word splitting.
func (m Manifest) Invocation(arguments json.RawMessage) ([]string, string, error) {
	if err := m.Validate(); err != nil {
		return nil, "", err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(arguments, &object) != nil || object == nil {
		return nil, "", fmt.Errorf("local tool arguments must be a JSON object")
	}
	command := []string{m.Command}
	for _, argument := range m.Args {
		if !strings.HasPrefix(argument, "{{") || !strings.HasSuffix(argument, "}}") {
			command = append(command, argument)
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(argument, "{{"), "}}")
		if name == "arguments" {
			command = append(command, string(arguments))
			continue
		}
		spread := strings.HasSuffix(name, "...")
		name = strings.TrimSuffix(name, "...")
		value, exists := object[name]
		if !exists {
			return nil, "", fmt.Errorf("missing local tool argument %q", name)
		}
		values := []json.RawMessage{value}
		if spread {
			if json.Unmarshal(value, &values) != nil || values == nil {
				return nil, "", fmt.Errorf("local tool argument %q must be an array", name)
			}
		}
		for _, item := range values {
			var text string
			var scalar any
			_ = json.Unmarshal(item, &scalar)
			switch value := scalar.(type) {
			case string:
				text = value
			case bool, float64:
				text = string(item)
			default:
				return nil, "", fmt.Errorf("local tool argument %q must contain strings, numbers or booleans", name)
			}
			command = append(command, text)
		}
	}
	stdin := ""
	if m.Mode == ModeStdio {
		stdin = string(arguments) + "\n"
	}
	return command, stdin, nil
}

func (m Manifest) Request(runID, callID string, arguments json.RawMessage) (worker.Request, error) {
	command, stdin, err := m.Invocation(arguments)
	if err != nil {
		return worker.Request{}, err
	}
	// Only operator-owned environment values opt in to run substitution. Model
	// arguments and stdin remain literal, and the reusable manifest stays intact.
	env := make(map[string]string, len(m.Env))
	for key, value := range m.Env {
		if strings.Contains(value, "{{run_id}}") {
			if strings.TrimSpace(runID) == "" {
				return worker.Request{}, errors.New("local tool environment requires a run ID")
			}
			value = strings.ReplaceAll(value, "{{run_id}}", runID)
		}
		env[key] = value
	}
	return worker.Request{RunID: runID, CallID: callID, Command: command, Dir: m.Dir, Env: envList(env), Stdin: stdin, CancelGraceSeconds: m.CancelGraceSeconds}, nil
}

func (m Manifest) Execute(ctx context.Context, runID, callID string, arguments json.RawMessage, output ...func(string, string)) (worker.Result, error) {
	request, err := m.Request(runID, callID, arguments)
	if err != nil {
		return worker.Result{}, err
	}
	if len(output) > 0 {
		request.OnOutput = output[0]
	}
	if m.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	return worker.Run(ctx, request)
}

func envList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	result := make([]string, 0, len(env))
	for key, value := range env {
		result = append(result, key+"="+value)
	}
	// Environment order is not semantically relevant; sorting makes action
	// snapshots deterministic when a manifest is inspected repeatedly.
	sort.Strings(result)
	return result
}
