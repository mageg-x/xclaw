package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"xclaw/cli/audit"
	"xclaw/cli/db"
	"xclaw/cli/engine"
	"xclaw/cli/queue"
)

const (
	settingGatewayConfig    = "gateway_config_json"
	settingGatewayProviders = "gateway_provider_configs_json"
	settingGatewayBindings  = "gateway_bindings_json"
	settingGatewayRoutes    = "gateway_routes_json"
	settingGatewayLast      = "gateway_last_target_json"
	settingGatewayDLQ       = "gateway_dlq_json"
)

type Gateway struct {
	store       *db.Store
	audit       *audit.Logger
	queue       *queue.LaneQueue
	reliability *reliabilityManager
	stream      *StreamHandler
	streamMgr   *StreamSessionManager
	adapter     *ContentAdapter
	health      *providerHealthTracker
	editTracker *editTracker
	rateLimiter *rateLimiter
	callback    *CallbackHandler

	mu            sync.RWMutex
	providers     map[string]Provider
	providerCfg   map[string]ProviderConfig
	bindings      map[string]Binding
	routes        []RouteRule
	config        Config
	lastTargets   map[string]string
	dlq           []DLQItem
	inboundHandle func(context.Context, InboundEvent) error
}

func New(store *db.Store, auditLogger *audit.Logger, q *queue.LaneQueue) (*Gateway, error) {
	g := &Gateway{
		store:       store,
		audit:       auditLogger,
		queue:       q,
		reliability: newReliabilityManager(),
		stream:      NewStreamHandler(DefaultStreamConfig()),
		streamMgr:   NewStreamSessionManager(),
		adapter:     NewContentAdapter(),
		health:      newProviderHealthTracker(),
		editTracker: newEditTracker(DefaultEditPolicy()),
		rateLimiter: newRateLimiter(),
		callback:    NewCallbackHandler(store, auditLogger, ""),
		providers:   make(map[string]Provider),
		providerCfg: make(map[string]ProviderConfig),
		bindings:    make(map[string]Binding),
		routes:      make([]RouteRule, 0),
		config: Config{
			DefaultTarget:   "last",
			FallbackTargets: []string{"console:default"},
			QuietHoursStart: "00:00",
			QuietHoursEnd:   "00:00",
		},
		lastTargets: make(map[string]string),
		dlq:         make([]DLQItem, 0),
	}
	if err := g.loadState(context.Background()); err != nil {
		return nil, err
	}
	g.ensureDefaultProviderConfig()
	g.RegisterProvider(NewConsoleProvider("console"))
	g.RegisterProvider(NewWebhookProvider("webhook", ""))
	for name, cfg := range g.copyProviderCfg() {
		if name == "console" || name == "webhook" {
			continue
		}
		g.RegisterProvider(NewProxyProvider(name, cfg.Protocol))
	}
	return g, nil
}

func (g *Gateway) SetInboundHandler(handler func(context.Context, InboundEvent) error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inboundHandle = handler
}

func (g *Gateway) RegisterProvider(provider Provider) {
	if provider == nil {
		return
	}
	provider.SetInboundHandler(g.ReceiveInbound)

	g.mu.Lock()
	g.providers[provider.Name()] = provider
	cfg, ok := g.providerCfg[provider.Name()]
	g.mu.Unlock()

	if ok {
		g.applyProviderConfig(provider, cfg)
	}
}

func (g *Gateway) Start(ctx context.Context) {
	providers := g.copyProviders()
	configs := g.copyProviderCfg()
	for name, provider := range providers {
		cfg, ok := configs[name]
		if !ok {
			cfg = ProviderConfig{Name: name, Protocol: provider.Protocol(), Enabled: name == "console", Settings: map[string]string{}}
		}
		g.applyProviderConfig(provider, cfg)
		if cfg.Enabled {
			if err := provider.Start(ctx); err != nil {
				g.audit.Log(ctx, "", "", "gateway", "provider_start_failed", fmt.Sprintf("provider=%s err=%v", name, err))
			}
		} else {
			_ = provider.Stop(ctx)
		}
	}
}

func (g *Gateway) Stop(ctx context.Context) {
	for _, provider := range g.copyProviders() {
		_ = provider.Stop(ctx)
	}
}

