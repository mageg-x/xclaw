package mcpregistry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xclaw/cli/db"
	"xclaw/cli/mcpclient"
)

func TestDiscoverSkillServers(t *testing.T) {
	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "installed", "github-pack", "tools")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
  "mcp_servers": [
    {
      "name": "GitHub Filesystem",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
      "enabled": true
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(skillDir, "github.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := DiscoverSkillServers(skillsRoot)
	if err != nil {
		t.Fatalf("DiscoverSkillServers failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 discovered server, got %d", len(items))
	}
	if items[0].ManagedBy != "skill:github-pack" || !items[0].Readonly {
		t.Fatalf("unexpected managed metadata: %#v", items[0])
	}
	if items[0].ID == "" {
		t.Fatalf("expected generated id")
	}
}

func TestDiscoverSkillServersFromYAML(t *testing.T) {
	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "installed", "fs-pack", "tools")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
mcp_servers:
  - name: Filesystem Tools
    transport: stdio
    command: npx
    args:
      - -y
      - "@modelcontextprotocol/server-filesystem"
      - /tmp
    env:
      HOME: /tmp/xclaw
      DEBUG: "1"
    enabled: true
    timeout_sec: 45
`
	if err := os.WriteFile(filepath.Join(skillDir, "filesystem.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := DiscoverSkillServers(skillsRoot)
	if err != nil {
		t.Fatalf("DiscoverSkillServers failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 discovered server, got %d", len(items))
	}
	if items[0].Transport != "stdio" || items[0].Command != "npx" {
		t.Fatalf("unexpected server config: %#v", items[0])
	}
	if len(items[0].Args) != 3 || items[0].Env["HOME"] != "/tmp/xclaw" || items[0].TimeoutSec != 45 {
		t.Fatalf("unexpected parsed args/env/timeout: %#v", items[0])
	}
}

func TestDiscoverSkillServersFromJSONServersAlias(t *testing.T) {
	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "installed", "json-pack", "tools")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
  "servers": [
    {
      "name": "Alias HTTP",
      "transport": "http",
      "url": "http://127.0.0.1:8181/mcp",
      "enabled": true
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(skillDir, "alias.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := DiscoverSkillServers(skillsRoot)
	if err != nil {
		t.Fatalf("DiscoverSkillServers failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 discovered server, got %d", len(items))
	}
	if items[0].Transport != "http" || items[0].URL != "http://127.0.0.1:8181/mcp" {
		t.Fatalf("unexpected alias server config: %#v", items[0])
	}
}

func TestSaveManualAndSyncMergesSkillServers(t *testing.T) {
	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "installed", "github-pack", "tools")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "github.json"), []byte(`{"name":"GitHub HTTP","transport":"http","url":"http://127.0.0.1:8123/mcp","enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	manual := []mcpclient.ServerConfig{{
		ID:        "manual-shell",
		Name:      "Manual Shell",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@foo/bar"},
		Enabled:   true,
	}}
	merged, err := SaveManualAndSync(context.Background(), store, skillsRoot, manual)
	if err != nil {
		t.Fatalf("SaveManualAndSync failed: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged servers, got %d", len(merged))
	}

	loadedManual, err := LoadManual(context.Background(), store)
	if err != nil {
		t.Fatalf("LoadManual failed: %v", err)
	}
	if len(loadedManual) != 1 || loadedManual[0].ID != "manual-shell" {
		t.Fatalf("unexpected manual servers: %#v", loadedManual)
	}
}

