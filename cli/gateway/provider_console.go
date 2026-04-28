package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type ConsoleProvider struct {
	name     string
	protocol string

	mu      sync.RWMutex
	handler func(context.Context, InboundEvent) error
	started bool
}

func NewConsoleProvider(name string) *ConsoleProvider {
	if name == "" {
		name = "console"
	}
	return &ConsoleProvider{name: name, protocol: "bridge"}
}

func (p *ConsoleProvider) Name() string {
	return p.name
}

func (p *ConsoleProvider) Protocol() string {
	return p.protocol
}

func (p *ConsoleProvider) Capabilities() CapabilityProfile {
	return CapabilityProfile{
		SupportsEdit:          false,
		SupportsButtons:       false,
		SupportsThreads:       true,
		SupportsTyping:        true,
		SupportsAudio:         false,
		SupportsStreamingText: true,
		MaxTextLen:            16000,
		RateLimitPerMinute:    120,
	}
}

func (p *ConsoleProvider) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = ctx
	p.started = true
	return nil
}

func (p *ConsoleProvider) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = ctx
	p.started = false
	return nil
}

func (p *ConsoleProvider) SetInboundHandler(handler func(context.Context, InboundEvent) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler = handler
}

func (p *ConsoleProvider) Send(ctx context.Context, event OutboundEvent) (SendResult, error) {
	_ = ctx
	text := event.TextMarkdown
	if text == "" && event.Phase != "" {
		text = fmt.Sprintf("[%s]", event.Phase)
	}
	if text == "" {
		text = "(empty message)"
	}
	fmt.Printf("[gateway/%s] %s:%s:%s %s\n", p.name, event.Platform, event.ChatID, event.ThreadID, text)
	return SendResult{
		ProviderMessageID: fmt.Sprintf("%s-%d", p.name, time.Now().UnixNano()),
		Status:            "ok",
		RetryCount:        0,
		LatencyMs:         0,
	}, nil
}

func (p *ConsoleProvider) Health(ctx context.Context) ProviderHealth {
	_ = ctx
	p.mu.RLock()
	started := p.started
	p.mu.RUnlock()
	status := "down"
	if started {
		status = "up"
	}
	return ProviderHealth{Status: status, Detail: "console provider", UpdatedAt: time.Now().UTC()}
}
