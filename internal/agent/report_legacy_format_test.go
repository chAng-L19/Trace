package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Seed a pre-format-version receipt directly: archiving through the current
// writer would create format 1 and would never exercise legacy CAS validation.
func legacyReportFormatFixture(t *testing.T, backend string) (*Store, Snapshot, map[string]string) {
	t.Helper()
	store, s := u7Store(t, backend)
	picture, err := store.PutArtifact("file", []byte{137, 80, 78, 71, 13, 10, 26, 10})
	if err != nil {
		t.Fatal(err)
	}
	picture.Name = "legacy.png"
	resultValidationSource(t, &s, "picture", "artifact_register", "builtin", "registered", picture)
	text := u7Text
	text.En += "\n\n![registered](" + reportArtifactURL(picture.Hash) + "?preview=1)"
	resultValidationClaim(t, &s, Result{Status: "failed", Summary: text.ZhCN, Report: &text})
	// Independent writer-side oracle for this final receipt: the registered
	// action contributes one local ref, but neither attachment metadata nor a
	// source_hash was serialized by the accepted 2026-11-22 writer.
	rawResult := s.Result
	rawResult.Report = nil
	c := reportCandidate{text: &text, original: rawResult.Summary, source: reportSource{
		RunID: s.Run.ID, Kind: "final", SubjectID: "result", Goal: s.Run.Goal, RootContract: s.RootContract,
		Business: rawResult.Status, Cleanup: rawResult.CleanupStatus, Subject: rawResult, Reproduction: rawResult.ReproductionSteps,
		Evidence: []reportSourceRef{{ID: "picture", Hash: s.Evidence[0].ContentHash, Verification: "verified"}},
	}}
	body, missing := c.bodies()
	if len(missing) != 0 {
		t.Fatalf("fixture lacks bilingual prose: %v", missing)
	}
	r := ReportArchive{Kind: c.source.Kind, SubjectID: c.source.SubjectID, Version: 1, SourceDigest: c.digest(), DocumentDigest: valueHash(body), TranslationStatus: "complete", CreatedAt: s.Run.CreatedAt}
	r.ID = "report-" + valueHash([]string{r.Kind, r.SubjectID, r.SourceDigest, r.DocumentDigest})[:24]
	documents := map[string]string{}
	for _, localized := range []struct{ language, body string }{{"zh-CN", body.ZhCN}, {"en", body.En}} {
		content := renderLegacyArchivedReport(c, r, localized.language, localized.body)
		if !strings.Contains(content, "```json\n") || !strings.Contains(content, `"source_digest":`) {
			t.Fatal("fixture no longer contains the original citation manifest")
		}
		ref, err := store.PutArtifact("report", []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		ref.Name, ref.MIMEType = fmt.Sprintf("legacy.%s.md", localized.language), "text/markdown; charset=utf-8"
		r.Files = append(r.Files, ref)
		documents[localized.language] = content
	}
	s.Reports = []ReportArchive{r}
	// Raw persistence retains the historical receipt without rederiving Result.
	if err := store.savePreparedLocked(s); err != nil {
		t.Fatal(err)
	}
	return store, s, documents
}

func TestLegacyReportFormatReceiptJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, s, documents := legacyReportFormatFixture(t, backend)
			s, err := store.loadRawLocked(s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			old := s.Reports[0]
			raw, err := json.Marshal(old)
			if err != nil || old.FormatVersion != 0 || strings.Contains(string(raw), "format_version") {
				t.Fatalf("historical receipt gained a format field: %s, %v", raw, err)
			}
			a := &Agent{store: store}
			if reportDeliveryIncomplete(&s) || !a.reportFilesComplete(&s) || continueForReport(&s, true) {
				t.Fatal("complete legacy bilingual CAS files became pending delivery")
			}
			s, err = store.Update(s.Run.ID, func(*Snapshot) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Reports) != 1 || !reflect.DeepEqual(s.Reports[0], old) {
				t.Fatal("unchanged legacy source was rearchived or its receipt rewritten")
			}
			for language, expected := range documents {
				_, current, err := store.ReportDocument(s.Run.ID, "", language)
				if err != nil || current != expected {
					t.Fatalf("current legacy %s download changed: %v", language, err)
				}
				ref, actual, err := store.ReportDocument(s.Run.ID, old.ID, language)
				if err != nil || actual != expected || ref.Hash != promptHash(expected) || ref.Bytes != int64(len(expected)) {
					t.Fatalf("legacy report_id bytes changed for %s: %v", language, err)
				}
			}
			// A new source gets the new format; the historical report_id still
			// returns the exact original document, including its JSON manifest.
			s.Run.Goal += " 新来源版本"
			fresh := u7Text
			fresh.ZhCN += " 这是新来源的报告正文。"
			fresh.En += " This prose belongs to the new source version."
			s.Result.Report = &fresh
			if err := store.archiveReports(&s); err != nil {
				t.Fatal(err)
			}
			if len(s.Reports) != 2 || s.Reports[1].FormatVersion != 1 || !reflect.DeepEqual(s.Reports[0], old) {
				t.Fatalf("new format replaced the legacy receipt: %+v", s.Reports)
			}
			if err := store.savePreparedLocked(s); err != nil {
				t.Fatal(err)
			}
			if !a.reportFilesComplete(&s) {
				t.Fatal("current format-1 files failed delivery after a legacy version")
			}
			for language, expected := range documents {
				_, actual, err := store.ReportDocument(s.Run.ID, old.ID, language)
				if err != nil || actual != expected {
					t.Fatalf("new source changed historical %s document: %v", language, err)
				}
			}
		})
	}
}

func TestLegacyReportMixedFormatDefaultJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, s, documents := legacyReportFormatFixture(t, backend)
			legacy := s.Reports[0]
			// Reproduce the old bug's persisted history: the Format1 source is
			// current, but the prose is still bound to the Format0 digest.
			c := reportCandidates(&s)[0]
			body, missing := c.bodies()
			if len(missing) == 0 {
				t.Fatal("mixed-history fixture must contain a missing translation")
			}
			partial := ReportArchive{FormatVersion: 1, ID: "legacy-bug-partial", Kind: c.source.Kind, SubjectID: c.source.SubjectID, Version: 2, SourceDigest: c.digest(), DocumentDigest: valueHash(body), TranslationStatus: "translation_missing", MissingLanguages: missing, Attachments: c.source.Attachments, CreatedAt: s.Run.CreatedAt}
			for _, localized := range []struct{ language, body string }{{"zh-CN", body.ZhCN}, {"en", body.En}} {
				ref, err := store.PutArtifact("report", []byte(renderArchivedReport(c, partial, localized.language, localized.body)))
				if err != nil {
					t.Fatal(err)
				}
				ref.Name = fmt.Sprintf("partial.%s.md", localized.language)
				partial.Files = append(partial.Files, ref)
			}
			s.Reports = append(s.Reports, partial)
			if err := store.savePreparedLocked(s); err != nil {
				t.Fatal(err)
			}
			for _, receipt := range ReportReceipts(s) {
				if receipt.SourceStatus != "current" {
					t.Fatal("fixture must have both Format0 and Format1 current sources")
				}
			}
			if reportDeliveryIncomplete(&s) || !(&Agent{store: store}).reportFilesComplete(&s) {
				t.Fatal("mixed history lost complete legacy delivery")
			}
			fixedRef, fixed, err := store.ReportDocument(s.Run.ID, legacy.ID, "en")
			if err != nil || fixed != documents["en"] {
				t.Fatalf("legacy English fixture changed: %v", err)
			}
			currentRef, current, err := store.ReportDocument(s.Run.ID, "", "en")
			if err != nil || current != fixed || !reflect.DeepEqual(currentRef, fixedRef) {
				t.Fatalf("default English did not select complete legacy bytes: %v", err)
			}
			// A complete Format1 for the same business source takes precedence.
			text := u7Text
			text.En += " The new complete document supersedes the legacy report."
			resultValidationClaim(t, &s, Result{Status: "failed", Summary: s.Result.Summary, Report: &text})
			if err := store.archiveReports(&s); err != nil {
				t.Fatal(err)
			}
			if len(s.Reports) != 3 || s.Reports[2].FormatVersion != 1 || s.Reports[2].TranslationStatus != "complete" || s.Reports[2].SourceDigest != partial.SourceDigest {
				t.Fatalf("expected complete Format1 for unchanged business source: %+v", s.Reports)
			}
			if err := store.savePreparedLocked(s); err != nil {
				t.Fatal(err)
			}
			fixedRef, fixed, err = store.ReportDocument(s.Run.ID, s.Reports[2].ID, "en")
			if err != nil {
				t.Fatal(err)
			}
			currentRef, current, err = store.ReportDocument(s.Run.ID, "", "en")
			if err != nil || current != fixed || !reflect.DeepEqual(currentRef, fixedRef) || current == documents["en"] {
				t.Fatalf("default English did not prefer complete Format1: %v", err)
			}
			s.Run.Goal += " changed source"
			if selected := preferredReport(s); selected == nil || selected.ID != s.Reports[2].ID || selected.SourceStatus != "changed" {
				t.Fatal("historical fallback no longer selects the newest final report")
			}
		})
	}
}

func TestLegacyReportFormatRejectsInvalidReceiptJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, original, _ := legacyReportFormatFixture(t, backend)
			a := &Agent{store: store}
			for _, tc := range []struct {
				name   string
				mutate func(*ReportArchive)
			}{
				{"missing_language", func(r *ReportArchive) { r.Files = r.Files[:1] }},
				{"wrong_bytes", func(r *ReportArchive) { r.Files[1].Bytes++ }},
				{"wrong_hash", func(r *ReportArchive) { r.Files[1].Hash = strings.Repeat("b", 64) }},
				{"wrong_format", func(r *ReportArchive) { r.FormatVersion = 1 }},
				{"unknown_format", func(r *ReportArchive) { r.FormatVersion = 2 }},
				{"wrong_source", func(r *ReportArchive) { r.SourceDigest += "changed" }},
				{"wrong_document", func(r *ReportArchive) { r.DocumentDigest += "changed" }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					s := original
					s.Reports = append([]ReportArchive(nil), original.Reports...)
					s.Reports[0].Files = append([]ArtifactRef(nil), original.Reports[0].Files...)
					tc.mutate(&s.Reports[0])
					if a.reportFilesComplete(&s) {
						t.Fatal("legacy compatibility accepted an invalid receipt")
					}
				})
			}
		})
	}
}

