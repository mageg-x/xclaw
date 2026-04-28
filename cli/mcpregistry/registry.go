package mcpregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"xclaw/cli/db"
	"xclaw/cli/mcpclient"
)

const ManualSettingKey = "mcp_servers_manual_json"

func LoadManual(ctx context.Context, store *db.Store) ([]mcpclient.ServerConfig, error) {
	if store == nil {
		return []mcpclient.ServerConfig{}, nil
	}
	if raw, ok, err := store.GetSetting(ctx, ManualSettingKey); err == nil && ok && strings.TrimSpace(raw) != "" {
		return mcpclient.DecodeServers(raw)
	}
	raw, ok, err := store.GetSetting(ctx, mcpclient.SettingsKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return []mcpclient.ServerConfig{}, err
	}
	items, err := mcpclient.DecodeServers(raw)
	if err != nil {
		return nil, err
	}
	return FilterManual(items), nil
}

func SaveManualAndSync(ctx context.Context, store *db.Store, skillsDir string, manual []mcpclient.ServerConfig) ([]mcpclient.ServerConfig, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	manual = FilterManual(manual)
	manualRaw, err := mcpclient.EncodeServers(manual)
	if err != nil {
		return nil, err
	}
	if err := store.SetSetting(ctx, ManualSettingKey, manualRaw); err != nil {
		return nil, err
	}
	merged, err := MergeWithSkillServers(skillsDir, manual)
	if err != nil {
		return nil, err
	}
	mergedRaw, err := mcpclient.EncodeServers(merged)
	if err != nil {
		return nil, err
	}
	if err := store.SetSetting(ctx, mcpclient.SettingsKey, mergedRaw); err != nil {
		return nil, err
	}
	return merged, nil
}

func MergeWithSkillServers(skillsDir string, manual []mcpclient.ServerConfig) ([]mcpclient.ServerConfig, error) {
	auto, err := DiscoverSkillServers(skillsDir)
	if err != nil {
		return nil, err
	}
	merged := make([]mcpclient.ServerConfig, 0, len(manual)+len(auto))
	seen := make(map[string]struct{}, len(manual)+len(auto))
	for _, item := range manual {
		item.Readonly = false
		item.ManagedBy = ""
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		merged = append(merged, item)
	}
	for _, item := range auto {
		baseID := strings.TrimSpace(item.ID)
		if baseID == "" {
			continue
		}
		if _, ok := seen[baseID]; !ok {
			seen[baseID] = struct{}{}
			merged = append(merged, item)
			continue
		}
		for i := 2; ; i++ {
			candidate := fmt.Sprintf("%s-%d", baseID, i)
			if _, ok := seen[candidate]; ok {
				continue
			}
			item.ID = candidate
			seen[candidate] = struct{}{}
			merged = append(merged, item)
			break
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		left := strings.ToLower(strings.TrimSpace(merged[i].ID))
		right := strings.ToLower(strings.TrimSpace(merged[j].ID))
		return left < right
	})
	return merged, nil
}

func FilterManual(items []mcpclient.ServerConfig) []mcpclient.ServerConfig {
	out := make([]mcpclient.ServerConfig, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item.Readonly || strings.TrimSpace(item.ManagedBy) != "" {
			continue
		}
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		out = append(out, item)
	}
	return out
}

func FilterManagedBy(items []mcpclient.ServerConfig, managedBy string) []mcpclient.ServerConfig {
	managedBy = strings.TrimSpace(managedBy)
	if managedBy == "" {
		return nil
	}
	out := make([]mcpclient.ServerConfig, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ManagedBy) == managedBy {
			out = append(out, item)
		}
	}
	return out
}

func DiscoverSkillServers(skillsDir string) ([]mcpclient.ServerConfig, error) {
	roots := []string{
		filepath.Join(strings.TrimSpace(skillsDir), "builtin"),
		filepath.Join(strings.TrimSpace(skillsDir), "installed"),
	}
	var out []mcpclient.ServerConfig
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read skills dir %s: %w", root, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skillName := entry.Name()
			items, err := loadSkillToolServers(filepath.Join(root, skillName), skillName)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
		}
	}
	return out, nil
}

