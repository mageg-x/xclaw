package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"xclaw/cli/approval"
	"xclaw/cli/audit"
	"xclaw/cli/config"
	"xclaw/cli/db"
	"xclaw/cli/engine"
	"xclaw/cli/llm"
	"xclaw/cli/mcpclient"
	"xclaw/cli/mcpregistry"
	"xclaw/cli/models"
	"xclaw/cli/queue"
	"xclaw/cli/sandbox"
	"xclaw/cli/tools"
	"xclaw/cli/workspace"
)

func runTUICommand(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dataDir string
	fs.StringVar(&dataDir, "data-dir", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCLIConfig(dataDir)
	if err != nil {
		return err
	}
	return launchTUI(cfg)
}

type tuiRuntime struct {
	store *db.Store
	queue *queue.LaneQueue
	eng   *engine.Service
	event *engine.EventHub
}

func initTUIRuntime(cfg config.RuntimeConfig) (*tuiRuntime, error) {
	store, err := db.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return nil, err
	}
	q := queue.NewLaneQueue(cfg.Queue.GlobalConcurrency)
	ws := workspace.NewManager(cfg.WorkspaceDir)
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
	gateway := llm.NewMonitoredGateway(llm.NewLocalGateway(), llm.NewTokenMonitor())
	eng := engine.NewService(store, ws, q, toolExec, auditLogger, eventHub, planCache, gateway)
	return &tuiRuntime{store: store, queue: q, eng: eng, event: eventHub}, nil
}

func (r *tuiRuntime) Close() {
	if r.queue != nil {
		r.queue.Close()
	}
	if r.store != nil {
		_ = r.store.Close()
	}
}

type tuiSessionItem struct {
	id      string
	title   string
	status  string
	updated time.Time
}

func (i tuiSessionItem) Title() string { return i.title }
func (i tuiSessionItem) Description() string {
	return fmt.Sprintf("%s · %s", i.status, i.updated.Local().Format("01-02 15:04"))
}
func (i tuiSessionItem) FilterValue() string { return i.id + " " + i.title + " " + i.status }

type tuiTickMsg struct{}
type tuiEventMsg struct{ event engine.Event }
type tuiErrMsg struct{ err error }

type tuiModel struct {
	cfg   config.RuntimeConfig
	rt    *tuiRuntime
	agent models.Agent

	sessions   []models.Session
	sessionIDs []string
	list       list.Model
	viewport   viewport.Model
	input      textinput.Model

	currentSessionID string
	sub              chan engine.EventFrame
	streaming        string
	statusLine       string
	page             int

	width  int
	height int

	styles tuiStyles
}

type tuiStyles struct {
	frame   lipgloss.Style
	header  lipgloss.Style
	panel   lipgloss.Style
	footer  lipgloss.Style
	accent  lipgloss.Style
	muted   lipgloss.Style
	warn    lipgloss.Style
	success lipgloss.Style
}

func defaultTUIStyles() tuiStyles {
	return tuiStyles{
		frame:   lipgloss.NewStyle().Padding(0, 1),
		header:  lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("31")).Padding(0, 1).Bold(true),
		panel:   lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("62")),
		footer:  lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Background(lipgloss.Color("236")).Padding(0, 1),
		accent:  lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true),
		muted:   lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		warn:    lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true),
		success: lipgloss.NewStyle().Foreground(lipgloss.Color("77")).Bold(true),
	}
}

func launchTUI(cfg config.RuntimeConfig) error {
	rt, err := initTUIRuntime(cfg)
	if err != nil {
		return err
	}
	defer rt.Close()

	agents, err := rt.store.ListAgents(context.Background())
	if err != nil {
		return err
	}
	var agent models.Agent
	if len(agents) == 0 {
		agent, err = rt.eng.CreateAgent(context.Background(), engine.CreateAgentInput{
			Name:          "终端助手",
			Emoji:         "🧭",
			Description:   "负责终端对话与执行",
			ModelProvider: "local",
			ModelName:     "local-reasoner",
			Tools:         []string{"list_dir", "read_file", "search_text", "write_file", "exec_cmd", "spawn_subagent", "delegate_to_subagent", "list_subagents", "terminate_subagent", "image_generate"},
		})
		if err != nil {
			return err
		}
	} else {
		agent = agents[0]
	}

	sessions, err := rt.store.ListSessions(context.Background(), agent.ID)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		sess, createErr := rt.eng.CreateSession(context.Background(), agent.ID, "终端会话", false)
		if createErr != nil {
			return createErr
		}
		sessions = append(sessions, sess)
	}

	items := make([]list.Item, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, tuiSessionItem{id: s.ID, title: s.Title, status: s.Status, updated: s.UpdatedAt})
	}
	sessionList := list.New(items, list.NewDefaultDelegate(), 40, 20)
	sessionList.Title = "Sessions"
	sessionList.SetShowStatusBar(false)
	sessionList.SetFilteringEnabled(true)
	sessionList.SetShowHelp(false)

	vp := viewport.New(80, 20)
	in := textinput.New()
	in.Placeholder = "输入消息，回车发送"
	in.Prompt = "> "
	in.Focus()
	in.CharLimit = 6000
	in.Width = 80

	m := &tuiModel{
		cfg:              cfg,
		rt:               rt,
		agent:            agent,
		sessions:         sessions,
		list:             sessionList,
		viewport:         vp,
		input:            in,
		currentSessionID: sessions[0].ID,
		statusLine:       "就绪",
		styles:           defaultTUIStyles(),
	}
	m.subscribeCurrentSession()
	_ = m.reloadMessages()

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(tuiTickCmd(), m.waitEventCmd())
}

