package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A2A (Agent-to-Agent) protocol implementation
// Based on Google's Agent-to-Agent protocol specification

// A2AMessage represents a message between agents
type A2AMessage struct {
	ID        string          `json:"id"`
	From      string          `json:"from"`
	To        string          `json:"to"`
	Type      string          `json:"type"` // text, task, result, error
	Content   string          `json:"content"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	ReplyTo   string          `json:"reply_to,omitempty"`
}

// A2ATask represents a task sent between agents
type A2ATask struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Inputs      map[string]string `json:"inputs"`
	Deadline    *time.Time        `json:"deadline,omitempty"`
	Priority    int               `json:"priority"` // 1-5
	CallbackURL string            `json:"callback_url,omitempty"`
}

// A2AResult represents a task result
type A2AResult struct {
	TaskID      string            `json:"task_id"`
	Status      string            `json:"status"` // accepted, running, success, failed, partial
	Output      string            `json:"output"`
	Error       string            `json:"error,omitempty"`
	Artifacts   []Artifact        `json:"artifacts,omitempty"`
	StartedAt   time.Time         `json:"started_at,omitempty"`
	CompletedAt time.Time         `json:"completed_at,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type Artifact struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // file, text, url
	Content  string `json:"content"`
	MIMEType string `json:"mime_type,omitempty"`
}

type A2ATaskStatus struct {
	TaskID    string     `json:"task_id"`
	State     string     `json:"state"` // queued, accepted, running, partial, completed, failed
	Message   string     `json:"message,omitempty"`
	Progress  int        `json:"progress,omitempty"`
	Result    *A2AResult `json:"result,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (s *A2ATaskStatus) UnmarshalJSON(data []byte) error {
	type alias A2ATaskStatus
	var raw struct {
		alias
		Status string `json:"status"`
		Task   struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
			Status string `json:"status"`
			State  string `json:"state"`
		} `json:"task"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = A2ATaskStatus(raw.alias)
	if strings.TrimSpace(s.TaskID) == "" {
		s.TaskID = firstNonEmptyText(raw.Task.TaskID, raw.Task.ID)
	}
	if strings.TrimSpace(s.State) == "" {
		s.State = firstNonEmptyText(raw.Status, raw.Task.State, raw.Task.Status)
	}
	return nil
}

// A2AHandler handles incoming A2A messages
type A2AHandler struct {
	agentID string
	onTask  func(ctx context.Context, task A2ATask) (A2AResult, error)
	onMsg   func(ctx context.Context, msg A2AMessage) error
	client  *http.Client
	token   string
}

