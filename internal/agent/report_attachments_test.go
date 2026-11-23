package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reportPNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func readReportZIP(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name] = b
	}
	return files
}

func reportBundleTestBytes(t *testing.T, store *Store, id, reportID, language string) (string, []byte, error) {
	t.Helper()
	name, file, err := store.ReportBundle(id, reportID, language)
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	data, err := io.ReadAll(file)
	return name, data, err
}

func TestReportScreenshotArchiveAndBundleJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			data := reportPNG(t)
			path := filepath.Join(t.TempDir(), "步骤截图.png")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(ArtifactRegistration{Path: path, Name: "步骤截图.png", Purpose: "report screenshot"})
			a, store := responsibilityAgent(t, backend, manualProvider(ToolCall{ID: "capture", Name: "artifact_register", Arguments: string(args)}))
			id := createTestRun(t, a)
			if _, err := a.Run(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			s, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			evidenceID := s.Evidence[0].ID
			claim := Result{Status: "failed", Summary: u7Text.ZhCN, Report: &u7Text, ArtifactEvidenceIDs: []string{evidenceID}, ReproductionSteps: []string{"capture actual output"}}
			raw, _ := json.Marshal(map[string]any{"trace_result": claim})
			s, err = store.Update(id, func(s *Snapshot) error {
				s.Run.Status = StatusCompleted
				s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			receipt := s.Reports[len(s.Reports)-1]
			if len(receipt.Attachments) != 1 || receipt.Attachments[0].EvidenceID != evidenceID {
				t.Fatalf("attachments lost: %+v", receipt)
			}
			ref := receipt.Attachments[0].Artifact
			view := s
			findingText := u7Text
			findingText.ZhCN += "\n![步骤](" + reportArtifactURL(ref.Hash) + "?preview=1)"
			view.Findings = []Finding{{ID: "screenshot-finding", Title: "Finding screenshot", Report: &findingText}}
			for _, c := range reportCandidates(&view) {
				if c.source.Kind == "finding" && (len(c.source.Attachments) != 1 || c.source.Attachments[0].EvidenceID != evidenceID) {
					t.Fatal("finding prose lost registered screenshot")
				}
			}
			for _, c := range reportCandidates(&view) {
				if c.source.Kind != "finding" {
					continue
				}
				digest := c.digest()
				view.Reports = append(view.Reports, ReportArchive{Kind: "finding", SubjectID: c.source.SubjectID, SourceDigest: digest, Attachments: c.source.Attachments})
				translated := u7Text
				translated.SourceDigest = digest
				view.Findings[0].Report = &translated
				for _, next := range reportCandidates(&view) {
					if next.source.Kind == "finding" && (next.digest() != digest || len(next.source.Attachments) != 1) {
						t.Fatal("translation without duplicate image markup lost source attachments")
					}
				}
			}
			view.Reports = s.Reports
			view.Findings[0].Report = &findingText
			unregistered, err := store.PutArtifact("file", []byte("unrelated CAS file"))
			if err != nil {
				t.Fatal(err)
			}
			findingText.ZhCN = u7Text.ZhCN + "\n![unregistered](" + reportArtifactURL(unregistered.Hash) + "?preview=1)"
			for _, c := range reportCandidates(&view) {
				if c.source.Kind == "finding" {
					if len(c.source.Attachments) != 0 {
						t.Fatal("unregistered CAS reference attached to finding")
					}
					if len(reportMissingImages(findingText, c.source.Attachments)) != 1 {
						t.Fatal("missing screenshot was not recorded")
					}
					rendered := renderArchivedReport(c, ReportArchive{FormatVersion: 1}, "zh-CN", findingText.ZhCN)
					if strings.Contains(rendered, reportArtifactURL(unregistered.Hash)) || !strings.Contains(rendered, "截图未交付") {
						t.Fatal("unbound screenshot still rendered or gap hidden")
					}
				}
			}
			_, online, err := store.ReportDocument(id, receipt.ID, "zh-CN")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(online, "![步骤截图.png]("+reportArtifactURL(ref.Hash)+"?preview=1)") || strings.Contains(online, "```json") {
				t.Fatalf("report is not prose + screenshot: %s", online)
			}
			for _, language := range []string{"zh-CN", "en"} {
				name, data, err := reportBundleTestBytes(t, store, id, receipt.ID, language)
				if err != nil {
					t.Fatal(err)
				}
				files := readReportZIP(t, data)
				asset := "assets/" + ref.Hash + ".png"
				if !strings.HasSuffix(name, "."+language+".zip") || !bytes.Equal(files[asset], reportPNG(t)) {
					t.Fatal("ZIP omitted original screenshot bytes")
				}
				report := string(files["report."+language+".md"])
				if !strings.Contains(report, "]("+asset+")") || strings.Contains(report, "/api/artifacts/") {
					t.Fatal("ZIP screenshot does not resolve offline")
				}
				var manifest struct {
					Report ReportArchive `json:"report"`
				}
				if json.Unmarshal(files["manifest.json"], &manifest) != nil || manifest.Report.ID != receipt.ID {
					t.Fatal("independent manifest lost fixed receipt")
				}
			}
			if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Goal += " changed"; return nil }); err != nil {
				t.Fatal(err)
			}
			_, fixed, err := store.ReportDocument(id, receipt.ID, "zh-CN")
			if err != nil || fixed != online {
				t.Fatal("historical document bytes changed", err)
			}
			if _, _, err = reportBundleTestBytes(t, store, id, receipt.ID, "zh-CN"); err != nil {
				t.Fatal("historical screenshot bundle lost", err)
			}
			if err = os.WriteFile(filepath.Join(store.root, "artifacts", ref.Hash), []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err = reportBundleTestBytes(t, store, id, receipt.ID, "zh-CN"); err == nil {
				t.Fatal("corrupt screenshot exported")
			}
		})
	}
}

func TestReportDelegatedArtifactSourceBindingJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			f := delegatedArtifactFixture(t, backend)
			delegatedRootCheck(t, f)
			artifact := f.worker.RunID + "/" + f.artifactID
			s, err := f.store.Update(f.root, func(s *Snapshot) error {
				resultValidationClaim(t, s, Result{Status: "success", Summary: u7Text.ZhCN, Report: &u7Text, EvidenceIDs: []string{f.qualified(0)}, ArtifactEvidenceIDs: []string{artifact}})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			candidates := reportCandidates(&s)
			var final reportCandidate
			for _, c := range candidates {
				if c.source.Kind == "final" {
					final = c
				}
			}
			if len(final.source.Attachments) != 1 || final.source.Attachments[0].EvidenceID != artifact {
				t.Fatalf("Worker attachment lost: %+v", final.source)
			}
			found := false
			for _, ref := range final.source.Evidence {
				if ref.ID == artifact {
					found = ref.Verification == "verified" && ref.SourceHash != ""
				}
			}
			if !found {
				t.Fatal("Worker attachment unbound from settled source")
			}
			if len(s.Actions) != 0 || len(s.Evidence) != 0 {
				t.Fatal("child ledger copied into coordinator")
			}
			child, err := f.store.Load(f.worker.RunID)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range child.Actions {
				if a.CallID == "deliver" {
					if err = os.WriteFile(filepath.Join(f.store.root, "artifacts", a.Result.Artifacts[0].Hash), []byte("corrupt"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			s, err = f.store.Load(f.root)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range reportCandidates(&s) {
				if c.source.Kind == "final" && len(c.source.Attachments) != 0 {
					t.Fatal("revoked Worker attachment still live")
				}
			}
		})
	}
}
func TestReportReadableUsagePreservesLegacyFixedBytes(t *testing.T) {
	s := Snapshot{}
	s.Run.Usage.ModelRequests = 73
	raw, err := json.MarshalIndent(s.Run.Usage, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fixed := renderReportSnapshot(&s, nil, nil)
	if !strings.HasSuffix(fixed, "\n## 用量记录\n\n```json\n"+string(raw)+"\n```\n") {
		t.Fatal("legacy source-hash renderer changed usage bytes")
	}
	readable := renderReportSnapshotView(&s, nil, nil, true)
	if strings.Contains(readable, "```json") || !strings.Contains(readable, "| 模型请求 / 响应 | 73 / 0 |") || !strings.Contains(readable, "未记录") {
		t.Fatal("current report did not use readable usage with unknown tokens")
	}
}
func TestReportMissingImageDeliveryFeedback(t *testing.T) {
	store, s := u7Store(t, "json")
	body := u7Text
	body.ZhCN += "\n![missing](/api/artifacts/" + strings.Repeat("0", 64) + "/download?preview=1)"
	claim := Result{Status: "failed", Summary: body.ZhCN, Report: &body}
	raw, _ := json.Marshal(map[string]any{"trace_result": claim})
	var err error
	s, err = store.Update(s.Run.ID, func(s *Snapshot) error {
		s.Run.Status = StatusCompleted
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reportDeliveryIncomplete(&s) {
		t.Fatal("broken screenshot treated as fully delivered")
	}
	if len(s.Reports) == 0 || len(s.Reports[len(s.Reports)-1].MissingAttachments) != 1 {
		t.Fatal("missing screenshot absent from receipt")
	}
	if !continueForReport(&s) || !strings.Contains(s.Messages[len(s.Messages)-1].Content, "missing_attachments") {
		t.Fatal("missing image did not receive report-only repair feedback")
	}
	if s.Result.Status != "failed" {
		t.Fatal("report image repair changed business result")
	}
}

func TestReportImageExamplesDoNotSuppressActualAttachment(t *testing.T) {
	url := "/api/artifacts/" + strings.Repeat("a", 64) + "/download?preview=1"
	image := "![step](" + url + ")"
	for _, body := range []string{"[download](" + url + ")", "`" + image + "`", "```md\n" + image + "\n```", "``" + image + "``"} {
		if reportBodyHasImage(body, url) {
			t.Fatal("code/link treated as displayed image")
		}
		if len(reportMissingImages(ReportText{ZhCN: body}, nil)) != 0 {
			t.Fatal("code/link treated as missing screenshot")
		}
	}
	if !reportBodyHasImage(image, url) {
		t.Fatal("actual image not detected")
	}
}

func reportCodeExample(kind string) string {
	image := "![example](/api/artifacts/" + strings.Repeat("0", 64) + "/download?preview=1)"
	tick3 := strings.Repeat(string(rune(96)), 3)
	switch kind {
	case "four_backticks":
		return strings.Join([]string{tick3 + string(rune(96)) + "markdown", tick3 + "markdown", image, tick3, tick3 + string(rune(96))}, "\n")
	case "quoted_fence":
		return strings.Join([]string{"> " + tick3 + "markdown", "> " + image, "> " + tick3}, "\n")
	case "table":
		return "| literal | example |\n| --- | --- |\n| " + string(rune(96)) + " | " + string(rune(96)) + image + string(rune(96)) + " |"
	case "ordinary_fence":
		return strings.Join([]string{tick3 + "markdown", image, tick3}, "\n")
	}
	return image
}
func TestReportCodeExamplesExcludeScreenshots(t *testing.T) {
	for _, kind := range []string{"ordinary_fence", "four_backticks", "quoted_fence", "table", "actual_image"} {
		t.Run(kind, func(t *testing.T) {
			missing := reportMissingImages(ReportText{ZhCN: reportCodeExample(kind)}, nil)
			want := 0
			if kind == "actual_image" {
				want = 1
			}
			t.Logf("kind=%s missing=%v", kind, missing)
			if len(missing) != want {
				t.Fatalf("code example classified as real screenshot: got %d missing, want %d", len(missing), want)
			}
		})
	}
}
func TestReportCodeExamplesDoNotContinueDelivery(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, s := u7Store(t, backend)
			body := u7Text
			body.ZhCN += "\n\n" + reportCodeExample("quoted_fence")
			claim := Result{Status: "failed", Summary: u7Text.ZhCN, Report: &body}
			raw, _ := json.Marshal(map[string]any{"trace_result": claim})
			s, err := store.Update(s.Run.ID, func(s *Snapshot) error {
				s.Run.Status = StatusCompleted
				s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			incomplete := reportDeliveryIncomplete(&s)
			continued := continueForReport(&s)
			t.Logf("backend=%s report_delivery_incomplete=%v report_only_continued=%v", backend, incomplete, continued)
			if incomplete || continued {
				t.Fatal("complete bilingual report with code example entered screenshot repair")
			}
		})
	}
}

func reportSelectionSnapshot(t *testing.T, backend string) (*Store, Snapshot, ArtifactRef) {
	store, s := u7Store(t, backend)
	ref, err := store.PutArtifact("file", reportPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	ref.Name = "unrelated.png"
	resultValidationSource(t, &s, "picture", "artifact_register", "builtin", "registered", ref)
	return store, s, ref
}
func TestReportFindingCodeDoesNotSelectAttachment(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			for _, context := range []string{"inline", "fence", "long_fence", "quote", "table", "heading", "empty_label", "code_label", "link", "image"} {
				t.Run(context, func(t *testing.T) {
					store, s, ref := reportSelectionSnapshot(t, backend)
					link := "[download example](" + reportArtifactURL(ref.Hash) + ")"
					tick := string(rune(96))
					prose := link
					switch context {
					case "inline":
						prose = tick + link + tick
					case "fence":
						prose = strings.Repeat(tick, 3) + "md\n" + link + "\n" + strings.Repeat(tick, 3)
					case "long_fence":
						prose = strings.Repeat(tick, 4) + "md\n" + strings.Repeat(tick, 3) + "md\n" + link + "\n" + strings.Repeat(tick, 3) + "\n" + strings.Repeat(tick, 4)
					case "quote":
						prose = "> " + strings.Repeat(tick, 3) + "md\n> " + link + "\n> " + strings.Repeat(tick, 3)
					case "table":
						prose = "| literal | example |\n| --- | --- |\n| " + tick + " | " + tick + link + tick + " |"
					case "heading":
						prose = "## example: " + tick + "command | " + link + tick + "\n---"
					case "empty_label":
						prose = "[](" + reportArtifactURL(ref.Hash) + ")"
					case "code_label":
						prose = "[download " + tick + "capture.png" + tick + "](" + reportArtifactURL(ref.Hash) + ")"
					case "image":
						prose = "![image](" + reportArtifactURL(ref.Hash) + "?preview=1)"
					}
					body := u7Text
					body.ZhCN += "\n\n" + prose
					s.Findings = []Finding{{ID: "finding-a", Title: "Finding A", Report: &body}}
					s, err := store.Update(s.Run.ID, func(current *Snapshot) error { *current = s; return nil })
					if err != nil {
						t.Fatal(err)
					}
					want := context == "link" || context == "image" || context == "code_label"
					var receipt *ReportArchive
					for i := range s.Reports {
						if s.Reports[i].Kind == "finding" {
							receipt = &s.Reports[i]
						}
					}
					if receipt == nil {
						t.Fatal("missing finding archive")
					}
					if (len(receipt.Attachments) != 0) != want {
						t.Fatalf("attachments=%d want selected=%v", len(receipt.Attachments), want)
					}
					_, zipData, err := reportBundleTestBytes(t, store, s.Run.ID, receipt.ID, "zh-CN")
					if err != nil {
						t.Fatal(err)
					}
					files := readReportZIP(t, zipData)
					_, included := files["assets/"+ref.Hash+".png"]
					if included != want {
						t.Fatalf("ZIP image included=%v want=%v", included, want)
					}
					if !want && strings.Contains(string(files["report.zh-CN.md"]), "![unrelated.png]") {
						t.Fatal("code example gained a real image")
					}
				})
			}
		})
	}
}

func TestReportIndependentLanguageAttachmentScan(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			_, s, ref := reportSelectionSnapshot(t, backend)
			body := u7Text
			body.ZhCN += "\n" + strings.Repeat(string(rune(96)), 3) + "md\nexample only"
			body.En += "\n![capture](" + reportArtifactURL(ref.Hash) + "?preview=1)"
			s.Findings = []Finding{{ID: "finding-a", Title: "Finding A", Report: &body}}
			for _, c := range reportCandidates(&s) {
				if c.source.Kind == "finding" {
					if len(c.source.Attachments) != 1 {
						t.Fatal("Chinese code fence hid English attachment")
					}
					if len(reportMissingImages(body, c.source.Attachments)) != 0 {
						t.Fatal("valid English image marked missing")
					}
				}
			}
		})
	}
}