func (g *Gateway) ReceiveInbound(ctx context.Context, event InboundEvent) error {
	event.Platform = strings.TrimSpace(strings.ToLower(event.Platform))
	event.EventID = strings.TrimSpace(event.EventID)
	if event.Platform != "" && event.EventID != "" && g.reliability.SeenInbound(event.Platform, event.EventID) {
		return nil
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}

	target := joinTarget(event.Platform, event.ChatID, event.ThreadID)
	if target != "" {
		agentID := strings.TrimSpace(event.Metadata["agent_id"])
		g.setLastTarget(agentID, target)
	}
	if rule, ok := g.matchRoute(event); ok {
		if event.Metadata == nil {
			event.Metadata = map[string]string{}
		}
		if strings.TrimSpace(rule.Action.TargetAgent) != "" {
			event.Metadata["target_agent"] = strings.TrimSpace(rule.Action.TargetAgent)
		}
		if strings.TrimSpace(rule.Action.TargetSession) != "" {
			event.Metadata["target_session"] = strings.TrimSpace(rule.Action.TargetSession)
		}
		if strings.TrimSpace(rule.Action.Target) != "" {
			event.Metadata["target"] = strings.TrimSpace(rule.Action.Target)
		}
		if rule.Action.StripPrefix {
			event.Metadata["route_strip_prefix"] = "true"
			if strings.TrimSpace(rule.Match.ContentPrefix) != "" {
				event.Metadata["route_content_prefix"] = strings.TrimSpace(rule.Match.ContentPrefix)
			}
		}
		if rule.Action.CreateSession {
			event.Metadata["route_create_session"] = "true"
		}
		if strings.TrimSpace(rule.Action.Priority) != "" {
			event.Metadata["route_priority"] = strings.TrimSpace(rule.Action.Priority)
		}
	}

	g.audit.Log(ctx, strings.TrimSpace(event.Metadata["agent_id"]), "", "gateway", "inbound", fmt.Sprintf("provider=%s protocol=%s event_id=%s", event.Platform, event.Protocol, event.EventID))

	if strings.TrimSpace(event.Metadata["callback_type"]) != "" || strings.TrimSpace(event.Metadata["callback_action_id"]) != "" {
		result, cbErr := g.callback.Handle(ctx, event)
		if cbErr != nil {
			g.audit.Log(ctx, "", "", "gateway", "callback_error", fmt.Sprintf("event_id=%s err=%v", event.EventID, cbErr))
		}
		if result.Handled {
			return nil
		}
	}

	g.mu.RLock()
	handler := g.inboundHandle
	g.mu.RUnlock()
	if handler != nil {
		return handler(ctx, event)
	}
	return nil
}

func (g *Gateway) Send(ctx context.Context, agentID, target string, event OutboundEvent) (SendResult, error) {
	if strings.TrimSpace(event.MessageID) == "" {
		event.MessageID = engine.NewID("gw")
	}
	if strings.TrimSpace(event.IdempotencyKey) == "" && strings.TrimSpace(event.MessageID) != "" {
		event.IdempotencyKey = "msg:" + strings.TrimSpace(event.MessageID)
	}
	if strings.TrimSpace(event.IdempotencyKey) != "" && g.reliability.SeenOutbound(event.IdempotencyKey) {
		return SendResult{Status: "idempotent_skip"}, nil
	}
	target = firstText(strings.TrimSpace(target), strings.TrimSpace(event.Target))
	resolved, err := g.resolveTarget(agentID, target, event)
	if err != nil {
		return SendResult{}, err
	}
	event.Platform = resolved.Platform
	event.ChatID = resolved.ChatID
	event.ThreadID = resolved.ThreadID

	if g.inQuietHours(time.Now()) {
		p := strings.ToLower(strings.TrimSpace(event.Priority))
		if p != "high" && p != "critical" {
			g.audit.Log(ctx, agentID, "", "gateway", "deferred", fmt.Sprintf("target=%s priority=%s", resolved.RawTarget, p))
			return SendResult{Status: "deferred_quiet_hours"}, nil
		}
	}

	provider, ok := g.getProvider(resolved.Platform)
	if !ok {
		provider, ok = g.getProvider("console")
		if !ok {
			return SendResult{}, fmt.Errorf("no provider for platform %s", resolved.Platform)
		}
	}

	event, _ = g.AdaptContent(event, provider.Capabilities())

	laneID := fmt.Sprintf("gateway:%s:%s:%s", event.Platform, event.ChatID, event.ThreadID)
	var (
		result  SendResult
		sendErr error
	)
	done := g.queue.Enqueue(ctx, laneID, func(taskCtx context.Context) error {
		result, sendErr = g.sendWithRetry(taskCtx, agentID, provider, event)
		return sendErr
	})
	if err := <-done; err != nil {
		sendErr = err
	}
	if sendErr == nil {
		if strings.TrimSpace(event.IdempotencyKey) != "" {
			g.reliability.MarkOutbound(event.IdempotencyKey)
		}
		g.setLastTarget(agentID, joinTarget(event.Platform, event.ChatID, event.ThreadID))
		return result, nil
	}

	for _, fb := range g.copyConfig().FallbackTargets {
		fb = strings.TrimSpace(fb)
		if fb == "" || fb == resolved.RawTarget {
			continue
		}
		fallbackEvent := event
		fallbackEvent.Target = fb
		fbRes, fbErr := g.Send(ctx, agentID, fb, fallbackEvent)
		if fbErr == nil {
			return fbRes, nil
		}
	}
	g.pushDLQ(agentID, resolved.RawTarget, event, sendErr)
	return result, sendErr
}

