package agent

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func commitRunPurge(stage string) error {
	f, err := os.CreateTemp(stage, ".commit-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString("committed\n"); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(stage, "committed"))
}

// A process may stop between any two renames. The next store transaction restores an
// uncommitted closure, or finishes unlinking an already committed deletion.
// Lock files and directories outside this exact staging format are untouched.
func (s *Store) recoverRunPurgesLocked() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".run-delete-") {
			continue
		}
		stage := filepath.Join(s.root, entry.Name())
		marker, err := os.ReadFile(filepath.Join(stage, "committed"))
		committed := err == nil && string(marker) == "committed\n"
		if s.sqlite {
			var e error
			committed, e = s.sqlitePurgeCommitted(stage)
			if e != nil {
				return e
			}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && !committed {
			return fmt.Errorf("invalid deletion commit marker: %s", stage)
		}
		files, err := os.ReadDir(stage)
		if err != nil {
			return err
		}
		// Validate all entries before changing an interrupted stage.
		for _, file := range files {
			name := file.Name()
			if file.Type()&os.ModeType != 0 {
				return fmt.Errorf("unexpected staged file: %s", filepath.Join(stage, name))
			}
			if name == "committed" || strings.HasPrefix(name, ".commit-") {
				continue
			}
			value := strings.TrimPrefix(name, "cas-")
			if value == name {
				value = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, "run-"), ".json"), ".display")
			}
			decoded, e := hex.DecodeString(value)
			if e != nil || !((strings.HasPrefix(name, "cas-") && len(decoded) == 32) || (strings.HasPrefix(name, "run-") && (strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".display")) && len(decoded) == 16)) {
				return fmt.Errorf("unexpected deletion staging entry: %s", filepath.Join(stage, name))
			}
		}
		for _, file := range files {
			name := file.Name()
			if name == "committed" {
				continue
			}
			path := filepath.Join(stage, name)
			if committed || strings.HasPrefix(name, ".commit-") {
				if err = os.Remove(path); err != nil {
					return err
				}
				continue
			}
			target := filepath.Join(s.root, strings.TrimPrefix(name, "run-"))
			if strings.HasPrefix(name, "cas-") {
				target = filepath.Join(s.root, "artifacts", strings.TrimPrefix(name, "cas-"))
			}
			if err = os.Rename(path, target); err != nil {
				return err
			}
		}
		if committed && !s.sqlite {
			if err = os.Remove(filepath.Join(stage, "committed")); err != nil {
				return err
			}
		}
		if err = os.Remove(stage); err != nil {
			return err
		}
		if s.sqlite {
			if err = syncStoreDirectory(s.root); err != nil {
				return err
			}
			if _, e := os.Stat(filepath.Join(s.root, "artifacts")); e == nil {
				if err = syncStoreDirectory(filepath.Join(s.root, "artifacts")); err != nil {
					return err
				}
			} else if !os.IsNotExist(e) {
				return e
			}
		}
	}
	return nil
}
