package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"xclaw/cli/audit"
	"xclaw/cli/db"
	"xclaw/cli/harness"
	"xclaw/cli/llm"
	"xclaw/cli/models"
	"xclaw/cli/queue"
	"xclaw/cli/skills"
	"xclaw/cli/tools"
	"xclaw/cli/workspace"
)

type Service struct {
	store       *db.Store
	ws          *workspace.Manager
	parser      *RoleParser
	queue       *queue.LaneQueue
	tools       *tools.Executor
	audit       *audit.Logger
	events      *EventHub
	plans       *PlanCache
	gateway     llm.Gateway
	skillLoader *skills.Loader

	presence PresenceEmitter
	resumeFn func(context.Context, models.Session, models.Message, bool) error
}

type PresenceEmitter interface {
	Emit(ctx context.Context, agentID, sessionID, state, message string)
}

const (
	PresenceThinking  = "thinking"
	PresenceTyping    = "typing"
	PresenceExecuting = "executing"
	PresenceIdle      = "idle"
)

type CreateAgentInput struct {
	Name          string   `json:"name"`
	Emoji         string   `json:"emoji"`
	Description   string   `json:"description"`
	ModelProvider string   `json:"model_provider"`
	ModelName     string   `json:"model_name"`
	Tools         []string `json:"tools"`
}

type UpdateAgentInput struct {
	Name          string   `json:"name"`
	Emoji         string   `json:"emoji"`
	Description   string   `json:"description"`
	ModelProvider string   `json:"model_provider"`
	ModelName     string   `json:"model_name"`
	Tools         []string `json:"tools"`
}

type SendMessageInput struct {
	Content     string                  `json:"content"`
	AutoApprove bool                    `json:"auto_approve"`
	ReplyToID   string                  `json:"reply_to_id,omitempty"`
	Attachments []SendMessageAttachment `json:"attachments,omitempty"`
	Poll        *SendMessagePoll        `json:"poll,omitempty"`
	Metadata    map[string]any          `json:"metadata,omitempty"`
}

type SendMessageAttachment struct {
	ID          string `json:"id,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Name        string `json:"name,omitempty"`
	URL         string `json:"url,omitempty"`
	MIME        string `json:"mime,omitempty"`
	Size        int64  `json:"size,omitempty"`
	DurationSec int    `json:"duration_sec,omitempty"`
	Transcript  string `json:"transcript,omitempty"`
}

type SendMessagePoll struct {
	Question string                  `json:"question"`
	Options  []SendMessagePollOption `json:"options"`
}

type SendMessagePollOption struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label"`
	Votes int    `json:"votes,omitempty"`
}

func NewService(store *db.Store, ws *workspace.Manager, q *queue.LaneQueue, te *tools.Executor, al *audit.Logger, events *EventHub, plans *PlanCache, gateway llm.Gateway) *Service {
	return &Service{
		store:   store,
		ws:      ws,
		parser:  NewRoleParser(gateway),
		queue:   q,
		tools:   te,
		audit:   al,
		events:  events,
		plans:   plans,
		gateway: gateway,
	}
}

func (s *Service) SetSkillLoader(loader *skills.Loader) {
	s.skillLoader = loader
}

func (s *Service) SetPresenceEmitter(emitter PresenceEmitter) {
	s.presence = emitter
}

func (s *Service) setSessionStatus(ctx context.Context, sessionID, status string) {
	status = strings.TrimSpace(status)
	if sessionID == "" || status == "" {
		return
	}
	if err := s.store.UpdateSessionStatus(ctx, sessionID, status); err != nil {
		return
	}
	s.events.Publish(Event{
		Type:      "session.status",
		SessionID: sessionID,
		Data: map[string]any{
			"status": status,
		},
	})
}

func (s *Service) SetResumeRunner(fn func(context.Context, models.Session, models.Message, bool) error) {
	s.resumeFn = fn
}