func (g *Gateway) SendPresence(ctx context.Context, agentID, target string, presence PresenceEvent) error {
	evt := OutboundEvent{
		Target:       target,
		Platform:     presence.Platform,
		ChatID:       presence.ChatID,
		ThreadID:     presence.ThreadID,
		TextMarkdown: strings.TrimSpace(presence.Message),
		Phase:        normalizePresence(presence.State),
		Priority:     "normal",
	}
	_, err := g.Send(ctx, agentID, target, evt)
	return err
}

func (g *Gateway) UpsertProviderConfig(ctx context.Context, cfg ProviderConfig) error {
	cfg.Name = strings.TrimSpace(strings.ToLower(cfg.Name))
	if cfg.Name == "" {
		return fmt.Errorf("provider name required")
	}
	if cfg.Protocol == "" {
		cfg.Protocol = "bridge"
	}
	if cfg.Settings == nil {
		cfg.Settings = map[string]string{}
	}
	g.mu.Lock()
	g.providerCfg[cfg.Name] = cfg
	provider := g.providers[cfg.Name]
	g.mu.Unlock()

	if provider != nil {
		g.applyProviderConfig(provider, cfg)
		if cfg.Enabled {
			if err := provider.Start(ctx); err != nil {
				return err
			}
		} else {
			_ = provider.Stop(ctx)
		}
	}
	return g.saveProviderCfg(ctx)
}

func (g *Gateway) ListProviderConfigs() []ProviderConfig {
	cfg := g.copyProviderCfg()
	out := make([]ProviderConfig, 0, len(cfg))
	for _, c := range cfg {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (g *Gateway) ListProviderHealth(ctx context.Context) map[string]ProviderHealth {
	providers := g.copyProviders()
	out := make(map[string]ProviderHealth, len(providers))
	for name, p := range providers {
		base := p.Health(ctx)
		out[name] = g.health.Enrich(name, base)
	}
	return out
}

func (g *Gateway) UpsertBinding(ctx context.Context, binding Binding) (Binding, error) {
	if binding.ID == "" {
		binding.ID = engine.NewID("bind")
	}
	binding.Platform = strings.ToLower(strings.TrimSpace(binding.Platform))
	binding.ChatID = strings.TrimSpace(binding.ChatID)
	binding.ThreadID = strings.TrimSpace(binding.ThreadID)
	binding.AgentID = strings.TrimSpace(binding.AgentID)
	binding.SenderID = strings.TrimSpace(binding.SenderID)
	if binding.Platform == "" || binding.ChatID == "" {
		return Binding{}, fmt.Errorf("platform and chat_id are required")
	}
	if binding.Metadata == nil {
		binding.Metadata = map[string]string{}
	}
	binding.UpdatedAt = time.Now().UTC()

	g.mu.Lock()
	g.bindings[binding.ID] = binding
	g.mu.Unlock()
	return binding, g.saveBindings(ctx)
}

func (g *Gateway) DeleteBinding(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("binding id required")
	}
	g.mu.Lock()
	delete(g.bindings, id)
	g.mu.Unlock()
	return g.saveBindings(ctx)
}

func (g *Gateway) ListBindings(agentID string) []Binding {
	agentID = strings.TrimSpace(agentID)
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Binding, 0, len(g.bindings))
	for _, item := range g.bindings {
		if agentID != "" && item.AgentID != agentID {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func (g *Gateway) UpsertRoute(ctx context.Context, route RouteRule) error {
	route.Name = strings.TrimSpace(route.Name)
	if route.Name == "" {
		route.Name = engine.NewID("route")
	}
	if route.Priority == 0 {
		route.Priority = 100
	}
	route.Match.Platform = strings.ToLower(strings.TrimSpace(route.Match.Platform))

	g.mu.Lock()
	updated := false
	for i := range g.routes {
		if g.routes[i].Name == route.Name {
			g.routes[i] = route
			updated = true
			break
		}
	}
	if !updated {
		g.routes = append(g.routes, route)
	}
	sort.Slice(g.routes, func(i, j int) bool { return g.routes[i].Priority > g.routes[j].Priority })
	g.mu.Unlock()
	return g.saveRoutes(ctx)
}

func (g *Gateway) DeleteRoute(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("route name required")
	}
	g.mu.Lock()
	out := make([]RouteRule, 0, len(g.routes))
	for _, item := range g.routes {
		if item.Name == name {
			continue
		}
		out = append(out, item)
	}
	g.routes = out
	g.mu.Unlock()
	return g.saveRoutes(ctx)
}

func (g *Gateway) ListRoutes() []RouteRule {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]RouteRule, len(g.routes))
	copy(out, g.routes)
	return out
}

