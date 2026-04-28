package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xclaw/cli/audit"
	"xclaw/cli/db"
	"xclaw/cli/engine"
	"xclaw/cli/gateway"
	"xclaw/cli/models"
)

const (
	settingHeartbeatConfig = "heartbeat_config_json"
)

type ActiveHours struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type Config struct {
	Enabled            bool        `json:"enabled"`
	Interval           string      `json:"interval"`
	ActiveHours        ActiveHours `json:"active_hours"`
	Target             string      `json:"target"`
	MaxRounds          int         `json:"max_rounds"`
	SessionIdleTimeout string      `json:"session_idle_timeout"`
}

type Runner struct {
	store   *db.Store
	engine  *engine.Service
	gateway *gateway.Gateway
	audit   *audit.Logger
	credChk credentialChecker

	mu        sync.Mutex
	lastRunAt time.Time
	lastGreet map[string]time.Time
	running   bool
	stopped   chan struct{}
}

type credentialChecker interface {
	hasAnthropicCredential(ctx context.Context) bool
}

type dbCredentialChecker struct {
	store *db.Store
}

func (c *dbCredentialChecker) hasAnthropicCredential(ctx context.Context) bool {
	raw, ok, err := c.store.GetSetting(ctx, "credential_anthropic")
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return false
	}
	return true
}

func DefaultConfig() Config {
	return Config{
		Enabled:  true,
		Interval: "30m",
		ActiveHours: ActiveHours{
			Start: "08:00",
			End:   "22:00",
		},
		Target:             "last",
		MaxRounds:          5,
		SessionIdleTimeout: "8h",
	}
}

func NewRunner(store *db.Store, eng *engine.Service, gw *gateway.Gateway, auditLogger *audit.Logger) *Runner {
	return &Runner{
		store:     store,
		engine:    eng,
		gateway:   gw,
		audit:     auditLogger,
		credChk:   &dbCredentialChecker{store: store},
		lastGreet: make(map[string]time.Time),
		stopped:   make(chan struct{}),
	}
}

func (r *Runner) Start(ctx context.Context) {
	go func() {
		defer close(r.stopped)
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.tick(context.Background())
			}
		}
	}()
}

func (r *Runner) Stop() {
	<-r.stopped
}

