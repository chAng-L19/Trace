package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type FileEdit struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

type FileRequest struct {
	StartLine    int        `json:"start_line,omitempty"`
	LineCount    int        `json:"line_count,omitempty"`
	Edits        []FileEdit `json:"edits,omitempty"`
	Path         string     `json:"path"`
	Content      string     `json:"content"`
	OldText      string     `json:"old_text"`
	NewText      string     `json:"new_text"`
	ExpectedHash string     `json:"expected_hash,omitempty"`
	Offset       int        `json:"offset,omitempty"`
	Limit        int        `json:"limit,omitempty"`
}

func fileOperation(name string, r FileRequest, checkRead ...func(string) error) (any, error) {
	if r.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	path, err := filepath.Abs(r.Path)
	if err != nil {
		return nil, err
	}
	if name == "read" {
		return readFilePage(path, r, checkRead)
	}
	path, unlock, err := lockFileMutation(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, readErr
	}
	if r.ExpectedHash != "" && (readErr != nil || promptHash(string(data)) != r.ExpectedHash) {
		return nil, fmt.Errorf("file changed: expected hash mismatch")
	}
	content := r.Content
	if name == "edit" {
		if readErr != nil {
			return nil, readErr
		}
		// Edits return a diff containing old bytes. Apply the same audit-only
		// gate as read before constructing that diff.
		if len(checkRead) > 0 {
			hash := promptHash(string(data))
			for _, check := range checkRead {
				if err := check(hash); err != nil {
					return nil, err
				}
			}
		}
		edits := r.Edits
		if len(edits) == 0 {
			edits = []FileEdit{{OldText: r.OldText, NewText: r.NewText}}
		}
		content = string(data)
		for _, edit := range edits {
			if edit.OldText == "" || strings.Count(content, edit.OldText) != 1 {
				return nil, fmt.Errorf("old_text must match exactly once")
			}
			content = strings.Replace(content, edit.OldText, edit.NewText, 1)
		}
	}
	mode := os.FileMode(0600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".trace-edit-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return nil, err
	}
	if _, err = f.WriteString(content); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return nil, err
	}
	result := map[string]any{"path": path, "hash": promptHash(content), "bytes": len(content)}
	if name == "edit" {
		result["diff"] = editDiff(string(data), content)
	}
	return result, nil
}

// Minimal unified hunk: trim unchanged lines, preserving the exact changed block.
func editDiff(before, after string) string {
	old, new := strings.Split(before, "\n"), strings.Split(after, "\n")
	start := 0
	for start < len(old) && start < len(new) && old[start] == new[start] {
		start++
	}
	if start == len(old) && start == len(new) {
		return ""
	}
	endOld, endNew := len(old), len(new)
	for endOld > start && endNew > start && old[endOld-1] == new[endNew-1] {
		endOld--
		endNew--
	}
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", start+1, endOld-start, start+1, endNew-start)
	for _, line := range old[start:endOld] {
		fmt.Fprintf(&b, "-%s\n", line)
	}
	for _, line := range new[start:endNew] {
		fmt.Fprintf(&b, "+%s\n", line)
	}
	return b.String()
}
