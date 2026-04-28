package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	DefaultHTTPPort = 5310
)

type ServerConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	TLS  bool   `json:"tls"`
}

func (s ServerConfig) Scheme() string {
	if s.TLS {
		return "https"
	}
	return "http"
}

type QueueConfig struct {
	MainLaneConcurrency int `json:"main_lane_concurrency"`
	SubLaneConcurrency  int `json:"sub_lane_concurrency"`
	GlobalConcurrency   int `json:"global_concurrency"`
}

type SandboxConfig struct {
	Mode            string `json:"mode"`
	WorkspaceAccess string `json:"workspace_access"`
	CustomCommand   string `json:"custom_command"`
	Scope           string `json:"scope"`
}

type SecurityConfig struct {
	PBKDF2Iterations int `json:"pbkdf2_iterations"`
	KeyBytes         int `json:"key_bytes"`
}

type RuntimeConfig struct {
	RootDir      string         `json:"root_dir"`
	ConfigDir    string         `json:"config_dir"`
	DataDir      string         `json:"data_dir"`
	SkillsDir    string         `json:"skills_dir"`
	CacheDir     string         `json:"cache_dir"`
	LogsDir      string         `json:"logs_dir"`
	RunDir       string         `json:"run_dir"`
	TmpDir       string         `json:"tmp_dir"`
	WorkspaceDir string         `json:"workspace_dir"`
	Server       ServerConfig   `json:"server"`
	Queue        QueueConfig    `json:"queue"`
	Sandbox      SandboxConfig  `json:"sandbox"`
	Security     SecurityConfig `json:"security"`
	ReleaseRepo  string         `json:"release_repo"`
}

func DefaultConfig() RuntimeConfig {
	rootDir := DefaultRootDir()
	return RuntimeConfig{
		RootDir:      rootDir,
		ConfigDir:    filepath.Join(rootDir, "config"),
		DataDir:      filepath.Join(rootDir, "data"),
		SkillsDir:    filepath.Join(rootDir, "skills"),
		CacheDir:     filepath.Join(rootDir, "cache"),
		LogsDir:      filepath.Join(rootDir, "logs"),
		RunDir:       filepath.Join(rootDir, "run"),
		TmpDir:       filepath.Join(rootDir, "tmp"),
		WorkspaceDir: filepath.Join(rootDir, "data", "workspaces"),
		Server: ServerConfig{
			Host: "127.0.0.1",
			Port: DefaultHTTPPort,
			TLS:  false,
		},
		Queue: QueueConfig{
			MainLaneConcurrency: 4,
			SubLaneConcurrency:  8,
			GlobalConcurrency:   16,
		},
		Sandbox: SandboxConfig{
			Mode:            "native",
			WorkspaceAccess: "rw",
			Scope:           "agent",
		},
		Security: SecurityConfig{
			PBKDF2Iterations: 100000,
			KeyBytes:         32,
		},
		ReleaseRepo: "xclaw/agent",
	}
}

func DefaultRootDir() string {
	switch runtime.GOOS {
	case "windows":
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			return filepath.Join(appData, "xclaw")
		}
		if dir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(dir) != "" {
			return filepath.Join(dir, "xclaw")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return filepath.Join(home, "Library", "Application Support", "xclaw")
		}
		if dir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(dir) != "" {
			return filepath.Join(dir, "xclaw")
		}
	default:
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return filepath.Join(home, ".xclaw")
		}
	}
	return filepath.Join(".", "xclaw")
}

func ConfigFilePath(rootDir string) string {
	return filepath.Join(rootDir, "config", "config.json")
}

func legacyConfigFilePaths(rootDir string) []string {
	return []string{
		filepath.Join(rootDir, "config", "config.yaml"),
		filepath.Join(rootDir, "config.yaml"),
		filepath.Join(rootDir, "config.json"),
	}
}

func LoadOrInit(dataDir string) (RuntimeConfig, error) {
	rootDir := strings.TrimSpace(dataDir)
	if rootDir == "" {
		rootDir = DefaultRootDir()
	}
	rootDir = filepath.Clean(rootDir)

	if err := migrateLegacyDefaultRoot(rootDir); err != nil {
		return RuntimeConfig{}, fmt.Errorf("migrate legacy root: %w", err)
	}

	cfg := DefaultConfig()
	cfg.RootDir = rootDir
	normalize(&cfg)

	if err := migrateLegacyLayout(&cfg); err != nil {
		return RuntimeConfig{}, fmt.Errorf("migrate legacy layout: %w", err)
	}
	if err := ensureLayout(cfg); err != nil {
		return RuntimeConfig{}, err
	}

	fp := ConfigFilePath(cfg.RootDir)
	b, err := os.ReadFile(fp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			var legacyBytes []byte
			var legacyErr error
			for _, legacy := range legacyConfigFilePaths(cfg.RootDir) {
				legacyBytes, legacyErr = os.ReadFile(legacy)
				if legacyErr == nil {
					break
				}
			}
			if legacyErr != nil {
				if err := Save(cfg); err != nil {
					return RuntimeConfig{}, err
				}
				return cfg, nil
			}
			b = legacyBytes
		} else {
			return RuntimeConfig{}, fmt.Errorf("read config: %w", err)
		}
	}

	if err := json.Unmarshal(b, &cfg); err != nil {
		return RuntimeConfig{}, fmt.Errorf("decode config: %w", err)
	}
	cfg.RootDir = rootDir
	normalize(&cfg)
	if err := Save(cfg); err != nil {
		return RuntimeConfig{}, err
	}
	return cfg, nil
}

