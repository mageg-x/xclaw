package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"xclaw/cli/protocol"
)

const SettingsKey = "mcp_servers_json"

type ServerConfig struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Transport  string            `json:"transport"`
	URL        string            `json:"url,omitempty"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Enabled    bool              `json:"enabled"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`
	ManagedBy  string            `json:"managed_by,omitempty"`
	Readonly   bool              `json:"readonly,omitempty"`
}

type ToolInfo struct {
	ServerID    string          `json:"server_id"`
	ServerName  string          `json:"server_name"`
	Name        string          `json:"name"`
	FullName    string          `json:"full_name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Risk        string          `json:"risk"`
	Transport   string          `json:"transport"`
	LastError   string          `json:"last_error,omitempty"`
	LastSyncAt  time.Time       `json:"last_sync_at,omitempty"`
	Available   bool            `json:"available"`
}

type cachedToolSet struct {
	tools       []ToolInfo
	lastError   string
	refreshedAt time.Time
}

type Manager struct {
	mu         sync.RWMutex
	servers    []ServerConfig
	cache      map[string]cachedToolSet
	httpClient *http.Client
	cacheTTL   time.Duration
}

func NewManager() *Manager {
	return &Manager{
		cache:    make(map[string]cachedToolSet),
		cacheTTL: 30 * time.Second,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func DecodeServers(raw string) ([]ServerConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []ServerConfig{}, nil
	}
	var items []ServerConfig
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	for i := range items {
		normalizeServer(&items[i])
	}
	return items, nil
}

func EncodeServers(items []ServerConfig) (string, error) {
	for i := range items {
		normalizeServer(&items[i])
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) SetServers(items []ServerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.servers = make([]ServerConfig, 0, len(items))
	for _, item := range items {
		normalizeServer(&item)
		m.servers = append(m.servers, item)
	}
	m.cache = make(map[string]cachedToolSet)
}

func (m *Manager) Servers() []ServerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServerConfig, len(m.servers))
	copy(out, m.servers)
	return out
}

func (m *Manager) UpsertServer(item ServerConfig) ServerConfig {
	normalizeServer(&item)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.servers {
		if m.servers[i].ID == item.ID {
			m.servers[i] = item
			delete(m.cache, item.ID)
			return item
		}
	}
	m.servers = append(m.servers, item)
	delete(m.cache, item.ID)
	return item
}

