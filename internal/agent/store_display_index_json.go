package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func displayGeneration(f *os.File) (displayFileGeneration, error) {
	info, err := f.Stat()
	if err != nil {
		return displayFileGeneration{}, err
	}
	identity, err := displayFileIdentity(f, info)
	return displayFileGeneration{Identity: identity, Size: info.Size(), Modified: info.ModTime().UnixNano()}, err
}

func readJSONDisplayIndex(path string, generation displayFileGeneration) (*displayRecordIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, activityProjectionMaxBytes+66))
	if err != nil {
		return nil, err
	}
	if len(raw) < 65 || len(raw) > activityProjectionMaxBytes+65 || raw[64] != '\n' {
		return nil, fmt.Errorf("invalid display index size")
	}
	sum := sha256.Sum256(raw[65:])
	if string(raw[:64]) != hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("display index checksum mismatch")
	}
	var index displayRecordIndex
	if err = json.Unmarshal(raw[65:], &index); err != nil {
		return nil, err
	}
	if index.Version != displayIndexVersion || index.Generation != generation || index.Fields == nil {
		return nil, fmt.Errorf("display index generation mismatch")
	}
	for _, spans := range index.Spans {
		for _, span := range spans {
			if span.Start < 0 || span.End < span.Start || span.End > generation.Size || span.End-span.Start > activityProjectionMaxBytes {
				return nil, fmt.Errorf("invalid display record span")
			}
		}
	}
	return &index, nil
}

func (s *Store) stageJSONDisplayIndex(index *displayRecordIndex) (path string, err error) {
	raw, err := json.Marshal(index)
	if err != nil {
		return "", err
	}
	if len(raw) > activityProjectionMaxBytes {
		return "", errActivityTooLarge
	}
	f, err := os.CreateTemp(s.root, ".trace-display-*.tmp")
	if err != nil {
		return "", err
	}
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	sum := sha256.Sum256(raw)
	if _, err = fmt.Fprintf(f, "%x\n", sum); err != nil {
		return "", err
	}
	if _, err = f.Write(raw); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// Open and validate the authoritative generation first. The index never owns
// message or action bodies; selected records are checksummed source reads.
func (s *Store) jsonDisplayIndex(ctx context.Context, id string, source *os.File, storeLocked bool) (*displayRecordIndex, *activityRecords, error) {
	generation, err := displayGeneration(source)
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(s.root, id+".display")
	index, err := readJSONDisplayIndex(path, generation)
	if err != nil {
		if _, err = source.Seek(0, io.SeekStart); err != nil {
			return nil, nil, err
		}
		index, err = buildDisplayRecordIndex(displaySource{ctx, source, source}, nil, nil)
		if err != nil {
			return nil, nil, err
		}
		after, e := displayGeneration(source)
		if e != nil {
			return nil, nil, e
		}
		if after != generation {
			return nil, nil, fmt.Errorf("snapshot changed during display indexing")
		}
		index.Generation = generation
		if err = ctx.Err(); err != nil {
			return nil, nil, err
		}
		// Index publication is best effort: state persistence and display never
		// depend on an auxiliary file being writable. A missing index is rebuilt.
		if staged, e := s.stageJSONDisplayIndex(index); e == nil {
			defer os.Remove(staged)
			s.publishJSONDisplayIndex(ctx, id, staged, generation, storeLocked)
		}
	}
	arrays := map[string][]snapshotJSON{}
	records := index.records(func(kind string, ordinal int) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		spans := index.Spans[kind]
		if ordinal < 0 || ordinal >= len(spans) {
			return nil, fmt.Errorf("display record ordinal mismatch")
		}
		span := spans[ordinal]
		if span.Start < 0 || span.End < span.Start || span.End > generation.Size || span.End-span.Start > activityProjectionMaxBytes {
			return nil, fmt.Errorf("invalid display record span")
		}
		raw := make([]byte, span.End-span.Start)
		if _, err := source.ReadAt(raw, span.Start); err != nil {
			return nil, err
		}
		if sha256.Sum256(raw) != span.Hash {
			return nil, fmt.Errorf("display record source checksum mismatch")
		}
		if _, ok := arrays[kind]; !ok {
			var err error
			arrays[kind], err = activityArray(index.Fields, kind)
			if err != nil {
				return nil, err
			}
		}
		if ordinal >= len(arrays[kind]) {
			return nil, fmt.Errorf("display header ordinal mismatch")
		}
		check := &displayIndexBuilder{index: &displayRecordIndex{Spans: map[string][]displayRecordSpan{}}}
		header, err := check.record(kind, ordinal, raw, span)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(header, arrays[kind][ordinal]) {
			return nil, fmt.Errorf("display record header mismatch")
		}
		return raw, ctx.Err()
	})
	return index, records, nil
}

