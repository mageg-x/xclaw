package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	runtimepkg "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xclaw/cli/config"
	"xclaw/cli/updater"
)

type runtimeController struct {
	cfg      config.RuntimeConfig
	rawArgs  []string
	shutdown func()
	drain    func()
	client   *http.Client
	listener net.Listener
}

func newRuntimeController(cfg config.RuntimeConfig, rawArgs []string, shutdown func(), drain func()) *runtimeController {
	return &runtimeController{
		cfg:      cfg,
		rawArgs:  cloneArgs(rawArgs),
		shutdown: shutdown,
		drain:    drain,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *runtimeController) SetListener(listener net.Listener) {
	c.listener = listener
}

func (c *runtimeController) Restart() error {
	handoff, err := c.spawnRestartHelper("")
	if err != nil {
		return err
	}
	if !handoff {
		c.requestShutdown()
	}
	return nil
}

func (c *runtimeController) UpdateAndRestart(ctx context.Context, channel string) (map[string]any, error) {
	repo := strings.TrimSpace(c.cfg.ReleaseRepo)
	if strings.EqualFold(strings.TrimSpace(channel), "beta") {
		repo = "xclaw/agent-beta"
	}
	release, err := updater.FetchLatest(ctx, c.client, repo)
	if err != nil {
		return nil, err
	}
	tag := strings.TrimSpace(release.TagName)
	if tag == "" {
		return nil, fmt.Errorf("release tag is empty")
	}
	if tag == version || tag == "v"+version {
		return map[string]any{
			"ok":             true,
			"updated":        false,
			"already_latest": true,
			"tag":            tag,
		}, nil
	}
	asset, checksum, err := updater.SelectPlatformAsset(release, "", "")
	if err != nil {
		return nil, err
	}
	stageDir := filepath.Join(c.cfg.RunDir, "updates", sanitizeTag(tag))
	binaryPath, err := updater.DownloadAndExtractBinary(ctx, c.client, asset, checksum, stageDir)
	if err != nil {
		return nil, err
	}
	handoff, err := c.spawnRestartHelper(binaryPath)
	if err != nil {
		return nil, err
	}
	if !handoff {
		c.requestShutdown()
	}
	return map[string]any{
		"ok":         true,
		"updated":    true,
		"tag":        tag,
		"asset_name": asset.Name,
		"staged":     binaryPath,
		"restarting": true,
	}, nil
}

func (c *runtimeController) spawnRestartHelper(stagedBinary string) (bool, error) {
	currentExe, err := os.Executable()
	if err != nil {
		return false, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(currentExe); resolveErr == nil {
		currentExe = resolved
	}
	if err := os.MkdirAll(c.cfg.RunDir, 0o755); err != nil {
		return false, err
	}
	helperPath := filepath.Join(c.cfg.RunDir, fmt.Sprintf("xclaw-helper-%d%s", time.Now().UnixNano(), filepath.Ext(currentExe)))
	if err := copyExecutable(currentExe, helperPath); err != nil {
		return false, err
	}
	args := []string{
		"internal-helper-restart",
		"--parent-pid", strconv.Itoa(os.Getpid()),
		"--target-exe", currentExe,
		"--run-dir", c.cfg.RunDir,
	}
	if healthURL := localHealthURL(c.cfg.Server); healthURL != "" {
		args = append(args, "--health-url", healthURL)
	}
	if strings.TrimSpace(stagedBinary) != "" {
		args = append(args, "--staged-exe", stagedBinary)
	}
	args = append(args, "--")
	args = append(args, cloneArgs(c.rawArgs)...)

	logPath := filepath.Join(c.cfg.LogsDir, "restart-helper.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer logFile.Close()

	cmd := exec.Command(helperPath, args...)
	cmd.Env = filterHelperEnv(os.Environ())
	handoff := false
	if listenerFile, ok := exportListenerFile(c.listener); ok {
		defer listenerFile.Close()
		readyFile := handoffReadyPath(c.cfg.RunDir, os.Getpid())
		cmd.ExtraFiles = []*os.File{listenerFile}
		cmd.Env = append(cmd.Env,
			"XCLAW_INHERITED_LISTENER_FD=3",
			"XCLAW_HANDOFF_READY_FILE="+readyFile,
		)
		handoff = true
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return false, err
	}
	return handoff, nil
}

func (c *runtimeController) requestShutdown() {
	if c.drain != nil {
		c.drain()
	}
	if c.shutdown == nil {
		return
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		c.shutdown()
	}()
}

func runInternalHelperRestartCommand(args []string) error {
	fs := flag.NewFlagSet("internal-helper-restart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var parentPID int
	var targetExe string
	var stagedExe string
	var runDir string
	var healthURL string
	fs.IntVar(&parentPID, "parent-pid", 0, "parent process id")
	fs.StringVar(&targetExe, "target-exe", "", "target executable path")
	fs.StringVar(&stagedExe, "staged-exe", "", "staged executable path")
	fs.StringVar(&runDir, "run-dir", "", "runtime run dir")
	fs.StringVar(&healthURL, "health-url", "", "child health probe url")
	if err := fs.Parse(args); err != nil {
		return err
	}
	launchArgs := cloneArgs(fs.Args())
	if parentPID <= 0 || strings.TrimSpace(targetExe) == "" {
		return fmt.Errorf("parent-pid and target-exe are required")
	}
	listenerFile, hasInheritedListener := inheritedListenerFile()
	if hasInheritedListener {
		if strings.TrimSpace(stagedExe) != "" {
			if err := replaceBinary(targetExe, stagedExe); err != nil {
				return err
			}
		}
		readyFile := strings.TrimSpace(os.Getenv("XCLAW_HANDOFF_READY_FILE"))
		if readyFile != "" {
			_ = os.Remove(readyFile)
		}
		childPID, err := spawnRuntimeProcess(targetExe, launchArgs, listenerFile, readyFile)
		if err != nil {
			if readyFile != "" {
				_ = os.Remove(readyFile)
			}
			return err
		}
		if err := waitForReadyFile(readyFile, 20*time.Second); err != nil {
			return err
		}
		if err := waitForHealthyProcess(healthURL, childPID, 20*time.Second); err != nil {
			return err
		}
		if err := markParentDraining(runDir, parentPID); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
		if err := signalParentShutdown(parentPID); err != nil {
			return err
		}
		_ = os.Remove(readyFile)
		_ = cleanupOldHelpers(runDir, filepath.Base(os.Args[0]))
		return nil
	}
	if err := waitForPIDExit(parentPID, 60*time.Second); err != nil {
		return err
	}
	if strings.TrimSpace(stagedExe) != "" {
		if err := replaceBinary(targetExe, stagedExe); err != nil {
			return err
		}
	}
	childPID, err := spawnRuntimeProcess(targetExe, launchArgs, nil, "")
	if err != nil {
		return err
	}
	if err := waitForHealthyProcess(healthURL, childPID, 30*time.Second); err != nil {
		return err
	}
	_ = cleanupOldHelpers(runDir, filepath.Base(os.Args[0]))
	return nil
}

func spawnRuntimeProcess(targetExe string, launchArgs []string, listenerFile *os.File, readyFile string) (int, error) {
	cmd := exec.Command(targetExe, launchArgs...)
	cmd.Env = append(filterHelperEnv(os.Environ()), "XCLAW_RESTART_CHILD=1")
	if listenerFile != nil {
		cmd.ExtraFiles = []*os.File{listenerFile}
		cmd.Env = append(cmd.Env, "XCLAW_INHERITED_LISTENER_FD=3")
	}
	if strings.TrimSpace(readyFile) != "" {
		cmd.Env = append(cmd.Env, "XCLAW_HANDOFF_READY_FILE="+strings.TrimSpace(readyFile))
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func waitForPIDExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !isPIDRunning(pid) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting parent process exit (pid=%d)", pid)
}

func replaceBinary(targetExe, stagedExe string) error {
	targetExe = filepath.Clean(strings.TrimSpace(targetExe))
	stagedExe = filepath.Clean(strings.TrimSpace(stagedExe))
	if targetExe == "" || stagedExe == "" {
		return fmt.Errorf("target executable and staged executable are required")
	}
	backupExe := targetExe + ".bak"
	_ = os.Remove(backupExe)
	if err := moveFile(targetExe, backupExe); err != nil {
		return err
	}
	if err := moveFile(stagedExe, targetExe); err != nil {
		_ = moveFile(backupExe, targetExe)
		return err
	}
	if err := os.Chmod(targetExe, 0o755); err != nil && runtimepkg.GOOS != "windows" {
		return err
	}
	_ = os.Remove(backupExe)
	return nil
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyExecutable(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

func filterHelperEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		if strings.HasPrefix(item, "XCLAW_DAEMON_CHILD=") ||
			strings.HasPrefix(item, "XCLAW_RESTART_CHILD=") ||
			strings.HasPrefix(item, "XCLAW_INHERITED_LISTENER_FD=") ||
			strings.HasPrefix(item, "XCLAW_HANDOFF_READY_FILE=") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func cleanupOldHelpers(runDir, currentHelper string) error {
	if strings.TrimSpace(runDir) == "" {
		return nil
	}
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "xclaw-helper-") || name == currentHelper {
			continue
		}
		_ = os.Remove(filepath.Join(runDir, name))
	}
	return nil
}

func handoffReadyPath(runDir string, parentPID int) string {
	return filepath.Join(runDir, fmt.Sprintf("handoff-%d.ready", parentPID))
}

func handoffDrainPath(runDir string, parentPID int) string {
	return filepath.Join(runDir, fmt.Sprintf("handoff-%d.drain", parentPID))
}

func waitForReadyFile(path string, timeout time.Duration) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting child handoff readiness")
}

func markParentDraining(runDir string, parentPID int) error {
	if strings.TrimSpace(runDir) == "" || parentPID <= 0 {
		return nil
	}
	path := handoffDrainPath(runDir, parentPID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(parentPID)), 0o644)
}

func signalParentShutdown(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid parent pid")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

func exportListenerFile(listener net.Listener) (*os.File, bool) {
	if listener == nil || runtimepkg.GOOS == "windows" {
		return nil, false
	}
	type fileListener interface {
		File() (*os.File, error)
	}
	fl, ok := listener.(fileListener)
	if !ok {
		return nil, false
	}
	file, err := fl.File()
	if err != nil {
		return nil, false
	}
	return file, true
}

func inheritedListenerFile() (*os.File, bool) {
	raw := strings.TrimSpace(os.Getenv("XCLAW_INHERITED_LISTENER_FD"))
	if raw == "" {
		return nil, false
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, false
	}
	return os.NewFile(uintptr(fd), "xclaw-inherited-listener"), true
}

func sanitizeTag(tag string) string {
	tag = strings.TrimSpace(strings.TrimPrefix(tag, "v"))
	tag = strings.ReplaceAll(tag, "/", "-")
	tag = strings.ReplaceAll(tag, "\\", "-")
	if tag == "" {
		return "latest"
	}
	return tag
}

func localHealthURL(serverCfg config.ServerConfig) string {
	host := normalizeLoopbackHost(serverCfg.Host)
	if host == "" || serverCfg.Port <= 0 {
		return ""
	}
	return (&url.URL{
		Scheme: serverCfg.Scheme(),
		Host:   net.JoinHostPort(host, strconv.Itoa(serverCfg.Port)),
		Path:   "/healthz",
	}).String()
}

func normalizeLoopbackHost(host string) string {
	host = strings.TrimSpace(host)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return host
	}
}

func waitForHealthyProcess(healthURL string, expectedPID int, timeout time.Duration) error {
	healthURL = strings.TrimSpace(healthURL)
	if healthURL == "" {
		return nil
	}
	parsed, err := url.Parse(healthURL)
	if err != nil {
		return fmt.Errorf("invalid health url: %w", err)
	}
	client := &http.Client{
		Timeout: 2 * time.Second,
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, healthURL, nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if healthResponseMatches(resp.StatusCode, resp.Header.Get("X-XClaw-PID"), expectedPID) {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if expectedPID > 0 {
		return fmt.Errorf("timed out waiting child health ready (pid=%d)", expectedPID)
	}
	return fmt.Errorf("timed out waiting child health ready")
}

func healthResponseMatches(statusCode int, pidHeader string, expectedPID int) bool {
	if statusCode != http.StatusOK {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(pidHeader))
	return expectedPID <= 0 || pid == expectedPID
}

func cloneArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	return out
}
