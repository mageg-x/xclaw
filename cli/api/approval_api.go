package api

import (
	"context"
	"net/http"
	"strings"

	"xclaw/cli/approval"
)

const approvalStateSettingKey = "approval_state_json"

func (s *Server) loadApprovalState() {
	if s.approver == nil {
		return
	}
	raw, ok, err := s.store.GetSetting(context.Background(), approvalStateSettingKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	_ = s.approver.Load(raw)
}

func (s *Server) saveApprovalState() {
	if s.approver == nil {
		return
	}
	raw, err := s.approver.Snapshot()
	if err != nil {
		return
	}
	_ = s.store.SetSetting(context.Background(), approvalStateSettingKey, string(raw))
}

func (s *Server) handleApprovalRules(w http.ResponseWriter, r *http.Request) {
	if s.approver == nil {
		writeError(w, http.StatusNotImplemented, errText("approval manager disabled"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.approver.Rules())
	case http.MethodPut:
		var rules []approval.Rule
		if err := decodeJSON(r, &rules); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.approver.SetRules(rules)
		s.saveApprovalState()
		writeJSON(w, http.StatusOK, s.approver.Rules())
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleApprovalRequests(w http.ResponseWriter, r *http.Request) {
	if s.approver == nil {
		writeError(w, http.StatusNotImplemented, errText("approval manager disabled"))
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, s.approver.ListRequests())
}

func (s *Server) handleApprovalApprove(w http.ResponseWriter, r *http.Request) {
	if s.approver == nil {
		writeError(w, http.StatusNotImplemented, errText("approval manager disabled"))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID    string `json:"id"`
		Actor string `json:"actor"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	item, err := s.approver.Approve(req.ID, req.Actor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.saveApprovalState()
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleApprovalReject(w http.ResponseWriter, r *http.Request) {
	if s.approver == nil {
		writeError(w, http.StatusNotImplemented, errText("approval manager disabled"))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID    string `json:"id"`
		Actor string `json:"actor"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	item, err := s.approver.Reject(req.ID, req.Actor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.saveApprovalState()
	writeJSON(w, http.StatusOK, item)
}

type errText string

func (e errText) Error() string {
	return string(e)
}
