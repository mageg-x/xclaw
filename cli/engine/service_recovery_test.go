package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"xclaw/cli/audit"
	"xclaw/cli/db"
	"xclaw/cli/models"
	"xclaw/cli/queue"
)

func TestCollectPendingUserMessages(t *testing.T) {
	t.Parallel()

	messages := []models.Message{
		{ID: "u1", Role: "user"},
		{ID: "a1", Role: "assistant"},
		{ID: "u2", Role: "user"},
		{ID: "u3", Role: "user"},
		{ID: "a2", Role: "assistant"},
		{ID: "s1", Role: "system"},
		{ID: "u4", Role: "user"},
	}

	pending := collectPendingUserMessages(messages)
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending user messages, got %d", len(pending))
	}
	if pending[0].ID != "u3" || pending[1].ID != "u4" {
		t.Fatalf("unexpected pending message order: %#v", pending)
	}
}

func TestEncodeDecodeMessageAutoApprove(t *testing.T) {
	t.Parallel()

	raw, err := encodeMessageMetadata(SendMessageInput{
		Content:     "hello",
		AutoApprove: true,
	})
	if err != nil {
		t.Fatalf("encodeMessageMetadata() error = %v", err)
	}
	if !decodeMessageAutoApprove(raw) {
		t.Fatalf("expected auto_approve metadata to decode as true")
	}

	raw, err = encodeMessageMetadata(SendMessageInput{
		Content:     "hello",
		AutoApprove: false,
	})
	if err != nil {
		t.Fatalf("encodeMessageMetadata() error = %v", err)
	}
	if decodeMessageAutoApprove(raw) {
		t.Fatalf("expected auto_approve metadata to decode as false")
	}
}

func TestRecoverInterruptedSessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "xclaw.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	agent := models.Agent{
		ID:                "agent_test",
		Name:              "test",
		Emoji:             "T",
		Description:       "test",
		SystemInstruction: "test",
		ModelProvider:     "local",
		ModelName:         "local",
		WorkspacePath:     t.TempDir(),
		Tools:             []string{},
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
	if err := store.CreateAgent(context.Background(), agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	session := models.Session{
		ID:        "sess_test",
		AgentID:   agent.ID,
		Title:     "test",
		IsMain:    true,
		Status:    "running",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := store.CreateSession(context.Background(), session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	writeMessage := func(id, role, metadata string, offset int) {
		t.Helper()
		if err := store.CreateMessage(context.Background(), models.Message{
			ID:        id,
			SessionID: session.ID,
			Role:      role,
			Content:   id,
			Metadata:  metadata,
			CreatedAt: time.Now().UTC().Add(time.Duration(offset) * time.Millisecond),
		}); err != nil {
			t.Fatalf("create message %s: %v", id, err)
		}
	}

	metaTrue, _ := encodeMessageMetadata(SendMessageInput{Content: "u1", AutoApprove: true})
	metaFalse, _ := encodeMessageMetadata(SendMessageInput{Content: "u2", AutoApprove: false})
	writeMessage("u1", "user", metaTrue, 0)
	writeMessage("a1", "assistant", `{}`, 1)
	writeMessage("u2", "user", metaFalse, 2)
	writeMessage("u3", "user", metaTrue, 3)

	laneQueue := queue.NewLaneQueue(1)
	defer laneQueue.Close()

	svc := NewService(store, nil, laneQueue, nil, audit.NewLogger(store), NewEventHub(), NewPlanCache(store), nil)
	resumeCalls := make(chan string, 4)
	svc.SetResumeRunner(func(ctx context.Context, sess models.Session, msg models.Message, autoApprove bool) error {
		resumeCalls <- fmt.Sprintf("%s:%t", msg.ID, autoApprove)
		return nil
	})

	resumed, err := svc.RecoverInterruptedSessions(context.Background())
	if err != nil {
		t.Fatalf("RecoverInterruptedSessions() error = %v", err)
	}
	if resumed != 2 {
		t.Fatalf("RecoverInterruptedSessions() resumed = %d, want 2", resumed)
	}

	got := make([]string, 0, 2)
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case call := <-resumeCalls:
			got = append(got, call)
		case <-deadline:
			t.Fatalf("timed out waiting for resumed calls, got %#v", got)
		}
	}

	if got[0] != "u2:false" || got[1] != "u3:true" {
		t.Fatalf("unexpected resumed call sequence: %#v", got)
	}

	updated, err := store.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.Status != "recovering" {
		t.Fatalf("expected recovered session status to become recovering, got %q", updated.Status)
	}
}

func TestSetSessionStatusPublishesEvent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "xclaw.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	agent := models.Agent{
		ID:                "agent_status",
		Name:              "status",
		Emoji:             "S",
		Description:       "status",
		SystemInstruction: "status",
		ModelProvider:     "local",
		ModelName:         "local",
		WorkspacePath:     t.TempDir(),
		Tools:             []string{},
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
	if err := store.CreateAgent(context.Background(), agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	session := models.Session{
		ID:        "sess_status",
		AgentID:   agent.ID,
		Title:     "status",
		IsMain:    true,
		Status:    "idle",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := store.CreateSession(context.Background(), session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	events := NewEventHub()
	svc := NewService(store, nil, queue.NewLaneQueue(1), nil, audit.NewLogger(store), events, NewPlanCache(store), nil)
	ch := events.Subscribe(session.ID)
	defer events.Unsubscribe(session.ID, ch)

	svc.setSessionStatus(context.Background(), session.ID, "recovering")

	frame := <-ch
	var evt Event
	if err := json.Unmarshal(frame.Payload, &evt); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if evt.Type != "session.status" {
		t.Fatalf("unexpected event type: %s", evt.Type)
	}
	data, ok := evt.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected event data type: %T", evt.Data)
	}
	if got := fmt.Sprint(data["status"]); got != "recovering" {
		t.Fatalf("unexpected session.status payload: %v", data)
	}

	updated, err := store.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.Status != "recovering" {
		t.Fatalf("expected stored session status to be recovering, got %q", updated.Status)
	}
}
