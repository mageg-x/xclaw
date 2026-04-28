package harness

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ValidationResult represents the result of a single validation step
type ValidationResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Output  string `json:"output"`
	Error   string `json:"error,omitempty"`
	Elapsed int64  `json:"elapsed_ms"`
}

// Pipeline runs deterministic validation checks before Agent actions
type Pipeline struct {
	workspace string
}

// NewPipeline creates a validation pipeline for a workspace
func NewPipeline(workspace string) *Pipeline {
	return &Pipeline{workspace: workspace}
}

// Run executes all applicable validation steps for the workspace
func (p *Pipeline) Run(ctx context.Context) []ValidationResult {
	results := make([]ValidationResult, 0)

	// Check 1: Lint (if config exists)
	if p.hasFile(".golangci.yml") || p.hasFile(".eslintrc") || p.hasFile("pyproject.toml") {
		results = append(results, p.runLint(ctx))
	}

	// Check 2: Type Check
	if p.hasFile("go.mod") {
		results = append(results, p.runGoTypeCheck(ctx))
	} else if p.hasFile("package.json") {
		results = append(results, p.runTSTypeCheck(ctx))
	} else if p.hasFile("requirements.txt") || p.hasFile("pyproject.toml") {
		results = append(results, p.runPyTypeCheck(ctx))
	}

	// Check 3: Unit Tests
	if p.hasFile("go.mod") || p.hasFile("package.json") || p.hasFile("pytest.ini") || p.hasFile("pyproject.toml") {
		results = append(results, p.runTests(ctx))
	}

	// Check 4: Security Scan (basic)
	if p.hasFile("go.mod") {
		results = append(results, p.runGoVulnCheck(ctx))
	}

	return results
}

// ShouldBlock returns true if any critical validation failed
func ShouldBlock(results []ValidationResult) bool {
	for _, r := range results {
		if !r.Passed && isCritical(r.Name) {
			return true
		}
	}
	return false
}

func isCritical(name string) bool {
	return strings.Contains(name, "test") || strings.Contains(name, "type")
}

func (p *Pipeline) hasFile(name string) bool {
	fp := filepath.Join(p.workspace, name)
	_, err := exec.LookPath("stat")
	if err != nil {
		// Fallback: try to read
		_, err = exec.Command("test", "-f", fp).Output()
		return err == nil
	}
	_, err = exec.Command("stat", fp).Output()
	return err == nil
}

func (p *Pipeline) runLint(ctx context.Context) ValidationResult {
	start := time.Now()
	var cmd *exec.Cmd

	switch {
	case p.hasFile("go.mod"):
		cmd = exec.CommandContext(ctx, "golangci-lint", "run", "--fast")
	case p.hasFile("package.json"):
		cmd = exec.CommandContext(ctx, "npx", "eslint", ".")
	case p.hasFile("pyproject.toml"):
		cmd = exec.CommandContext(ctx, "ruff", "check", ".")
	default:
		return ValidationResult{Name: "lint", Passed: true, Output: "no linter configured"}
	}

	cmd.Dir = p.workspace
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		return ValidationResult{Name: "lint", Passed: false, Output: string(out), Error: err.Error(), Elapsed: elapsed}
	}
	return ValidationResult{Name: "lint", Passed: true, Output: string(out), Elapsed: elapsed}
}

func (p *Pipeline) runGoTypeCheck(ctx context.Context) ValidationResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, "go", "build", "./...")
	cmd.Dir = p.workspace
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		return ValidationResult{Name: "type-check", Passed: false, Output: string(out), Error: err.Error(), Elapsed: elapsed}
	}
	return ValidationResult{Name: "type-check", Passed: true, Output: "go build ok", Elapsed: elapsed}
}

func (p *Pipeline) runTSTypeCheck(ctx context.Context) ValidationResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, "npx", "tsc", "--noEmit")
	cmd.Dir = p.workspace
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		return ValidationResult{Name: "type-check", Passed: false, Output: string(out), Error: err.Error(), Elapsed: elapsed}
	}
	return ValidationResult{Name: "type-check", Passed: true, Output: "tsc ok", Elapsed: elapsed}
}

func (p *Pipeline) runPyTypeCheck(ctx context.Context) ValidationResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, "mypy", ".")
	cmd.Dir = p.workspace
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		return ValidationResult{Name: "type-check", Passed: false, Output: string(out), Error: err.Error(), Elapsed: elapsed}
	}
	return ValidationResult{Name: "type-check", Passed: true, Output: "mypy ok", Elapsed: elapsed}
}

func (p *Pipeline) runTests(ctx context.Context) ValidationResult {
	start := time.Now()
	var cmd *exec.Cmd

	switch {
	case p.hasFile("go.mod"):
		cmd = exec.CommandContext(ctx, "go", "test", "./...", "-short")
	case p.hasFile("package.json"):
		cmd = exec.CommandContext(ctx, "npm", "test", "--if-present")
	case p.hasFile("pytest.ini") || p.hasFile("pyproject.toml"):
		cmd = exec.CommandContext(ctx, "pytest", "-x", "-q")
	default:
		return ValidationResult{Name: "test", Passed: true, Output: "no test runner configured"}
	}

	cmd.Dir = p.workspace
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		return ValidationResult{Name: "test", Passed: false, Output: string(out), Error: err.Error(), Elapsed: elapsed}
	}
	return ValidationResult{Name: "test", Passed: true, Output: string(out), Elapsed: elapsed}
}

func (p *Pipeline) runGoVulnCheck(ctx context.Context) ValidationResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, "govulncheck", "./...")
	cmd.Dir = p.workspace
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		// govulncheck exits 0 on no vulnerabilities, non-zero if found or error
		output := string(out)
		if strings.Contains(output, "No vulnerabilities found") {
			return ValidationResult{Name: "security-scan", Passed: true, Output: output, Elapsed: elapsed}
		}
		return ValidationResult{Name: "security-scan", Passed: false, Output: output, Error: err.Error(), Elapsed: elapsed}
	}
	return ValidationResult{Name: "security-scan", Passed: true, Output: string(out), Elapsed: elapsed}
}

// FormatResults formats validation results for LLM consumption
func FormatResults(results []ValidationResult) string {
	var b strings.Builder
	b.WriteString("## 自动化验证结果\n\n")
	allPassed := true
	for _, r := range results {
		status := "✅ 通过"
		if !r.Passed {
			status = "❌ 失败"
			allPassed = false
		}
		b.WriteString(fmt.Sprintf("### %s %s (%dms)\n", status, r.Name, r.Elapsed))
		if r.Output != "" {
			b.WriteString(fmt.Sprintf("```\n%s\n```\n", strings.TrimSpace(r.Output)))
		}
		if r.Error != "" {
			b.WriteString(fmt.Sprintf("错误: %s\n", r.Error))
		}
		b.WriteString("\n")
	}
	if allPassed {
		b.WriteString("**所有检查通过，可以安全提交。**\n")
	} else {
		b.WriteString("**部分检查失败，请修复后再提交。**\n")
	}
	return b.String()
}
