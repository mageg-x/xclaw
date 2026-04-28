package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xclaw/cli/approval"
	"xclaw/cli/harness"
	"xclaw/cli/mcpclient"
	"xclaw/cli/models"
	"xclaw/cli/sandbox"
)

type CronStore interface {
	CreateCronJob(ctx context.Context, job models.CronJob) error
	UpdateCronJob(ctx context.Context, job models.CronJob) error
	DeleteCronJob(ctx context.Context, id string) error
	ListCronJobs(ctx context.Context, agentID string, enabledOnly bool) ([]models.CronJob, error)
}

type RiskLevel string

const (
	RiskRead  RiskLevel = "read"
	RiskWrite RiskLevel = "write"
	RiskExec  RiskLevel = "exec"
)

type ToolCall struct {
	Name        string            `json:"name"`
	Params      map[string]string `json:"params"`
	AutoApprove bool              `json:"auto_approve"`
}

type ToolResult struct {
	Name             string    `json:"name"`
	Risk             RiskLevel `json:"risk"`
	RequiresApproval bool      `json:"requires_approval"`
	ApprovalRequest  string    `json:"approval_request,omitempty"`
	ApprovalRuleID   string    `json:"approval_rule_id,omitempty"`
	Output           string    `json:"output"`
	Error            string    `json:"error"`
}

