package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"xclaw/cli/config"
	"xclaw/cli/protocol"
)

type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func runMCPServerCommand(args []string) error {
	cfg, err := loadCLIConfig("")
	if err != nil {
		cfg = config.DefaultConfig()
	}

	tools := []mcpTool{
		{Name: "get_project_context", Description: "返回项目结构、AGENTS.md 与最近目录摘要"},
		{Name: "run_test", Description: "运行测试命令"},
		{Name: "lint", Description: "运行 lint 命令"},
		{Name: "typecheck", Description: "运行类型检查命令"},
		{Name: "delegate_task", Description: "将任务委托给本地 Agent 执行（占位实现）"},
	}

	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	for {
		payload, err := readMCPPayload(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(payload) == 0 {
			continue
		}

		var req protocol.MCPRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			resp := protocol.MCPResponse{JSONRPC: "2.0", ID: nil, Error: &protocol.MCPError{Code: -32700, Message: "parse error", Data: err.Error()}}
			if err := writeMCPPayload(writer, resp); err != nil {
				return err
			}
			continue
		}
		resp := handleMCPRequest(context.Background(), cfg, tools, req)
		if err := writeMCPPayload(writer, resp); err != nil {
			return err
		}
	}
}

func handleMCPRequest(ctx context.Context, cfg config.RuntimeConfig, tools []mcpTool, req protocol.MCPRequest) protocol.MCPResponse {
	resp := protocol.MCPResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		result := map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"serverInfo": map[string]string{
				"name":    "xclaw",
				"version": version,
			},
			"capabilities": map[string]interface{}{
				"tools": map[string]bool{"listChanged": false},
			},
		}
		resp.Result = mustJSON(result)
	case "tools/list":
		out := make([]map[string]interface{}, 0, len(tools))
		for _, t := range tools {
			out = append(out, map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			})
		}
		resp.Result = mustJSON(map[string]interface{}{"tools": out})
	case "resources/list":
		resp.Result = mustJSON(map[string]interface{}{"resources": []map[string]interface{}{}})
	case "tools/call":
		var call struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &call); err != nil {
			resp.Error = &protocol.MCPError{Code: -32602, Message: "invalid params", Data: err.Error()}
			return resp
		}
		text, err := runMCPTool(ctx, cfg, call.Name, call.Arguments)
		if err != nil {
			resp.Error = &protocol.MCPError{Code: -32603, Message: "tool failed", Data: err.Error()}
			return resp
		}
		resp.Result = mustJSON(map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": text}},
		})
	default:
		resp.Error = &protocol.MCPError{Code: -32601, Message: "method not found", Data: req.Method}
	}
	return resp
}

func runMCPTool(ctx context.Context, cfg config.RuntimeConfig, name string, args map[string]interface{}) (string, error) {
	switch strings.TrimSpace(name) {
	case "get_project_context":
		cwd, _ := os.Getwd()
		agentsPath := filepath.Join(cwd, "AGENTS.md")
		agentRules := ""
		if b, err := os.ReadFile(agentsPath); err == nil {
			agentRules = trimForLog(string(b), 1600)
		}
		entries, _ := os.ReadDir(cwd)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name()+"/")
			} else {
				names = append(names, e.Name())
			}
			if len(names) >= 40 {
				break
			}
		}
		return fmt.Sprintf("data_dir=%s\nroot=%s\nentries=%s\nAGENTS.md=%s", cfg.DataDir, cwd, strings.Join(names, ", "), agentRules), nil
	case "run_test":
		return runShellAndCapture(ctx, argOrDefault(args, "command", "go test ./..."))
	case "lint":
		return runShellAndCapture(ctx, argOrDefault(args, "command", "go test ./..."))
	case "typecheck":
		return runShellAndCapture(ctx, argOrDefault(args, "command", "go test ./..."))
	case "delegate_task":
		task := argOrDefault(args, "task", "")
		if strings.TrimSpace(task) == "" {
			return "", fmt.Errorf("task is required")
		}
		return "delegated locally: " + strings.TrimSpace(task), nil
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func runShellAndCapture(ctx context.Context, command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("empty command")
	}
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bash", "-lc", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), err
	}
	return strings.TrimSpace(string(out)), nil
}

func argOrDefault(args map[string]interface{}, key, fallback string) string {
	if args == nil {
		return fallback
	}
	if v, ok := args[key]; ok {
		t := strings.TrimSpace(fmt.Sprintf("%v", v))
		if t != "" {
			return t
		}
	}
	return fallback
}

func readMCPPayload(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid content-length header")
		}
		n, err := strconvAtoi(strings.TrimSpace(parts[1]))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid content-length")
		}
		for {
			h, err := r.ReadString('\n')
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(h) == "" {
				break
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
		return payload, nil
	}
	return []byte(trimmed), nil
}

func writeMCPPayload(w *bufio.Writer, resp protocol.MCPResponse) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return w.Flush()
}

func mustJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func strconvAtoi(s string) (int, error) {
	v := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid int")
		}
		v = v*10 + int(ch-'0')
	}
	return v, nil
}
