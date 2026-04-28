package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"xclaw/cli/harness"
	"xclaw/cli/models"
	"xclaw/cli/tools"
)

const reactSystemPrompt = `
你是一个 ReAct (Reasoning + Acting) 智能体执行引擎。
你的任务是按照「思考→行动→观察」的循环，逐步解决用户问题。

本轮可用工具为“动态注入”，仅会提供与当前任务相关的子集。

输出格式（严格 JSON）：
{
  "thought": "当前思考内容",
  "action": {
    "tool": "工具名或空字符串",
    "params": {"参数名": "参数值"}
  }
}

规则：
1. 每次只输出一个 JSON 对象
2. action.tool 为空字符串表示任务完成，给出最终结论
3. 不要输出任何 JSON 之外的文本
4. 工具执行失败时，思考如何修复或绕过
5. 最多执行 15 轮，超过则总结当前结果
`

const maxReactIterations = 15

type ReactStep struct {
	Thought     string      `json:"thought"`
	Action      ReactAction `json:"action"`
	Observation string      `json:"observation,omitempty"`
}

type ReactAction struct {
	Tool   string            `json:"tool"`
	Params map[string]string `json:"params"`
}

func (s *Service) runReAct(ctx context.Context, session models.Session, agent models.Agent, userMsg models.Message, autoApprove bool) (string, []ReactStep, []string, error) {
	contextBundle, err := s.ws.LoadRecentContext(agent.WorkspacePath)
	if err != nil {
		return "", nil, nil, fmt.Errorf("load context: %w", err)
	}
	if conv := s.buildConversationContext(ctx, session.ID); conv != "" {
		contextBundle += "\n\n[会话上下文压缩]\n" + conv
	}
	if semanticMemory := s.buildSemanticMemoryContext(ctx, agent.ID, userMsg.Content); semanticMemory != "" {
		contextBundle += "\n\n[向量记忆]\n" + semanticMemory
	}

	steps := make([]ReactStep, 0, maxReactIterations)
	failures := make([]string, 0)
	var finalAnswer string
	ranPipeline := false
	blocked := false

	for i := 0; i < maxReactIterations; i++ {
		prompt := buildReActPrompt(agent.SystemInstruction, contextBundle, userMsg.Content, steps, s.tools.ListSchemas(ctx, agent.Tools), s.buildSkillContext())

		raw, err := s.gateway.Generate(ctx, agent.ModelProvider, agent.ModelName, prompt)
		if err != nil {
			failures = append(failures, "llm generate failed: "+err.Error())
			return "", steps, failures, fmt.Errorf("llm generate: %w", err)
		}

		step, err := parseReactStep(raw)
		if err != nil {
			// LLM 输出非 JSON，尝试作为最终答案
			step = ReactStep{
				Thought: "直接输出结论",
				Action:  ReactAction{Tool: ""},
			}
			finalAnswer = raw
		}

		// Stream thought to frontend
		s.events.Publish(Event{
			Type:      "react.thought",
			SessionID: session.ID,
			Data: map[string]any{
				"iteration": i + 1,
				"thought":   step.Thought,
			},
		})

		if step.Action.Tool == "" {
			// Task complete
			if finalAnswer == "" {
				finalAnswer = step.Thought
			}
			steps = append(steps, step)
			break
		}

		// Execute tool
		s.emitPresence(ctx, session, PresenceExecuting, "正在执行工具...")
		result := s.tools.Run(ctx, session.IsMain, agent.WorkspacePath, agent.Tools, tools.ToolCall{
			Name:        step.Action.Tool,
			Params:      step.Action.Params,
			AutoApprove: autoApprove,
		})
		s.emitPresence(ctx, session, PresenceThinking, "正在思考中...")

		observation := ""
		if result.Error != "" {
			observation = fmt.Sprintf("工具 %s 失败: %s", step.Action.Tool, result.Error)
			failures = append(failures, fmt.Sprintf("%s: %s", step.Action.Tool, result.Error))
		} else {
			observation = fmt.Sprintf("工具 %s 输出: %s", step.Action.Tool, trimOutput(result.Output))
			if (step.Action.Tool == "write_file" || step.Action.Tool == "exec_cmd") && !ranPipeline {
				ranPipeline = true
				pipeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				results := harness.NewPipeline(agent.WorkspacePath).Run(pipeCtx)
				cancel()
				report := harness.FormatResults(results)
				observation += "\n" + report
				s.audit.Log(ctx, agent.ID, session.ID, "harness", "pipeline", trimOutput(report))
				if harness.ShouldBlock(results) {
					step.Observation = observation
					steps = append(steps, step)
					finalAnswer = "自动化验证失败，流程已阻塞。请先修复验证问题再继续执行。"
					blocked = true
					failures = append(failures, "harness pipeline blocked execution")
				}
			}
		}

		if result.RequiresApproval {
			observation = fmt.Sprintf("工具 %s 需要用户审批", step.Action.Tool)
			step.Observation = observation
			steps = append(steps, step)
			finalAnswer = fmt.Sprintf("执行暂停：%s 需要审批。当前思考：%s", step.Action.Tool, step.Thought)
			break
		}
		if blocked {
			break
		}

		step.Observation = observation
		steps = append(steps, step)

		// Publish tool result event
		s.events.Publish(Event{
			Type:      "tool.result",
			SessionID: session.ID,
			Data: map[string]any{
				"iteration": i + 1,
				"tool":      step.Action.Tool,
				"result":    result,
			},
		})

		rawAudit, _ := json.Marshal(result)
		s.audit.Log(ctx, agent.ID, session.ID, "tool", step.Action.Tool, string(rawAudit))

		// Check if we should replan
		if result.Error != "" && i >= maxReactIterations-3 {
			// Near limit and error, force conclusion
			finalAnswer = fmt.Sprintf("执行遇到阻碍（%s），已接近最大迭代次数。当前结果：%s", result.Error, observation)
			break
		}
	}

	if finalAnswer == "" && len(steps) > 0 {
		finalAnswer = steps[len(steps)-1].Thought
	}

	return finalAnswer, steps, failures, nil
}