func TestDiscoverSkillServersRejectsInvalidConfig(t *testing.T) {
	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "installed", "broken-pack", "tools")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "broken.yaml"), []byte(`
mcp_servers:
  - name: Broken Server
    transport: stdio
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := DiscoverSkillServers(skillsRoot)
	if err == nil {
		t.Fatalf("expected invalid config error")
	}
	if got := err.Error(); got == "" || !containsAll(got, []string{"invalid skill mcp server", "stdio transport requires command"}) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDiscoverSkillServersExpandsSkillPathsAndTemplates(t *testing.T) {
	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "installed", "bridge-pack", "tools")
	if err := os.MkdirAll(filepath.Join(skillsRoot, "installed", "bridge-pack", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
mcp_servers:
  - name: ${SKILL_NAME} bridge
    transport: stdio
    command: ./bin/bridge
    args:
      - ${TOOLS_DIR}/schema.json
    env:
      DATA_DIR: ${SKILL_DIR}/data
    enabled: true
`
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "bridge.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := DiscoverSkillServers(skillsRoot)
	if err != nil {
		t.Fatalf("DiscoverSkillServers failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 discovered server, got %d", len(items))
	}
	wantCmd := filepath.Join(skillsRoot, "installed", "bridge-pack", "bin", "bridge")
	if items[0].Command != wantCmd {
		t.Fatalf("unexpected command: %s", items[0].Command)
	}
	wantArg := filepath.Join(skillsRoot, "installed", "bridge-pack", "tools", "schema.json")
	if len(items[0].Args) != 1 || items[0].Args[0] != wantArg {
		t.Fatalf("unexpected args: %#v", items[0].Args)
	}
	if items[0].Env["DATA_DIR"] != filepath.Join(skillsRoot, "installed", "bridge-pack", "data") {
		t.Fatalf("unexpected env: %#v", items[0].Env)
	}
}

func TestDiscoverBuiltinSkillServers(t *testing.T) {
	skillsRoot := t.TempDir()
	toolDir := filepath.Join(skillsRoot, "builtin", "filesystem-mcp", "tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "filesystem.json"), []byte(`{"name":"Filesystem MCP","transport":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","/tmp"],"enabled":false}`), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := DiscoverSkillServers(skillsRoot)
	if err != nil {
		t.Fatalf("DiscoverSkillServers failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 discovered server, got %d", len(items))
	}
	if items[0].ManagedBy != "skill:filesystem-mcp" {
		t.Fatalf("unexpected managedBy: %#v", items[0])
	}
}

func TestDiscoverSkillServersFromSingleServerYAMLWithFallbackTemplate(t *testing.T) {
	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "installed", "http-pack", "tools")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
name: HTTP Bridge
transport: http
url: ${MCP_BASE_URL:-http://127.0.0.1:9010}/mcp
enabled: true
timeout_sec: 12
`
	if err := os.WriteFile(filepath.Join(skillDir, "http.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := DiscoverSkillServers(skillsRoot)
	if err != nil {
		t.Fatalf("DiscoverSkillServers failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 discovered server, got %d", len(items))
	}
	if items[0].Transport != "http" || items[0].URL != "http://127.0.0.1:9010/mcp" {
		t.Fatalf("unexpected server config: %#v", items[0])
	}
	if items[0].TimeoutSec != 12 {
		t.Fatalf("unexpected timeout: %#v", items[0])
	}
	if items[0].ManagedBy != "skill:http-pack" || !items[0].Readonly {
		t.Fatalf("unexpected managed metadata: %#v", items[0])
	}
}

func TestDiscoverSkillServersFallbackTemplateUsesEnvironmentOverride(t *testing.T) {
	t.Setenv("MCP_BASE_URL", "http://10.0.0.8:7777")

	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "installed", "http-pack", "tools")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
name: HTTP Bridge
transport: http
url: ${MCP_BASE_URL:-http://127.0.0.1:9010}/mcp
enabled: true
`
	if err := os.WriteFile(filepath.Join(skillDir, "http.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := DiscoverSkillServers(skillsRoot)
	if err != nil {
		t.Fatalf("DiscoverSkillServers failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 discovered server, got %d", len(items))
	}
	if items[0].URL != "http://10.0.0.8:7777/mcp" {
		t.Fatalf("unexpected env override url: %#v", items[0])
	}
}

func containsAll(value string, parts []string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