func tuiTickCmd() tea.Cmd {
	return tea.Tick(1200*time.Millisecond, func(time.Time) tea.Msg {
		return tuiTickMsg{}
	})
}

func (m *tuiModel) waitEventCmd() tea.Cmd {
	ch := m.sub
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		frame, ok := <-ch
		if !ok {
			return nil
		}
		var evt engine.Event
		if err := json.Unmarshal(frame.Payload, &evt); err != nil {
			return tuiErrMsg{err: err}
		}
		return tuiEventMsg{event: evt}
	}
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = v.Width
		m.height = v.Height
		m.layout()
		return m, nil
	case tea.KeyMsg:
		switch v.String() {
		case "ctrl+c":
			m.unsubscribeCurrentSession()
			return m, tea.Quit
		case "ctrl+n":
			if err := m.createSession(); err != nil {
				m.statusLine = "创建会话失败: " + err.Error()
			} else {
				m.statusLine = "已创建新会话"
			}
			return m, nil
		case "ctrl+w":
			if err := m.closeCurrentSession(); err != nil {
				m.statusLine = "关闭会话失败: " + err.Error()
			} else {
				m.statusLine = "已关闭会话"
			}
			return m, nil
		case "ctrl+s":
			if err := m.exportCurrentSession(); err != nil {
				m.statusLine = "保存失败: " + err.Error()
			} else {
				m.statusLine = "对话历史已保存"
			}
			return m, nil
		case "f1":
			m.page = 1
			return m, nil
		case "f2":
			m.page = 2
			return m, nil
		case "f3":
			m.page = 3
			return m, nil
		case "esc":
			m.page = 0
			return m, nil
		case "enter":
			if m.page == 0 {
				if err := m.sendMessage(); err != nil {
					m.statusLine = "发送失败: " + err.Error()
				} else {
					m.statusLine = "已发送，等待回复..."
				}
				return m, nil
			}
		}
		if m.page == 0 {
			var cmd1 tea.Cmd
			m.list, cmd1 = m.list.Update(v)
			if sel := m.list.SelectedItem(); sel != nil {
				if item, ok := sel.(tuiSessionItem); ok && item.id != "" && item.id != m.currentSessionID {
					m.switchSession(item.id)
				}
			}
			var cmd2 tea.Cmd
			m.input, cmd2 = m.input.Update(v)
			return m, tea.Batch(cmd1, cmd2)
		}
		return m, nil
	case tuiTickMsg:
		m.refreshSessions()
		if m.page == 0 {
			_ = m.reloadMessages()
		}
		return m, tuiTickCmd()
	case tuiEventMsg:
		m.handleEvent(v.event)
		return m, m.waitEventCmd()
	case tuiErrMsg:
		m.statusLine = "事件错误: " + v.err.Error()
		return m, m.waitEventCmd()
	}
	return m, nil
}

func (m *tuiModel) View() string {
	title := m.styles.header.Render("XClaw Terminal · Ctrl+N 新建 · Ctrl+W 关闭 · Ctrl+S 保存 · F1/F2/F3 面板 · Esc 回到聊天")
	body := ""
	switch m.page {
	case 1:
		body = m.renderStatusPage()
	case 2:
		body = m.renderCronPage()
	case 3:
		body = m.renderHeartbeatPage()
	default:
		left := m.styles.panel.Width(maxInt(24, m.width/3)).Render(m.list.View())
		rightContent := m.viewport.View() + "\n" + m.input.View()
		right := m.styles.panel.Width(maxInt(50, m.width-m.width/3-4)).Render(rightContent)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	}
	footer := m.styles.footer.Render("状态: " + m.statusLine)
	return m.styles.frame.Render(lipgloss.JoinVertical(lipgloss.Left, title, body, footer))
}

