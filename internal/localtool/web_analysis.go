package localtool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
)

// The selected web implementation is a separately installed local command,
// never an embedded REA runtime. Its own catalog is the schema source of truth.
func webAnalysisPresets(ctx context.Context) ([]Manifest, DependencyStatus) {
	status := DependencyStatus{Name: "Trace Web Analysis", Detail: "Set TRACE_WEB_ANALYSIS_DIR to the built tools/web-analysis directory"}
	root := os.Getenv("TRACE_WEB_ANALYSIS_DIR")
	if root == "" {
		if executable, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(executable), "tools", "web-analysis")
			if _, err := os.Stat(filepath.Join(candidate, "dist", "trace-cli.js")); err == nil {
				root = candidate
			}
		}
	}
	if root == "" {
		return nil, status
	}
	root, err := filepath.Abs(root)
	if err != nil {
		status.Detail = err.Error()
		return nil, status
	}
	node, err := exec.LookPath("node")
	if err != nil {
		status.Detail = err.Error()
		return nil, status
	}
	entry := filepath.Join(root, "dist", "trace-cli.js")
	command := exec.CommandContext(ctx, node, entry, "--catalog")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		status.Detail = string(output)
		return nil, status
	}
	var catalog struct {
		Operations []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(output, &catalog); err != nil {
		status.Detail = err.Error()
		return nil, status
	}
	if len(catalog.Operations) == 0 {
		status.Detail = "Web analysis catalog contains no operations"
		return nil, status
	}
	var manifests []Manifest
	for _, operation := range catalog.Operations {
		// Each named tool retains its exact upstream input schema. The CLI accepts
		// the operation as argv so the existing generic JSONL adapter can be used.
		manifests = append(manifests, Manifest{
			Name: operation.Name, Description: operation.Description, InputSchema: operation.InputSchema,
			Command: node, Args: []string{entry, "--operation", operation.Name}, Mode: ModeStdio,
			Dir: root, GitHubURL: "https://github.com/morluto/rea", LocalPath: root,
			CancelGraceSeconds: 15,
		})
	}
	status.Available = true
	status.Detail = root
	return manifests, status
}
