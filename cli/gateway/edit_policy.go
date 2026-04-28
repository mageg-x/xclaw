package gateway

import (
	"sync"
	"time"
)

type EditPolicy struct {
	MaxEdits      int
	EditWindow    time.Duration
	EditThreshold int
}

func DefaultEditPolicy() EditPolicy {
	return EditPolicy{
		MaxEdits:      10,
		EditWindow:    24 * time.Hour,
		EditThreshold: 3,
	}
}

func (p EditPolicy) ShouldEdit(original, newContent string, editCount int, firstSent time.Time) bool {
	if editCount >= p.MaxEdits {
		return false
	}
	if time.Since(firstSent) > p.EditWindow {
		return false
	}
	diff := len(newContent) - len(original)
	if diff < 0 {
		diff = -diff
	}
	if diff < p.EditThreshold {
		return false
	}
	return true
}

type editTracker struct {
	mu       sync.RWMutex
	records  map[string]*editRecord
	policy   EditPolicy
}

type editRecord struct {
	EditCount int
	FirstSent time.Time
	LastText  string
}

func newEditTracker(policy EditPolicy) *editTracker {
	return &editTracker{
		records: make(map[string]*editRecord),
		policy:  policy,
	}
}

func (t *editTracker) CheckAndRecord(messageID, text string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	r, ok := t.records[messageID]
	if !ok {
		t.records[messageID] = &editRecord{
			EditCount: 0,
			FirstSent: time.Now().UTC(),
			LastText:  text,
		}
		return true
	}

	if !t.policy.ShouldEdit(r.LastText, text, r.EditCount, r.FirstSent) {
		return false
	}

	r.EditCount++
	r.LastText = text
	return true
}

func (t *editTracker) GetRecord(messageID string) (editRecord, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.records[messageID]
	if !ok {
		return editRecord{}, false
	}
	return *r, true
}

func (t *editTracker) Cleanup(maxAge time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	for k, r := range t.records {
		if now.Sub(r.FirstSent) > maxAge {
			delete(t.records, k)
		}
	}
}
