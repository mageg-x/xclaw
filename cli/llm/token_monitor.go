package llm

import (
	"context"
	"strings"
	"sync"
	"time"
)

// TokenUsage tracks token consumption for a single request
type TokenUsage struct {
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	Timestamp        time.Time `json:"timestamp"`
	SessionID        string    `json:"session_id"`
	AgentID          string    `json:"agent_id"`
}

// TokenMonitor tracks and reports token usage
type TokenMonitor struct {
	mu      sync.RWMutex
	history []TokenUsage
	limits  map[string]int // agent_id -> daily limit
}

// NewTokenMonitor creates a new token monitor
func NewTokenMonitor() *TokenMonitor {
	return &TokenMonitor{
		history: make([]TokenUsage, 0, 1000),
		limits:  make(map[string]int),
	}
}

// Record logs token usage for a request
func (m *TokenMonitor) Record(usage TokenUsage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = append(m.history, usage)

	// Trim history if too large (keep last 10000)
	if len(m.history) > 10000 {
		m.history = m.history[len(m.history)-10000:]
	}
}

// SetLimit sets a daily token limit for an agent
func (m *TokenMonitor) SetLimit(agentID string, limit int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limits[agentID] = limit
}

// CheckLimit returns true if the agent has exceeded its daily limit
func (m *TokenMonitor) CheckLimit(agentID string) (bool, int, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	limit, hasLimit := m.limits[agentID]
	if !hasLimit {
		return false, 0, 0
	}

	today := time.Now().Truncate(24 * time.Hour)
	used := 0
	for _, u := range m.history {
		if u.AgentID == agentID && u.Timestamp.After(today) {
			used += u.TotalTokens
		}
	}

	return used >= limit, used, limit
}

// GetStats returns usage statistics for an agent
func (m *TokenMonitor) GetStats(agentID string) map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	today := time.Now().Truncate(24 * time.Hour)
	weekAgo := today.Add(-7 * 24 * time.Hour)

	var todayTokens, weekTokens, totalTokens int
	var requestCount int
	providerBreakdown := make(map[string]int)
	modelBreakdown := make(map[string]int)

	for _, u := range m.history {
		if u.AgentID != agentID {
			continue
		}
		requestCount++
		totalTokens += u.TotalTokens
		providerBreakdown[u.Provider] += u.TotalTokens
		modelBreakdown[u.Model] += u.TotalTokens

		if u.Timestamp.After(today) {
			todayTokens += u.TotalTokens
		}
		if u.Timestamp.After(weekAgo) {
			weekTokens += u.TotalTokens
		}
	}

	return map[string]any{
		"agent_id":           agentID,
		"today_tokens":       todayTokens,
		"week_tokens":        weekTokens,
		"total_tokens":       totalTokens,
		"request_count":      requestCount,
		"provider_breakdown": providerBreakdown,
		"model_breakdown":    modelBreakdown,
	}
}

// GetGlobalStats returns global usage statistics
func (m *TokenMonitor) GetGlobalStats() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	today := time.Now().Truncate(24 * time.Hour)
	var todayTokens, totalTokens int
	agentBreakdown := make(map[string]int)
	providerBreakdown := make(map[string]int)

	for _, u := range m.history {
		totalTokens += u.TotalTokens
		agentBreakdown[u.AgentID] += u.TotalTokens
		providerBreakdown[u.Provider] += u.TotalTokens
		if u.Timestamp.After(today) {
			todayTokens += u.TotalTokens
		}
	}

	return map[string]any{
		"today_tokens":       todayTokens,
		"total_tokens":       totalTokens,
		"total_requests":     len(m.history),
		"agent_breakdown":    agentBreakdown,
		"provider_breakdown": providerBreakdown,
	}
}

// MonitoredGateway wraps a Gateway and tracks token usage
type MonitoredGateway struct {
	inner   Gateway
	monitor *TokenMonitor
}

// NewMonitoredGateway creates a token-monitoring gateway wrapper
func NewMonitoredGateway(inner Gateway, monitor *TokenMonitor) *MonitoredGateway {
	return &MonitoredGateway{inner: inner, monitor: monitor}
}

func (g *MonitoredGateway) Generate(ctx context.Context, provider, model, prompt string) (string, error) {
	// Check limit before request
	// Note: agentID/sessionID would need to be passed through context in production

	reply, err := g.inner.Generate(ctx, provider, model, prompt)
	if err != nil {
		return "", err
	}

	// Estimate tokens (production should get actual counts from API response)
	promptTokens := g.inner.CountTokens(prompt)
	completionTokens := g.inner.CountTokens(reply)

	g.monitor.Record(TokenUsage{
		Provider:         provider,
		Model:            model,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		Timestamp:        time.Now().UTC(),
	})

	return reply, nil
}

func (g *MonitoredGateway) GenerateStream(ctx context.Context, provider, model, prompt string, handler func(chunk string)) error {
	var fullReply strings.Builder

	wrapHandler := func(chunk string) {
		fullReply.WriteString(chunk)
		handler(chunk)
	}

	err := g.inner.GenerateStream(ctx, provider, model, prompt, wrapHandler)
	if err != nil {
		return err
	}

	promptTokens := g.inner.CountTokens(prompt)
	completionTokens := g.inner.CountTokens(fullReply.String())

	g.monitor.Record(TokenUsage{
		Provider:         provider,
		Model:            model,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		Timestamp:        time.Now().UTC(),
	})

	return nil
}

func (g *MonitoredGateway) CountTokens(text string) int {
	return g.inner.CountTokens(text)
}