func (g *Gateway) ListDLQ() []DLQItem {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]DLQItem, len(g.dlq))
	copy(out, g.dlq)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (g *Gateway) ReplayDLQ(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("dlq id required")
	}
	g.mu.Lock()
	var (
		item  DLQItem
		found bool
	)
	for _, candidate := range g.dlq {
		if candidate.ID == id {
			item = candidate
			found = true
			break
		}
	}
	g.mu.Unlock()
	if !found {
		return fmt.Errorf("dlq item not found")
	}
	_, err := g.Send(ctx, item.AgentID, item.Target, item.Event)
	if err != nil {
		return err
	}
	return g.DeleteDLQ(ctx, id)
}

func (g *Gateway) DeleteDLQ(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("dlq id required")
	}
	g.mu.Lock()
	next := make([]DLQItem, 0, len(g.dlq))
	for _, item := range g.dlq {
		if item.ID == id {
			continue
		}
		next = append(next, item)
	}
	g.dlq = next
	g.mu.Unlock()
	return g.saveDLQ(ctx)
}

func (g *Gateway) BatchRetryDLQ(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	g.mu.RLock()
	items := make(map[string]DLQItem)
	for _, id := range ids {
		id = strings.TrimSpace(id)
		for _, item := range g.dlq {
			if item.ID == id {
				items[id] = item
				break
			}
		}
	}
	g.mu.RUnlock()

	retried := 0
	var lastErr error
	for _, id := range ids {
		id = strings.TrimSpace(id)
		item, ok := items[id]
		if !ok {
			continue
		}
		if _, err := g.Send(ctx, item.AgentID, item.Target, item.Event); err != nil {
			lastErr = err
			g.audit.Log(ctx, item.AgentID, "", "gateway", "dlq_batch_retry_failed", fmt.Sprintf("id=%s err=%v", id, err))
			continue
		}
		_ = g.DeleteDLQ(ctx, id)
		retried++
	}
	return retried, lastErr
}

func (g *Gateway) PurgeDLQ(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		maxAge = 30 * 24 * time.Hour
	}
	g.mu.Lock()
	cutoff := time.Now().UTC().Add(-maxAge)
	next := make([]DLQItem, 0, len(g.dlq))
	purged := 0
	for _, item := range g.dlq {
		if item.CreatedAt.Before(cutoff) {
			purged++
			continue
		}
		next = append(next, item)
	}
	g.dlq = next
	g.mu.Unlock()
	if purged > 0 {
		_ = g.saveDLQ(ctx)
	}
	return purged, nil
}

func (g *Gateway) UpdateConfig(ctx context.Context, cfg Config) error {
	cfg.DefaultTarget = firstText(strings.TrimSpace(cfg.DefaultTarget), "last")
	if cfg.FallbackTargets == nil {
		cfg.FallbackTargets = []string{"console:default"}
	}
	g.mu.Lock()
	g.config = cfg
	g.mu.Unlock()
	return g.saveConfig(ctx)
}

func (g *Gateway) GetConfig() Config {
	return g.copyConfig()
}

func (g *Gateway) GetStreamConfig() StreamConfig {
	return g.stream.GetConfig()
}

func (g *Gateway) UpdateStreamConfig(cfg StreamConfig) {
	g.stream.UpdateConfig(cfg)
}

func (g *Gateway) GetEditPolicy() EditPolicy {
	return g.editTracker.policy
}

func (g *Gateway) UpdateEditPolicy(policy EditPolicy) {
	if policy.MaxEdits <= 0 {
		policy.MaxEdits = DefaultEditPolicy().MaxEdits
	}
	if policy.EditWindow <= 0 {
		policy.EditWindow = DefaultEditPolicy().EditWindow
	}
	if policy.EditThreshold <= 0 {
		policy.EditThreshold = DefaultEditPolicy().EditThreshold
	}
	g.editTracker.policy = policy
}

func (g *Gateway) CreateCallbackAction(ctx context.Context, actionType, actionID string, data map[string]string, ttl time.Duration) (*CallbackAction, error) {
	return g.callback.CreateAction(ctx, actionType, actionID, data, ttl)
}

func (g *Gateway) HandleCallback(ctx context.Context, event InboundEvent) (CallbackResult, error) {
	return g.callback.Handle(ctx, event)
}

func (g *Gateway) ListPendingCallbacks() []CallbackAction {
	return g.callback.ListPending()
}

func (g *Gateway) RegisterCallbackHandler(actionType string, handler func(context.Context, *CallbackAction) error) {
	g.callback.RegisterHandler(actionType, handler)
}

func (g *Gateway) AdaptContent(event OutboundEvent, cap CapabilityProfile) (OutboundEvent, []ContentFragment) {
	text, actions, fragments := g.adapter.Adapt(event.TextMarkdown, event.Actions, cap)
	event.TextMarkdown = text
	event.Actions = actions
	return event, fragments
}

