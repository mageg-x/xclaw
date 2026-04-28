package gateway

import (
	"sync"
	"time"
)

type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64
	lastRefill time.Time
}

func newTokenBucket(maxTokens, refillPerSecond float64) *tokenBucket {
	now := time.Now().UTC()
	return &tokenBucket{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillPerSecond,
		lastRefill: now,
	}
}

func (b *tokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().UTC()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (b *tokenBucket) Tokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now().UTC()
	elapsed := now.Sub(b.lastRefill).Seconds()
	current := b.tokens + elapsed*b.refillRate
	if current > b.maxTokens {
		current = b.maxTokens
	}
	return current
}

type rateLimiter struct {
	mu      sync.RWMutex
	buckets map[string]*tokenBucket
	configs map[string]rateLimitConfig
}

type rateLimitConfig struct {
	MaxTokens      float64
	RefillPerSecond float64
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*tokenBucket),
		configs: make(map[string]rateLimitConfig),
	}
}

func (r *rateLimiter) Configure(key string, maxTokens, refillPerSecond float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configs[key] = rateLimitConfig{
		MaxTokens:      maxTokens,
		RefillPerSecond: refillPerSecond,
	}
	if b, ok := r.buckets[key]; ok {
		b.mu.Lock()
		b.maxTokens = maxTokens
		b.refillRate = refillPerSecond
		if b.tokens > maxTokens {
			b.tokens = maxTokens
		}
		b.mu.Unlock()
	}
}

func (r *rateLimiter) Allow(key string) bool {
	r.mu.RLock()
	b, ok := r.buckets[key]
	cfg, cfgOk := r.configs[key]
	r.mu.RUnlock()

	if !ok {
		r.mu.Lock()
		b, exists := r.buckets[key]
		if !exists {
			maxT := cfg.MaxTokens
			refill := cfg.RefillPerSecond
			if !cfgOk {
				maxT = 60
				refill = 1.0
			}
			b = newTokenBucket(maxT, refill)
			r.buckets[key] = b
		}
		r.mu.Unlock()
	}

	return b.Allow()
}

func (r *rateLimiter) AllowThreeLevel(platform string, platformLimit int, tenantID string, tenantLimit int, sessionKey string, sessionLimit int) bool {
	if platformLimit > 0 {
		platformKey := "platform:" + platform
		r.ensureBucket(platformKey, float64(platformLimit), float64(platformLimit)/60.0)
		if !r.Allow(platformKey) {
			return false
		}
	}
	if tenantLimit > 0 && tenantID != "" {
		tenantKey := "tenant:" + tenantID + ":" + platform
		r.ensureBucket(tenantKey, float64(tenantLimit), float64(tenantLimit)/60.0)
		if !r.Allow(tenantKey) {
			return false
		}
	}
	if sessionLimit > 0 && sessionKey != "" {
		sessionBucketKey := "session:" + sessionKey
		r.ensureBucket(sessionBucketKey, float64(sessionLimit), float64(sessionLimit)/60.0)
		if !r.Allow(sessionBucketKey) {
			return false
		}
	}
	return true
}

func (r *rateLimiter) ensureBucket(key string, maxTokens, refillPerSecond float64) {
	r.mu.RLock()
	_, ok := r.buckets[key]
	r.mu.RUnlock()
	if !ok {
		r.mu.Lock()
		if _, exists := r.buckets[key]; !exists {
			r.buckets[key] = newTokenBucket(maxTokens, refillPerSecond)
		}
		r.mu.Unlock()
	}
}

func (r *rateLimiter) Cleanup(maxAge time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	for k, b := range r.buckets {
		b.mu.Lock()
		if now.Sub(b.lastRefill) > maxAge {
			delete(r.buckets, k)
		}
		b.mu.Unlock()
	}
}
