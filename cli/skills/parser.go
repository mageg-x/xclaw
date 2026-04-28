package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type SkillMeta struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	Author       string   `json:"author,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	TriggerHints []string `json:"trigger_hints,omitempty"`
}

type SkillDocument struct {
	Meta         SkillMeta `json:"meta"`
	Instructions string    `json:"instructions"`
}

type SkillPackageManifest struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Version          string   `json:"version"`
	Description      string   `json:"description"`
	Author           string   `json:"author,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	AllowedTools     []string `json:"allowed_tools,omitempty"`
	TriggerHints     []string `json:"trigger_hints,omitempty"`
	InstructionsFile string   `json:"instructions_file,omitempty"`
	Files            []string `json:"files,omitempty"`
	Dependencies     []string `json:"dependencies,omitempty"`
}

var yamlFrontMatterRe = regexp.MustCompile(`(?s)^---\s*\n(.*?)\n---\s*\n?(.*)$`)

func ParseSKILLMd(content string) (SkillDocument, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return SkillDocument{}, fmt.Errorf("empty SKILL.md content")
	}

	matches := yamlFrontMatterRe.FindStringSubmatch(content)
	if len(matches) < 3 {
		return SkillDocument{}, fmt.Errorf("SKILL.md must start with YAML front matter (---)")
	}

	yamlBlock := strings.TrimSpace(matches[1])
	markdownBody := strings.TrimSpace(matches[2])

	meta := parseYAMLMeta(yamlBlock)
	if strings.TrimSpace(meta.Name) == "" {
		return SkillDocument{}, fmt.Errorf("SKILL.md front matter must contain 'name' field")
	}
	if strings.TrimSpace(meta.Version) == "" {
		meta.Version = "1.0.0"
	}

	return SkillDocument{
		Meta:         meta,
		Instructions: markdownBody,
	}, nil
}

func ParseSkillDir(dir string) (SkillDocument, string, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" {
		return SkillDocument{}, "", fmt.Errorf("skill dir is empty")
	}

	skillMD := filepath.Join(dir, "SKILL.md")
	if data, err := os.ReadFile(skillMD); err == nil {
		doc, parseErr := ParseSKILLMd(string(data))
		if parseErr != nil {
			return SkillDocument{}, "", parseErr
		}
		return doc, skillMD, nil
	}

	for _, name := range []string{"skill.json", "skill.yaml", "skill.yml"} {
		manifestPath := filepath.Join(dir, name)
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		doc, parseErr := ParseSkillManifest(name, data, dir)
		if parseErr != nil {
			return SkillDocument{}, "", parseErr
		}
		return doc, manifestPath, nil
	}

	return SkillDocument{}, "", fmt.Errorf("no supported skill descriptor found in %s", dir)
}

func ParseSkillManifest(name string, data []byte, dir string) (SkillDocument, error) {
	manifest := SkillPackageManifest{}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json":
		if err := json.Unmarshal(data, &manifest); err != nil {
			return SkillDocument{}, fmt.Errorf("decode skill manifest json: %w", err)
		}
	default:
		manifest = parseYAMLManifest(string(data))
	}
	if strings.TrimSpace(manifest.Name) == "" {
		manifest.Name = strings.TrimSpace(manifest.ID)
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return SkillDocument{}, fmt.Errorf("skill manifest must contain name or id")
	}
	if strings.TrimSpace(manifest.Version) == "" {
		manifest.Version = "1.0.0"
	}

	instructionsFile := firstNonEmpty(strings.TrimSpace(manifest.InstructionsFile), "instructions.md", "README.md")
	instructionsPath := filepath.Join(dir, instructionsFile)
	body, err := os.ReadFile(instructionsPath)
	if err != nil {
		if errorsText(err, "no such file") {
			body = []byte("")
		} else {
			return SkillDocument{}, fmt.Errorf("read skill instructions: %w", err)
		}
	}

	return SkillDocument{
		Meta: SkillMeta{
			Name:         manifest.Name,
			Version:      manifest.Version,
			Description:  manifest.Description,
			AllowedTools: manifest.AllowedTools,
			Author:       manifest.Author,
			Tags:         manifest.Tags,
			TriggerHints: manifest.TriggerHints,
		},
		Instructions: strings.TrimSpace(string(body)),
	}, nil
}

