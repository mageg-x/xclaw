package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSkillDirWithJSONManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := `{
  "id": "github-tools",
  "name": "github-tools",
  "version": "1.2.3",
  "description": "GitHub helper toolkit",
  "allowed_tools": ["mcp:github:list_prs"],
  "trigger_hints": ["github", "pull request"],
  "instructions_file": "instructions.md"
}`
	if err := os.WriteFile(filepath.Join(dir, "skill.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("Use GitHub tools carefully."), 0o644); err != nil {
		t.Fatal(err)
	}

	doc, source, err := ParseSkillDir(dir)
	if err != nil {
		t.Fatalf("ParseSkillDir failed: %v", err)
	}
	if source != filepath.Join(dir, "skill.json") {
		t.Fatalf("unexpected source path: %s", source)
	}
	if doc.Meta.Name != "github-tools" {
		t.Fatalf("unexpected skill name: %s", doc.Meta.Name)
	}
	if doc.Meta.Version != "1.2.3" {
		t.Fatalf("unexpected version: %s", doc.Meta.Version)
	}
	if len(doc.Meta.AllowedTools) != 1 || doc.Meta.AllowedTools[0] != "mcp:github:list_prs" {
		t.Fatalf("unexpected allowed tools: %#v", doc.Meta.AllowedTools)
	}
	if doc.Instructions != "Use GitHub tools carefully." {
		t.Fatalf("unexpected instructions: %q", doc.Instructions)
	}
}

func TestParseSkillDirWithYAMLManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := `
id: filesystem-skill
name: filesystem-skill
version: 0.9.0
description: Access filesystem helpers
allowed_tools: [mcp:fs:list, mcp:fs:read]
trigger_hints: [filesystem, files]
instructions_file: guide.md
`
	if err := os.WriteFile(filepath.Join(dir, "skill.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "guide.md"), []byte("Read before write."), 0o644); err != nil {
		t.Fatal(err)
	}

	doc, source, err := ParseSkillDir(dir)
	if err != nil {
		t.Fatalf("ParseSkillDir failed: %v", err)
	}
	if source != filepath.Join(dir, "skill.yaml") {
		t.Fatalf("unexpected source path: %s", source)
	}
	if doc.Meta.Name != "filesystem-skill" {
		t.Fatalf("unexpected skill name: %s", doc.Meta.Name)
	}
	if len(doc.Meta.AllowedTools) != 2 {
		t.Fatalf("unexpected allowed tools: %#v", doc.Meta.AllowedTools)
	}
}

func TestParseSKILLMdWithMultilineFrontMatter(t *testing.T) {
	content := `---
name: github-review
version: 2.1.0
description: |
  Review GitHub pull requests.
  Focus on bugs and regressions.
allowed-tools:
  - mcp:github:list_prs
  - mcp:github:get_pr
tags:
  - github
  - review
trigger-hints:
  - pull request
  - code review
author: XClaw
---

## Instructions

Be strict.
`
	doc, err := ParseSKILLMd(content)
	if err != nil {
		t.Fatalf("ParseSKILLMd failed: %v", err)
	}
	if doc.Meta.Name != "github-review" {
		t.Fatalf("unexpected name: %s", doc.Meta.Name)
	}
	if doc.Meta.Description != "Review GitHub pull requests.\nFocus on bugs and regressions." {
		t.Fatalf("unexpected description: %q", doc.Meta.Description)
	}
	if len(doc.Meta.AllowedTools) != 2 || doc.Meta.AllowedTools[1] != "mcp:github:get_pr" {
		t.Fatalf("unexpected allowed tools: %#v", doc.Meta.AllowedTools)
	}
	if len(doc.Meta.TriggerHints) != 2 || doc.Meta.TriggerHints[0] != "pull request" {
		t.Fatalf("unexpected trigger hints: %#v", doc.Meta.TriggerHints)
	}
	if doc.Instructions == "" {
		t.Fatal("expected instructions")
	}
}

func TestParseSkillManifestWithMultilineYAML(t *testing.T) {
	dir := t.TempDir()
	manifest := `
id: browser-ops
version: 1.0.0
description: >
  Browser automation
  and page inspection.
allowed_tools:
  - mcp:browser:navigate
  - mcp:browser:click
dependencies:
  - core-utils@^1.2.0
  - page-parser@>=2.0.0
prompt_file: instructions.md
`
	if err := os.WriteFile(filepath.Join(dir, "skill.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("Inspect carefully."), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, source, err := ParseSkillDir(dir)
	if err != nil {
		t.Fatalf("ParseSkillDir failed: %v", err)
	}
	if source != filepath.Join(dir, "skill.yml") {
		t.Fatalf("unexpected source: %s", source)
	}
	if doc.Meta.Name != "browser-ops" {
		t.Fatalf("expected name to fall back to id, got %s", doc.Meta.Name)
	}
	if doc.Meta.Description != "Browser automation and page inspection." {
		t.Fatalf("unexpected description: %q", doc.Meta.Description)
	}
	if len(doc.Meta.AllowedTools) != 2 || doc.Meta.AllowedTools[0] != "mcp:browser:navigate" {
		t.Fatalf("unexpected allowed tools: %#v", doc.Meta.AllowedTools)
	}
	if doc.Instructions != "Inspect carefully." {
		t.Fatalf("unexpected instructions: %q", doc.Instructions)
	}
}
