package agent

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func reportArtifactURL(hash string) string { return "/api/artifacts/" + hash + "/download" }
func reportImageExtension(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}
func reportAttachmentLabel(ref ArtifactRef) string {
	name := ref.Name
	if name == "" {
		name = ref.Hash
	}
	return strings.NewReplacer("\\", "＼", "[", "［", "]", "］", "\r", " ", "\n", " ").Replace(name)
}

// Use the same validated source view as result acceptance. Paths and model prose
// do not supply attachments, and output logs stay in the evidence export.
func reportEvidenceAttachments(s *Snapshot, id string) []ReportAttachment {
	var refs []ArtifactRef
	if o, ok := delegatedObservationFor(s, id); ok {
		refs = o.Artifacts
	} else {
		for _, e := range s.Evidence {
			if e.ID != id {
				continue
			}
			for _, a := range s.Actions {
				if a.RequestID == e.RequestID && a.CallID == e.CallID && a.Result != nil {
					refs = a.Result.Artifacts
					break
				}
			}
			break
		}
	}
	var selected []ArtifactRef
	for _, ref := range refs {
		if ref.Kind != "stdout" && ref.Kind != "stderr" && ref.Kind != "search_response" {
			selected = append(selected, ref)
		}
	}
	if len(selected) == 0 {
		return nil
	}
	if _, issue := diagnoseResultEvidence(s, id, true); issue != nil {
		return nil
	}
	var out []ReportAttachment
	for _, ref := range selected {
		// Detect bytes rather than trusting a tool-supplied image MIME or filename.
		f, err := os.Open(filepath.Join(s.artifactRoot, "artifacts", ref.Hash))
		if err != nil {
			continue
		}
		var header [512]byte
		n, err := f.Read(header[:])
		f.Close()
		if err != nil && err != io.EOF {
			continue
		}
		ref.MIMEType = http.DetectContentType(header[:n])
		ref.Preview = nil
		out = append(out, ReportAttachment{EvidenceID: id, Artifact: ref})
	}
	return out
}

// ARTEX 6997a4661921 server/finding_traffic_export.go (AGPL-3.0) is the
// behavioral reference for report + screenshot ZIP export. Trace's independent
// ReportBundle uses its
// pinned CAS receipts rather than resolving filenames across runs. Historical
// report bytes are unchanged; only the ZIP copy uses relative asset links.
func (store *Store) ReportBundle(id, reportID, language string) (string, *os.File, error) {
	ref, body, err := store.ReportDocument(id, reportID, language)
	if err != nil {
		return "", nil, err
	}
	s, err := store.documentSnapshot(id, "fixed")
	if err != nil || s.Run.Deleted {
		if err == nil {
			err = os.ErrNotExist
		}
		return "", nil, err
	}
	var receipt *ReportArchive
	for i := range s.Reports {
		for _, file := range s.Reports[i].Files {
			if file.Hash == ref.Hash && file.Name == ref.Name {
				receipt = &s.Reports[i]
				break
			}
		}
		if receipt != nil {
			break
		}
	}
	if receipt == nil {
		return "", nil, fmt.Errorf("report receipt: %w", os.ErrNotExist)
	}
	file, err := os.CreateTemp("", "trace-report-*.zip")
	if err != nil {
		return "", nil, err
	}
	complete := false
	defer func() {
		if !complete {
			file.Close()
			os.Remove(file.Name())
		}
	}()
	zw := zip.NewWriter(file)
	add := func(name string, source io.Reader) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, source)
		return err
	}
	written := map[string]bool{}
	for _, a := range receipt.Attachments {
		ext := reportImageExtension(a.Artifact.MIMEType)
		if ext == "" {
			ext = ".bin"
		}
		name := "assets/" + a.Artifact.Hash + ext
		url := reportArtifactURL(a.Artifact.Hash)
		body = strings.ReplaceAll(body, url+"?preview=1", name)
		body = strings.ReplaceAll(body, url, name)
		if written[name] {
			continue
		}
		f, err := store.OpenArtifact(a.Artifact.Hash)
		if err != nil {
			return "", nil, fmt.Errorf("report attachment %s: %w", a.EvidenceID, err)
		}
		err = add(name, f)
		f.Close()
		if err != nil {
			return "", nil, err
		}
		written[name] = true
	}
	if language == "" {
		language = "zh-CN"
	}
	if err = add("report."+language+".md", strings.NewReader(body)); err != nil {
		return "", nil, err
	}
	manifest, err := json.MarshalIndent(struct {
		RunID  string        `json:"run_id"`
		Report ReportArchive `json:"report"`
	}{id, *receipt}, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err = add("manifest.json", bytes.NewReader(manifest)); err != nil {
		return "", nil, err
	}
	if err = zw.Close(); err != nil {
		return "", nil, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return "", nil, err
	}
	complete = true
	return strings.TrimSuffix(ref.Name, ".md") + ".zip", file, nil
}

