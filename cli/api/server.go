package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"xclaw/cli/app"
	"xclaw/cli/approval"
	"xclaw/cli/config"
	"xclaw/cli/db"
	"xclaw/cli/engine"
	"xclaw/cli/gateway"
	"xclaw/cli/heartbeat"
	"xclaw/cli/llm"
	"xclaw/cli/mcpclient"
	"xclaw/cli/models"
	"xclaw/cli/protocol"
	"xclaw/cli/scheduler"
	"xclaw/cli/skills"
	"xclaw/cli/updater"
	"xclaw/cli/webui"
)

type Server struct {
	cfg          config.RuntimeConfig
	store        *db.Store
	app          *app.Service
	engine       *engine.Service
	events       *engine.EventHub
	static       http.Handler
	a2a          *protocol.A2AHandler
	mcp          *protocol.MCPServer
	mcpClients   *mcpclient.Manager
	monitor      *llm.TokenMonitor
	market       *skills.Market
	skillLoader  *skills.Loader
	approver     *approval.Manager
	localGateway *llm.LocalGateway
	discovery    *protocol.Discovery
	gateway      *gateway.Gateway
	heartbeat    *heartbeat.Runner
	runtimeHooks RuntimeControlHooks
	drainCheck   func() bool
	a2aClientFn  func(baseURL string) *protocol.A2AClient
	activeHTTP   atomic.Int64
	activeSSE    atomic.Int64
	activeWS     atomic.Int64
}

type RuntimeControlHooks struct {
	Restart func() error
	Update  func(ctx context.Context, channel string) (map[string]any, error)
}

type runtimeStatusSnapshot struct {
	Version        string `json:"version"`
	Draining       bool   `json:"draining"`
	ActiveRequests int64  `json:"active_requests"`
	ActiveSSE      int64  `json:"active_sse"`
	ActiveWS       int64  `json:"active_ws"`
}

func NewServer(cfg config.RuntimeConfig, store *db.Store, appSvc *app.Service, eng *engine.Service, events *engine.EventHub, monitor *llm.TokenMonitor, approver *approval.Manager, localGateway *llm.LocalGateway, gatewaySvc *gateway.Gateway, heartbeatRunner *heartbeat.Runner, mcpMgr *mcpclient.Manager) (*Server, error) {
	sub, err := webui.SubFS()
	if err != nil {
		return nil, fmt.Errorf("load embedded static dist: %w", err)
	}

	static := http.FileServer(http.FS(sub))

	// Initialize A2A handler
	a2aHandler := protocol.NewA2AHandler("xclaw")
	if token := strings.TrimSpace(os.Getenv("XCLAW_A2A_TOKEN")); token != "" {
		a2aHandler.SetAuthToken(token)
	}
	a2aHandler.OnTask(func(ctx context.Context, task protocol.A2ATask) (protocol.A2AResult, error) {
		// Route A2A tasks to local engine
		agentID := strings.TrimSpace(task.Inputs["agent_id"])
		if agentID == "" {
			items, listErr := eng.ListAgents(ctx)
			if listErr != nil || len(items) == 0 {
				errText := "no available local agent for A2A task"
				if listErr != nil {
					errText = listErr.Error()
				}
				return protocol.A2AResult{TaskID: task.ID, Status: "failed", Error: errText}, nil
			}
			agentID = items[0].ID
		}
		session, err := eng.CreateSession(ctx, agentID, "A2A: "+task.Name, false)
		if err != nil {
			return protocol.A2AResult{TaskID: task.ID, Status: "failed", Error: err.Error()}, nil
		}
		msg, err := eng.SendMessage(ctx, session.ID, engine.SendMessageInput{
			Content:     task.Description,
			AutoApprove: true,
		})
		if err != nil {
			return protocol.A2AResult{TaskID: task.ID, Status: "failed", Error: err.Error()}, nil
		}
		return protocol.A2AResult{
			TaskID: task.ID,
			Status: "success",
			Output: msg.Content,
		}, nil
	})

	// Initialize MCP server
	mcpServer := protocol.NewMCPServer("xclaw", "1.0.0")
	mcpServer.SetCapabilities([]protocol.MCPCapability{
		{Name: "tools", Version: "1.0"},
		{Name: "resources", Version: "1.0"},
	})

	if mcpMgr == nil {
		mcpMgr = mcpclient.NewManager()
	}
	srv := &Server{
		cfg:          cfg,
		store:        store,
		app:          appSvc,
		engine:       eng,
		events:       events,
		static:       static,
		a2a:          a2aHandler,
		mcp:          mcpServer,
		mcpClients:   mcpMgr,
		monitor:      monitor,
		market:       skills.NewMarket(cfg.SkillsDir),
		approver:     approver,
		localGateway: localGateway,
		gateway:      gatewaySvc,
		heartbeat:    heartbeatRunner,
		a2aClientFn:  protocol.NewA2AClient,
	}

	if err := skills.EnsureBuiltinSkills(cfg.SkillsDir); err != nil {
		return nil, fmt.Errorf("ensure builtin skills: %w", err)
	}
	skillLoader := skills.NewLoader(cfg.SkillsDir, cfg.WorkspaceDir)
	if _, err := skillLoader.LoadAll(); err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}
	srv.skillLoader = skillLoader
	eng.SetSkillLoader(skillLoader)
	registryURL := strings.TrimSpace(os.Getenv("XCLAW_A2A_REGISTRY"))
	if registryURL == "" {
		if saved, ok, err := store.GetSetting(context.Background(), "a2a_registry_url"); err == nil && ok {
			registryURL = strings.TrimSpace(saved)
		}
	}
	if registryURL != "" || cfg.Server.Port > 0 {
		card := protocol.DefaultLocalCard(cfg.Server.Host, cfg.Server.Port, cfg.Server.TLS)
		srv.discovery = protocol.NewDiscovery(card, registryURL)
		srv.discovery.Start(context.Background())
	}
	srv.loadApprovalState()
	srv.loadModelRouting()
	srv.loadMCPServers()
	if srv.gateway != nil {
		srv.gateway.SetInboundHandler(srv.handleGatewayInboundEvent)
	}
	return srv, nil
}

func (s *Server) Close() {
	if s.discovery != nil {
		s.discovery.Close()
	}
}

func (s *Server) SetRuntimeControlHooks(hooks RuntimeControlHooks) {
	s.runtimeHooks = hooks
}

func (s *Server) SetDrainCheck(fn func() bool) {
	s.drainCheck = fn
}

func (s *Server) ActiveRequestCount() int64 {
	return s.activeHTTP.Load()
}

func (s *Server) ActiveSSECount() int64 {
	return s.activeSSE.Load()
}