func (m *tuiModel) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	leftWidth := maxInt(28, m.width/3)
	rightWidth := maxInt(50, m.width-leftWidth-8)
	bodyHeight := maxInt(10, m.height-8)
	m.list.SetSize(leftWidth-4, bodyHeight)
	m.viewport.Width = rightWidth - 4
	m.viewport.Height = bodyHeight - 4
	m.input.Width = rightWidth - 6
}

func (m *tuiModel) refreshSessions() {
	sessions, err := m.rt.store.ListSessions(context.Background(), m.agent.ID)
	if err != nil {
		m.statusLine = "刷新会话失败: " + err.Error()
		return
	}
	if len(sessions) == 0 {
		return
	}
	m.sessions = sessions
	items := make([]list.Item, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, tuiSessionItem{id: s.ID, title: s.Title, status: s.Status, updated: s.UpdatedAt})
	}
	m.list.SetItems(items)
}

func (m *tuiModel) reloadMessages() error {
	if m.currentSessionID == "" {
		return nil
	}
	msgs, err := m.rt.eng.ListMessages(context.Background(), m.currentSessionID, 200)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, msg := range msgs {
		role := strings.ToUpper(msg.Role)
		if role == "ASSISTANT" {
			role = "AI"
		}
		b.WriteString(m.styles.accent.Render(role))
		b.WriteString(": ")
		b.WriteString(msg.Content)
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(m.streaming) != "" {
		b.WriteString(m.styles.warn.Render("AI(typing): "))
		b.WriteString(m.streaming)
		b.WriteString("\n")
	}
	m.viewport.SetContent(strings.TrimSpace(b.String()))
	m.viewport.GotoBottom()
	return nil
}

func (m *tuiModel) subscribeCurrentSession() {
	if m.currentSessionID == "" {
		return
	}
	m.sub = m.rt.event.Subscribe(m.currentSessionID)
}

func (m *tuiModel) unsubscribeCurrentSession() {
	if m.currentSessionID == "" || m.sub == nil {
		return
	}
	m.rt.event.Unsubscribe(m.currentSessionID, m.sub)
	m.sub = nil
}

func (m *tuiModel) switchSession(id string) {
	if strings.TrimSpace(id) == "" || id == m.currentSessionID {
		return
	}
	m.unsubscribeCurrentSession()
	m.currentSessionID = id
	m.streaming = ""
	m.subscribeCurrentSession()
	_ = m.reloadMessages()
	m.statusLine = "已切换会话"
}

func (m *tuiModel) createSession() error {
	s, err := m.rt.eng.CreateSession(context.Background(), m.agent.ID, fmt.Sprintf("终端会话 %s", time.Now().Format("15:04")), false)
	if err != nil {
		return err
	}
	m.refreshSessions()
	m.switchSession(s.ID)
	return nil
}

func (m *tuiModel) closeCurrentSession() error {
	if m.currentSessionID == "" {
		return fmt.Errorf("no active session")
	}
	if err := m.rt.store.DeleteSession(context.Background(), m.currentSessionID); err != nil {
		return err
	}
	m.unsubscribeCurrentSession()
	m.refreshSessions()
	if len(m.sessions) > 0 {
		m.currentSessionID = m.sessions[0].ID
		m.subscribeCurrentSession()
		_ = m.reloadMessages()
	} else {
		s, err := m.rt.eng.CreateSession(context.Background(), m.agent.ID, "终端会话", false)
		if err != nil {
			return err
		}
		m.currentSessionID = s.ID
		m.refreshSessions()
		m.subscribeCurrentSession()
		_ = m.reloadMessages()
	}
	return nil
}

func (m *tuiModel) exportCurrentSession() error {
	msgs, err := m.rt.eng.ListMessages(context.Background(), m.currentSessionID, 500)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, msg := range msgs {
		b.WriteString("[")
		b.WriteString(msg.CreatedAt.Local().Format("2006-01-02 15:04:05"))
		b.WriteString("] ")
		b.WriteString(strings.ToUpper(msg.Role))
		b.WriteString(": ")
		b.WriteString(msg.Content)
		b.WriteString("\n\n")
	}
	dir := filepath.Join(m.cfg.DataDir, "exports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("session-%s-%s.md", m.currentSessionID, time.Now().Format("20060102-150405")))
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func (m *tuiModel) sendMessage() error {
	content := strings.TrimSpace(m.input.Value())
	if content == "" {
		return nil
	}
	m.input.SetValue("")
	_, err := m.rt.eng.SendMessage(context.Background(), m.currentSessionID, engine.SendMessageInput{Content: content, AutoApprove: true})
	if err != nil {
		return err
	}
	m.streaming = ""
	_ = m.reloadMessages()
	return nil
}

func (m *tuiModel) handleEvent(evt engine.Event) {
	switch evt.Type {
	case "react.thought":
		var data struct {
			Thought string `json:"thought"`
		}
		b, _ := json.Marshal(evt.Data)
		_ = json.Unmarshal(b, &data)
		if strings.TrimSpace(data.Thought) != "" {
			m.statusLine = "思考中: " + trimForLog(data.Thought, 90)
		}
	case "assistant.delta":
		var data struct {
			Chunk string `json:"chunk"`
		}
		b, _ := json.Marshal(evt.Data)
		_ = json.Unmarshal(b, &data)
		m.streaming += data.Chunk
		_ = m.reloadMessages()
		m.statusLine = "正在输入..."
	case "assistant.done":
		m.streaming = ""
		_ = m.reloadMessages()
		m.statusLine = "已完成"
	case "session.status":
		var data struct {
			Status string `json:"status"`
		}
		b, _ := json.Marshal(evt.Data)
		_ = json.Unmarshal(b, &data)
		m.refreshSessions()
		switch strings.ToLower(strings.TrimSpace(data.Status)) {
		case "recovering":
			m.statusLine = "会话恢复中..."
		case "running":
			m.statusLine = "会话执行中..."
		case "idle":
			m.statusLine = "会话已空闲"
		}
	case "message.created":
		_ = m.reloadMessages()
	}
}

func (m *tuiModel) renderStatusPage() string {
	store := m.rt.store
	agents, _ := store.ListAgents(context.Background())
	sessions, _ := store.ListSessions(context.Background(), "")
	running := 0
	for _, sess := range sessions {
		if strings.EqualFold(strings.TrimSpace(sess.Status), "running") {
			running++
		}
	}
	logs, _ := store.ListAudit(context.Background(), "", 80)
	hb := "none"
	for _, row := range logs {
		if row.Category == "heartbeat" {
			hb = row.CreatedAt.Local().Format("2006-01-02 15:04:05") + " (" + row.Action + ")"
			break
		}
	}
	text := fmt.Sprintf("Agent: %s\nAgents: %d\nSessions: %d (running=%d)\nLast Heartbeat: %s\nDataDir: %s", m.agent.Name, len(agents), len(sessions), running, hb, m.cfg.DataDir)
	return m.styles.panel.Width(maxInt(80, m.width-4)).Render(text)
}

func (m *tuiModel) renderCronPage() string {
	jobs, _ := m.rt.store.ListCronJobs(context.Background(), "", false)
	if len(jobs) == 0 {
		return m.styles.panel.Width(maxInt(80, m.width-4)).Render("暂无定时任务")
	}
	sort.Slice(jobs, func(i, j int) bool {
		a := time.Time{}
		b := time.Time{}
		if jobs[i].NextRunAt != nil {
			a = *jobs[i].NextRunAt
		}
		if jobs[j].NextRunAt != nil {
			b = *jobs[j].NextRunAt
		}
		return a.Before(b)
	})
	var b strings.Builder
	for _, job := range jobs {
		next := "-"
		if job.NextRunAt != nil {
			next = job.NextRunAt.Local().Format("01-02 15:04:05")
		}
		b.WriteString(fmt.Sprintf("- %s [%s] %s -> %s\n", job.Name, job.ScheduleType, job.Schedule, next))
	}
	return m.styles.panel.Width(maxInt(80, m.width-4)).Render(strings.TrimSpace(b.String()))
}

func (m *tuiModel) renderHeartbeatPage() string {
	rows, _ := m.rt.store.ListAudit(context.Background(), "", 200)
	lines := make([]string, 0, 20)
	for _, row := range rows {
		if row.Category != "heartbeat" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", row.CreatedAt.Local().Format("01-02 15:04:05"), row.Action, trimForLog(row.Detail, 120)))
		if len(lines) >= 20 {
			break
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "暂无心跳记录")
	}
	return m.styles.panel.Width(maxInt(80, m.width-4)).Render(strings.Join(lines, "\n"))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