func (m *Manager) RemoveServer(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	filtered := m.servers[:0]
	removed := false
	for _, item := range m.servers {
		if item.ID == id {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	m.servers = append([]ServerConfig(nil), filtered...)
	delete(m.cache, id)
	return removed
}

func (m *Manager) TestServer(ctx context.Context, id string) (map[string]any, error) {
	server, ok := m.getServer(id)
	if !ok {
		return nil, fmt.Errorf("mcp server not found: %s", id)
	}
	tools, err := m.fetchTools(ctx, server)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":         server.ID,
		"name":       server.Name,
		"transport":  server.Transport,
		"tool_count": len(tools),
		"tools":      tools,
	}, nil
}

func (m *Manager) ListTools(ctx context.Context) []ToolInfo {
	servers := m.Servers()
	out := make([]ToolInfo, 0)
	for _, server := range servers {
		if !server.Enabled {
			continue
		}
		tools, err := m.fetchTools(ctx, server)
		if err != nil {
			out = append(out, ToolInfo{
				ServerID:   server.ID,
				ServerName: server.Name,
				FullName:   "mcp:" + server.ID + ":*",
				Transport:  server.Transport,
				LastError:  err.Error(),
				LastSyncAt: time.Now().UTC(),
				Available:  false,
			})
			continue
		}
		out = append(out, tools...)
	}
	return out
}

func (m *Manager) CallTool(ctx context.Context, fullName string, params map[string]string) (ToolInfo, string, error) {
	serverID, toolName, err := parseFullToolName(fullName)
	if err != nil {
		return ToolInfo{}, "", err
	}
	server, ok := m.getServer(serverID)
	if !ok {
		return ToolInfo{}, "", fmt.Errorf("mcp server not found: %s", serverID)
	}
	tools, err := m.fetchTools(ctx, server)
	if err != nil {
		return ToolInfo{}, "", err
	}
	var tool ToolInfo
	found := false
	for _, item := range tools {
		if item.Name == toolName {
			tool = item
			found = true
			break
		}
	}
	if !found {
		return ToolInfo{}, "", fmt.Errorf("mcp tool not found: %s", fullName)
	}
	result, err := m.callTool(ctx, server, toolName, params)
	return tool, result, err
}

func (m *Manager) fetchTools(ctx context.Context, server ServerConfig) ([]ToolInfo, error) {
	m.mu.RLock()
	cached, ok := m.cache[server.ID]
	m.mu.RUnlock()
	if ok && time.Since(cached.refreshedAt) < m.cacheTTL {
		return cached.tools, cachedErr(cached.lastError)
	}

	tools, err := m.listTools(ctx, server)
	cache := cachedToolSet{
		tools:       tools,
		refreshedAt: time.Now().UTC(),
	}
	if err != nil {
		cache.lastError = err.Error()
	}
	m.mu.Lock()
	m.cache[server.ID] = cache
	m.mu.Unlock()
	return tools, err
}

func (m *Manager) listTools(ctx context.Context, server ServerConfig) ([]ToolInfo, error) {
	reqCtx, cancel := context.WithTimeout(ctx, server.timeout())
	defer cancel()
	switch server.Transport {
	case "stdio":
		return m.listToolsSDK(reqCtx, server)
	case "http":
		tools, err := m.listToolsSDK(reqCtx, server)
		if err == nil {
			return tools, nil
		}
		legacyTools, legacyErr := m.listToolsLegacy(reqCtx, server)
		if legacyErr == nil {
			return legacyTools, nil
		}
		return nil, fmt.Errorf("mcp sdk tools/list failed: %v; legacy fallback failed: %w", err, legacyErr)
	default:
		return nil, fmt.Errorf("unsupported mcp transport: %s", server.Transport)
	}
}

func (m *Manager) callTool(ctx context.Context, server ServerConfig, toolName string, params map[string]string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, server.timeout())
	defer cancel()
	switch server.Transport {
	case "stdio":
		return m.callToolSDK(reqCtx, server, toolName, params)
	case "http":
		result, err := m.callToolSDK(reqCtx, server, toolName, params)
		if err == nil {
			return result, nil
		}
		legacyResult, legacyErr := m.callToolLegacy(reqCtx, server, toolName, params)
		if legacyErr == nil {
			return legacyResult, nil
		}
		return "", fmt.Errorf("mcp sdk tools/call failed: %v; legacy fallback failed: %w", err, legacyErr)
	default:
		return "", fmt.Errorf("unsupported mcp transport: %s", server.Transport)
	}
}

func (m *Manager) initialize(ctx context.Context, server ServerConfig) error {
	if server.Transport == "stdio" {
		return nil
	}
	_, err := m.request(ctx, server, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo": map[string]string{
			"name":    "xclaw",
			"version": "1.0.0",
		},
	})
	return err
}

func (m *Manager) request(ctx context.Context, server ServerConfig, method string, params map[string]any) (json.RawMessage, error) {
	switch server.Transport {
	case "http":
		return m.requestHTTP(ctx, server, method, params)
	case "stdio":
		return nil, fmt.Errorf("direct stdio json-rpc path retired; use sdk-backed list/call")
	default:
		return nil, fmt.Errorf("unsupported mcp transport: %s", server.Transport)
	}
}

func (m *Manager) listToolsSDK(ctx context.Context, server ServerConfig) ([]ToolInfo, error) {
	session, err := m.connectSDKSession(ctx, server)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	result, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]ToolInfo, 0, len(result.Tools))
	for _, item := range result.Tools {
		if item == nil {
			continue
		}
		out = append(out, ToolInfo{
			ServerID:    server.ID,
			ServerName:  server.Name,
			Name:        item.Name,
			FullName:    BuildFullToolName(server.ID, item.Name),
			Description: strings.TrimSpace(firstNonEmpty(item.Description, item.Title)),
			InputSchema: marshalSDKValue(item.InputSchema),
			Risk:        inferRisk(item.Name),
			Transport:   server.Transport,
			LastSyncAt:  now,
			Available:   true,
		})
	}
	return out, nil
}

