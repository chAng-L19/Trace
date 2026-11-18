package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const activityProjectionMaxBytes = 32 << 20

var errActivityTooLarge = errors.New("activity projection exceeds 32 MiB; use paged history or record details")

type activityRecords struct {
	response func(int) (snapshotJSON, error)
	message  func(int) (snapshotJSON, error)
	action   func(int) (snapshotJSON, error)
	indexed  bool
}

// Limit each value before encoding/json buffers it. The complete source may
// exceed the limit; only one record and the retained display fields are bounded.
type activityReader struct {
	io.Reader
	offset, through int64
}

func (r *activityReader) Read(p []byte) (int, error) {
	remaining := r.through - r.offset
	if remaining <= 0 {
		return 0, errActivityTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.Reader.Read(p)
	r.offset += int64(n)
	return n, err
}

// DSH's bounded read records and Harness's passive history projections are
// adapted to the existing immutable JSON generation. Requests retain metadata
// and legacy message IDs; response bodies are read only for selected records.
func readActivityFields(source interface {
	io.Reader
	io.ReaderAt
}) (*storedView, *activityRecords, error) {
	return readActivityFieldsFor(source, "activity")
}

func readActivityFieldsFor(source interface {
	io.Reader
	io.ReaderAt
}, mode string) (*storedView, *activityRecords, error) {
	return readActivityFieldsIndexed(source, mode, nil)
}

func readActivityFieldsIndexed(source interface {
	io.Reader
	io.ReaderAt
}, mode string, index *displayIndexBuilder) (*storedView, *activityRecords, error) {
	r := &activityReader{Reader: source, through: activityProjectionMaxBytes}
	d := json.NewDecoder(r)
	fields := map[string]json.RawMessage{}
	var responses []jsonSpan
	retained := 0
	err := readJSONObject(d, func(name string) (err error) {
		if index != nil {
			original := name
			name = strings.ToLower(name)
			if original != name || index.seen[name] {
				index.index.Canonical = false
			}
			index.seen[name] = true
			defer func() {
				if name == "run" {
					index.index.RunEnd = d.InputOffset()
				}
				if name == "events" {
					index.index.EventEnd = d.InputOffset()
				}
			}()
		}
		defer func() {
			if err != nil {
				err = fmt.Errorf("activity field %s at byte %d: %w", name, d.InputOffset(), err)
			}
		}()
		r.through = d.InputOffset() + activityProjectionMaxBytes
		if index != nil && name == "events" {
			return readJSONArray(d, func() error {
				r.through = d.InputOffset() + activityProjectionMaxBytes
				var header struct {
					Sequence int `json:"sequence"`
				}
				return d.Decode(&header)
			}, nil)
		}
		if mode == "planner" {
			switch name {
			case "run", "plan", "workers", "mailbox", "root_contract", "messages", "worker_messages", "child_reports":
			default:
				return discardActivityJSON(d, r)
			}
		}
		if mode == "worker" && name != "run" && name != "requests" && name != "responses" && name != "actions" {
			return discardActivityJSON(d, r)
		}
		var raw json.RawMessage
		switch name {
		case "run", "plan", "workers", "mailbox", "root_contract", "worker_context", "worker_settlement":
			if index == nil && (name == "worker_context" || name == "worker_settlement") {
				return discardActivityJSON(d, r)
			}
			if err := d.Decode(&raw); err != nil {
				return err
			}
			if index != nil {
				if err := index.capacityField(name, raw); err != nil {
					return err
				}
			}
		case "messages", "requests", "responses", "actions", "assets", "child_reports", "reports", "worker_messages", "evidence_chains", "evidence_chain_refs", "compactions":
			if index != nil {
				if err := index.reset(name); err != nil {
					return err
				}
			}
			var out bytes.Buffer
			out.WriteByte('[')
			count := 0
			present := false
			if name == "responses" {
				responses = nil
			}
			err := readJSONArray(d, func() error {
				r.through = d.InputOffset() + activityProjectionMaxBytes
				var row json.RawMessage
				if err := d.Decode(&row); err != nil {
					return err
				}
				if index != nil && name == "actions" {
					if err := index.capacityAction(count, row); err != nil {
						return err
					}
				}
				if index != nil && (name == "messages" || name == "responses" || name == "actions") {
					span := displayRecordSpan{Start: d.InputOffset() - int64(len(row)), End: d.InputOffset()}
					var err error
					row, err = index.record(name, count, row, span)
					if err != nil {
						return err
					}
				}
				switch name {
				case "messages":
					if mode == "planner" {
						var header struct {
							ID          string            `json:"id"`
							Role        string            `json:"role"`
							Attachments []InputAttachment `json:"attachments,omitempty"`
						}
						if err := json.Unmarshal(row, &header); err != nil {
							return err
						}
						row, _ = json.Marshal(header)
					}
				case "requests":
					if mode == "worker" {
						var h struct {
							ID string `json:"id"`
						}
						if err := json.Unmarshal(row, &h); err != nil {
							return err
						}
						row, _ = json.Marshal(h)
						break
					}
					var request struct {
						ModelRequest
						Messages []struct {
							ID string `json:"id"`
						} `json:"messages"`
						Tools snapshotJSON `json:"tools"`
					}
					if err := json.Unmarshal(row, &request); err != nil {
						return err
					}
					// Validate all typed metadata once, then exclude duplicate
					// histories and manifests before retaining any display cache.
					// Complete metadata stays in the authoritative request detail.
					request.ModelRequest.Messages = nil
					request.ModelRequest.Tools = nil
					request.ModelRequest.ContextManifest = nil
					header := struct {
						ModelRequest
						Messages any `json:"messages"`
					}{request.ModelRequest, request.Messages}
					var err error
					row, err = json.Marshal(header)
					if err != nil {
						return err
					}
				case "responses":
					if index != nil {
						break
					}
					responses = append(responses, jsonSpan{int(d.InputOffset()) - len(row), int(d.InputOffset())})
					var header struct {
						RequestID string    `json:"request_id"`
						CreatedAt time.Time `json:"created_at"`
					}
					if err := json.Unmarshal(row, &header); err != nil {
						return err
					}
					row, _ = json.Marshal(header)
				case "actions":
					if mode == "worker" {
						var h workerActionHeader
						if err := json.Unmarshal(row, &h); err != nil {
							return err
						}
						row, _ = json.Marshal(h)
					}
				}
				if retained+out.Len()+len(row) > activityProjectionMaxBytes {
					return fmt.Errorf("record %d retained=%d field=%d record=%d: %w", count, retained, out.Len(), len(row), errActivityTooLarge)
				}
				if count > 0 {
					out.WriteByte(',')
				}
				out.Write(row)
				count++
				return nil
			}, func() { present = true })
			if err != nil {
				return err
			}
			if index != nil && name == "actions" {
				if !present {
					index.capacity.Actions = nil
				} else if count == 0 {
					index.capacity.Actions = []Action{}
				} else {
					index.capacity.Actions = index.capacity.Actions[:count]
				}
			}
			out.WriteByte(']')
			raw = out.Bytes()
		default:
			// Omitted arrays (especially stream events) can be much larger than
			// the complete display. Reset the read budget between their records.
			return discardActivityJSON(d, r)
		}
		retained -= len(fields[name])
		retained += len(raw)
		if retained > activityProjectionMaxBytes {
			return fmt.Errorf("retained=%d: %w", retained, errActivityTooLarge)
		}
		fields[name] = raw
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	read := func(span jsonSpan) (snapshotJSON, error) {
		raw := make([]byte, span.end-span.start)
		_, err := source.ReadAt(raw, int64(span.start))
		return raw, err
	}
	loaded := 0
	records := &activityRecords{
		response: func(index int) (snapshotJSON, error) {
			span := responses[index]
			loaded += span.end - span.start
			if loaded > activityProjectionMaxBytes {
				return nil, fmt.Errorf("selected responses bytes=%d: %w", loaded, errActivityTooLarge)
			}
			return read(span)
		},
	}
	return &storedView{fields: fields, workspaceBase: &workspaceBase{}}, records, nil
}

func discardActivityJSON(d *json.Decoder, r *activityReader) error {
	return discardJSONDepth(d, 1, func() {
		r.through = d.InputOffset() + activityProjectionMaxBytes
	})
}
