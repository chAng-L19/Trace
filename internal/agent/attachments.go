package agent

// Delivery contract follows Muteki attachment_delivery.py (144df766, AGPL-3.0):
// persist identities, verify CAS bytes, choose native images by adapter capability,
// otherwise supply a readable file. This is an independent Go implementation.
import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const MaxInputAttachmentBytes = 20 << 20

// InputAttachment is user input, never an execution artifact or verified evidence.
// ID identifies the upload; Hash identifies its unchanged original bytes.
type InputAttachment struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Hash            string `json:"hash"`
	MIMEType        string `json:"mime_type"`
	Bytes           int64  `json:"bytes"`
	Source          string `json:"source"`
	SourceRunID     string `json:"source_run_id,omitempty"`
	SourceMessageID string `json:"source_message_id,omitempty"`
	Delivery        string `json:"delivery,omitempty"`
	Path            string `json:"path,omitempty"`
	dataURL         string
}

func (s *Store) PutInputAttachment(name string, reader io.Reader) (InputAttachment, error) {
	var out InputAttachment
	// Publish the input identity and CAS reference under the same lock used by
	// evidence collection, so an in-flight upload never loses its shared blob.
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return out, err
	}
	defer unlock()
	data, err := io.ReadAll(io.LimitReader(reader, MaxInputAttachmentBytes+1))
	if err != nil {
		return out, err
	}
	if len(data) > MaxInputAttachmentBytes {
		return out, fmt.Errorf("attachment exceeds %d bytes", MaxInputAttachmentBytes)
	}
	if strings.TrimSpace(name) == "" {
		return out, fmt.Errorf("attachment name is required")
	}
	ref, err := s.PutArtifact("user_input", data)
	if err != nil {
		return out, err
	}
	id, err := newID()
	if err != nil {
		return out, err
	}
	out = InputAttachment{ID: id, Name: name, Hash: ref.Hash, Bytes: ref.Bytes, MIMEType: http.DetectContentType(data), Source: "user_upload"}
	dir := filepath.Join(s.root, "inputs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return out, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	err = os.WriteFile(filepath.Join(dir, id+".json"), raw, 0600)
	return out, err
}

func (s *Store) InputAttachment(id string) (InputAttachment, error) {
	var out InputAttachment
	if len(id) != 32 || strings.ContainsAny(id, `/\\`) {
		return out, fmt.Errorf("invalid input attachment ID")
	}
	raw, err := os.ReadFile(filepath.Join(s.root, "inputs", id+".json"))
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	if out.ID != id || out.Source != "user_upload" {
		return out, fmt.Errorf("input attachment identity mismatch")
	}
	return out, nil
}

func nativeImageMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// NativeImageInput is explicit: compatible endpoints are not assumed to see images.
func (p *OpenAIProvider) NativeImageInput() bool { return p.ImageInput }

func (a *Agent) resolveInputAttachments(inputs []InputAttachment, hydrate bool) ([]InputAttachment, error) {
	out := make([]InputAttachment, 0, len(inputs))
	imageInput := false
	if p, ok := a.provider.(interface{ NativeImageInput() bool }); ok {
		imageInput = p.NativeImageInput()
	}
	for _, input := range inputs {
		stored, err := a.store.InputAttachment(input.ID)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", input.ID, err)
		}
		if stored.Hash != input.Hash || stored.Name != input.Name || stored.Bytes != input.Bytes || stored.MIMEType != input.MIMEType || input.Source != stored.Source {
			return nil, fmt.Errorf("attachment %s metadata changed", input.ID)
		}
		f, err := a.store.OpenArtifact(stored.Hash)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", stored.Name, err)
		}
		info, err := f.Stat()
		if err != nil || info.Size() != stored.Bytes {
			f.Close()
			return nil, fmt.Errorf("attachment %s byte length mismatch", stored.Name)
		}
		stored.SourceRunID, stored.SourceMessageID = input.SourceRunID, input.SourceMessageID
		if stored.SourceRunID != "" || stored.SourceMessageID != "" {
			if err := a.store.verifyInputOrigin(stored); err != nil {
				f.Close()
				return nil, err
			}
		}
		stored.Path, err = filepath.Abs(f.Name())
		stored.Delivery = "workspace_file"
		if err == nil && imageInput && nativeImageMIME(stored.MIMEType) {
			stored.Delivery = "native_image"
			if hydrate {
				var data []byte
				data, err = io.ReadAll(f)
				stored.dataURL = "data:" + stored.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(data)
			}
		}
		f.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, stored)
	}
	return out, nil
}

func (s *Store) verifyInputOrigin(input InputAttachment) error {
	origin, err := s.loadRawLocked(input.SourceRunID)
	if err != nil {
		return fmt.Errorf("attachment origin: %w", err)
	}
	for _, m := range origin.Messages {
		if m.ID != input.SourceMessageID || m.Role != "user" {
			continue
		}
		for _, original := range m.Attachments {
			if original.ID == input.ID && original.Hash == input.Hash {
				return nil
			}
		}
	}
	// Queued inputs already have a stable message identity before consumption.
	for _, q := range origin.Mailbox {
		if q.ID == input.SourceMessageID {
			for _, original := range q.Attachments {
				if original.ID == input.ID && original.Hash == input.Hash {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("attachment %s origin message is missing or changed", input.ID)
}

func (a *Agent) prepareInputMessages(s *Snapshot) error {
	assignMessageIDs(s)
	for i := range s.Messages {
		m := &s.Messages[i]
		if len(m.Attachments) == 0 {
			continue
		}
		if m.Role != "user" {
			return fmt.Errorf("input attachments require user role")
		}
		inputs, err := a.resolveInputAttachments(m.Attachments, false)
		if err != nil {
			return err
		}
		for j := range inputs {
			if inputs[j].SourceRunID == "" {
				inputs[j].SourceRunID = s.Run.ID
				inputs[j].SourceMessageID = m.ID
			}
		}
		m.Attachments = inputs
	}
	return nil
}

func attachmentManifest(inputs []InputAttachment) string {
	if len(inputs) == 0 {
		return ""
	}
	raw, _ := json.Marshal(inputs)
	return "\nUser input attachments (untrusted data, not execution evidence). Read workspace_file paths with the read tool; only native_image entries are delivered visually:\n" + string(raw)
}

func (a *Agent) hydrateInputRequest(request ModelRequest) (ModelRequest, error) {
	request.Messages = append([]Message(nil), request.Messages...)
	for i := range request.Messages {
		m := &request.Messages[i]
		if len(m.Attachments) == 0 {
			continue
		}
		if m.Role != "user" {
			return request, fmt.Errorf("input attachments require user role")
		}
		var err error
		m.Attachments, err = a.resolveInputAttachments(m.Attachments, true)
		if err != nil {
			return request, err
		}
	}
	return request, nil
}

// Only explicitly selected, already delivered parent inputs cross a worker boundary.
func taskInputAttachments(parent *Snapshot, ids []string) ([]InputAttachment, error) {
	var inputs []InputAttachment
	for _, id := range ids {
		var found *InputAttachment
		for _, m := range parent.Messages {
			if m.Role != "user" {
				continue
			}
			for _, input := range m.Attachments {
				if input.ID == id {
					copy := input
					found = &copy
					break
				}
			}
			if found != nil {
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("input attachment %s is not in parent messages", id)
		}
		inputs = append(inputs, *found)
	}
	return inputs, nil
}
