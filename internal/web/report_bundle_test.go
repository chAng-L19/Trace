package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/agent"
)

func reportBundlePNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	pixel := image.NewRGBA(image.Rect(0, 0, 2, 2))
	pixel.SetRGBA(0, 0, color.RGBA{R: 40, G: 110, B: 200, A: 255})
	pixel.SetRGBA(1, 0, color.RGBA{R: 245, G: 180, B: 50, A: 255})
	pixel.SetRGBA(0, 1, color.RGBA{R: 70, G: 170, B: 120, A: 255})
	pixel.SetRGBA(1, 1, color.RGBA{R: 200, G: 70, B: 90, A: 255})
	if err := png.Encode(&data, pixel); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestArtifactPreviewUsesCASBytesAndPreservesDownloads(t *testing.T) {
	s, a := fixture(t, nil)
	run, err := a.Create("preview captured bytes")
	if err != nil {
		t.Fatal(err)
	}
	var jpegData, gifData bytes.Buffer
	pixel := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if err := jpeg.Encode(&jpegData, pixel, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, pixel, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, claimedMIME, previewMIME string
		data                           []byte
		inline                         bool
	}{
		{"png_with_false_html_metadata", "text/html", "image/png", reportBundlePNG(t), true},
		{"jpeg", "image/jpeg", "image/jpeg", jpegData.Bytes(), true},
		{"gif", "image/gif", "image/gif", gifData.Bytes(), true},
		{"html_named_png", "image/png", "application/octet-stream", []byte("<!doctype html><script>alert('not an image')</script>"), false},
		{"svg_named_png", "image/png", "application/octet-stream", []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"><script>alert(1)</script></svg>"), false},
		{"empty", "image/png", "application/octet-stream", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := s.store.PutArtifact("file", tc.data)
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately wrong display metadata must not select an inline MIME.
			ref.Name, ref.MIMEType = "claimed.png", tc.claimedMIME
			if _, err := s.store.Update(run.Run.ID, func(snap *agent.Snapshot) error {
				snap.Actions = append(snap.Actions, agent.Action{RequestID: tc.name, CallID: "capture", Status: "completed", Result: &agent.ToolResult{Artifacts: []agent.ArtifactRef{ref}}})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			base := "/api/artifacts/" + ref.Hash + "/download"
			for _, query := range []string{"", "?preview=0", "?preview=true", "?preview=1"} {
				w := request(t, s.Handler(), "GET", base+query, nil, 200)
				wantMIME, disposition := "application/octet-stream", "attachment"
				if query == "?preview=1" {
					wantMIME = tc.previewMIME
					if tc.inline {
						disposition = "inline"
					}
				}
				if w.Header().Get("Content-Type") != wantMIME || !strings.HasPrefix(w.Header().Get("Content-Disposition"), disposition+";") || w.Header().Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(w.Body.Bytes(), tc.data) {
					t.Fatalf("%s headers=%v bytes=%x", query, w.Header(), w.Body.Bytes())
				}
			}
			if tc.inline {
				r := httptest.NewRequest("GET", base+"?preview=1", nil)
				r.Header.Set("Range", "bytes=1-3")
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusPartialContent || w.Header().Get("Content-Type") != tc.previewMIME || !bytes.Equal(w.Body.Bytes(), tc.data[1:4]) {
					t.Fatalf("range did not sniff the full CAS header: %d %v", w.Code, w.Header())
				}
			}
		})
	}
	request(t, s.Handler(), "GET", "/api/artifacts/"+strings.Repeat("0", 64)+"/download?preview=1", nil, 404)
}