func (r *Runner) LoadConfig(ctx context.Context) (Config, error) {
	cfg := DefaultConfig()
	raw, ok, err := r.store.GetSetting(ctx, settingHeartbeatConfig)
	if err != nil {
		return cfg, err
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	_ = json.Unmarshal([]byte(raw), &cfg)
	return normalizeConfig(cfg), nil
}

func (r *Runner) SaveConfig(ctx context.Context, cfg Config) error {
	cfg = normalizeConfig(cfg)
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return r.store.SetSetting(ctx, settingHeartbeatConfig, string(raw))
}

func (r *Runner) tick(ctx context.Context) {
	cfg, err := r.LoadConfig(ctx)
	if err != nil {
		r.audit.Log(ctx, "", "", "heartbeat", "config_error", err.Error())
		return
	}
	if !cfg.Enabled {
		return
	}
	if !inActiveHours(time.Now(), cfg.ActiveHours) {
		return
	}
	interval, err := time.ParseDuration(cfg.Interval)
	if err != nil || interval <= 0 {
		interval = 30 * time.Minute
	}
	if r.credChk != nil && r.credChk.hasAnthropicCredential(ctx) {
		if interval < 1*time.Hour {
			interval = 1 * time.Hour
		}
	}

	r.mu.Lock()
	lastRun := r.lastRunAt
	if r.running || (!lastRun.IsZero() && time.Since(lastRun) < interval) {
		r.mu.Unlock()
		return
	}
	r.lastRunAt = time.Now().UTC()
	r.mu.Unlock()
	_ = r.RunOnce(ctx, cfg)
}

func (r *Runner) RunOnce(ctx context.Context, cfg Config) error {
	if !r.tryStartRun() {
		return nil
	}
	defer r.finishRun()

	agents, err := r.engine.ListAgents(ctx)
	if err != nil {
		return err
	}
	for _, agent := range agents {
		if err := r.runForAgent(ctx, agent, cfg); err != nil {
			r.audit.Log(ctx, agent.ID, "", "heartbeat", "run_failed", err.Error())
		}
	}
	return nil
}

func (r *Runner) tryStartRun() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

func (r *Runner) finishRun() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

func (r *Runner) runForAgent(ctx context.Context, agent models.Agent, cfg Config) error {
	hbPath := filepath.Join(agent.WorkspacePath, "HEARTBEAT.md")
	checklist, err := os.ReadFile(hbPath)
	if err != nil {
		if os.IsNotExist(err) {
			r.audit.Log(ctx, agent.ID, "", "heartbeat", "skipped", "HEARTBEAT.md not found")
			return nil
		}
		return err
	}
	prompt := fmt.Sprintf(`<system>
You are the heartbeat monitor for Agent %s.
Checklist:
%s
</system>
Current time: %s
Execute the checklist. If everything is OK, respond with exactly "HEARTBEAT_OK" and nothing else.
If you find any issue, respond with a concise alert message (max 200 chars).`,
		agent.ID, string(checklist), time.Now().Format(time.RFC3339))

	if max := cfg.MaxRounds; max <= 0 {
		cfg.MaxRounds = 5
	}
	reply, err := r.engine.RunHeartbeat(ctx, agent.ID, prompt, cfg.MaxRounds)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reply) == "HEARTBEAT_OK" {
		r.audit.Log(ctx, agent.ID, "", "heartbeat", "ok", "")
	} else {
		_, sendErr := r.gateway.Send(ctx, agent.ID, cfg.Target, gateway.OutboundEvent{
			MessageID:      engine.NewID("hb"),
			TextMarkdown:   strings.TrimSpace(reply),
			Priority:       "high",
			IdempotencyKey: fmt.Sprintf("hb:%s:%d", agent.ID, time.Now().Unix()/300),
			TTLSeconds:     3600,
		})
		if sendErr != nil {
			return sendErr
		}
		r.audit.Log(ctx, agent.ID, "", "heartbeat", "alert", strings.TrimSpace(reply))
	}

	idleTimeout, err := time.ParseDuration(cfg.SessionIdleTimeout)
	if err != nil || idleTimeout <= 0 {
		idleTimeout = 8 * time.Hour
	}
	if r.shouldSendIdleGreeting(agent.ID, idleTimeout) {
		_, _ = r.gateway.Send(ctx, agent.ID, cfg.Target, gateway.OutboundEvent{
			MessageID:      engine.NewID("hb_idle"),
			TextMarkdown:   "你已经一段时间没有互动了，需要我主动帮你检查什么吗？",
			Priority:       "normal",
			IdempotencyKey: fmt.Sprintf("hb-idle:%s:%s", agent.ID, time.Now().Format("2006-01-02")),
			TTLSeconds:     7200,
		})
		r.audit.Log(ctx, agent.ID, "", "heartbeat", "idle_greeting", "")
	}
	return nil
}

func (r *Runner) shouldSendIdleGreeting(agentID string, idleTimeout time.Duration) bool {
	sessions, err := r.engine.ListSessions(context.Background(), agentID)
	if err != nil || len(sessions) == 0 {
		return false
	}
	latest := sessions[0].UpdatedAt
	for i := 1; i < len(sessions); i++ {
		if sessions[i].UpdatedAt.After(latest) {
			latest = sessions[i].UpdatedAt
		}
	}
	if time.Since(latest) < idleTimeout {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastGreet[agentID]; ok {
		if time.Since(last) < idleTimeout/2 {
			return false
		}
	}
	r.lastGreet[agentID] = time.Now().UTC()
	return true
}

func normalizeConfig(cfg Config) Config {
	def := DefaultConfig()
	if strings.TrimSpace(cfg.Interval) == "" {
		cfg.Interval = def.Interval
	}
	if strings.TrimSpace(cfg.ActiveHours.Start) == "" {
		cfg.ActiveHours.Start = def.ActiveHours.Start
	}
	if strings.TrimSpace(cfg.ActiveHours.End) == "" {
		cfg.ActiveHours.End = def.ActiveHours.End
	}
	if strings.TrimSpace(cfg.Target) == "" {
		cfg.Target = def.Target
	}
	if cfg.MaxRounds <= 0 {
		cfg.MaxRounds = def.MaxRounds
	}
	if strings.TrimSpace(cfg.SessionIdleTimeout) == "" {
		cfg.SessionIdleTimeout = def.SessionIdleTimeout
	}
	return cfg
}

func inActiveHours(now time.Time, hours ActiveHours) bool {
	startMin, ok1 := parseHHMM(hours.Start)
	endMin, ok2 := parseHHMM(hours.End)
	if !ok1 || !ok2 || startMin == endMin {
		return true
	}
	local := now.In(time.Local)
	current := local.Hour()*60 + local.Minute()
	if startMin < endMin {
		return current >= startMin && current < endMin
	}
	return current >= startMin || current < endMin
}

func parseHHMM(raw string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return 0, false
	}
	t, err := time.Parse("15:04", parts[0]+":"+parts[1])
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}
