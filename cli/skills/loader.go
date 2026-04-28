package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type SkillLevel string

const (
	SkillLevelSystem  SkillLevel = "system"
	SkillLevelUser    SkillLevel = "user"
	SkillLevelProject SkillLevel = "project"
)

type LoadedSkill struct {
	Name         string     `json:"name"`
	Version      string     `json:"version"`
	Description  string     `json:"description"`
	Level        SkillLevel `json:"level"`
	Path         string     `json:"path"`
	AllowedTools []string   `json:"allowed_tools,omitempty"`
	Author       string     `json:"author,omitempty"`
	Tags         []string   `json:"tags,omitempty"`
	TriggerHints []string   `json:"trigger_hints,omitempty"`

	document *SkillDocument
}

type Loader struct {
	mu         sync.RWMutex
	skillsDir  string
	projectDir string
	cache      map[string]*LoadedSkill
}

func NewLoader(skillsDir, projectDir string) *Loader {
	return &Loader{
		skillsDir:  skillsDir,
		projectDir: projectDir,
		cache:      make(map[string]*LoadedSkill),
	}
}

func (l *Loader) LoadAll() ([]LoadedSkill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cache = make(map[string]*LoadedSkill)

	systemSkills := l.loadFromDir(l.systemSkillsDir(), SkillLevelSystem)
	userSkills := l.loadFromDir(l.userSkillsDir(), SkillLevelUser)
	projectSkills := l.loadFromDir(l.projectSkillsDir(), SkillLevelProject)

	all := make([]LoadedSkill, 0, len(systemSkills)+len(userSkills)+len(projectSkills))
	all = append(all, projectSkills...)
	all = append(all, userSkills...)
	all = append(all, systemSkills...)

	for i := range all {
		l.cache[strings.ToLower(all[i].Name)] = &all[i]
	}

	return l.deduplicate(all), nil
}

func (l *Loader) Get(name string) (*LoadedSkill, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.cache[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, false
	}
	return s, true
}

func (l *Loader) GetFullPrompt(name string) (string, error) {
	s, ok := l.Get(name)
	if !ok {
		return "", fmt.Errorf("skill not found: %s", name)
	}
	if s.document == nil {
		doc, err := l.parseSkillSource(s.Path)
		if err != nil {
			return "", fmt.Errorf("parse skill %s: %w", name, err)
		}
		s.document = &doc
	}
	return s.document.FullPrompt(), nil
}

func (l *Loader) BuildSkillSummaries() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.cache) == 0 {
		return ""
	}

	skills := make([]*LoadedSkill, 0, len(l.cache))
	for _, s := range l.cache {
		skills = append(skills, s)
	}
	sort.Slice(skills, func(i, j int) bool {
		return strings.ToLower(skills[i].Name) < strings.ToLower(skills[j].Name)
	})

	var b strings.Builder
	b.WriteString("[可用技能摘要]\n")
	b.WriteString("以下是当前可用的技能列表（仅显示摘要，调用时加载完整指令）：\n\n")
	for _, s := range skills {
		b.WriteString(fmt.Sprintf("- %s (v%s): %s [%s]\n", s.Name, s.Version, s.Description, s.Level))
	}
	b.WriteString("\n当用户任务匹配某技能时，请优先使用对应技能执行。\n")
	return b.String()
}

func (l *Loader) systemSkillsDir() string {
	return filepath.Join(l.skillsDir, "builtin")
}

func (l *Loader) userSkillsDir() string {
	return filepath.Join(l.skillsDir, "installed")
}

func (l *Loader) projectSkillsDir() string {
	if l.projectDir == "" {
		return ""
	}
	return filepath.Join(l.projectDir, ".xclaw", "skills")
}

func (l *Loader) loadFromDir(dir string, level SkillLevel) []LoadedSkill {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return nil
	}

	var out []LoadedSkill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillPath := filepath.Join(dir, entry.Name())
		doc, sourcePath, err := ParseSkillDir(skillPath)
		if err != nil {
			continue
		}

		loaded := LoadedSkill{
			Name:         doc.Meta.Name,
			Version:      doc.Meta.Version,
			Description:  doc.Meta.Description,
			Level:        level,
			Path:         sourcePath,
			AllowedTools: doc.Meta.AllowedTools,
			Author:       doc.Meta.Author,
			Tags:         doc.Meta.Tags,
			TriggerHints: doc.Meta.TriggerHints,
			document:     &doc,
		}
		out = append(out, loaded)
	}
	return out
}

