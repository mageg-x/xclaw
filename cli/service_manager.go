package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func handleServiceCommand(cmd string, dataDir string) error {
	cmd = strings.TrimSpace(strings.ToLower(cmd))
	if cmd == "" {
		return nil
	}
	switch runtime.GOOS {
	case "linux":
		return handleLinuxService(cmd, dataDir)
	case "darwin":
		return handleDarwinService(cmd, dataDir)
	case "windows":
		return handleWindowsService(cmd, dataDir)
	default:
		return fmt.Errorf("service mode not supported on %s", runtime.GOOS)
	}
}

func handleLinuxService(cmd string, dataDir string) error {
	unitDir := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
	unitPath := filepath.Join(unitDir, "xclaw-agent.service")
	binPath, _ := os.Executable()
	unit := fmt.Sprintf(`[Unit]
Description=XClaw Agent
After=network.target

[Service]
Type=simple
ExecStart=%s --data-dir %s
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`, binPath, dataDir)

	switch cmd {
	case "install":
		if err := os.MkdirAll(unitDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
			return err
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		_ = exec.Command("systemctl", "--user", "enable", "xclaw-agent.service").Run()
		return nil
	case "uninstall":
		_ = exec.Command("systemctl", "--user", "disable", "xclaw-agent.service").Run()
		_ = os.Remove(unitPath)
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		return nil
	case "start", "stop", "restart", "status":
		out, err := exec.Command("systemctl", "--user", cmd, "xclaw-agent.service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %s: %v: %s", cmd, err, strings.TrimSpace(string(out)))
		}
		if cmd == "status" {
			fmt.Println(string(out))
		}
		return nil
	default:
		return fmt.Errorf("unknown service command: %s", cmd)
	}
}

func handleDarwinService(cmd string, dataDir string) error {
	plistDir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	plistPath := filepath.Join(plistDir, "com.xclaw.agent.plist")
	binPath, _ := os.Executable()
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.xclaw.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>--data-dir</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, binPath, dataDir, filepath.Join(dataDir, "launchd.log"), filepath.Join(dataDir, "launchd.err.log"))

	switch cmd {
	case "install":
		if err := os.MkdirAll(plistDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
			return err
		}
		out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl load: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "uninstall":
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		_ = os.Remove(plistPath)
		return nil
	case "start":
		out, err := exec.Command("launchctl", "start", "com.xclaw.agent").CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl start: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "stop":
		out, err := exec.Command("launchctl", "stop", "com.xclaw.agent").CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl stop: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "status":
		out, err := exec.Command("launchctl", "list", "com.xclaw.agent").CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl list: %v: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Println(string(out))
		return nil
	default:
		return fmt.Errorf("unknown service command: %s", cmd)
	}
}

func handleWindowsService(cmd string, dataDir string) error {
	binPath, _ := os.Executable()
	serviceName := "xclaw-agent"
	switch cmd {
	case "install":
		out, err := exec.Command("sc.exe", "create", serviceName, "binPath=", fmt.Sprintf("\"%s\" --data-dir %s", binPath, dataDir), "start=", "auto").CombinedOutput()
		if err != nil {
			return fmt.Errorf("sc create: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "uninstall":
		out, err := exec.Command("sc.exe", "delete", serviceName).CombinedOutput()
		if err != nil {
			return fmt.Errorf("sc delete: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "start", "stop":
		out, err := exec.Command("sc.exe", cmd, serviceName).CombinedOutput()
		if err != nil {
			return fmt.Errorf("sc %s: %v: %s", cmd, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "status":
		out, err := exec.Command("sc.exe", "query", serviceName).CombinedOutput()
		if err != nil {
			return fmt.Errorf("sc query: %v: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Println(string(out))
		return nil
	default:
		return fmt.Errorf("unknown service command: %s", cmd)
	}
}