func (s *Service) CreateAgent(ctx context.Context, in CreateAgentInput) (models.Agent, error) {
	now := time.Now().UTC()
	parsed := s.parser.Parse(in.Description)
	instruction := s.parser.ToSystemInstruction(in.Name, parsed)

	agent := models.Agent{
		ID:                NewID("agent"),
		Name:              defaultIfEmpty(strings.TrimSpace(in.Name), "通用执行Agent"),
		Emoji:             defaultIfEmpty(strings.TrimSpace(in.Emoji), "🤖"),
		Description:       defaultIfEmpty(strings.TrimSpace(in.Description), "负责理解并完成用户任务"),
		SystemInstruction: instruction,
		ModelProvider:     defaultIfEmpty(strings.TrimSpace(in.ModelProvider), "local"),
		ModelName:         defaultIfEmpty(strings.TrimSpace(in.ModelName), "local-reasoner"),
		Tools:             normalizeTools(in.Tools),
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	workspacePath, err := s.ws.EnsureAgentWorkspace(agent)
	if err != nil {
		return models.Agent{}, err
	}
	agent.WorkspacePath = workspacePath

	if err := s.store.CreateAgent(ctx, agent); err != nil {
		return models.Agent{}, err
	}
	s.audit.Log(ctx, agent.ID, "", "agent", "create", "agent created")

	return agent, nil
}

func (s *Service) ListAgents(ctx context.Context) ([]models.Agent, error) {
	return s.store.ListAgents(ctx)
}

func (s *Service) GetAgent(ctx context.Context, id string) (models.Agent, error) {
	return s.store.GetAgent(ctx, id)
}

func (s *Service) UpdateAgent(ctx context.Context, id string, in UpdateAgentInput) (models.Agent, error) {
	old, err := s.store.GetAgent(ctx, id)
	if err != nil {
		return models.Agent{}, err
	}

	parsed := s.parser.Parse(in.Description)
	old.Name = defaultIfEmpty(strings.TrimSpace(in.Name), old.Name)
	old.Emoji = defaultIfEmpty(strings.TrimSpace(in.Emoji), old.Emoji)
	old.Description = defaultIfEmpty(strings.TrimSpace(in.Description), old.Description)
	old.SystemInstruction = s.parser.ToSystemInstruction(old.Name, parsed)
	old.ModelProvider = defaultIfEmpty(strings.TrimSpace(in.ModelProvider), old.ModelProvider)
	old.ModelName = defaultIfEmpty(strings.TrimSpace(in.ModelName), old.ModelName)
	old.Tools = normalizeTools(in.Tools)
	old.UpdatedAt = time.Now().UTC()

	if err := s.store.UpdateAgent(ctx, old); err != nil {
		return models.Agent{}, err
	}
	if err := s.ws.SyncProfile(old); err != nil {
		return models.Agent{}, err
	}
	s.audit.Log(ctx, old.ID, "", "agent", "update", "agent updated")

	return old, nil
}

func (s *Service) DeleteAgent(ctx context.Context, id string) error {
	s.audit.Log(ctx, id, "", "agent", "delete", "agent deleted")
	return s.store.DeleteAgent(ctx, id)
}

func (s *Service) CreateSession(ctx context.Context, agentID, title string, isMain bool) (models.Session, error) {
	if _, err := s.store.GetAgent(ctx, agentID); err != nil {
		return models.Session{}, err
	}

	now := time.Now().UTC()
	session := models.Session{
		ID:        NewID("sess"),
		AgentID:   agentID,
		Title:     defaultIfEmpty(strings.TrimSpace(title), "新会话"),
		IsMain:    isMain,
		Status:    "idle",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.CreateSession(ctx, session); err != nil {
		return models.Session{}, err
	}
	s.audit.Log(ctx, agentID, session.ID, "session", "create", "session created")

	return session, nil
}

func (s *Service) ListSessions(ctx context.Context, agentID string) ([]models.Session, error) {
	return s.store.ListSessions(ctx, agentID)
}

func (s *Service) ListMessages(ctx context.Context, sessionID string, limit int) ([]models.Message, error) {
	return s.store.ListMessages(ctx, sessionID, limit)
}

func (s *Service) SendMessage(ctx context.Context, sessionID string, in SendMessageInput) (models.Message, error) {
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return models.Message{}, err
	}

	content := buildStructuredMessageContent(in)
	if content == "" {
		return models.Message{}, fmt.Errorf("message content cannot be empty")
	}
	metadataRaw, err := encodeMessageMetadata(in)
	if err != nil {
		return models.Message{}, fmt.Errorf("invalid message metadata: %w", err)
	}

	userMsg := models.Message{
		ID:        NewID("msg"),
		SessionID: sessionID,
		Role:      "user",
		Content:   content,
		Metadata:  metadataRaw,
		CreatedAt: time.Now().UTC(),
	}

	if err := s.store.CreateMessage(ctx, userMsg); err != nil {
		return models.Message{}, err
	}
	_ = s.store.TouchSession(ctx, session.ID)

	s.events.Publish(Event{Type: "message.created", SessionID: sessionID, Data: userMsg})
	s.audit.Log(ctx, session.AgentID, session.ID, "message", "user", userMsg.Content)

	laneID := "session:" + session.ID
	done := s.queue.Enqueue(context.Background(), laneID, func(taskCtx context.Context) error {
		return s.runAssistant(taskCtx, session, userMsg, in.AutoApprove)
	})

	go func() {
		<-done
	}()

	return userMsg, nil
}

func (s *Service) runAssistant(ctx context.Context, session models.Session, userMsg models.Message, autoApprove bool) error {
	s.setSessionStatus(ctx, session.ID, "running")
	defer func() {
		s.setSessionStatus(context.Background(), session.ID, "idle")
		_ = s.store.TouchSession(context.Background(), session.ID)
	}()

	agent, err := s.store.GetAgent(ctx, session.AgentID)
	if err != nil {
		s.events.Publish(Event{Type: "error", SessionID: session.ID, Data: err.Error()})
		return err
	}

	s.events.Publish(Event{
		Type:      "assistant.start",
		SessionID: session.ID,
		Data: map[string]any{
			"agent_id":        agent.ID,
			"user_message_id": userMsg.ID,
			"resume":          strings.EqualFold(strings.TrimSpace(session.Status), "recovering"),
		},
	})
	s.emitPresence(ctx, session, PresenceThinking, "正在思考中...")

	// Try deterministic plan cache first
	plan, fromCache, planHits, err := s.buildPlan(ctx, agent.ID, userMsg.Content)
	if err != nil {
		s.audit.Log(ctx, agent.ID, session.ID, "plan", "error", err.Error())
	}

	var reply string
	var metadata map[string]any
	failures := make([]string, 0)
	toolResults := make([]string, 0)

	if fromCache && len(plan.Steps) > 0 {
		// Use cached deterministic plan
		s.audit.Log(ctx, agent.ID, session.ID, "plan", "hit", "deterministic plan hit")
		scriptPath, scriptReady, _ := s.plans.MaybeCompileScript(agent.WorkspacePath, userMsg.Content, plan, planHits)
		reply, metadata, failures = s.runDeterministicPlan(ctx, session, agent, userMsg, plan, autoApprove, scriptPath, scriptReady)
		if observations, ok := metadata["observations"].([]string); ok {
			toolResults = append(toolResults, observations...)
		}
	} else {
		// Use full ReAct loop
		reactReply, steps, reactFailures, err := s.runReAct(ctx, session, agent, userMsg, autoApprove)
		failures = append(failures, reactFailures...)
		if err != nil {
			s.events.Publish(Event{Type: "error", SessionID: session.ID, Data: err.Error()})
			reply = "执行遇到错误：" + err.Error()
		} else {
			reply = reactReply
		}
		for _, step := range steps {
			if strings.TrimSpace(step.Action.Tool) == "" {
				continue
			}
			toolResults = append(toolResults, fmt.Sprintf("%s => %s", step.Action.Tool, trimOutput(step.Observation)))
		}
		metadata = map[string]any{"react_steps": len(steps), "mode": "react"}
	}

	// Stream response
	s.emitPresence(ctx, session, PresenceTyping, "正在输入中...")
	streamChunks := chunkText(reply, 28)
	for _, c := range streamChunks {
		s.events.Publish(Event{Type: "assistant.delta", SessionID: session.ID, Data: map[string]string{"chunk": c}})
		time.Sleep(15 * time.Millisecond)
	}

	metaJSON, _ := json.Marshal(metadata)
	assistantMsg := models.Message{
		ID:        NewID("msg"),
		SessionID: session.ID,
		Role:      "assistant",
		Content:   reply,
		Metadata:  string(metaJSON),
		CreatedAt: time.Now().UTC(),
	}

	if err := s.store.CreateMessage(ctx, assistantMsg); err != nil {
		s.events.Publish(Event{Type: "error", SessionID: session.ID, Data: err.Error()})
		return err
	}
	s.events.Publish(Event{Type: "message.created", SessionID: session.ID, Data: assistantMsg})
	s.events.Publish(Event{Type: "assistant.done", SessionID: session.ID, Data: map[string]string{"message_id": assistantMsg.ID}})
	s.emitPresence(ctx, session, PresenceIdle, "")

	s.audit.Log(ctx, agent.ID, session.ID, "message", "assistant", trimOutput(reply))
	_ = s.ws.AutoWriteMemory(agent.WorkspacePath, userMsg.Content, reply, toolResults)
	_ = s.ws.RecordReflection(agent.WorkspacePath, userMsg.Content, reply, toolResults, failures)
	_ = s.ws.LearnFromFailure(agent.WorkspacePath, failures)
	_ = s.ws.LearnUserPreference(agent.WorkspacePath, userMsg.Content, reply)
	_ = s.ws.AppendDailyMemory(agent.WorkspacePath, "会话沉淀", fmt.Sprintf("用户问题：%s\n结论：%s", userMsg.Content, trimOutput(reply)))
	_, _ = s.store.AddVectorMemory(ctx, agent.ID, session.ID, fmt.Sprintf("用户: %s\n助手: %s", userMsg.Content, trimOutput(reply)), buildHashEmbedding(userMsg.Content+"\n"+reply, semanticVectorDim))

	return nil
}

func (s *Service) RecoverInterruptedSessions(ctx context.Context) (int, error) {
	sessions, err := s.store.ListSessions(ctx, "")
	if err != nil {
		return 0, err
	}

	resumed := 0
	for _, session := range sessions {
		msgs, err := s.store.ListMessages(ctx, session.ID, 10000)
		if err != nil {
			s.audit.Log(context.Background(), session.AgentID, session.ID, "session", "resume_error", err.Error())
			continue
		}

		pending := collectPendingUserMessages(msgs)
		if len(pending) == 0 {
			status := strings.ToLower(strings.TrimSpace(session.Status))
			if status == "running" || status == "recovering" {
				s.setSessionStatus(context.Background(), session.ID, "idle")
			}
			continue
		}

		s.setSessionStatus(context.Background(), session.ID, "recovering")
		session.Status = "recovering"
		laneID := "session:" + session.ID
		for _, userMsg := range pending {
			resumed++
			currentSession := session
			currentUserMsg := userMsg
			autoApprove := decodeMessageAutoApprove(currentUserMsg.Metadata)
			s.audit.Log(
				context.Background(),
				currentSession.AgentID,
				currentSession.ID,
				"session",
				"resume_pending_message",
				fmt.Sprintf("resume assistant for user message %s", currentUserMsg.ID),
			)
			done := s.queue.Enqueue(context.Background(), laneID, func(taskCtx context.Context) error {
				if s.resumeFn != nil {
					return s.resumeFn(taskCtx, currentSession, currentUserMsg, autoApprove)
				}
				return s.runAssistant(taskCtx, currentSession, currentUserMsg, autoApprove)
			})
			go func(sess models.Session, msg models.Message) {
				if runErr := <-done; runErr != nil {
					s.audit.Log(
						context.Background(),
						sess.AgentID,
						sess.ID,
						"session",
						"resume_error",
						fmt.Sprintf("resume user message %s failed: %v", msg.ID, runErr),
					)
				}
			}(currentSession, currentUserMsg)
		}
	}

	return resumed, nil
}

func (s *Service) emitPresence(ctx context.Context, session models.Session, state, message string) {
	payload := map[string]string{
		"state":   strings.TrimSpace(state),
		"message": strings.TrimSpace(message),
	}
	s.events.Publish(Event{Type: "presence", SessionID: session.ID, Data: payload})
	if s.presence != nil {
		s.presence.Emit(ctx, session.AgentID, session.ID, state, message)
	}
}

func (s *Service) RunHeartbeat(ctx context.Context, agentID, prompt string, maxRounds int) (string, error) {
	if maxRounds <= 0 {
		maxRounds = 5
	}
	agent, err := s.store.GetAgent(ctx, strings.TrimSpace(agentID))
	if err != nil {
		return "", err
	}
	prompt = strings.TrimSpace(prompt) + fmt.Sprintf("\n\n[限制] 最多内部推理轮数=%d。", maxRounds)
	reply, err := s.gateway.Generate(ctx, agent.ModelProvider, agent.ModelName, prompt)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(reply), nil
}

func (s *Service) runDeterministicPlan(ctx context.Context, session models.Session, agent models.Agent, userMsg models.Message, plan DeterministicPlan, autoApprove bool, scriptPath string, scriptReady bool) (string, map[string]any, []string) {
	observation := make([]string, 0)
	planChanged := false
	ranPipeline := false
	failures := make([]string, 0)

	if scriptReady && strings.TrimSpace(scriptPath) != "" {
		scriptResult := s.tools.Run(ctx, session.IsMain, agent.WorkspacePath, agent.Tools, tools.ToolCall{
			Name: "exec_cmd",
			Params: map[string]string{
				"command": "sh",
				"args":    scriptPath,
			},
			AutoApprove: autoApprove,
		})
		if scriptResult.Error == "" {
			observation = append(observation, "命中确定性脚本缓存并执行成功: "+scriptPath)
			observation = append(observation, trimOutput(scriptResult.Output))
			return "已通过确定性脚本缓存完成本次高频任务。", map[string]any{
				"steps":              len(plan.Steps),
				"mode":               "deterministic-script-cache",
				"plan_changed":       false,
				"script_cache":       true,
				"script_path":        scriptPath,
				"observations":       observation,
				"deterministic_hits": true,
			}, failures
		}
		observation = append(observation, "脚本缓存执行失败，回退到逐步执行: "+scriptResult.Error)
		failures = append(failures, "script-cache execution failed: "+scriptResult.Error)
	}

	for idx, step := range plan.Steps {
		switch step.Kind {
		case "tool":
			s.emitPresence(ctx, session, PresenceExecuting, "正在执行工具...")
			result := s.tools.Run(ctx, session.IsMain, agent.WorkspacePath, agent.Tools, tools.ToolCall{
				Name:        step.Name,
				Params:      step.Params,
				AutoApprove: autoApprove,
			})
			s.emitPresence(ctx, session, PresenceThinking, "正在思考中...")
			meta := map[string]any{"step": idx + 1, "tool": step.Name, "result": result}
			s.events.Publish(Event{Type: "tool.result", SessionID: session.ID, Data: meta})
			raw, _ := json.Marshal(result)
			s.audit.Log(ctx, agent.ID, session.ID, "tool", step.Name, string(raw))

			if result.RequiresApproval {
				observation = append(observation, fmt.Sprintf("工具 %s 需要审批，已停止执行。", step.Name))
				planChanged = true
				goto FINAL
			}
			if result.Error != "" {
				observation = append(observation, fmt.Sprintf("工具 %s 失败：%s", step.Name, result.Error))
				failures = append(failures, fmt.Sprintf("%s: %s", step.Name, result.Error))
				planChanged = true
				continue
			}
			observation = append(observation, fmt.Sprintf("工具 %s 输出：%s", step.Name, trimOutput(result.Output)))
			if (step.Name == "write_file" || step.Name == "exec_cmd") && !ranPipeline {
				ranPipeline = true
				pipeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				results := harness.NewPipeline(agent.WorkspacePath).Run(pipeCtx)
				cancel()
				report := harness.FormatResults(results)
				observation = append(observation, report)
				s.audit.Log(ctx, agent.ID, session.ID, "harness", "pipeline", trimOutput(report))
				if harness.ShouldBlock(results) {
					planChanged = true
					failures = append(failures, "harness pipeline blocked execution")
					goto FINAL
				}
			}
		case "think":
			observation = append(observation, step.Params["content"])
		}
	}

FINAL:
	if planChanged {
		observation = append(observation, "原计划出现阻塞，已启用重排策略：转为总结当前结果并给出下一步建议。")
	}

	contextBundle, _ := s.ws.LoadRecentContext(agent.WorkspacePath)
	semanticMemory := s.buildSemanticMemoryContext(ctx, agent.ID, userMsg.Content)
	if semanticMemory != "" {
		contextBundle += "\n\n[向量记忆]\n" + semanticMemory
	}
	prompt := s.composePrompt(agent, userMsg.Content, contextBundle, observation)
	reply, err := s.gateway.Generate(ctx, agent.ModelProvider, agent.ModelName, prompt)
	if err != nil {
		reply = "模型调用失败，已降级输出：\n" + strings.Join(observation, "\n")
		failures = append(failures, "llm generate failed: "+err.Error())
	}

	return reply, map[string]any{
		"steps":        len(plan.Steps),
		"mode":         "deterministic",
		"plan_changed": planChanged,
		"observations": observation,
	}, failures
}

func (s *Service) buildPlan(ctx context.Context, agentID, userContent string) (DeterministicPlan, bool, int, error) {
	cached, meta, ok, err := s.plans.GetMeta(ctx, agentID, userContent)
	if err != nil {
		return DeterministicPlan{}, false, 0, err
	}
	if ok {
		return cached, true, meta.Hits + 1, nil
	}

	plan := parseUserPlan(userContent)
	if err := s.plans.Save(ctx, agentID, userContent, plan); err != nil {
		return DeterministicPlan{}, false, 0, err
	}
	return plan, false, 1, nil
}

func parseUserPlan(content string) DeterministicPlan {
	c := strings.TrimSpace(content)
	lower := strings.ToLower(c)

	if strings.HasPrefix(lower, "/tool ") {
		rest := strings.TrimSpace(c[6:])
		chunks := strings.Fields(rest)
		if len(chunks) > 0 {
			name := chunks[0]
			params := map[string]string{}
			for _, kv := range chunks[1:] {
				pair := strings.SplitN(kv, "=", 2)
				if len(pair) == 2 {
					params[pair[0]] = pair[1]
				}
			}
			return DeterministicPlan{Steps: []Step{{Kind: "tool", Name: name, Params: params}}}
		}
	}

	steps := make([]Step, 0)
	if strings.Contains(lower, "列出") && strings.Contains(lower, "目录") {
		steps = append(steps, Step{Kind: "tool", Name: "list_dir", Params: map[string]string{"path": "."}})
	}
	if strings.Contains(lower, "读取") && strings.Contains(lower, "文件") {
		path := extractAfterKeyword(c, "文件")
		if path == "" {
			path = "AGENTS.md"
		}
		steps = append(steps, Step{Kind: "tool", Name: "read_file", Params: map[string]string{"path": path}})
	}
	if strings.Contains(lower, "搜索") || strings.Contains(lower, "查找") {
		query := extractQuoted(c)
		if query == "" {
			query = "agent"
		}
		steps = append(steps, Step{Kind: "tool", Name: "search_text", Params: map[string]string{"query": query, "path": "."}})
	}
	if strings.Contains(lower, "执行命令") || strings.Contains(lower, "运行命令") {
		cmd := extractQuoted(c)
		if cmd != "" {
			fields := strings.Fields(cmd)
			if len(fields) > 0 {
				params := map[string]string{"command": fields[0], "args": strings.Join(fields[1:], " ")}
				steps = append(steps, Step{Kind: "tool", Name: "exec_cmd", Params: params})
			}
		}
	}

	if len(steps) == 0 {
		steps = append(steps, Step{Kind: "think", Params: map[string]string{"content": "当前任务无需额外工具，直接使用上下文进行推理。"}})
	}

	return DeterministicPlan{Steps: steps}
}

func (s *Service) composePrompt(agent models.Agent, userMsg, contextBundle string, observations []string) string {
	var b strings.Builder
	b.WriteString(agent.SystemInstruction)

	if s.skillLoader != nil {
		if summary := s.skillLoader.BuildSkillSummaries(); summary != "" {
			b.WriteString("\n\n")
			b.WriteString(summary)
		}
	}

	b.WriteString("\n\n[工作区上下文]\n")
	b.WriteString(contextBundle)
	b.WriteString("\n\n[用户消息]\n")
	b.WriteString(userMsg)
	b.WriteString("\n\n[执行观察]\n")
	if len(observations) == 0 {
		b.WriteString("- 无")
	} else {
		for _, o := range observations {
			b.WriteString("- ")
			b.WriteString(o)
			b.WriteByte('\n')
		}
	}
	b.WriteString("\n请给出最终回答，结构包含：结论、证据、下一步。")
	return b.String()
}

func defaultIfEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func normalizeTools(in []string) []string {
	if len(in) == 0 {
		return []string{
			"list_dir", "read_file", "search_text", "write_file", "exec_cmd",
			"spawn_subagent", "delegate_to_subagent", "list_subagents", "terminate_subagent", "image_generate",
		}
	}
	set := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := set[t]; ok {
			continue
		}
		set[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return []string{
			"list_dir", "read_file", "search_text", "write_file", "exec_cmd",
			"spawn_subagent", "delegate_to_subagent", "list_subagents", "terminate_subagent", "image_generate",
		}
	}
	return out
}

func trimOutput(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 800 {
		return s[:800] + " ..."
	}
	return s
}

func chunkText(s string, chunk int) []string {
	if chunk <= 0 {
		chunk = 20
	}
	runes := []rune(s)
	out := make([]string, 0, len(runes)/chunk+1)
	for i := 0; i < len(runes); i += chunk {
		end := i + chunk
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func extractQuoted(s string) string {
	start := strings.IndexRune(s, '"')
	if start < 0 {
		return ""
	}
	end := strings.IndexRune(s[start+1:], '"')
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(s[start+1 : start+1+end])
}

func extractAfterKeyword(s, key string) string {
	idx := strings.Index(s, key)
	if idx < 0 {
		return ""
	}
	tail := strings.TrimSpace(s[idx+len(key):])
	if tail == "" {
		return ""
	}
	fields := strings.Fields(tail)
	if len(fields) == 0 {
		return ""
	}
	candidate := strings.Trim(fields[0], "，。,.\"'")
	if strings.Contains(candidate, "/") || strings.Contains(candidate, ".") || strings.Contains(candidate, "-") {
		return candidate
	}
	return ""
}

func (s *Service) buildConversationContext(ctx context.Context, sessionID string) string {
	msgs, err := s.store.ListMessages(ctx, sessionID, 400)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	if len(msgs) <= 16 {
		var b strings.Builder
		for _, msg := range msgs {
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			b.WriteString(msg.Role)
			b.WriteString(": ")
			b.WriteString(trimOutput(msg.Content))
			b.WriteByte('\n')
		}
		return strings.TrimSpace(b.String())
	}

	older := msgs[:len(msgs)-12]
	recent := msgs[len(msgs)-12:]
	userCount := 0
	assistantCount := 0
	highlights := make([]string, 0, 6)
	for _, msg := range older {
		if msg.Role == "user" {
			userCount++
		}
		if msg.Role == "assistant" {
			assistantCount++
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		if strings.Contains(content, "失败") || strings.Contains(content, "报错") || strings.Contains(content, "完成") || strings.Contains(content, "关键") {
			highlights = append(highlights, trimOutput(content))
			if len(highlights) >= 6 {
				break
			}
		}
	}

	var b strings.Builder
	b.WriteString("历史消息已压缩。\n")
	b.WriteString(fmt.Sprintf("- 已压缩消息: %d (user=%d assistant=%d)\n", len(older), userCount, assistantCount))
	if len(highlights) > 0 {
		b.WriteString("- 关键摘要:\n")
		for _, h := range highlights {
			b.WriteString("  - ")
			b.WriteString(h)
			b.WriteByte('\n')
		}
	}
	b.WriteString("- 最近消息:\n")
	for _, msg := range recent {
		b.WriteString("  - ")
		b.WriteString(msg.Role)
		b.WriteString(": ")
		b.WriteString(trimOutput(msg.Content))
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func buildStructuredMessageContent(in SendMessageInput) string {
	content := strings.TrimSpace(in.Content)
	lines := make([]string, 0, 24)
	if content != "" {
		lines = append(lines, content)
	}

	replyID := strings.TrimSpace(in.ReplyToID)
	if replyID != "" {
		lines = append(lines, "", fmt.Sprintf("（引用消息 ID: %s）", replyID))
	}

	attachments := normalizeMessageAttachments(in.Attachments)
	if len(attachments) > 0 {
		lines = append(lines, "", "附件：")
		for _, att := range attachments {
			kind := strings.TrimSpace(att.Kind)
			if kind == "" {
				kind = "file"
			}
			name := strings.TrimSpace(att.Name)
			if name == "" {
				name = "unnamed"
			}
			sizeText := ""
			if att.Size > 0 {
				sizeText = fmt.Sprintf("，%d bytes", att.Size)
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s%s", kind, name, sizeText))
			if strings.TrimSpace(att.Transcript) != "" {
				lines = append(lines, fmt.Sprintf("  转写: %s", strings.TrimSpace(att.Transcript)))
			}
		}
	}

	if poll := normalizeMessagePoll(in.Poll); poll != nil {
		lines = append(lines, "", fmt.Sprintf("投票：%s", poll.Question))
		for _, option := range poll.Options {
			lines = append(lines, fmt.Sprintf("- %s", option.Label))
		}
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func encodeMessageMetadata(in SendMessageInput) (string, error) {
	meta := map[string]any{}
	meta["auto_approve"] = in.AutoApprove

	if replyID := strings.TrimSpace(in.ReplyToID); replyID != "" {
		meta["reply_to_id"] = replyID
	}

	if attachments := normalizeMessageAttachments(in.Attachments); len(attachments) > 0 {
		meta["attachments"] = attachments
	}

	if poll := normalizeMessagePoll(in.Poll); poll != nil {
		meta["poll"] = poll
	}

	if in.Metadata != nil {
		for key, value := range in.Metadata {
			k := strings.TrimSpace(key)
			if k == "" {
				continue
			}
			meta[k] = value
		}
	}

	if len(meta) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeMessageAutoApprove(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return false
	}
	value, ok := meta["auto_approve"]
	if !ok {
		return false
	}
	enabled, ok := value.(bool)
	return ok && enabled
}

func collectPendingUserMessages(messages []models.Message) []models.Message {
	pending := make([]models.Message, 0)
	for _, msg := range messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "user":
			pending = append(pending, msg)
		case "assistant":
			if len(pending) > 0 {
				pending = pending[1:]
			}
		}
	}
	return pending
}

func normalizeMessageAttachments(items []SendMessageAttachment) []SendMessageAttachment {
	out := make([]SendMessageAttachment, 0, len(items))
	for _, item := range items {
		entry := SendMessageAttachment{
			ID:          strings.TrimSpace(item.ID),
			Kind:        strings.TrimSpace(item.Kind),
			Name:        strings.TrimSpace(item.Name),
			URL:         strings.TrimSpace(item.URL),
			MIME:        strings.TrimSpace(item.MIME),
			Size:        item.Size,
			DurationSec: item.DurationSec,
			Transcript:  strings.TrimSpace(item.Transcript),
		}
		if entry.Size < 0 {
			entry.Size = 0
		}
		if entry.DurationSec < 0 {
			entry.DurationSec = 0
		}
		if entry.ID == "" && entry.Name == "" && entry.URL == "" && entry.Transcript == "" {
			continue
		}
		if entry.Kind == "" {
			entry.Kind = "file"
		}
		out = append(out, entry)
	}
	return out
}

func normalizeMessagePoll(poll *SendMessagePoll) *SendMessagePoll {
	if poll == nil {
		return nil
	}
	question := strings.TrimSpace(poll.Question)
	if question == "" {
		return nil
	}
	options := make([]SendMessagePollOption, 0, len(poll.Options))
	for _, option := range poll.Options {
		label := strings.TrimSpace(option.Label)
		if label == "" {
			continue
		}
		entry := SendMessagePollOption{
			ID:    strings.TrimSpace(option.ID),
			Label: label,
			Votes: option.Votes,
		}
		if entry.Votes < 0 {
			entry.Votes = 0
		}
		options = append(options, entry)
	}
	if len(options) == 0 {
		return nil
	}
	return &SendMessagePoll{
		Question: question,
		Options:  options,
	}
}

func IsNotFound(err error) bool {
	return err != nil && err == sql.ErrNoRows
}

func (s *Service) buildSkillContext() string {
	if s.skillLoader == nil {
		return ""
	}
	return s.skillLoader.BuildSkillSummaries()
}

func (s *Service) GetSkillFullPrompt(name string) (string, error) {
	if s.skillLoader == nil {
		return "", fmt.Errorf("skill loader not initialized")
	}
	return s.skillLoader.GetFullPrompt(name)
}