func buildReActPrompt(systemInstruction, contextBundle, userMsg string, steps []ReactStep, schemas []tools.ToolSchema, skillContext string) string {
	var b strings.Builder
	b.WriteString(reactSystemPrompt)
	b.WriteString("\n\n[可用工具 Schema]\n")
	b.WriteString(formatToolSchema(selectRelevantTools(schemas, userMsg)))
	b.WriteString("\n\n[角色设定]\n")
	b.WriteString(systemInstruction)
	if skillContext != "" {
		b.WriteString("\n\n")
		b.WriteString(skillContext)
	}
	b.WriteString("\n\n[工作区上下文]\n")
	b.WriteString(contextBundle)
	b.WriteString("\n\n[用户问题]\n")
	b.WriteString(userMsg)

	if len(steps) > 0 {
		b.WriteString("\n\n[执行历史]\n")
		for i, step := range steps {
			b.WriteString(fmt.Sprintf("轮次 %d:\n", i+1))
			b.WriteString(fmt.Sprintf("思考: %s\n", step.Thought))
			if step.Action.Tool != "" {
				b.WriteString(fmt.Sprintf("行动: %s %v\n", step.Action.Tool, step.Action.Params))
				b.WriteString(fmt.Sprintf("观察: %s\n", step.Observation))
			}
		}
	}

	b.WriteString("\n\n请输出下一步的 JSON（action.tool 为空表示完成）：")
	return b.String()
}

func selectRelevantTools(base []tools.ToolSchema, userMsg string) []tools.ToolSchema {
	selected := make([]tools.ToolSchema, 0, len(base))
	lower := strings.ToLower(userMsg)
	for _, tool := range base {
		if strings.Contains(lower, "执行") || strings.Contains(lower, "命令") {
			selected = append(selected, tool)
			continue
		}
		if strings.Contains(lower, "子智能体") || strings.Contains(lower, "子代理") || strings.Contains(lower, "subagent") || strings.Contains(lower, "委托") {
			if tool.Name == "spawn_subagent" || tool.Name == "delegate_to_subagent" || tool.Name == "list_subagents" || tool.Name == "terminate_subagent" {
				selected = append(selected, tool)
			}
			continue
		}
		if strings.Contains(lower, "图片") || strings.Contains(lower, "图像") || strings.Contains(lower, "image") || strings.Contains(lower, "画图") {
			if tool.Name == "image_generate" || tool.Name == "read_file" || tool.Name == "write_file" {
				selected = append(selected, tool)
			}
			continue
		}
		if strings.Contains(lower, "搜索") || strings.Contains(lower, "查找") {
			if tool.Name == "search_text" || tool.Name == "read_file" || tool.Name == "list_dir" {
				selected = append(selected, tool)
			}
			continue
		}
		if strings.Contains(lower, "修改") || strings.Contains(lower, "写入") || strings.Contains(lower, "生成") {
			if tool.Name == "write_file" || tool.Name == "read_file" || tool.Name == "list_dir" {
				selected = append(selected, tool)
			}
			continue
		}
		if tool.Name == "list_dir" || tool.Name == "read_file" {
			selected = append(selected, tool)
		}
	}
	if len(selected) == 0 {
		selected = append(selected, base...)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	return dedupToolSchemas(selected)
}

func dedupToolSchemas(in []tools.ToolSchema) []tools.ToolSchema {
	out := make([]tools.ToolSchema, 0, len(in))
	seen := map[string]struct{}{}
	for _, item := range in {
		if _, ok := seen[item.Name]; ok {
			continue
		}
		seen[item.Name] = struct{}{}
		out = append(out, item)
	}
	return out
}

func formatToolSchema(items []tools.ToolSchema) string {
	if len(items) == 0 {
		return "- 无可用工具，仅能给出结论。"
	}
	var b strings.Builder
	for _, item := range items {
		b.WriteString("- ")
		b.WriteString(item.Name)
		b.WriteString(": ")
		b.WriteString(item.Description)
		if len(item.Params) > 0 {
			b.WriteString("。参数: ")
			b.WriteString(strings.Join(item.Params, ", "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func parseReactStep(raw string) (ReactStep, error) {
	// Extract JSON from potentially mixed output
	text := strings.TrimSpace(raw)

	// Try to find JSON block
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		text = text[start : end+1]
	}

	var step ReactStep
	if err := json.Unmarshal([]byte(text), &step); err != nil {
		return ReactStep{}, err
	}
	return step, nil
}
