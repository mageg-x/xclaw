package approval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Strategy string

const (
	StrategySingle Strategy = "single"
	StrategyAll    Strategy = "all"
	StrategyAny    Strategy = "any"
)

type Rule struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Enabled         bool     `json:"enabled"`
	Tools           []string `json:"tools,omitempty"`
	Risks           []string `json:"risks,omitempty"`
	PathPrefixes    []string `json:"path_prefixes,omitempty"`
	CommandPatterns []string `json:"command_patterns,omitempty"`
	ImpactMin       int      `json:"impact_min,omitempty"`
	Strategy        Strategy `json:"strategy"`
	Approvers       []string `json:"approvers,omitempty"`
	ExpiresInSec    int      `json:"expires_in_sec"`
	Reason          string   `json:"reason,omitempty"`
}

type RequestStatus string

const (
	RequestPending  RequestStatus = "pending"
	RequestApproved RequestStatus = "approved"
	RequestRejected RequestStatus = "rejected"
	RequestExpired  RequestStatus = "expired"
)

type Request struct {
	ID          string            `json:"id"`
	RuleID      string            `json:"rule_id"`
	Tool        string            `json:"tool"`
	Risk        string            `json:"risk"`
	Params      map[string]string `json:"params"`
	Reason      string            `json:"reason"`
	Strategy    Strategy          `json:"strategy"`
	Approvers   []string          `json:"approvers,omitempty"`
	ApprovedBy  []string          `json:"approved_by,omitempty"`
	RejectedBy  []string          `json:"rejected_by,omitempty"`
	Status      RequestStatus     `json:"status"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
}

type EvalInput struct {
	Tool      string
	Risk      string
	Params    map[string]string
	Workspace string
}

type EvalResult struct {
	RequiresApproval bool
	RequestID        string
	RuleID           string
	Message          string
}

type Manager struct {
	mu       sync.Mutex
	rules    []Rule
	requests map[string]Request
}

type PersistedState struct {
	Rules    []Rule    `json:"rules"`
	Requests []Request `json:"requests"`
}

func NewManager() *Manager {
	m := &Manager{requests: make(map[string]Request)}
	m.rules = []Rule{
		{
			ID:           "rule-risk-write",
			Name:         "写操作需要审批",
			Enabled:      true,
			Risks:        []string{"write"},
			Strategy:     StrategySingle,
			ExpiresInSec: 24 * 3600,
			Reason:       "写入会修改工作区内容",
		},
		{
			ID:           "rule-risk-exec",
			Name:         "命令执行需要审批",
			Enabled:      true,
			Risks:        []string{"exec"},
			Strategy:     StrategySingle,
			ExpiresInSec: 24 * 3600,
			Reason:       "命令执行存在系统影响风险",
		},
	}
	return m
}

func (m *Manager) Rules() []Rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Rule, len(m.rules))
	copy(out, m.rules)
	return out
}

func (m *Manager) SetRules(rules []Rule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range rules {
		rules[i] = normalizeRule(rules[i])
	}
	m.rules = rules
}

func (m *Manager) Evaluate(in EvalInput) EvalResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	m.expireRequests(now)

	approvalID := strings.TrimSpace(in.Params["approval_request_id"])
	if approvalID != "" {
		req, ok := m.requests[approvalID]
		if !ok {
			return EvalResult{RequiresApproval: true, RequestID: approvalID, Message: "approval request not found"}
		}
		if req.Status == RequestApproved {
			return EvalResult{}
		}
		if req.Status == RequestExpired {
			return EvalResult{RequiresApproval: true, RequestID: req.ID, RuleID: req.RuleID, Message: "approval request expired"}
		}
		if req.Status == RequestRejected {
			return EvalResult{RequiresApproval: true, RequestID: req.ID, RuleID: req.RuleID, Message: "approval request rejected"}
		}
		return EvalResult{RequiresApproval: true, RequestID: req.ID, RuleID: req.RuleID, Message: "approval request pending"}
	}

	for _, rule := range m.rules {
		if !matchRule(rule, in) {
			continue
		}
		req := Request{
			ID:        fmt.Sprintf("apr_%d", time.Now().UnixNano()),
			RuleID:    rule.ID,
			Tool:      strings.TrimSpace(in.Tool),
			Risk:      strings.TrimSpace(in.Risk),
			Params:    cloneParams(in.Params),
			Reason:    firstNonEmpty(rule.Reason, defaultReason(rule, in)),
			Strategy:  normalizeStrategy(rule.Strategy),
			Approvers: normalizeStringList(rule.Approvers),
			Status:    RequestPending,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Duration(maxInt(rule.ExpiresInSec, 900)) * time.Second),
		}
		m.requests[req.ID] = req
		return EvalResult{RequiresApproval: true, RequestID: req.ID, RuleID: rule.ID, Message: req.Reason}
	}

	return EvalResult{}
}

func (m *Manager) ListRequests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireRequests(time.Now().UTC())
	out := make([]Request, 0, len(m.requests))
	for _, req := range m.requests {
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (m *Manager) Approve(id, actor string) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id = strings.TrimSpace(id)
	actor = strings.TrimSpace(actor)
	if id == "" {
		return Request{}, fmt.Errorf("id required")
	}
	if actor == "" {
		actor = "anonymous"
	}

	m.expireRequests(time.Now().UTC())
	req, ok := m.requests[id]
	if !ok {
		return Request{}, fmt.Errorf("request not found")
	}
	if req.Status != RequestPending {
		return req, nil
	}
	req.ApprovedBy = appendUnique(req.ApprovedBy, actor)
	if approvalSatisfied(req) {
		now := time.Now().UTC()
		req.Status = RequestApproved
		req.CompletedAt = &now
	}
	m.requests[id] = req
	return req, nil
}

func (m *Manager) Reject(id, actor string) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id = strings.TrimSpace(id)
	actor = strings.TrimSpace(actor)
	if id == "" {
		return Request{}, fmt.Errorf("id required")
	}
	if actor == "" {
		actor = "anonymous"
	}

	m.expireRequests(time.Now().UTC())
	req, ok := m.requests[id]
	if !ok {
		return Request{}, fmt.Errorf("request not found")
	}
	now := time.Now().UTC()
	req.RejectedBy = appendUnique(req.RejectedBy, actor)
	req.Status = RequestRejected
	req.CompletedAt = &now
	m.requests[id] = req
	return req, nil
}

func (m *Manager) Snapshot() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireRequests(time.Now().UTC())
	requests := make([]Request, 0, len(m.requests))
	for _, req := range m.requests {
		requests = append(requests, req)
	}
	state := PersistedState{Rules: m.rules, Requests: requests}
	return json.Marshal(state)
}

func (m *Manager) Load(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var state PersistedState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(state.Rules) > 0 {
		for i := range state.Rules {
			state.Rules[i] = normalizeRule(state.Rules[i])
		}
		m.rules = state.Rules
	}
	m.requests = make(map[string]Request, len(state.Requests))
	for _, req := range state.Requests {
		if strings.TrimSpace(req.ID) == "" {
			continue
		}
		m.requests[req.ID] = req
	}
	m.expireRequests(time.Now().UTC())
	return nil
}

func (m *Manager) expireRequests(now time.Time) {
	for id, req := range m.requests {
		if req.Status != RequestPending {
			continue
		}
		if !req.ExpiresAt.IsZero() && now.After(req.ExpiresAt) {
			req.Status = RequestExpired
			finished := now
			req.CompletedAt = &finished
			m.requests[id] = req
		}
	}
}

func matchRule(rule Rule, in EvalInput) bool {
	rule = normalizeRule(rule)
	if !rule.Enabled {
		return false
	}
	tool := strings.TrimSpace(in.Tool)
	risk := strings.TrimSpace(in.Risk)
	if len(rule.Tools) > 0 && !contains(rule.Tools, tool) {
		return false
	}
	if len(rule.Risks) > 0 && !contains(rule.Risks, risk) {
		return false
	}
	if len(rule.PathPrefixes) > 0 {
		pathVal := firstNonEmpty(in.Params["path"], in.Params["target"], in.Params["file"])
		if pathVal == "" {
			return false
		}
		matched := false
		for _, prefix := range rule.PathPrefixes {
			prefix = strings.TrimSpace(prefix)
			if prefix == "" {
				continue
			}
			if strings.HasPrefix(pathVal, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(rule.CommandPatterns) > 0 {
		cmd := firstNonEmpty(in.Params["command"], in.Params["cmd"])
		if cmd == "" {
			return false
		}
		ok := false
		for _, pat := range rule.CommandPatterns {
			re, err := regexp.Compile(pat)
			if err != nil {
				continue
			}
			if re.MatchString(cmd) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if rule.ImpactMin > 0 {
		score := impactScore(in)
		if score < rule.ImpactMin {
			return false
		}
	}
	return true
}

func impactScore(in EvalInput) int {
	score := 0
	if strings.TrimSpace(in.Risk) == "exec" {
		score += 6
	}
	if strings.TrimSpace(in.Risk) == "write" {
		score += 4
	}
	pathVal := firstNonEmpty(in.Params["path"], in.Params["target"], in.Params["file"])
	if strings.HasPrefix(pathVal, "/etc") || strings.HasPrefix(pathVal, "/usr") {
		score += 6
	}
	cmd := strings.ToLower(firstNonEmpty(in.Params["command"], in.Params["cmd"]))
	if strings.Contains(cmd, "rm") || strings.Contains(cmd, "chmod") || strings.Contains(cmd, "chown") {
		score += 5
	}
	return score
}

func approvalSatisfied(req Request) bool {
	strategy := normalizeStrategy(req.Strategy)
	switch strategy {
	case StrategyAll:
		if len(req.Approvers) == 0 {
			return len(req.ApprovedBy) >= 2
		}
		for _, actor := range req.Approvers {
			if !contains(req.ApprovedBy, actor) {
				return false
			}
		}
		return true
	case StrategyAny, StrategySingle:
		fallthrough
	default:
		return len(req.ApprovedBy) >= 1
	}
}

func normalizeRule(rule Rule) Rule {
	rule.ID = strings.TrimSpace(rule.ID)
	if rule.ID == "" {
		rule.ID = fmt.Sprintf("rule_%d", time.Now().UnixNano())
	}
	rule.Name = strings.TrimSpace(rule.Name)
	if rule.Name == "" {
		rule.Name = rule.ID
	}
	rule.Tools = normalizeStringList(rule.Tools)
	rule.Risks = normalizeStringList(rule.Risks)
	rule.PathPrefixes = normalizeStringList(rule.PathPrefixes)
	rule.CommandPatterns = normalizeStringList(rule.CommandPatterns)
	rule.Approvers = normalizeStringList(rule.Approvers)
	rule.Strategy = normalizeStrategy(rule.Strategy)
	if rule.ExpiresInSec <= 0 {
		rule.ExpiresInSec = 24 * 3600
	}
	return rule
}

func normalizeStrategy(strategy Strategy) Strategy {
	switch Strategy(strings.TrimSpace(string(strategy))) {
	case StrategyAll:
		return StrategyAll
	case StrategyAny:
		return StrategyAny
	default:
		return StrategySingle
	}
}

func defaultReason(rule Rule, in EvalInput) string {
	if rule.Name != "" {
		return "审批策略命中: " + rule.Name
	}
	return fmt.Sprintf("审批策略命中: tool=%s risk=%s", in.Tool, in.Risk)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func contains(list []string, value string) bool {
	value = strings.TrimSpace(value)
	for _, v := range list {
		if strings.TrimSpace(v) == value {
			return true
		}
	}
	return false
}

func appendUnique(list []string, value string) []string {
	if contains(list, value) {
		return list
	}
	return append(list, value)
}

func cloneParams(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
