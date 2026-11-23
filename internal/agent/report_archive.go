package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
)

// DSH reportRelay (MIT) keeps routine tool progress internal; Muteki reports.py
// (AGPL-3.0) binds frozen documents to source watermarks. This independent
// implementation uses Trace's existing snapshot transaction and CAS only.
type ReportText struct {
	SourceDigest string `json:"source_digest,omitempty"`
	ZhCN         string `json:"zh_cn"`
	En           string `json:"en"`
}

type ReportArchive struct {
	FormatVersion      int                `json:"format_version,omitempty"`
	MissingAttachments []string           `json:"missing_attachments,omitempty"`
	ID                 string             `json:"id"`
	Kind               string             `json:"kind"`
	SubjectID          string             `json:"subject_id"`
	Version            int                `json:"version"`
	SourceDigest       string             `json:"source_digest"`
	DocumentDigest     string             `json:"document_digest"`
	TranslationStatus  string             `json:"translation_status"`
	MissingLanguages   []string           `json:"missing_languages,omitempty"`
	Files              []ArtifactRef      `json:"files"`
	Attachments        []ReportAttachment `json:"attachments,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	SourceStatus       string             `json:"source_status,omitempty"`
}

// ReportAttachment pins an original artifact to the evidence cited by this version.
type ReportAttachment struct {
	EvidenceID string      `json:"evidence_id"`
	Artifact   ArtifactRef `json:"artifact"`
}

type reportSource struct {
	RunID        string                    `json:"run_id"`
	Kind         string                    `json:"kind"`
	SubjectID    string                    `json:"subject_id"`
	Goal         string                    `json:"goal"`
	RootContract *RootContract             `json:"root_contract,omitempty"`
	Business     string                    `json:"business_status"`
	Cleanup      string                    `json:"cleanup_status"`
	Subject      any                       `json:"subject"`
	Evidence     []reportSourceRef         `json:"evidence"`
	Children     []ChildReport             `json:"child_reports,omitempty"`
	Requirements []VerificationRequirement `json:"requirements,omitempty"`
	Checks       []VerificationCheck       `json:"checks,omitempty"`
	Findings     []Finding                 `json:"findings,omitempty"`
	Chains       []EvidenceChain           `json:"evidence_chains,omitempty"`
	ChainRefs    []EvidenceChainReference  `json:"evidence_chain_refs,omitempty"`
	Reproduction []string                  `json:"original_reproduction_steps,omitempty"`
	Attachments  []ReportAttachment        `json:"attachments,omitempty"`
}

type reportSourceRef struct {
	ID           string `json:"id"`
	Hash         string `json:"hash"`
	Verification string `json:"verification"`
	SourceHash   string `json:"source_hash,omitempty"`
}

type reportCandidate struct {
	format   int
	legacy   *reportCandidate
	source   reportSource
	text     *ReportText
	original string
}

func (c reportCandidate) digest() string { return valueHash(c.source) }

// Only semantic milestones produce documents. Tool stdout/stderr, directory
// listings and search results remain source records, never report bodies.
func reportCandidates(s *Snapshot) []reportCandidate {
	out := projectedReportCandidates(s, 1)
	if slices.ContainsFunc(s.Reports, func(r ReportArchive) bool { return r.FormatVersion == 0 }) {
		// Both projections enumerate the same semantic milestones in the same order.
		legacy := projectedReportCandidates(s, 0)
		for i := range out {
			out[i].legacy = &legacy[i]
		}
	}
	return out
}

func projectedReportCandidates(s *Snapshot, format int) []reportCandidate {
	var out []reportCandidate
	attachmentCache := map[string][]ReportAttachment{}
	attachmentsFor := func(id string) []ReportAttachment {
		if refs, ok := attachmentCache[id]; ok {
			return refs
		}
		refs := reportEvidenceAttachments(s, id)
		attachmentCache[id] = refs
		return refs
	}
	makeCandidate := func(kind, id, original string, subject any, body *ReportText, ids, steps []string) reportCandidate {
		c := reportCandidate{format: format, source: reportSource{RunID: s.Run.ID, Kind: kind, SubjectID: id, Goal: s.Run.Goal, Business: s.Result.Status, Cleanup: s.Result.CleanupStatus, Subject: subject, Reproduction: steps}, text: body, original: original}
		if format == 1 {
			ids = reportAttachmentEvidenceIDs(s, kind, id, original, body, ids, attachmentsFor)
		}
		for _, id := range ids {
			ref := reportSourceRef{ID: id, Verification: "missing"}
			if format == 1 {
				if o, ok := delegatedObservationFor(s, id); ok {
					ref.Hash, ref.SourceHash, ref.Verification = o.Reference.Hash, o.Reference.SourceHash, "verified"
				}
			}
			for _, e := range s.Evidence {
				if e.ID == id {
					ref.Hash, ref.Verification = e.ContentHash, e.Verification
					break
				}
			}
			c.source.Evidence = append(c.source.Evidence, ref)
			if format == 1 {
				c.source.Attachments = append(c.source.Attachments, attachmentsFor(id)...)
			}
		}
		return c
	}
	for _, f := range s.Findings {
		body := f.Report
		f.Report = nil
		f.UpdatedAt = time.Time{}
		original := strings.Join([]string{f.Title, f.RootCause, f.Impact, f.Remediation, strings.Join(f.ReproductionSteps, "\n")}, "\n\n")
		c := makeCandidate("finding", f.ID, original, f, body, f.EvidenceIDs, f.ReproductionSteps)
		// An unrelated later tool call does not create a new finding version.
		c.source.Business, c.source.Cleanup = f.Status, "not_applicable"
		out = append(out, c)
	}
	if s.WorkerCheckpoint != nil {
		cp := *s.WorkerCheckpoint
		body := cp.Report
		cp.Report = nil
		cp.CreatedAt = time.Time{}
		var structured WorkerHandoff
		if json.Unmarshal([]byte(cp.Summary), &structured) == nil && structured.NextAction != "" {
			cp.Summary = cp.Progress
		}
		c := makeCandidate("stage", "checkpoint", cp.Summary+"\n\n"+cp.Progress+"\n\n"+cp.NextAction, cp, body, cp.EvidenceIDs, nil)
		c.source.Business, c.source.Cleanup = "checkpoint", "not_checked"
		out = append(out, c)
	}
	if s.Run.Status == StatusCompleted || reportDeliveryPending(s) || s.Run.PauseReason == "report_translation_missing" || s.Run.Status == StatusFailed || s.Run.Status == StatusCancelled {
		r := s.Result
		body := r.Report
		r.Report = nil
		if r.Summary != "" || r.TerminalReason != "" || len(s.Findings) > 0 || len(s.ChildReports) > 0 {
			original := r.Summary
			if original == "" {
				original = r.TerminalReason
			}
			ids := append([]string(nil), r.EvidenceIDs...)
			if format == 1 {
				ids = append(ids, r.ArtifactEvidenceIDs...)
			}
			// Preserve source identities for natural-language reports too; raw
			// execution bytes stay in CAS, never in either document body.
			for _, ev := range s.Evidence {
				for _, action := range s.Actions {
					if action.RequestID == ev.RequestID && action.CallID == ev.CallID && (observationAction(action) || action.Name == "source_search" || action.Name == "artifact_register") && !hasString(ids, ev.ID) {
						ids = append(ids, ev.ID)
					}
				}
			}
			c := makeCandidate("final", "result", original, r, body, ids, r.ReproductionSteps)
			c.source.Children = ChildReportReceipts(*s, 0)
			for i := range c.source.Children {
				c.source.Children[i].ReportFiles = nil
				c.source.Children[i].ReportPath = ""
			}
			c.source.RootContract = s.RootContract
			c.source.Requirements, c.source.Checks = s.Requirements, s.Checks
			for _, f := range s.Findings {
				f.Report = nil
				f.UpdatedAt = time.Time{}
				c.source.Findings = append(c.source.Findings, f)
			}
			c.source.Chains, c.source.ChainRefs = s.EvidenceChains, s.EvidenceChainRefs
			out = append(out, c)
		}
	}
	for i := range out {
		c := &out[i]
		if c.text == nil || c.text.SourceDigest != "" {
			continue
		}
		body, _ := c.bodies()
		for _, archived := range s.Reports {
			if archived.Kind == c.source.Kind && archived.SubjectID == c.source.SubjectID && archived.DocumentDigest == valueHash(body) {
				copy := *c.text
				copy.SourceDigest = archived.SourceDigest
				c.text = &copy
				break
			}
		}
	}
	return out
}

// A language label or a translated heading is not translated factual prose.
// This detects missing/copy-only bodies, not semantic translation correctness.
func substantiveReportText(body, language string) bool {
	han, latin, fenced := 0, 0, false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || strings.HasPrefix(line, "#") {
			continue
		}
		for _, r := range line {
			if unicode.Is(unicode.Han, r) {
				han++
			}
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
				latin++
			}
		}
	}
	if language == "zh-CN" {
		return han >= 8
	}
	return latin >= 40 && han < max(1, latin/12)
}

func (c reportCandidate) bodies() (ReportText, []string) {
	var body ReportText
	if c.text != nil && (c.text.SourceDigest == "" || c.text.SourceDigest == c.digest()) {
		body = *c.text
		body.SourceDigest = ""
	}
	// Legacy original-language summaries remain usable, with the other
	// language explicitly pending. No text is relabelled as its translation.
	if strings.TrimSpace(body.ZhCN) == "" && substantiveReportText(c.original, "zh-CN") {
		body.ZhCN = c.original
	}
	if strings.TrimSpace(body.En) == "" && substantiveReportText(c.original, "en") {
		body.En = c.original
	}
	var missing []string
	if !substantiveReportText(body.ZhCN, "zh-CN") {
		missing = append(missing, "zh-CN")
		body.ZhCN = ""
	}
	if !substantiveReportText(body.En, "en") || strings.TrimSpace(body.ZhCN) == strings.TrimSpace(body.En) {
		missing = append(missing, "en")
		body.En = ""
	}
	return body, missing
}

func renderArchivedReport(c reportCandidate, r ReportArchive, language, body string) string {
	if r.FormatVersion == 0 {
		return renderLegacyArchivedReport(c, r, language, body)
	}
	body = reportImageBody(body, c.source.Attachments, language)
	var b strings.Builder
	if language == "zh-CN" {
		fmt.Fprintf(&b, "# 研究总结报告 · %s · v%d\n\n", c.source.Kind, r.Version)
		if body == "" {
			body = "本来源版本缺少实质中文译文，双语报告交付尚未齐备。"
		}
		fmt.Fprintf(&b, "%s\n\n## 来源与交付状态\n\n双语正文：%s；缺失：%s。正文由模型撰写，归档不新增执行证明，也不改变业务验收或清理结果。固定版本保留生成时状态，当前来源需另行核对。\n\n", body, r.TranslationStatus, strings.Join(r.MissingLanguages, ", "))
		fmt.Fprintf(&b, "来源运行 `%s`；报告类型 `%s`；业务状态 `%s`；清理状态 `%s`；引用证据 %d 条。\n\n", c.source.RunID, c.source.Kind, c.source.Business, c.source.Cleanup, len(c.source.Evidence))
	} else {
		fmt.Fprintf(&b, "# Research summary · %s · v%d\n\n", c.source.Kind, r.Version)
		if body == "" {
			body = "Substantive English translation is missing for this source version. Bilingual report delivery is incomplete."
		}
		fmt.Fprintf(&b, "%s\n\n## Source and delivery status\n\nBilingual content: %s; missing: %s. Model-authored prose and archiving do not prove execution, business acceptance or cleanup. This immutable version records the state at generation; current sources require a separate check.\n\n", body, r.TranslationStatus, strings.Join(r.MissingLanguages, ", "))
		fmt.Fprintf(&b, "Source run `%s`; report kind `%s`; business status `%s`; cleanup status `%s`; %d cited evidence records.\n\n", c.source.RunID, c.source.Kind, c.source.Business, c.source.Cleanup, len(c.source.Evidence))
	}
	if len(c.source.Reproduction) > 0 {
		if language == "zh-CN" {
			b.WriteString("## 复现步骤\n\n")
		} else {
			b.WriteString("## Reproduction\n\n")
		}
		for _, step := range c.source.Reproduction {
			fmt.Fprintf(&b, "    %s\n\n", strings.ReplaceAll(step, "\n", "\n    "))
		}
	}
	if len(c.source.Attachments) > 0 {
		if language == "zh-CN" {
			b.WriteString("## 截图与附件\n\n")
		} else {
			b.WriteString("## Screenshots and attachments\n\n")
		}
		for _, a := range c.source.Attachments {
			name := reportAttachmentLabel(a.Artifact)
			url := reportArtifactURL(a.Artifact.Hash)
			if reportImageExtension(a.Artifact.MIMEType) != "" && !reportBodyHasImage(body, url+"?preview=1") {
				fmt.Fprintf(&b, "![%s](%s?preview=1)\n\n", name, url)
			}
			fmt.Fprintf(&b, "- [%s](%s) · `%s`\n\n", name, url, a.EvidenceID)
		}
	}
	if language == "zh-CN" {
		b.WriteString("## 来源引用\n\n")
	} else {
		b.WriteString("## Source references\n\n")
	}
	fmt.Fprintf(&b, "Source: `%s`\n\nDocument: `%s`\n\n", r.SourceDigest, r.DocumentDigest)
	for _, e := range c.source.Evidence {
		fmt.Fprintf(&b, "- `%s` · `%s` · %s\n", e.ID, e.Hash, e.Verification)
	}
	for _, child := range c.source.Children {
		fmt.Fprintf(&b, "- Worker `%s` · visit %d · `%s` · %s / %s\n", child.SourceRunID, child.Visit, child.SourceHash, child.ResultStatus, child.CleanupStatus)
	}

	return b.String()
}

func (store *Store) archiveReports(s *Snapshot) error {
	if s.Run.Deleted {
		return nil
	}
	archiveConclusionChains(s)
	applyReportTranslations(s)
	for _, c := range reportCandidates(s) {
		body, missing := c.bodies()
		digest, document := c.digest(), valueHash(body)
		version, exists := 1, false
		for _, old := range s.Reports {
			if old.Kind != c.source.Kind || old.SubjectID != c.source.SubjectID {
				continue
			}
			version = max(version, old.Version+1)
			if old.FormatVersion == 1 && old.SourceDigest == digest && old.DocumentDigest == document ||
				old.FormatVersion == 0 && c.legacy.matchesDocument(old) {
				exists = true
			}
		}
		if exists {
			continue
		}
		r := ReportArchive{FormatVersion: 1, MissingAttachments: reportMissingImages(body, c.source.Attachments), Kind: c.source.Kind, SubjectID: c.source.SubjectID, Version: version, SourceDigest: digest, DocumentDigest: document, TranslationStatus: "complete", MissingLanguages: missing, Attachments: c.source.Attachments, CreatedAt: time.Now().UTC()}
		if len(missing) > 0 {
			r.TranslationStatus = "translation_missing"
		}
		r.ID = "report-" + valueHash([]string{r.Kind, r.SubjectID, digest, document})[:24]
		for _, localized := range []struct{ language, body string }{{"zh-CN", body.ZhCN}, {"en", body.En}} {
			content := renderArchivedReport(c, r, localized.language, localized.body)
			ref, err := store.PutArtifact("report", []byte(content))
			if err != nil {
				return fmt.Errorf("archive report: %w", err)
			}
			ref.Name = fmt.Sprintf("trace-%s-%s-%s-v%d.%s.md", s.Run.ID, r.Kind, valueHash(r.SubjectID)[:10], version, localized.language)
			ref.MIMEType, ref.Purpose = "text/markdown; charset=utf-8", "source-bound summary report; "+r.TranslationStatus
			r.Files = append(r.Files, ref)
		}
		s.Reports = append(s.Reports, r)
	}
	return nil
}

// File availability is a view, not a change to the saved receipt. OpenArtifact
// checks the selected digest at download time.
func reportFiles(s *Snapshot, files []ArtifactRef) []ArtifactRef {
	if s.artifactRoot == "" {
		return files
	}
	dir := os.DirFS(s.artifactRoot)
	return slices.DeleteFunc(slices.Clone(files), func(ref ArtifactRef) bool {
		info, err := fs.Stat(dir, "artifacts/"+ref.Hash)
		return err != nil || !info.Mode().IsRegular()
	})
}

// A view may mark a version changed, but never rewrites its archived bytes.
func ReportReceipts(s Snapshot) []ReportArchive {
	out := append([]ReportArchive{}, s.Reports...)
	if len(out) == 0 {
		return out
	}
	current := map[string][2]string{}
	for _, c := range reportCandidates(&s) {
		digests := [2]string{1: c.digest()}
		if c.legacy != nil {
			digests[0] = c.legacy.digest()
		}
		current[c.source.Kind+"/"+c.source.SubjectID] = digests
	}
	// The archive pins child receipts, not their live availability. A newer
	// settlement supersedes an early receipt from the same child visit.
	seen := map[string]bool{}
	for i := len(s.ChildReports) - 1; i >= 0; i-- {
		child := s.ChildReports[i]
		if !child.Current || seen[child.SourceRunID] {
			continue
		}
		seen[child.SourceRunID] = true
		if child.SourceStatus == "changed" || child.SourceStatus == "unavailable" {
			delete(current, "final/result")
			break
		}
	}
	for i := range out {
		out[i].Files = reportFiles(&s, out[i].Files)
		out[i].SourceStatus = "changed"
		format := out[i].FormatVersion
		if digests, exists := current[out[i].Kind+"/"+out[i].SubjectID]; exists && (format == 0 || format == 1) && digests[format] == out[i].SourceDigest {
			out[i].SourceStatus = "current"
		}
	}
	return out
}

// A later finding translation must not replace the final-summary download.
// Prefer the current delivery receipt, then the newest current or historical document.
func preferredReport(s Snapshot) *ReportArchive {
	receipts := ReportReceipts(s)
	candidates := deliveryReportCandidates(&s)
	for _, kind := range []string{"final", "stage", "finding"} {
		// A partial Format1 archive may follow a complete Format0 receipt for
		// the same source. Select the receipt used by delivery before falling
		// back to source-only ordering, including for historical documents.
		for i := len(receipts) - 1; i >= 0; i-- {
			r := &receipts[i]
			if len(r.Files) == 0 || r.Kind != kind || r.SourceStatus != "current" || r.TranslationStatus != "complete" {
				continue
			}
			for _, c := range candidates {
				if c.matchesDocument(*r) {
					return r
				}
			}
		}
		for _, current := range []bool{true, false} {
			for i := len(receipts) - 1; i >= 0; i-- {
				r := &receipts[i]
				if len(r.Files) != 0 && r.Kind == kind && (!current || r.SourceStatus == "current") {
					return r
				}
			}
		}
	}
	return nil
}

// A pinned document is historical: reading it must not refresh or write the Run.
// Only an unpinned request with existing receipts needs current source selection.
// Muteki reports.py get (a73535a19fd2) separates frozen reads from freeze;
// pi exportFromFile (1f6f319bb289) uses saved entries; ARTEX ReportTab
// (e6ec56999175) exposes only existing content. Trace selects existing CAS
// files and rechecks sources through its own Store, without a separate export store.
func (store *Store) documentSnapshot(id, reportID string) (Snapshot, error) {
	unlock, err := store.acquireStoreLock()
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	s, err := store.loadRawLocked(id)
	if err != nil || s.Run.Deleted || reportID != "" {
		return s, err
	}
	for _, r := range s.Reports {
		if len(reportFiles(&s, r.Files)) != 0 {
			// Select among existing files. Do not commit a new version merely
			// because the current source no longer matches the saved result.
			store.verifyDerivedLocked(&s, map[string]bool{id: true}, readSourceOnly)
			s.ChildReports = store.childReportsFromLocked(s, 0)
			break
		}
	}
	return s, nil
}

func (store *Store) ReportDocument(id, reportID, language string) (ArtifactRef, string, error) {
	if language == "" {
		language = "zh-CN"
	}
	if language != "zh-CN" && language != "en" {
		return ArtifactRef{}, "", fmt.Errorf("language must be zh-CN or en")
	}
	s, err := store.documentSnapshot(id, reportID)
	if err != nil {
		return ArtifactRef{}, "", err
	}
	if s.Run.Deleted {
		return ArtifactRef{}, "", os.ErrNotExist
	}
	if reportID == "" {
		selected := preferredReport(s)
		if selected == nil {
			return ArtifactRef{}, "", fmt.Errorf("no saved summary report is available (recorded run=%s, pause_reason=%q, business=%s, cleanup=%s)", s.Run.Status, s.Run.PauseReason, s.Result.Status, s.Result.CleanupStatus)
		}
		if selected.SourceStatus != "current" {
			return ArtifactRef{}, "", fmt.Errorf("no saved summary report matches the current source; select a historical report_id")
		}
		reportID = selected.ID
	}
	for i := len(s.Reports) - 1; i >= 0; i-- {
		r := s.Reports[i]
		if r.ID != reportID {
			continue
		}
		for _, ref := range r.Files {
			if !strings.HasSuffix(ref.Name, "."+language+".md") {
				continue
			}
			f, err := store.OpenArtifact(ref.Hash)
			if err != nil {
				return ArtifactRef{}, "", err
			}
			data, err := io.ReadAll(f)
			f.Close()
			return ref, string(data), err
		}
	}
	return ArtifactRef{}, "", fmt.Errorf("report version: %w", os.ErrNotExist)
}
