package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A history page subscribes at its committed watermark. Reuse only an existing
// index of the pinned generation whose complete event sequence was validated;
// replay and telemetry retain the original event reader and ownership rules.
func (s *Store) indexedEventWatermark(id string, after int) (Run, bool, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return Run{}, false, os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(s.root, id+".json"))
	if err != nil {
		return Run{}, false, err
	}
	defer f.Close()
	generation, err := displayGeneration(f)
	if err != nil {
		return Run{}, false, err
	}
	index, err := readJSONDisplayIndex(filepath.Join(s.root, id+".display"), generation)
	if err != nil || !index.EventsValidated {
		return Run{}, false, nil
	}
	var run Run
	if err = json.Unmarshal(index.Fields["run"], &run); err != nil {
		return Run{}, false, nil
	}
	if run.ID == "" {
		return Run{}, false, os.ErrNotExist
	}
	if run.ID != id {
		return Run{}, false, fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	if run.Deleted {
		return run, false, os.ErrNotExist
	}
	return run, after >= run.EventSeq, nil
}