// Share the authoritative publication fence with writes and PurgeRun. Readers
// skip contended auxiliary publication; they can still use their open snapshot.
func (s *Store) publishJSONDisplayIndex(ctx context.Context, id, staged string, generation displayFileGeneration, storeLocked bool) {
	if !storeLocked {
		unlock, err := s.finishStoreLock(acquireFileLock(filepath.Join(s.root, ".store.lock"), false))
		if err != nil {
			return
		}
		defer unlock()
	}
	if ctx.Err() != nil {
		return
	}
	current, err := os.Open(filepath.Join(s.root, id+".json"))
	if err != nil {
		return
	}
	now, err := displayGeneration(current)
	current.Close()
	if err == nil && now == generation && ctx.Err() == nil {
		_ = os.Rename(staged, filepath.Join(s.root, id+".display"))
	}
}

func (s *Store) prepareJSONDisplayIndex(id string, staged *stagedSnapshot, view *storedView) {
	f, err := os.Open(staged.path)
	if err != nil {
		return
	}
	defer f.Close()
	generation, err := displayGeneration(f)
	if err != nil {
		return
	}
	index := s.appendedJSONDisplayIndex(id, view, generation.Size)
	if index == nil {
		index, err = buildDisplayRecordIndex(f, nil, nil)
		if err != nil {
			return
		}
	}
	index.Generation = generation
	staged.displayPath, _ = s.stageJSONDisplayIndex(index)
}

// The existing telemetry writer replaces exactly its recorded run/events
// spans. Everything else is byte-identical, so shift only spans after them.
func (s *Store) appendedJSONDisplayIndex(id string, view *storedView, size int64) *displayRecordIndex {
	if view == nil || view.info == nil || view.runSpan.end <= view.runSpan.start || view.eventSpan.end <= view.eventSpan.start {
		return nil
	}
	f, err := os.Open(filepath.Join(s.root, id+".json"))
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !sameGeneration(info, view.info) {
		return nil
	}
	generation, err := displayGeneration(f)
	if err != nil {
		return nil
	}
	index, err := readJSONDisplayIndex(filepath.Join(s.root, id+".display"), generation)
	if err != nil {
		return nil
	}
	if !index.Canonical || index.RunEnd == 0 || index.EventEnd == 0 {
		return nil
	}
	runDelta := int64(len(view.fields["run"]) - len(index.Fields["run"]))
	eventDelta := size - generation.Size - runDelta
	runEnd, eventEnd := index.RunEnd, index.EventEnd
	for kind, spans := range index.Spans {
		for i, span := range spans {
			delta := int64(0)
			if span.Start >= runEnd {
				delta += runDelta
			}
			if span.Start >= eventEnd {
				delta += eventDelta
			}
			spans[i].Start, spans[i].End = span.Start+delta, span.End+delta
		}
		index.Spans[kind] = spans
	}
	index.Fields["run"] = view.fields["run"]
	index.RunEnd += runDelta
	index.EventEnd += eventDelta
	if runEnd < eventEnd {
		index.EventEnd += runDelta
	} else {
		index.RunEnd += eventDelta
	}
	return index
}