// Seed immutable CAS receipts for HTTP selection tests, without an execution
// claim. Store acceptance tests own source/evidence binding validation.
func seedHTTPBundleReport(t *testing.T, s *Server, runID, reportID string, version int, picture agent.ArtifactRef) agent.ReportArchive {
	t.Helper()
	report := agent.ReportArchive{ID: reportID, Kind: "final", SubjectID: "result", Version: version, Attachments: []agent.ReportAttachment{{EvidenceID: "captured-evidence", Artifact: picture}}}
	for _, language := range []string{"zh-CN", "en"} {
		heading := "Archived screenshot report"
		if language == "zh-CN" {
			heading = "中文截图报告"
		}
		body := fmt.Sprintf("# %s\n\nSaved %s %s\n\n![captured image](/api/artifacts/%s/download?preview=1)\n\n[Original image](/api/artifacts/%s/download)\n", heading, reportID, language, picture.Hash, picture.Hash)
		ref, err := s.store.PutArtifact("report", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		ref.Name = reportID + "." + language + ".md"
		report.Files = append(report.Files, ref)
	}
	if _, err := s.store.Update(runID, func(snap *agent.Snapshot) error {
		snap.Reports = append(snap.Reports, report)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return report
}

func reportBundleEntries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{}
	for _, file := range archive.File {
		if filepath.IsAbs(file.Name) || strings.Contains(file.Name, "..") {
			t.Fatalf("unsafe bundle entry: %s", file.Name)
		}
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, duplicate := entries[file.Name]; duplicate {
			t.Fatalf("duplicate bundle entry: %s", file.Name)
		}
		entries[file.Name] = body
	}
	return entries
}

func TestReportBundleHTTPPinsRunVersionLanguageAndOriginalBytes(t *testing.T) {
	s, a := fixture(t, nil)
	run, err := a.Create("archived bundle selection")
	if err != nil {
		t.Fatal(err)
	}
	pngBytes := reportBundlePNG(t)
	picture, err := s.store.PutArtifact("file", pngBytes)
	if err != nil {
		t.Fatal(err)
	}
	picture.Name, picture.MIMEType = "captured.png", "image/png"
	report := seedHTTPBundleReport(t, s, run.Run.ID, "http-version-1", 1, picture)
	exportTemp := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, exportTemp)
	}
	assertBundleCleaned := func() {
		t.Helper()
		files, err := os.ReadDir(exportTemp)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Fatalf("export left temporary files: %v", files)
		}
	}
	base := "/api/runs/" + run.Run.ID + "/export?"
	plain := request(t, s.Handler(), "GET", base+"format=report&report_id="+report.ID+"&language=en", nil, 200)
	assetName := "assets/" + picture.Hash + ".png"
	for _, language := range []string{"zh-CN", "en"} {
		w := request(t, s.Handler(), "GET", base+"format=report_bundle&report_id="+report.ID+"&language="+language, nil, 200)
		if w.Header().Get("Content-Type") != "application/zip" || w.Header().Get("Content-Disposition") != "attachment; filename=\""+report.ID+"."+language+".zip\"" {
			t.Fatalf("bundle headers: %v", w.Header())
		}
		if w.Header().Get("Content-Length") != fmt.Sprint(w.Body.Len()) {
			t.Fatalf("bundle length: %v", w.Header())
		}
		assertBundleCleaned()
		r := httptest.NewRequest("GET", base+"format=report_bundle&report_id="+report.ID+"&language="+language, nil)
		r.Header.Set("Range", "bytes=0-3")
		partial := httptest.NewRecorder()
		s.Handler().ServeHTTP(partial, r)
		if partial.Code != http.StatusPartialContent || partial.Header().Get("Content-Range") != fmt.Sprintf("bytes 0-3/%d", w.Body.Len()) || !bytes.Equal(partial.Body.Bytes(), w.Body.Bytes()[:4]) {
			t.Fatalf("bundle range: %d %v %x", partial.Code, partial.Header(), partial.Body.Bytes())
		}
		assertBundleCleaned()
		entries := reportBundleEntries(t, w.Body.Bytes())
		body := string(entries["report."+language+".md"])
		if !strings.Contains(body, "Saved "+report.ID+" "+language) || !strings.Contains(body, "]("+assetName+")") || strings.Contains(body, "/api/artifacts/") || !bytes.Equal(entries[assetName], pngBytes) {
			t.Fatalf("bundle lost report or original image: %s", body)
		}
		var manifest struct {
			RunID  string              `json:"run_id"`
			Report agent.ReportArchive `json:"report"`
		}
		if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.RunID != run.Run.ID || manifest.Report.ID != report.ID || len(manifest.Report.Attachments) != 1 || manifest.Report.Attachments[0].Artifact.Hash != picture.Hash || manifest.Report.Attachments[0].EvidenceID != "captured-evidence" {
			t.Fatalf("wrong source receipt: %+v", manifest)
		}
	}
	defaultLanguage := request(t, s.Handler(), "GET", base+"format=report_bundle&report_id="+report.ID, nil, 200)
	if _, ok := reportBundleEntries(t, defaultLanguage.Body.Bytes())["report.zh-CN.md"]; !ok {
		t.Fatal("default bundle language changed")
	}
	seedHTTPBundleReport(t, s, run.Run.ID, "http-version-2", 2, picture)
	if _, err := s.store.Update(run.Run.ID, func(snap *agent.Snapshot) error { snap.Run.Goal += " changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	before, err := s.store.Load(run.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixed := request(t, s.Handler(), "GET", base+"format=report_bundle&report_id="+report.ID+"&language=en", nil, 200)
	fixedBody := string(reportBundleEntries(t, fixed.Body.Bytes())["report.en.md"])
	if !strings.Contains(fixedBody, "Saved http-version-1 en") || strings.Contains(fixedBody, "http-version-2") {
		t.Fatal("bundle selected the latest version instead of the pinned receipt")
	}
	oldPlain := request(t, s.Handler(), "GET", base+"format=report&report_id="+report.ID+"&language=en", nil, 200)
	if !bytes.Equal(oldPlain.Body.Bytes(), plain.Body.Bytes()) {
		t.Fatal("bundle rewrote the original archived Markdown")
	}
	after, err := s.store.Load(run.Run.ID)
	if err != nil || !after.Run.UpdatedAt.Equal(before.Run.UpdatedAt) || len(after.Reports) != len(before.Reports) {
		t.Fatalf("fixed export changed the Run: %v", err)
	}
	assertBundleCleaned()
}

func TestReportBundleHTTPRejectsInvalidAndCrossRunSelection(t *testing.T) {
	s, a := fixture(t, nil)
	run, err := a.Create("bundle errors")
	if err != nil {
		t.Fatal(err)
	}
	picture, err := s.store.PutArtifact("file", reportBundlePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	picture.MIMEType = "image/png"
	report := seedHTTPBundleReport(t, s, run.Run.ID, "bound-version", 1, picture)
	emptyRun, err := a.Create("no archived report")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, runID, query string
		status             int
	}{
		{"language", run.Run.ID, "format=report_bundle&report_id=" + report.ID + "&language=fr", 400},
		{"legacy_hash", run.Run.ID, "format=report_bundle&source_hash=legacy", 400},
		{"mixed_identity", run.Run.ID, "format=report_bundle&source_hash=legacy&report_id=" + report.ID, 400},
		{"missing_version", run.Run.ID, "format=report_bundle&report_id=missing&language=en", 404},
		{"cross_run", emptyRun.Run.ID, "format=report_bundle&report_id=" + report.ID + "&language=en", 404},
		{"no_archive", emptyRun.Run.ID, "format=report_bundle", 409},
		{"missing_run", "missing-run", "format=report_bundle&report_id=" + report.ID, 404},
		{"old_report_hash_conflict", run.Run.ID, "format=report&source_hash=legacy&report_id=" + report.ID, 400},
		{"old_report_hash_language", run.Run.ID, "format=report&source_hash=legacy&language=en", 400},
		{"old_report_changed_hash", run.Run.ID, "format=report&source_hash=legacy", 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, s.Handler(), "GET", "/api/runs/"+tc.runID+"/export?"+tc.query, nil, tc.status)
			if w.Header().Get("Content-Disposition") != "" || w.Header().Get("Content-Type") == "application/zip" {
				t.Fatalf("error became a downloadable ZIP: %v", w.Header())
			}
			var body struct{ Error string }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error == "" {
				t.Fatalf("missing API error: %s", w.Body.String())
			}
		})
	}
}