// NewA2AHandler creates a new A2A handler
func NewA2AHandler(agentID string) *A2AHandler {
	return &A2AHandler{
		agentID: agentID,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (h *A2AHandler) OnTask(handler func(ctx context.Context, task A2ATask) (A2AResult, error)) {
	h.onTask = handler
}

func (h *A2AHandler) OnMessage(handler func(ctx context.Context, msg A2AMessage) error) {
	h.onMsg = handler
}

func (h *A2AHandler) SetHTTPClient(client *http.Client) {
	if client != nil {
		h.client = client
	}
}

func (h *A2AHandler) SetAuthToken(token string) {
	h.token = strings.TrimSpace(token)
}

// ServeHTTP implements http.Handler for A2A protocol
func (h *A2AHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if token := strings.TrimSpace(h.token); token != "" {
		if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var msg A2AMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, fmt.Sprintf("decode error: %v", err), http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	switch msg.Type {
	case "task":
		if h.onTask == nil {
			http.Error(w, "task handler not configured", http.StatusNotImplemented)
			return
		}
		var task A2ATask
		if err := json.Unmarshal(msg.Payload, &task); err != nil {
			http.Error(w, fmt.Sprintf("task decode error: %v", err), http.StatusBadRequest)
			return
		}
		if task.ID == "" {
			task.ID = generateID()
		}
		acceptedAt := time.Now().UTC()
		h.reportTaskStatus(ctx, task.CallbackURL, A2ATaskStatus{
			TaskID:    task.ID,
			State:     "accepted",
			Message:   "task accepted",
			UpdatedAt: acceptedAt,
		})
		startedAt := time.Now().UTC()
		h.reportTaskStatus(ctx, task.CallbackURL, A2ATaskStatus{
			TaskID:    task.ID,
			State:     "running",
			Message:   "task running",
			UpdatedAt: startedAt,
		})
		result, err := h.onTask(ctx, task)
		if err != nil {
			completedAt := time.Now().UTC()
			failed := A2AResult{
				TaskID:      task.ID,
				Status:      "failed",
				Error:       err.Error(),
				StartedAt:   startedAt,
				CompletedAt: completedAt,
			}
			h.reportTaskStatus(ctx, task.CallbackURL, A2ATaskStatus{
				TaskID:    task.ID,
				State:     "failed",
				Message:   err.Error(),
				Result:    &failed,
				UpdatedAt: completedAt,
			})
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(failed)
			return
		}
		if result.TaskID == "" {
			result.TaskID = task.ID
		}
		if result.Status == "" {
			result.Status = "success"
		}
		if result.StartedAt.IsZero() {
			result.StartedAt = startedAt
		}
		if result.CompletedAt.IsZero() {
			result.CompletedAt = time.Now().UTC()
		}
		finalState := "completed"
		switch result.Status {
		case "failed":
			finalState = "failed"
		case "partial":
			finalState = "partial"
		}
		h.reportTaskStatus(ctx, task.CallbackURL, A2ATaskStatus{
			TaskID:    task.ID,
			State:     finalState,
			Message:   result.Status,
			Result:    &result,
			UpdatedAt: result.CompletedAt,
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)

	case "text", "message":
		if h.onMsg == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := h.onMsg(ctx, msg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, fmt.Sprintf("unknown message type: %s", msg.Type), http.StatusBadRequest)
	}
}

func (h *A2AHandler) reportTaskStatus(ctx context.Context, callbackURL string, status A2ATaskStatus) {
	callbackURL = strings.TrimSpace(callbackURL)
	if callbackURL == "" || h.client == nil {
		return
	}
	body, err := json.Marshal(status)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}

// A2AClient sends A2A messages to other agents
type A2AClient struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewA2AClient creates a new A2A client
func NewA2AClient(baseURL string) *A2AClient {
	return NewA2AClientWithHTTPClient(baseURL, &http.Client{Timeout: 30 * time.Second})
}

func NewA2AClientWithHTTPClient(baseURL string, client *http.Client) *A2AClient {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &A2AClient{
		client:  client,
		baseURL: normalizeA2ABaseURL(baseURL),
	}
}

func (c *A2AClient) SetAuthToken(token string) {
	c.token = strings.TrimSpace(token)
}

// SendMessage sends a text message to another agent
func (c *A2AClient) SendMessage(ctx context.Context, from, to, content string) error {
	msg := A2AMessage{
		ID:        generateID(),
		From:      from,
		To:        to,
		Type:      "text",
		Content:   content,
		Timestamp: time.Now().UTC(),
	}
	return c.post(ctx, msg)
}

// SendTask sends a task to another agent
func (c *A2AClient) SendTask(ctx context.Context, from, to string, task A2ATask) (*A2AResult, error) {
	payload, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("marshal task: %w", err)
	}

	msg := A2AMessage{
		ID:        generateID(),
		From:      from,
		To:        to,
		Type:      "task",
		Content:   task.Description,
		Payload:   payload,
		Timestamp: time.Now().UTC(),
	}

	resp, statusCode, err := c.postWithResponse(ctx, msg)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(resp)) == 0 && statusCode == http.StatusAccepted {
		return normalizeA2AResult(&A2AResult{
			TaskID: task.ID,
			Status: "accepted",
		}), nil
	}

	result, err := decodeA2AResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	return result, nil
}

func (c *A2AClient) post(ctx context.Context, msg A2AMessage) error {
	_, _, err := c.postWithResponse(ctx, msg)
	return err
}

func (c *A2AClient) postWithResponse(ctx context.Context, msg A2AMessage) ([]byte, int, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, normalizeA2AEndpoint(c.baseURL), bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http error: %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	return buf.Bytes(), resp.StatusCode, nil
}

func generateID() string {
	return fmt.Sprintf("msg_%d", time.Now().UnixNano())
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

func decodeA2AResponse(raw []byte) (*A2AResult, error) {
	var direct A2AResult
	if err := json.Unmarshal(raw, &direct); err == nil && hasA2AResultPayload(direct) {
		return normalizeA2AResult(&direct), nil
	}

	var wrapped struct {
		Result  *A2AResult `json:"result"`
		TaskID  string     `json:"task_id"`
		Status  string     `json:"status"`
		State   string     `json:"state"`
		Output  string     `json:"output"`
		Error   string     `json:"error"`
		Message string     `json:"message"`
		Task    struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
			Status string `json:"status"`
			State  string `json:"state"`
		} `json:"task"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, err
	}
	if wrapped.Result != nil && hasA2AResultPayload(*wrapped.Result) {
		return normalizeA2AResult(wrapped.Result), nil
	}

	result := &A2AResult{
		TaskID: firstNonEmptyText(wrapped.TaskID, wrapped.Task.TaskID, wrapped.Task.ID),
		Status: normalizeA2AResultStatus(firstNonEmptyText(wrapped.Status, wrapped.State, wrapped.Task.Status, wrapped.Task.State)),
		Output: strings.TrimSpace(wrapped.Output),
		Error:  strings.TrimSpace(wrapped.Error),
	}
	if result.Error == "" && strings.EqualFold(result.Status, "failed") {
		result.Error = strings.TrimSpace(wrapped.Message)
	}
	if result.Output == "" && isA2ASuccessState(result.Status) {
		result.Output = strings.TrimSpace(wrapped.Message)
	}
	if !hasA2AResultPayload(*result) {
		return nil, fmt.Errorf("unsupported a2a response payload")
	}
	return normalizeA2AResult(result), nil
}

func hasA2AResultPayload(result A2AResult) bool {
	return strings.TrimSpace(result.Status) != "" ||
		strings.TrimSpace(result.Output) != "" ||
		strings.TrimSpace(result.Error) != "" ||
		len(result.Artifacts) > 0 ||
		!result.StartedAt.IsZero() ||
		!result.CompletedAt.IsZero()
}

func normalizeA2AResult(result *A2AResult) *A2AResult {
	if result == nil {
		return &A2AResult{}
	}
	result.TaskID = strings.TrimSpace(result.TaskID)
	result.Status = normalizeA2AResultStatus(result.Status)
	result.Output = strings.TrimSpace(result.Output)
	result.Error = strings.TrimSpace(result.Error)
	return result
}

func normalizeA2AResultStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "queued":
		return "accepted"
	case "in_progress", "processing":
		return "running"
	case "success", "succeeded", "complete":
		return "success"
	case "error":
		return "failed"
	case "partial_success":
		return "partial"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func isA2ASuccessState(status string) bool {
	switch normalizeA2AResultStatus(status) {
	case "success", "partial", "completed":
		return true
	default:
		return false
	}
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