func TestReportBoundTranslationCannotExpandAttachments(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, s, first := reportSelectionSnapshot(t, backend)
			second, err := store.PutArtifact("file", append(reportPNG(t), byte(0)))
			if err != nil {
				t.Fatal(err)
			}
			second.Name = "other.png"
			resultValidationSource(t, &s, "other", "artifact_register", "builtin", "registered other", second)
			body := u7Text
			body.ZhCN += "\n[original](" + reportArtifactURL(first.Hash) + ")"
			s.Findings = []Finding{{ID: "finding-a", Title: "Finding A", Report: &body}}
			if err := store.archiveReports(&s); err != nil {
				t.Fatal(err)
			}
			var original reportCandidate
			for _, c := range reportCandidates(&s) {
				if c.source.Kind == "finding" {
					original = c
				}
			}
			if len(original.source.Attachments) != 1 {
				t.Fatal("fixture lacks original attachment")
			}
			translated := body
			translated.SourceDigest = original.digest()
			translated.En += "\n[other](" + reportArtifactURL(second.Hash) + ")"
			s.Findings[0].Report = &translated
			for _, c := range reportCandidates(&s) {
				if c.source.Kind == "finding" {
					if c.digest() != original.digest() || len(c.source.Attachments) != 1 || c.source.Attachments[0].Artifact.Hash != first.Hash {
						t.Fatal("bound translation changed attachment source")
					}
					text, missing := c.bodies()
					if len(missing) != 0 || text.En != translated.En {
						t.Fatal("bound translation was discarded")
					}
				}
			}
		})
	}
}

func TestReportLinkedImageMatchesRenderedLink(t *testing.T) {
	url := reportArtifactURL(strings.Repeat("0", 64)) + "?preview=1"
	body := "[![thumbnail](" + url + ")](/detail)"
	if len(reportMissingImages(ReportText{ZhCN: body}, nil)) != 0 || reportBodyHasImage(body, url) {
		t.Fatal("link token was recursively treated as a displayed image")
	}
}
