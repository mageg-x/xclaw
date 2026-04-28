package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"xclaw/cli/audit"
	"xclaw/cli/config"
	"xclaw/cli/db"
	"xclaw/cli/engine"
	"xclaw/cli/gateway"
	"xclaw/cli/mcpclient"
	"xclaw/cli/mcpregistry"
	"xclaw/cli/models"
	"xclaw/cli/queue"
	"xclaw/cli/scheduler"
	"xclaw/cli/updater"
)

func runCommand(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	cmd := strings.TrimSpace(strings.ToLower(args[0]))
	if cmd == "" || strings.HasPrefix(cmd, "-") {
		return false, nil
	}
	rest := args[1:]
	switch cmd {
	case "start":
		return true, runStartCommand(rest)
	case "stop":
		return true, runStopCommand(rest)
	case "restart":
		return true, runRestartCommand(rest)
	case "status":
		return true, runStatusCommand(rest)
	case "list":
		return true, runListCommand(rest)
	case "doctor":
		return true, runDoctorCommand(rest)
	case "logs":
		return true, runLogsCommand(rest)
	case "config":
		return true, runConfigCommand(rest)
	case "cron":
		return true, runCronCommand(rest)
	case "gateway":
		return true, runGatewayCommand(rest)
	case "workspace":
		return true, runWorkspaceCommand(rest)
	case "update":
		return true, runUpdateCommand(rest)
	case "tui":
		return true, runTUICommand(rest)
	case "mcp-server":
		return true, runMCPServerCommand(rest)
	case "mcp":
		return true, runMCPCommand(rest)
	case "internal-helper-restart":
		return true, runInternalHelperRestartCommand(rest)
	default:
		return false, nil
	}
}

func loadCLIConfig(dataDir string) (config.RuntimeConfig, error) {
	cfg, err := config.LoadOrInit(strings.TrimSpace(dataDir))
	if err != nil {
		return config.RuntimeConfig{}, err
	}
	applyEnvOverrides(&cfg)
	return cfg, nil
}

func runStartCommand(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts runtimeOptions
	tlsFlag := &optionalBoolFlag{}
	fs.StringVar(&opts.RootDir, "root-dir", "", "root directory")
	fs.StringVar(&opts.DataDir, "data-dir", "", "data directory")
	fs.StringVar(&opts.Host, "host", "", "server host")
	fs.IntVar(&opts.Port, "port", 0, "server port")
	fs.Var(tlsFlag, "tls", "enable https/tls mode")
	fs.BoolVar(&opts.Daemon, "daemon", false, "run in daemon mode")
	fs.BoolVar(&opts.CheckUpdate, "check-update", true, "check release")
	fs.StringVar(&opts.ServiceCmd, "service", "", "service command")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if tlsFlag.set {
		opts.TLSOverride = &tlsFlag.value
	}
	raw := append([]string{"start"}, args...)
	return runRuntime(opts, raw)
}

