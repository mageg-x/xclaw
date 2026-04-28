package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	runtimepkg "runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"xclaw/cli/api"
	"xclaw/cli/app"
	"xclaw/cli/approval"
	"xclaw/cli/audit"
	"xclaw/cli/certs"
	"xclaw/cli/config"
	"xclaw/cli/db"
	"xclaw/cli/engine"
	"xclaw/cli/gateway"
	"xclaw/cli/heartbeat"
	"xclaw/cli/llm"
	"xclaw/cli/mcpclient"
	"xclaw/cli/mcpregistry"
	"xclaw/cli/queue"
	"xclaw/cli/sandbox"
	"xclaw/cli/scheduler"
	"xclaw/cli/security"
	"xclaw/cli/tools"
	"xclaw/cli/updater"
	"xclaw/cli/workspace"
)

const version = "1.0.0"

type runtimeOptions struct {
	RootDir     string
	DataDir     string
	Host        string
	Port        int
	TLSOverride *bool
	Daemon      bool
	CheckUpdate bool
	ServiceCmd  string
}

type runtimeState struct {
	PID                int       `json:"pid"`
	Version            string    `json:"version"`
	Host               string    `json:"host"`
	Port               int       `json:"port"`
	StartedAt          time.Time `json:"started_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	Draining           bool      `json:"draining"`
	ActiveRequests     int64     `json:"active_requests"`
	ActiveSSE          int64     `json:"active_sse"`
	ActiveWebSockets   int64     `json:"active_ws"`
	MemoryAllocBytes   uint64    `json:"memory_alloc_bytes"`
	MemorySysBytes     uint64    `json:"memory_sys_bytes"`
	AgentCount         int       `json:"agent_count"`
	SessionCount       int       `json:"session_count"`
	RunningSession     int       `json:"running_session_count"`
	LastHeartbeatAt    string    `json:"last_heartbeat_at"`
	LastHeartbeatState string    `json:"last_heartbeat_state"`
}

type optionalBoolFlag struct {
	set   bool
	value bool
}

func (f *optionalBoolFlag) String() string {
	if f == nil {
		return ""
	}
	if f.value {
		return "true"
	}
	return "false"
}

func (f *optionalBoolFlag) Set(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		f.value = true
		f.set = true
		return nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return err
	}
	f.value = v
	f.set = true
	return nil
}

func (f *optionalBoolFlag) IsBoolFlag() bool { return true }

func main() {
	if shouldDefaultToTUI(os.Args[1:]) {
		if err := runTUICommand(nil); err == nil {
			return
		}
	}
	handled, err := runCommand(os.Args[1:])
	if handled {
		if err != nil {
			log.Fatalf("command failed: %v", err)
		}
		return
	}

	opts, err := parseLegacyFlags(os.Args[1:])
	if err != nil {
		log.Fatalf("parse flags: %v", err)
	}
	if err := runRuntime(opts, os.Args[1:]); err != nil {
		log.Fatalf("runtime failed: %v", err)
	}
}

func shouldDefaultToTUI(args []string) bool {
	if len(args) > 0 {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("XCLAW_NO_TUI")), "1") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("XCLAW_FORCE_TUI")), "1") {
		return true
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	if runtimepkg.GOOS == "windows" {
		return true
	}
	return strings.TrimSpace(os.Getenv("DISPLAY")) == "" && strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) == ""
}

func parseLegacyFlags(args []string) (runtimeOptions, error) {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	opts := runtimeOptions{}
	tlsFlag := &optionalBoolFlag{}
	fs.StringVar(&opts.RootDir, "root-dir", "", "root directory")
	fs.StringVar(&opts.DataDir, "data-dir", "", "data directory")
	fs.StringVar(&opts.Host, "host", "", "http host")
	fs.IntVar(&opts.Port, "port", 0, "server port")
	fs.Var(tlsFlag, "tls", "enable https/tls mode")
	fs.BoolVar(&opts.Daemon, "daemon", false, "run as background service style process")
	fs.BoolVar(&opts.CheckUpdate, "check-update", true, "check GitHub releases in background")
	fs.StringVar(&opts.ServiceCmd, "service", "", "service control command: install|uninstall|start|stop|restart|status")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, nil
		}
		return opts, err
	}
	if tlsFlag.set {
		opts.TLSOverride = &tlsFlag.value
	}
	return opts, nil
}

func runRuntime(opts runtimeOptions, rawArgs []string) error {
	rootDir := strings.TrimSpace(opts.RootDir)
	if rootDir == "" {
		rootDir = strings.TrimSpace(opts.DataDir)
	}
	if rootDir == "" {
		rootDir = strings.TrimSpace(os.Getenv("XCLAW_ROOT_DIR"))
	}
	if rootDir == "" {
		rootDir = strings.TrimSpace(os.Getenv("XCLAW_DATA_DIR"))
	}

	cfg, err := config.LoadOrInit(rootDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	applyEnvOverrides(&cfg)

	if opts.Host != "" {
		cfg.Server.Host = opts.Host
	}
	if opts.Port > 0 {
		cfg.Server.Port = opts.Port
	}
	if opts.TLSOverride != nil {
		cfg.Server.TLS = *opts.TLSOverride
	}
	if strings.TrimSpace(opts.ServiceCmd) != "" {
		if err := handleServiceCommand(opts.ServiceCmd, cfg.RootDir); err != nil {
			return fmt.Errorf("service command failed: %w", err)
		}
		log.Printf("service command executed: %s", opts.ServiceCmd)
		return nil
	}
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	if opts.Daemon && os.Getenv("XCLAW_DAEMON_CHILD") != "1" {
		if err := spawnDaemon(cfg.LogsDir, rawArgs); err != nil {
			return fmt.Errorf("start daemon process: %w", err)
		}
		log.Printf("daemon process started, logs: %s", filepath.Join(cfg.LogsDir, "agent-daemon.log"))
		return nil
	}

	if err := acquireRuntimePID(cfg.RunDir); err != nil {
		return err
	}
	defer releaseRuntimePID(cfg.RunDir)

	certPair := certs.Pair{}
	if cfg.Server.TLS {
		certPair, err = certs.Ensure(cfg.ConfigDir)
		if err != nil {
			return fmt.Errorf("ensure cert: %w", err)
		}
	}

	sqlitePath := filepath.Join(cfg.DataDir, "xclaw.db")
	store, err := db.Open(sqlitePath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()

	ws := workspace.NewManager(cfg.WorkspaceDir)
	laneQueue := queue.NewLaneQueue(cfg.Queue.GlobalConcurrency)
	defer laneQueue.Close()

	sb := sandbox.NewManager(cfg.Sandbox, cfg.DataDir)
	approver := approval.NewManager()
	toolExec := tools.NewExecutor(sb, approver)
	toolExec.SetCronStore(store, "")
	mcpMgr := mcpclient.NewManager()
	if servers, err := mcpregistry.LoadManual(context.Background(), store); err == nil {
		if merged, mergeErr := mcpregistry.MergeWithSkillServers(cfg.SkillsDir, servers); mergeErr == nil {
			mcpMgr.SetServers(merged)
		}
	}
	toolExec.SetMCPManager(mcpMgr)

	auditLogger := audit.NewLogger(store)
	eventHub := engine.NewEventHub()
	planCache := engine.NewPlanCache(store)

	tokenMonitor := llm.NewTokenMonitor()
	rawGateway := llm.NewLocalGateway()
	configureCloudProviders(rawGateway)
	llmGateway := llm.NewMonitoredGateway(rawGateway, tokenMonitor)

	engineSvc := engine.NewService(store, ws, laneQueue, toolExec, auditLogger, eventHub, planCache, llmGateway)
	gatewaySvc, err := gateway.New(store, auditLogger, laneQueue)
	if err != nil {
		return fmt.Errorf("create gateway service: %w", err)
	}
	gatewaySvc.Start(context.Background())
	defer gatewaySvc.Stop(context.Background())
	engineSvc.SetPresenceEmitter(presenceBridge{gateway: gatewaySvc})
	heartbeatRunner := heartbeat.NewRunner(store, engineSvc, gatewaySvc, auditLogger)

	keyring := security.NewKeyring(filepath.Join(cfg.ConfigDir, "keyring.dat"))
	appSvc := app.NewService(store, cfg.Security.PBKDF2Iterations, cfg.Security.KeyBytes, keyring)

	apiServer, err := api.NewServer(cfg, store, appSvc, engineSvc, eventHub, tokenMonitor, approver, rawGateway, gatewaySvc, heartbeatRunner, mcpMgr)
	if err != nil {
		return fmt.Errorf("create api server: %w", err)
	}
	defer apiServer.Close()

	cronScheduler := scheduler.New(store, laneQueue, auditLogger, apiServer.RunCronJob)
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var draining atomic.Bool
	startDraining := func() {
		if draining.CompareAndSwap(false, true) {
			log.Printf("runtime entering draining mode")
		}
	}
	controller := newRuntimeController(cfg, rawArgs, cancel, startDraining)
	apiServer.SetRuntimeControlHooks(api.RuntimeControlHooks{
		Restart: controller.Restart,
		Update:  controller.UpdateAndRestart,
	})
	apiServer.SetDrainCheck(func() bool { return draining.Load() })
	cronScheduler.Start(rootCtx)
	heartbeatRunner.Start(rootCtx)
	startRuntimeReporter(rootCtx, cfg, store, func() runtimeSnapshotMeta {
		return runtimeSnapshotMeta{
			Draining:       draining.Load(),
			ActiveRequests: apiServer.ActiveRequestCount(),
			ActiveSSE:      apiServer.ActiveSSECount(),
			ActiveWS:       apiServer.ActiveWebSocketCount(),
		}
	})
	startDrainFileWatcher(rootCtx, cfg.RunDir, os.Getpid(), startDraining)

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	listener, err := prepareServerListener(srv.Addr)
	if err != nil {
		return fmt.Errorf("listen server: %w", err)
	}
	controller.SetListener(listener)
	if err := markHandoffReady(); err != nil {
		log.Printf("handoff ready mark failed: %v", err)
	}

	go func() {
		log.Printf("xclaw runtime started")
		log.Printf("version: %s", version)
		log.Printf("dashboard: %s://%s:%d", cfg.Server.Scheme(), cfg.Server.Host, cfg.Server.Port)
		log.Printf("sandbox layer: %s", sb.Layer())
		if cfg.Server.TLS {
			log.Printf("cert trust hint: %s", config.TrustInstruction())
		}
		if opts.Daemon {
			log.Printf("daemon mode enabled")
		}

		if cfg.Server.TLS {
			if err := srv.ServeTLS(listener, certPair.CertPath, certPair.KeyPath); err != nil && err != http.ErrServerClosed {
				log.Printf("server failed: %v", err)
				cancel()
			}
			return
		}
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("server failed: %v", err)
			cancel()
		}
	}()

	go func() {
		time.Sleep(300 * time.Millisecond)
		resumed, err := engineSvc.RecoverInterruptedSessions(rootCtx)
		if err != nil {
			log.Printf("recover interrupted sessions failed: %v", err)
			return
		}
		if resumed > 0 {
			log.Printf("recovered %d interrupted pending message(s)", resumed)
		}
	}()

	if opts.CheckUpdate {
		go checkRelease(cfg.ReleaseRepo)
	}

	waitExit(cancel, startDraining)
	waitForHTTPDrain(func() int64 { return apiServer.ActiveRequestCount() }, 15*time.Second)

	ctx, stop := context.WithTimeout(context.Background(), 60*time.Second)
	defer stop()
	_ = srv.Shutdown(ctx)
	cronScheduler.Stop()
	heartbeatRunner.Stop()
	return nil
}

type presenceBridge struct {
	gateway *gateway.Gateway
}

func (p presenceBridge) Emit(ctx context.Context, agentID, sessionID, state, message string) {
	if p.gateway == nil {
		return
	}
	_ = p.gateway.SendPresence(ctx, agentID, "last", gateway.PresenceEvent{
		State:   state,
		Message: message,
		TTLMs:   8000,
	})
}

func applyEnvOverrides(cfg *config.RuntimeConfig) {
	if v := strings.TrimSpace(os.Getenv("XCLAW_HOST")); v != "" {
		cfg.Server.Host = v
	}
	if v := strings.TrimSpace(os.Getenv("XCLAW_PORT")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			cfg.Server.Port = p
		}
	}
	if v := strings.TrimSpace(os.Getenv("XCLAW_TLS")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			cfg.Server.TLS = true
		case "0", "false", "no", "off":
			cfg.Server.TLS = false
		}
	}
	if v := strings.TrimSpace(os.Getenv("XCLAW_SANDBOX_MODE")); v != "" {
		cfg.Sandbox.Mode = v
	}
	if v := strings.TrimSpace(os.Getenv("XCLAW_WORKSPACE_ACCESS")); v != "" {
		cfg.Sandbox.WorkspaceAccess = v
	}
	if v := strings.TrimSpace(os.Getenv("XCLAW_SANDBOX_SCOPE")); v != "" {
		cfg.Sandbox.Scope = v
	}
	if v := strings.TrimSpace(os.Getenv("XCLAW_SANDBOX_CUSTOM_COMMAND")); v != "" {
		cfg.Sandbox.CustomCommand = v
	}
	if v := strings.TrimSpace(os.Getenv("XCLAW_RELEASE_REPO")); v != "" {
		cfg.ReleaseRepo = v
	}
}

func configureCloudProviders(g *llm.LocalGateway) {
	type providerEnv struct {
		Name        string
		KeyEnv      string
		BaseURLEnv  string
		DefaultBase string
	}
	providers := []providerEnv{
		{Name: "openai", KeyEnv: "OPENAI_API_KEY", BaseURLEnv: "OPENAI_BASE_URL", DefaultBase: "https://api.openai.com"},
		{Name: "anthropic", KeyEnv: "ANTHROPIC_API_KEY", BaseURLEnv: "ANTHROPIC_BASE_URL", DefaultBase: "https://api.anthropic.com"},
		{Name: "deepseek", KeyEnv: "DEEPSEEK_API_KEY", BaseURLEnv: "DEEPSEEK_BASE_URL", DefaultBase: "https://api.deepseek.com"},
	}
	for _, p := range providers {
		key := strings.TrimSpace(os.Getenv(p.KeyEnv))
		if key == "" {
			continue
		}
		base := strings.TrimSpace(os.Getenv(p.BaseURLEnv))
		if base == "" {
			base = p.DefaultBase
		}
		g.RegisterProvider(p.Name, llm.ProviderConfig{
			BaseURL: base,
			APIKey:  key,
		})
		log.Printf("provider registered from env: %s (%s)", p.Name, p.BaseURLEnv)
	}
}

func spawnDaemon(logDir string, rawArgs []string) error {
	args := make([]string, 0, len(rawArgs))
	for _, arg := range rawArgs {
		if arg == "--daemon" {
			continue
		}
		args = append(args, arg)
	}

	logPath := filepath.Join(logDir, "agent-daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open daemon log file: %w", err)
	}

	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "XCLAW_DAEMON_CHILD=1")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("spawn child: %w", err)
	}
	_ = logFile.Close()
	return nil
}

func waitExit(cancel context.CancelFunc, startDraining func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			if startDraining != nil {
				startDraining()
			}
			cancel()
			return
		}
	}
}

func checkRelease(repo string) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	release, err := updater.FetchLatest(ctx, http.DefaultClient, repo)
	if err != nil {
		log.Printf("update check skipped: %v", err)
		return
	}
	if strings.TrimSpace(release.TagName) != "" && release.TagName != "v"+version && release.TagName != version {
		log.Printf("new release detected: %s (%s)", release.TagName, release.HTMLURL)
	}
}

func runtimePIDPath(runDir string) string {
	return filepath.Join(runDir, "agent.pid")
}

func runtimeStatePath(runDir string) string {
	return filepath.Join(runDir, "runtime-status.json")
}

func acquireRuntimePID(runDir string) error {
	pidPath := runtimePIDPath(runDir)
	allowHandoff := strings.EqualFold(strings.TrimSpace(os.Getenv("XCLAW_RESTART_CHILD")), "1")
	if b, err := os.ReadFile(pidPath); err == nil {
		if oldPID, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && oldPID > 0 && isPIDRunning(oldPID) && !allowHandoff {
			return fmt.Errorf("agent is already running (pid=%d)", oldPID)
		}
	}
	return os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func releaseRuntimePID(runDir string) {
	pidPath := runtimePIDPath(runDir)
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		_ = os.Remove(pidPath)
		return
	}
	if pid == os.Getpid() {
		_ = os.Remove(pidPath)
	}
}

func isPIDRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtimepkg.GOOS == "windows" {
		err = proc.Signal(syscall.Signal(0))
		return err == nil
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

func prepareServerListener(addr string) (net.Listener, error) {
	if file, ok := inheritedListenerFile(); ok {
		defer file.Close()
		listener, err := net.FileListener(file)
		if err != nil {
			return nil, err
		}
		return listener, nil
	}
	return net.Listen("tcp", addr)
}

func markHandoffReady() error {
	path := strings.TrimSpace(os.Getenv("XCLAW_HANDOFF_READY_FILE"))
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func startDrainFileWatcher(ctx context.Context, runDir string, pid int, startDraining func()) {
	if strings.TrimSpace(runDir) == "" || pid <= 0 || startDraining == nil {
		return
	}
	path := handoffDrainPath(runDir, pid)
	_ = os.Remove(path)
	go func() {
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = os.Remove(path)
				return
			case <-ticker.C:
				if _, err := os.Stat(path); err == nil {
					startDraining()
					_ = os.Remove(path)
					return
				}
			}
		}
	}()
}

type runtimeSnapshotMeta struct {
	Draining       bool
	ActiveRequests int64
	ActiveSSE      int64
	ActiveWS       int64
}

func startRuntimeReporter(ctx context.Context, cfg config.RuntimeConfig, store *db.Store, meta func() runtimeSnapshotMeta) {
	startedAt := time.Now().UTC()
	writeRuntimeSnapshot(cfg, store, startedAt, meta)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				writeRuntimeSnapshot(cfg, store, startedAt, meta)
				return
			case <-ticker.C:
				writeRuntimeSnapshot(cfg, store, startedAt, meta)
			}
		}
	}()
}

func writeRuntimeSnapshot(cfg config.RuntimeConfig, store *db.Store, startedAt time.Time, meta func() runtimeSnapshotMeta) {
	state := runtimeState{
		PID:       os.Getpid(),
		Version:   version,
		Host:      cfg.Server.Host,
		Port:      cfg.Server.Port,
		StartedAt: startedAt,
		UpdatedAt: time.Now().UTC(),
	}
	if meta != nil {
		extra := meta()
		state.Draining = extra.Draining
		state.ActiveRequests = extra.ActiveRequests
		state.ActiveSSE = extra.ActiveSSE
		state.ActiveWebSockets = extra.ActiveWS
	}
	var mem runtimepkg.MemStats
	runtimepkg.ReadMemStats(&mem)
	state.MemoryAllocBytes = mem.Alloc
	state.MemorySysBytes = mem.Sys

	agents, _ := store.ListAgents(context.Background())
	state.AgentCount = len(agents)
	sessions, _ := store.ListSessions(context.Background(), "")
	state.SessionCount = len(sessions)
	for _, sess := range sessions {
		if strings.EqualFold(strings.TrimSpace(sess.Status), "running") {
			state.RunningSession++
		}
	}
	logs, _ := store.ListAudit(context.Background(), "", 200)
	for _, row := range logs {
		if row.Category != "heartbeat" {
			continue
		}
		state.LastHeartbeatAt = row.CreatedAt.UTC().Format(time.RFC3339)
		state.LastHeartbeatState = row.Action
		break
	}
	raw, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(runtimeStatePath(cfg.RunDir), raw, 0o644)
}

func waitForHTTPDrain(activeRequests func() int64, timeout time.Duration) {
	if activeRequests == nil || timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if activeRequests() <= 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
