// Package picore hosts the upstream pi conversational loop. Trace remains the
// authority for requests, actions, evidence, controls and worker settlement.
package picore

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

//go:embed host.mjs
var host string

// IsHaltOnly recognizes a control boundary only when every leaf cause is halt.
// errors.Is alone also matches errors.Join(halt, persistenceFailure).
func IsHaltOnly(err, halt error) bool {
	if err == nil || halt == nil {
		return false
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		found := false
		for _, cause := range wrapped.Unwrap() {
			if cause == nil {
				continue
			}
			found = true
			if !IsHaltOnly(cause, halt) {
				return false
			}
		}
		return found
	case interface{ Unwrap() error }:
		return IsHaltOnly(wrapped.Unwrap(), halt)
	default:
		return errors.Is(err, halt)
	}
}

// RuntimeDir resolves installed dependencies, never installs on the request path.
func RuntimeDir() (string, error) {
	if dir := os.Getenv("TRACE_PI_RUNTIME"); dir != "" {
		absolute, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(absolute, "node_modules", "@earendil-works", "pi-agent-core", "package.json")); err != nil {
			return "", fmt.Errorf("pi runtime %s: %w", absolute, err)
		}
		return absolute, nil
	}
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	for _, dir := range []string{filepath.Join(filepath.Dir(exe), "..", "runtime", "pi"), filepath.Join(cwd, "runtime", "pi"), filepath.Join(cwd, "internal", "picore")} {
		if _, err := os.Stat(filepath.Join(dir, "node_modules", "@earendil-works", "pi-agent-core", "package.json")); err == nil {
			return filepath.Abs(dir)
		}
	}
	return "", errors.New("pi runtime missing: install internal/picore/package-lock.json dependencies and set TRACE_PI_RUNTIME")
}

// Run invokes one native pi loop per active run. No fallback to the former Go
// loop: a missing/broken framework is an explicit execution error.
func Run(ctx context.Context, handle func(string, json.RawMessage) (any, error), halt error) error {
	dir, err := RuntimeDir()
	if err != nil {
		return err
	}
	node := os.Getenv("TRACE_PI_NODE")
	if node == "" {
		node = "node"
	}
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "--eval", host)
	cmd.Dir = dir
	cmd.Env = []string{}
	// The host only coordinates pi; credentials stay in the Go provider/tool
	// channels. Do not inherit model or MCP secrets into the Node process.
	for _, entry := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
		if key == "PATH" || key == "HOME" || key == "USERPROFILE" || key == "SYSTEMROOT" || key == "WINDIR" || key == "TMPDIR" || key == "TMP" || key == "TEMP" || key == "LANG" || strings.HasPrefix(key, "LC_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return err
	}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	if err = cmd.Start(); err != nil {
		input.Close()
		return fmt.Errorf("start pi host: %w", err)
	}
	encoder := json.NewEncoder(input)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var runErr error
	var resultMu sync.Mutex
	var calls sync.WaitGroup
	slots := make(chan struct{}, 5)
	recordError := func(err error) {
		resultMu.Lock()
		defer resultMu.Unlock()
		if runErr == nil || IsHaltOnly(runErr, halt) && !IsHaltOnly(err, halt) {
			runErr = err
		} else if !IsHaltOnly(runErr, halt) && !IsHaltOnly(err, halt) {
			runErr = errors.Join(runErr, err)
		}
	}
	dispatch := func(id int, method string, params json.RawMessage) {
		result, callErr := handle(method, params)
		response := map[string]any{"id": id, "result": result}
		if callErr != nil {
			recordError(callErr)
			if IsHaltOnly(callErr, halt) {
				response["halt"] = true
			} else {
				response["error"] = callErr.Error()
			}
		}
		resultMu.Lock()
		defer resultMu.Unlock()
		if err := encoder.Encode(response); err != nil && (runErr == nil || IsHaltOnly(runErr, halt)) {
			// Late successful callbacks may write after a normal halt closed stdin.
			closed := errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE)
			if !IsHaltOnly(runErr, halt) || !closed {
				runErr = err
			}
		}
	}
	done := false
	for scanner.Scan() {
		var request struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  string          `json:"error"`
		}
		if err = json.Unmarshal(scanner.Bytes(), &request); err != nil {
			recordError(fmt.Errorf("pi protocol: %w", err))
			break
		}
		if request.Method == "done" {
			calls.Wait()
			done = true
			if request.Error != "" {
				recordError(errors.New(request.Error))
			}
			break
		}
		if request.ID <= 0 || request.Method == "" {
			recordError(errors.New("invalid pi request envelope"))
			break
		}
		var mode struct {
			Parallel bool `json:"parallel"`
		}
		_ = json.Unmarshal(request.Params, &mode)
		if request.Method == "tool" && mode.Parallel {
			slots <- struct{}{}
			calls.Add(1)
			go func(id int, params json.RawMessage) {
				defer calls.Done()
				defer func() { <-slots }()
				dispatch(id, "tool", params)
			}(request.ID, request.Params)
		} else {
			calls.Wait()
			dispatch(request.ID, request.Method, request.Params)
		}
	}
	calls.Wait()
	if err = scanner.Err(); err != nil {
		recordError(fmt.Errorf("pi output: %w", err))
	}
	input.Close()
	if !done {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		if runErr != nil && !IsHaltOnly(runErr, halt) {
			return errors.Join(context.Cause(ctx), runErr)
		}
		return context.Cause(ctx)
	}
	if runErr != nil {
		if waitErr != nil {
			return errors.Join(runErr, fmt.Errorf("pi host: %w: %s", waitErr, diagnostics.String()))
		}
		return runErr
	}
	if waitErr != nil {
		return fmt.Errorf("pi host: %w: %s", waitErr, diagnostics.String())
	}
	if !done {
		return errors.New("pi host exited before settlement")
	}
	return nil
}
