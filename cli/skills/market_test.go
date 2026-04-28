package skills

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallFromLocalManifestFile(t *testing.T) {
	sourceDir := t.TempDir()
	manifest := SkillPackageManifest{
		Name:             "github-pack",
		Version:          "2.1.0",
		Description:      "GitHub toolkit",
		InstructionsFile: "instructions.md",
		Files:            []string{"tools/github.json", "README.md"},
		Dependencies:     []string{"deps/dep-a"},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "skill.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "instructions.md"), []byte("Use GitHub MCP carefully."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceDir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "tools", "github.json"), []byte(`{"server":"github"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("readme"), 0o644); err != nil {
		t.Fatal(err)
	}
	depDir := filepath.Join(sourceDir, "deps", "dep-a")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "SKILL.md"), []byte(`---
name: dep-a
version: 1.0.0
description: dependency
---

Use dependency.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	skillsRoot := t.TempDir()
	market := NewMarket(skillsRoot)
	item, err := market.Install(InstallOptions{
		SourcePath: filepath.Join(sourceDir, "skill.json"),
	})
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if item.Name != "github-pack" {
		t.Fatalf("unexpected installed skill name: %s", item.Name)
	}
	if item.Version != "2.1.0" {
		t.Fatalf("unexpected version: %s", item.Version)
	}
	if item.Source != "local" {
		t.Fatalf("unexpected source: %s", item.Source)
	}
	if item.Integrity == "" {
		t.Fatalf("expected integrity to be recorded")
	}

	skillDir := filepath.Join(skillsRoot, "installed", "github-pack")
	for _, rel := range []string{"skill.json", "instructions.md", "tools/github.json", "README.md", installLockName} {
		if _, err := os.Stat(filepath.Join(skillDir, rel)); err != nil {
			t.Fatalf("expected installed file %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(skillsRoot, "installed", "dep-a", "SKILL.md")); err != nil {
		t.Fatalf("expected dependency skill to be installed: %v", err)
	}
}

func TestInstallDependencyWithVersionConstraintFromMarket(t *testing.T) {
	sourceDir := t.TempDir()
	manifest := SkillPackageManifest{
		Name:             "github-pack",
		Version:          "2.1.0",
		InstructionsFile: "instructions.md",
		Dependencies:     []string{"dep-a@>=1.5.0,<2.0.0"},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "skill.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "instructions.md"), []byte("Use GitHub MCP carefully."), 0o644); err != nil {
		t.Fatal(err)
	}

	skillsRoot := t.TempDir()
	market := NewMarket(skillsRoot)
	market.remoteURL = "https://skills.example/catalog.json"
	market.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://skills.example/catalog.json":
				body, _ := json.Marshal([]Item{
					{Name: "dep-a", Version: "1.4.0", SkillURL: "https://skills.example/dep-a-1.4.0.json"},
					{Name: "dep-a", Version: "1.6.0", SkillURL: "https://skills.example/dep-a-1.6.0.json"},
					{Name: "dep-a", Version: "2.0.0", SkillURL: "https://skills.example/dep-a-2.0.0.json"},
				})
				return jsonResponse(body), nil
			case "https://skills.example/dep-a-1.4.0.json":
				body, _ := json.Marshal(SkillPackageManifest{Name: "dep-a", Version: "1.4.0"})
				return jsonResponse(body), nil
			case "https://skills.example/dep-a-1.6.0.json":
				body, _ := json.Marshal(SkillPackageManifest{Name: "dep-a", Version: "1.6.0"})
				return jsonResponse(body), nil
			case "https://skills.example/dep-a-2.0.0.json":
				body, _ := json.Marshal(SkillPackageManifest{Name: "dep-a", Version: "2.0.0"})
				return jsonResponse(body), nil
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("not found")),
				}, nil
			}
		}),
	}

	if _, err := market.Install(InstallOptions{
		SourcePath: filepath.Join(sourceDir, "skill.json"),
	}); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	doc, _, err := ParseSkillDir(filepath.Join(skillsRoot, "installed", "dep-a"))
	if err != nil {
		t.Fatalf("parse installed dependency: %v", err)
	}
	if doc.Meta.Version != "1.6.0" {
		t.Fatalf("expected dependency version 1.6.0, got %s", doc.Meta.Version)
	}
}

func TestInstallFromLocalZipSkillPackageWithManifestAliases(t *testing.T) {
	sourceDir := t.TempDir()
	depDir := filepath.Join(sourceDir, "deps", "dep-a")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "SKILL.md"), []byte(`---
name: dep-a
version: 1.0.0
description: dependency
---

Use dependency.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(sourceDir, "github-pack.zip")
	zipFiles := map[string]string{
		"skill.yaml": `name: github-pack
version: 2.2.0
description: >
  GitHub toolkit
  for agentskills compatibility.
prompt_file: prompts/guide.md
files:
  - tools/github.json
  - README.md
dependencies:
  - deps/dep-a
allowed_tools:
  - mcp:github:list_prs
tags:
  - github
  - automation
trigger_hints:
  - pull requests
  - issues
`,
		"prompts/guide.md":  "Use GitHub MCP carefully.\n",
		"tools/github.json": `{"server":"github","command":"npx","args":["-y","@modelcontextprotocol/server-github"]}`,
		"README.md":         "GitHub Pack\n",
	}
	if err := writeZip(zipPath, zipFiles); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	skillsRoot := t.TempDir()
	market := NewMarket(skillsRoot)
	item, err := market.Install(InstallOptions{SourcePath: zipPath})
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if item.Name != "github-pack" {
		t.Fatalf("unexpected installed skill name: %s", item.Name)
	}
	if item.Version != "2.2.0" {
		t.Fatalf("unexpected version: %s", item.Version)
	}
	if item.Integrity == "" {
		t.Fatalf("expected integrity to be recorded")
	}

	skillDir := filepath.Join(skillsRoot, "installed", "github-pack")
	doc, source, err := ParseSkillDir(skillDir)
	if err != nil {
		t.Fatalf("parse installed skill: %v", err)
	}
	if filepath.Base(source) != "skill.yaml" {
		t.Fatalf("unexpected skill source: %s", source)
	}
	if doc.Meta.Name != "github-pack" {
		t.Fatalf("unexpected parsed skill name: %s", doc.Meta.Name)
	}
	if doc.Instructions != "Use GitHub MCP carefully." {
		t.Fatalf("unexpected parsed instructions: %q", doc.Instructions)
	}

	lockBody, err := os.ReadFile(filepath.Join(skillDir, installLockName))
	if err != nil {
		t.Fatalf("read install lock: %v", err)
	}
	var lock InstallLock
	if err := json.Unmarshal(lockBody, &lock); err != nil {
		t.Fatalf("decode install lock: %v", err)
	}
	if lock.Manifest == nil {
		t.Fatalf("expected lock manifest")
	}
	if lock.Manifest.InstructionsFile != "prompts/guide.md" {
		t.Fatalf("unexpected instructions file: %s", lock.Manifest.InstructionsFile)
	}
	if len(lock.Manifest.Dependencies) != 1 || lock.Manifest.Dependencies[0] != "deps/dep-a" {
		t.Fatalf("unexpected dependencies: %#v", lock.Manifest.Dependencies)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "tools", "github.json")); err != nil {
		t.Fatalf("expected installed tool asset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsRoot, "installed", "dep-a", "SKILL.md")); err != nil {
		t.Fatalf("expected dependency skill to be installed: %v", err)
	}
}