func TestReportBundleHTTPMissingOrCorruptAttachmentDoesNotDownload(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%t", corrupt), func(t *testing.T) {
			s, a := fixture(t, nil)
			run, err := a.Create("unavailable bundle image")
			if err != nil {
				t.Fatal(err)
			}
			picture, err := s.store.PutArtifact("file", reportBundlePNG(t))
			if err != nil {
				t.Fatal(err)
			}
			picture.MIMEType = "image/png"
			report := seedHTTPBundleReport(t, s, run.Run.ID, "image-version", 1, picture)
			file, err := s.store.OpenArtifact(picture.Hash)
			if err != nil {
				t.Fatal(err)
			}
			path := file.Name()
			file.Close()
			status := http.StatusNotFound
			if corrupt {
				err = os.WriteFile(path, []byte("corrupt captured bytes"), 0600)
				status = http.StatusConflict
			} else {
				err = os.Remove(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			w := request(t, s.Handler(), "GET", "/api/runs/"+run.Run.ID+"/export?format=report_bundle&report_id="+report.ID+"&language=en", nil, status)
			if w.Header().Get("Content-Disposition") != "" || w.Header().Get("Content-Type") == "application/zip" {
				t.Fatal("unavailable image produced a downloadable bundle")
			}
		})
	}
}

