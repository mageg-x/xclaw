package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"xclaw/cli/models"
)

type Manager struct {
	root string
}

func NewManager(root string) *Manager {
	return &Manager{root: root}
}

func (m *Manager) Root() string {
	return m.root
}

func (m *Manager) EnsureAgentWorkspace(agent models.Agent) (string, error) {
	safe := sanitize(agent.Name)
	dir := filepath.Join(m.root, fmt.Sprintf("%s-%s", safe, agent.ID))
	memoryDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}

	files := map[string]string{
		"AGENTS.md":   defaultAgents(agent),
		"SOUL.md":     defaultSoul(agent),
		"IDENTITY.md": defaultIdentity(agent),
		"USER.md":     defaultUser(agent),
		"TOOLS.md":    defaultTools(agent),
		"MEMORY.md":   "# 长期记忆\n\n",
	}

	for name, content := range files {
		fp := filepath.Join(dir, name)
		if err := writeIfNotExists(fp, content); err != nil {
			return "", err
		}
	}

	todayFile := filepath.Join(memoryDir, dateFile(time.Now()))
	if err := writeIfNotExists(todayFile, "# 每日记忆\n\n"); err != nil {
		return "", err
	}

	return dir, nil
}

func (m *Manager) SyncProfile(agent models.Agent) error {
	dir := agent.WorkspacePath
	if dir == "" {
		return fmt.Errorf("workspace path is empty")
	}
	files := map[string]string{
		"AGENTS.md":   defaultAgents(agent),
		"SOUL.md":     defaultSoul(agent),
		"IDENTITY.md": defaultIdentity(agent),
		"TOOLS.md":    defaultTools(agent),
	}
	for name, content := range files {
		fp := filepath.Join(dir, name)
		if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
			return fmt.Errorf("sync %s: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) LoadRecentContext(workspacePath string) (string, error) {
	memoryRoot := filepath.Join(workspacePath, "memory")
	today := filepath.Join(memoryRoot, dateFile(time.Now()))
	yesterday := filepath.Join(memoryRoot, dateFile(time.Now().Add(-24*time.Hour)))

	files := []string{
		filepath.Join(workspacePath, "AGENTS.md"),
		filepath.Join(workspacePath, "SOUL.md"),
		filepath.Join(workspacePath, "IDENTITY.md"),
		filepath.Join(workspacePath, "USER.md"),
		filepath.Join(workspacePath, "TOOLS.md"),
		filepath.Join(workspacePath, "MEMORY.md"),
		yesterday,
		today,
	}

	var b strings.Builder
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("read context file %s: %w", f, err)
		}
		b.WriteString("\n\n---\n")
		b.WriteString(filepath.Base(f))
		b.WriteString("\n")
		b.Write(content)
	}
	return strings.TrimSpace(b.String()), nil
}

func (m *Manager) AppendDailyMemory(workspacePath, title, detail string) error {
	if strings.TrimSpace(detail) == "" {
		return nil
	}
	memoryRoot := filepath.Join(workspacePath, "memory")
	if err := os.MkdirAll(memoryRoot, 0o755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	dayFile := filepath.Join(memoryRoot, dateFile(time.Now()))

	entry := fmt.Sprintf("## %s\n- 标题: %s\n- 记录: %s\n\n", time.Now().Format("15:04:05"), title, strings.TrimSpace(detail))
	if err := appendFile(dayFile, entry); err != nil {
		return err
	}

	summary := fmt.Sprintf("- %s %s\n", time.Now().Format("2006-01-02 15:04"), title)
	if err := appendFile(filepath.Join(workspacePath, "MEMORY.md"), summary); err != nil {
		return err
	}
	return nil
}

// WriteLongTermMemory writes consolidated long-term memory to MEMORY.md
func (m *Manager) WriteLongTermMemory(workspacePath, category, content string) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	memoryFile := filepath.Join(workspacePath, "MEMORY.md")

	entry := fmt.Sprintf("\n## %s [%s]\n%s\n", category, time.Now().Format("2006-01-02"), strings.TrimSpace(content))
	if err := appendFile(memoryFile, entry); err != nil {
		return fmt.Errorf("write long-term memory: %w", err)
	}
	return nil
}