func (s *Server) ActiveWebSocketCount() int64 {
	return s.activeWS.Load()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/system/status", s.handleSystemStatus)
	mux.HandleFunc("/api/system/bootstrap", s.handleBootstrap)
	mux.HandleFunc("/api/system/trust-hint", s.handleTrustHint)
	mux.HandleFunc("/api/system/update-check", s.handleUpdateCheck)
	mux.HandleFunc("/api/system/update-install", s.handleUpdateInstall)
	mux.HandleFunc("/api/system/restart", s.handleSystemRestart)
	mux.HandleFunc("/api/system/metrics", s.handleSystemMetrics)
	mux.HandleFunc("/api/system/vector-status", s.handleVectorStatus)
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)
	mux.HandleFunc("/api/system/token-stats", s.handleTokenStats)
	mux.HandleFunc("/api/a2a/card", s.handleA2ACard)
	mux.HandleFunc("/api/a2a/peers", s.handleA2APeers)
	mux.HandleFunc("/api/a2a/peers/register", s.handleA2APeerRegister)
	mux.HandleFunc("/api/a2a/dispatch", s.handleA2ADispatch)
	mux.HandleFunc("/api/a2a/tasks", s.handleA2ATasks)
	mux.HandleFunc("/api/a2a/tasks/", s.handleA2ATaskByID)
	mux.HandleFunc("/register", s.handleA2ARegistryRegister)
	mux.HandleFunc("/peers", s.handleA2ARegistryPeers)
	mux.HandleFunc("/api/mcp/proxy", s.handleMCPProxy)
	mux.HandleFunc("/api/mcp/servers", s.handleMCPServers)
	mux.HandleFunc("/api/mcp/servers/test", s.handleMCPServerTest)
	mux.HandleFunc("/api/mcp/tools", s.handleMCPTools)
	mux.HandleFunc("/api/skills/catalog", s.handleSkillCatalog)
	mux.HandleFunc("/api/skills/market-url", s.handleSkillMarketURL)
	mux.HandleFunc("/api/skills/installed", s.handleSkillInstalled)
	mux.HandleFunc("/api/skills/install", s.handleSkillInstall)
	mux.HandleFunc("/api/skills/uninstall", s.handleSkillUninstall)
	mux.HandleFunc("/api/skills/list", s.handleSkillList)
	mux.HandleFunc("/api/skills/detail", s.handleSkillDetail)
	mux.HandleFunc("/api/skills/reload", s.handleSkillReload)
	mux.HandleFunc("/api/multimodal/upload", s.handleMultimodalUpload)
	mux.HandleFunc("/api/multimodal/files", s.handleMultimodalFiles)
	mux.HandleFunc("/api/multimodal/analyze", s.handleMultimodalAnalyze)
	mux.HandleFunc("/api/multimodal/render", s.handleMultimodalRender)
	mux.HandleFunc("/api/audio/stt", s.handleAudioSTT)
	mux.HandleFunc("/api/audio/tts", s.handleAudioTTS)
	mux.HandleFunc("/api/knowledge/mounts", s.handleKnowledgeMounts)
	mux.HandleFunc("/api/knowledge/reindex", s.handleKnowledgeReindex)
	mux.HandleFunc("/api/knowledge/search", s.handleKnowledgeSearch)
	mux.HandleFunc("/api/system/model-routing", s.handleModelRouting)
	mux.HandleFunc("/api/memory/vector/search", s.handleVectorMemorySearch)
	mux.HandleFunc("/api/approvals/rules", s.handleApprovalRules)
	mux.HandleFunc("/api/approvals/requests", s.handleApprovalRequests)
	mux.HandleFunc("/api/approvals/approve", s.handleApprovalApprove)
	mux.HandleFunc("/api/approvals/reject", s.handleApprovalReject)
	mux.HandleFunc("/api/gateway/config", s.handleGatewayConfig)
	mux.HandleFunc("/api/gateway/providers", s.handleGatewayProviders)
	mux.HandleFunc("/api/gateway/bindings", s.handleGatewayBindings)
	mux.HandleFunc("/api/gateway/bindings/", s.handleGatewayBindingByID)
	mux.HandleFunc("/api/gateway/routes", s.handleGatewayRoutes)
	mux.HandleFunc("/api/gateway/routes/", s.handleGatewayRouteByName)
	mux.HandleFunc("/api/gateway/send", s.handleGatewaySend)
	mux.HandleFunc("/api/gateway/dlq", s.handleGatewayDLQ)
	mux.HandleFunc("/api/gateway/dlq/", s.handleGatewayDLQByID)
	mux.HandleFunc("/api/gateway/dlq/batch-retry", s.handleGatewayDLQBatchRetry)
	mux.HandleFunc("/api/gateway/dlq/purge", s.handleGatewayDLQPurge)
	mux.HandleFunc("/api/gateway/webhook/", s.handleGatewayWebhook)
	mux.HandleFunc("/api/heartbeat/config", s.handleHeartbeatConfig)
	mux.HandleFunc("/api/heartbeat/runs", s.handleHeartbeatRuns)
	mux.HandleFunc("/api/heartbeat/run", s.handleHeartbeatRunNow)

	mux.HandleFunc("/api/agents", s.handleAgents)
	mux.HandleFunc("/api/agents/", s.handleAgentByID)

	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/", s.handleSessionSubRoutes)

	mux.HandleFunc("/api/audit", s.handleAudit)

	mux.HandleFunc("/api/cron", s.handleCron)
	mux.HandleFunc("/api/cron/", s.handleCronByID)

	mux.HandleFunc("/api/credentials", s.handleCredential)
	mux.HandleFunc("/api/credentials/reveal", s.handleCredentialReveal)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-XClaw-PID", strconv.Itoa(os.Getpid()))
		w.Header().Set("X-XClaw-Active-Requests", strconv.FormatInt(s.ActiveRequestCount(), 10))
		w.Header().Set("X-XClaw-Active-SSE", strconv.FormatInt(s.ActiveSSECount(), 10))
		w.Header().Set("X-XClaw-Active-WS", strconv.FormatInt(s.ActiveWebSocketCount(), 10))
		w.Header().Set("X-XClaw-Draining", strconv.FormatBool(s.isDraining()))
		if s.isDraining() {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("draining"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// A2A protocol endpoint
	mux.HandleFunc("/a2a", s.a2a.ServeHTTP)

	// MCP protocol endpoint
	mux.HandleFunc("/mcp", s.mcp.ServeHTTP)

	mux.HandleFunc("/", s.handleWeb)
	return withCORS(s.withRequestTracking(s.withDrain(s.withAuth(mux))))
}

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	boot, err := s.app.IsBootstrapped(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	agents, err := s.engine.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sessions, err := s.engine.ListSessions(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	runningSessions := 0
	recoveringSessions := 0
	for _, sess := range sessions {
		switch strings.ToLower(strings.TrimSpace(sess.Status)) {
		case "running":
			runningSessions++
		case "recovering":
			recoveringSessions++
		}
	}
	snapshot := s.readRuntimeStatusSnapshot()

	writeJSON(w, http.StatusOK, map[string]any{
		"bootstrapped": boot,
		"version":      snapshot.Version,
		"server": map[string]any{
			"host": s.cfg.Server.Host,
			"port": s.cfg.Server.Port,
			"tls":  s.cfg.Server.TLS,
		},
		"sandbox":             s.cfg.Sandbox,
		"agents_count":        len(agents),
		"vector":              s.store.VectorStatus(),
		"draining":            snapshot.Draining || s.isDraining(),
		"active_requests":     maxInt64(snapshot.ActiveRequests, s.ActiveRequestCount()),
		"active_sse":          maxInt64(snapshot.ActiveSSE, s.ActiveSSECount()),
		"active_ws":           maxInt64(snapshot.ActiveWS, s.ActiveWebSocketCount()),
		"running_sessions":    runningSessions,
		"recovering_sessions": recoveringSessions,
	})
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var req app.BootstrapInput
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if err := s.app.Bootstrap(r.Context(), req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTrustHint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !s.cfg.Server.TLS {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "command": ""})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "command": config.TrustInstruction()})
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	release, err := updater.FetchLatest(r.Context(), http.DefaultClient, s.cfg.ReleaseRepo)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	asset, checksum, assetErr := updater.SelectPlatformAsset(release, "", "")
	payload := map[string]any{
		"available": true,
		"release":   release,
	}
	if assetErr == nil {
		payload["selected_asset"] = asset
		if checksum != nil {
			payload["checksum_asset"] = checksum
		}
	} else {
		payload["asset_error"] = assetErr.Error()
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.runtimeHooks.Update == nil {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("update control is not configured"))
		return
	}
	var req struct {
		Channel string `json:"channel"`
	}
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.runtimeHooks.Update(r.Context(), req.Channel)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.runtimeHooks.Restart == nil {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("restart control is not configured"))
		return
	}
	if err := s.runtimeHooks.Restart(); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"restarting": true,
	})
}

