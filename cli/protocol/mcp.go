package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// MCP (Model Context Protocol) implementation
// Based on Anthropic's Model Context Protocol specification

// MCPRequest represents an MCP request
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MCPResponse represents an MCP response
type MCPResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *MCPError       `json:"error,omitempty"`
}

// MCPError represents an MCP error
type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *MCPError) Error() string {
	return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message)
}

// MCPCapability represents a server capability
type MCPCapability struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// MCPResource represents an accessible resource
type MCPResource struct {
	URI         string            `json:"uri"`
	Name        string            `json:"name"`
	MIMEType    string            `json:"mimeType,omitempty"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// MCPTool represents an available tool
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPPrompt represents an available prompt
type MCPPrompt struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Template    string `json:"template"`
}

// MCPServer implements an MCP server endpoint
type MCPServer struct {
	name         string
	version      string
	capabilities []MCPCapability
	resources    []MCPResource
	tools        []MCPTool
	prompts      []MCPPrompt

	handlers map[string]func(ctx context.Context, params json.RawMessage) (any, error)
}

// NewMCPServer creates a new MCP server
func NewMCPServer(name, version string) *MCPServer {
	s := &MCPServer{
		name:     name,
		version:  version,
		handlers: make(map[string]func(ctx context.Context, params json.RawMessage) (any, error)),
	}

	// Register default handlers
	s.handlers["initialize"] = s.handleInitialize
	s.handlers["resources/list"] = s.handleListResources
	s.handlers["resources/read"] = s.handleReadResource
	s.handlers["tools/list"] = s.handleListTools
	s.handlers["tools/call"] = s.handleCallTool
	s.handlers["prompts/list"] = s.handleListPrompts
	s.handlers["prompts/get"] = s.handleGetPrompt

	return s
}

func (s *MCPServer) SetCapabilities(caps []MCPCapability) {
	s.capabilities = caps
}

func (s *MCPServer) SetResources(resources []MCPResource) {
	s.resources = resources
}

func (s *MCPServer) SetTools(tools []MCPTool) {
	s.tools = tools
}

func (s *MCPServer) SetPrompts(prompts []MCPPrompt) {
	s.prompts = prompts
}

func (s *MCPServer) RegisterTool(name string, handler func(ctx context.Context, args map[string]any) (any, error)) {
	for i, t := range s.tools {
		if t.Name == name {
			// Update existing
			s.tools[i] = MCPTool{
				Name:        name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			}
			break
		}
	}

	s.handlers["tools/call"] = func(ctx context.Context, params json.RawMessage) (any, error) {
		var req struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
		if req.Name != name {
			// Fallback to default
			return s.handleCallTool(ctx, params)
		}
		return handler(ctx, req.Arguments)
	}
}

// ServeHTTP implements http.Handler for MCP protocol
func (s *MCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var req MCPRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, nil, -32700, "Parse error", err.Error())
		return
	}

	ctx := r.Context()
	handler, ok := s.handlers[req.Method]
	if !ok {
		s.writeError(w, req.ID, -32601, "Method not found", req.Method)
		return
	}

	result, err := handler(ctx, req.Params)
	if err != nil {
		s.writeError(w, req.ID, -32603, "Internal error", err.Error())
		return
	}

	resp := MCPResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}
	if result != nil {
		raw, _ := json.Marshal(result)
		resp.Result = raw
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *MCPServer) writeError(w http.ResponseWriter, id any, code int, message string, data any) {
	resp := MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &MCPError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // MCP returns 200 even for errors
	json.NewEncoder(w).Encode(resp)
}

func (s *MCPServer) handleInitialize(ctx context.Context, params json.RawMessage) (any, error) {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"serverInfo": map[string]string{
			"name":    s.name,
			"version": s.version,
		},
		"capabilities": s.capabilities,
	}, nil
}

func (s *MCPServer) handleListResources(ctx context.Context, params json.RawMessage) (any, error) {
	return map[string]any{"resources": s.resources}, nil
}

func (s *MCPServer) handleReadResource(ctx context.Context, params json.RawMessage) (any, error) {
	var req struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	for _, r := range s.resources {
		if r.URI == req.URI {
			return map[string]any{"resource": r}, nil
		}
	}
	return nil, fmt.Errorf("resource not found: %s", req.URI)
}

func (s *MCPServer) handleListTools(ctx context.Context, params json.RawMessage) (any, error) {
	return map[string]any{"tools": s.tools}, nil
}

func (s *MCPServer) handleCallTool(ctx context.Context, params json.RawMessage) (any, error) {
	var req struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	return map[string]any{
		"content": []map[string]string{
			{"type": "text", "text": fmt.Sprintf("Tool %s called with %v", req.Name, req.Arguments)},
		},
	}, nil
}

func (s *MCPServer) handleListPrompts(ctx context.Context, params json.RawMessage) (any, error) {
	return map[string]any{"prompts": s.prompts}, nil
}

func (s *MCPServer) handleGetPrompt(ctx context.Context, params json.RawMessage) (any, error) {
	var req struct {
		Name string            `json:"name"`
		Args map[string]string `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	for _, p := range s.prompts {
		if p.Name == req.Name {
			return map[string]any{"prompt": p}, nil
		}
	}
	return nil, fmt.Errorf("prompt not found: %s", req.Name)
}
