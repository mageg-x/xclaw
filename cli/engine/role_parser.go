package engine

import (
	"context"
	"fmt"
	"strings"

	"xclaw/cli/llm"
)

type RoleParsed struct {
	Responsibilities []string
	Tone             string
	Constraints      []string
}

type RoleParser struct {
	gateway llm.Gateway
}

// NewRoleParser creates a role parser with optional LLM enhancement
func NewRoleParser(gateway llm.Gateway) *RoleParser {
	return &RoleParser{gateway: gateway}
}

func (p *RoleParser) Parse(description string) RoleParsed {
	text := strings.TrimSpace(description)
	if text == "" {
		return RoleParsed{
			Responsibilities: []string{"响应用户任务", "拆解计划", "执行并回报结果"},
			Tone:             "专业、直接",
			Constraints:      []string{"不伪造结果", "高风险动作先审批", "保留审计记录"},
		}
	}

	parts := splitSentences(text)
	responsibilities := make([]string, 0, len(parts))
	constraints := make([]string, 0)
	tone := "专业、直接"

	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}

		low := strings.ToLower(s)
		if strings.Contains(low, "语气") || strings.Contains(low, "风格") {
			tone = s
			continue
		}
		if strings.Contains(low, "不要") || strings.Contains(low, "必须") || strings.Contains(low, "禁止") || strings.Contains(low, "不能") {
			constraints = append(constraints, s)
			continue
		}
		responsibilities = append(responsibilities, s)
	}

	if len(responsibilities) == 0 {
		responsibilities = []string{"理解用户意图并执行"}
	}
	if len(constraints) == 0 {
		constraints = []string{"不伪造结果", "执行危险动作前先审批"}
	}

	return RoleParsed{
		Responsibilities: responsibilities,
		Tone:             tone,
		Constraints:      constraints,
	}
}

// EnhanceWithLLM uses LLM to polish and expand role description into structured system instruction
func (p *RoleParser) EnhanceWithLLM(ctx context.Context, provider, model, name, description string) (string, error) {
	if p.gateway == nil {
		// Fallback to local parsing
		parsed := p.Parse(description)
		return p.ToSystemInstruction(name, parsed), nil
	}

	prompt := fmt.Sprintf(`你是一位专业的 AI 角色设计师。请将以下角色描述转化为结构化的 System Instruction。

角色名称：%s
角色描述：%s

要求：
1. 明确角色的核心职责（3-5条）
2. 定义行为风格和语气
3. 列出关键约束和边界
4. 包含执行框架说明（ReAct 循环）
5. 输出格式为 Markdown 列表

请直接输出 System Instruction 内容：`, name, description)

	reply, err := p.gateway.Generate(ctx, provider, model, prompt)
	if err != nil {
		// Fallback to local parsing on LLM failure
		parsed := p.Parse(description)
		return p.ToSystemInstruction(name, parsed), fmt.Errorf("llm enhance failed, using local parse: %w", err)
	}

	return strings.TrimSpace(reply), nil
}

func (p *RoleParser) ToSystemInstruction(name string, parsed RoleParsed) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("你是 %s。\n", name))
	b.WriteString("职责：\n")
	for _, r := range parsed.Responsibilities {
		b.WriteString("- ")
		b.WriteString(strings.TrimSpace(r))
		b.WriteString("\n")
	}
	b.WriteString("行为风格：")
	b.WriteString(strings.TrimSpace(parsed.Tone))
	b.WriteString("\n约束：\n")
	for _, c := range parsed.Constraints {
		b.WriteString("- ")
		b.WriteString(strings.TrimSpace(c))
		b.WriteString("\n")
	}
	b.WriteString("执行框架：遵循 ReAct（思考→行动→观察），出现阻塞时自动重排计划。")
	return b.String()
}

func splitSentences(s string) []string {
	replacer := strings.NewReplacer("；", "。", ";", ".", "!", ".", "！", ".", "?", ".", "？", ".", "\n", ".")
	s = replacer.Replace(s)
	chunks := strings.Split(s, ".")
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		c = strings.TrimSpace(c)
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}