// Pin prose-selected images across translation; translated prose is not a new
// source set. Revoked evidence is still removed by the current verified view.
func reportAttachmentEvidenceIDs(s *Snapshot, kind, subject, original string, body *ReportText, ids []string, attachmentsFor func(string) []ReportAttachment) []string {
	ids = append([]string(nil), ids...)
	selected := map[string]bool{}
	for _, id := range ids {
		for _, a := range attachmentsFor(id) {
			selected[a.Artifact.Hash] = true
		}
	}
	prose := []string{original}
	if body != nil {
		if body.SourceDigest == "" {
			prose = append(prose, body.ZhCN, body.En)
		}
		text := *body
		text.SourceDigest = ""
		for i := len(s.Reports) - 1; i >= 0; i-- {
			report := s.Reports[i]
			if report.Kind != kind || report.SubjectID != subject || !(body.SourceDigest != "" && report.SourceDigest == body.SourceDigest || body.SourceDigest == "" && report.DocumentDigest == valueHash(text)) {
				continue
			}
			for _, old := range report.Attachments {
				for _, current := range attachmentsFor(old.EvidenceID) {
					if current.Artifact.Hash == old.Artifact.Hash && !selected[old.Artifact.Hash] {
						if !hasString(ids, old.EvidenceID) {
							ids = append(ids, old.EvidenceID)
						}
						selected[old.Artifact.Hash] = true
					}
				}
			}
			break
		}
	}
	links := map[string]bool{}
	for _, text := range prose {
		reportMarkdownLines(text, func(text string) string {
			for _, parts := range reportLinkPattern.FindAllStringSubmatch(text, -1) {
				links[parts[1]] = true
			}
			return text
		})
	}
	for _, id := range acceptanceEvidenceIDs(s) {
		if hasString(ids, id) {
			continue
		}
		for _, a := range attachmentsFor(id) {
			url := reportArtifactURL(a.Artifact.Hash)
			if !selected[a.Artifact.Hash] && (links[url+"?preview=1"] || links[url]) {
				ids = append(ids, id)
				for _, ref := range attachmentsFor(id) {
					selected[ref.Artifact.Hash] = true
				}
				break
			}
		}
	}
	return ids
}

var reportImagePattern = regexp.MustCompile(`^!\[([^\]\n]*)\]\(([^\s)]+)\)$`)
var reportLinkPattern = regexp.MustCompile(`^(?:!\[[^\]\n]*\]|\[[^\]\n]+\])\(([^\s)]+)\)$`)
var reportInlinePattern = regexp.MustCompile("`[^`]+`|!?\\[[^\\]]*\\]\\([^\\s)]+\\)")
var reportHeadingPattern = regexp.MustCompile(`^#{1,6}\s+.+$`)
var reportTablePattern = regexp.MustCompile(`^\s*\|?\s*:?-{3,}`)
var reportFencePattern = regexp.MustCompile("^\\s*(`{3,}|~{3,})")
var reportQuotePattern = regexp.MustCompile(`^\s*>\s?`)

