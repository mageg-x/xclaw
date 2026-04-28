package models

import "time"

type Agent struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Emoji             string    `json:"emoji"`
	Description       string    `json:"description"`
	SystemInstruction string    `json:"system_instruction"`
	ModelProvider     string    `json:"model_provider"`
	ModelName         string    `json:"model_name"`
	WorkspacePath     string    `json:"workspace_path"`
	Tools             []string  `json:"tools"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Session struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Title     string    `json:"title"`
	IsMain    bool      `json:"is_main"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Message struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Metadata  string    `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
}

type AuditLog struct {
	ID        int64     `json:"id"`
	AgentID   string    `json:"agent_id"`
	SessionID string    `json:"session_id"`
	Category  string    `json:"category"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

type Credential struct {
	Provider      string    `json:"provider"`
	CiphertextB64 string    `json:"ciphertext_b64"`
	NonceB64      string    `json:"nonce_b64"`
	SaltB64       string    `json:"salt_b64"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CronJob struct {
	ID            string     `json:"id"`
	AgentID       string     `json:"agent_id"`
	Name          string     `json:"name"`
	Schedule      string     `json:"schedule"`
	ScheduleType  string     `json:"type"`
	JobType       string     `json:"job_type"`
	Payload       string     `json:"payload"`
	ExecutionMode string     `json:"execution_mode"`
	SessionID     string     `json:"session_id,omitempty"`
	TargetChannel string     `json:"target_channel"`
	Priority      string     `json:"priority"`
	Enabled       bool       `json:"enabled"`
	RetryLimit    int        `json:"retry_limit"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	NextRunAt     *time.Time `json:"next_run_at,omitempty"`
	LastStatus    string     `json:"last_status"`
	LastError     string     `json:"last_error"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type DeterministicPlan struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"`
	Fingerprint string    `json:"fingerprint"`
	PlanJSON    string    `json:"plan_json"`
	Hits        int       `json:"hits"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type OnboardingState struct {
	Bootstrapped bool      `json:"bootstrapped"`
	CreatedAt    time.Time `json:"created_at"`
}

type VectorMemoryHit struct {
	RowID     int64     `json:"row_id"`
	AgentID   string    `json:"agent_id"`
	SessionID string    `json:"session_id"`
	Content   string    `json:"content"`
	Distance  float64   `json:"distance"`
	CreatedAt time.Time `json:"created_at"`
}