func Save(cfg RuntimeConfig) error {
	normalize(&cfg)
	if err := ensureLayout(cfg); err != nil {
		return err
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	fp := ConfigFilePath(cfg.RootDir)
	if err := os.WriteFile(fp, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func normalize(cfg *RuntimeConfig) {
	if strings.TrimSpace(cfg.RootDir) == "" {
		cfg.RootDir = DefaultRootDir()
	}
	cfg.RootDir = filepath.Clean(cfg.RootDir)
	if cfg.ConfigDir == "" || !isWithinRoot(cfg.ConfigDir, cfg.RootDir) {
		cfg.ConfigDir = filepath.Join(cfg.RootDir, "config")
	}
	if cfg.DataDir == "" || !isWithinRoot(cfg.DataDir, cfg.RootDir) || filepath.Clean(cfg.DataDir) == cfg.RootDir {
		cfg.DataDir = filepath.Join(cfg.RootDir, "data")
	}
	if cfg.SkillsDir == "" || !isWithinRoot(cfg.SkillsDir, cfg.RootDir) {
		cfg.SkillsDir = filepath.Join(cfg.RootDir, "skills")
	}
	if cfg.CacheDir == "" || !isWithinRoot(cfg.CacheDir, cfg.RootDir) {
		cfg.CacheDir = filepath.Join(cfg.RootDir, "cache")
	}
	if cfg.LogsDir == "" || !isWithinRoot(cfg.LogsDir, cfg.RootDir) {
		cfg.LogsDir = filepath.Join(cfg.RootDir, "logs")
	}
	if cfg.RunDir == "" || !isWithinRoot(cfg.RunDir, cfg.RootDir) {
		cfg.RunDir = filepath.Join(cfg.RootDir, "run")
	}
	if cfg.TmpDir == "" || !isWithinRoot(cfg.TmpDir, cfg.RootDir) {
		cfg.TmpDir = filepath.Join(cfg.RootDir, "tmp")
	}
	if cfg.WorkspaceDir == "" || !isWithinRoot(cfg.WorkspaceDir, cfg.DataDir) || filepath.Clean(cfg.WorkspaceDir) == filepath.Join(cfg.RootDir, "workspaces") {
		cfg.WorkspaceDir = filepath.Join(cfg.DataDir, "workspaces")
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = "127.0.0.1"
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = DefaultHTTPPort
	}

	if cfg.Queue.MainLaneConcurrency <= 0 {
		cfg.Queue.MainLaneConcurrency = 4
	}
	if cfg.Queue.SubLaneConcurrency <= 0 {
		cfg.Queue.SubLaneConcurrency = 8
	}
	if cfg.Queue.GlobalConcurrency <= 0 {
		cfg.Queue.GlobalConcurrency = 16
	}

	switch cfg.Sandbox.Mode {
	case "off", "native", "custom":
	default:
		cfg.Sandbox.Mode = "native"
	}
	switch cfg.Sandbox.WorkspaceAccess {
	case "none", "ro", "rw":
	default:
		cfg.Sandbox.WorkspaceAccess = "ro"
	}
	switch cfg.Sandbox.Scope {
	case "session", "agent", "shared":
	default:
		cfg.Sandbox.Scope = "agent"
	}
	cfg.Sandbox.CustomCommand = filepath.Clean(strings.TrimSpace(cfg.Sandbox.CustomCommand))
	if cfg.Sandbox.CustomCommand == "." {
		cfg.Sandbox.CustomCommand = ""
	}

	if cfg.Security.PBKDF2Iterations < 100000 {
		cfg.Security.PBKDF2Iterations = 100000
	}
	if cfg.Security.KeyBytes != 16 && cfg.Security.KeyBytes != 24 && cfg.Security.KeyBytes != 32 {
		cfg.Security.KeyBytes = 32
	}
	if strings.TrimSpace(cfg.ReleaseRepo) == "" {
		cfg.ReleaseRepo = "xclaw/agent"
	}
}

func ensureLayout(cfg RuntimeConfig) error {
	for _, dir := range []string{
		cfg.RootDir,
		cfg.ConfigDir,
		cfg.DataDir,
		cfg.SkillsDir,
		cfg.CacheDir,
		cfg.LogsDir,
		cfg.RunDir,
		cfg.TmpDir,
		cfg.WorkspaceDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	return nil
}

func isWithinRoot(path, root string) bool {
	path = filepath.Clean(strings.TrimSpace(path))
	root = filepath.Clean(strings.TrimSpace(root))
	if path == "" || root == "" {
		return false
	}
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "")
}

func defaultLegacyRootDir() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".agent")
}

func migrateLegacyDefaultRoot(rootDir string) error {
	legacyRoot := defaultLegacyRootDir()
	if legacyRoot == "" {
		return nil
	}
	if filepath.Clean(rootDir) != filepath.Clean(DefaultRootDir()) || filepath.Clean(rootDir) == filepath.Clean(legacyRoot) {
		return nil
	}
	if !hasLegacyRootLayout(legacyRoot) || hasModernLayout(rootDir) {
		return nil
	}
	return migrateRootEntries(legacyRoot, rootDir)
}

func migrateLegacyLayout(cfg *RuntimeConfig) error {
	return migrateRootEntries(cfg.RootDir, cfg.RootDir)
}

func hasLegacyRootLayout(rootDir string) bool {
	for _, name := range []string{
		"config.json",
		"config.yaml",
		"xclaw.db",
		"workspaces",
		"skills",
		"keyring.dat",
		"cert.pem",
		"key.pem",
		"uploads",
		"knowledge",
		"exports",
		"agent-daemon.log",
		"agent.pid",
		"runtime-status.json",
	} {
		if _, err := os.Stat(filepath.Join(rootDir, name)); err == nil {
			return true
		}
	}
	return false
}

func hasModernLayout(rootDir string) bool {
	for _, name := range []string{"config", "data", "skills", "cache", "logs", "run", "tmp"} {
		if _, err := os.Stat(filepath.Join(rootDir, name)); err == nil {
			return true
		}
	}
	return false
}

func migrateRootEntries(srcRoot, dstRoot string) error {
	if strings.TrimSpace(srcRoot) == "" || strings.TrimSpace(dstRoot) == "" {
		return nil
	}
	srcRoot = filepath.Clean(srcRoot)
	dstRoot = filepath.Clean(dstRoot)
	cfg := DefaultConfig()
	cfg.RootDir = dstRoot
	normalize(&cfg)

	moves := [][2]string{
		{filepath.Join(srcRoot, "config.json"), ConfigFilePath(dstRoot)},
		{filepath.Join(srcRoot, "config.yaml"), ConfigFilePath(dstRoot)},
		{filepath.Join(srcRoot, "xclaw.db"), filepath.Join(cfg.DataDir, "xclaw.db")},
		{filepath.Join(srcRoot, "workspaces"), cfg.WorkspaceDir},
		{filepath.Join(srcRoot, "skills"), cfg.SkillsDir},
		{filepath.Join(srcRoot, "keyring.dat"), filepath.Join(cfg.ConfigDir, "keyring.dat")},
		{filepath.Join(srcRoot, "cert.pem"), filepath.Join(cfg.ConfigDir, "cert.pem")},
		{filepath.Join(srcRoot, "key.pem"), filepath.Join(cfg.ConfigDir, "key.pem")},
		{filepath.Join(srcRoot, "uploads"), filepath.Join(cfg.DataDir, "uploads")},
		{filepath.Join(srcRoot, "knowledge"), filepath.Join(cfg.DataDir, "knowledge")},
		{filepath.Join(srcRoot, "exports"), filepath.Join(cfg.DataDir, "exports")},
		{filepath.Join(srcRoot, "agent-daemon.log"), filepath.Join(cfg.LogsDir, "agent-daemon.log")},
		{filepath.Join(srcRoot, "agent.pid"), filepath.Join(cfg.RunDir, "agent.pid")},
		{filepath.Join(srcRoot, "runtime-status.json"), filepath.Join(cfg.RunDir, "runtime-status.json")},
	}

	for _, move := range moves {
		if err := movePath(move[0], move[1]); err != nil {
			return err
		}
	}
	return nil
}

func movePath(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := movePath(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
		_ = os.Remove(src)
		return nil
	}
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(src)
		return nil
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Remove(src)
}

func TrustInstruction() string {
	switch runtime.GOOS {
	case "darwin":
		return "sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain cert.pem"
	case "windows":
		return "certutil -addstore -f Root cert.pem"
	default:
		return "sudo cp cert.pem /usr/local/share/ca-certificates/xclaw.crt && sudo update-ca-certificates"
	}
}