func (s *Server) handleSystemMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	agents, _ := s.engine.ListAgents(r.Context())
	sessions, _ := s.engine.ListSessions(r.Context(), "")
	audits, _ := s.store.ListAudit(r.Context(), "", 1000)
	cronJobs, _ := s.store.ListCronJobs(r.Context(), "", false)

	payload := map[string]any{
		"agents":               len(agents),
		"sessions":             len(sessions),
		"audit_logs_last_1000": len(audits),
		"cron_jobs":            len(cronJobs),
	}
	if s.monitor != nil {
		payload["tokens"] = s.monitor.GetGlobalStats()
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleVectorStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, s.store.VectorStatus())
}

func (s *Server) handleSkillCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	writeJSON(w, http.StatusOK, s.market.Catalog(query))
}

func (s *Server) handleSkillMarketURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"url": s.market.MarketURL(),
	})
}

func (s *Server) handleSkillInstalled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	items, err := s.market.ListInstalled()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleSkillInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req skills.InstallOptions
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	item, err := s.market.Install(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.skillLoader != nil {
		_, _ = s.skillLoader.LoadAll()
	}
	s.loadMCPServers()
	managed := []map[string]any{}
	if s.mcpClients != nil {
		for _, server := range s.mcpClients.Servers() {
			if strings.TrimSpace(server.ManagedBy) != "skill:"+item.Name {
				continue
			}
			managed = append(managed, map[string]any{
				"id":          server.ID,
				"name":        server.Name,
				"transport":   server.Transport,
				"url":         server.URL,
				"command":     server.Command,
				"args":        server.Args,
				"env":         server.Env,
				"enabled":     server.Enabled,
				"timeout_sec": server.TimeoutSec,
				"managed_by":  server.ManagedBy,
				"readonly":    server.Readonly,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"item":                   item,
		"registered_mcp_servers": managed,
	})
}

func (s *Server) handleSkillUninstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	removedServers := []map[string]any{}
	removedTools := []string{}
	if s.mcpClients != nil {
		serverIDs := make(map[string]struct{})
		for _, server := range s.mcpClients.Servers() {
			if strings.TrimSpace(server.ManagedBy) != "skill:"+strings.TrimSpace(req.Name) {
				continue
			}
			serverIDs[server.ID] = struct{}{}
			removedServers = append(removedServers, map[string]any{
				"id":          server.ID,
				"name":        server.Name,
				"transport":   server.Transport,
				"url":         server.URL,
				"command":     server.Command,
				"args":        server.Args,
				"env":         server.Env,
				"enabled":     server.Enabled,
				"timeout_sec": server.TimeoutSec,
				"managed_by":  server.ManagedBy,
				"readonly":    server.Readonly,
			})
		}
		if len(serverIDs) > 0 {
			for _, tool := range s.mcpClients.ListTools(r.Context()) {
				if _, ok := serverIDs[tool.ServerID]; ok {
					removedTools = append(removedTools, tool.FullName)
				}
			}
		}
	}
	if err := s.market.Uninstall(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.skillLoader != nil {
		_, _ = s.skillLoader.LoadAll()
	}
	s.loadMCPServers()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"removed_mcp_servers": removedServers,
		"removed_mcp_tools":   removedTools,
	})
}

func (s *Server) handleSkillList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.skillLoader == nil {
		writeJSON(w, http.StatusOK, []skills.LoadedSkill{})
		return
	}
	loaded, err := s.skillLoader.LoadAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, loaded)
}

func (s *Server) handleSkillDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name parameter is required"))
		return
	}
	if s.skillLoader == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("skill loader not initialized"))
		return
	}
	skill, ok := s.skillLoader.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("skill not found: %s", name))
		return
	}
	fullPrompt, err := s.skillLoader.GetFullPrompt(name)
	if err != nil {
		fullPrompt = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"skill":       skill,
		"full_prompt": fullPrompt,
	})
}

func (s *Server) handleSkillReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.skillLoader == nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("skill loader not initialized"))
		return
	}
	loaded, err := s.skillLoader.LoadAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.loadMCPServers()
	mcpCount := 0
	if s.mcpClients != nil {
		mcpCount = len(s.mcpClients.Servers())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"count":     len(loaded),
		"mcp_count": mcpCount,
	})
}

func (s *Server) handleMultimodalUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()

	uploadDir := filepath.Join(s.cfg.DataDir, "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	filename := fmt.Sprintf("%d_%s", time.Now().UnixNano(), sanitizeFilename(header.Filename))
	target := filepath.Join(uploadDir, filename)
	out, err := os.Create(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	size, err := io.Copy(out, file)
	_ = out.Close()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	scan, err := scanFileForViruses(r.Context(), target, header.Header.Get("Content-Type"))
	if err != nil {
		_ = os.Remove(target)
		writeError(w, http.StatusBadRequest, err)
		return
	}

	items, _ := s.loadMultimodalFiles(r.Context())
	item := map[string]any{
		"id":          engine.NewID("mm"),
		"name":        header.Filename,
		"stored_name": filename,
		"path":        target,
		"mime":        header.Header.Get("Content-Type"),
		"size":        size,
		"scan_mode":   scan.Mode,
		"scan_status": scan.Status,
		"scan_detail": scan.Detail,
		"scan_at":     time.Now().UTC(),
		"uploaded_at": time.Now().UTC(),
	}
	items = append(items, item)
	if err := s.saveMultimodalFiles(r.Context(), items); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) handleMultimodalFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	items, err := s.loadMultimodalFiles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleVectorMemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		AgentID string `json:"agent_id"`
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("query is required"))
		return
	}
	hits, err := s.store.SearchVectorMemory(r.Context(), buildHashEmbeddingForAPI(req.Query), req.Limit, strings.TrimSpace(req.AgentID))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) loadMultimodalFiles(ctx context.Context) ([]map[string]any, error) {
	raw, ok, err := s.store.GetSetting(ctx, "multimodal_files_json")
	if err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return []map[string]any{}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Server) saveMultimodalFiles(ctx context.Context, items []map[string]any) error {
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return s.store.SetSetting(ctx, "multimodal_files_json", string(raw))
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "..", "_")
	if name == "" {
		return "file"
	}
	return name
}