func (g *Gateway) SendStream(ctx context.Context, agentID, target string, event OutboundEvent) ([]SendResult, error) {
	provider, ok := g.getProvider(event.Platform)
	if !ok {
		provider, ok = g.getProvider("console")
		if !ok {
			return nil, fmt.Errorf("no provider for platform %s", event.Platform)
		}
	}
	cap := provider.Capabilities()
	mode := g.stream.SelectMode(cap, len(event.TextMarkdown), 0)
	chunks := g.stream.ChunkText(event.TextMarkdown, 0)

	sessionID := event.MessageID
	sess := g.streamMgr.Start(sessionID, mode)
	defer g.streamMgr.Finish(sessionID)

	var results []SendResult
	for i, chunk := range chunks {
		chunkEvent := event
		chunkEvent.TextMarkdown = chunk.Content
		chunkEvent.Stream = true
		chunkEvent.Phase = "streaming"
		if chunk.Finished {
			chunkEvent.Phase = "done"
		}

		g.streamMgr.AppendChunk(sessionID, StreamChunk{
			Index:    i,
			Mode:     mode,
			Content:  chunk.Content,
			Finished: chunk.Finished,
		})

		if mode == StreamModeEdit {
			if !g.editTracker.CheckAndRecord(event.MessageID, chunk.Content) {
				mode = StreamModeChunk
				sess.Mode = mode
			}
		}

		laneID := fmt.Sprintf("gateway:%s:%s:%s", chunkEvent.Platform, chunkEvent.ChatID, chunkEvent.ThreadID)
		var (
			result  SendResult
			sendErr error
		)
		done := g.queue.Enqueue(ctx, laneID, func(taskCtx context.Context) error {
			result, sendErr = g.sendWithRetry(taskCtx, agentID, provider, chunkEvent)
			return sendErr
		})
		if err := <-done; err != nil {
			return results, err
		}
		results = append(results, result)

		if mode == StreamModeEdit && sess.EditCount >= g.stream.GetConfig().MaxEditsPerMsg {
			mode = StreamModeChunk
			sess.Mode = mode
		}
	}
	return results, nil
}

func (g *Gateway) loadState(ctx context.Context) error {
	if raw, ok, err := g.store.GetSetting(ctx, settingGatewayConfig); err != nil {
		return err
	} else if ok && strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &g.config)
	}

	if raw, ok, err := g.store.GetSetting(ctx, settingGatewayProviders); err != nil {
		return err
	} else if ok && strings.TrimSpace(raw) != "" {
		var items []ProviderConfig
		if err := json.Unmarshal([]byte(raw), &items); err == nil {
			for _, item := range items {
				item.Name = strings.TrimSpace(strings.ToLower(item.Name))
				if item.Name == "" {
					continue
				}
				if item.Settings == nil {
					item.Settings = map[string]string{}
				}
				g.providerCfg[item.Name] = item
			}
		}
	}

	if raw, ok, err := g.store.GetSetting(ctx, settingGatewayBindings); err != nil {
		return err
	} else if ok && strings.TrimSpace(raw) != "" {
		var items []Binding
		if err := json.Unmarshal([]byte(raw), &items); err == nil {
			for _, item := range items {
				if strings.TrimSpace(item.ID) == "" {
					continue
				}
				g.bindings[item.ID] = item
			}
		}
	}

	if raw, ok, err := g.store.GetSetting(ctx, settingGatewayRoutes); err != nil {
		return err
	} else if ok && strings.TrimSpace(raw) != "" {
		var items []RouteRule
		if err := json.Unmarshal([]byte(raw), &items); err == nil {
			g.routes = items
			sort.Slice(g.routes, func(i, j int) bool { return g.routes[i].Priority > g.routes[j].Priority })
		}
	}

	if raw, ok, err := g.store.GetSetting(ctx, settingGatewayLast); err != nil {
		return err
	} else if ok && strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &g.lastTargets)
	}
	if raw, ok, err := g.store.GetSetting(ctx, settingGatewayDLQ); err != nil {
		return err
	} else if ok && strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &g.dlq)
	}
	return nil
}

func (g *Gateway) saveConfig(ctx context.Context) error {
	g.mu.RLock()
	raw, err := json.Marshal(g.config)
	g.mu.RUnlock()
	if err != nil {
		return err
	}
	return g.store.SetSetting(ctx, settingGatewayConfig, string(raw))
}

func (g *Gateway) saveProviderCfg(ctx context.Context) error {
	cfg := g.ListProviderConfigs()
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return g.store.SetSetting(ctx, settingGatewayProviders, string(raw))
}

func (g *Gateway) saveBindings(ctx context.Context) error {
	items := g.ListBindings("")
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return g.store.SetSetting(ctx, settingGatewayBindings, string(raw))
}

func (g *Gateway) saveRoutes(ctx context.Context) error {
	items := g.ListRoutes()
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return g.store.SetSetting(ctx, settingGatewayRoutes, string(raw))
}

func (g *Gateway) saveLastTargets(ctx context.Context) error {
	g.mu.RLock()
	raw, err := json.Marshal(g.lastTargets)
	g.mu.RUnlock()
	if err != nil {
		return err
	}
	return g.store.SetSetting(ctx, settingGatewayLast, string(raw))
}

