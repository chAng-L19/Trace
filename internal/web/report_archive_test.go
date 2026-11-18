package web

import (
	"encoding/json"
	"strings"
	"testing"

	"trace/internal/agent"
)

func TestU7ArchivedReportHTTPVersionsAndErrors(t *testing.T) {
	server, a := fixture(t, nil)
	s, err := a.Create("检查报告文件交付")
	if err != nil {
		t.Fatal(err)
	}
	h := server.Handler()
	base := "/api/runs/" + s.Run.ID + "/export?format=report"
	request(t, h, "GET", base+"&language=fr", nil, 400)
	request(t, h, "GET", base+"&report_id=missing", nil, 404)
	request(t, h, "GET", base, nil, 409)
	claim := agent.Result{Status: "failed", Summary: "观察未满足验收条件，仍保留具体检查结果。", Report: &agent.ReportText{ZhCN: "此次检查未满足预期条件，影响范围仅限已观察到的对象。清理仍待核对，继续工作需要补充来源。", En: "The check did not satisfy the expected condition. Impact is limited to the observed object. Cleanup remains unchecked and further work requires additional sources."}}
	raw, _ := json.Marshal(map[string]any{"trace_result": claim})
	s, err = server.store.Update(s.Run.ID, func(s *agent.Snapshot) error {
		s.Run.Status = agent.StatusCompleted
		s.Messages = append(s.Messages, agent.Message{Role: "assistant", Content: string(raw)})
		return nil
	})
	if err != nil || len(s.Reports) != 1 {
		t.Fatalf("report fixture: %+v %v", s.Reports, err)
	}
	id := s.Reports[0].ID
	en := request(t, h, "GET", base+"&report_id="+id+"&language=en", nil, 200)
	zh := request(t, h, "GET", base+"&language=zh-CN", nil, 200)
	if !strings.Contains(en.Header().Get("Content-Disposition"), ".en.md") || !strings.Contains(zh.Header().Get("Content-Disposition"), ".zh-CN.md") || !strings.Contains(en.Body.String(), claim.Report.En) || !strings.Contains(zh.Body.String(), claim.Report.ZhCN) {
		t.Fatal("language/file content mismatch")
	}
	if strings.Contains(en.Body.String(), claim.Report.ZhCN) || !strings.Contains(en.Body.String(), `"business_status": "failed"`) {
		t.Fatal("English export changed business or copied Chinese")
	}
	_, err = server.store.Update(s.Run.ID, func(s *agent.Snapshot) error { s.Run.Goal += " changed"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	fixed := request(t, h, "GET", base+"&report_id="+id+"&language=en", nil, 200)
	if fixed.Body.String() != en.Body.String() {
		t.Fatal("historical download was rewritten")
	}
	current := request(t, h, "GET", base+"&language=en", nil, 200)
	if !strings.Contains(current.Body.String(), "translation_missing") || !strings.Contains(current.Body.String(), "translation is missing") {
		t.Fatal("stale source translation presented as current")
	}
	request(t, h, "GET", base+"&source_hash=legacy&language=en", nil, 400)
	request(t, h, "GET", base+"&source_hash=legacy&report_id="+id, nil, 400)
}