func buildHashEmbeddingForAPI(text string) []float32 {
	const dim = 256
	vec := make([]float32, dim)
	tokens := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(tokens) == 0 {
		return vec
	}
	for _, tok := range tokens {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum32()
		idx := int(sum % uint32(dim))
		sign := float32(1)
		if (sum>>31)&1 == 1 {
			sign = -1
		}
		vec[idx] += sign
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v * v)
	}
	if norm == 0 {
		return vec
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	token, err := s.app.Login(r.Context(), req.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":          token,
		"expires_in_sec": 24 * 3600,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	token := extractBearer(r.Header.Get("Authorization"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Auth-Token"))
	}
	if token != "" {
		s.app.Logout(token)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTokenStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.monitor == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentID != "" {
		writeJSON(w, http.StatusOK, s.monitor.GetStats(agentID))
		return
	}
	writeJSON(w, http.StatusOK, s.monitor.GetGlobalStats())
}

func (s *Server) handleA2ACard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	card := protocol.DefaultLocalCard(s.cfg.Server.Host, s.cfg.Server.Port, s.cfg.Server.TLS)
	if s.discovery != nil {
		card = s.discovery.SelfCard()
	}
	writeJSON(w, http.StatusOK, card)
}

func (s *Server) handleA2APeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	peers, err := s.collectA2APeerViews(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, peers)
}

func (s *Server) handleA2APeerRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	peers, err := s.loadPeers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	peer := map[string]string{
		"name":  strings.TrimSpace(req.Name),
		"url":   strings.TrimSpace(req.URL),
		"token": strings.TrimSpace(req.Token),
	}
	if peer["name"] == "" || peer["url"] == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name and url are required"))
		return
	}
	replaced := false
	for i := range peers {
		if peers[i]["name"] == peer["name"] {
			peers[i] = peer
			replaced = true
			break
		}
	}
	if !replaced {
		peers = append(peers, peer)
	}
	if err := s.savePeers(r.Context(), peers); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(peers)})
}

func (s *Server) handleA2ADispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		PeerURL    string            `json:"peer_url"`
		PeerName   string            `json:"peer_name"`
		Capability string            `json:"capability"`
		From       string            `json:"from"`
		To         string            `json:"to"`
		Task       string            `json:"task"`
		Token      string            `json:"token"`
		Inputs     map[string]string `json:"inputs"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Task) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("task is required"))
		return
	}
	if strings.TrimSpace(req.PeerURL) == "" && strings.TrimSpace(req.PeerName) == "" && strings.TrimSpace(req.Capability) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("peer_url, peer_name or capability is required"))
		return
	}
	peer, err := s.resolveA2APeer(r.Context(), req.PeerURL, req.PeerName, req.Capability)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	taskID := engine.NewID("a2a")
	record := a2aTaskRecord{
		ID:          taskID,
		PeerURL:     peer.URL,
		From:        strings.TrimSpace(req.From),
		To:          firstText(strings.TrimSpace(req.To), strings.TrimSpace(peer.Name)),
		Name:        "delegated-task",
		Description: strings.TrimSpace(req.Task),
		Inputs:      cloneStringMap(req.Inputs),
		Status:      "queued",
		Progress:    0,
		Source:      "dispatch",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		CallbackURL: s.buildA2ATaskStatusURL(r, taskID),
	}
	if token := strings.TrimSpace(req.Token); token != "" {
		if record.Inputs == nil {
			record.Inputs = map[string]string{}
		}
		record.Inputs["_a2a_token"] = token
	} else if token := strings.TrimSpace(peer.Token); token != "" {
		if record.Inputs == nil {
			record.Inputs = map[string]string{}
		}
		record.Inputs["_a2a_token"] = token
	}
	if err := s.upsertA2ATask(r.Context(), record); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	go s.dispatchA2ATask(record)
	writeJSON(w, http.StatusAccepted, record)
}

func (s *Server) handleA2ATasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	items, err := s.loadA2ATasks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleA2ATaskByID(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/a2a/tasks/")
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("task id is required"))
		return
	}
	if strings.HasSuffix(trimmed, "/status") {
		id := strings.TrimSuffix(trimmed, "/status")
		id = strings.Trim(id, "/")
		s.handleA2ATaskStatus(w, r, id)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	item, ok, err := s.findA2ATask(r.Context(), trimmed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("task not found"))
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleA2ATaskStatus(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var status protocol.A2ATaskStatus
	if err := decodeJSON(r, &status); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(status.TaskID) == "" {
		status.TaskID = id
	}
	if err := s.applyA2ATaskStatus(r.Context(), status); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	item, _, _ := s.findA2ATask(r.Context(), status.TaskID)
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleA2ARegistryRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var card protocol.AgentCard
	if err := decodeJSON(r, &card); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	card.Endpoint = strings.TrimSpace(card.Endpoint)
	card.Name = strings.TrimSpace(card.Name)
	if card.Endpoint == "" || card.Name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name and endpoint are required"))
		return
	}
	card.Source = "registry"
	card.LastSeenAt = time.Now().UTC()
	if err := s.upsertA2ARegistryPeer(r.Context(), card); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleA2ARegistryPeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	items, err := s.loadA2ARegistryPeers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

type a2aTaskRecord struct {
	ID          string              `json:"id"`
	PeerURL     string              `json:"peer_url"`
	From        string              `json:"from"`
	To          string              `json:"to"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Inputs      map[string]string   `json:"inputs,omitempty"`
	Status      string              `json:"status"`
	Progress    int                 `json:"progress,omitempty"`
	Error       string              `json:"error,omitempty"`
	Output      string              `json:"output,omitempty"`
	Result      *protocol.A2AResult `json:"result,omitempty"`
	Source      string              `json:"source,omitempty"`
	CallbackURL string              `json:"callback_url,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
	StartedAt   *time.Time          `json:"started_at,omitempty"`
	CompletedAt *time.Time          `json:"completed_at,omitempty"`
}

type a2aPeerView struct {
	ID           string    `json:"id,omitempty"`
	Name         string    `json:"name,omitempty"`
	URL          string    `json:"url"`
	Endpoint     string    `json:"endpoint,omitempty"`
	Description  string    `json:"description,omitempty"`
	Source       string    `json:"source,omitempty"`
	Sources      []string  `json:"sources,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
	Protocols    []string  `json:"protocols,omitempty"`
	TaskTypes    []string  `json:"task_types,omitempty"`
	LastSeenAt   time.Time `json:"last_seen_at,omitempty"`
	HasToken     bool      `json:"has_token,omitempty"`
	Token        string    `json:"-"`
}

func (s *Server) dispatchA2ATask(record a2aTaskRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	_ = s.applyA2ATaskStatus(ctx, protocol.A2ATaskStatus{
		TaskID:    record.ID,
		State:     "running",
		Message:   "dispatch started",
		Progress:  10,
		UpdatedAt: time.Now().UTC(),
	})

	clientFn := s.a2aClientFn
	if clientFn == nil {
		clientFn = protocol.NewA2AClient
	}
	client := clientFn(strings.TrimSpace(record.PeerURL))
	if token := strings.TrimSpace(record.Inputs["_a2a_token"]); token != "" {
		client.SetAuthToken(token)
	}
	result, err := client.SendTask(ctx, record.From, record.To, protocol.A2ATask{
		ID:          record.ID,
		Name:        record.Name,
		Description: record.Description,
		Inputs:      cloneStringMap(record.Inputs),
		Priority:    3,
		CallbackURL: record.CallbackURL,
	})
	if err != nil {
		_ = s.applyA2ATaskStatus(ctx, protocol.A2ATaskStatus{
			TaskID:    record.ID,
			State:     "failed",
			Message:   err.Error(),
			Result:    &protocol.A2AResult{TaskID: record.ID, Status: "failed", Error: err.Error()},
			UpdatedAt: time.Now().UTC(),
		})
		return
	}
	state := "completed"
	progress := 100
	switch strings.ToLower(strings.TrimSpace(result.Status)) {
	case "accepted", "queued":
		state = "accepted"
		progress = 20
	case "running", "in_progress", "processing":
		state = "running"
		progress = 40
	case "failed", "error":
		state = "failed"
	case "partial", "partial_success":
		state = "partial"
	case "success", "succeeded", "completed", "complete", "":
		state = "completed"
	default:
		state = strings.ToLower(strings.TrimSpace(result.Status))
	}
	_ = s.applyA2ATaskStatus(ctx, protocol.A2ATaskStatus{
		TaskID:    record.ID,
		State:     state,
		Message:   result.Status,
		Progress:  progress,
		Result:    result,
		UpdatedAt: time.Now().UTC(),
	})
}

