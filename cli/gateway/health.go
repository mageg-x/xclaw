package gateway

import (
	"sync"
	"time"
)

type healthRecord struct {
	successCount int64
	errorCount   int64
	totalLatency int64
	lastSuccess  time.Time
	lastError    time.Time
	lastLatency  int64
}

type providerHealthTracker struct {
	mu      sync.RWMutex
	records map[string]*healthRecord
}

func newProviderHealthTracker() *providerHealthTracker {
	return &providerHealthTracker{records: make(map[string]*healthRecord)}
}

func (t *providerHealthTracker) RecordSuccess(provider string, latencyMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.getOrCreate(provider)
	r.successCount++
	r.totalLatency += latencyMs
	r.lastLatency = latencyMs
	r.lastSuccess = time.Now().UTC()
}

func (t *providerHealthTracker) RecordError(provider string, latencyMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.getOrCreate(provider)
	r.errorCount++
	r.totalLatency += latencyMs
	r.lastLatency = latencyMs
	r.lastError = time.Now().UTC()
}

func (t *providerHealthTracker) Enrich(provider string, base ProviderHealth) ProviderHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.records[provider]
	if !ok {
		base.LatencyMs = 0
		base.ErrorRate = 0
		base.Metrics = map[string]any{}
		return base
	}

	total := r.successCount + r.errorCount
	avgLatency := int64(0)
	if total > 0 {
		avgLatency = r.totalLatency / total
	}
	errorRate := 0.0
	if total > 0 {
		errorRate = float64(r.errorCount) / float64(total)
	}

	metrics := map[string]any{
		"success_count": r.successCount,
		"error_count":   r.errorCount,
		"avg_latency":   avgLatency,
		"last_latency":  r.lastLatency,
	}
	if !r.lastSuccess.IsZero() {
		metrics["last_success"] = r.lastSuccess.Format(time.RFC3339)
	}
	if !r.lastError.IsZero() {
		metrics["last_error"] = r.lastError.Format(time.RFC3339)
	}

	base.LatencyMs = avgLatency
	base.ErrorRate = errorRate
	base.Metrics = metrics
	return base
}

func (t *providerHealthTracker) getOrCreate(provider string) *healthRecord {
	if r, ok := t.records[provider]; ok {
		return r
	}
	r := &healthRecord{}
	t.records[provider] = r
	return r
}
