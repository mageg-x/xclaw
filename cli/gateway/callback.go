package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"xclaw/cli/audit"
	"xclaw/cli/db"
)

type CallbackAction struct {
	Type      string            `json:"type"`
	ActionID  string            `json:"action_id"`
	Data      map[string]string `json:"data"`
	ExpiresAt time.Time         `json:"expires_at"`
	Signature string            `json:"signature"`
}

type CallbackResult struct {
	Handled bool   `json:"handled"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type CallbackHandler struct {
	store     *db.Store
	audit     *audit.Logger
	secret    string
	mu        sync.RWMutex
	pending   map[string]*CallbackAction
	handlers  map[string]func(context.Context, *CallbackAction) error
}

const settingCallbackSecret = "gateway_callback_secret"
const settingCallbackPending = "gateway_callback_pending_json"

func NewCallbackHandler(store *db.Store, auditLogger *audit.Logger, secret string) *CallbackHandler {
	if strings.TrimSpace(secret) == "" {
		secret = fmt.Sprintf("cb_%d", time.Now().UnixNano())
	}
	h := &CallbackHandler{
		store:    store,
		audit:    auditLogger,
		secret:   secret,
		pending:  make(map[string]*CallbackAction),
		handlers: make(map[string]func(context.Context, *CallbackAction) error),
	}
	h.handlers["approve"] = h.handleApprove
	h.handlers["reject"] = h.handleReject
	h.loadState(context.Background())
	return h
}

func (h *CallbackHandler) RegisterHandler(actionType string, handler func(context.Context, *CallbackAction) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[actionType] = handler
}

func (h *CallbackHandler) CreateAction(ctx context.Context, actionType, actionID string, data map[string]string, ttl time.Duration) (*CallbackAction, error) {
	actionType = strings.TrimSpace(strings.ToLower(actionType))
	actionID = strings.TrimSpace(actionID)
	if actionType == "" || actionID == "" {
		return nil, fmt.Errorf("action_type and action_id are required")
	}
	if data == nil {
		data = map[string]string{}
	}

	now := time.Now().UTC()
	action := &CallbackAction{
		Type:      actionType,
		ActionID:  actionID,
		Data:      data,
		ExpiresAt: now.Add(ttl),
	}
	action.Signature = h.sign(action)

	h.mu.Lock()
	h.pending[actionID] = action
	h.mu.Unlock()
	_ = h.saveState(ctx)

	h.audit.Log(ctx, data["agent_id"], data["session_id"], "callback", "created", fmt.Sprintf("action_id=%s type=%s", actionID, actionType))
	return action, nil
}

func (h *CallbackHandler) Handle(ctx context.Context, event InboundEvent) (CallbackResult, error) {
	action, err := h.parseCallback(event)
	if err != nil {
		return CallbackResult{Handled: false, Status: "error", Message: err.Error()}, err
	}

	if time.Now().UTC().After(action.ExpiresAt) {
		h.mu.Lock()
		delete(h.pending, action.ActionID)
		h.mu.Unlock()
		_ = h.saveState(ctx)
		return CallbackResult{Handled: false, Status: "expired", Message: "callback expired"}, nil
	}

	if !h.verifySignature(action) {
		return CallbackResult{Handled: false, Status: "invalid", Message: "signature verification failed"}, fmt.Errorf("invalid callback signature")
	}

	h.mu.RLock()
	handler, ok := h.handlers[action.Type]
	h.mu.RUnlock()
	if !ok {
		return CallbackResult{Handled: false, Status: "unknown_type", Message: fmt.Sprintf("no handler for type: %s", action.Type)}, nil
	}

	if err := handler(ctx, action); err != nil {
		h.audit.Log(ctx, action.Data["agent_id"], action.Data["session_id"], "callback", "error", fmt.Sprintf("action_id=%s type=%s err=%v", action.ActionID, action.Type, err))
		return CallbackResult{Handled: false, Status: "error", Message: err.Error()}, err
	}

	h.mu.Lock()
	delete(h.pending, action.ActionID)
	h.mu.Unlock()
	_ = h.saveState(ctx)

	h.audit.Log(ctx, action.Data["agent_id"], action.Data["session_id"], "callback", action.Type, fmt.Sprintf("action_id=%s", action.ActionID))
	return CallbackResult{Handled: true, Status: "ok", Message: "callback processed"}, nil
}

func (h *CallbackHandler) ListPending() []CallbackAction {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]CallbackAction, 0, len(h.pending))
	for _, a := range h.pending {
		out = append(out, *a)
	}
	return out
}

func (h *CallbackHandler) Cleanup() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now().UTC()
	count := 0
	for k, a := range h.pending {
		if now.After(a.ExpiresAt) {
			delete(h.pending, k)
			count++
		}
	}
	if count > 0 {
		_ = h.saveState(context.Background())
	}
	return count
}

func (h *CallbackHandler) parseCallback(event InboundEvent) (*CallbackAction, error) {
	if strings.TrimSpace(event.Metadata["callback_type"]) != "" {
		return &CallbackAction{
			Type:      strings.TrimSpace(event.Metadata["callback_type"]),
			ActionID:  strings.TrimSpace(event.Metadata["callback_action_id"]),
			Data:      event.Metadata,
			ExpiresAt: time.Now().UTC().Add(1 * time.Hour),
			Signature: strings.TrimSpace(event.Metadata["callback_signature"]),
		}, nil
	}

	if strings.TrimSpace(event.Text) != "" {
		var action CallbackAction
		if err := json.Unmarshal([]byte(event.Text), &action); err == nil && action.Type != "" {
			return &action, nil
		}
	}

	if strings.TrimSpace(event.ReplyToID) != "" {
		actionType := "approve"
		text := strings.ToLower(strings.TrimSpace(event.Text))
		if strings.Contains(text, "reject") || strings.Contains(text, "deny") || strings.Contains(text, "no") {
			actionType = "reject"
		}
		return &CallbackAction{
			Type:     actionType,
			ActionID: strings.TrimSpace(event.ReplyToID),
			Data:     event.Metadata,
		}, nil
	}

	return nil, fmt.Errorf("cannot parse callback from event")
}

func (h *CallbackHandler) sign(action *CallbackAction) string {
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write([]byte(action.Type))
	mac.Write([]byte(action.ActionID))
	mac.Write([]byte(action.ExpiresAt.Format(time.RFC3339)))
	for k, v := range action.Data {
		mac.Write([]byte(k))
		mac.Write([]byte(v))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *CallbackHandler) verifySignature(action *CallbackAction) bool {
	expected := h.sign(action)
	if strings.TrimSpace(action.Signature) == "" {
		return true
	}
	return hmac.Equal([]byte(action.Signature), []byte(expected))
}

func (h *CallbackHandler) handleApprove(ctx context.Context, action *CallbackAction) error {
	h.audit.Log(ctx, action.Data["agent_id"], action.Data["session_id"], "approval", "approved", action.ActionID)
	return nil
}

func (h *CallbackHandler) handleReject(ctx context.Context, action *CallbackAction) error {
	h.audit.Log(ctx, action.Data["agent_id"], action.Data["session_id"], "approval", "rejected", action.ActionID)
	return nil
}

func (h *CallbackHandler) loadState(ctx context.Context) {
	if h.store == nil {
		return
	}
	if raw, ok, err := h.store.GetSetting(ctx, settingCallbackPending); err == nil && ok && strings.TrimSpace(raw) != "" {
		var items []CallbackAction
		if err := json.Unmarshal([]byte(raw), &items); err == nil {
			h.mu.Lock()
			for i := range items {
				h.pending[items[i].ActionID] = &items[i]
			}
			h.mu.Unlock()
		}
	}
}

func (h *CallbackHandler) saveState(ctx context.Context) error {
	if h.store == nil {
		return nil
	}
	h.mu.RLock()
	items := make([]CallbackAction, 0, len(h.pending))
	for _, a := range h.pending {
		items = append(items, *a)
	}
	h.mu.RUnlock()
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return h.store.SetSetting(ctx, settingCallbackPending, string(raw))
}