func (s *Server) handleMCPProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		URL    string          `json:"url"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		ID     any             `json:"id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	body, _ := json.Marshal(protocol.MCPRequest{
		JSONRPC: "2.0",
		ID:      req.ID,
		Method:  req.Method,
		Params:  req.Params,
	})
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, req.URL, strings.NewReader(string(body)))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		writeError(w, http.StatusBadGateway, fmt.Errorf("mcp upstream status %d", resp.StatusCode))
		return
	}
	var result any
	if err := json.Unmarshal(raw, &result); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) loadPeers(ctx context.Context) ([]map[string]string, error) {
	raw, ok, err := s.store.GetSetting(ctx, "a2a_peers_json")
	if err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return []map[string]string{}, nil
	}
	var peers []map[string]string
	if err := json.Unmarshal([]byte(raw), &peers); err != nil {
		return nil, err
	}
	return peers, nil
}

func (s *Server) savePeers(ctx context.Context, peers []map[string]string) error {
	raw, err := json.Marshal(peers)
	if err != nil {
		return err
	}
	return s.store.SetSetting(ctx, "a2a_peers_json", string(raw))
}

func (s *Server) collectA2APeerViews(ctx context.Context) ([]a2aPeerView, error) {
	byEndpoint := make(map[string]*a2aPeerView)
	upsert := func(endpoint string) *a2aPeerView {
		endpoint = normalizeA2AEndpoint(endpoint)
		if endpoint == "" {
			return nil
		}
		if existing, ok := byEndpoint[endpoint]; ok {
			return existing
		}
		view := &a2aPeerView{
			URL:      normalizeA2ABaseURL(endpoint),
			Endpoint: endpoint,
		}
		byEndpoint[endpoint] = view
		return view
	}
	addSource := func(view *a2aPeerView, source string) {
		source = strings.TrimSpace(source)
		if view == nil || source == "" {
			return
		}
		for _, existing := range view.Sources {
			if strings.EqualFold(existing, source) {
				return
			}
		}
		view.Sources = append(view.Sources, source)
		sort.Strings(view.Sources)
		view.Source = strings.Join(view.Sources, ",")
	}
	mergeCard := func(view *a2aPeerView, card protocol.AgentCard, fallbackSource string) {
		if view == nil {
			return
		}
		view.ID = firstText(view.ID, card.ID)
		view.Name = firstText(view.Name, card.Name)
		view.Description = firstText(view.Description, card.Description)
		view.Capabilities = appendUniqueFold(view.Capabilities, card.Capabilities...)
		view.Protocols = appendUniqueFold(view.Protocols, card.Protocols...)
		view.TaskTypes = appendUniqueFold(view.TaskTypes, card.TaskTypes...)
		if view.LastSeenAt.IsZero() || (!card.LastSeenAt.IsZero() && card.LastSeenAt.After(view.LastSeenAt)) {
			view.LastSeenAt = card.LastSeenAt
		}
		addSource(view, firstText(card.Source, fallbackSource))
	}

	manualPeers, err := s.loadPeers(ctx)
	if err != nil {
		return nil, err
	}
	for _, peer := range manualPeers {
		view := upsert(peer["url"])
		if view == nil {
			continue
		}
		view.Name = firstText(strings.TrimSpace(peer["name"]), view.Name)
		if token := strings.TrimSpace(peer["token"]); token != "" {
			view.HasToken = true
			view.Token = token
		}
		addSource(view, "manual")
	}
	if s.discovery != nil {
		for _, peer := range s.discovery.ListPeers() {
			mergeCard(upsert(peer.Endpoint), peer, "discovery")
		}
	}
	registryPeers, err := s.loadA2ARegistryPeers(ctx)
	if err != nil {
		return nil, err
	}
	for _, peer := range registryPeers {
		mergeCard(upsert(peer.Endpoint), peer, "registry")
	}

	views := make([]a2aPeerView, 0, len(byEndpoint))
	for _, view := range byEndpoint {
		if strings.TrimSpace(view.URL) == "" {
			continue
		}
		views = append(views, *view)
	}
	sort.Slice(views, func(i, j int) bool {
		left := strings.ToLower(firstText(views[i].Name, views[i].URL))
		right := strings.ToLower(firstText(views[j].Name, views[j].URL))
		if left == right {
			return views[i].URL < views[j].URL
		}
		return left < right
	})
	return views, nil
}

func (s *Server) resolveA2APeer(ctx context.Context, peerURL, peerName, capability string) (a2aPeerView, error) {
	peerURL = strings.TrimSpace(peerURL)
	peerName = strings.TrimSpace(peerName)
	capability = strings.TrimSpace(capability)

	views, err := s.collectA2APeerViews(ctx)
	if err != nil {
		return a2aPeerView{}, err
	}

	if peerURL != "" {
		baseURL := normalizeA2ABaseURL(peerURL)
		endpoint := normalizeA2AEndpoint(peerURL)
		for _, peer := range views {
			if sameText(peer.URL, baseURL) || sameText(peer.Endpoint, endpoint) {
				return peer, nil
			}
		}
		return a2aPeerView{
			URL:      baseURL,
			Endpoint: endpoint,
			Source:   "direct",
			Sources:  []string{"direct"},
		}, nil
	}

	if peerName != "" {
		var matched *a2aPeerView
		for i := range views {
			if !sameText(views[i].Name, peerName) {
				continue
			}
			if matched == nil || views[i].HasToken || views[i].LastSeenAt.After(matched.LastSeenAt) {
				matched = &views[i]
			}
		}
		if matched != nil {
			return *matched, nil
		}
		return a2aPeerView{}, fmt.Errorf("a2a peer not found by name: %s", peerName)
	}

	var matched *a2aPeerView
	for i := range views {
		if !matchesA2ACapability(views[i], capability) {
			continue
		}
		if matched == nil || views[i].LastSeenAt.After(matched.LastSeenAt) {
			matched = &views[i]
		}
	}
	if matched != nil {
		return *matched, nil
	}
	return a2aPeerView{}, fmt.Errorf("a2a peer not found for capability: %s", capability)
}

func (s *Server) loadA2ARegistryPeers(ctx context.Context) ([]protocol.AgentCard, error) {
	raw, ok, err := s.store.GetSetting(ctx, "a2a_registry_runtime_json")
	if err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return []protocol.AgentCard{}, nil
	}
	var items []protocol.AgentCard
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	filtered := make([]protocol.AgentCard, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.Endpoint) == "" {
			continue
		}
		if item.LastSeenAt.IsZero() || now.Sub(item.LastSeenAt) > 2*time.Minute {
			continue
		}
		filtered = append(filtered, item)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].LastSeenAt.After(filtered[j].LastSeenAt)
	})
	return filtered, nil
}

func (s *Server) saveA2ARegistryPeers(ctx context.Context, items []protocol.AgentCard) error {
	sort.Slice(items, func(i, j int) bool {
		return items[i].LastSeenAt.After(items[j].LastSeenAt)
	})
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return s.store.SetSetting(ctx, "a2a_registry_runtime_json", string(raw))
}

func (s *Server) upsertA2ARegistryPeer(ctx context.Context, card protocol.AgentCard) error {
	items, err := s.loadA2ARegistryPeers(ctx)
	if err != nil {
		return err
	}
	replaced := false
	for i := range items {
		if strings.TrimSpace(items[i].Endpoint) == strings.TrimSpace(card.Endpoint) {
			items[i] = card
			replaced = true
			break
		}
	}
	if !replaced {
		items = append(items, card)
	}
	return s.saveA2ARegistryPeers(ctx, items)
}

