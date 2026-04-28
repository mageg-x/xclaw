package engine

import (
	"encoding/json"
	"sync"
	"time"
)

type Event struct {
	Type      string      `json:"type"`
	SessionID string      `json:"session_id"`
	Data      interface{} `json:"data"`
	Sequence  int64       `json:"sequence"`
	Timestamp time.Time   `json:"timestamp"`
}

type EventFrame struct {
	Sequence int64
	Payload  []byte
}

type EventHub struct {
	mu          sync.RWMutex
	subscribers map[string]map[chan EventFrame]struct{}
	history     map[string][]EventFrame
	nextSeq     int64
	maxHistory  int
}

func NewEventHub() *EventHub {
	return &EventHub{
		subscribers: make(map[string]map[chan EventFrame]struct{}),
		history:     make(map[string][]EventFrame),
		maxHistory:  512,
	}
}

func (h *EventHub) Subscribe(sessionID string) chan EventFrame {
	ch, _ := h.SubscribeSince(sessionID, 0)
	return ch
}

func (h *EventHub) SubscribeSince(sessionID string, afterSeq int64) (chan EventFrame, []EventFrame) {
	ch := make(chan EventFrame, 128)

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subscribers[sessionID]; !ok {
		h.subscribers[sessionID] = make(map[chan EventFrame]struct{})
	}
	h.subscribers[sessionID][ch] = struct{}{}

	history := h.history[sessionID]
	if len(history) == 0 || afterSeq <= 0 {
		return ch, nil
	}

	backlog := make([]EventFrame, 0, len(history))
	for _, item := range history {
		if item.Sequence > afterSeq {
			backlog = append(backlog, item)
		}
	}
	return ch, backlog
}

func (h *EventHub) Unsubscribe(sessionID string, ch chan EventFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()

	bucket, ok := h.subscribers[sessionID]
	if !ok {
		close(ch)
		return
	}
	delete(bucket, ch)
	close(ch)
	if len(bucket) == 0 {
		delete(h.subscribers, sessionID)
	}
}

func (h *EventHub) Publish(event Event) {
	h.mu.Lock()
	h.nextSeq++
	event.Sequence = h.nextSeq
	event.Timestamp = time.Now().UTC()
	payload, err := json.Marshal(event)
	if err != nil {
		h.mu.Unlock()
		return
	}

	frame := EventFrame{
		Sequence: event.Sequence,
		Payload:  payload,
	}
	history := append(h.history[event.SessionID], frame)
	if len(history) > h.maxHistory {
		history = append([]EventFrame(nil), history[len(history)-h.maxHistory:]...)
	}
	h.history[event.SessionID] = history

	subs := h.subscribers[event.SessionID]
	copies := make([]chan EventFrame, 0, len(subs))
	for ch := range subs {
		copies = append(copies, ch)
	}
	h.mu.Unlock()

	for _, ch := range copies {
		select {
		case ch <- frame:
		default:
		}
	}
}
