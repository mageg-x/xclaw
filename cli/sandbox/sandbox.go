package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"xclaw/cli/config"
)

type Manager struct {
	cfg      config.SandboxConfig
	tempRoot string
	dataDir  string
}

type CommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func NewManager(cfg config.SandboxConfig, dataDir string) *Manager {
	tempRoot := filepath.Join(dataDir, "sandbox")
	_ = os.MkdirAll(tempRoot, 0o755)
	return &Manager{cfg: cfg, tempRoot: tempRoot, dataDir: dataDir}
}

func (m *Manager) Layer() string {
	switch m.cfg.Mode {
	case "off":
		return "host"
	case "custom":
		return "custom"
	default:
		return "native"
	}
}

func (m *Manager) ShouldSandbox(_ bool) bool {
	return m.cfg.Mode != "off"
}

func (m *Manager) CanWriteWorkspace() bool {
	return m.cfg.WorkspaceAccess == "rw"
}

func (m *Manager) CanReadWorkspace() bool {
	return m.cfg.WorkspaceAccess == "ro" || m.cfg.WorkspaceAccess == "rw"
}

func (m *Manager) Exec(ctx context.Context, isMain bool, workspace string, command string, args []string, timeout time.Duration) (CommandResult, error) {
	_ = isMain
	if strings.TrimSpace(command) == "" {
		return CommandResult{}, fmt.Errorf("empty command")
	}

	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := m.buildCommand(cctx, command, args)
	if err != nil {
		return CommandResult{}, err
	}
	cmd.Env = m.baseEnv()
	cmd.Dir = m.resolveWorkingDir(workspace)
	if m.cfg.Mode == "native" {
		applyNativeSandbox(cmd, nativeSandboxOptions{
			WorkspaceDir:    cmd.Dir,
			WorkspaceAccess: m.cfg.WorkspaceAccess,
			TempRoot:        m.tempRoot,
			DataDir:         m.dataDir,
		})
	}

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return CommandResult{}, fmt.Errorf("exec command start: %w", err)
	}
	if m.cfg.Mode == "native" {
		onNativeProcessStarted(cmd.Process.Pid, nativeSandboxOptions{
			WorkspaceDir:    cmd.Dir,
			WorkspaceAccess: m.cfg.WorkspaceAccess,
			TempRoot:        m.tempRoot,
			DataDir:         m.dataDir,
		})
	}
	err = cmd.Wait()
	exitCode := 0
	if err != nil {
		var exErr *exec.ExitError
		if errors.As(err, &exErr) {
			exitCode = exErr.ExitCode()
		} else {
			return CommandResult{}, fmt.Errorf("exec command: %w", err)
		}
	}

	return CommandResult{Stdout: stdoutBuf.String(), Stderr: stderrBuf.String(), ExitCode: exitCode}, nil
}

func (m *Manager) buildCommand(ctx context.Context, command string, args []string) (*exec.Cmd, error) {
	if m.cfg.Mode != "custom" {
		return exec.CommandContext(ctx, command, args...), nil
	}

	custom := strings.TrimSpace(m.cfg.CustomCommand)
	if custom == "" {
		return nil, fmt.Errorf("custom sandbox mode requires custom_command")
	}

	wrapper := strings.Fields(custom)
	if len(wrapper) == 0 {
		return nil, fmt.Errorf("invalid custom sandbox command")
	}

	finalArgs := append(wrapper[1:], command)
	finalArgs = append(finalArgs, args...)
	return exec.CommandContext(ctx, wrapper[0], finalArgs...), nil
}

func (m *Manager) resolveWorkingDir(workspace string) string {
	if m.cfg.Mode == "off" {
		if workspace != "" {
			return workspace
		}
		return "."
	}

	switch m.cfg.WorkspaceAccess {
	case "none":
		isolated := filepath.Join(m.tempRoot, "isolated")
		_ = os.MkdirAll(isolated, 0o755)
		return isolated
	case "ro":
		if workspace == "" {
			return m.tempRoot
		}
		snapshot, err := m.createWorkspaceSnapshot(workspace)
		if err != nil {
			return m.tempRoot
		}
		return snapshot
	case "rw":
		if workspace != "" {
			return workspace
		}
		return m.tempRoot
	default:
		return m.tempRoot
	}
}

func (m *Manager) baseEnv() []string {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"HOME=" + m.tempRoot,
	}
	if runtime.GOOS == "windows" {
		env = []string{"Path=" + os.Getenv("Path")}
	}
	return env
}

func (m *Manager) createWorkspaceSnapshot(workspace string) (string, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return "", fmt.Errorf("workspace is empty")
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("abs workspace: %w", err)
	}
	snapshotRoot := filepath.Join(m.tempRoot, "snapshot", fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(snapshotRoot, 0o755); err != nil {
		return "", fmt.Errorf("create snapshot root: %w", err)
	}
	if err := filepath.WalkDir(absWorkspace, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(absWorkspace, path)
		if err != nil {
			return err
		}
		target := filepath.Join(snapshotRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	}); err != nil {
		return "", fmt.Errorf("copy workspace snapshot: %w", err)
	}
	return snapshotRoot, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
