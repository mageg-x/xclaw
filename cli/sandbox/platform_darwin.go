//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func applyNativeSandbox(cmd *exec.Cmd, opts nativeSandboxOptions) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return
	}
	profile, err := writeDarwinProfile(opts)
	if err != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return
	}
	origPath := cmd.Path
	origArgs := cmd.Args
	if len(origArgs) == 0 {
		origArgs = []string{origPath}
	}
	wrapped := []string{"sandbox-exec", "-f", profile}
	wrapped = append(wrapped, origPath)
	if len(origArgs) > 1 {
		wrapped = append(wrapped, origArgs[1:]...)
	}
	cmd.Path = "sandbox-exec"
	cmd.Args = wrapped
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func onNativeProcessStarted(pid int, opts nativeSandboxOptions) {
	_ = pid
	_ = opts
}

func writeDarwinProfile(opts nativeSandboxOptions) (string, error) {
	workspace := strings.TrimSpace(opts.WorkspaceDir)
	if workspace == "" {
		workspace = opts.TempRoot
	}
	if workspace == "" {
		workspace = "/tmp"
	}
	workspace = filepath.Clean(workspace)
	profileDir := filepath.Join(opts.TempRoot, "profiles")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return "", err
	}
	profilePath := filepath.Join(profileDir, fmt.Sprintf("darwin-%d.sb", time.Now().UnixNano()))

	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")
	b.WriteString("(import \"system.sb\")\n")
	b.WriteString("(allow process*)\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow file-read* (subpath \"/usr\"))\n")
	b.WriteString("(allow file-read* (subpath \"/System\"))\n")
	b.WriteString("(allow file-read* (subpath \"/Library\"))\n")
	b.WriteString("(allow file-read* (subpath \"/private/tmp\"))\n")

	switch opts.WorkspaceAccess {
	case "rw":
		b.WriteString(fmt.Sprintf("(allow file-read* (subpath \"%s\"))\n", workspace))
		b.WriteString(fmt.Sprintf("(allow file-write* (subpath \"%s\"))\n", workspace))
	case "ro":
		b.WriteString(fmt.Sprintf("(allow file-read* (subpath \"%s\"))\n", workspace))
	case "none":
		// no workspace mount
	default:
		b.WriteString(fmt.Sprintf("(allow file-read* (subpath \"%s\"))\n", workspace))
	}

	if err := os.WriteFile(profilePath, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return profilePath, nil
}