func (g *Gateway) saveDLQ(ctx context.Context) error {
	g.mu.RLock()
	raw, err := json.Marshal(g.dlq)
	g.mu.RUnlock()
	if err != nil {
		return err
	}
	return g.store.SetSetting(ctx, settingGatewayDLQ, string(raw))
}

func (g *Gateway) copyProviders() map[string]Provider {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make(map[string]Provider, len(g.providers))
	for k, v := range g.providers {
		out[k] = v
	}
	return out
}

func (g *Gateway) copyProviderCfg() map[string]ProviderConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make(map[string]ProviderConfig, len(g.providerCfg))
	for k, v := range g.providerCfg {
		out[k] = v
	}
	return out
}

func (g *Gateway) copyConfig() Config {
	g.mu.RLock()
	defer g.mu.RUnlock()
	cfg := g.config
	cfg.FallbackTargets = append([]string(nil), cfg.FallbackTargets...)
	return cfg
}

func (g *Gateway) ensureDefaultProviderConfig() {
	g.mu.Lock()
	defer g.mu.Unlock()
	defaults := []ProviderConfig{
		{Name: "console", Protocol: "bridge", Enabled: true, Settings: map[string]string{}},
		{Name: "webhook", Protocol: "webhook", Enabled: false, Settings: map[string]string{}},
		{Name: "telegram", Protocol: "longpoll", Enabled: false, Settings: map[string]string{}},
		{Name: "discord", Protocol: "websocket", Enabled: false, Settings: map[string]string{}},
		{Name: "whatsapp", Protocol: "bridge", Enabled: false, Settings: map[string]string{}},
		{Name: "weixin", Protocol: "bridge", Enabled: false, Settings: map[string]string{}},
		{Name: "qq", Protocol: "websocket", Enabled: false, Settings: map[string]string{}},
		{Name: "slack", Protocol: "websocket", Enabled: false, Settings: map[string]string{}},
		{Name: "matrix", Protocol: "longpoll", Enabled: false, Settings: map[string]string{}},
		{Name: "dingtalk", Protocol: "websocket", Enabled: false, Settings: map[string]string{}},
		{Name: "feishu", Protocol: "websocket", Enabled: false, Settings: map[string]string{}},
		{Name: "line", Protocol: "webhook", Enabled: false, Settings: map[string]string{}},
		{Name: "onebot", Protocol: "websocket", Enabled: false, Settings: map[string]string{}},
		{Name: "wecom", Protocol: "webhook", Enabled: false, Settings: map[string]string{}},
		{Name: "maixcam", Protocol: "bridge", Enabled: false, Settings: map[string]string{}},
		{Name: "irc", Protocol: "bridge", Enabled: false, Settings: map[string]string{}},
		{Name: "vk", Protocol: "webhook", Enabled: false, Settings: map[string]string{}},
		{Name: "teams_webhook", Protocol: "webhook", Enabled: false, Settings: map[string]string{}},
	}
	for _, item := range defaults {
		if _, ok := g.providerCfg[item.Name]; ok {
			continue
		}
		g.providerCfg[item.Name] = item
	}
}

func (g *Gateway) applyProviderConfig(provider Provider, cfg ProviderConfig) {
	if wp, ok := provider.(*WebhookProvider); ok {
		wp.SetEndpoint(strings.TrimSpace(cfg.Settings["endpoint"]))
		wp.SetAuthToken(strings.TrimSpace(cfg.Settings["token"]))
		return
	}
	if pp, ok := provider.(*ProxyProvider); ok {
		pp.SetEndpoint(strings.TrimSpace(cfg.Settings["endpoint"]))
		pp.SetAuthToken(strings.TrimSpace(cfg.Settings["token"]))
	}
}

type resolvedTarget struct {
	RawTarget string
	Platform  string
	ChatID    string
	ThreadID  string
}

func (g *Gateway) resolveTarget(agentID, target string, event OutboundEvent) (resolvedTarget, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		target = strings.TrimSpace(event.Target)
	}
	if target == "" {
		target = g.copyConfig().DefaultTarget
	}
	if target == "" {
		target = "last"
	}

	if target == "last" {
		g.mu.RLock()
		fallback := g.lastTargets[strings.TrimSpace(agentID)]
		if fallback == "" {
			fallback = g.lastTargets["*"]
		}
		g.mu.RUnlock()
		if fallback != "" {
			target = fallback
		}
	}

	if strings.HasPrefix(target, "binding:") {
		id := strings.TrimSpace(strings.TrimPrefix(target, "binding:"))
		g.mu.RLock()
		binding, ok := g.bindings[id]
		g.mu.RUnlock()
		if !ok {
			return resolvedTarget{}, fmt.Errorf("binding not found: %s", id)
		}
		return resolvedTarget{RawTarget: target, Platform: binding.Platform, ChatID: binding.ChatID, ThreadID: binding.ThreadID}, nil
	}

	parts := strings.Split(target, ":")
	if len(parts) >= 2 {
		platform := strings.ToLower(strings.TrimSpace(parts[0]))
		chatID := strings.TrimSpace(parts[1])
		threadID := ""
		if len(parts) >= 3 {
			threadID = strings.TrimSpace(parts[2])
		}
		if platform != "" && chatID != "" {
			return resolvedTarget{RawTarget: target, Platform: platform, ChatID: chatID, ThreadID: threadID}, nil
		}
	}

	platform := firstText(strings.TrimSpace(event.Platform), "console")
	chatID := firstText(strings.TrimSpace(event.ChatID), "default")
	threadID := strings.TrimSpace(event.ThreadID)
	return resolvedTarget{RawTarget: target, Platform: platform, ChatID: chatID, ThreadID: threadID}, nil
}