func TestInstallFromZipSkillPackageWithSingleRootFolder(t *testing.T) {
	sourceDir := t.TempDir()
	zipPath := filepath.Join(sourceDir, "community-pack.zip")
	zipFiles := map[string]string{
		"community-pack-main/skill.yaml": `name: community-pack
version: 3.0.0
description: community style package
instructions_file: prompts/guide.md
files:
  - tools/fetch.yaml
`,
		"community-pack-main/prompts/guide.md": `Use the fetch bridge.`,
		"community-pack-main/tools/fetch.yaml": `mcp_servers:
  - name: Fetch Bridge
    command: npx
    args:
      - -y
      - "@modelcontextprotocol/server-fetch"
    enabled: true
`,
	}
	if err := writeZip(zipPath, zipFiles); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	skillsRoot := t.TempDir()
	market := NewMarket(skillsRoot)
	item, err := market.Install(InstallOptions{SourcePath: zipPath})
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if item.Name != "community-pack" {
		t.Fatalf("unexpected installed skill name: %s", item.Name)
	}

	skillDir := filepath.Join(skillsRoot, "installed", "community-pack")
	if _, err := os.Stat(filepath.Join(skillDir, "skill.yaml")); err != nil {
		t.Fatalf("expected flattened manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "tools", "fetch.yaml")); err != nil {
		t.Fatalf("expected flattened tool config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "community-pack-main")); !os.IsNotExist(err) {
		t.Fatalf("unexpected nested root folder after install: %v", err)
	}

	doc, _, err := ParseSkillDir(skillDir)
	if err != nil {
		t.Fatalf("parse installed skill: %v", err)
	}
	if doc.Instructions != "Use the fetch bridge." {
		t.Fatalf("unexpected instructions: %q", doc.Instructions)
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func jsonResponse(body []byte) *http.Response {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
	resp.Header.Set("Content-Type", "application/json")
	return resp
}

func writeZip(path string, files map[string]string) error {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, content); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
