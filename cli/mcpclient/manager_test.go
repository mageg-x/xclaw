package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"xclaw/cli/protocol"
)

func TestBuildAndParseFullToolName(t *testing.T) {
	full := BuildFullToolName("github-tools", "list_prs")
	if full != "mcp:github-tools:list_prs" {
		t.Fatalf("unexpected full tool name: %s", full)
	}
	serverID, toolName, err := parseFullToolName(full)
	if err != nil {
		t.Fatalf("parseFullToolName failed: %v", err)
	}
	if serverID != "github-tools" || toolName != "list_prs" {
		t.Fatalf("unexpected parse result: %s / %s", serverID, toolName)
	}
}

func TestNormalizeServerPreservesDisabledState(t *testing.T) {
	item := ServerConfig{
		Name:      "GitHub Tools",
		Transport: "http",
		URL:       "http://127.0.0.1:8123/mcp",
		Enabled:   false,
	}
	normalizeServer(&item)
	if item.ID != "github-tools" {
		t.Fatalf("unexpected server id: %s", item.ID)
	}
	if item.Enabled {
		t.Fatalf("expected disabled server to remain disabled")
	}
	if item.TimeoutSec != 20 {
		t.Fatalf("unexpected timeout: %d", item.TimeoutSec)
	}
}

type echoToolInput struct {
	Text string `json:"text"`
}

func TestManagerListToolsAndCallToolHTTPViaSDK(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "echo", Version: "v0.0.1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo text"}, func(ctx context.Context, req *sdkmcp.CallToolRequest, args echoToolInput) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{
				&sdkmcp.TextContent{Text: "sdk:" + args.Text},
			},
		}, nil, nil
	})
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return server
	}, &sdkmcp.StreamableHTTPOptions{
		JSONResponse: true,
		Stateless:    true,
	})

	manager := NewManager()
	manager.httpClient = newHandlerHTTPClient(handler)
	manager.SetServers([]ServerConfig{{
		ID:        "sdk-http",
		Name:      "SDK HTTP",
		Transport: "http",
		URL:       "http://sdk.example/mcp",
		Enabled:   true,
	}})

	tools := manager.ListTools(context.Background())
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Fatalf("unexpected tool name: %s", tools[0].Name)
	}
	if !json.Valid(tools[0].InputSchema) {
		t.Fatalf("expected valid input schema, got %s", string(tools[0].InputSchema))
	}

	tool, result, err := manager.CallTool(context.Background(), BuildFullToolName("sdk-http", "echo"), map[string]string{
		"text": "hello",
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if tool.Name != "echo" {
		t.Fatalf("unexpected returned tool: %s", tool.Name)
	}
	if result != "sdk:hello" {
		t.Fatalf("unexpected call result: %s", result)
	}
}

func TestManagerHTTPFallsBackToLegacyJSONRPC(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			http.Error(w, "streamable not supported", http.StatusBadRequest)
			return
		}

		var req protocol.MCPRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo": map[string]string{
					"name":    "legacy",
					"version": "1.0.0",
				},
			}
		case "tools/list":
			result = map[string]any{
				"tools": []protocol.MCPTool{{
					Name:        "legacy_echo",
					Description: "legacy echo",
					InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
				}},
			}
		case "tools/call":
			result = map[string]any{
				"content": []map[string]any{{
					"type": "text",
					"text": "legacy:hello",
				}},
			}
		default:
			t.Fatalf("unexpected method: %s", req.Method)
		}

		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("marshal result: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(protocol.MCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  raw,
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	})

	manager := NewManager()
	manager.httpClient = newHandlerHTTPClient(handler)
	manager.SetServers([]ServerConfig{{
		ID:        "legacy-http",
		Name:      "Legacy HTTP",
		Transport: "http",
		URL:       "http://legacy.example/mcp",
		Enabled:   true,
	}})

	tools := manager.ListTools(context.Background())
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "legacy_echo" {
		t.Fatalf("unexpected tool name: %s", tools[0].Name)
	}

	_, result, err := manager.CallTool(context.Background(), BuildFullToolName("legacy-http", "legacy_echo"), map[string]string{
		"text": "hello",
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result != "legacy:hello" {
		t.Fatalf("unexpected call result: %s", result)
	}
}

func newHandlerHTTPClient(handler http.Handler) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec.Result(), nil
		}),
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