func (g *Gateway) getProvider(platform string) (Provider, bool) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	g.mu.RLock()
	defer g.mu.RUnlock()
	if p, ok := g.providers[platform]; ok {
		return p, true
	}
	if p, ok := g.providers["webhook"]; ok {
		if platform != "console" {
			return p, true
		}
	}
	p, ok := g.providers["console"]
	return p, ok
}

func (g *Gateway) sendWithRetry(ctx context.Context, agentID string, provider Provider, event OutboundEvent) (SendResult, error) {
	caps := provider.Capabilities()
	event = applyDegradePolicy(event, caps)
	segments := splitByMaxLen(event.TextMarkdown, caps.MaxTextLen)
	if len(segments) == 0 {
		segments = []string{""}
	}

	backoff := []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}
	var lastErr error
	var final SendResult
	for idx, seg := range segments {
		piece := event
		piece.TextMarkdown = seg
		for attempt := 1; attempt <= 5; attempt++ {
			sessionKey := event.Platform + ":" + event.ChatID + ":" + event.ThreadID
			if !g.rateLimiter.AllowThreeLevel(
				event.Platform, caps.RateLimitPerMinute,
				agentID, maxInt(30, caps.RateLimitPerMinute/2),
				sessionKey, maxInt(12, caps.RateLimitPerMinute/4),
			) {
				lastErr = fmt.Errorf("rate limit exceeded")
				timer := time.NewTimer(1 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return SendResult{}, ctx.Err()
				case <-timer.C:
				}
				continue
			}

			start := time.Now()
			res, err := provider.Send(ctx, piece)
			if err == nil {
				res.RetryCount = attempt - 1
				if res.LatencyMs == 0 {
					res.LatencyMs = time.Since(start).Milliseconds()
				}
				g.health.RecordSuccess(provider.Name(), res.LatencyMs)
				final = res
				g.audit.Log(ctx, agentID, "", "gateway", "send_success", fmt.Sprintf("provider=%s protocol=%s event_id=%s provider_message_id=%s retry_count=%d final_status=%s latency_ms=%d segment=%d/%d", provider.Name(), provider.Protocol(), event.MessageID, res.ProviderMessageID, res.RetryCount, res.Status, res.LatencyMs, idx+1, len(segments)))
				break
			}
			lastErr = err
			if attempt == 5 {
				g.health.RecordError(provider.Name(), time.Since(start).Milliseconds())
				g.audit.Log(ctx, agentID, "", "gateway", "send_failed", fmt.Sprintf("provider=%s protocol=%s event_id=%s retry_count=%d final_status=failed err=%v", provider.Name(), provider.Protocol(), event.MessageID, attempt-1, err))
				break
			}
			timer := time.NewTimer(backoff[minInt(attempt-1, len(backoff)-1)])
			select {
			case <-ctx.Done():
				timer.Stop()
				return SendResult{}, ctx.Err()
			case <-timer.C:
			}
		}
		if lastErr != nil {
			return final, lastErr
		}
	}
	return final, nil
}

func (g *Gateway) setLastTarget(agentID, target string) {
	target = strings.TrimSpace(target)
	if target == "" {
		return
	}
	g.mu.Lock()
	if strings.TrimSpace(agentID) != "" {
		g.lastTargets[strings.TrimSpace(agentID)] = target
	}
	g.lastTargets["*"] = target
	g.mu.Unlock()
	_ = g.saveLastTargets(context.Background())
}

func (g *Gateway) pushDLQ(agentID, target string, event OutboundEvent, sendErr error) {
	if sendErr == nil {
		return
	}
	item := DLQItem{
		ID:         engine.NewID("dlq"),
		AgentID:    strings.TrimSpace(agentID),
		Target:     strings.TrimSpace(target),
		Event:      event,
		Error:      sendErr.Error(),
		RetryCount: 5,
		CreatedAt:  time.Now().UTC(),
	}
	g.mu.Lock()
	g.dlq = append(g.dlq, item)
	if len(g.dlq) > 2000 {
		g.dlq = g.dlq[len(g.dlq)-2000:]
	}
	g.mu.Unlock()
	_ = g.saveDLQ(context.Background())
}