func (s *Server) buildA2ATaskStatusURL(r *http.Request, taskID string) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ""
	}
	scheme := "http"
	if r != nil {
		if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
			scheme = proto
		} else if r.TLS != nil {
			scheme = "https"
		}
	}
	host := ""
	if r != nil {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		port := s.cfg.Server.Port
		if port <= 0 {
			return ""
		}
		host = fmt.Sprintf("%s:%d", firstText(strings.TrimSpace(s.cfg.Server.Host), "127.0.0.1"), port)
	}
	base := (&url.URL{Scheme: scheme, Host: host, Path: "/api/a2a/tasks/" + taskID + "/status"}).String()
	return base
}

func (s *Server) loadA2ATasks(ctx context.Context) ([]a2aTaskRecord, error) {
	raw, ok, err := s.store.GetSetting(ctx, "a2a_tasks_json")
	if err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return []a2aTaskRecord{}, nil
	}
	var items []a2aTaskRecord
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	return items, nil
}

func (s *Server) saveA2ATasks(ctx context.Context, items []a2aTaskRecord) error {
	sort.Slice(items, func(i, j int) bool {
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	if len(items) > 200 {
		items = items[:200]
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return s.store.SetSetting(ctx, "a2a_tasks_json", string(raw))
}

func (s *Server) upsertA2ATask(ctx context.Context, record a2aTaskRecord) error {
	items, err := s.loadA2ATasks(ctx)
	if err != nil {
		return err
	}
	replaced := false
	for i := range items {
		if items[i].ID == record.ID {
			items[i] = record
			replaced = true
			break
		}
	}
	if !replaced {
		items = append(items, record)
	}
	return s.saveA2ATasks(ctx, items)
}

func (s *Server) findA2ATask(ctx context.Context, id string) (a2aTaskRecord, bool, error) {
	items, err := s.loadA2ATasks(ctx)
	if err != nil {
		return a2aTaskRecord{}, false, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, true, nil
		}
	}
	return a2aTaskRecord{}, false, nil
}

func (s *Server) applyA2ATaskStatus(ctx context.Context, status protocol.A2ATaskStatus) error {
	if strings.TrimSpace(status.TaskID) == "" {
		return fmt.Errorf("task id is required")
	}
	if status.UpdatedAt.IsZero() {
		status.UpdatedAt = time.Now().UTC()
	}
	normalizedState := normalizeA2ATaskState(strings.TrimSpace(status.State))
	if normalizedState == "" && status.Result != nil {
		normalizedState = normalizeA2AResultState(strings.TrimSpace(status.Result.Status))
	}
	items, err := s.loadA2ATasks(ctx)
	if err != nil {
		return err
	}
	found := false
	for i := range items {
		if items[i].ID != status.TaskID {
			continue
		}
		found = true
		items[i].Status = firstText(normalizedState, items[i].Status)
		if status.Progress > 0 || normalizedState == "completed" {
			items[i].Progress = status.Progress
		}
		if strings.TrimSpace(status.Message) != "" {
			if normalizedState == "failed" {
				items[i].Error = strings.TrimSpace(status.Message)
			}
		}
		if status.Result != nil {
			items[i].Result = status.Result
			items[i].Output = strings.TrimSpace(status.Result.Output)
			if strings.TrimSpace(status.Result.Error) != "" {
				items[i].Error = strings.TrimSpace(status.Result.Error)
			}
			if !status.Result.StartedAt.IsZero() {
				startedAt := status.Result.StartedAt
				items[i].StartedAt = &startedAt
			}
			if !status.Result.CompletedAt.IsZero() {
				completedAt := status.Result.CompletedAt
				items[i].CompletedAt = &completedAt
			}
		}
		switch normalizedState {
		case "accepted", "running", "partial":
			if items[i].StartedAt == nil {
				startedAt := status.UpdatedAt
				items[i].StartedAt = &startedAt
			}
		case "completed", "failed":
			completedAt := status.UpdatedAt
			items[i].CompletedAt = &completedAt
			if items[i].Progress == 0 {
				items[i].Progress = 100
			}
		}
		items[i].UpdatedAt = status.UpdatedAt
		break
	}
	if !found {
		record := a2aTaskRecord{
			ID:        status.TaskID,
			Status:    normalizedState,
			Progress:  status.Progress,
			Result:    status.Result,
			CreatedAt: status.UpdatedAt,
			UpdatedAt: status.UpdatedAt,
			Source:    "callback",
		}
		if status.Result != nil {
			record.Output = strings.TrimSpace(status.Result.Output)
			record.Error = strings.TrimSpace(status.Result.Error)
			if !status.Result.StartedAt.IsZero() {
				startedAt := status.Result.StartedAt
				record.StartedAt = &startedAt
			}
			if !status.Result.CompletedAt.IsZero() {
				completedAt := status.Result.CompletedAt
				record.CompletedAt = &completedAt
			}
		}
		switch normalizedState {
		case "accepted", "running", "partial":
			if record.StartedAt == nil {
				startedAt := status.UpdatedAt
				record.StartedAt = &startedAt
			}
		case "completed", "failed":
			if record.CompletedAt == nil {
				completedAt := status.UpdatedAt
				record.CompletedAt = &completedAt
			}
			if record.Progress == 0 {
				record.Progress = 100
			}
		}
		items = append(items, record)
	}
	return s.saveA2ATasks(ctx, items)
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func normalizeA2ATaskState(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "queued":
		return "accepted"
	case "accepted", "running", "partial", "completed", "failed":
		return strings.ToLower(strings.TrimSpace(raw))
	case "success", "succeeded", "complete":
		return "completed"
	case "error":
		return "failed"
	case "partial_success":
		return "partial"
	case "in_progress", "processing":
		return "running"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func normalizeA2AResultState(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "accepted", "queued":
		return "accepted"
	case "running", "in_progress", "processing":
		return "running"
	case "partial", "partial_success":
		return "partial"
	case "failed", "error":
		return "failed"
	case "success", "succeeded", "completed", "complete":
		return "completed"
	default:
		return normalizeA2ATaskState(raw)
	}
}

func normalizeA2ABaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return strings.TrimRight(strings.TrimSuffix(raw, "/a2a"), "/")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/a2a")
	return strings.TrimRight(parsed.String(), "/")
}

func normalizeA2AEndpoint(raw string) string {
	base := normalizeA2ABaseURL(raw)
	if base == "" {
		return ""
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/") + "/a2a"
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/a2a"
	return parsed.String()
}

func appendUniqueFold(dst []string, values ...string) []string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		found := false
		for _, existing := range dst {
			if strings.EqualFold(existing, value) {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, value)
		}
	}
	return dst
}

func sameText(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func matchesA2ACapability(peer a2aPeerView, capability string) bool {
	capability = strings.TrimSpace(capability)
	if capability == "" {
		return false
	}
	for _, item := range peer.Capabilities {
		if sameText(item, capability) {
			return true
		}
	}
	for _, item := range peer.TaskTypes {
		if sameText(item, capability) {
			return true
		}
	}
	for _, item := range peer.Protocols {
		if sameText(item, capability) {
			return true
		}
	}
	return false
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.engine.ListAgents(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var req engine.CreateAgentInput
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		item, err := s.engine.CreateAgent(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, item)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	id = strings.TrimSpace(id)
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("agent id required"))
		return
	}

	switch r.Method {
	case http.MethodGet:
		item, err := s.engine.GetAgent(r.Context(), id)
		if err != nil {
			if engine.IsNotFound(err) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	case http.MethodPut:
		var req engine.UpdateAgentInput
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		item, err := s.engine.UpdateAgent(r.Context(), id, req)
		if err != nil {
			if engine.IsNotFound(err) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	case http.MethodDelete:
		if err := s.engine.DeleteAgent(r.Context(), id); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
		items, err := s.engine.ListSessions(r.Context(), agentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var req struct {
			AgentID string `json:"agent_id"`
			Title   string `json:"title"`
			IsMain  bool   `json:"is_main"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		sess, err := s.engine.CreateSession(r.Context(), req.AgentID, req.Title, req.IsMain)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, sess)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleSessionSubRoutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	trimmed = strings.Trim(trimmed, "/")
	parts := strings.Split(trimmed, "/")

	if len(parts) < 2 {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown route"))
		return
	}
	sessionID := parts[0]
	action := parts[1]

	switch action {
	case "messages":
		if r.Method == http.MethodGet {
			limit := 200
			if raw := r.URL.Query().Get("limit"); raw != "" {
				if n, err := strconv.Atoi(raw); err == nil {
					limit = n
				}
			}
			items, err := s.engine.ListMessages(r.Context(), sessionID, limit)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, http.StatusOK, items)
			return
		}

		if r.Method == http.MethodPost {
			var req engine.SendMessageInput
			if err := decodeJSON(r, &req); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			msg, err := s.engine.SendMessage(r.Context(), sessionID, req)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusCreated, msg)
			return
		}

		methodNotAllowed(w)
	case "events":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handleSessionEvents(w, r, sessionID)
	case "ws":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handleSessionWebSocket(w, r, sessionID)
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown route"))
	}
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.isDraining() {
		w.Header().Set("Retry-After", "3")
		http.Error(w, "server is draining for restart", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	s.activeSSE.Add(1)
	defer s.activeSSE.Add(-1)

	afterSeq := parseEventReplaySequence(r)
	ch, backlog := s.events.SubscribeSince(sessionID, afterSeq)
	defer s.events.Unsubscribe(sessionID, ch)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	_, _ = w.Write([]byte(": connected\n\n"))
	flusher.Flush()

	for _, item := range backlog {
		writeSSEEvent(w, "message", item.Sequence, item.Payload)
	}
	if len(backlog) > 0 {
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if s.isDraining() {
				writeSSEShutdown(w)
				flusher.Flush()
				return
			}
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case frame := <-ch:
			writeSSEEvent(w, "message", frame.Sequence, frame.Payload)
			flusher.Flush()
		}
	}
}

func (s *Server) handleSessionWebSocket(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.isDraining() {
		w.Header().Set("Retry-After", "3")
		http.Error(w, "server is draining for restart", http.StatusServiceUnavailable)
		return
	}
	websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		s.activeWS.Add(1)
		defer s.activeWS.Add(-1)

		afterSeq := parseEventReplaySequence(r)
		ch, backlog := s.events.SubscribeSince(sessionID, afterSeq)
		defer s.events.Unsubscribe(sessionID, ch)

		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()

		for _, item := range backlog {
			if err := websocket.Message.Send(conn, string(item.Payload)); err != nil {
				return
			}
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case <-heartbeat.C:
				if s.isDraining() {
					_ = websocket.Message.Send(conn, `{"type":"shutdown","state":"draining"}`)
					return
				}
				if err := websocket.Message.Send(conn, `{"type":"ping"}`); err != nil {
					return
				}
			case frame := <-ch:
				if err := websocket.Message.Send(conn, string(frame.Payload)); err != nil {
					return
				}
			}
		}
	}).ServeHTTP(w, r)
}

func parseEventReplaySequence(r *http.Request) int64 {
	raw := strings.TrimSpace(r.URL.Query().Get("since_id"))
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	}
	if raw == "" {
		return 0
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 0 {
		return 0
	}
	return seq
}

func writeSSEEvent(w http.ResponseWriter, eventName string, sequence int64, payload []byte) {
	if sequence > 0 {
		_, _ = w.Write([]byte("id: "))
		_, _ = w.Write([]byte(strconv.FormatInt(sequence, 10)))
		_, _ = w.Write([]byte("\n"))
	}
	_, _ = w.Write([]byte("event: "))
	_, _ = w.Write([]byte(eventName))
	_, _ = w.Write([]byte("\n"))
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n\n"))
}

func writeSSEShutdown(w http.ResponseWriter) {
	_, _ = w.Write([]byte("event: shutdown\n"))
	_, _ = w.Write([]byte("data: {\"type\":\"shutdown\",\"data\":{\"state\":\"draining\",\"message\":\"服务切换中，请稍后重连\"}}\n\n"))
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			limit = v
		}
	}

	rows, err := s.store.ListAudit(r.Context(), agentID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleCron(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
		rows, err := s.store.ListCronJobs(r.Context(), agentID, false)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	case http.MethodPost:
		var req struct {
			AgentID       string `json:"agent_id"`
			Name          string `json:"name"`
			Schedule      string `json:"schedule"`
			ScheduleType  string `json:"type"`
			JobType       string `json:"job_type"`
			Payload       string `json:"payload"`
			ExecutionMode string `json:"execution_mode"`
			SessionID     string `json:"session_id"`
			TargetChannel string `json:"target_channel"`
			Priority      string `json:"priority"`
			Enabled       *bool  `json:"enabled"`
			RetryLimit    int    `json:"retry_limit"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}

		now := time.Now().UTC()
		item := models.CronJob{
			ID:            engine.NewID("cron"),
			AgentID:       strings.TrimSpace(req.AgentID),
			Name:          defaultString(req.Name, "定时任务"),
			Schedule:      defaultString(req.Schedule, "*/10 * * * *"),
			ScheduleType:  normalizeScheduleType(req.ScheduleType),
			JobType:       defaultString(req.JobType, "agent"),
			Payload:       req.Payload,
			ExecutionMode: defaultExecutionMode(req.ExecutionMode),
			SessionID:     strings.TrimSpace(req.SessionID),
			TargetChannel: defaultString(req.TargetChannel, "last"),
			Priority:      defaultCronPriority(req.Priority),
			Enabled:       true,
			RetryLimit:    maxInt(req.RetryLimit, 5),
			LastStatus:    "never",
			LastError:     "",
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if req.Enabled != nil {
			item.Enabled = *req.Enabled
		}
		if item.AgentID == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("agent_id required"))
			return
		}
		if strings.TrimSpace(item.Payload) == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("payload required"))
			return
		}
		if item.ExecutionMode == "custom" && strings.TrimSpace(item.SessionID) == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("custom execution mode requires session_id"))
			return
		}
		nextRun, err := scheduler.ResolveNextRun(item, now)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid schedule: %w", err))
			return
		}
		if item.ScheduleType == "at" && !nextRun.After(now) && item.Enabled {
			writeError(w, http.StatusBadRequest, fmt.Errorf("at schedule must be in the future"))
			return
		}
		if !nextRun.IsZero() {
			item.NextRunAt = &nextRun
		}
		if err := s.store.CreateCronJob(r.Context(), item); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, item)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleCronByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/cron/")
	id = path.Clean("/" + id)
	id = strings.TrimPrefix(id, "/")
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("cron id required"))
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Name          string `json:"name"`
			Schedule      string `json:"schedule"`
			ScheduleType  string `json:"type"`
			JobType       string `json:"job_type"`
			Payload       string `json:"payload"`
			ExecutionMode string `json:"execution_mode"`
			SessionID     string `json:"session_id"`
			TargetChannel string `json:"target_channel"`
			Priority      string `json:"priority"`
			Enabled       *bool  `json:"enabled"`
			RetryLimit    int    `json:"retry_limit"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}

		jobs, err := s.store.ListCronJobs(r.Context(), "", false)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		var current *models.CronJob
		for i := range jobs {
			if jobs[i].ID == id {
				current = &jobs[i]
				break
			}
		}
		if current == nil {
			writeError(w, http.StatusNotFound, fmt.Errorf("cron job not found"))
			return
		}

		current.Name = defaultString(req.Name, current.Name)
		current.Schedule = defaultString(req.Schedule, current.Schedule)
		current.ScheduleType = normalizeScheduleType(defaultString(req.ScheduleType, current.ScheduleType))
		current.JobType = defaultString(req.JobType, current.JobType)
		current.Payload = req.Payload
		current.ExecutionMode = defaultExecutionMode(defaultString(req.ExecutionMode, current.ExecutionMode))
		current.SessionID = defaultString(req.SessionID, current.SessionID)
		current.TargetChannel = defaultString(req.TargetChannel, current.TargetChannel)
		current.Priority = defaultCronPriority(defaultString(req.Priority, current.Priority))
		if req.Enabled != nil {
			current.Enabled = *req.Enabled
		}
		current.RetryLimit = maxInt(req.RetryLimit, current.RetryLimit)
		if current.RetryLimit <= 0 {
			current.RetryLimit = 5
		}
		current.UpdatedAt = time.Now().UTC()
		nextRun, err := scheduler.ResolveNextRun(*current, current.UpdatedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid schedule: %w", err))
			return
		}
		if current.ScheduleType == "at" && !nextRun.After(current.UpdatedAt) && current.Enabled {
			writeError(w, http.StatusBadRequest, fmt.Errorf("at schedule must be in the future"))
			return
		}
		if !nextRun.IsZero() {
			current.NextRunAt = &nextRun
		} else {
			current.NextRunAt = nil
		}

		if err := s.store.UpdateCronJob(r.Context(), *current); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, current)
	case http.MethodDelete:
		if err := s.store.DeleteCronJob(r.Context(), id); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req app.SetCredentialInput
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.app.SetCredential(r.Context(), req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCredentialReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Provider       string `json:"provider"`
		MasterPassword string `json:"master_password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	secret, err := s.app.GetCredential(r.Context(), req.Provider, req.MasterPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret})
}

