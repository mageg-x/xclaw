package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec"

	"xclaw/cli/models"
)

const (
	vectorDimension = 256
)

type Store struct {
	sql                *sql.DB
	vecEnabled         bool
	vecHNSWSupported   bool
	vecHNSWSupportNote string
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(3 * time.Minute)

	s := &Store{sql: db}
	if err := s.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.sql.Close()
}

func (s *Store) initSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS settings (
            key TEXT PRIMARY KEY,
            value TEXT NOT NULL,
            updated_at TEXT NOT NULL
        );`,
		`CREATE TABLE IF NOT EXISTS agents (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            emoji TEXT NOT NULL,
            description TEXT NOT NULL,
            system_instruction TEXT NOT NULL,
            model_provider TEXT NOT NULL,
            model_name TEXT NOT NULL,
            workspace_path TEXT NOT NULL,
            tools_json TEXT NOT NULL,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        );`,
		`CREATE TABLE IF NOT EXISTS sessions (
            id TEXT PRIMARY KEY,
            agent_id TEXT NOT NULL,
            title TEXT NOT NULL,
            is_main INTEGER NOT NULL,
            status TEXT NOT NULL,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL,
            FOREIGN KEY(agent_id) REFERENCES agents(id) ON DELETE CASCADE
        );`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_agent_id ON sessions(agent_id);`,
		`CREATE TABLE IF NOT EXISTS messages (
            id TEXT PRIMARY KEY,
            session_id TEXT NOT NULL,
            role TEXT NOT NULL,
            content TEXT NOT NULL,
            metadata TEXT NOT NULL,
            created_at TEXT NOT NULL,
            FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
        );`,
		`CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, created_at);`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            agent_id TEXT NOT NULL,
            session_id TEXT NOT NULL,
            category TEXT NOT NULL,
            action TEXT NOT NULL,
            detail TEXT NOT NULL,
            created_at TEXT NOT NULL
        );`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_agent ON audit_logs(agent_id, created_at DESC);`,
		`CREATE TABLE IF NOT EXISTS credentials (
            provider TEXT PRIMARY KEY,
            ciphertext_b64 TEXT NOT NULL,
            nonce_b64 TEXT NOT NULL,
            salt_b64 TEXT NOT NULL,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        );`,
		`CREATE TABLE IF NOT EXISTS cron_jobs (
            id TEXT PRIMARY KEY,
            agent_id TEXT NOT NULL,
            name TEXT NOT NULL,
            schedule TEXT NOT NULL,
            schedule_type TEXT NOT NULL DEFAULT 'cron',
            job_type TEXT NOT NULL,
            payload TEXT NOT NULL,
            execution_mode TEXT NOT NULL DEFAULT 'main',
            session_id TEXT NOT NULL DEFAULT '',
            target_channel TEXT NOT NULL DEFAULT '',
            priority TEXT NOT NULL DEFAULT 'normal',
            enabled INTEGER NOT NULL,
            retry_limit INTEGER NOT NULL,
            last_run_at TEXT,
            next_run_at TEXT,
            last_status TEXT NOT NULL,
            last_error TEXT NOT NULL,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL,
            FOREIGN KEY(agent_id) REFERENCES agents(id) ON DELETE CASCADE
        );`,
		`CREATE INDEX IF NOT EXISTS idx_cron_jobs_enabled ON cron_jobs(enabled);`,
		`CREATE TABLE IF NOT EXISTS deterministic_plans (
            id TEXT PRIMARY KEY,
            agent_id TEXT NOT NULL,
            fingerprint TEXT NOT NULL,
            plan_json TEXT NOT NULL,
            hits INTEGER NOT NULL,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL,
            UNIQUE(agent_id, fingerprint)
        );`,
		`CREATE TABLE IF NOT EXISTS vector_memory_meta (
            rowid INTEGER PRIMARY KEY AUTOINCREMENT,
            agent_id TEXT NOT NULL,
            session_id TEXT NOT NULL,
            content TEXT NOT NULL,
            created_at TEXT NOT NULL
        );`,
		`CREATE INDEX IF NOT EXISTS idx_vector_memory_agent ON vector_memory_meta(agent_id, created_at DESC);`,
	}

	for _, stmt := range statements {
		if _, err := s.sql.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
	}
	if err := s.ensureSchemaCompatibility(ctx); err != nil {
		return err
	}
	if err := s.initVectorIndex(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureSchemaCompatibility(ctx context.Context) error {
	columns := []struct {
		name       string
		definition string
	}{
		{name: "schedule_type", definition: "TEXT NOT NULL DEFAULT 'cron'"},
		{name: "execution_mode", definition: "TEXT NOT NULL DEFAULT 'main'"},
		{name: "session_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "target_channel", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "priority", definition: "TEXT NOT NULL DEFAULT 'normal'"},
		{name: "next_run_at", definition: "TEXT"},
	}
	for _, c := range columns {
		if err := s.ensureTableColumn(ctx, "cron_jobs", c.name, c.definition); err != nil {
			return err
		}
	}
	if _, err := s.sql.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_cron_jobs_next_run ON cron_jobs(next_run_at);`); err != nil {
		return fmt.Errorf("create cron next_run index: %w", err)
	}
	return nil
}

func (s *Store) ensureTableColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.sql.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()

	var exists bool
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notnull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &primaryKey); err != nil {
			return fmt.Errorf("scan table_info %s: %w", table, err)
		}
		if strings.EqualFold(name, column) {
			exists = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate table_info %s: %w", table, err)
	}
	if exists {
		return nil
	}

	_, err = s.sql.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	if err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) initVectorIndex(ctx context.Context) error {
	_, err := s.sql.ExecContext(ctx, fmt.Sprintf(`
		CREATE VIRTUAL TABLE IF NOT EXISTS vector_memory_idx
		USING vec0(embedding float[%d]);
	`, vectorDimension))
	if err != nil {
		s.vecEnabled = false
		s.vecHNSWSupported = false
		s.vecHNSWSupportNote = "sqlite/vec extension unavailable: " + err.Error()
		return fmt.Errorf("init vector table: %w", err)
	}
	s.vecEnabled = true

	// Probe sqlite/vec HNSW grammar support. If unsupported, keep exact vec0 mode.
	_, probeErr := s.sql.ExecContext(ctx, `
		CREATE VIRTUAL TABLE IF NOT EXISTS _vec_hnsw_probe
		USING vec0(embedding float[4], index=hnsw);
	`)
	if probeErr == nil {
		s.vecHNSWSupported = true
		s.vecHNSWSupportNote = "hnsw options accepted by sqlite/vec"
		_, _ = s.sql.ExecContext(ctx, `DROP TABLE IF EXISTS _vec_hnsw_probe`)
		return nil
	}

	s.vecHNSWSupported = false
	s.vecHNSWSupportNote = "sqlite/vec currently does not expose hnsw table options, fallback to vec0 exact search: " + probeErr.Error()
	_, _ = s.sql.ExecContext(ctx, `DROP TABLE IF EXISTS _vec_hnsw_probe`)
	return nil
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func parseTime(raw string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, raw)
	return t
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.sql.ExecContext(ctx, `
        INSERT INTO settings(key, value, updated_at)
        VALUES(?, ?, ?)
        ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at;
    `, key, value, now())
	if err != nil {
		return fmt.Errorf("set setting: %w", err)
	}
	return nil
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get setting: %w", err)
	}
	return value, true, nil
}

func (s *Store) CreateAgent(ctx context.Context, agent models.Agent) error {
	tools, err := json.Marshal(agent.Tools)
	if err != nil {
		return fmt.Errorf("marshal tools: %w", err)
	}

	_, err = s.sql.ExecContext(ctx, `
        INSERT INTO agents(id, name, emoji, description, system_instruction, model_provider, model_name, workspace_path, tools_json, created_at, updated_at)
        VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, agent.ID, agent.Name, agent.Emoji, agent.Description, agent.SystemInstruction, agent.ModelProvider, agent.ModelName, agent.WorkspacePath, string(tools),
		agent.CreatedAt.UTC().Format(time.RFC3339Nano), agent.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert agent: %w", err)
	}
	return nil
}

func (s *Store) UpdateAgent(ctx context.Context, agent models.Agent) error {
	tools, err := json.Marshal(agent.Tools)
	if err != nil {
		return fmt.Errorf("marshal tools: %w", err)
	}

	res, err := s.sql.ExecContext(ctx, `
        UPDATE agents
        SET name=?, emoji=?, description=?, system_instruction=?, model_provider=?, model_name=?, workspace_path=?, tools_json=?, updated_at=?
        WHERE id=?
    `, agent.Name, agent.Emoji, agent.Description, agent.SystemInstruction, agent.ModelProvider, agent.ModelName,
		agent.WorkspacePath, string(tools), agent.UpdatedAt.UTC().Format(time.RFC3339Nano), agent.ID)
	if err != nil {
		return fmt.Errorf("update agent: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	_, err := s.sql.ExecContext(ctx, `DELETE FROM agents WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	return nil
}

func (s *Store) GetAgent(ctx context.Context, id string) (models.Agent, error) {
	row := s.sql.QueryRowContext(ctx, `
        SELECT id, name, emoji, description, system_instruction, model_provider, model_name, workspace_path, tools_json, created_at, updated_at
        FROM agents WHERE id=?
    `, id)
	return scanAgent(row)
}

func (s *Store) ListAgents(ctx context.Context) ([]models.Agent, error) {
	rows, err := s.sql.QueryContext(ctx, `
        SELECT id, name, emoji, description, system_instruction, model_provider, model_name, workspace_path, tools_json, created_at, updated_at
        FROM agents
        ORDER BY updated_at DESC
    `)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()

	out := make([]models.Agent, 0)
	for rows.Next() {
		var (
			agent                models.Agent
			toolsJSON            string
			createdAt, updatedAt string
		)
		if err := rows.Scan(&agent.ID, &agent.Name, &agent.Emoji, &agent.Description, &agent.SystemInstruction,
			&agent.ModelProvider, &agent.ModelName, &agent.WorkspacePath, &toolsJSON, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan agent: %w", err)
		}
		if err := json.Unmarshal([]byte(toolsJSON), &agent.Tools); err != nil {
			return nil, fmt.Errorf("decode tools: %w", err)
		}
		agent.CreatedAt = parseTime(createdAt)
		agent.UpdatedAt = parseTime(updatedAt)
		out = append(out, agent)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agents: %w", err)
	}
	return out, nil
}

func scanAgent(row interface{ Scan(dest ...any) error }) (models.Agent, error) {
	var (
		agent                models.Agent
		toolsJSON            string
		createdAt, updatedAt string
	)
	if err := row.Scan(&agent.ID, &agent.Name, &agent.Emoji, &agent.Description, &agent.SystemInstruction,
		&agent.ModelProvider, &agent.ModelName, &agent.WorkspacePath, &toolsJSON, &createdAt, &updatedAt); err != nil {
		return models.Agent{}, err
	}
	if err := json.Unmarshal([]byte(toolsJSON), &agent.Tools); err != nil {
		return models.Agent{}, fmt.Errorf("decode tools: %w", err)
	}
	agent.CreatedAt = parseTime(createdAt)
	agent.UpdatedAt = parseTime(updatedAt)
	return agent, nil
}

func (s *Store) CreateSession(ctx context.Context, session models.Session) error {
	_, err := s.sql.ExecContext(ctx, `
        INSERT INTO sessions(id, agent_id, title, is_main, status, created_at, updated_at)
        VALUES(?, ?, ?, ?, ?, ?, ?)
    `, session.ID, session.AgentID, session.Title, boolToInt(session.IsMain), session.Status,
		session.CreatedAt.UTC().Format(time.RFC3339Nano), session.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, id string) (models.Session, error) {
	var (
		sess                 models.Session
		isMain               int
		createdAt, updatedAt string
	)
	err := s.sql.QueryRowContext(ctx, `
        SELECT id, agent_id, title, is_main, status, created_at, updated_at
        FROM sessions WHERE id=?
    `, id).Scan(&sess.ID, &sess.AgentID, &sess.Title, &isMain, &sess.Status, &createdAt, &updatedAt)
	if err != nil {
		return models.Session{}, err
	}
	sess.IsMain = isMain == 1
	sess.CreatedAt = parseTime(createdAt)
	sess.UpdatedAt = parseTime(updatedAt)
	return sess, nil
}

func (s *Store) ListSessions(ctx context.Context, agentID string) ([]models.Session, error) {
	q := `SELECT id, agent_id, title, is_main, status, created_at, updated_at FROM sessions`
	args := make([]any, 0, 1)
	if agentID != "" {
		q += ` WHERE agent_id=?`
		args = append(args, agentID)
	}
	q += ` ORDER BY updated_at DESC`

	rows, err := s.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	out := make([]models.Session, 0)
	for rows.Next() {
		var (
			sess                 models.Session
			isMain               int
			createdAt, updatedAt string
		)
		if err := rows.Scan(&sess.ID, &sess.AgentID, &sess.Title, &isMain, &sess.Status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sess.IsMain = isMain == 1
		sess.CreatedAt = parseTime(createdAt)
		sess.UpdatedAt = parseTime(updatedAt)
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) GetMainSession(ctx context.Context, agentID string) (models.Session, bool, error) {
	var (
		sess                 models.Session
		isMain               int
		createdAt, updatedAt string
	)
	err := s.sql.QueryRowContext(ctx, `
		SELECT id, agent_id, title, is_main, status, created_at, updated_at
		FROM sessions
		WHERE agent_id=? AND is_main=1
		ORDER BY updated_at DESC
		LIMIT 1
	`, agentID).Scan(&sess.ID, &sess.AgentID, &sess.Title, &isMain, &sess.Status, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return models.Session{}, false, nil
		}
		return models.Session{}, false, fmt.Errorf("get main session: %w", err)
	}
	sess.IsMain = isMain == 1
	sess.CreatedAt = parseTime(createdAt)
	sess.UpdatedAt = parseTime(updatedAt)
	return sess, true, nil
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.sql.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *Store) UpdateSessionStatus(ctx context.Context, id, status string) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE sessions SET status=?, updated_at=? WHERE id=?`, status, now(), id)
	if err != nil {
		return fmt.Errorf("update session status: %w", err)
	}
	return nil
}

func (s *Store) TouchSession(ctx context.Context, id string) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE sessions SET updated_at=? WHERE id=?`, now(), id)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

func (s *Store) CreateMessage(ctx context.Context, msg models.Message) error {
	_, err := s.sql.ExecContext(ctx, `
        INSERT INTO messages(id, session_id, role, content, metadata, created_at)
        VALUES(?, ?, ?, ?, ?, ?)
    `, msg.ID, msg.SessionID, msg.Role, msg.Content, msg.Metadata, msg.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

func (s *Store) ListMessages(ctx context.Context, sessionID string, limit int) ([]models.Message, error) {
	if limit <= 0 {
		limit = 200
	}

	rows, err := s.sql.QueryContext(ctx, `
        SELECT id, session_id, role, content, metadata, created_at
        FROM messages WHERE session_id=?
        ORDER BY created_at ASC
        LIMIT ?
    `, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	out := make([]models.Message, 0)
	for rows.Next() {
		var msg models.Message
		var createdAt string
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Role, &msg.Content, &msg.Metadata, &createdAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msg.CreatedAt = parseTime(createdAt)
		out = append(out, msg)
	}
	return out, rows.Err()
}

func (s *Store) InsertAudit(ctx context.Context, log models.AuditLog) error {
	_, err := s.sql.ExecContext(ctx, `
        INSERT INTO audit_logs(agent_id, session_id, category, action, detail, created_at)
        VALUES(?, ?, ?, ?, ?, ?)
    `, log.AgentID, log.SessionID, log.Category, log.Action, log.Detail, log.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}

func (s *Store) ListAudit(ctx context.Context, agentID string, limit int) ([]models.AuditLog, error) {
	if limit <= 0 {
		limit = 200
	}

	query := `SELECT id, agent_id, session_id, category, action, detail, created_at FROM audit_logs`
	args := make([]any, 0, 2)
	if agentID != "" {
		query += ` WHERE agent_id=?`
		args = append(args, agentID)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()

	out := make([]models.AuditLog, 0)
	for rows.Next() {
		var log models.AuditLog
		var createdAt string
		if err := rows.Scan(&log.ID, &log.AgentID, &log.SessionID, &log.Category, &log.Action, &log.Detail, &createdAt); err != nil {
			return nil, fmt.Errorf("scan audit: %w", err)
		}
		log.CreatedAt = parseTime(createdAt)
		out = append(out, log)
	}
	return out, rows.Err()
}

func (s *Store) UpsertCredential(ctx context.Context, cred models.Credential) error {
	_, err := s.sql.ExecContext(ctx, `
        INSERT INTO credentials(provider, ciphertext_b64, nonce_b64, salt_b64, created_at, updated_at)
        VALUES(?, ?, ?, ?, ?, ?)
        ON CONFLICT(provider) DO UPDATE SET
          ciphertext_b64=excluded.ciphertext_b64,
          nonce_b64=excluded.nonce_b64,
          salt_b64=excluded.salt_b64,
          updated_at=excluded.updated_at
    `, cred.Provider, cred.CiphertextB64, cred.NonceB64, cred.SaltB64,
		cred.CreatedAt.UTC().Format(time.RFC3339Nano), cred.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert credential: %w", err)
	}
	return nil
}

func (s *Store) GetCredential(ctx context.Context, provider string) (models.Credential, error) {
	var (
		cred                 models.Credential
		createdAt, updatedAt string
	)
	err := s.sql.QueryRowContext(ctx, `
        SELECT provider, ciphertext_b64, nonce_b64, salt_b64, created_at, updated_at
        FROM credentials WHERE provider=?
    `, provider).Scan(&cred.Provider, &cred.CiphertextB64, &cred.NonceB64, &cred.SaltB64, &createdAt, &updatedAt)
	if err != nil {
		return models.Credential{}, err
	}
	cred.CreatedAt = parseTime(createdAt)
	cred.UpdatedAt = parseTime(updatedAt)
	return cred, nil
}

func (s *Store) CreateCronJob(ctx context.Context, job models.CronJob) error {
	scheduleType := strings.TrimSpace(job.ScheduleType)
	if scheduleType == "" {
		scheduleType = "cron"
	}
	executionMode := strings.TrimSpace(job.ExecutionMode)
	if executionMode == "" {
		executionMode = "main"
	}
	priority := strings.TrimSpace(job.Priority)
	if priority == "" {
		priority = "normal"
	}
	_, err := s.sql.ExecContext(ctx, `
        INSERT INTO cron_jobs(
            id, agent_id, name, schedule, schedule_type, job_type, payload, execution_mode, session_id, target_channel, priority,
            enabled, retry_limit, last_run_at, next_run_at, last_status, last_error, created_at, updated_at
        )
        VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, job.ID, job.AgentID, job.Name, job.Schedule, scheduleType, job.JobType, job.Payload, executionMode, job.SessionID, job.TargetChannel, priority,
		boolToInt(job.Enabled), job.RetryLimit, nullableTime(job.LastRunAt), nullableTime(job.NextRunAt), job.LastStatus, job.LastError,
		job.CreatedAt.UTC().Format(time.RFC3339Nano), job.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert cron job: %w", err)
	}
	return nil
}

func (s *Store) UpdateCronJob(ctx context.Context, job models.CronJob) error {
	scheduleType := strings.TrimSpace(job.ScheduleType)
	if scheduleType == "" {
		scheduleType = "cron"
	}
	executionMode := strings.TrimSpace(job.ExecutionMode)
	if executionMode == "" {
		executionMode = "main"
	}
	priority := strings.TrimSpace(job.Priority)
	if priority == "" {
		priority = "normal"
	}
	_, err := s.sql.ExecContext(ctx, `
        UPDATE cron_jobs
        SET
            name=?, schedule=?, schedule_type=?, job_type=?, payload=?, execution_mode=?, session_id=?, target_channel=?, priority=?,
            enabled=?, retry_limit=?, next_run_at=?, updated_at=?
        WHERE id=?
    `, job.Name, job.Schedule, scheduleType, job.JobType, job.Payload, executionMode, job.SessionID, job.TargetChannel, priority,
		boolToInt(job.Enabled), job.RetryLimit, nullableTime(job.NextRunAt), job.UpdatedAt.UTC().Format(time.RFC3339Nano), job.ID)
	if err != nil {
		return fmt.Errorf("update cron job: %w", err)
	}
	return nil
}

func (s *Store) UpdateCronResult(ctx context.Context, id string, runAt *time.Time, status, detail string) error {
	_, err := s.sql.ExecContext(ctx, `
        UPDATE cron_jobs
        SET last_run_at=?, last_status=?, last_error=?, updated_at=?
        WHERE id=?
    `, nullableTime(runAt), status, detail, now(), id)
	if err != nil {
		return fmt.Errorf("update cron result: %w", err)
	}
	return nil
}

func (s *Store) UpdateCronScheduleState(ctx context.Context, id string, nextRunAt *time.Time, enabled bool) error {
	_, err := s.sql.ExecContext(ctx, `
		UPDATE cron_jobs
		SET next_run_at=?, enabled=?, updated_at=?
		WHERE id=?
	`, nullableTime(nextRunAt), boolToInt(enabled), now(), id)
	if err != nil {
		return fmt.Errorf("update cron schedule state: %w", err)
	}
	return nil
}

func (s *Store) DeleteCronJob(ctx context.Context, id string) error {
	_, err := s.sql.ExecContext(ctx, `DELETE FROM cron_jobs WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete cron job: %w", err)
	}
	return nil
}

func (s *Store) ListCronJobs(ctx context.Context, agentID string, enabledOnly bool) ([]models.CronJob, error) {
	q := `
		SELECT
			id, agent_id, name, schedule, schedule_type, job_type, payload, execution_mode, session_id, target_channel, priority,
			enabled, retry_limit, last_run_at, next_run_at, last_status, last_error, created_at, updated_at
		FROM cron_jobs
		WHERE 1=1
	`
	args := make([]any, 0, 2)
	if agentID != "" {
		q += ` AND agent_id=?`
		args = append(args, agentID)
	}
	if enabledOnly {
		q += ` AND enabled=1`
	}
	q += ` ORDER BY updated_at DESC`

	rows, err := s.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list cron jobs: %w", err)
	}
	defer rows.Close()

	out := make([]models.CronJob, 0)
	for rows.Next() {
		var (
			job                  models.CronJob
			enabled              int
			lastRun, nextRun     sql.NullString
			createdAt, updatedAt string
		)
		if err := rows.Scan(
			&job.ID, &job.AgentID, &job.Name, &job.Schedule, &job.ScheduleType, &job.JobType, &job.Payload,
			&job.ExecutionMode, &job.SessionID, &job.TargetChannel, &job.Priority,
			&enabled, &job.RetryLimit, &lastRun, &nextRun, &job.LastStatus, &job.LastError, &createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan cron job: %w", err)
		}
		job.Enabled = enabled == 1
		if strings.TrimSpace(job.ScheduleType) == "" {
			job.ScheduleType = "cron"
		}
		if strings.TrimSpace(job.ExecutionMode) == "" {
			job.ExecutionMode = "main"
		}
		if strings.TrimSpace(job.Priority) == "" {
			job.Priority = "normal"
		}
		if lastRun.Valid {
			t := parseTime(lastRun.String)
			job.LastRunAt = &t
		}
		if nextRun.Valid {
			t := parseTime(nextRun.String)
			job.NextRunAt = &t
		}
		job.CreatedAt = parseTime(createdAt)
		job.UpdatedAt = parseTime(updatedAt)
		out = append(out, job)
	}
	return out, rows.Err()
}

func (s *Store) UpsertDeterministicPlan(ctx context.Context, plan models.DeterministicPlan) error {
	_, err := s.sql.ExecContext(ctx, `
        INSERT INTO deterministic_plans(id, agent_id, fingerprint, plan_json, hits, created_at, updated_at)
        VALUES(?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(agent_id, fingerprint) DO UPDATE SET
          plan_json=excluded.plan_json,
          hits=excluded.hits,
          updated_at=excluded.updated_at
    `, plan.ID, plan.AgentID, plan.Fingerprint, plan.PlanJSON, plan.Hits,
		plan.CreatedAt.UTC().Format(time.RFC3339Nano), plan.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert deterministic plan: %w", err)
	}
	return nil
}

func (s *Store) GetDeterministicPlan(ctx context.Context, agentID, fingerprint string) (models.DeterministicPlan, bool, error) {
	var (
		plan                 models.DeterministicPlan
		createdAt, updatedAt string
	)
	err := s.sql.QueryRowContext(ctx, `
        SELECT id, agent_id, fingerprint, plan_json, hits, created_at, updated_at
        FROM deterministic_plans
        WHERE agent_id=? AND fingerprint=?
    `, agentID, fingerprint).Scan(&plan.ID, &plan.AgentID, &plan.Fingerprint, &plan.PlanJSON, &plan.Hits, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return models.DeterministicPlan{}, false, nil
		}
		return models.DeterministicPlan{}, false, fmt.Errorf("get deterministic plan: %w", err)
	}
	plan.CreatedAt = parseTime(createdAt)
	plan.UpdatedAt = parseTime(updatedAt)
	return plan, true, nil
}

func (s *Store) IncDeterministicPlanHits(ctx context.Context, id string) error {
	_, err := s.sql.ExecContext(ctx, `
        UPDATE deterministic_plans
        SET hits = hits + 1, updated_at=?
        WHERE id=?
    `, now(), id)
	if err != nil {
		return fmt.Errorf("increment deterministic plan hits: %w", err)
	}
	return nil
}

func (s *Store) VectorStatus() map[string]any {
	return map[string]any{
		"enabled":               s.vecEnabled,
		"hnsw_supported":        s.vecHNSWSupported,
		"hnsw_support_note":     s.vecHNSWSupportNote,
		"dimension":             vectorDimension,
		"engine":                "modernc.org/sqlite/vec",
		"active_index_strategy": strategyName(s.vecHNSWSupported),
	}
}

func (s *Store) AddVectorMemory(ctx context.Context, agentID, sessionID, content string, vector []float32) (int64, error) {
	if !s.vecEnabled {
		return 0, fmt.Errorf("vector index is not enabled")
	}
	if len(vector) != vectorDimension {
		return 0, fmt.Errorf("vector dimension mismatch: got=%d expected=%d", len(vector), vectorDimension)
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return 0, fmt.Errorf("vector content is empty")
	}

	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO vector_memory_meta(agent_id, session_id, content, created_at)
		VALUES (?, ?, ?, ?)
	`, agentID, sessionID, content, now())
	if err != nil {
		return 0, fmt.Errorf("insert vector meta: %w", err)
	}
	rowID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get vector row id: %w", err)
	}

	embeddingJSON := vectorToJSON(vector)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO vector_memory_idx(rowid, embedding)
		VALUES (?, ?)
	`, rowID, embeddingJSON); err != nil {
		return 0, fmt.Errorf("insert vector row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit vector tx: %w", err)
	}
	return rowID, nil
}

func (s *Store) SearchVectorMemory(ctx context.Context, queryVector []float32, limit int, agentID string) ([]models.VectorMemoryHit, error) {
	if !s.vecEnabled {
		return nil, fmt.Errorf("vector index is not enabled")
	}
	if len(queryVector) != vectorDimension {
		return nil, fmt.Errorf("query vector dimension mismatch: got=%d expected=%d", len(queryVector), vectorDimension)
	}
	if limit <= 0 {
		limit = 5
	}

	rows, err := s.sql.QueryContext(ctx, `
		SELECT
			m.rowid,
			m.agent_id,
			m.session_id,
			m.content,
			m.created_at,
			v.distance
		FROM vector_memory_idx AS v
		JOIN vector_memory_meta AS m
			ON m.rowid = v.rowid
		WHERE v.embedding MATCH ?
			AND (? = '' OR m.agent_id = ?)
		ORDER BY v.distance ASC
		LIMIT ?
	`, vectorToJSON(queryVector), agentID, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("search vector memory: %w", err)
	}
	defer rows.Close()

	out := make([]models.VectorMemoryHit, 0, limit)
	for rows.Next() {
		var (
			hit       models.VectorMemoryHit
			createdAt string
		)
		if err := rows.Scan(&hit.RowID, &hit.AgentID, &hit.SessionID, &hit.Content, &createdAt, &hit.Distance); err != nil {
			return nil, fmt.Errorf("scan vector hit: %w", err)
		}
		hit.CreatedAt = parseTime(createdAt)
		out = append(out, hit)
	}
	return out, rows.Err()
}

func strategyName(hnsw bool) string {
	if hnsw {
		return "hnsw"
	}
	return "vec0-exact"
}

func vectorToJSON(vector []float32) string {
	var b strings.Builder
	b.Grow(len(vector) * 8)
	b.WriteByte('[')
	for i, v := range vector {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'f', 6, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
