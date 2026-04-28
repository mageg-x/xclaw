package gateway

import (
	"strings"
	"sync"
	"time"
)

type StreamMode string

const (
	StreamModeEdit    StreamMode = "edit"
	StreamModeChunk   StreamMode = "chunk"
	StreamModeReplace StreamMode = "replace"
)

type StreamConfig struct {
	Mode             StreamMode `json:"mode"`
	ChunkSize        int        `json:"chunk_size"`
	EditWindowMs     int        `json:"edit_window_ms"`
	MaxEditsPerMsg   int        `json:"max_edits_per_msg"`
	AutoMode         bool       `json:"auto_mode"`
}

type StreamHandler struct {
	mu     sync.RWMutex
	config StreamConfig
}

func DefaultStreamConfig() StreamConfig {
	return StreamConfig{
		Mode:           StreamModeEdit,
		ChunkSize:      3500,
		EditWindowMs:   3000,
		MaxEditsPerMsg: 30,
		AutoMode:       true,
	}
}

func NewStreamHandler(cfg StreamConfig) *StreamHandler {
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = 3500
	}
	if cfg.EditWindowMs <= 0 {
		cfg.EditWindowMs = 3000
	}
	if cfg.MaxEditsPerMsg <= 0 {
		cfg.MaxEditsPerMsg = 30
	}
	if cfg.Mode == "" {
		cfg.Mode = StreamModeEdit
	}
	return &StreamHandler{config: cfg}
}

func (h *StreamHandler) UpdateConfig(cfg StreamConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cfg.ChunkSize > 0 {
		h.config.ChunkSize = cfg.ChunkSize
	}
	if cfg.EditWindowMs > 0 {
		h.config.EditWindowMs = cfg.EditWindowMs
	}
	if cfg.MaxEditsPerMsg > 0 {
		h.config.MaxEditsPerMsg = cfg.MaxEditsPerMsg
	}
	if cfg.Mode != "" {
		h.config.Mode = cfg.Mode
	}
	h.config.AutoMode = cfg.AutoMode
}

func (h *StreamHandler) GetConfig() StreamConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.config
}

func (h *StreamHandler) SelectMode(cap CapabilityProfile, textLen int, editCount int) StreamMode {
	h.mu.RLock()
	autoMode := h.config.AutoMode
	fixedMode := h.config.Mode
	maxEdits := h.config.MaxEditsPerMsg
	h.mu.RUnlock()

	if !autoMode {
		return fixedMode
	}

	if !cap.SupportsEdit || editCount >= maxEdits {
		if cap.SupportsStreamingText {
			return StreamModeChunk
		}
		return StreamModeReplace
	}

	if cap.MaxTextLen > 0 && textLen > cap.MaxTextLen {
		return StreamModeChunk
	}

	return StreamModeEdit
}

type StreamChunk struct {
	Index    int        `json:"index"`
	Mode     StreamMode `json:"mode"`
	Content  string     `json:"content"`
	Finished bool       `json:"finished"`
}

func (h *StreamHandler) ChunkText(text string, maxSize int) []StreamChunk {
	if maxSize <= 0 {
		h.mu.RLock()
		maxSize = h.config.ChunkSize
		h.mu.RUnlock()
	}
	if maxSize <= 0 {
		maxSize = 3500
	}

	if len(text) <= maxSize {
		return []StreamChunk{{Index: 0, Content: text, Finished: true}}
	}

	var chunks []StreamChunk
	pos := 0
	idx := 0
	for pos < len(text) {
		end := pos + maxSize
		if end > len(text) {
			end = len(text)
		}
		if end < len(text) {
			lastNL := strings.LastIndex(text[pos:end], "\n")
			if lastNL > maxSize/2 {
				end = pos + lastNL + 1
			}
		}
		chunk := text[pos:end]
		finished := end >= len(text)
		chunks = append(chunks, StreamChunk{
			Index:    idx,
			Content:  chunk,
			Finished: finished,
		})
		pos = end
		idx++
	}
	return chunks
}

type StreamSession struct {
	ID        string
	Mode      StreamMode
	Chunks    []StreamChunk
	EditCount int
	StartedAt time.Time
	UpdatedAt time.Time
}

type StreamSessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*StreamSession
}

func NewStreamSessionManager() *StreamSessionManager {
	return &StreamSessionManager{sessions: make(map[string]*StreamSession)}
}

func (m *StreamSessionManager) Start(id string, mode StreamMode) *StreamSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess := &StreamSession{
		ID:        id,
		Mode:      mode,
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	m.sessions[id] = sess
	return sess
}

func (m *StreamSessionManager) Get(id string) (*StreamSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[id]
	return sess, ok
}

func (m *StreamSessionManager) AppendChunk(id string, chunk StreamChunk) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sess, ok := m.sessions[id]; ok {
		sess.Chunks = append(sess.Chunks, chunk)
		sess.UpdatedAt = time.Now().UTC()
		if chunk.Mode == StreamModeEdit {
			sess.EditCount++
		}
	}
}

func (m *StreamSessionManager) Finish(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sess, ok := m.sessions[id]; ok {
		sess.UpdatedAt = time.Now().UTC()
	}
}

func (m *StreamSessionManager) Cleanup(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for k, sess := range m.sessions {
		if now.Sub(sess.UpdatedAt) > maxAge {
			delete(m.sessions, k)
		}
	}
}
