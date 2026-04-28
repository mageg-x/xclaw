package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"xclaw/cli/config"
	"xclaw/cli/db"
	"xclaw/cli/mcpclient"
	"xclaw/cli/skills"
)

func TestHandleSkillReloadSyncsMCPServers(t *testing.T) {
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")
	skillDir := filepath.Join(skillsDir, "installed", "demo-mcp")
	if err := os.MkdirAll(filepath.Join(skillDir, "tools"), 0o755); err != nil {
		t.Fatalf("mkdir skill tools: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: demo-mcp
description: demo skill
version: 1.0.0
---

demo instructions
`), 0o644); err != nil {
		t.Fatalf("write skill doc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "tools", "fetch.json"), []byte(`{
  "name": "Skill Fetch",
  "transport": "stdio",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-fetch"]
}`), 0o644); err != nil {
		t.Fatalf("write tool config: %v", err)
	}

	store, err := db.Open(filepath.Join(root, "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{
		cfg:         config.RuntimeConfig{SkillsDir: skillsDir},
		store:       store,
		skillLoader: skills.NewLoader(skillsDir, ""),
		mcpClients:  mcpclient.NewManager(),
	}

	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/skills/reload", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	server.handleSkillReload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := int(resp["count"].(float64)); got != 1 {
		t.Fatalf("unexpected skill count: %v", resp)
	}
	if got := int(resp["mcp_count"].(float64)); got != 1 {
		t.Fatalf("unexpected mcp count: %v", resp)
	}

	servers := server.mcpClients.Servers()
	if len(servers) != 1 {
		t.Fatalf("unexpected servers: %#v", servers)
	}
	if servers[0].ManagedBy != "skill:demo-mcp" || !servers[0].Readonly {
		t.Fatalf("unexpected managed server: %#v", servers[0])
	}
	if servers[0].ID != "skill-demo-mcp-fetch-skill-fetch" {
		t.Fatalf("unexpected server id: %#v", servers[0])
	}
}

func TestHandleSkillInstallRegistersMCPServersFromCommunityZip(t *testing.T) {
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")
	zipPath := filepath.Join(root, "community-pack.zip")
	if err := writeSkillZip(zipPath, map[string]string{
		"community-pack-main/skill.yaml": `name: community-pack
version: 3.1.0
description: community package
instructions_file: prompts/guide.md
files:
  - tools/fetch.yaml
`,
		"community-pack-main/prompts/guide.md": `Use the fetch bridge.`,
		"community-pack-main/tools/fetch.yaml": `mcp_servers:
  - name: Fetch Bridge
    transport: stdio
    command: npx
    args:
      - -y
      - "@modelcontextprotocol/server-fetch"
    enabled: true
`,
	}); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	store, err := db.Open(filepath.Join(root, "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{
		cfg:         config.RuntimeConfig{SkillsDir: skillsDir},
		store:       store,
		skillLoader: skills.NewLoader(skillsDir, ""),
		market:      skills.NewMarket(skillsDir),
		mcpClients:  mcpclient.NewManager(),
	}

	reqBody := map[string]any{"source_path": zipPath}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/skills/install", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleSkillInstall(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Item struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"item"`
		RegisteredMCPServers []mcpclient.ServerConfig `json:"registered_mcp_servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Item.Name != "community-pack" || resp.Item.Version != "3.1.0" {
		t.Fatalf("unexpected item: %#v", resp.Item)
	}
	if len(resp.RegisteredMCPServers) != 1 {
		t.Fatalf("unexpected registered servers: %#v", resp.RegisteredMCPServers)
	}
	if resp.RegisteredMCPServers[0].ManagedBy != "skill:community-pack" || !resp.RegisteredMCPServers[0].Readonly {
		t.Fatalf("unexpected managed server: %#v", resp.RegisteredMCPServers[0])
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "installed", "community-pack", "tools", "fetch.yaml")); err != nil {
		t.Fatalf("expected installed tool config: %v", err)
	}
}

func TestHandleSkillInstallRegistersMCPServersFromRemoteZip(t *testing.T) {
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")

	zipBody := new(bytes.Buffer)
	zw := zip.NewWriter(zipBody)
	for name, content := range map[string]string{
		"remote-pack-root/skill.yaml": `name: remote-pack
version: 4.0.0
description: remote community package
instructions_file: prompts/guide.md
files:
  - tools/filesystem.yaml
`,
		"remote-pack-root/prompts/guide.md": `Use the filesystem bridge.`,
		"remote-pack-root/tools/filesystem.yaml": `mcp_servers:
  - name: Filesystem Bridge
    command: npx
    args:
      - -y
      - "@modelcontextprotocol/server-filesystem"
      - /tmp
    enabled: true
`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipBody.Bytes())
	}))
	defer remote.Close()

	store, err := db.Open(filepath.Join(root, "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{
		cfg:         config.RuntimeConfig{SkillsDir: skillsDir},
		store:       store,
		skillLoader: skills.NewLoader(skillsDir, ""),
		market:      skills.NewMarket(skillsDir),
		mcpClients:  mcpclient.NewManager(),
	}

	reqBody := map[string]any{"source_url": remote.URL + "/community.zip"}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/skills/install", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleSkillInstall(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Item struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"item"`
		RegisteredMCPServers []mcpclient.ServerConfig `json:"registered_mcp_servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Item.Name != "remote-pack" || resp.Item.Version != "4.0.0" {
		t.Fatalf("unexpected item: %#v", resp.Item)
	}
	if len(resp.RegisteredMCPServers) != 1 {
		t.Fatalf("unexpected registered servers: %#v", resp.RegisteredMCPServers)
	}
	if resp.RegisteredMCPServers[0].ManagedBy != "skill:remote-pack" || !resp.RegisteredMCPServers[0].Readonly {
		t.Fatalf("unexpected managed server: %#v", resp.RegisteredMCPServers[0])
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "installed", "remote-pack", "tools", "filesystem.yaml")); err != nil {
		t.Fatalf("expected installed remote tool config: %v", err)
	}
}

func TestHandleSkillInstallRegistersMCPServersFromRemoteManifest(t *testing.T) {
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/skill.yaml":
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = w.Write([]byte(`name: remote-manifest-pack
version: 4.1.0
description: remote manifest package
prompt_file: prompts/guide.md
files:
  - tools/http.yaml
`))
		case "/prompts/guide.md":
			_, _ = w.Write([]byte("Use the remote manifest bridge."))
		case "/tools/http.yaml":
			_, _ = w.Write([]byte(`name: Remote HTTP Bridge
transport: http
url: http://127.0.0.1:7777/mcp
enabled: true
`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()

	store, err := db.Open(filepath.Join(root, "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{
		cfg:         config.RuntimeConfig{SkillsDir: skillsDir},
		store:       store,
		skillLoader: skills.NewLoader(skillsDir, ""),
		market:      skills.NewMarket(skillsDir),
		mcpClients:  mcpclient.NewManager(),
	}

	reqBody := map[string]any{"source_url": remote.URL + "/skill.yaml"}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/skills/install", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleSkillInstall(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Item struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"item"`
		RegisteredMCPServers []mcpclient.ServerConfig `json:"registered_mcp_servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Item.Name != "remote-manifest-pack" || resp.Item.Version != "4.1.0" {
		t.Fatalf("unexpected item: %#v", resp.Item)
	}
	if len(resp.RegisteredMCPServers) != 1 {
		t.Fatalf("unexpected registered servers: %#v", resp.RegisteredMCPServers)
	}
	if resp.RegisteredMCPServers[0].ManagedBy != "skill:remote-manifest-pack" || !resp.RegisteredMCPServers[0].Readonly {
		t.Fatalf("unexpected managed server: %#v", resp.RegisteredMCPServers[0])
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "installed", "remote-manifest-pack", "tools", "http.yaml")); err != nil {
		t.Fatalf("expected installed remote manifest tool config: %v", err)
	}
}

func TestHandleSkillInstallFromRemoteManifestInstallsRelativeDependencyMCP(t *testing.T) {
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/skill.yaml":
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = w.Write([]byte(`name: remote-parent-pack
version: 5.0.0
description: remote parent package
prompt_file: prompts/guide.md
files:
  - tools/parent.yaml
dependencies:
  - deps/helper/skill.yaml
`))
		case "/prompts/guide.md":
			_, _ = w.Write([]byte("Use the remote parent bridge."))
		case "/tools/parent.yaml":
			_, _ = w.Write([]byte(`name: Parent HTTP Bridge
transport: http
url: http://127.0.0.1:8801/mcp
enabled: true
`))
		case "/deps/helper/skill.yaml":
			_, _ = w.Write([]byte(`name: remote-helper-pack
version: 1.2.0
instructions_file: instructions.md
files:
  - tools/helper.yaml
`))
		case "/deps/helper/instructions.md":
			_, _ = w.Write([]byte("Use the helper bridge."))
		case "/deps/helper/tools/helper.yaml":
			_, _ = w.Write([]byte(`name: Helper HTTP Bridge
transport: http
url: http://127.0.0.1:8802/mcp
enabled: true
`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()

	store, err := db.Open(filepath.Join(root, "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{
		cfg:         config.RuntimeConfig{SkillsDir: skillsDir},
		store:       store,
		skillLoader: skills.NewLoader(skillsDir, ""),
		market:      skills.NewMarket(skillsDir),
		mcpClients:  mcpclient.NewManager(),
	}

	reqBody := map[string]any{"source_url": remote.URL + "/skill.yaml"}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/skills/install", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleSkillInstall(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "installed", "remote-parent-pack", "tools", "parent.yaml")); err != nil {
		t.Fatalf("expected parent tool config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "installed", "remote-helper-pack", "tools", "helper.yaml")); err != nil {
		t.Fatalf("expected dependency tool config: %v", err)
	}

	servers := server.mcpClients.Servers()
	if len(servers) != 2 {
		t.Fatalf("unexpected mcp servers: %#v", servers)
	}
	managed := map[string]bool{}
	for _, item := range servers {
		managed[item.ManagedBy] = true
	}
	if !managed["skill:remote-parent-pack"] || !managed["skill:remote-helper-pack"] {
		t.Fatalf("unexpected managed servers: %#v", servers)
	}
}

func writeSkillZip(path string, files map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}
