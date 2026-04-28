package gateway

import (
	"context"
	"time"
)

type Attachment struct {
	Type     string `json:"type"`
	URL      string `json:"url"`
	FileName string `json:"file_name"`
	MIME     string `json:"mime"`
	Size     int64  `json:"size"`
}

type Action struct {
	Label string `json:"label"`
	Value string `json:"value"`
	URL   string `json:"url"`
}

type InboundEvent struct {
	EventID     string            `json:"event_id"`
	Platform    string            `json:"platform"`
	Protocol    string            `json:"protocol"`
	EventType   string            `json:"event_type"`
	SenderID    string            `json:"sender_id"`
	SenderName  string            `json:"sender_name"`
	ChatID      string            `json:"chat_id"`
	ThreadID    string            `json:"thread_id"`
	ReplyToID   string            `json:"reply_to_id"`
	Mentions    []string          `json:"mentions"`
	Text        string            `json:"text"`
	Attachments []Attachment      `json:"attachments"`
	Metadata    map[string]string `json:"metadata"`
	OccurredAt  time.Time         `json:"occurred_at"`
	RawPayload  []byte            `json:"raw_payload"`
}

type OutboundEvent struct {
	MessageID      string       `json:"message_id"`
	Target         string       `json:"target"`
	Platform       string       `json:"platform"`
	ChatID         string       `json:"chat_id"`
	ThreadID       string       `json:"thread_id"`
	ReplyToID      string       `json:"reply_to_id"`
	TextMarkdown   string       `json:"text_markdown"`
	Actions        []Action     `json:"actions"`
	Attachments    []Attachment `json:"attachments"`
	Stream         bool         `json:"stream"`
	Phase          string       `json:"phase"`
	IdempotencyKey string       `json:"idempotency_key"`
	Priority       string       `json:"priority"`
	TTLSeconds     int          `json:"ttl_seconds"`
}

type PresenceEvent struct {
	Platform string `json:"platform"`
	ChatID   string `json:"chat_id"`
	ThreadID string `json:"thread_id"`
	State    string `json:"state"`
	Message  string `json:"message"`
	TTLMs    int    `json:"ttl_ms"`
}

type SendResult struct {
	ProviderMessageID string `json:"provider_message_id"`
	Status            string `json:"status"`
	RetryCount        int    `json:"retry_count"`
	LatencyMs         int64  `json:"latency_ms"`
}

type CapabilityProfile struct {
	SupportsEdit          bool `json:"supports_edit"`
	SupportsButtons       bool `json:"supports_buttons"`
	SupportsThreads       bool `json:"supports_threads"`
	SupportsTyping        bool `json:"supports_typing"`
	SupportsAudio         bool `json:"supports_audio"`
	SupportsStreamingText bool `json:"supports_streaming_text"`
	MaxTextLen            int  `json:"max_text_len"`
	RateLimitPerMinute    int  `json:"rate_limit_per_minute"`
}

type ProviderHealth struct {
	Status    string            `json:"status"`
	Detail    string            `json:"detail"`
	UpdatedAt time.Time         `json:"updated_at"`
	LatencyMs int64             `json:"latency_ms"`
	ErrorRate float64           `json:"error_rate"`
	Metrics   map[string]any    `json:"metrics"`
}

type Provider interface {
	Name() string
	Protocol() string
	Capabilities() CapabilityProfile
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	SetInboundHandler(handler func(context.Context, InboundEvent) error)
	Send(ctx context.Context, event OutboundEvent) (SendResult, error)
	Health(ctx context.Context) ProviderHealth
}

type ProviderConfig struct {
	Name     string            `json:"name"`
	Protocol string            `json:"protocol"`
	Enabled  bool              `json:"enabled"`
	Settings map[string]string `json:"settings"`
}

type Binding struct {
	ID          string            `json:"id"`
	AgentID     string            `json:"agent_id"`
	Platform    string            `json:"platform"`
	ChatID      string            `json:"chat_id"`
	ThreadID    string            `json:"thread_id"`
	SenderID    string            `json:"sender_id"`
	DisplayName string            `json:"display_name"`
	Enabled     bool              `json:"enabled"`
	Metadata    map[string]string `json:"metadata"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type RouteMatch struct {
	Platform      string            `json:"platform"`
	ChatID        string            `json:"chat_id"`
	ThreadID      string            `json:"thread_id"`
	SenderID      string            `json:"sender_id"`
	EventType     string            `json:"event_type"`
	ContentPrefix string            `json:"content_prefix"`
	Regex         string            `json:"regex"`
	Mention       string            `json:"mention"`
	Metadata      map[string]string `json:"metadata"`
}

type RouteAction struct {
	TargetAgent   string `json:"target_agent"`
	Target        string `json:"target"`
	TargetSession string `json:"target_session"`
	StripPrefix   bool   `json:"strip_prefix"`
	CreateSession bool   `json:"create_session"`
	Priority      string `json:"priority"`
}

type RouteRule struct {
	Name     string      `json:"name"`
	Priority int         `json:"priority"`
	Match    RouteMatch  `json:"match"`
	Action   RouteAction `json:"action"`
	Enabled  bool        `json:"enabled"`
}

type Config struct {
	DefaultTarget   string   `json:"default_target"`
	FallbackTargets []string `json:"fallback_targets"`
	QuietHoursStart string   `json:"quiet_hours_start"`
	QuietHoursEnd   string   `json:"quiet_hours_end"`
}

type DLQItem struct {
	ID         string        `json:"id"`
	AgentID    string        `json:"agent_id"`
	Target     string        `json:"target"`
	Event      OutboundEvent `json:"event"`
	Error      string        `json:"error"`
	RetryCount int           `json:"retry_count"`
	CreatedAt  time.Time     `json:"created_at"`
}

const (
	PresenceThinking  = "thinking"
	PresenceTyping    = "typing"
	PresenceExecuting = "executing"
	PresenceIdle      = "idle"
)
