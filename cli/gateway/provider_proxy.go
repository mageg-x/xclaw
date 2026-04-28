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

type ProxyProvider struct {
	name      string
	protocol  string
	http      *http.Client
	endpoint  string
	authToken string

	mu      sync.RWMutex
	handler func(context.Context, InboundEvent) error
	started bool
}

func NewProxyProvider(name, protocol string) *ProxyProvider {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = "proxy"
	}
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		protocol = "bridge"
	}
	return &ProxyProvider{
		name:     name,
		protocol: protocol,
		http: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (p *ProxyProvider) Name() string {
	return p.name
}

func (p *ProxyProvider) Protocol() string {
	return p.protocol
}

func (p *ProxyProvider) Capabilities() CapabilityProfile {
	caps := CapabilityProfile{
		SupportsEdit:          false,
		SupportsButtons:       true,
		SupportsThreads:       true,
		SupportsTyping:        true,
		SupportsAudio:         true,
		SupportsStreamingText: true,
		MaxTextLen:            8000,
		RateLimitPerMinute:    90,
	}
	switch p.protocol {
	case "webhook":
		caps.RateLimitPerMinute = 120
	case "websocket":
		caps.RateLimitPerMinute = 180
	case "longpoll":
		caps.RateLimitPerMinute = 100
	case "bridge":
		caps.RateLimitPerMinute = 80
	}
	return caps
}

func (p *ProxyProvider) Start(ctx context.Context) error {
	_ = ctx
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return nil
}

func (p *ProxyProvider) Stop(ctx context.Context) error {
	_ = ctx
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = false
	return nil
}

func (p *ProxyProvider) SetInboundHandler(handler func(context.Context, InboundEvent) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler = handler
}

func (p *ProxyProvider) SetEndpoint(endpoint string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endpoint = strings.TrimSpace(endpoint)
}

func (p *ProxyProvider) SetAuthToken(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authToken = strings.TrimSpace(token)
}

func (p *ProxyProvider) Send(ctx context.Context, event OutboundEvent) (SendResult, error) {
	p.mu.RLock()
	started := p.started
	endpoint := p.endpoint
	token := p.authToken
	p.mu.RUnlock()
	if !started {
		return SendResult{}, fmt.Errorf("provider not started")
	}
	if endpoint == "" {
		text := strings.TrimSpace(event.TextMarkdown)
		if text == "" {
			text = "(" + firstText(event.Phase, "empty") + ")"
		}
		fmt.Printf("[gateway/%s:%s] %s\n", p.name, p.protocol, text)
		return SendResult{ProviderMessageID: fmt.Sprintf("%s-%d", p.name, time.Now().UnixNano()), Status: "ok"}, nil
	}

	body, err := json.Marshal(event)
	if err != nil {
		return SendResult{}, fmt.Errorf("marshal outbound event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return SendResult{}, fmt.Errorf("create provider request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	start := time.Now()
	resp, err := p.http.Do(req)
	if err != nil {
		return SendResult{}, fmt.Errorf("provider request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return SendResult{}, fmt.Errorf("provider status %d", resp.StatusCode)
	}
	return SendResult{
		ProviderMessageID: fmt.Sprintf("%s-%d", p.name, time.Now().UnixNano()),
		Status:            "ok",
		LatencyMs:         time.Since(start).Milliseconds(),
	}, nil
}

func (p *ProxyProvider) Health(ctx context.Context) ProviderHealth {
	_ = ctx
	p.mu.RLock()
	started := p.started
	endpoint := p.endpoint
	p.mu.RUnlock()
	status := "down"
	detail := "stopped"
	if started {
		status = "up"
		detail = "ready"
		if endpoint == "" {
			detail = "ready (console fallback)"
		}
	}
	return ProviderHealth{Status: status, Detail: detail, UpdatedAt: time.Now().UTC()}
}