func parseYAMLMeta(yaml string) SkillMeta {
	values := parseYAMLObject(yaml)
	meta := SkillMeta{}
	meta.Name = yamlString(values, "name")
	meta.Version = yamlString(values, "version")
	meta.Description = yamlString(values, "description")
	meta.Author = yamlString(values, "author")
	meta.AllowedTools = yamlStringSlice(values, "allowed-tools", "allowed_tools")
	meta.Tags = yamlStringSlice(values, "tags")
	meta.TriggerHints = yamlStringSlice(values, "trigger-hints", "trigger_hints")
	return meta
}

func parseYAMLManifest(yaml string) SkillPackageManifest {
	values := parseYAMLObject(yaml)
	manifest := SkillPackageManifest{}
	manifest.ID = yamlString(values, "id")
	manifest.Name = yamlString(values, "name")
	manifest.Version = yamlString(values, "version")
	manifest.Description = yamlString(values, "description")
	manifest.Author = yamlString(values, "author")
	manifest.InstructionsFile = yamlString(values, "instructions-file", "instructions_file", "prompt-file", "prompt_file")
	manifest.AllowedTools = yamlStringSlice(values, "allowed-tools", "allowed_tools")
	manifest.Tags = yamlStringSlice(values, "tags")
	manifest.TriggerHints = yamlStringSlice(values, "trigger-hints", "trigger_hints")
	manifest.Files = yamlStringSlice(values, "files")
	manifest.Dependencies = yamlStringSlice(values, "dependencies")
	return manifest
}

func parseYAMLObject(raw string) map[string]any {
	lines := strings.Split(raw, "\n")
	out := make(map[string]any)
	for i := 0; i < len(lines); {
		i = nextYAMLLine(lines, i)
		if i >= len(lines) {
			break
		}
		line := lines[i]
		indent := leadingSpaces(line)
		if indent != 0 {
			i++
			continue
		}
		key, val, ok := parseYAMLKeyValue(strings.TrimSpace(line))
		if !ok {
			i++
			continue
		}
		switch strings.TrimSpace(val) {
		case "|", ">":
			block, next := collectYAMLBlock(lines, i+1, indent, strings.TrimSpace(val) == ">")
			out[strings.ToLower(key)] = block
			i = next
			continue
		case "":
			next := nextYAMLLine(lines, i+1)
			if next < len(lines) && leadingSpaces(lines[next]) > indent {
				trimmedNext := strings.TrimSpace(lines[next])
				if strings.HasPrefix(trimmedNext, "-") {
					list, after := collectYAMLList(lines, i+1, indent)
					out[strings.ToLower(key)] = list
					i = after
					continue
				}
				block, after := collectYAMLNestedText(lines, i+1, indent)
				out[strings.ToLower(key)] = block
				i = after
				continue
			}
			out[strings.ToLower(key)] = ""
			i++
			continue
		default:
			if strings.HasPrefix(strings.TrimSpace(val), "[") && strings.HasSuffix(strings.TrimSpace(val), "]") {
				out[strings.ToLower(key)] = parseInlineYAMLList(val)
			} else {
				out[strings.ToLower(key)] = unquote(val)
			}
			i++
		}
	}
	return out
}

func yamlString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		v, ok := values[strings.ToLower(strings.TrimSpace(key))]
		if !ok {
			continue
		}
		switch vv := v.(type) {
		case string:
			return strings.TrimSpace(vv)
		case []string:
			return strings.Join(vv, ", ")
		case []any:
			parts := make([]string, 0, len(vv))
			for _, item := range vv {
				if text := strings.TrimSpace(fmt.Sprintf("%v", item)); text != "" {
					parts = append(parts, text)
				}
			}
			return strings.Join(parts, ", ")
		}
	}
	return ""
}

func yamlStringSlice(values map[string]any, keys ...string) []string {
	for _, key := range keys {
		v, ok := values[strings.ToLower(strings.TrimSpace(key))]
		if !ok {
			continue
		}
		switch vv := v.(type) {
		case []string:
			return append([]string(nil), vv...)
		case []any:
			out := make([]string, 0, len(vv))
			for _, item := range vv {
				text := strings.TrimSpace(fmt.Sprintf("%v", item))
				if text != "" {
					out = append(out, text)
				}
			}
			return out
		case string:
			if strings.TrimSpace(vv) == "" {
				return nil
			}
			return []string{strings.TrimSpace(vv)}
		}
	}
	return nil
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func errorsText(err error, fragment string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), strings.ToLower(fragment))
}

