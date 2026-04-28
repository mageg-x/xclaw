package engine

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xclaw/cli/db"
	"xclaw/cli/models"
)

type Step struct {
	Kind   string            `json:"kind"`
	Name   string            `json:"name"`
	Params map[string]string `json:"params"`
}

type DeterministicPlan struct {
	Steps []Step `json:"steps"`
}

type PlanCache struct {
	store *db.Store
}

func NewPlanCache(store *db.Store) *PlanCache {
	return &PlanCache{store: store}
}

func Fingerprint(input string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(strings.ToLower(input))))
	return hex.EncodeToString(sum[:])
}

func (p *PlanCache) Get(ctx context.Context, agentID, message string) (DeterministicPlan, bool, error) {
	plan, _, ok, err := p.GetMeta(ctx, agentID, message)
	return plan, ok, err
}

func (p *PlanCache) GetMeta(ctx context.Context, agentID, message string) (DeterministicPlan, models.DeterministicPlan, bool, error) {
	fp := Fingerprint(message)
	row, ok, err := p.store.GetDeterministicPlan(ctx, agentID, fp)
	if err != nil || !ok {
		return DeterministicPlan{}, models.DeterministicPlan{}, false, err
	}

	var plan DeterministicPlan
	if err := json.Unmarshal([]byte(row.PlanJSON), &plan); err != nil {
		return DeterministicPlan{}, models.DeterministicPlan{}, false, fmt.Errorf("decode deterministic plan: %w", err)
	}
	_ = p.store.IncDeterministicPlanHits(ctx, row.ID)
	return plan, row, true, nil
}

func (p *PlanCache) Save(ctx context.Context, agentID, message string, plan DeterministicPlan) error {
	b, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode deterministic plan: %w", err)
	}
	now := time.Now().UTC()
	return p.store.UpsertDeterministicPlan(ctx, models.DeterministicPlan{
		ID:          "plan-" + randomSuffix(),
		AgentID:     agentID,
		Fingerprint: Fingerprint(message),
		PlanJSON:    string(b),
		Hits:        1,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
}

func (p *PlanCache) MaybeCompileScript(workspacePath, message string, plan DeterministicPlan, hits int) (string, bool, error) {
	if hits < 3 {
		return "", false, nil
	}
	commands := make([]string, 0)
	for _, step := range plan.Steps {
		if step.Kind != "tool" || step.Name != "exec_cmd" {
			continue
		}
		cmd := strings.TrimSpace(step.Params["command"])
		args := strings.TrimSpace(step.Params["args"])
		if cmd == "" {
			continue
		}
		if args == "" {
			commands = append(commands, cmd)
		} else {
			commands = append(commands, cmd+" "+args)
		}
	}
	if len(commands) == 0 {
		return "", false, nil
	}
	scriptDir := filepath.Join(workspacePath, ".agent", "script-cache")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		return "", false, fmt.Errorf("create script cache dir: %w", err)
	}
	scriptName := Fingerprint(message) + ".sh"
	absPath := filepath.Join(scriptDir, scriptName)
	if _, err := os.Stat(absPath); err == nil {
		return filepath.ToSlash(filepath.Join(".agent", "script-cache", scriptName)), true, nil
	}
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("# Auto-compiled deterministic script cache\n")
	for _, cmd := range commands {
		b.WriteString(cmd)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(absPath, []byte(b.String()), 0o755); err != nil {
		return "", false, fmt.Errorf("write script cache: %w", err)
	}
	return filepath.ToSlash(filepath.Join(".agent", "script-cache", scriptName)), true, nil
}
