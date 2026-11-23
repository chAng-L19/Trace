package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"trace/internal/agent"
)

func TestRegisteredFileDownloadMatchesRecordedSource(t *testing.T) {
	data := []byte("actual delivered report\n")
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(agent.ArtifactRegistration{Path: path, Name: "report.txt", Purpose: "test deliverable"})
	calls := 0
	p := providerFunc(func(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
		calls++
		if calls == 1 {
			return agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "register", Name: "artifact_register", Arguments: string(args)}}}, nil
		}
		if calls > 2 {
			return agent.ModelResponse{}, fmt.Errorf("unexpected model call after report delivery: %d", calls)
		}
		body, _ := json.Marshal(map[string]any{"trace_report": agent.ReportText{
			ZhCN: "已注册实际报告文件并记录内容摘要和来源；本测试仅核对交付字节，不声明目标验证成功，影响与清理状态保持独立。",
			En:   "The actual report file was registered with its content hash and producing source. This fixture verifies delivered bytes only; target success, impact and cleanup remain separate claims.",
		}})
		return agent.ModelResponse{Content: "file captured\n\n```json\n" + string(body) + "\n```"}, nil
	})
	s, a := fixture(t, p)
	// This pre-role download fixture registers a file directly; keep legacy execution.
	created, err := a.Store().Create("deliver report", agent.DefaultPrompt, a.PromptHash())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Run(context.Background(), created.Run.ID)
	if err != nil || len(snap.Actions) != 1 || snap.Actions[0].Result == nil || len(snap.Actions[0].Result.Artifacts) != 1 {
		t.Fatalf("file registration: %+v %v", snap.Actions, err)
	}
	ref := snap.Actions[0].Result.Artifacts[0]
	digest := sha256.Sum256(data)
	if ref.Hash != hex.EncodeToString(digest[:]) {
		t.Fatal("registered file hash mismatch")
	}
	h := s.Handler()
	w := request(t, h, "GET", "/api/artifacts/"+ref.Hash+"/download", nil, 200)
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("download differs from actual file bytes")
	}
	// Run details preserve the exact producing call and metadata for navigation.
	w = request(t, h, "GET", "/api/runs/"+created.Run.ID, nil, 200)
	var detail agent.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Actions[0].RequestID != snap.Requests[0].ID || detail.Actions[0].CallID != "register" || detail.Actions[0].Result.Artifacts[0] != ref {
		t.Fatal("download reference lost producing source")
	}
}