func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, fmt.Errorf("api endpoint not found"))
		return
	}

	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" || p == "." {
		p = "index.html"
	}

	if !webui.Exists(p) {
		r2 := *r
		r2.URL.Path = "/"
		s.static.ServeHTTP(w, &r2)
		return
	}

	s.static.ServeHTTP(w, r)
}

func decodeJSON(r *http.Request, out any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	if status < 400 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
}

func defaultString(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}

func normalizeScheduleType(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "at":
		return "at"
	case "every":
		return "every"
	case "cron":
		return "cron"
	default:
		return "cron"
	}
}

func defaultExecutionMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "main":
		return "main"
	case "isolated":
		return "isolated"
	case "custom":
		return "custom"
	default:
		return "main"
	}
}

func defaultCronPriority(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "low":
		return "low"
	case "high":
		return "high"
	case "critical":
		return "critical"
	default:
		return "normal"
	}
}

func maxInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		if isAuthExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		bootstrapped, err := s.app.IsBootstrapped(r.Context())
		if err == nil && !bootstrapped {
			next.ServeHTTP(w, r)
			return
		}

		token := extractRequestToken(r)
		if !s.app.ValidateToken(token) {
			writeError(w, http.StatusUnauthorized, fmt.Errorf("unauthorized"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withDrain(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isDraining() || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "3")
		w.Header().Set("Connection", "close")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("server is draining for restart"))
			return
		}
		http.Error(w, "server is draining for restart", http.StatusServiceUnavailable)
	})
}