func (g *Gateway) matchRoute(event InboundEvent) (RouteRule, bool) {
	routes := g.ListRoutes()
	for _, rule := range routes {
		if !rule.Enabled {
			continue
		}
		if !routeMatches(rule.Match, event) {
			continue
		}
		return rule, true
	}
	return RouteRule{}, false
}

func routeMatches(match RouteMatch, event InboundEvent) bool {
	if v := strings.TrimSpace(strings.ToLower(match.Platform)); v != "" && v != strings.ToLower(strings.TrimSpace(event.Platform)) {
		return false
	}
	if v := strings.TrimSpace(match.ChatID); v != "" && v != strings.TrimSpace(event.ChatID) {
		return false
	}
	if v := strings.TrimSpace(match.ThreadID); v != "" && v != strings.TrimSpace(event.ThreadID) {
		return false
	}
	if v := strings.TrimSpace(match.SenderID); v != "" && v != strings.TrimSpace(event.SenderID) {
		return false
	}
	if v := strings.TrimSpace(match.EventType); v != "" && v != strings.TrimSpace(event.EventType) {
		return false
	}
	if v := strings.TrimSpace(match.ContentPrefix); v != "" && !strings.HasPrefix(strings.TrimSpace(event.Text), v) {
		return false
	}
	if v := strings.TrimSpace(match.Mention); v != "" {
		found := false
		for _, m := range event.Mentions {
			if strings.TrimSpace(m) == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if v := strings.TrimSpace(match.Regex); v != "" {
		re, err := regexp.Compile(v)
		if err != nil || !re.MatchString(event.Text) {
			return false
		}
	}
	for k, want := range match.Metadata {
		if strings.TrimSpace(want) == "" {
			continue
		}
		if event.Metadata[k] != want {
			return false
		}
	}
	return true
}

func applyDegradePolicy(event OutboundEvent, caps CapabilityProfile) OutboundEvent {
	if !caps.SupportsButtons && len(event.Actions) > 0 {
		var b strings.Builder
		if strings.TrimSpace(event.TextMarkdown) != "" {
			b.WriteString(strings.TrimSpace(event.TextMarkdown))
			b.WriteString("\n\n")
		}
		b.WriteString("可用操作：\n")
		for _, action := range event.Actions {
			name := strings.TrimSpace(action.Label)
			if name == "" {
				name = strings.TrimSpace(action.Value)
			}
			if name == "" {
				continue
			}
			b.WriteString("- ")
			b.WriteString(name)
			if strings.TrimSpace(action.URL) != "" {
				b.WriteString(" (")
				b.WriteString(strings.TrimSpace(action.URL))
				b.WriteString(")")
			}
			b.WriteByte('\n')
		}
		event.TextMarkdown = strings.TrimSpace(b.String())
		event.Actions = nil
	}
	if !caps.SupportsThreads {
		event.ThreadID = ""
	}
	return event
}

func splitByMaxLen(text string, max int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if max <= 0 || len(text) <= max {
		return []string{text}
	}
	out := make([]string, 0, (len(text)/max)+1)
	for len(text) > max {
		split := strings.LastIndex(text[:max], "\n")
		if split < max/2 {
			split = max
		}
		out = append(out, strings.TrimSpace(text[:split]))
		text = strings.TrimSpace(text[split:])
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

func joinTarget(platform, chatID, threadID string) string {
	platform = strings.TrimSpace(strings.ToLower(platform))
	chatID = strings.TrimSpace(chatID)
	threadID = strings.TrimSpace(threadID)
	if platform == "" || chatID == "" {
		return ""
	}
	if threadID == "" {
		return platform + ":" + chatID
	}
	return platform + ":" + chatID + ":" + threadID
}

func firstText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizePresence(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case PresenceThinking:
		return PresenceThinking
	case PresenceTyping:
		return PresenceTyping
	case PresenceExecuting:
		return PresenceExecuting
	case PresenceIdle:
		return PresenceIdle
	default:
		return PresenceTyping
	}
}

func (g *Gateway) inQuietHours(now time.Time) bool {
	cfg := g.copyConfig()
	start := strings.TrimSpace(cfg.QuietHoursStart)
	end := strings.TrimSpace(cfg.QuietHoursEnd)
	if start == "" || end == "" {
		return false
	}
	startMin, ok1 := parseClockMinutes(start)
	endMin, ok2 := parseClockMinutes(end)
	if !ok1 || !ok2 {
		return false
	}
	current := now.In(time.Local).Hour()*60 + now.In(time.Local).Minute()
	if startMin == endMin {
		return false
	}
	if startMin < endMin {
		return current >= startMin && current < endMin
	}
	return current >= startMin || current < endMin
}

func parseClockMinutes(raw string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour := strings.TrimSpace(parts[0])
	minute := strings.TrimSpace(parts[1])
	if hour == "" || minute == "" {
		return 0, false
	}
	h, err := time.Parse("15:04", fmt.Sprintf("%s:%s", hour, minute))
	if err != nil {
		return 0, false
	}
	return h.Hour()*60 + h.Minute(), true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
