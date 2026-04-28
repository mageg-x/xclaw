package api

import (
	"net/http"
	"strconv"
	"strings"

	"xclaw/cli/heartbeat"
)

func (s *Server) handleHeartbeatConfig(w http.ResponseWriter, r *http.Request) {
	if s.heartbeat == nil {
		writeError(w, http.StatusNotImplemented, errText("heartbeat runner disabled"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.heartbeat.LoadConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPut:
		var cfg heartbeat.Config
		if err := decodeJSON(r, &cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.heartbeat.SaveConfig(r.Context(), cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		latest, _ := s.heartbeat.LoadConfig(r.Context())
		writeJSON(w, http.StatusOK, latest)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleHeartbeatRunNow(w http.ResponseWriter, r *http.Request) {
	if s.heartbeat == nil {
		writeError(w, http.StatusNotImplemented, errText("heartbeat runner disabled"))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cfg, err := s.heartbeat.LoadConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.heartbeat.RunOnce(r.Context(), cfg); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleHeartbeatRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := s.store.ListAudit(r.Context(), agentID, limit*4)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]any, 0, limit)
	for _, row := range rows {
		if row.Category != "heartbeat" {
			continue
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}
