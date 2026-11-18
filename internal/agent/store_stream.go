package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
)

const largeSnapshotBytes = 16 << 20

// Walk top-level arrays one record at a time. Decoder.Decode(&Snapshot) also
// buffers the complete JSON value, even when its input is an os.File.
func decodeSnapshot(r io.Reader, snap *Snapshot) error {
	d := json.NewDecoder(r)
	// Historical requests repeat message bodies. Share only immutable strings
	// within this decode; every message and its mutable slices remain independent.
	// Bound retained payload (not map overhead), including overwritten JSON keys.
	var requestStrings map[string]string
	requestStringBytes := 0
	shareRequestString := func(text string) string {
		if len(text) < 1024 {
			return text
		}
		if shared, ok := requestStrings[text]; ok {
			return shared
		}
		if len(text) <= (16<<20)-requestStringBytes {
			if requestStrings == nil {
				requestStrings = make(map[string]string)
			}
			requestStrings[text] = text
			requestStringBytes += len(text)
		}
		return text
	}
	return readJSONObject(d, func(name string) error {
		value := reflect.ValueOf(snap).Elem()
		typ := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := typ.Field(i)
			key := strings.Split(field.Tag.Get("json"), ",")[0]
			if field.PkgPath != "" || key == "-" || !strings.EqualFold(name, key) {
				continue
			}
			v := value.Field(i)
			if v.Kind() != reflect.Slice {
				return d.Decode(v.Addr().Interface())
			}
			count, present := 0, false
			err := readJSONArray(d, func() error {
				// Match encoding/json's slice reuse, including duplicate keys
				// whose later objects merge into existing element values.
				if count >= v.Cap() {
					v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
				} else if count >= v.Len() {
					v.SetLen(count + 1)
				}
				record := v.Index(count).Addr().Interface()
				if err := d.Decode(record); err != nil {
					return err
				}
				if request, ok := record.(*ModelRequest); ok {
					for i := range request.Messages {
						message := &request.Messages[i]
						message.Content = shareRequestString(message.Content)
						message.Thinking = shareRequestString(message.Thinking)
					}
				}
				count++
				return nil
			}, func() { present = true })
			if err != nil {
				return err
			}
			if !present {
				v.Set(reflect.Zero(v.Type()))
			} else if count == 0 {
				v.Set(reflect.MakeSlice(v.Type(), 0, 0))
			} else {
				v.SetLen(count)
			}
			return nil
		}
		return discardJSON(d)
	})
}

func readJSONObject(d *json.Decoder, field func(string) error) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return errors.New("invalid run snapshot")
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return errors.New("invalid snapshot field")
		}
		if err := field(name); err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("invalid trailing snapshot data")
	}
	return nil
}

func readJSONArray(d *json.Decoder, record func() error, present func()) error {
	token, err := d.Token()
	if err != nil || token == nil {
		return err
	}
	if token != json.Delim('[') {
		return errors.New("invalid snapshot array")
	}
	if present != nil {
		present()
	}
	for d.More() {
		if err := record(); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// Unknown/display-omitted fields still undergo standard-library syntax
// validation, but their arrays/objects are never retained as one raw value.
func discardJSON(d *json.Decoder) error {
	return discardJSONDepth(d, 1, nil)
}

func discardJSONDepth(d *json.Decoder, depth int, before func()) error {
	if before != nil {
		before()
	}
	token, err := d.Token()
	if err != nil {
		// Token converts unknown numbers to float64, unlike Unmarshal's
		// ignored fields. A range error follows complete lexical consumption
		// and token-state advancement; keep skipping this valid JSON number.
		// Known fields still use Decode and retain their float64/error contract.
		var numeric *json.UnmarshalTypeError
		if errors.As(err, &numeric) && numeric.Type == reflect.TypeOf(float64(0)) && strings.HasPrefix(numeric.Value, "number ") {
			return nil
		}
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if delim != '[' && delim != '{' {
		return errors.New("invalid snapshot value")
	}
	// Decoder.Token does not apply encoding/json's nesting limit to its
	// delimiters. Include the surrounding root object in the same 10000 cap.
	if depth >= 10000 {
		return errors.New("snapshot JSON exceeds maximum depth")
	}
	for d.More() {
		if delim == '{' {
			if before != nil {
				before()
			}
			if _, err := d.Token(); err != nil {
				return err
			}
		}
		if err := discardJSONDepth(d, depth+1, before); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// The existing verified Update path owns this snapshot. Preserve the atomic
// staged-file publication while bounding the output buffer to one record.
func (s *Store) saveLargeSnapshotLocked(snap Snapshot) (err error) {
	if s.beforePersist != nil {
		s.beforePersist(snap.Run.ID)
	}
	f, err := os.CreateTemp(s.root, ".trace-*.tmp")
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	w := bufio.NewWriter(f)
	if err = encodeSnapshot(w, snap); err != nil {
		return err
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	metadata, err := json.Marshal(Observation{Run: snap.Run, Plan: snap.Plan, Workers: snap.Workers, WorkerMessages: snap.WorkerMessages, Mailbox: snap.Mailbox})
	if err != nil {
		return err
	}
	if err = s.publishSnapshotViewsLocked(snap.Run.ID, &stagedSnapshot{path: f.Name(), info: info}, nil, metadata); err != nil {
		return err
	}
	s.notifyEventCommit(snap.Run.ID)
	s.notifySchedulingCommit(snap.Run.ID, snap.Events, snap.Run.EventSeq)
	return nil
}

type snapshotByteCount int

func (n *snapshotByteCount) Write(p []byte) (int, error) {
	*n += snapshotByteCount(len(p))
	return len(p), nil
}

func encodeSnapshot(w io.Writer, snap Snapshot) error {
	write := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	value := reflect.ValueOf(snap)
	typ := value.Type()
	separator := ""
	for i := 0; i < value.NumField(); i++ {
		field, v := typ.Field(i), value.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")
		if field.PkgPath != "" || tag[0] == "-" {
			continue
		}
		if len(tag) > 1 && tag[1] == "omitempty" && (v.IsZero() || (v.Kind() == reflect.Slice || v.Kind() == reflect.Map) && v.Len() == 0) {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s%q:", separator, tag[0]); err != nil {
			return err
		}
		separator = ","
		if v.Kind() != reflect.Slice || v.IsNil() {
			if err := write(v.Interface()); err != nil {
				return err
			}
			continue
		}
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
		for j := 0; j < v.Len(); j++ {
			if j > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if err := write(v.Index(j).Interface()); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "]"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "}")
	return err
}
