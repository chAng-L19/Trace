package web

import (
	"fmt"
	"net/http"
	"strconv"
)

func (s *Server) registerMessageDeliveryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runs/{id}/message-deliveries", func(w http.ResponseWriter, r *http.Request) {
		limit := 40
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				writeError(w, http.StatusBadRequest, fmt.Errorf("limit must be 1..100"))
				return
			}
		}
		page, err := s.store.MessageDeliveries(r.PathValue("id"), r.URL.Query().Get("before"), limit)
		respond(w, page, err)
	})
}