func runStopCommand(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	fs.StringVar(&dataDir, "root-dir", "", "root directory")
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	pidPath := runtimePIDPath(cfg.RunDir)
	b, err := os.ReadFile(pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("agent is not running")
			return nil
		}
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		_ = os.Remove(pidPath)
		return fmt.Errorf("invalid pid file")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(pidPath)
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if killErr := proc.Kill(); killErr != nil {
			return fmt.Errorf("stop process: %w", err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !isPIDRunning(pid) {
			_ = os.Remove(pidPath)
			fmt.Printf("agent stopped (pid=%d)\n", pid)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting process stop (pid=%d)", pid)
}

func runRestartCommand(args []string) error {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var host string
	var port int
	var daemon bool
	tlsFlag := &optionalBoolFlag{}
	fs.StringVar(&dataDir, "root-dir", "", "root directory")
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&host, "host", "", "server host")
	fs.IntVar(&port, "port", 0, "server port")
	fs.Var(tlsFlag, "tls", "enable https/tls mode")
	fs.BoolVar(&daemon, "daemon", true, "restart in daemon mode")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = runStopCommand([]string{"--data-dir", dataDir})
	time.Sleep(500 * time.Millisecond)
	var tlsOverride *bool
	if tlsFlag.set {
		tlsOverride = &tlsFlag.value
	}
	return runRuntime(runtimeOptions{
		DataDir:     dataDir,
		Host:        host,
		Port:        port,
		TLSOverride: tlsOverride,
		Daemon:      daemon,
		CheckUpdate: true,
	}, append([]string{"restart"}, args...))
}

type statusReport struct {
	Version         string                 `json:"version"`
	Scheme          string                 `json:"scheme"`
	Running         bool                   `json:"running"`
	PID             int                    `json:"pid"`
	Uptime          string                 `json:"uptime"`
	Host            string                 `json:"host"`
	Port            int                    `json:"port"`
	MemoryAllocMB   int64                  `json:"memory_alloc_mb"`
	MemorySysMB     int64                  `json:"memory_sys_mb"`
	ActiveAgents    int                    `json:"active_agents"`
	ActiveSessions  int                    `json:"active_sessions"`
	RunningSessions int                    `json:"running_sessions"`
	LastHeartbeat   string                 `json:"last_heartbeat"`
	CronJobs        int                    `json:"cron_jobs"`
	GatewayChannels []string               `json:"gateway_channels"`
	TokenUsage24h   string                 `json:"token_usage_24h"`
	DiskUsedBytes   int64                  `json:"disk_used_bytes"`
	DiskFreeBytes   uint64                 `json:"disk_free_bytes"`
	VectorStatus    map[string]any         `json:"vector_status"`
	RuntimeState    map[string]interface{} `json:"runtime_state,omitempty"`
}

func runStatusCommand(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var asJSON bool
	fs.StringVar(&dataDir, "root-dir", "", "root directory")
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.BoolVar(&asJSON, "json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	report := statusReport{
		Version:       version,
		Scheme:        cfg.Server.Scheme(),
		Host:          cfg.Server.Host,
		Port:          cfg.Server.Port,
		TokenUsage24h: "n/a",
		VectorStatus:  store.VectorStatus(),
	}
	if b, err := os.ReadFile(runtimePIDPath(cfg.RunDir)); err == nil {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil {
			report.PID = pid
			report.Running = isPIDRunning(pid)
		}
	}
	if b, err := os.ReadFile(runtimeStatePath(cfg.RunDir)); err == nil {
		var state runtimeState
		if jsonErr := json.Unmarshal(b, &state); jsonErr == nil {
			report.MemoryAllocMB = int64(state.MemoryAllocBytes / (1024 * 1024))
			report.MemorySysMB = int64(state.MemorySysBytes / (1024 * 1024))
			if state.PID != 0 {
				report.PID = state.PID
				report.Running = isPIDRunning(state.PID)
			}
			if !state.StartedAt.IsZero() {
				report.Uptime = time.Since(state.StartedAt).Round(time.Second).String()
			}
			report.RuntimeState = map[string]interface{}{
				"started_at": state.StartedAt,
				"updated_at": state.UpdatedAt,
			}
		}
	}
	if report.Uptime == "" {
		report.Uptime = "n/a"
	}

	agents, _ := store.ListAgents(context.Background())
	sessions, _ := store.ListSessions(context.Background(), "")
	report.ActiveAgents = len(agents)
	report.ActiveSessions = len(sessions)
	for _, sess := range sessions {
		if strings.EqualFold(strings.TrimSpace(sess.Status), "running") {
			report.RunningSessions++
		}
	}

	logs, _ := store.ListAudit(context.Background(), "", 300)
	for _, row := range logs {
		if row.Category == "heartbeat" {
			report.LastHeartbeat = fmt.Sprintf("%s (%s)", row.CreatedAt.Local().Format(time.RFC3339), row.Action)
			break
		}
	}
	if report.LastHeartbeat == "" {
		report.LastHeartbeat = "none"
	}

	cronJobs, _ := store.ListCronJobs(context.Background(), "", false)
	report.CronJobs = len(cronJobs)
	report.GatewayChannels = loadGatewayChannelStatus(store)
	report.DiskUsedBytes = dirSize(cfg.DataDir)
	report.DiskFreeBytes = diskFreeBytes(cfg.DataDir)

	if asJSON {
		raw, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(raw))
		return nil
	}

	fmt.Println("Agent Runtime Status")
	fmt.Printf("Version: %s\n", report.Version)
	fmt.Printf("Dashboard: %s://%s:%d\n", report.Scheme, report.Host, report.Port)
	fmt.Printf("Running: %t", report.Running)
	if report.PID > 0 {
		fmt.Printf(" (pid=%d)", report.PID)
	}
	fmt.Println()
	fmt.Printf("Uptime: %s\n", report.Uptime)
	fmt.Printf("Memory: %dMB alloc / %dMB sys\n", report.MemoryAllocMB, report.MemorySysMB)
	fmt.Printf("Active Agents: %d\n", report.ActiveAgents)
	fmt.Printf("Active Sessions: %d\n", report.ActiveSessions)
	fmt.Printf("Running Sessions: %d\n", report.RunningSessions)
	fmt.Printf("Last Heartbeat: %s\n", report.LastHeartbeat)
	fmt.Printf("Cron Jobs: %d\n", report.CronJobs)
	fmt.Printf("Gateway Channels: %s\n", strings.Join(report.GatewayChannels, ", "))
	fmt.Printf("Token Usage (24h): %s\n", report.TokenUsage24h)
	fmt.Printf("Disk Usage: %s used / %s free\n", humanBytes(report.DiskUsedBytes), humanBytes(int64(report.DiskFreeBytes)))
	return nil
}

func runListCommand(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var team string
	var asJSON bool
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&team, "team", "", "team filter")
	fs.BoolVar(&asJSON, "json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	agents, err := store.ListAgents(context.Background())
	if err != nil {
		return err
	}
	flt := strings.ToLower(strings.TrimSpace(team))
	out := make([]models.Agent, 0, len(agents))
	for _, a := range agents {
		if flt != "" {
			joined := strings.ToLower(a.Name + " " + a.Description)
			if !strings.Contains(joined, flt) {
				continue
			}
		}
		out = append(out, a)
	}
	if asJSON {
		raw, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	if len(out) == 0 {
		fmt.Println("no agents")
		return nil
	}
	fmt.Println("ID\tName\tProvider/Model\tUpdated")
	for _, a := range out {
		fmt.Printf("%s\t%s\t%s/%s\t%s\n", a.ID, a.Name, a.ModelProvider, a.ModelName, a.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return nil
}

type doctorItem struct {
	Name       string `json:"name"`
	Severity   string `json:"severity"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
	Fixed      bool   `json:"fixed"`
}

func runDoctorCommand(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var fix bool
	var asJSON bool
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.BoolVar(&fix, "fix", false, "auto fix")
	fs.BoolVar(&asJSON, "json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}

	results := make([]doctorItem, 0, 12)
	add := func(item doctorItem) {
		if item.Status == "" {
			item.Status = "ok"
		}
		if item.Severity == "" {
			item.Severity = "info"
		}
		results = append(results, item)
	}

	if err := config.Save(cfg); err != nil {
		add(doctorItem{Name: "config", Severity: "error", Status: "error", Message: err.Error(), Suggestion: "检查配置文件权限和格式"})
	} else {
		add(doctorItem{Name: "config", Severity: "error", Status: "ok", Message: "配置文件有效"})
	}

	apiKeys := []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "DEEPSEEK_API_KEY"}
	foundKey := false
	for _, key := range apiKeys {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			foundKey = true
			break
		}
	}
	if !foundKey {
		add(doctorItem{Name: "api_keys", Severity: "error", Status: "error", Message: "未检测到 LLM API Key", Suggestion: "配置至少一个 API Key 环境变量"})
	} else {
		add(doctorItem{Name: "api_keys", Severity: "error", Status: "ok", Message: "已检测到 API Key"})
	}

	workspaceOK := isDirWritable(cfg.WorkspaceDir)
	itemWorkspace := doctorItem{Name: "workspace", Severity: "error"}
	if workspaceOK {
		itemWorkspace.Status = "ok"
		itemWorkspace.Message = "工作空间可读写"
	} else {
		itemWorkspace.Status = "error"
		itemWorkspace.Message = "工作空间不可写"
		itemWorkspace.Suggestion = "检查目录权限"
		if fix {
			if mkErr := os.MkdirAll(cfg.WorkspaceDir, 0o755); mkErr == nil && isDirWritable(cfg.WorkspaceDir) {
				itemWorkspace.Fixed = true
				itemWorkspace.Status = "ok"
				itemWorkspace.Message = "已自动修复工作空间目录"
			}
		}
	}
	add(itemWorkspace)

	dbPath := filepath.Join(cfg.DataDir, "xclaw.db")
	itemDB := doctorItem{Name: "db", Severity: "warn"}
	if err := checkDBIntegrity(dbPath, fix); err != nil {
		itemDB.Status = "warn"
		itemDB.Message = err.Error()
		itemDB.Suggestion = "建议备份后执行 VACUUM / integrity check"
	} else {
		itemDB.Status = "ok"
		itemDB.Message = "SQLite 完整性检查通过"
	}
	add(itemDB)

	if cfg.Sandbox.Mode == "off" {
		add(doctorItem{Name: "sandbox", Severity: "warn", Status: "warn", Message: "沙箱模式为 off", Suggestion: "建议启用 native 沙箱"})
	} else {
		add(doctorItem{Name: "sandbox", Severity: "warn", Status: "ok", Message: "沙箱模式已启用: " + cfg.Sandbox.Mode})
	}

	store, openErr := db.Open(dbPath)
	if openErr != nil {
		add(doctorItem{Name: "gateway", Severity: "warn", Status: "warn", Message: "无法读取网关配置: " + openErr.Error()})
		add(doctorItem{Name: "cron", Severity: "info", Status: "warn", Message: "无法检查 cron: " + openErr.Error()})
		add(doctorItem{Name: "memory", Severity: "info", Status: "warn", Message: "无法检查记忆库: " + openErr.Error()})
	} else {
		defer store.Close()
		channels := loadGatewayChannelStatus(store)
		if len(channels) == 0 {
			item := doctorItem{Name: "gateway", Severity: "warn", Status: "warn", Message: "网关尚未配置通道", Suggestion: "至少启用一个通道 provider"}
			if fix {
				_ = ensureGatewayDefaults(store)
				item.Fixed = true
				item.Status = "ok"
				item.Message = "已恢复默认网关通道配置"
			}
			add(item)
		} else {
			add(doctorItem{Name: "gateway", Severity: "warn", Status: "ok", Message: "网关通道状态: " + strings.Join(channels, ", ")})
		}

		jobs, err := store.ListCronJobs(context.Background(), "", false)
		if err != nil {
			add(doctorItem{Name: "cron", Severity: "info", Status: "warn", Message: err.Error()})
		} else {
			invalid := 0
			for _, job := range jobs {
				if _, resolveErr := scheduler.ResolveNextRun(job, time.Now().UTC()); resolveErr != nil {
					invalid++
				}
			}
			if invalid > 0 {
				add(doctorItem{Name: "cron", Severity: "info", Status: "warn", Message: fmt.Sprintf("发现 %d 个无效计划任务", invalid), Suggestion: "修正 schedule 语法"})
			} else {
				add(doctorItem{Name: "cron", Severity: "info", Status: "ok", Message: fmt.Sprintf("cron 任务 %d 个", len(jobs))})
			}
		}

		vs := store.VectorStatus()
		add(doctorItem{Name: "memory", Severity: "info", Status: "ok", Message: fmt.Sprintf("向量库状态: enabled=%v, hnsw=%v", vs["enabled"], vs["hnsw_supported"])})
	}

	connItem := doctorItem{Name: "connectivity", Severity: "error"}
	if err := checkConnectivity(); err != nil {
		connItem.Status = "error"
		connItem.Message = err.Error()
		connItem.Suggestion = "检查网络或代理配置"
	} else {
		connItem.Status = "ok"
		connItem.Message = "核心端点网络可达"
	}
	add(connItem)

	free := diskFreeBytes(cfg.DataDir)
	if free < 500*1024*1024 {
		add(doctorItem{Name: "disk_space", Severity: "warn", Status: "warn", Message: fmt.Sprintf("可用磁盘空间不足: %s", humanBytes(int64(free))), Suggestion: "清理日志或迁移数据目录"})
	} else {
		add(doctorItem{Name: "disk_space", Severity: "warn", Status: "ok", Message: fmt.Sprintf("可用磁盘空间: %s", humanBytes(int64(free)))})
	}

	latest, relURL, relErr := checkLatestRelease("xclaw/agent")
	if relErr != nil {
		add(doctorItem{Name: "version", Severity: "info", Status: "warn", Message: "无法检查更新: " + relErr.Error()})
	} else {
		msg := fmt.Sprintf("当前版本 %s，最新版本 %s", version, latest)
		if latest != "" && latest != version && latest != "v"+version {
			msg += "，可执行 agent update"
		}
		if relURL != "" {
			msg += " (" + relURL + ")"
		}
		add(doctorItem{Name: "version", Severity: "info", Status: "ok", Message: msg})
	}

	if asJSON {
		raw, _ := json.MarshalIndent(results, "", "  ")
		fmt.Println(string(raw))
		return nil
	}

	for _, item := range results {
		icon := "✅"
		switch item.Status {
		case "warn":
			icon = "⚠️"
		case "error":
			icon = "❌"
		}
		line := fmt.Sprintf("%s %-12s %s", icon, item.Name, item.Message)
		if item.Fixed {
			line += " [fixed]"
		}
		fmt.Println(line)
		if item.Suggestion != "" && item.Status != "ok" {
			fmt.Printf("   -> %s\n", item.Suggestion)
		}
	}
	return nil
}

func runLogsCommand(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var follow bool
	var agentID string
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.BoolVar(&follow, "follow", false, "follow output")
	fs.StringVar(&agentID, "agent", "", "agent id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}

	if strings.TrimSpace(agentID) != "" {
		store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
		if err != nil {
			return err
		}
		defer store.Close()
		lastID := int64(0)
		for {
			logs, listErr := store.ListAudit(context.Background(), agentID, 200)
			if listErr != nil {
				return listErr
			}
			sort.Slice(logs, func(i, j int) bool { return logs[i].ID < logs[j].ID })
			for _, row := range logs {
				if row.ID <= lastID {
					continue
				}
				lastID = row.ID
				fmt.Printf("%s [%s/%s] %s\n", row.CreatedAt.Local().Format("2006-01-02 15:04:05"), row.Category, row.Action, row.Detail)
			}
			if !follow {
				return nil
			}
			time.Sleep(2 * time.Second)
		}
	}

	path := filepath.Join(cfg.LogsDir, "agent-daemon.log")
	if !follow {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Print(string(b))
		return nil
	}
	return followFile(path)
}

func runConfigCommand(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var getKey string
	var setKV string
	var asJSON bool
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&getKey, "get", "", "get key")
	fs.StringVar(&setKV, "set", "", "set key=value")
	fs.BoolVar(&asJSON, "json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}

	if strings.TrimSpace(setKV) != "" {
		if err := applyConfigKV(&cfg, setKV); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Println("config updated")
		return nil
	}
	if strings.TrimSpace(getKey) != "" {
		v, err := readConfigKey(cfg, getKey)
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if asJSON {
		fmt.Println(string(raw))
		return nil
	}
	fmt.Print(string(raw))
	fmt.Println()
	return nil
}

func runCronCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agent cron <list|add|remove|run>")
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	switch sub {
	case "list":
		return runCronList(rest)
	case "add":
		return runCronAdd(rest)
	case "remove":
		return runCronRemove(rest)
	case "run":
		return runCronRun(rest)
	default:
		return fmt.Errorf("unknown cron subcommand: %s", sub)
	}
}

func runCronList(args []string) error {
	fs := flag.NewFlagSet("cron list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var agentID string
	var asJSON bool
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&agentID, "agent", "", "agent id")
	fs.BoolVar(&asJSON, "json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	jobs, err := store.ListCronJobs(context.Background(), agentID, false)
	if err != nil {
		return err
	}
	if asJSON {
		raw, _ := json.MarshalIndent(jobs, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	if len(jobs) == 0 {
		fmt.Println("no cron jobs")
		return nil
	}
	fmt.Println("ID\tAgent\tName\tType\tSchedule\tEnabled\tNext")
	for _, j := range jobs {
		next := "-"
		if j.NextRunAt != nil {
			next = j.NextRunAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%t\t%s\n", j.ID, j.AgentID, j.Name, j.ScheduleType, j.Schedule, j.Enabled, next)
	}
	return nil
}

func runCronAdd(args []string) error {
	fs := flag.NewFlagSet("cron add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var agentID string
	var name string
	var scheduleType string
	var execMode string
	var sessionID string
	var target string
	var priority string
	var retry int
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&agentID, "agent", "", "agent id")
	fs.StringVar(&name, "name", "定时任务", "job name")
	fs.StringVar(&scheduleType, "type", "cron", "cron|every|at")
	fs.StringVar(&execMode, "mode", "main", "main|isolated|custom")
	fs.StringVar(&sessionID, "session", "", "session id for custom mode")
	fs.StringVar(&target, "target", "last", "target channel")
	fs.StringVar(&priority, "priority", "normal", "priority")
	fs.IntVar(&retry, "retry", 5, "retry limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return fmt.Errorf("usage: agent cron add <schedule> <payload> --agent <id>")
	}
	schedule := strings.TrimSpace(pos[0])
	payload := strings.TrimSpace(strings.Join(pos[1:], " "))
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("--agent is required")
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	now := time.Now().UTC()
	job := models.CronJob{
		ID:            engine.NewID("cron"),
		AgentID:       agentID,
		Name:          strings.TrimSpace(name),
		Schedule:      schedule,
		ScheduleType:  strings.ToLower(strings.TrimSpace(scheduleType)),
		JobType:       "agent",
		Payload:       payload,
		ExecutionMode: strings.ToLower(strings.TrimSpace(execMode)),
		SessionID:     strings.TrimSpace(sessionID),
		TargetChannel: strings.TrimSpace(target),
		Priority:      strings.TrimSpace(priority),
		Enabled:       true,
		RetryLimit:    retry,
		LastStatus:    "never",
		LastError:     "",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	next, err := scheduler.ResolveNextRun(job, now)
	if err != nil {
		return err
	}
	job.NextRunAt = &next
	if err := store.CreateCronJob(context.Background(), job); err != nil {
		return err
	}
	fmt.Printf("cron job created: %s\n", job.ID)
	return nil
}

func runCronRemove(args []string) error {
	fs := flag.NewFlagSet("cron remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		return fmt.Errorf("usage: agent cron remove <id>")
	}
	id := strings.TrimSpace(fs.Args()[0])
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.DeleteCronJob(context.Background(), id); err != nil {
		return err
	}
	fmt.Printf("cron job removed: %s\n", id)
	return nil
}

func runCronRun(args []string) error {
	fs := flag.NewFlagSet("cron run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		return fmt.Errorf("usage: agent cron run <id>")
	}
	id := strings.TrimSpace(fs.Args()[0])
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	jobs, err := store.ListCronJobs(context.Background(), "", false)
	if err != nil {
		return err
	}
	var job *models.CronJob
	for i := range jobs {
		if jobs[i].ID == id {
			job = &jobs[i]
			break
		}
	}
	if job == nil {
		return fmt.Errorf("cron job not found: %s", id)
	}
	if err := runCronJobOnce(cfg, store, *job); err != nil {
		return err
	}
	now := time.Now().UTC()
	_ = store.UpdateCronResult(context.Background(), job.ID, &now, "success", "manual run")
	fmt.Printf("cron job triggered: %s\n", id)
	return nil
}

func runGatewayCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agent gateway <status|enable|disable|restart>")
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]

	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	laneQueue := queue.NewLaneQueue(8)
	defer laneQueue.Close()
	auditLogger := audit.NewLogger(store)
	gw, err := gateway.New(store, auditLogger, laneQueue)
	if err != nil {
		return err
	}

	switch sub {
	case "status":
		gw.Start(context.Background())
		defer gw.Stop(context.Background())
		cfgs := gw.ListProviderConfigs()
		health := gw.ListProviderHealth(context.Background())
		fmt.Println("Provider\tEnabled\tProtocol\tStatus")
		for _, c := range cfgs {
			h := health[c.Name]
			fmt.Printf("%s\t%t\t%s\t%s\n", c.Name, c.Enabled, c.Protocol, firstText(strings.TrimSpace(h.Status), "unknown"))
		}
		return nil
	case "enable":
		if len(fs.Args()) == 0 {
			return fmt.Errorf("usage: agent gateway enable <provider>")
		}
		name := strings.ToLower(strings.TrimSpace(fs.Args()[0]))
		if name == "" {
			return fmt.Errorf("provider required")
		}
		if err := gw.UpsertProviderConfig(context.Background(), gateway.ProviderConfig{Name: name, Protocol: "bridge", Enabled: true, Settings: map[string]string{}}); err != nil {
			return err
		}
		fmt.Printf("gateway provider enabled: %s\n", name)
		return nil
	case "disable":
		if len(fs.Args()) == 0 {
			return fmt.Errorf("usage: agent gateway disable <provider>")
		}
		name := strings.ToLower(strings.TrimSpace(fs.Args()[0]))
		if name == "" {
			return fmt.Errorf("provider required")
		}
		cfgs := gw.ListProviderConfigs()
		var targetCfg gateway.ProviderConfig
		found := false
		for _, c := range cfgs {
			if c.Name == name {
				targetCfg = c
				found = true
				break
			}
		}
		if !found {
			targetCfg = gateway.ProviderConfig{Name: name, Protocol: "bridge", Settings: map[string]string{}}
		}
		targetCfg.Enabled = false
		if err := gw.UpsertProviderConfig(context.Background(), targetCfg); err != nil {
			return err
		}
		fmt.Printf("gateway provider disabled: %s\n", name)
		return nil
	case "restart":
		gw.Stop(context.Background())
		gw.Start(context.Background())
		fmt.Println("gateway restarted")
		return nil
	default:
		return fmt.Errorf("unknown gateway subcommand: %s", sub)
	}
}

func runWorkspaceCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agent workspace <export|import|prune>")
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	switch sub {
	case "export":
		return runWorkspaceExport(rest)
	case "import":
		return runWorkspaceImport(rest)
	case "prune":
		return runWorkspacePrune(rest)
	default:
		return fmt.Errorf("unknown workspace subcommand: %s", sub)
	}
}

func runWorkspaceExport(args []string) error {
	fs := flag.NewFlagSet("workspace export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var out string
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&out, "output", "", "output zip file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		out = filepath.Join(cfg.DataDir, fmt.Sprintf("workspace-export-%s.zip", time.Now().Format("20060102-150405")))
	}
	if err := zipDir(cfg.WorkspaceDir, out); err != nil {
		return err
	}
	fmt.Printf("workspace exported: %s\n", out)
	return nil
}

func runWorkspaceImport(args []string) error {
	fs := flag.NewFlagSet("workspace import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var in string
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&in, "input", "", "input zip file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(in) == "" {
		return fmt.Errorf("--input is required")
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	if err := unzipTo(in, cfg.WorkspaceDir); err != nil {
		return err
	}
	fmt.Printf("workspace imported from: %s\n", in)
	return nil
}

func runWorkspacePrune(args []string) error {
	fs := flag.NewFlagSet("workspace prune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var days int
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.IntVar(&days, "days", 30, "prune files older than days")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	if days <= 0 {
		days = 30
	}
	removed, err := pruneOldFiles(cfg.WorkspaceDir, time.Duration(days)*24*time.Hour)
	if err != nil {
		return err
	}
	fmt.Printf("workspace pruned, removed files: %d\n", removed)
	return nil
}

func runUpdateCommand(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var channel string
	var yes bool
	fs.StringVar(&channel, "channel", "stable", "stable|beta")
	fs.BoolVar(&yes, "yes", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo := "xclaw/agent"
	if strings.EqualFold(strings.TrimSpace(channel), "beta") {
		repo = "xclaw/agent-beta"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, err := updater.FetchLatest(ctx, http.DefaultClient, repo)
	if err != nil {
		return err
	}
	tag := strings.TrimSpace(release.TagName)
	if tag == "" {
		fmt.Println("no release info")
		return nil
	}
	if tag == version || tag == "v"+version {
		fmt.Printf("already latest: %s\n", version)
		return nil
	}
	asset, checksum, err := updater.SelectPlatformAsset(release, "", "")
	if err != nil {
		return err
	}
	fmt.Printf("new version available: %s (current: %s)\n", tag, version)
	fmt.Printf("selected asset: %s\n", asset.Name)

	if !yes {
		fmt.Print("download and install? [y/N] ")
		var input string
		_, _ = fmt.Scanln(&input)
		if strings.ToLower(strings.TrimSpace(input)) != "y" {
			fmt.Println("update cancelled")
			return nil
		}
	}

	tmpDir, err := os.MkdirTemp("", "xclaw-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Printf("downloading %s ...\n", asset.DownloadURL)
	binaryPath, err := updater.DownloadAndExtractBinary(ctx, http.DefaultClient, asset, checksum, tmpDir)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		currentExe, _ = os.Executable()
	}

	backupPath := currentExe + ".bak"
	if err := os.Rename(currentExe, backupPath); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}

	if err := os.Rename(binaryPath, currentExe); err != nil {
		_ = os.Rename(backupPath, currentExe)
		return fmt.Errorf("replace binary: %w", err)
	}
	_ = os.Chmod(currentExe, 0o755)
	_ = os.Remove(backupPath)

	fmt.Printf("updated %s -> %s\n", version, tag)
	fmt.Println("restart the service to use the new version")
	return nil
}

func downloadFile(url, dest string) error {
	resp, err := http.DefaultClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func verifyChecksum(filePath, checksumPath string) error {
	checksumData, err := os.ReadFile(checksumPath)
	if err != nil {
		return err
	}
	parts := strings.Fields(strings.TrimSpace(string(checksumData)))
	if len(parts) == 0 {
		return fmt.Errorf("empty checksum file")
	}
	expectedHash := parts[0]

	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(fileData)
	actualHash := hex.EncodeToString(sum[:])

	if !strings.EqualFold(actualHash, expectedHash) {
		return fmt.Errorf("expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

func followFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			fmt.Print(string(buf[:n]))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				time.Sleep(300 * time.Millisecond)
				continue
			}
			return readErr
		}
	}
}

func humanBytes(v int64) string {
	if v < 0 {
		v = 0
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(v)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d%s", int64(f), units[i])
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}

func loadGatewayChannelStatus(store *db.Store) []string {
	raw, ok, err := store.GetSetting(context.Background(), "gateway_provider_configs_json")
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return []string{"none"}
	}
	var cfg map[string]gateway.ProviderConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return []string{"invalid_config"}
	}
	keys := make([]string, 0, len(cfg))
	for name, item := range cfg {
		state := "disabled"
		if item.Enabled {
			state = "enabled"
		}
		keys = append(keys, fmt.Sprintf("%s(%s)", name, state))
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return []string{"none"}
	}
	return keys
}

func dirSize(root string) int64 {
	root = strings.TrimSpace(root)
	if root == "" {
		return 0
	}
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func diskFreeBytes(path string) uint64 {
	return diskFreeBytesPlatform(path)
}

func isDirWritable(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	fp := filepath.Join(dir, ".write-test")
	if err := os.WriteFile(fp, []byte("ok"), 0o644); err != nil {
		return false
	}
	_ = os.Remove(fp)
	return true
}

func checkDBIntegrity(dbPath string, fix bool) error {
	sqlDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s", dbPath))
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	var result string
	if err := sqlDB.QueryRow("PRAGMA integrity_check;").Scan(&result); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(result), "ok") {
		if fix {
			_, _ = sqlDB.Exec("VACUUM;")
		}
		return nil
	}
	if fix {
		_, _ = sqlDB.Exec("VACUUM;")
		var verify string
		if err := sqlDB.QueryRow("PRAGMA integrity_check;").Scan(&verify); err == nil && strings.EqualFold(strings.TrimSpace(verify), "ok") {
			return nil
		}
	}
	return fmt.Errorf("integrity_check=%s", strings.TrimSpace(result))
}

func checkConnectivity() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("upstream status: %d", resp.StatusCode)
	}
	return nil
}

func checkLatestRelease(repo string) (string, string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", "", fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", err
	}
	return strings.TrimSpace(payload.TagName), strings.TrimSpace(payload.HTMLURL), nil
}

func ensureGatewayDefaults(store *db.Store) error {
	defaults := map[string]gateway.ProviderConfig{
		"console": {Name: "console", Protocol: "stdio", Enabled: true, Settings: map[string]string{}},
	}
	raw, _ := json.Marshal(defaults)
	return store.SetSetting(context.Background(), "gateway_provider_configs_json", string(raw))
}

func applyConfigKV(cfg *config.RuntimeConfig, kv string) error {
	parts := strings.SplitN(strings.TrimSpace(kv), "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid --set, expected key=value")
	}
	key := strings.ToLower(strings.TrimSpace(parts[0]))
	value := strings.TrimSpace(parts[1])
	switch key {
	case "root_dir":
		cfg.RootDir = value
	case "data_dir":
		cfg.DataDir = value
	case "config_dir":
		cfg.ConfigDir = value
	case "skills_dir":
		cfg.SkillsDir = value
	case "cache_dir":
		cfg.CacheDir = value
	case "logs_dir":
		cfg.LogsDir = value
	case "run_dir":
		cfg.RunDir = value
	case "tmp_dir":
		cfg.TmpDir = value
	case "workspace_dir":
		cfg.WorkspaceDir = value
	case "server.host":
		cfg.Server.Host = value
	case "server.port":
		v, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		cfg.Server.Port = v
	case "server.tls":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		cfg.Server.TLS = v
	case "sandbox.mode":
		cfg.Sandbox.Mode = value
	case "sandbox.workspace_access":
		cfg.Sandbox.WorkspaceAccess = value
	case "sandbox.scope":
		cfg.Sandbox.Scope = value
	case "sandbox.custom_command":
		cfg.Sandbox.CustomCommand = value
	case "queue.global_concurrency":
		v, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		cfg.Queue.GlobalConcurrency = v
	case "release_repo":
		cfg.ReleaseRepo = value
	default:
		return fmt.Errorf("unsupported config key: %s", key)
	}
	return nil
}

func readConfigKey(cfg config.RuntimeConfig, key string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "root_dir":
		return cfg.RootDir, nil
	case "data_dir":
		return cfg.DataDir, nil
	case "config_dir":
		return cfg.ConfigDir, nil
	case "skills_dir":
		return cfg.SkillsDir, nil
	case "cache_dir":
		return cfg.CacheDir, nil
	case "logs_dir":
		return cfg.LogsDir, nil
	case "run_dir":
		return cfg.RunDir, nil
	case "tmp_dir":
		return cfg.TmpDir, nil
	case "workspace_dir":
		return cfg.WorkspaceDir, nil
	case "server.host":
		return cfg.Server.Host, nil
	case "server.port":
		return strconv.Itoa(cfg.Server.Port), nil
	case "server.tls":
		return strconv.FormatBool(cfg.Server.TLS), nil
	case "sandbox.mode":
		return cfg.Sandbox.Mode, nil
	case "sandbox.workspace_access":
		return cfg.Sandbox.WorkspaceAccess, nil
	case "sandbox.scope":
		return cfg.Sandbox.Scope, nil
	case "release_repo":
		return cfg.ReleaseRepo, nil
	default:
		return "", fmt.Errorf("unsupported config key: %s", key)
	}
}

func zipDir(srcDir, dstZip string) error {
	srcDir = filepath.Clean(srcDir)
	if err := os.MkdirAll(filepath.Dir(dstZip), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dstZip)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	})
}

func runMCPCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: xclaw mcp <list|add|remove|test|tools>")
	}
	sub := strings.TrimSpace(strings.ToLower(args[0]))
	rest := args[1:]
	switch sub {
	case "list":
		return runMCPListCommand(rest)
	case "add":
		return runMCPAddCommand(rest)
	case "remove":
		return runMCPRemoveCommand(rest)
	case "test":
		return runMCPTestCommand(rest)
	case "tools":
		return runMCPToolsCommand(rest)
	default:
		return fmt.Errorf("unknown mcp subcommand: %s", sub)
	}
}

func loadMCPManagerFromStore(ctx context.Context, dataDir string) (*mcpclient.Manager, config.RuntimeConfig, *db.Store, error) {
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return nil, config.RuntimeConfig{}, nil, err
	}
	store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return nil, config.RuntimeConfig{}, nil, err
	}
	manager := mcpclient.NewManager()
	if items, err := mcpregistry.LoadManual(ctx, store); err == nil {
		if merged, mergeErr := mcpregistry.MergeWithSkillServers(cfg.SkillsDir, items); mergeErr == nil {
			manager.SetServers(merged)
		}
	}
	return manager, cfg, store, nil
}

func persistMCPServers(ctx context.Context, store *db.Store, cfg config.RuntimeConfig, manager *mcpclient.Manager) error {
	items, err := mcpregistry.SaveManualAndSync(ctx, store, cfg.SkillsDir, manager.Servers())
	if err != nil {
		return err
	}
	manager.SetServers(items)
	return nil
}

func runMCPListCommand(args []string) error {
	fs := flag.NewFlagSet("mcp list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	fs.StringVar(&dataDir, "root-dir", "", "root directory")
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manager, _, store, err := loadMCPManagerFromStore(context.Background(), dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	items := manager.Servers()
	if len(items) == 0 {
		fmt.Println("no mcp servers configured")
		return nil
	}
	for _, item := range items {
		target := item.URL
		if item.Transport == "stdio" {
			target = strings.TrimSpace(item.Command + " " + strings.Join(item.Args, " "))
		}
		source := "manual"
		if item.Readonly && item.ManagedBy != "" {
			source = item.ManagedBy
		}
		fmt.Printf("%s\t%s\t%s\tenabled=%t\t%s\t%s\n", item.ID, item.Name, item.Transport, item.Enabled, source, strings.TrimSpace(target))
	}
	return nil
}

func runMCPAddCommand(args []string) error {
	fs := flag.NewFlagSet("mcp add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var id string
	var name string
	var transport string
	var url string
	var command string
	var argv string
	var envRaw string
	var enabled bool
	var timeout int
	fs.StringVar(&dataDir, "root-dir", "", "root directory")
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&id, "id", "", "server id")
	fs.StringVar(&name, "name", "", "server name")
	fs.StringVar(&transport, "transport", "", "http or stdio")
	fs.StringVar(&url, "url", "", "mcp http endpoint")
	fs.StringVar(&command, "command", "", "stdio command")
	fs.StringVar(&argv, "args", "", "comma-separated stdio args")
	fs.StringVar(&envRaw, "env", "", "comma-separated env pairs, e.g. KEY=VALUE,FOO=BAR")
	fs.BoolVar(&enabled, "enabled", true, "enabled")
	fs.IntVar(&timeout, "timeout", 20, "timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manager, cfg, store, err := loadMCPManagerFromStore(context.Background(), dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	item := mcpclient.ServerConfig{
		ID:         id,
		Name:       name,
		Transport:  transport,
		URL:        url,
		Command:    command,
		Args:       splitCSV(argv),
		Env:        splitEnvPairs(envRaw),
		Enabled:    enabled,
		TimeoutSec: timeout,
	}
	item = manager.UpsertServer(item)
	if err := persistMCPServers(context.Background(), store, cfg, manager); err != nil {
		return err
	}
	fmt.Printf("saved mcp server %s (%s)\n", item.ID, item.Transport)
	return nil
}

func runMCPRemoveCommand(args []string) error {
	fs := flag.NewFlagSet("mcp remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var id string
	fs.StringVar(&dataDir, "root-dir", "", "root directory")
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&id, "id", "", "server id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("id required")
	}
	manager, cfg, store, err := loadMCPManagerFromStore(context.Background(), dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	if existing, ok := findMCPServer(manager.Servers(), id); ok && existing.Readonly {
		return fmt.Errorf("mcp server is managed by %s and cannot be deleted directly", existing.ManagedBy)
	}
	if !manager.RemoveServer(id) {
		return fmt.Errorf("mcp server not found: %s", id)
	}
	if err := persistMCPServers(context.Background(), store, cfg, manager); err != nil {
		return err
	}
	fmt.Printf("removed mcp server %s\n", id)
	return nil
}

func runMCPTestCommand(args []string) error {
	fs := flag.NewFlagSet("mcp test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	var id string
	fs.StringVar(&dataDir, "root-dir", "", "root directory")
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	fs.StringVar(&id, "id", "", "server id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("id required")
	}
	manager, _, store, err := loadMCPManagerFromStore(context.Background(), dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := manager.TestServer(context.Background(), id)
	if err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(raw))
	return nil
}

func runMCPToolsCommand(args []string) error {
	fs := flag.NewFlagSet("mcp tools", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	fs.StringVar(&dataDir, "root-dir", "", "root directory")
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manager, _, store, err := loadMCPManagerFromStore(context.Background(), dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	items := manager.ListTools(context.Background())
	if len(items) == 0 {
		fmt.Println("no mcp tools discovered")
		return nil
	}
	for _, item := range items {
		if !item.Available && item.LastError != "" {
			fmt.Printf("%s\tERROR\t%s\n", item.FullName, item.LastError)
			continue
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", item.FullName, item.ServerName, item.Risk, item.Description)
	}
	return nil
}

func splitCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func findMCPServer(items []mcpclient.ServerConfig, id string) (mcpclient.ServerConfig, bool) {
	id = strings.TrimSpace(id)
	for _, item := range items {
		if strings.TrimSpace(item.ID) == id {
			return item, true
		}
	}
	return mcpclient.ServerConfig{}, false
}

func splitEnvPairs(raw string) map[string]string {
	items := splitCSV(raw)
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		parts := strings.SplitN(item, "=", 2)
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		val := ""
		if len(parts) == 2 {
			val = strings.TrimSpace(parts[1])
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func unzipTo(srcZip, dstDir string) error {
	r, err := zip.OpenReader(srcZip)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	for _, f := range r.File {
		cleanName := filepath.Clean(f.Name)
		target := filepath.Join(dstDir, cleanName)
		if !strings.HasPrefix(target, filepath.Clean(dstDir)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(dstDir) {
			return fmt.Errorf("invalid zip path: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func pruneOldFiles(root string, olderThan time.Duration) (int, error) {
	threshold := time.Now().Add(-olderThan)
	removed := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if info.ModTime().After(threshold) {
			return nil
		}
		if rmErr := os.Remove(path); rmErr == nil {
			removed++
		}
		return nil
	})
	return removed, err
}

func runCronJobOnce(cfg config.RuntimeConfig, store *db.Store, job models.CronJob) error {
	laneQueue := queue.NewLaneQueue(cfg.Queue.GlobalConcurrency)
	defer laneQueue.Close()
	auditLogger := audit.NewLogger(store)
	gw, err := gateway.New(store, auditLogger, laneQueue)
	if err != nil {
		return err
	}
	gw.Start(context.Background())
	defer gw.Stop(context.Background())

	if strings.TrimSpace(job.TargetChannel) != "" {
		_, _ = gw.Send(context.Background(), job.AgentID, job.TargetChannel, gateway.OutboundEvent{
			MessageID:      engine.NewID("cron_notify"),
			TextMarkdown:   fmt.Sprintf("⏰ 定时任务 `%s` 已触发（手动）", job.Name),
			Priority:       firstText(strings.TrimSpace(job.Priority), "normal"),
			IdempotencyKey: fmt.Sprintf("cron-manual:%s:%d", job.ID, time.Now().Unix()/60),
			TTLSeconds:     3600,
		})
	}
	auditLogger.Log(context.Background(), job.AgentID, job.SessionID, "cron", "manual_run", fmt.Sprintf("job=%s payload=%s", job.ID, trimForLog(job.Payload, 200)))
	return nil
}

func trimForLog(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + " ..."
}

func firstText(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func marshalJSONLine(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(bytes.TrimSpace(b))
}