func (l *Loader) parseSkillSource(path string) (SkillDocument, error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		doc, _, parseErr := ParseSkillDir(path)
		return doc, parseErr
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillDocument{}, fmt.Errorf("read skill source: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" || ext == ".yaml" || ext == ".yml" {
		return ParseSkillManifest(filepath.Base(path), data, filepath.Dir(path))
	}
	return ParseSKILLMd(string(data))
}

func (l *Loader) deduplicate(skills []LoadedSkill) []LoadedSkill {
	seen := make(map[string]struct{})
	out := make([]LoadedSkill, 0, len(skills))
	for _, s := range skills {
		key := strings.ToLower(s.Name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

func EnsureBuiltinSkills(skillsDir string) error {
	builtinDir := filepath.Join(skillsDir, "builtin")
	skills := map[string]string{
		"code-review/SKILL.md": `---
name: code-review
description: 规范化代码审查能力，自动检测代码质量问题并给出改进建议
version: 1.0.0
allowed-tools:
  - read_file
  - search_text
  - list_dir
trigger-hints:
  - 代码审查
  - code review
  - 代码质量
---

## 指令

请对目标代码进行规范化审查，输出问题列表和改进建议。

### 审查维度
1. 代码规范：命名、格式、注释
2. 逻辑正确性：边界条件、异常处理
3. 安全风险：注入、越权、信息泄露
4. 性能问题：不必要的循环、内存泄漏
5. 可维护性：耦合度、重复代码

### 输出格式
- 严重程度：🔴 严重 / 🟡 警告 / 🔵 建议
- 位置：文件路径 + 行号
- 描述：问题说明
- 建议：修复方案
`,
		"ops-monitor/SKILL.md": `---
name: ops-monitor
description: 运维巡检与告警能力，监控系统健康状态并生成巡检报告
version: 1.0.0
allowed-tools:
  - exec_cmd
  - read_file
  - search_text
trigger-hints:
  - 运维巡检
  - 系统监控
  - 健康检查
---

## 指令

对目标系统执行运维巡检，生成健康报告。

### 巡检项目
1. 系统资源：CPU、内存、磁盘使用率
2. 服务状态：关键进程运行状态
3. 日志分析：错误日志统计与趋势
4. 网络连通性：端口与服务可达性
5. 安全基线：权限、配置合规检查

### 输出格式
- 状态：✅ 正常 / ⚠️ 警告 / ❌ 异常
- 详情：检查项 + 当前值 + 阈值
- 建议：优化或修复方案
`,
		"reporting/SKILL.md": `---
name: reporting
description: 自动报告生成能力，将数据和分析结果转化为结构化报告
version: 1.0.0
allowed-tools:
  - read_file
  - write_file
  - search_text
trigger-hints:
  - 生成报告
  - 数据报告
  - 分析报告
---

## 指令

根据提供的数据和分析结果，生成结构化报告。

### 报告结构
1. 摘要：核心发现与结论
2. 背景：分析目标与范围
3. 方法：数据来源与分析方法
4. 结果：关键数据与可视化描述
5. 建议：基于分析的改进建议
6. 附录：详细数据表

### 输出格式
- Markdown 格式
- 包含数据表格
- 关键指标高亮
`,
		"filesystem-mcp/SKILL.md": `---
name: filesystem-mcp
description: 内置 Filesystem MCP Skill，提供本地文件浏览与读写能力示例
version: 1.0.0
allowed-tools:
  - mcp:skill-filesystem-mcp-filesystem:*
trigger-hints:
  - 文件系统MCP
  - filesystem mcp
  - 浏览目录
---

## 指令

当用户需要浏览、读取或修改工作目录文件时，可优先启用对应 MCP Server。
`,
		"filesystem-mcp/tools/filesystem.json": `{
  "name": "Filesystem MCP",
  "transport": "stdio",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-filesystem", "${WORKSPACE_DIR:-/tmp}"],
  "enabled": false
}`,
		"fetch-mcp/SKILL.md": `---
name: fetch-mcp
description: 内置 Fetch MCP Skill，提供网页抓取能力示例
version: 1.0.0
allowed-tools:
  - mcp:skill-fetch-mcp-fetch:*
trigger-hints:
  - 抓网页
  - fetch mcp
  - 读取网页
---

## 指令

当用户需要抓取网页正文、读取链接内容时，可启用对应 MCP Server。
`,
		"fetch-mcp/tools/fetch.json": `{
  "name": "Fetch MCP",
  "transport": "stdio",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-fetch"],
  "enabled": false
}`,
		"github-mcp/SKILL.md": `---
name: github-mcp
description: 内置 GitHub MCP Skill，提供仓库、Issue、PR 读写能力示例
version: 1.0.0
allowed-tools:
  - mcp:skill-github-mcp-github:*
trigger-hints:
  - GitHub MCP
  - 仓库 issue
  - pull request
---

## 指令

当用户需要访问 GitHub 仓库、Issue、PR 时，可启用对应 MCP Server。需要提前配置 GITHUB_TOKEN。
`,
		"github-mcp/tools/github.json": `{
  "name": "GitHub MCP",
  "transport": "stdio",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-github"],
  "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"},
  "enabled": false
}`,
		"a2a-bridge/SKILL.md": `---
name: a2a-bridge
description: 跨实例协作桥接能力，支持多 Agent 实例之间的任务分发与结果汇总
version: 1.0.0
allowed-tools:
  - exec_cmd
  - read_file
  - write_file
trigger-hints:
  - 跨实例协作
  - 多Agent
  - 任务分发
---

## 指令

协调多个 Agent 实例之间的协作任务。

### 协作流程
1. 任务拆解：将复杂任务分解为可并行的子任务
2. 实例发现：查找可用的 Agent 实例
3. 任务分发：将子任务分配给合适的实例
4. 结果汇总：收集并整合各实例的执行结果
5. 冲突处理：解决结果间的矛盾与重复

### 输出格式
- 任务分配表
- 执行进度追踪
- 结果汇总报告
`,
	}

	for relPath, content := range skills {
		fullPath := filepath.Join(builtinDir, relPath)
		if _, err := os.Stat(fullPath); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fmt.Errorf("create builtin skill dir: %w", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write builtin skill %s: %w", relPath, err)
		}
	}
	return nil
}