func (s *Server) withRequestTracking(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.activeHTTP.Add(1)
		defer s.activeHTTP.Add(-1)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) isDraining() bool {
	return s.drainCheck != nil && s.drainCheck()
}

func (s *Server) readRuntimeStatusSnapshot() runtimeStatusSnapshot {
	var snapshot runtimeStatusSnapshot
	path := filepath.Join(s.cfg.RunDir, "runtime-status.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return snapshot
	}
	_ = json.Unmarshal(raw, &snapshot)
	return snapshot
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func isAuthExemptPath(p string) bool {
	switch {
	case p == "/api/system/status":
		return true
	case p == "/api/system/bootstrap":
		return true
	case p == "/api/system/trust-hint":
		return true
	case p == "/api/auth/login":
		return true
	case p == "/api/a2a/card":
		return true
	case strings.HasPrefix(p, "/api/gateway/webhook/"):
		return true
	default:
		return false
	}
}

func extractBearer(auth string) string {
	auth = strings.TrimSpace(auth)
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, prefix))
}

func extractRequestToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	token := extractBearer(r.Header.Get("Authorization"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Auth-Token"))
	}
	if token != "" {
		return token
	}
	if strings.HasSuffix(strings.TrimSpace(r.URL.Path), "/ws") {
		return strings.TrimSpace(r.URL.Query().Get("access_token"))
	}
	return ""
}

func (s *Server) RunCronJob(ctx context.Context, job models.CronJob) error {
	mode := strings.ToLower(strings.TrimSpace(job.ExecutionMode))
	if mode == "" {
		mode = "main"
	}

	var (
		sessionID string
		cleanup   func()
	)
	switch mode {
	case "main":
		mainSession, ok, err := s.store.GetMainSession(ctx, job.AgentID)
		if err != nil {
			return err
		}
		if ok {
			sessionID = mainSession.ID
		} else {
			sess, err := s.engine.CreateSession(ctx, job.AgentID, "主会话", true)
			if err != nil {
				return err
			}
			sessionID = sess.ID
		}
	case "isolated":
		sess, err := s.engine.CreateSession(ctx, job.AgentID, "Cron: "+job.Name, false)
		if err != nil {
			return err
		}
		sessionID = sess.ID
		cleanup = func() {
			go func(id string) {
				// Best-effort cleanup after execution window,避免在异步执行尚未结束前删除会话。
				time.Sleep(2 * time.Minute)
				_ = s.store.DeleteSession(context.Background(), id)
			}(sessionID)
		}
	case "custom":
		sessionID = strings.TrimSpace(job.SessionID)
		if sessionID == "" {
			return fmt.Errorf("custom execution mode requires session_id")
		}
	default:
		return fmt.Errorf("unknown execution mode: %s", mode)
	}
	if cleanup != nil {
		defer cleanup()
	}

	content := fmt.Sprintf("这是定时任务。调度类型=%s。执行内容：%s", firstText(job.ScheduleType, "cron"), job.Payload)
	if _, err := s.engine.SendMessage(ctx, sessionID, engine.SendMessageInput{Content: content, AutoApprove: true}); err != nil {
		return err
	}

	if s.gateway != nil {
		target := strings.TrimSpace(job.TargetChannel)
		if target == "" {
			target = "last"
		}
		_, _ = s.gateway.Send(ctx, job.AgentID, target, gateway.OutboundEvent{
			MessageID:      engine.NewID("cron_notify"),
			TextMarkdown:   fmt.Sprintf("⏰ 定时任务 `%s` 已触发，正在执行。", job.Name),
			Priority:       firstText(strings.TrimSpace(job.Priority), "normal"),
			IdempotencyKey: fmt.Sprintf("cron:%s:%d", job.ID, time.Now().Unix()/60),
			TTLSeconds:     3600,
		})
	}
	return nil
}
