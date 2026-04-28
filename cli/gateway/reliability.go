package gateway

import (
	"sync"
	"time"
)

type dedupeCache struct {
	mu    sync.Mutex
	items map[string]time.Time
}

func newDedupeCache() *dedupeCache {
	return &dedupeCache{items: make(map[string]time.Time)}
}

func (c *dedupeCache) purge(now time.Time) {
	for k, v := range c.items {
		if now.After(v) {
			delete(c.items, k)
		}
	}
}

func (c *dedupeCache) Seen(key string, ttl time.Duration) bool {
	if key == "" {
		return false
	}
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purge(now)
	if until, ok := c.items[key]; ok && now.Before(until) {
		return true
	}
	expires := now.Add(ttl)
	c.items[key] = expires
	return false
}

func (c *dedupeCache) Exists(key string) bool {
	if key == "" {
		return false
	}
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purge(now)
	until, ok := c.items[key]
	return ok && now.Before(until)
}

func (c *dedupeCache) Mark(key string, ttl time.Duration) {
	if key == "" {
		return
	}
	now := time.Now().UTC()
	c.mu.Lock()
	c.purge(now)
	c.items[key] = now.Add(ttl)
	c.mu.Unlock()
}

type reliabilityManager struct {
	inboundDedupe *dedupeCache
	outboundIdem  *dedupeCache
}

func newReliabilityManager() *reliabilityManager {
	return &reliabilityManager{
		inboundDedupe: newDedupeCache(),
		outboundIdem:  newDedupeCache(),
	}
}

func (r *reliabilityManager) SeenInbound(platform, eventID string) bool {
	return r.inboundDedupe.Seen(platform+":"+eventID, 24*time.Hour)
}

func (r *reliabilityManager) SeenOutbound(idempotencyKey string) bool {
	return r.outboundIdem.Exists(idempotencyKey)
}

func (r *reliabilityManager) MarkOutbound(idempotencyKey string) {
	r.outboundIdem.Mark(idempotencyKey, 24*time.Hour)
}
