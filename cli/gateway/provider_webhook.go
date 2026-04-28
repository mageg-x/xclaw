package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type WebhookProvider struct {
	name      string
	protocol  string
	http      *http.Client
	endpoint  string
	authToken string

	mu      sync.RWMutex
	handler func(context.Context, InboundEvent) error
	started bool
}

func NewWebhookProvider(name, endpoint string) *WebhookProvider {
	if strings.TrimSpace(name) == "" {
		name = "webhook"
	}
	return &WebhookProvider{
		name:     name,
		protocol: "webhook",
		http: &http.Client{
			Timeout: 20 * time.Second,
		},
		endpoint: strings.TrimSpace(endpoint),
	}
}

func (p *WebhookProvider) SetEndpoint(endpoint string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endpoint = strings.TrimSpace(endpoint)
}

func (p *WebhookProvider) SetAuthToken(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authToken = strings.TrimSpace(token)
}

func (p *WebhookProvider) Name() string {
	return p.name
}

func (p *WebhookProvider) Protocol() string {
	return p.protocol
}

func (p *WebhookProvider) Capabilities() CapabilityProfile {
	return CapabilityProfile{
		SupportsEdit:          false,
		SupportsButtons:       true,
		SupportsThreads:       true,
		SupportsTyping:        true,
		SupportsAudio:         true,
		SupportsStreamingText: true,
		MaxTextLen:            16000,
		RateLimitPerMinute:    180,
	}
}

func (p *WebhookProvider) Start(ctx context.Context) error {
	_ = ctx
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return nil
}

func (p *WebhookProvider) Stop(ctx context.Context) error {
	_ = ctx
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = false
	return nil
}

func (p *WebhookProvider) SetInboundHandler(handler func(context.Context, InboundEvent) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler = handler
}

func (p *WebhookProvider) Send(ctx context.Context, event OutboundEvent) (SendResult, error) {
	p.mu.RLock()
	endpoint := p.endpoint
	token := p.authToken
	started := p.started
	p.mu.RUnlock()
	if !started {
		return SendResult{}, fmt.Errorf("provider not started")
	}
	if endpoint == "" {
		return SendResult{}, fmt.Errorf("webhook endpoint is empty")
	}

	body, err := json.Marshal(event)
	if err != nil {
		return SendResult{}, fmt.Errorf("marshal outbound event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return SendResult{}, fmt.Errorf("create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	start := time.Now()
	resp, err := p.http.Do(req)
	if err != nil {
		return SendResult{}, fmt.Errorf("webhook request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return SendResult{}, fmt.Errorf("webhook status %d", resp.StatusCode)
	}

	return SendResult{
		ProviderMessageID: fmt.Sprintf("%s-%d", p.name, time.Now().UnixNano()),
		Status:            "ok",
		LatencyMs:         time.Since(start).Milliseconds(),
	}, nil
}

func (p *WebhookProvider) Health(ctx context.Context) ProviderHealth {
	_ = ctx
	p.mu.RLock()
	defer p.mu.RUnlock()
	status := "down"
	detail := "webhook provider disabled"
	if p.started {
		status = "up"
		detail = "ready"
	}
	if p.endpoint == "" {
		detail = "missing endpoint"
	}
	return ProviderHealth{Status: status, Detail: detail, UpdatedAt: time.Now().UTC()}
}
