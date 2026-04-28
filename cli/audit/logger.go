package audit

import (
	"context"
	"time"

	"xclaw/cli/db"
	"xclaw/cli/models"
)

type Logger struct {
	store *db.Store
}

func NewLogger(store *db.Store) *Logger {
	return &Logger{store: store}
}

func (l *Logger) Log(ctx context.Context, agentID, sessionID, category, action, detail string) {
	_ = l.store.InsertAudit(ctx, models.AuditLog{
		AgentID:   agentID,
		SessionID: sessionID,
		Category:  category,
		Action:    action,
		Detail:    detail,
		CreatedAt: time.Now().UTC(),
	})
}