func loadSkillToolServers(skillDir, skillName string) ([]mcpclient.ServerConfig, error) {
	toolsDir := filepath.Join(skillDir, "tools")
	entries, err := os.ReadDir(toolsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skill tools dir %s: %w", skillName, err)
	}
	var out []mcpclient.ServerConfig
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		lower := strings.ToLower(entry.Name())
		if !strings.HasSuffix(lower, ".json") && !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
			continue
		}
		filePath := filepath.Join(toolsDir, entry.Name())
		body, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read skill tool file %s: %w", filePath, err)
		}
		servers, err := decodeToolServers(body)
		if err != nil {
			return nil, fmt.Errorf("decode skill tool file %s: %w", filePath, err)
		}
		base := slugify(skillName) + "-" + slugify(strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
		for i := range servers {
			normalized, err := validateSkillServerConfig(servers[i])
			if err != nil {
				return nil, fmt.Errorf("invalid skill mcp server in %s: %w", filePath, err)
			}
			normalized = normalizeSkillServerConfig(normalized, skillDir, skillName)
			servers[i] = normalized
			if strings.TrimSpace(servers[i].ID) == "" {
				servers[i].ID = "skill-" + strings.Trim(strings.Join([]string{base, slugify(servers[i].Name)}, "-"), "-")
				servers[i].ID = strings.TrimSuffix(servers[i].ID, "-")
			}
			servers[i].ManagedBy = "skill:" + skillName
			servers[i].Readonly = true
		}
		out = append(out, servers...)
	}
	return out, nil
}

