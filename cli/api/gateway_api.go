package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"xclaw/cli/engine"
	"xclaw/cli/gateway"
)

func (s *Server) handleGatewayConfig(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.gateway.GetConfig())
	case http.MethodPut:
		var cfg gateway.Config
		if err := decodeJSON(r, &cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.gateway.UpdateConfig(r.Context(), cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, s.gateway.GetConfig())
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleGatewayProviders(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"configs": s.gateway.ListProviderConfigs(),
			"health":  s.gateway.ListProviderHealth(r.Context()),
		})
	case http.MethodPut:
		var cfg gateway.ProviderConfig
		if err := decodeJSON(r, &cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.gateway.UpsertProviderConfig(r.Context(), cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleGatewayBindings(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
		writeJSON(w, http.StatusOK, s.gateway.ListBindings(agentID))
	case http.MethodPost:
		var req gateway.Binding
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		item, err := s.gateway.UpsertBinding(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleGatewayBindingByID(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/gateway/bindings/")
	id = strings.TrimPrefix(path.Clean("/"+id), "/")
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("binding id required"))
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if err := s.gateway.DeleteBinding(r.Context(), id); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGatewayRoutes(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.gateway.ListRoutes())
	case http.MethodPost:
		var req gateway.RouteRule
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.gateway.UpsertRoute(r.Context(), req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleGatewayRouteByName(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/gateway/routes/")
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("route name required"))
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if err := s.gateway.DeleteRoute(r.Context(), name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGatewaySend(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		AgentID string                `json:"agent_id"`
		Target  string                `json:"target"`
		Event   gateway.OutboundEvent `json:"event"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.gateway.Send(r.Context(), strings.TrimSpace(req.AgentID), strings.TrimSpace(req.Target), req.Event)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGatewayDLQ(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, s.gateway.ListDLQ())
}

func (s *Server) handleGatewayDLQByID(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/gateway/dlq/")
	id = strings.TrimPrefix(path.Clean("/"+id), "/")
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("dlq id required"))
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.gateway.DeleteDLQ(r.Context(), id); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodPost:
		if err := s.gateway.ReplayDLQ(r.Context(), id); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replayed": id})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleGatewayDLQBatchRetry(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	items := s.gateway.ListDLQ()
	var retried []string
	var failed []string
	for _, item := range items {
		if err := s.gateway.ReplayDLQ(r.Context(), item.ID); err != nil {
			failed = append(failed, item.ID)
		} else {
			retried = append(retried, item.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"retried": retried, "failed": failed})
}

func (s *Server) handleGatewayDLQPurge(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	items := s.gateway.ListDLQ()
	var purged []string
	var failed []string
	for _, item := range items {
		if err := s.gateway.DeleteDLQ(r.Context(), item.ID); err != nil {
			failed = append(failed, item.ID)
		} else {
			purged = append(purged, item.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"purged": purged, "failed": failed})
}

func (s *Server) handleGatewayWebhook(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		writeError(w, http.StatusNotImplemented, errText("gateway disabled"))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	provider := strings.TrimPrefix(r.URL.Path, "/api/gateway/webhook/")
	provider = strings.TrimPrefix(path.Clean("/"+provider), "/")
	if provider == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("provider required"))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.verifyWebhookToken(provider, r, body) {
		writeError(w, http.StatusUnauthorized, fmt.Errorf("invalid webhook signature or token"))
		return
	}

	var evt gateway.InboundEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		evt = gateway.InboundEvent{Text: strings.TrimSpace(string(body)), Metadata: map[string]string{}}
	}
	if strings.TrimSpace(evt.EventID) == "" {
		evt.EventID = engine.NewID("evt")
	}
	evt.Platform = firstText(strings.ToLower(strings.TrimSpace(evt.Platform)), strings.ToLower(provider))
	evt.Protocol = firstText(strings.TrimSpace(evt.Protocol), "webhook")
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now().UTC()
	}
	if evt.Metadata == nil {
		evt.Metadata = map[string]string{}
	}
	evt.RawPayload = body

	if err := s.gateway.ReceiveInbound(r.Context(), evt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) verifyWebhookToken(provider string, r *http.Request, body []byte) bool {
	if s.gateway == nil {
		return false
	}
	configs := s.gateway.ListProviderConfigs()
	for _, cfg := range configs {
		if cfg.Name != strings.ToLower(provider) {
			continue
		}
		secret := strings.TrimSpace(cfg.Settings["inbound_token"])
		if secret == "" {
			secret = strings.TrimSpace(cfg.Settings["token"])
		}
		if secret == "" {
			return s.verifyWebhookSignature(cfg, r, body)
		}
		incoming := strings.TrimSpace(r.Header.Get("X-Gateway-Token"))
		if incoming == "" {
			incoming = extractBearer(r.Header.Get("Authorization"))
		}
		if incoming != secret {
			return false
		}
		return s.verifyWebhookSignature(cfg, r, body)
	}
	return true
}

func (s *Server) verifyWebhookSignature(cfg gateway.ProviderConfig, r *http.Request, body []byte) bool {
	signingSecret := strings.TrimSpace(cfg.Settings["signing_secret"])
	if signingSecret == "" {
		return true
	}
	tsRaw := strings.TrimSpace(r.Header.Get("X-Gateway-Timestamp"))
	sigRaw := strings.TrimSpace(r.Header.Get("X-Gateway-Signature"))
	if tsRaw == "" || sigRaw == "" {
		return false
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	if now-ts > 300 || ts-now > 300 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(signingSecret))
	_, _ = mac.Write([]byte(tsRaw))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(expected)), []byte(strings.ToLower(sigRaw)))
}

func (s *Server) handleGatewayInboundEvent(ctx context.Context, evt gateway.InboundEvent) error {
	agentID := strings.TrimSpace(evt.Metadata["target_agent"])
	if agentID == "" {
		agentID = strings.TrimSpace(evt.Metadata["agent_id"])
	}
	if agentID == "" {
		agents, err := s.engine.ListAgents(ctx)
		if err != nil {
			return err
		}
		if len(agents) == 0 {
			return fmt.Errorf("no available agent for inbound event")
		}
		agentID = agents[0].ID
	}

	forceNewSession := strings.EqualFold(strings.TrimSpace(evt.Metadata["route_create_session"]), "true")
	sessionID := strings.TrimSpace(evt.Metadata["target_session"])
	if forceNewSession || sessionID == "" {
		if !forceNewSession {
			mainSession, ok, err := s.store.GetMainSession(ctx, agentID)
			if err != nil {
				return err
			}
			if ok {
				sessionID = mainSession.ID
			}
		}
		if sessionID == "" {
			title := "社交通道会话"
			if strings.TrimSpace(evt.SenderName) != "" {
				title = "社交通道会话 - " + strings.TrimSpace(evt.SenderName)
			}
			sess, err := s.engine.CreateSession(ctx, agentID, title, !forceNewSession)
			if err != nil {
				return err
			}
			sessionID = sess.ID
		}
	}

	text := strings.TrimSpace(evt.Text)
	if strings.EqualFold(strings.TrimSpace(evt.Metadata["route_strip_prefix"]), "true") {
		prefix := strings.TrimSpace(evt.Metadata["route_content_prefix"])
		if prefix != "" && strings.HasPrefix(text, prefix) {
			text = strings.TrimSpace(strings.TrimPrefix(text, prefix))
		}
	}
	if text == "" {
		text = "[收到一条无文本消息]"
	}
	if len(evt.Attachments) > 0 {
		text += fmt.Sprintf("\n\n(附件数量: %d)", len(evt.Attachments))
	}
	_, err := s.engine.SendMessage(ctx, sessionID, engine.SendMessageInput{Content: text, AutoApprove: true})
	return err
}
