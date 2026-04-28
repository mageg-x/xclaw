package engine

import (
	"encoding/json"
	"testing"
)

func TestEventHubSubscribeSince(t *testing.T) {
	t.Parallel()

	hub := NewEventHub()
	hub.Publish(Event{Type: "assistant.start", SessionID: "sess_1", Data: map[string]string{"state": "thinking"}})
	hub.Publish(Event{Type: "assistant.delta", SessionID: "sess_1", Data: map[string]string{"chunk": "hello"}})
	hub.Publish(Event{Type: "assistant.done", SessionID: "sess_1", Data: map[string]string{"message_id": "msg_1"}})

	ch, backlog := hub.SubscribeSince("sess_1", 1)
	defer hub.Unsubscribe("sess_1", ch)

	if len(backlog) != 2 {
		t.Fatalf("expected 2 replay frames, got %d", len(backlog))
	}
	if backlog[0].Sequence != 2 || backlog[1].Sequence != 3 {
		t.Fatalf("unexpected replay sequence order: %#v", backlog)
	}

	var first Event
	if err := json.Unmarshal(backlog[0].Payload, &first); err != nil {
		t.Fatalf("unmarshal replay payload: %v", err)
	}
	if first.Type != "assistant.delta" || first.Sequence != 2 {
		t.Fatalf("unexpected first replay event: %#v", first)
	}
}

func TestEventHubLivePublishSequence(t *testing.T) {
	t.Parallel()

	hub := NewEventHub()
	ch := hub.Subscribe("sess_live")
	defer hub.Unsubscribe("sess_live", ch)

	hub.Publish(Event{Type: "presence", SessionID: "sess_live", Data: map[string]string{"state": "typing"}})
	frame := <-ch

	if frame.Sequence <= 0 {
		t.Fatalf("expected positive sequence, got %d", frame.Sequence)
	}

	var event Event
	if err := json.Unmarshal(frame.Payload, &event); err != nil {
		t.Fatalf("unmarshal live payload: %v", err)
	}
	if event.Sequence != frame.Sequence {
		t.Fatalf("payload sequence %d != frame sequence %d", event.Sequence, frame.Sequence)
	}
}
