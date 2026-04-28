package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"xclaw/cli/mcpclient"
	"xclaw/cli/mcpregistry"
)

const mcpServersSettingKey = mcpclient.SettingsKey

func (s *Server) loadMCPServers() {
	if s.mcpClients == nil || s.store == nil {
		return
	}
	manual, err := mcpregistry.LoadManual(context.Background(), s.store)
	if err != nil {
		return
	}
	items, err := mcpregistry.MergeWithSkillServers(s.cfg.SkillsDir, manual)
	if err != nil {
		return
	}
	s.mcpClients.SetServers(items)
	raw, err := mcpclient.EncodeServers(items)
	if err == nil {
		_ = s.store.SetSetting(context.Background(), mcpServersSettingKey, raw)
	}
}

func (s *Server) saveMCPServers(ctx context.Context) error {
	if s.mcpClients == nil || s.store == nil {
		return nil
	}
	items, err := mcpregistry.SaveManualAndSync(ctx, s.store, s.cfg.SkillsDir, s.mcpClients.Servers())
	if err != nil {
		return err
	}
	s.mcpClients.SetServers(items)
	return nil
}

func (s *Server) handleMCPServers(w http.ResponseWriter, r *http.Request) {
	if s.mcpClients == nil {
		writeError(w, http.StatusNotImplemented, errText("mcp client manager not available"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.mcpClients.Servers())
	case http.MethodPut:
		var items []mcpclient.ServerConfig
		if err := decodeJSON(r, &items); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.mcpClients.SetServers(items)
		if err := s.saveMCPServers(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, s.mcpClients.Servers())
	case http.MethodPost:
		var item mcpclient.ServerConfig
		if err := decodeJSON(r, &item); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if existing, ok := findMCPServer(s.mcpClients.Servers(), item.ID); ok && existing.Readonly {
			writeError(w, http.StatusBadRequest, fmt.Errorf("mcp server is managed by %s and cannot be edited directly", existing.ManagedBy))
			return
		}
		saved := s.mcpClients.UpsertServer(item)
		if err := s.saveMCPServers(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("id required"))
			return
		}
		if existing, ok := findMCPServer(s.mcpClients.Servers(), id); ok && existing.Readonly {
			writeError(w, http.StatusBadRequest, fmt.Errorf("mcp server is managed by %s and cannot be deleted directly", existing.ManagedBy))
			return
		}
		if !s.mcpClients.RemoveServer(id) {
			writeError(w, http.StatusNotFound, fmt.Errorf("mcp server not found: %s", id))
			return
		}
		if err := s.saveMCPServers(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleMCPServerTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.mcpClients == nil {
		writeError(w, http.StatusNotImplemented, errText("mcp client manager not available"))
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		var req struct {
			ID string `json:"id"`
		}
		if err := decodeJSON(r, &req); err == nil {
			id = strings.TrimSpace(req.ID)
		}
	}
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id required"))
		return
	}
	result, err := s.mcpClients.TestServer(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMCPTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.mcpClients == nil {
		writeJSON(w, http.StatusOK, []mcpclient.ToolInfo{})
		return
	}
	writeJSON(w, http.StatusOK, s.mcpClients.ListTools(r.Context()))
}

func findMCPServer(items []mcpclient.ServerConfig, id string) (mcpclient.ServerConfig, bool) {
	id = strings.TrimSpace(id)
	for _, item := range items {
		if strings.TrimSpace(item.ID) == id {
			return item, true
		}
	}
	return mcpclient.ServerConfig{}, false
}