func decodeToolServers(body []byte) ([]mcpclient.ServerConfig, error) {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, nil
	}
	if body[0] != '{' && body[0] != '[' {
		return decodeToolServersYAML(string(body))
	}
	if body[0] == '[' {
		var items []mcpclient.ServerConfig
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, err
		}
		return items, nil
	}
	var wrapper struct {
		Servers    []mcpclient.ServerConfig `json:"servers"`
		MCPServers []mcpclient.ServerConfig `json:"mcp_servers"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil {
		switch {
		case len(wrapper.MCPServers) > 0:
			return wrapper.MCPServers, nil
		case len(wrapper.Servers) > 0:
			return wrapper.Servers, nil
		}
	}
	var single mcpclient.ServerConfig
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, err
	}
	return []mcpclient.ServerConfig{single}, nil
}

func decodeToolServersYAML(raw string) ([]mcpclient.ServerConfig, error) {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	start := nextYAMLLine(lines, 0)
	if start >= len(lines) {
		return nil, nil
	}
	if strings.HasPrefix(strings.TrimSpace(lines[start]), "-") {
		items, _, err := parseYAMLServerList(lines, start, -1)
		return items, err
	}
	root := make(map[string]any)
	for i := 0; i < len(lines); {
		i = nextYAMLLine(lines, i)
		if i >= len(lines) {
			break
		}
		line := lines[i]
		indent := leadingSpaces(line)
		if indent > 0 {
			i++
			continue
		}
		key, val, ok := parseYAMLKeyValue(strings.TrimSpace(line))
		if !ok {
			i++
			continue
		}
		switch strings.ToLower(key) {
		case "mcp_servers", "servers":
			items, next, err := parseYAMLServerList(lines, i+1, indent)
			if err != nil {
				return nil, err
			}
			if len(items) > 0 {
				return items, nil
			}
			i = next
			continue
		default:
			assignYAMLScalar(root, key, val)
		}
		i++
	}
	item := buildServerConfig(root)
	if strings.TrimSpace(item.Name) == "" && strings.TrimSpace(item.ID) == "" && strings.TrimSpace(item.Command) == "" && strings.TrimSpace(item.URL) == "" {
		return nil, fmt.Errorf("unsupported yaml tool config")
	}
	return []mcpclient.ServerConfig{item}, nil
}

func parseYAMLServerList(lines []string, start, parentIndent int) ([]mcpclient.ServerConfig, int, error) {
	items := make([]mcpclient.ServerConfig, 0)
	i := start
	for {
		i = nextYAMLLine(lines, i)
		if i >= len(lines) {
			return items, i, nil
		}
		line := lines[i]
		indent := leadingSpaces(line)
		trimmed := strings.TrimSpace(line)
		if indent <= parentIndent {
			return items, i, nil
		}
		if !strings.HasPrefix(trimmed, "-") {
			if len(items) == 0 {
				return items, i, fmt.Errorf("expected yaml list item")
			}
			return items, i, nil
		}
		entry := make(map[string]any)
		inline := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		if inline != "" {
			key, val, ok := parseYAMLKeyValue(inline)
			if ok {
				assignYAMLScalar(entry, key, val)
			}
		}
		i++
		for {
			next := nextYAMLLine(lines, i)
			if next >= len(lines) {
				i = next
				break
			}
			subLine := lines[next]
			subIndent := leadingSpaces(subLine)
			if subIndent <= indent {
				i = next
				break
			}
			key, val, ok := parseYAMLKeyValue(strings.TrimSpace(subLine))
			if !ok {
				i = next + 1
				continue
			}
			fieldIndent := subIndent
			switch strings.ToLower(key) {
			case "args":
				if strings.TrimSpace(val) != "" {
					entry[key] = parseInlineYAMLList(val)
					i = next + 1
					continue
				}
				list, after := collectYAMLList(lines, next+1, fieldIndent)
				entry[key] = list
				i = after
			case "env":
				if strings.TrimSpace(val) != "" {
					entry[key] = parseInlineYAMLMap(val)
					i = next + 1
					continue
				}
				env, after := collectYAMLMap(lines, next+1, fieldIndent)
				entry[key] = env
				i = after
			default:
				assignYAMLScalar(entry, key, val)
				i = next + 1
			}
		}
		items = append(items, buildServerConfig(entry))
	}
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
		out = append(out, unquoteYAML(strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))))
		i++
	}
}

func collectYAMLMap(lines []string, start, parentIndent int) (map[string]string, int) {
	out := make(map[string]string)
	i := start
	for {
		i = nextYAMLLine(lines, i)
		if i >= len(lines) {
			return out, i
		}
		line := lines[i]
		indent := leadingSpaces(line)
		if indent <= parentIndent {
			return out, i
		}
		key, val, ok := parseYAMLKeyValue(strings.TrimSpace(line))
		if ok {
			out[key] = unquoteYAML(val)
		}
		i++
	}
}

func buildServerConfig(values map[string]any) mcpclient.ServerConfig {
	item := mcpclient.ServerConfig{
		Enabled:    true,
		TimeoutSec: 20,
	}
	if v, ok := values["id"].(string); ok {
		item.ID = strings.TrimSpace(v)
	}
	if v, ok := values["name"].(string); ok {
		item.Name = strings.TrimSpace(v)
	}
	if v, ok := values["transport"].(string); ok {
		item.Transport = strings.TrimSpace(v)
	}
	if v, ok := values["url"].(string); ok {
		item.URL = strings.TrimSpace(v)
	}
	if v, ok := values["command"].(string); ok {
		item.Command = strings.TrimSpace(v)
	}
	if v, ok := values["args"].([]string); ok {
		item.Args = append([]string(nil), v...)
	}
	if v, ok := values["env"].(map[string]string); ok {
		item.Env = v
	}
	if v, ok := values["enabled"].(bool); ok {
		item.Enabled = v
	}
	if v, ok := values["timeout_sec"].(int); ok {
		item.TimeoutSec = v
	}
	if v, ok := values["timeout"].(int); ok && item.TimeoutSec == 0 {
		item.TimeoutSec = v
	}
	return item
}

func assignYAMLScalar(values map[string]any, key, raw string) {
	key = strings.TrimSpace(key)
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(key) {
	case "args":
		values[key] = parseInlineYAMLList(raw)
	case "enabled", "readonly":
		values[key] = strings.EqualFold(raw, "true")
	case "timeout", "timeout_sec":
		var n int
		_, _ = fmt.Sscanf(raw, "%d", &n)
		values[key] = n
	default:
		values[key] = unquoteYAML(raw)
	}
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
		part = unquoteYAML(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseInlineYAMLMap(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = strings.TrimPrefix(strings.TrimSuffix(raw, "}"), "{")
	out := make(map[string]string)
	for _, part := range strings.Split(raw, ",") {
		key, val, ok := parseYAMLKeyValue(strings.TrimSpace(part))
		if ok && strings.TrimSpace(key) != "" {
			out[key] = unquoteYAML(val)
		}
	}
	if len(out) == 0 {
		return nil
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

func unquoteYAML(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 {
		if (raw[0] == '"' && raw[len(raw)-1] == '"') || (raw[0] == '\'' && raw[len(raw)-1] == '\'') {
			return raw[1 : len(raw)-1]
		}
	}
	return raw
}

func validateSkillServerConfig(item mcpclient.ServerConfig) (mcpclient.ServerConfig, error) {
	item.ID = strings.TrimSpace(item.ID)
	item.Name = strings.TrimSpace(item.Name)
	item.Transport = strings.ToLower(strings.TrimSpace(item.Transport))
	item.URL = strings.TrimSpace(item.URL)
	item.Command = strings.TrimSpace(item.Command)
	if item.Transport == "" {
		switch {
		case item.URL != "":
			item.Transport = "http"
		case item.Command != "":
			item.Transport = "stdio"
		}
	}
	switch item.Transport {
	case "http", "https":
		if item.URL == "" {
			return item, fmt.Errorf("http transport requires url")
		}
		item.Transport = "http"
	case "stdio":
		if item.Command == "" {
			return item, fmt.Errorf("stdio transport requires command")
		}
	default:
		return item, fmt.Errorf("unsupported transport %q", item.Transport)
	}
	if item.Name == "" && item.ID == "" {
		return item, fmt.Errorf("server name or id is required")
	}
	if item.TimeoutSec <= 0 {
		item.TimeoutSec = 20
	}
	return item, nil
}

func normalizeSkillServerConfig(item mcpclient.ServerConfig, skillDir, skillName string) mcpclient.ServerConfig {
	ctx := map[string]string{
		"SKILL_DIR":  skillDir,
		"SKILL_NAME": skillName,
		"TOOLS_DIR":  filepath.Join(skillDir, "tools"),
	}
	item.Name = expandSkillTemplate(item.Name, ctx)
	item.URL = expandSkillTemplate(item.URL, ctx)
	item.Command = resolveSkillPath(expandSkillTemplate(item.Command, ctx), skillDir)
	for i := range item.Args {
		item.Args[i] = resolveSkillPath(expandSkillTemplate(item.Args[i], ctx), skillDir)
	}
	if len(item.Env) > 0 {
		env := make(map[string]string, len(item.Env))
		for k, v := range item.Env {
			env[k] = expandSkillTemplate(v, ctx)
		}
		item.Env = env
	}
	return item
}

func expandSkillTemplate(value string, ctx map[string]string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return os.Expand(value, func(key string) string {
		if strings.Contains(key, ":-") {
			parts := strings.SplitN(key, ":-", 2)
			name := strings.TrimSpace(parts[0])
			fallback := ""
			if len(parts) > 1 {
				fallback = parts[1]
			}
			if v, ok := ctx[name]; ok && strings.TrimSpace(v) != "" {
				return v
			}
			if v := os.Getenv(name); strings.TrimSpace(v) != "" {
				return v
			}
			return fallback
		}
		if v, ok := ctx[key]; ok {
			return v
		}
		return os.Getenv(key)
	})
}

func resolveSkillPath(value, skillDir string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return value
	}
	if strings.Contains(value, "://") {
		return value
	}
	if strings.HasPrefix(value, ".") || strings.Contains(value, string(os.PathSeparator)) || strings.Contains(value, "/") {
		return filepath.Clean(filepath.Join(skillDir, filepath.FromSlash(value)))
	}
	return value
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			b.WriteRune(ch)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