// AutoWriteMemory intelligently decides what to remember from a session
func (m *Manager) AutoWriteMemory(workspacePath, userQuestion, assistantAnswer string, toolResults []string) error {
	// Only remember if the session involved tools or had substantial content
	if len(toolResults) == 0 && len(assistantAnswer) < 100 {
		return nil
	}

	// Write to daily memory
	detail := fmt.Sprintf("Q: %s\nA: %s", userQuestion, assistantAnswer)
	if err := m.AppendDailyMemory(workspacePath, "会话记录", detail); err != nil {
		return err
	}

	// If there were tool executions, write to long-term memory as learned strategy
	if len(toolResults) > 0 {
		strategy := fmt.Sprintf("任务类型: %s\n有效策略: %s", userQuestion, strings.Join(toolResults, "; "))
		if err := m.WriteLongTermMemory(workspacePath, "策略沉淀", strategy); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) RecordReflection(workspacePath, userQuestion, assistantAnswer string, toolResults, failures []string) error {
	parts := []string{
		fmt.Sprintf("问题: %s", strings.TrimSpace(userQuestion)),
		fmt.Sprintf("结论: %s", trimText(assistantAnswer, 480)),
	}
	if len(toolResults) > 0 {
		parts = append(parts, "有效动作: "+strings.Join(toolResults, "; "))
	}
	if len(failures) > 0 {
		parts = append(parts, "失败点: "+strings.Join(failures, "; "))
	}
	summary := strings.Join(parts, "\n")
	if err := m.WriteLongTermMemory(workspacePath, "反思总结", summary); err != nil {
		return err
	}
	if err := m.AppendDailyMemory(workspacePath, "任务反思", summary); err != nil {
		return err
	}
	return nil
}

func (m *Manager) LearnFromFailure(workspacePath string, failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	rules := make([]string, 0, len(failures))
	for _, failure := range failures {
		n := normalizeLine(failure)
		if n == "" {
			continue
		}
		rules = append(rules, "失败预防: "+n)
	}
	if len(rules) == 0 {
		return nil
	}
	if err := appendUniqueSectionLines(filepath.Join(workspacePath, "AGENTS.md"), "## 失败经验规则", rules); err != nil {
		return err
	}
	return m.WriteLongTermMemory(workspacePath, "失败学习", strings.Join(rules, "\n"))
}

func (m *Manager) LearnUserPreference(workspacePath, userQuestion, assistantAnswer string) error {
	prefs := detectPreferences(userQuestion, assistantAnswer)
	if len(prefs) == 0 {
		return nil
	}
	return appendUniqueSectionLines(filepath.Join(workspacePath, "USER.md"), "## 自动学习偏好", prefs)
}

func writeIfNotExists(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	return nil
}

func appendFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open file %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("append file %s: %w", path, err)
	}
	return nil
}

func dateFile(now time.Time) string {
	return now.Format("2006-01-02") + ".md"
}

func sanitize(name string) string {
	lowered := strings.ToLower(strings.TrimSpace(name))
	if lowered == "" {
		lowered = "agent"
	}
	re := regexp.MustCompile(`[^a-z0-9]+`)
	lowered = re.ReplaceAllString(lowered, "-")
	lowered = strings.Trim(lowered, "-")
	if lowered == "" {
		return "agent"
	}
	return lowered
}

func defaultAgents(agent models.Agent) string {
	return fmt.Sprintf(`# AGENTS

## 角色目标
%s

## 执行准则
- 先拆解任务，再执行。
- 高风险操作先说明影响，再请求审批。
- 输出结论前给出可验证证据。

## 工程约束
- 变更前先读相关文件。
- 提交前运行验证命令。
- 所有变更记录到审计日志。
`, strings.TrimSpace(agent.Description))
}

func defaultSoul(agent models.Agent) string {
	return fmt.Sprintf(`# SOUL

- 名称：%s
- 语气：专业、直接、可执行
- 边界：不伪造结果，不隐瞒失败，必要时降级处理
`, agent.Name)
}

func defaultIdentity(agent models.Agent) string {
	return fmt.Sprintf(`# IDENTITY

- 名称：%s
- 图标：%s
- 使命：在边界内稳定完成任务
`, agent.Name, agent.Emoji)
}

func defaultUser(agent models.Agent) string {
	return fmt.Sprintf(`# USER

- 默认服务对象：%s 的协作者
- 偏好：结论先行，步骤简洁，遇风险明确说明
`, agent.Name)
}

func defaultTools(agent models.Agent) string {
	list := "- " + strings.Join(agent.Tools, "\n- ")
	if strings.TrimSpace(list) == "- " {
		list = "- list_dir\n- read_file"
	}
	return fmt.Sprintf(`# TOOLS

## 允许工具
%s

## 调用原则
- 先读后写。
- 最小权限。
- 每次调用必须产生日志。
`, list)
}

func detectPreferences(userQuestion, assistantAnswer string) []string {
	text := strings.ToLower(userQuestion + "\n" + assistantAnswer)
	out := make([]string, 0, 4)
	if strings.Contains(text, "中文") {
		out = append(out, "偏好中文交流与注释")
	}
	if strings.Contains(text, "简洁") || strings.Contains(text, "精简") {
		out = append(out, "偏好结论先行与简洁输出")
	}
	if strings.Contains(text, "详细") || strings.Contains(text, "展开") {
		out = append(out, "偏好分步骤详细说明")
	}
	if strings.Contains(text, "测试") || strings.Contains(text, "验收") {
		out = append(out, "重视可验证结果与测试覆盖")
	}
	sort.Strings(out)
	return uniqueLines(out)
}

func appendUniqueSectionLines(path, section string, lines []string) error {
	lines = uniqueLines(lines)
	if len(lines) == 0 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read file %s: %w", path, err)
	}
	content := string(raw)
	if strings.TrimSpace(content) == "" {
		content = "# 文件\n\n"
	}
	for _, line := range lines {
		entry := "- " + normalizeLine(line)
		if entry == "- " {
			continue
		}
		if strings.Contains(content, entry) {
			continue
		}
		content = ensureSection(content, section)
		content = strings.TrimRight(content, "\n") + "\n" + entry + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	return nil
}

func ensureSection(content, section string) string {
	if strings.Contains(content, section) {
		return content
	}
	content = strings.TrimRight(content, "\n")
	if content != "" {
		content += "\n\n"
	}
	content += section + "\n"
	return content
}

func uniqueLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	seen := map[string]struct{}{}
	for _, line := range lines {
		line = normalizeLine(line)
		if line == "" {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	return out
}

func normalizeLine(line string) string {
	return strings.TrimSpace(strings.ReplaceAll(line, "\n", " "))
}

func trimText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + " ..."
}