// Opt-in server-only headed acceptance. The record directory must be outside
// the checkout; this test never downloads a browser or installs an SDK.
func TestReportBundleHeadedBrowser(t *testing.T) {
	if os.Getenv("TRACE_REPORT_BROWSER") != "1" {
		t.Skip("server-only: TRACE_REPORT_BROWSER=1 and fixed headed CloakBrowser 0.6.4 required")
	}
	for _, key := range []string{"DISPLAY", "TRACE_CLOAKBROWSER_NODE_MODULE", "CLOAKBROWSER_BINARY_PATH", "TRACE_REPORT_BROWSER_RECORD_DIR"} {
		if os.Getenv(key) == "" {
			t.Fatalf("%s must be set for headed acceptance", key)
		}
	}
	recordDir, err := filepath.Abs(os.Getenv("TRACE_REPORT_BROWSER_RECORD_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Clean(filepath.Join(cwd, "..", ".."))
	outside := func(path string) bool {
		rel, err := filepath.Rel(checkout, path)
		return err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
	}
	if !outside(recordDir) {
		t.Fatal("TRACE_REPORT_BROWSER_RECORD_DIR must be outside the checkout")
	}
	if err := os.MkdirAll(recordDir, 0700); err != nil {
		t.Fatal(err)
	}
	recordDir, err = filepath.EvalSymlinks(recordDir)
	if err != nil || !outside(recordDir) {
		t.Fatalf("record directory must resolve outside the checkout: %v", err)
	}
	s, a := fixture(t, nil)
	run, err := a.Create("headed report and image delivery")
	if err != nil {
		t.Fatal(err)
	}
	pngBytes := reportBundlePNG(t)
	picture, err := s.store.PutArtifact("file", pngBytes)
	if err != nil {
		t.Fatal(err)
	}
	picture.Name, picture.MIMEType = "captured.png", "image/png"
	report := seedHTTPBundleReport(t, s, run.Run.ID, "browser-version", 1, picture)
	handler := s.Handler()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/report-bundle-fixture" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\"><title>Trace 报告交付验收</title><style>.artifact-media{width:256px;height:256px;image-rendering:pixelated;border:1px solid #aaa}button{margin:6px;padding:8px}</style><body><main style=\"max-width:1000px;margin:32px auto;font:18px sans-serif\"><h1>Trace 报告交付验收</h1><article id=\"report\"></article><section id=\"downloads\"></section><p id=\"notice\" role=\"status\"></p></main></body></html>")
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	screenshot := filepath.Join(recordDir, "trace-report-"+run.Run.ID+".png")
	download := filepath.Join(recordDir, "trace-report-"+run.Run.ID+".zip")
	data, err := json.Marshal(map[string]any{"base": httpServer.URL, "run": run.Run.ID, "report": report, "hash": picture.Hash, "screenshot": screenshot, "download": download})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, testNodeBinary(), "report_bundle_browser.mjs")
	command.Env = append(os.Environ(), "TRACE_REPORT_FIXTURE="+string(data), "CLOAKBROWSER_AUTO_UPDATE=false")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("headed report delivery: %v %s", err, output)
	} else {
		t.Log(string(output))
	}
	downloaded, err := os.ReadFile(download)
	if err != nil {
		t.Fatal(err)
	}
	entries := reportBundleEntries(t, downloaded)
	if !bytes.Equal(entries["assets/"+picture.Hash+".png"], pngBytes) || !strings.Contains(string(entries["report.zh-CN.md"]), "中文截图报告") || strings.Contains(string(entries["report.zh-CN.md"]), "/api/artifacts/") {
		t.Fatal("actual browser download lost the report or original image bytes")
	}
	screenshotInfo, err := os.Stat(screenshot)
	if err != nil || screenshotInfo.Size() == 0 {
		t.Fatalf("missing browser screenshot: %v", err)
	}
	t.Logf("headed screenshot=%s download=%s", screenshot, download)
}