func parseInlineYAMLList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = strings.TrimPrefix(strings.TrimSuffix(raw, "]"), "[")
	}
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = unquote(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseYAMLKeyValue(line string) (string, string, bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}

func nextYAMLLine(lines []string, start int) int {
	for i := start; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return i
	}
	return len(lines)
}

func leadingSpaces(line string) int {
	n := 0
	for _, ch := range line {
		if ch != ' ' {
			break
		}
		n++
	}
	return n
}

func collectYAMLList(lines []string, start, parentIndent int) ([]string, int) {
	out := make([]string, 0)
	i := start
	for {
		i = nextYAMLLine(lines, i)
		if i >= len(lines) {
			return out, i
		}
		line := lines[i]
		indent := leadingSpaces(line)
		trimmed := strings.TrimSpace(line)
		if indent <= parentIndent || !strings.HasPrefix(trimmed, "-") {
			return out, i
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		if value == "|" || value == ">" {
			block, next := collectYAMLBlock(lines, i+1, indent, value == ">")
			if strings.TrimSpace(block) != "" {
				out = append(out, block)
			}
			i = next
			continue
		}
		value = unquote(value)
		if value != "" {
			out = append(out, value)
		}
		i++
	}
}

func collectYAMLBlock(lines []string, start, parentIndent int, folded bool) (string, int) {
	rawParts := make([]string, 0)
	minIndent := -1
	i := start
	for {
		if i >= len(lines) {
			break
		}
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		indent := leadingSpaces(line)
		if trimmed != "" && indent <= parentIndent {
			break
		}
		if trimmed == "" {
			rawParts = append(rawParts, "")
			i++
			continue
		}
		if minIndent < 0 || indent < minIndent {
			minIndent = indent
		}
		rawParts = append(rawParts, strings.TrimRight(line, " "))
		i++
	}
	parts := make([]string, 0, len(rawParts))
	for _, part := range rawParts {
		if strings.TrimSpace(part) == "" {
			parts = append(parts, "")
			continue
		}
		if minIndent > 0 && len(part) >= minIndent {
			part = part[minIndent:]
		}
		parts = append(parts, part)
	}
	if folded {
		joined := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				joined = append(joined, part)
			}
		}
		return strings.Join(joined, " "), i
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), i
}

func collectYAMLNestedText(lines []string, start, parentIndent int) (string, int) {
	parts := make([]string, 0)
	i := start
	for {
		i = nextYAMLLine(lines, i)
		if i >= len(lines) {
			return strings.TrimSpace(strings.Join(parts, "\n")), i
		}
		line := lines[i]
		indent := leadingSpaces(line)
		if indent <= parentIndent {
			return strings.TrimSpace(strings.Join(parts, "\n")), i
		}
		parts = append(parts, strings.TrimSpace(line))
		i++
	}
}

func (d SkillDocument) Summary() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("技能: %s", d.Meta.Name))
	if d.Meta.Version != "" {
		b.WriteString(fmt.Sprintf(" (v%s)", d.Meta.Version))
	}
	if d.Meta.Description != "" {
		b.WriteString(fmt.Sprintf(" - %s", d.Meta.Description))
	}
	if len(d.Meta.AllowedTools) > 0 {
		b.WriteString(fmt.Sprintf(" [需要工具: %s]", strings.Join(d.Meta.AllowedTools, ", ")))
	}
	if len(d.Meta.TriggerHints) > 0 {
		b.WriteString(fmt.Sprintf(" [触发词: %s]", strings.Join(d.Meta.TriggerHints, ", ")))
	}
	return b.String()
}

func (d SkillDocument) FullPrompt() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# 技能: %s\n\n", d.Meta.Name))
	if d.Meta.Description != "" {
		b.WriteString(fmt.Sprintf("描述: %s\n\n", d.Meta.Description))
	}
	if len(d.Meta.AllowedTools) > 0 {
		b.WriteString(fmt.Sprintf("允许工具: %s\n\n", strings.Join(d.Meta.AllowedTools, ", ")))
	}
	if d.Instructions != "" {
		b.WriteString("## 指令\n\n")
		b.WriteString(d.Instructions)
		b.WriteString("\n")
	}
	return b.String()
}