func (m *Manager) callToolSDK(ctx context.Context, server ServerConfig, toolName string, params map[string]string) (string, error) {
	session, err := m.connectSDKSession(ctx, server)
	if err != nil {
		return "", err
	}
	defer session.Close()

	args := make(map[string]any, len(params))
	for k, v := range params {
		args[k] = v
	}
	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return "", err
	}
	return flattenSDKToolResult(result), nil
}

func (m *Manager) connectSDKSession(ctx context.Context, server ServerConfig) (*sdkmcp.ClientSession, error) {
	client := sdkmcp.NewClient(&sdkmcp.Implementation{
		Name:    "xclaw",
		Version: "1.0.0",
	}, nil)
	transport, err := m.sdkTransport(ctx, server)
	if err != nil {
		return nil, err
	}
	return client.Connect(ctx, transport, nil)
}

func (m *Manager) sdkTransport(ctx context.Context, server ServerConfig) (sdkmcp.Transport, error) {
	switch server.Transport {
	case "stdio":
		if strings.TrimSpace(server.Command) == "" {
			return nil, fmt.Errorf("mcp stdio command is required")
		}
		cmd := exec.CommandContext(ctx, server.Command, server.Args...)
		if shouldSetCommandDir(server.Command) {
			cmd.Dir = filepath.Dir(server.Command)
		}
		cmd.Env = os.Environ()
		for k, v := range server.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
		return &sdkmcp.CommandTransport{
			Command:           cmd,
			TerminateDuration: 3 * time.Second,
		}, nil
	case "http":
		endpoint := strings.TrimSpace(server.URL)
		if endpoint == "" {
			return nil, fmt.Errorf("mcp http url is required")
		}
		httpClient := m.httpClient
		if httpClient == nil {
			httpClient = &http.Client{Timeout: server.timeout()}
		}
		lowerEndpoint := strings.ToLower(endpoint)
		if strings.Contains(lowerEndpoint, "/sse") {
			return &sdkmcp.SSEClientTransport{
				Endpoint:   endpoint,
				HTTPClient: httpClient,
			}, nil
		}
		return &sdkmcp.StreamableClientTransport{
			Endpoint:             endpoint,
			HTTPClient:           httpClient,
			DisableStandaloneSSE: true,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported mcp transport: %s", server.Transport)
	}
}

func shouldSetCommandDir(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	return filepath.IsAbs(command) || strings.Contains(command, string(os.PathSeparator))
}

func (m *Manager) listToolsLegacy(ctx context.Context, server ServerConfig) ([]ToolInfo, error) {
	_ = m.initialize(ctx, server)
	result, err := m.request(ctx, server, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tools []protocol.MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return nil, fmt.Errorf("decode tools/list: %w", err)
	}
	out := make([]ToolInfo, 0, len(payload.Tools))
	now := time.Now().UTC()
	for _, item := range payload.Tools {
		out = append(out, ToolInfo{
			ServerID:    server.ID,
			ServerName:  server.Name,
			Name:        item.Name,
			FullName:    BuildFullToolName(server.ID, item.Name),
			Description: item.Description,
			InputSchema: item.InputSchema,
			Risk:        inferRisk(item.Name),
			Transport:   server.Transport,
			LastSyncAt:  now,
			Available:   true,
		})
	}
	return out, nil
}

func (m *Manager) callToolLegacy(ctx context.Context, server ServerConfig, toolName string, params map[string]string) (string, error) {
	_ = m.initialize(ctx, server)

	args := make(map[string]any, len(params))
	for k, v := range params {
		args[k] = v
	}
	result, err := m.request(ctx, server, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	return flattenToolResult(result), nil
}

func (m *Manager) requestHTTP(ctx context.Context, server ServerConfig, method string, params map[string]any) (json.RawMessage, error) {
	reqBody, err := json.Marshal(protocol.MCPRequest{
		JSONRPC: "2.0",
		ID:      fmt.Sprintf("%s-%d", method, time.Now().UnixNano()),
		Method:  method,
		Params:  mustJSON(params),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("mcp http status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out protocol.MCPResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, out.Error
	}
	return out.Result, nil
}

func parseFullToolName(fullName string) (string, string, error) {
	fullName = strings.TrimSpace(fullName)
	if !strings.HasPrefix(fullName, "mcp:") {
		return "", "", fmt.Errorf("invalid mcp tool name: %s", fullName)
	}
	rest := strings.TrimPrefix(fullName, "mcp:")
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid mcp tool name: %s", fullName)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func BuildFullToolName(serverID, toolName string) string {
	return "mcp:" + strings.TrimSpace(serverID) + ":" + strings.TrimSpace(toolName)
}

func inferRisk(toolName string) string {
	lower := strings.ToLower(strings.TrimSpace(toolName))
	switch {
	case strings.Contains(lower, "read"), strings.Contains(lower, "list"), strings.Contains(lower, "get"), strings.Contains(lower, "search"):
		return "read"
	case strings.Contains(lower, "write"), strings.Contains(lower, "create"), strings.Contains(lower, "update"), strings.Contains(lower, "delete"), strings.Contains(lower, "install"):
		return "write"
	default:
		return "exec"
	}
}

func flattenToolResult(raw json.RawMessage) string {
	var payload struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && len(payload.Content) > 0 {
		parts := make([]string, 0, len(payload.Content))
		for _, item := range payload.Content {
			if text := strings.TrimSpace(fmt.Sprintf("%v", item["text"])); text != "" && text != "<nil>" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return strings.TrimSpace(string(raw))
}

func flattenSDKToolResult(result *sdkmcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	parts := make([]string, 0, len(result.Content)+1)
	for _, item := range result.Content {
		switch v := item.(type) {
		case *sdkmcp.TextContent:
			if text := strings.TrimSpace(v.Text); text != "" {
				parts = append(parts, text)
			}
		case *sdkmcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image:%s]", strings.TrimSpace(v.MIMEType)))
		case *sdkmcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio:%s]", strings.TrimSpace(v.MIMEType)))
		default:
			raw, err := json.Marshal(v)
			if err == nil && strings.TrimSpace(string(raw)) != "" && string(raw) != "null" {
				parts = append(parts, string(raw))
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	if raw := marshalSDKValue(result.StructuredContent); len(raw) > 0 {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

func marshalSDKValue(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

func cachedErr(msg string) error {
	if strings.TrimSpace(msg) == "" {
		return nil
	}
	return fmt.Errorf("%s", msg)
}

func (m *Manager) getServer(id string) (ServerConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, item := range m.servers {
		if item.ID == id {
			return item, true
		}
	}
	return ServerConfig{}, false
}

func normalizeServer(item *ServerConfig) {
	item.ID = slugify(firstNonEmpty(item.ID, item.Name))
	item.Name = firstNonEmpty(strings.TrimSpace(item.Name), item.ID)
	item.Transport = strings.ToLower(strings.TrimSpace(item.Transport))
	if item.Transport == "" {
		if strings.TrimSpace(item.URL) != "" {
			item.Transport = "http"
		} else {
			item.Transport = "stdio"
		}
	}
	if item.Transport == "https" {
		item.Transport = "http"
	}
	if item.TimeoutSec <= 0 {
		item.TimeoutSec = 20
	}
}

func (s ServerConfig) timeout() time.Duration {
	if s.TimeoutSec <= 0 {
		return 20 * time.Second
	}
	return time.Duration(s.TimeoutSec) * time.Second
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return fmt.Sprintf("mcp-%d", time.Now().UnixNano())
	}
	var b strings.Builder
	prevDash := false
	for _, ch := range s {
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
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return fmt.Sprintf("mcp-%d", time.Now().UnixNano())
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
