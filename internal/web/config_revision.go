package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// Revisions are random tokens, never digests of credentials or public config.
func newConfigRevision() (string, error) {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

// Caller holds mu. All configuration writers share this precondition and busy gate.
func (s *Server) configRevisionHeader(w http.ResponseWriter) {
	w.Header().Set("ETag", `"`+s.config.Revision+`"`)
}

func (s *Server) checkConfigRevision(w http.ResponseWriter, r *http.Request) bool {
	s.configRevisionHeader(w)
	match := strings.TrimSpace(r.Header.Get("If-Match"))
	code, status, detail := "config_revision_conflict", http.StatusConflict, "配置已被其他操作修改；请保留草稿，刷新并核对差异后再保存。"
	if match == "" {
		code, status, detail = "config_revision_required", http.StatusPreconditionRequired, "请先读取配置版本，再通过 If-Match 提交保存。"
	} else if match == `"`+s.config.Revision+`"` {
		return true
	}
	writeJSON(w, status, map[string]any{"error": detail, "code": code, "revision": s.config.Revision})
	return false
}