func reportImageLines(body string, replace func(string, []string) string) string {
	return reportMarkdownLines(body, func(text string) string {
		return reportImagePattern.ReplaceAllStringFunc(text, func(image string) string { return replace(image, reportImagePattern.FindStringSubmatch(image)) })
	})
}

// Match the escaped web renderer's fenced blocks and recursive block quotes.
// The callback only sees prose, shared by screenshot checks and attachment selection.
func reportMarkdownLines(body string, replace func(string) string) string {
	lines := strings.Split(body, "\n")
	fence := ""
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fence != "" {
			if strings.HasPrefix(strings.TrimSpace(line), fence) {
				fence = ""
			}
			continue
		}
		if opening := reportFencePattern.FindStringSubmatch(line); opening != nil {
			fence = opening[1]
			continue
		}
		if reportHeadingPattern.MatchString(line) {
			lines[i] = reportMarkdownInline(line, replace)
			continue
		}
		if strings.Contains(line, "|") && i+1 < len(lines) && reportTablePattern.MatchString(lines[i+1]) {
			lines[i] = reportMarkdownCells(line, replace)
			i += 2
			for i < len(lines) && strings.Contains(lines[i], "|") {
				lines[i] = reportMarkdownCells(lines[i], replace)
				i++
			}
			i--
			continue
		}
		if reportQuotePattern.MatchString(line) {
			start := i
			var quoted, prefixes []string
			for i < len(lines) {
				prefix := reportQuotePattern.FindString(lines[i])
				if prefix == "" {
					break
				}
				prefixes = append(prefixes, prefix)
				quoted = append(quoted, strings.TrimPrefix(lines[i], prefix))
				i++
			}
			rendered := strings.Split(reportMarkdownLines(strings.Join(quoted, "\n"), replace), "\n")
			for j := range rendered {
				lines[start+j] = prefixes[j] + rendered[j]
			}
			i--
			continue
		}
		lines[i] = reportMarkdownInline(line, replace)
	}
	return strings.Join(lines, "\n")
}
func reportAllowedImages(attachments []ReportAttachment) map[string]bool {
	allowed := map[string]bool{}
	for _, a := range attachments {
		if reportImageExtension(a.Artifact.MIMEType) != "" {
			allowed[reportArtifactURL(a.Artifact.Hash)+"?preview=1"] = true
		}
	}
	return allowed
}
func reportMissingImages(body ReportText, attachments []ReportAttachment) []string {
	allowed := reportAllowedImages(attachments)
	var missing []string
	for _, language := range []string{body.ZhCN, body.En} {
		reportImageLines(language, func(image string, parts []string) string {
			if !allowed[parts[2]] && !hasString(missing, parts[2]) {
				missing = append(missing, parts[2])
			}
			return image
		})
	}
	return missing
}
func reportImageBody(body string, attachments []ReportAttachment, language string) string {
	allowed := reportAllowedImages(attachments)
	return reportImageLines(body, func(image string, parts []string) string {
		if allowed[parts[2]] {
			return image
		}
		if language == "zh-CN" {
			return "**截图未交付：" + parts[1] + "（缺少有效来源附件）**"
		}
		return "**Screenshot not delivered: " + parts[1] + " (no verified source attachment)**"
	})
}

func reportBodyHasImage(body, url string) bool {
	found := false
	reportImageLines(body, func(image string, parts []string) string {
		if parts[2] == url {
			found = true
		}
		return image
	})
	return found
}

func reportMarkdownCells(line string, replace func(string) string) string {
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = reportMarkdownInline(cells[i], replace)
	}
	return strings.Join(cells, "|")
}

func reportMarkdownInline(line string, replace func(string) string) string {
	// Use the web renderer's left-to-right token precedence: code in a link
	// label belongs to that link, while a whole link inside code stays literal.
	return reportInlinePattern.ReplaceAllStringFunc(line, func(token string) string {
		if strings.HasPrefix(token, "`") && strings.HasSuffix(token, "`") {
			return token
		}
		return replace(token)
	})
}