// A qualified ID was looked up literally in the local ledger by Format0;
// ArtifactEvidenceIDs remained in the result subject, not the ref list.
func TestLegacyReportQualifiedProjection(t *testing.T) {
	_, s := u7Store(t, "json")
	s.Run.Status, s.Run.Responsibility = StatusCompleted, ResponsibilityCoordinator
	qualified := "child/picture"
	s.delegatedAcceptance = map[string]delegatedObservation{qualified: {Reference: EvidenceReference{ID: qualified, Hash: "content", SourceHash: "binding"}}}
	s.Result = Result{Status: "failed", Summary: "summary", EvidenceIDs: []string{qualified}, ArtifactEvidenceIDs: []string{"extra"}}
	s.Reports = []ReportArchive{{Kind: "final", SubjectID: "result"}}
	c := reportCandidates(&s)[0]
	if !reflect.DeepEqual(c.legacy.source.Evidence, []reportSourceRef{{ID: qualified, Verification: "missing"}}) ||
		!reflect.DeepEqual(c.source.Evidence, []reportSourceRef{{ID: qualified, Hash: "content", Verification: "verified", SourceHash: "binding"}, {ID: "extra", Verification: "missing"}}) {
		t.Fatalf("wrong evidence projections: old=%+v new=%+v", c.legacy.source.Evidence, c.source.Evidence)
	}
	s.Reports[0].SourceDigest = c.digest()
	if ReportReceipts(s)[0].SourceStatus == "current" {
		t.Fatal("Format0 accepted a digest from another format")
	}
	s.Reports[0].SourceDigest = c.legacy.digest()
	s.Result.ArtifactEvidenceIDs = nil
	if ReportReceipts(s)[0].SourceStatus == "current" {
		t.Fatal("Format0 stopped hashing ArtifactEvidenceIDs in its result subject")
	}
}

func TestLegacyReportRegisteredArtifactRevocationJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, s, documents := legacyReportFormatFixture(t, backend)
			ref := s.Actions[0].Result.Artifacts[0]
			if err := os.WriteFile(filepath.Join(store.root, "artifacts", ref.Hash), make([]byte, ref.Bytes), 0600); err != nil {
				t.Fatal(err)
			}
			view, err := store.documentSnapshot(s.Run.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if ReportReceipts(view)[0].SourceStatus == "current" || (&Agent{store: store}).reportFilesComplete(&view) {
				t.Fatal("tampered registered artifact retained current delivery")
			}
			if _, _, err := store.ReportDocument(s.Run.ID, "", "en"); err == nil {
				t.Fatal("current download accepted revoked evidence")
			}
			if _, body, err := store.ReportDocument(s.Run.ID, s.Reports[0].ID, "en"); err != nil || body != documents["en"] {
				t.Fatalf("revocation changed historical document: %v", err)
			}
		})
	}
}

func TestReportFormatOneHasNoManifestJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, s := u7Store(t, backend)
			text := u7Text
			s.Run.Status = StatusCompleted
			s.Result = Result{Status: "failed", Summary: "本次检查未达到目标，保留已经观察到的事实。", CleanupStatus: "unknown", Report: &text}
			if err := store.archiveReports(&s); err != nil {
				t.Fatal(err)
			}
			if len(s.Reports) != 1 || s.Reports[0].FormatVersion != 1 || len(s.Reports[0].Files) != 2 {
				t.Fatalf("new archive lacks explicit format 1 or bilingual files: %+v", s.Reports)
			}
			if err := store.savePreparedLocked(s); err != nil {
				t.Fatal(err)
			}
			for _, localized := range []struct{ language, body string }{{"zh-CN", text.ZhCN}, {"en", text.En}} {
				_, actual, err := store.ReportDocument(s.Run.ID, s.Reports[0].ID, localized.language)
				if err != nil || !strings.Contains(actual, localized.body) {
					t.Fatalf("format-1 prose missing for %s: %v", localized.language, err)
				}
				if strings.Contains(actual, "```json\n") || strings.Contains(actual, `"source_digest":`) || strings.Contains(actual, `"document_digest":`) {
					t.Fatal("format-1 document still dumps a citation manifest")
				}
			}
			a := &Agent{store: store}
			if reportDeliveryIncomplete(&s) || !a.reportFilesComplete(&s) {
				t.Fatal("format-1 CAS files failed delivery validation")
			}
		})
	}
}
