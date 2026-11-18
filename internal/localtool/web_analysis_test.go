package localtool

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestInstalledWebAnalysis(t *testing.T) {
	if os.Getenv("TRACE_WEB_ANALYSIS_DIR") == "" {
		t.Skip("set TRACE_WEB_ANALYSIS_DIR to exercise the separately built web package")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manifests, status := webAnalysisPresets(ctx)
	if !status.Available || len(manifests) != 28 {
		t.Fatalf("web discovery: %+v, tools=%d", status, len(manifests))
	}
	for _, manifest := range manifests {
		if err := manifest.Validate(); err != nil {
			t.Fatal(err)
		}
		if manifest.Name != "analyze_javascript_application" {
			continue
		}
		request, err := manifest.Request("web-test", "invalid-input", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if request.Stdin != "{}\n" || len(request.Command) != 4 || request.Command[2] != "--operation" || request.CancelGraceSeconds != 15 {
			t.Fatalf("unexpected generic localtool request: %+v", request)
		}
		result, err := manifest.Execute(ctx, "web-test", "invalid-input", json.RawMessage(`{}`))
		if err == nil || result.ExitCode == nil || *result.ExitCode != 1 {
			t.Fatalf("expected rejected input: %+v, %v", result, err)
		}
		var envelope struct {
			OK    bool            `json:"ok"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(result.Stdout), &envelope); err != nil {
			t.Fatalf("invalid output: %+v: %v", result, err)
		}
		if envelope.OK || len(envelope.Error) == 0 {
			t.Fatalf("missing structured failure: %+v", result)
		}
		return
	}
	t.Fatal("missing JavaScript analysis preset")
}