type subagentInfo struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Workspace  string    `json:"workspace"`
	Tools      []string  `json:"tools"`
	Status     string    `json:"status"`
	LastTask   string    `json:"last_task,omitempty"`
	LastResult string    `json:"last_result,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Executor struct {
	sandbox     *sandbox.Manager
	approver    *approval.Manager
	cronStore   CronStore
	cronAgentID string
	mcp         *mcpclient.Manager
	subagentM   sync.RWMutex
	subagents   map[string]subagentInfo
}

type ToolSchema struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Params      []string `json:"params"`
	Risk        string   `json:"risk"`
}

func NewExecutor(sb *sandbox.Manager, approver *approval.Manager) *Executor {
	return &Executor{sandbox: sb, approver: approver, subagents: make(map[string]subagentInfo)}
}

func (e *Executor) SetCronStore(store CronStore, agentID string) {
	e.cronStore = store
	e.cronAgentID = strings.TrimSpace(agentID)
}

func (e *Executor) SetMCPManager(manager *mcpclient.Manager) {
	e.mcp = manager
}

func (e *Executor) ListSchemas(ctx context.Context, allowed []string) []ToolSchema {
	base := []ToolSchema{
		{Name: "list_dir", Description: "列出目录内容", Params: []string{"path"}, Risk: string(RiskRead)},
		{Name: "read_file", Description: "读取文件", Params: []string{"path"}, Risk: string(RiskRead)},
		{Name: "write_file", Description: "写入文件", Params: []string{"path", "content"}, Risk: string(RiskWrite)},
		{Name: "search_text", Description: "文本搜索", Params: []string{"query", "path"}, Risk: string(RiskRead)},
		{Name: "exec_cmd", Description: "执行命令", Params: []string{"command", "args", "approval_request_id"}, Risk: string(RiskExec)},
		{Name: "spawn_subagent", Description: "创建临时子智能体", Params: []string{"role", "description", "tools"}, Risk: string(RiskWrite)},
		{Name: "delegate_to_subagent", Description: "委托任务给子智能体", Params: []string{"id", "task", "command", "args"}, Risk: string(RiskExec)},
		{Name: "list_subagents", Description: "列出当前子智能体", Params: []string{}, Risk: string(RiskRead)},
		{Name: "terminate_subagent", Description: "终止并清理子智能体", Params: []string{"id"}, Risk: string(RiskWrite)},
		{Name: "image_generate", Description: "生成图片", Params: []string{"prompt", "size", "style", "model"}, Risk: string(RiskWrite)},
		{Name: "add_cron_job", Description: "添加定时任务", Params: []string{"name", "schedule", "payload", "schedule_type", "job_type", "execution_mode", "priority", "retry_limit"}, Risk: string(RiskWrite)},
		{Name: "list_cron_jobs", Description: "列出定时任务", Params: []string{"agent_id", "enabled_only"}, Risk: string(RiskRead)},
		{Name: "remove_cron_job", Description: "移除定时任务", Params: []string{"id"}, Risk: string(RiskWrite)},
		{Name: "update_cron_job", Description: "更新定时任务", Params: []string{"id", "name", "schedule", "payload", "enabled", "priority", "retry_limit"}, Risk: string(RiskWrite)},
	}

	set := make(map[string]struct{}, len(allowed))
	for _, item := range allowed {
		item = strings.TrimSpace(item)
		if item != "" {
			set[item] = struct{}{}
		}
	}

	out := make([]ToolSchema, 0, len(base)+8)
	for _, item := range base {
		if len(set) > 0 {
			if _, ok := set[item.Name]; !ok {
				continue
			}
		}
		out = append(out, item)
	}
	if e.mcp != nil {
		for _, item := range e.mcp.ListTools(ctx) {
			if len(set) > 0 {
				if _, ok := set[item.FullName]; !ok {
					continue
				}
			}
			out = append(out, ToolSchema{
				Name:        item.FullName,
				Description: firstNonEmpty(item.Description, fmt.Sprintf("MCP tool from %s", item.ServerName)),
				Params:      schemaParamNames(item.InputSchema),
				Risk:        item.Risk,
			})
		}
	}
	return out
}

func (e *Executor) Run(ctx context.Context, isMain bool, workspace string, allowed []string, call ToolCall) ToolResult {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		allowedSet[strings.TrimSpace(a)] = struct{}{}
	}
	if _, ok := allowedSet[call.Name]; !ok {
		return ToolResult{Name: call.Name, Error: "tool not allowed for this agent"}
	}

	risk := classify(call.Name)
	if strings.HasPrefix(call.Name, "mcp:") {
		risk = classifyExternal(call.Name)
	}
	requiresApproval := risk == RiskWrite || risk == RiskExec
	if e.approver != nil {
		decision := e.approver.Evaluate(approval.EvalInput{
			Tool:      call.Name,
			Risk:      string(risk),
			Params:    call.Params,
			Workspace: workspace,
		})
		if decision.RequiresApproval {
			return ToolResult{
				Name:             call.Name,
				Risk:             risk,
				RequiresApproval: true,
				ApprovalRequest:  decision.RequestID,
				ApprovalRuleID:   decision.RuleID,
				Error:            firstNonEmpty(decision.Message, "approval required by policy"),
			}
		}
		requiresApproval = false
	}
	if requiresApproval && !call.AutoApprove {
		return ToolResult{
			Name:             call.Name,
			Risk:             risk,
			RequiresApproval: true,
			Error:            "approval required for high-risk tool",
		}
	}

	switch call.Name {
	case "list_dir":
		return e.listDir(workspace, call.Params)
	case "read_file":
		return e.readFile(workspace, call.Params)
	case "write_file":
		return e.writeFile(workspace, call.Params)
	case "search_text":
		return e.searchText(ctx, workspace, call.Params)
	case "exec_cmd":
		return e.execCommand(ctx, isMain, workspace, call.Params)
	case "spawn_subagent":
		return e.spawnSubagent(workspace, call.Params)
	case "delegate_to_subagent":
		return e.delegateToSubagent(ctx, workspace, call.Params)
	case "list_subagents":
		return e.listSubagents()
	case "terminate_subagent":
		return e.terminateSubagent(call.Params)
	case "image_generate":
		return e.imageGenerate(ctx, workspace, call.Params)
	case "add_cron_job":
		return e.addCronJob(ctx, call.Params)
	case "list_cron_jobs":
		return e.listCronJobs(ctx, call.Params)
	case "remove_cron_job":
		return e.removeCronJob(ctx, call.Params)
	case "update_cron_job":
		return e.updateCronJob(ctx, call.Params)
	default:
		if strings.HasPrefix(call.Name, "mcp:") {
			return e.callExternalTool(ctx, call.Name, call.Params, risk)
		}
		return ToolResult{Name: call.Name, Risk: risk, Error: "unknown tool"}
	}
}

func classify(name string) RiskLevel {
	switch name {
	case "list_dir", "read_file", "search_text", "list_subagents", "list_cron_jobs":
		return RiskRead
	case "write_file", "spawn_subagent", "terminate_subagent", "image_generate", "add_cron_job", "remove_cron_job", "update_cron_job":
		return RiskWrite
	case "exec_cmd", "delegate_to_subagent":
		return RiskExec
	default:
		return RiskWrite
	}
}

func classifyExternal(name string) RiskLevel {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(lower, ":read"), strings.Contains(lower, ":list"), strings.Contains(lower, ":get"), strings.Contains(lower, ":search"):
		return RiskRead
	case strings.Contains(lower, ":write"), strings.Contains(lower, ":create"), strings.Contains(lower, ":update"), strings.Contains(lower, ":delete"), strings.Contains(lower, ":install"):
		return RiskWrite
	default:
		return RiskExec
	}
}

func (e *Executor) callExternalTool(ctx context.Context, name string, params map[string]string, risk RiskLevel) ToolResult {
	if e.mcp == nil {
		return ToolResult{Name: name, Risk: risk, Error: "mcp manager not configured"}
	}
	tool, output, err := e.mcp.CallTool(ctx, name, params)
	if err != nil {
		return ToolResult{Name: name, Risk: risk, Error: err.Error()}
	}
	detail := output
	if tool.ServerName != "" {
		detail = fmt.Sprintf("[%s] %s", tool.ServerName, detail)
	}
	return ToolResult{Name: name, Risk: risk, Output: detail}
}

func schemaParamNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var payload struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	names := make([]string, 0, len(payload.Properties))
	for name := range payload.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (e *Executor) listDir(workspace string, params map[string]string) ToolResult {
	if !e.sandbox.CanReadWorkspace() {
		return ToolResult{Name: "list_dir", Risk: RiskRead, Error: "sandbox workspace access is none"}
	}
	target := params["path"]
	abs, err := safePath(workspace, target)
	if err != nil {
		return ToolResult{Name: "list_dir", Risk: RiskRead, Error: err.Error()}
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return ToolResult{Name: "list_dir", Risk: RiskRead, Error: err.Error()}
	}

	names := make([]string, 0, len(entries))
	for _, en := range entries {
		name := en.Name()
		if en.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	return ToolResult{Name: "list_dir", Risk: RiskRead, Output: strings.Join(names, "\n")}
}

func (e *Executor) readFile(workspace string, params map[string]string) ToolResult {
	if !e.sandbox.CanReadWorkspace() {
		return ToolResult{Name: "read_file", Risk: RiskRead, Error: "sandbox workspace access is none"}
	}
	target := params["path"]
	abs, err := safePath(workspace, target)
	if err != nil {
		return ToolResult{Name: "read_file", Risk: RiskRead, Error: err.Error()}
	}

	b, err := os.ReadFile(abs)
	if err != nil {
		return ToolResult{Name: "read_file", Risk: RiskRead, Error: err.Error()}
	}

	content := string(b)
	if len(content) > 12000 {
		content = content[:12000] + "\n...<truncated>"
	}
	return ToolResult{Name: "read_file", Risk: RiskRead, Output: content}
}

func (e *Executor) writeFile(workspace string, params map[string]string) ToolResult {
	if !e.sandbox.CanWriteWorkspace() {
		return ToolResult{Name: "write_file", Risk: RiskWrite, Error: "sandbox workspace access is not rw"}
	}

	pipeline := harness.NewPipeline(workspace)
	results := pipeline.Run(context.Background())
	if harness.ShouldBlock(results) {
		report := harness.FormatResults(results)
		return ToolResult{
			Name:  "write_file",
			Risk:  RiskWrite,
			Error: "harness validation failed:\n" + report,
		}
	}

	target := params["path"]
	abs, err := safePath(workspace, target)
	if err != nil {
		return ToolResult{Name: "write_file", Risk: RiskWrite, Error: err.Error()}
	}

	content := params["content"]
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return ToolResult{Name: "write_file", Risk: RiskWrite, Error: err.Error()}
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return ToolResult{Name: "write_file", Risk: RiskWrite, Error: err.Error()}
	}

	return ToolResult{Name: "write_file", Risk: RiskWrite, Output: "ok"}
}

func (e *Executor) searchText(ctx context.Context, workspace string, params map[string]string) ToolResult {
	if !e.sandbox.CanReadWorkspace() {
		return ToolResult{Name: "search_text", Risk: RiskRead, Error: "sandbox workspace access is none"}
	}
	query := strings.TrimSpace(params["query"])
	if query == "" {
		return ToolResult{Name: "search_text", Risk: RiskRead, Error: "query is empty"}
	}

	target := params["path"]
	if target == "" {
		target = "."
	}
	abs, err := safePath(workspace, target)
	if err != nil {
		return ToolResult{Name: "search_text", Risk: RiskRead, Error: err.Error()}
	}

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, "rg", "--line-number", "--hidden", "--glob", "!.git", query, abs)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) == 0 {
			return ToolResult{Name: "search_text", Risk: RiskRead, Error: err.Error()}
		}
	}

	result := string(out)
	if len(result) > 12000 {
		result = result[:12000] + "\n...<truncated>"
	}
	return ToolResult{Name: "search_text", Risk: RiskRead, Output: result}
}

func (e *Executor) execCommand(ctx context.Context, isMain bool, workspace string, params map[string]string) ToolResult {
	command := strings.TrimSpace(params["command"])
	rawArgs := strings.TrimSpace(params["args"])
	args := splitArgs(rawArgs)

	res, err := e.sandbox.Exec(ctx, isMain, workspace, command, args, 30*time.Second)
	if err != nil {
		return ToolResult{Name: "exec_cmd", Risk: RiskExec, Error: err.Error()}
	}

	output := strings.TrimSpace(res.Stdout)
	if strings.TrimSpace(res.Stderr) != "" {
		if output != "" {
			output += "\n"
		}
		output += res.Stderr
	}
	return ToolResult{Name: "exec_cmd", Risk: RiskExec, Output: output}
}

func (e *Executor) spawnSubagent(workspace string, params map[string]string) ToolResult {
	role := firstNonEmpty(params["role"], params["description"], "通用子智能体")
	id := fmt.Sprintf("sub_%d", time.Now().UnixNano())
	dir := filepath.Join(workspace, ".subagents", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ToolResult{Name: "spawn_subagent", Risk: RiskWrite, Error: err.Error()}
	}
	item := subagentInfo{
		ID:        id,
		Role:      role,
		Workspace: dir,
		Tools:     parseSubagentTools(params["tools"]),
		Status:    "idle",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	e.subagentM.Lock()
	e.subagents[item.ID] = item
	e.subagentM.Unlock()
	raw, _ := json.Marshal(item)
	return ToolResult{Name: "spawn_subagent", Risk: RiskWrite, Output: string(raw)}
}

func (e *Executor) delegateToSubagent(ctx context.Context, workspace string, params map[string]string) ToolResult {
	id := strings.TrimSpace(params["id"])
	if id == "" {
		id = strings.TrimSpace(params["subagent_id"])
	}
	if id == "" {
		return ToolResult{Name: "delegate_to_subagent", Risk: RiskExec, Error: "id is required"}
	}
	e.subagentM.RLock()
	sub, ok := e.subagents[id]
	e.subagentM.RUnlock()
	if !ok {
		return ToolResult{Name: "delegate_to_subagent", Risk: RiskExec, Error: "subagent not found"}
	}

	task := strings.TrimSpace(params["task"])
	if task == "" {
		task = strings.TrimSpace(params["prompt"])
	}
	if task == "" {
		task = "执行委托任务"
	}

	resultText := ""
	cmd := strings.TrimSpace(params["command"])
	if cmd != "" {
		execRes := e.execCommand(ctx, false, sub.Workspace, map[string]string{"command": cmd, "args": params["args"]})
		if execRes.Error != "" {
			return ToolResult{Name: "delegate_to_subagent", Risk: RiskExec, Error: execRes.Error}
		}
		resultText = execRes.Output
	} else {
		notePath := filepath.Join(sub.Workspace, "task.log")
		note := fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), task)
		f, err := os.OpenFile(notePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return ToolResult{Name: "delegate_to_subagent", Risk: RiskExec, Error: err.Error()}
		}
		_, _ = f.WriteString(note)
		_ = f.Close()
		resultText = "task recorded"
	}

	sub.Status = "completed"
	sub.LastTask = task
	sub.LastResult = trimText(resultText, 1800)
	sub.UpdatedAt = time.Now().UTC()
	e.subagentM.Lock()
	e.subagents[sub.ID] = sub
	e.subagentM.Unlock()

	payload := map[string]interface{}{
		"id":          sub.ID,
		"status":      sub.Status,
		"task":        sub.LastTask,
		"result":      sub.LastResult,
		"workspace":   sub.Workspace,
		"parent_work": workspace,
	}
	raw, _ := json.Marshal(payload)
	return ToolResult{Name: "delegate_to_subagent", Risk: RiskExec, Output: string(raw)}
}

func (e *Executor) listSubagents() ToolResult {
	e.subagentM.RLock()
	items := make([]subagentInfo, 0, len(e.subagents))
	for _, item := range e.subagents {
		items = append(items, item)
	}
	e.subagentM.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	raw, _ := json.Marshal(items)
	return ToolResult{Name: "list_subagents", Risk: RiskRead, Output: string(raw)}
}

func (e *Executor) terminateSubagent(params map[string]string) ToolResult {
	id := strings.TrimSpace(params["id"])
	if id == "" {
		id = strings.TrimSpace(params["subagent_id"])
	}
	if id == "" {
		return ToolResult{Name: "terminate_subagent", Risk: RiskWrite, Error: "id is required"}
	}
	e.subagentM.Lock()
	sub, ok := e.subagents[id]
	if ok {
		delete(e.subagents, id)
	}
	e.subagentM.Unlock()
	if !ok {
		return ToolResult{Name: "terminate_subagent", Risk: RiskWrite, Error: "subagent not found"}
	}
	_ = os.RemoveAll(sub.Workspace)
	payload := map[string]string{"id": id, "status": "terminated"}
	raw, _ := json.Marshal(payload)
	return ToolResult{Name: "terminate_subagent", Risk: RiskWrite, Output: string(raw)}
}

func (e *Executor) imageGenerate(ctx context.Context, workspace string, params map[string]string) ToolResult {
	prompt := strings.TrimSpace(params["prompt"])
	if prompt == "" {
		return ToolResult{Name: "image_generate", Risk: RiskWrite, Error: "prompt is required"}
	}
	model := firstNonEmpty(params["model"], "gpt-image-1")
	size := firstNonEmpty(params["size"], "1024x1024")
	style := strings.TrimSpace(params["style"])

	outDir := filepath.Join(workspace, "generated")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return ToolResult{Name: "image_generate", Risk: RiskWrite, Error: err.Error()}
	}
	nameBase := fmt.Sprintf("img_%d", time.Now().UnixNano())

	apiKey := firstNonEmpty(strings.TrimSpace(params["api_key"]), strings.TrimSpace(os.Getenv("OPENAI_API_KEY")))
	if apiKey != "" {
		payload := map[string]interface{}{"model": model, "prompt": prompt, "size": size}
		if style != "" {
			payload["style"] = style
		}
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/images/generations", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			rawResp, _ := ioReadAllLimit(resp.Body, 8<<20)
			_ = resp.Body.Close()
			if resp.StatusCode < 400 {
				var parsed struct {
					Data []struct {
						B64JSON       string `json:"b64_json"`
						URL           string `json:"url"`
						RevisedPrompt string `json:"revised_prompt"`
					} `json:"data"`
				}
				if json.Unmarshal(rawResp, &parsed) == nil && len(parsed.Data) > 0 {
					if parsed.Data[0].B64JSON != "" {
						bin, decErr := base64.StdEncoding.DecodeString(parsed.Data[0].B64JSON)
						if decErr == nil {
							path := filepath.Join(outDir, nameBase+".png")
							if writeErr := os.WriteFile(path, bin, 0o644); writeErr == nil {
								payload := map[string]string{"mode": "openai", "path": path, "revised_prompt": parsed.Data[0].RevisedPrompt}
								raw, _ := json.Marshal(payload)
								return ToolResult{Name: "image_generate", Risk: RiskWrite, Output: string(raw)}
							}
						}
					}
					if parsed.Data[0].URL != "" {
						payload := map[string]string{"mode": "openai", "url": parsed.Data[0].URL, "revised_prompt": parsed.Data[0].RevisedPrompt}
						raw, _ := json.Marshal(payload)
						return ToolResult{Name: "image_generate", Risk: RiskWrite, Output: string(raw)}
					}
				}
			}
		}
	}

	svg := fallbackSVG(prompt, size, style)
	path := filepath.Join(outDir, nameBase+".svg")
	if err := os.WriteFile(path, []byte(svg), 0o644); err != nil {
		return ToolResult{Name: "image_generate", Risk: RiskWrite, Error: err.Error()}
	}
	payload := map[string]string{"mode": "local-svg", "path": path, "prompt": prompt}
	raw, _ := json.Marshal(payload)
	return ToolResult{Name: "image_generate", Risk: RiskWrite, Output: string(raw)}
}

func (e *Executor) addCronJob(ctx context.Context, params map[string]string) ToolResult {
	if e.cronStore == nil {
		return ToolResult{Name: "add_cron_job", Risk: RiskWrite, Error: "cron store not configured"}
	}
	name := strings.TrimSpace(params["name"])
	if name == "" {
		return ToolResult{Name: "add_cron_job", Risk: RiskWrite, Error: "name is required"}
	}
	schedule := strings.TrimSpace(params["schedule"])
	if schedule == "" {
		return ToolResult{Name: "add_cron_job", Risk: RiskWrite, Error: "schedule is required"}
	}
	scheduleType := strings.TrimSpace(params["schedule_type"])
	if scheduleType == "" {
		scheduleType = "cron"
	}
	jobType := strings.TrimSpace(params["job_type"])
	if jobType == "" {
		jobType = "message"
	}
	payload := strings.TrimSpace(params["payload"])
	if payload == "" {
		payload = strings.TrimSpace(params["message"])
	}
	if payload == "" {
		return ToolResult{Name: "add_cron_job", Risk: RiskWrite, Error: "payload is required"}
	}
	execMode := strings.TrimSpace(params["execution_mode"])
	if execMode == "" {
		execMode = "main"
	}
	priority := strings.TrimSpace(params["priority"])
	if priority == "" {
		priority = "normal"
	}
	retryLimit := 3
	if v := strings.TrimSpace(params["retry_limit"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			retryLimit = n
		}
	}
	agentID := e.cronAgentID
	if v := strings.TrimSpace(params["agent_id"]); v != "" {
		agentID = v
	}
	if agentID == "" {
		return ToolResult{Name: "add_cron_job", Risk: RiskWrite, Error: "agent_id not set"}
	}
	job := models.CronJob{
		ID:            fmt.Sprintf("cron_%d", time.Now().UnixNano()),
		AgentID:       agentID,
		Name:          name,
		Schedule:      schedule,
		ScheduleType:  scheduleType,
		JobType:       jobType,
		Payload:       payload,
		ExecutionMode: execMode,
		SessionID:     strings.TrimSpace(params["session_id"]),
		TargetChannel: strings.TrimSpace(params["target_channel"]),
		Priority:      priority,
		Enabled:       true,
		RetryLimit:    retryLimit,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := e.cronStore.CreateCronJob(ctx, job); err != nil {
		return ToolResult{Name: "add_cron_job", Risk: RiskWrite, Error: err.Error()}
	}
	raw, _ := json.Marshal(map[string]string{"id": job.ID, "name": job.Name, "schedule": job.Schedule, "status": "created"})
	return ToolResult{Name: "add_cron_job", Risk: RiskWrite, Output: string(raw)}
}

func (e *Executor) listCronJobs(ctx context.Context, params map[string]string) ToolResult {
	if e.cronStore == nil {
		return ToolResult{Name: "list_cron_jobs", Risk: RiskRead, Error: "cron store not configured"}
	}
	agentID := e.cronAgentID
	if v := strings.TrimSpace(params["agent_id"]); v != "" {
		agentID = v
	}
	enabledOnly := false
	if v := strings.TrimSpace(params["enabled_only"]); v == "true" || v == "1" {
		enabledOnly = true
	}
	jobs, err := e.cronStore.ListCronJobs(ctx, agentID, enabledOnly)
	if err != nil {
		return ToolResult{Name: "list_cron_jobs", Risk: RiskRead, Error: err.Error()}
	}
	raw, _ := json.Marshal(jobs)
	return ToolResult{Name: "list_cron_jobs", Risk: RiskRead, Output: string(raw)}
}

func (e *Executor) removeCronJob(ctx context.Context, params map[string]string) ToolResult {
	if e.cronStore == nil {
		return ToolResult{Name: "remove_cron_job", Risk: RiskWrite, Error: "cron store not configured"}
	}
	id := strings.TrimSpace(params["id"])
	if id == "" {
		id = strings.TrimSpace(params["cron_id"])
	}
	if id == "" {
		return ToolResult{Name: "remove_cron_job", Risk: RiskWrite, Error: "id is required"}
	}
	if err := e.cronStore.DeleteCronJob(ctx, id); err != nil {
		return ToolResult{Name: "remove_cron_job", Risk: RiskWrite, Error: err.Error()}
	}
	raw, _ := json.Marshal(map[string]string{"id": id, "status": "removed"})
	return ToolResult{Name: "remove_cron_job", Risk: RiskWrite, Output: string(raw)}
}

func (e *Executor) updateCronJob(ctx context.Context, params map[string]string) ToolResult {
	if e.cronStore == nil {
		return ToolResult{Name: "update_cron_job", Risk: RiskWrite, Error: "cron store not configured"}
	}
	id := strings.TrimSpace(params["id"])
	if id == "" {
		id = strings.TrimSpace(params["cron_id"])
	}
	if id == "" {
		return ToolResult{Name: "update_cron_job", Risk: RiskWrite, Error: "id is required"}
	}
	agentID := e.cronAgentID
	if v := strings.TrimSpace(params["agent_id"]); v != "" {
		agentID = v
	}
	jobs, err := e.cronStore.ListCronJobs(ctx, agentID, false)
	if err != nil {
		return ToolResult{Name: "update_cron_job", Risk: RiskWrite, Error: err.Error()}
	}
	var existing *models.CronJob
	for i := range jobs {
		if jobs[i].ID == id {
			existing = &jobs[i]
			break
		}
	}
	if existing == nil {
		return ToolResult{Name: "update_cron_job", Risk: RiskWrite, Error: "cron job not found"}
	}
	if v := strings.TrimSpace(params["name"]); v != "" {
		existing.Name = v
	}
	if v := strings.TrimSpace(params["schedule"]); v != "" {
		existing.Schedule = v
	}
	if v := strings.TrimSpace(params["schedule_type"]); v != "" {
		existing.ScheduleType = v
	}
	if v := strings.TrimSpace(params["payload"]); v != "" {
		existing.Payload = v
	}
	if v := strings.TrimSpace(params["execution_mode"]); v != "" {
		existing.ExecutionMode = v
	}
	if v := strings.TrimSpace(params["priority"]); v != "" {
		existing.Priority = v
	}
	if v := strings.TrimSpace(params["enabled"]); v == "true" {
		existing.Enabled = true
	} else if v == "false" {
		existing.Enabled = false
	}
	if v := strings.TrimSpace(params["retry_limit"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			existing.RetryLimit = n
		}
	}
	existing.UpdatedAt = time.Now().UTC()
	if err := e.cronStore.UpdateCronJob(ctx, *existing); err != nil {
		return ToolResult{Name: "update_cron_job", Risk: RiskWrite, Error: err.Error()}
	}
	raw, _ := json.Marshal(map[string]string{"id": existing.ID, "name": existing.Name, "status": "updated"})
	return ToolResult{Name: "update_cron_job", Risk: RiskWrite, Output: string(raw)}
}

func safePath(root, target string) (string, error) {
	if root == "" {
		return "", errors.New("workspace root is empty")
	}
	if target == "" {
		target = "."
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("abs root: %w", err)
	}

	joined := filepath.Join(absRoot, target)
	absTarget, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("abs target: %w", err)
	}

	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return "", fmt.Errorf("rel target: %w", err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes workspace")
	}

	return absTarget, nil
}

func splitArgs(raw string) []string {
	if raw == "" {
		return nil
	}
	chunks := strings.Fields(raw)
	return chunks
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseSubagentTools(raw string) []string {
	allowed := map[string]struct{}{
		"list_dir": {}, "read_file": {}, "search_text": {}, "write_file": {}, "exec_cmd": {}, "image_generate": {},
	}
	if strings.TrimSpace(raw) == "" {
		return []string{"list_dir", "read_file", "search_text"}
	}
	parts := strings.Split(raw, ",")
	seen := map[string]struct{}{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		k := strings.TrimSpace(p)
		if _, ok := allowed[k]; !ok {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	if len(out) == 0 {
		return []string{"list_dir", "read_file", "search_text"}
	}
	return out
}

func trimText(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + " ..."
}

func fallbackSVG(prompt, size, style string) string {
	w, h := 1024, 1024
	if strings.Contains(size, "x") {
		parts := strings.SplitN(size, "x", 2)
		if len(parts) == 2 {
			if v, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil && v >= 128 && v <= 2048 {
				w = v
			}
			if v, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil && v >= 128 && v <= 2048 {
				h = v
			}
		}
	}
	accent := "#2E7DD1"
	if strings.Contains(strings.ToLower(style), "warm") {
		accent = "#D96B2B"
	}
	escaped := xmlEscape(trimText(prompt, 220))
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">
  <defs>
    <linearGradient id="g" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0%%" stop-color="#0F172A"/>
      <stop offset="100%%" stop-color="%s"/>
    </linearGradient>
  </defs>
  <rect width="100%%" height="100%%" fill="url(#g)"/>
  <circle cx="%d" cy="%d" r="%d" fill="rgba(255,255,255,0.08)"/>
  <text x="48" y="80" fill="#F8FAFC" font-size="34" font-family="monospace">AI Generated (fallback)</text>
  <foreignObject x="48" y="120" width="%d" height="%d">
    <div xmlns="http://www.w3.org/1999/xhtml" style="font: 24px sans-serif; color: #E2E8F0; line-height: 1.4;">%s</div>
  </foreignObject>
</svg>`, w, h, w, h, accent, w-180, h-180, minInt(w, h)/4, w-96, h-180, escaped)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

func ioReadAllLimit(rc io.ReadCloser, limit int64) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}
