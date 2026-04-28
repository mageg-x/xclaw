package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestA2AHandlerReportsLifecycleStatuses(t *testing.T) {
	var statuses []A2ATaskStatus
	callback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status A2ATaskStatus
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Fatalf("decode callback: %v", err)
		}
		statuses = append(statuses, status)
		w.WriteHeader(http.StatusOK)
	})

	handler := NewA2AHandler("agent-a")
	handler.SetHTTPClient(newRouteClient(map[string]http.Handler{
		"http://callback.local/tasks/1/status": callback,
	}))
	handler.OnTask(func(ctx context.Context, task A2ATask) (A2AResult, error) {
		return A2AResult{
			TaskID: task.ID,
			Status: "success",
			Output: "done",
		}, nil
	})

	task := A2ATask{
		ID:          "task-1",
		Name:        "demo",
		Description: "run demo",
		CallbackURL: "http://callback.local/tasks/1/status",
	}
	payload, _ := json.Marshal(task)
	msg := A2AMessage{
		ID:      "msg-1",
		From:    "caller",
		To:      "agent-a",
		Type:    "task",
		Payload: payload,
	}
	body, _ := json.Marshal(msg)
	req := httptest.NewRequest(http.MethodPost, "http://agent.local/a2a", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d", rec.Code)
	}
	if len(statuses) != 3 {
		t.Fatalf("expected 3 statuses, got %d", len(statuses))
	}
	want := []string{"accepted", "running", "completed"}
	for i, item := range statuses {
		if item.State != want[i] {
			t.Fatalf("status %d: got %s want %s", i, item.State, want[i])
		}
	}
	if statuses[2].Result == nil || statuses[2].Result.Output != "done" {
		t.Fatalf("unexpected final result: %#v", statuses[2].Result)
	}
}

func TestA2AClientSendTaskInMemory(t *testing.T) {
	handler := NewA2AHandler("agent-b")
	handler.OnTask(func(ctx context.Context, task A2ATask) (A2AResult, error) {
		return A2AResult{
			TaskID: task.ID,
			Status: "success",
			Output: "echo:" + task.Description,
		}, nil
	})

	client := NewA2AClientWithHTTPClient("http://agent-b.local", newRouteClient(map[string]http.Handler{
		"http://agent-b.local/a2a": handler,
	}))
	result, err := client.SendTask(context.Background(), "caller", "agent-b", A2ATask{
		ID:          "task-2",
		Name:        "echo",
		Description: "hello",
	})
	if err != nil {
		t.Fatalf("SendTask failed: %v", err)
	}
	if result.Output != "echo:hello" {
		t.Fatalf("unexpected output: %#v", result)
	}
}

func TestA2AClientAcceptsEndpointURLWithoutDoubleAppending(t *testing.T) {
	client := NewA2AClientWithHTTPClient("http://agent-b.local/a2a", newRouteClient(map[string]http.Handler{
		"http://agent-b.local/a2a": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var msg A2AMessage
			if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			var task A2ATask
			if err := json.Unmarshal(msg.Payload, &task); err != nil {
				t.Fatalf("decode task: %v", err)
			}
			_ = json.NewEncoder(w).Encode(A2AResult{
				TaskID: task.ID,
				Status: "success",
				Output: "ok",
			})
		}),
	}))
	result, err := client.SendTask(context.Background(), "caller", "agent-b", A2ATask{
		ID:          "task-endpoint",
		Name:        "echo",
		Description: "hello",
	})
	if err != nil {
		t.Fatalf("SendTask failed: %v", err)
	}
	if result.Output != "ok" {
		t.Fatalf("unexpected output: %#v", result)
	}
}

func TestA2AClientRoundTripWithRealHTTPServerAndCallback(t *testing.T) {
	var (
		mu       sync.Mutex
		statuses []A2ATaskStatus
	)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status A2ATaskStatus
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Fatalf("decode callback status: %v", err)
		}
		mu.Lock()
		statuses = append(statuses, status)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer callback.Close()

	handler := NewA2AHandler("agent-http")
	handler.SetAuthToken("secret")
	handler.OnTask(func(ctx context.Context, task A2ATask) (A2AResult, error) {
		return A2AResult{
			TaskID: task.ID,
			Status: "partial",
			Output: "streamed-result",
		}, nil
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	client := NewA2AClient(server.URL + "/a2a")
	client.SetAuthToken("secret")
	result, err := client.SendTask(context.Background(), "caller", "agent-http", A2ATask{
		ID:          "task-http",
		Name:        "demo",
		Description: "real http",
		CallbackURL: callback.URL,
	})
	if err != nil {
		t.Fatalf("SendTask failed: %v", err)
	}
	if result.Status != "partial" || result.Output != "streamed-result" {
		t.Fatalf("unexpected result: %#v", result)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(statuses)
		last := A2ATaskStatus{}
		if count > 0 {
			last = statuses[count-1]
		}
		mu.Unlock()
		if count == 3 && last.State == "partial" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) != 3 {
		t.Fatalf("expected 3 statuses, got %d", len(statuses))
	}
	want := []string{"accepted", "running", "partial"}
	for i := range want {
		if statuses[i].State != want[i] {
			t.Fatalf("status %d: got %s want %s", i, statuses[i].State, want[i])
		}
	}
}

func TestA2AClientDecodesWrappedResultPayload(t *testing.T) {
	client := NewA2AClientWithHTTPClient("http://wrapped.local", newRouteClient(map[string]http.Handler{
		"http://wrapped.local/a2a": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{
					"task_id": "task-wrapped",
					"status":  "succeeded",
					"output":  "wrapped-ok",
				},
			})
		}),
	}))

	result, err := client.SendTask(context.Background(), "caller", "wrapped", A2ATask{
		ID:          "task-wrapped",
		Name:        "wrapped",
		Description: "wrapped result",
	})
	if err != nil {
		t.Fatalf("SendTask failed: %v", err)
	}
	if result.Status != "success" || result.Output != "wrapped-ok" {
		t.Fatalf("unexpected wrapped result: %#v", result)
	}
}

func TestA2AClientDecodesAcceptedAsyncPayload(t *testing.T) {
	client := NewA2AClientWithHTTPClient("http://async.local", newRouteClient(map[string]http.Handler{
		"http://async.local/a2a": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task_id": "task-async",
				"state":   "queued",
				"message": "accepted for processing",
			})
		}),
	}))

	result, err := client.SendTask(context.Background(), "caller", "async", A2ATask{
		ID:          "task-async",
		Name:        "async",
		Description: "async task",
	})
	if err != nil {
		t.Fatalf("SendTask failed: %v", err)
	}
	if result.TaskID != "task-async" || result.Status != "accepted" {
		t.Fatalf("unexpected async result: %#v", result)
	}
}

func TestA2AClientAcceptsEmpty202Response(t *testing.T) {
	client := NewA2AClientWithHTTPClient("http://empty-async.local", newRouteClient(map[string]http.Handler{
		"http://empty-async.local/a2a": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}),
	}))

	result, err := client.SendTask(context.Background(), "caller", "empty-async", A2ATask{
		ID:          "task-empty-async",
		Name:        "async",
		Description: "empty async task",
	})
	if err != nil {
		t.Fatalf("SendTask failed: %v", err)
	}
	if result.TaskID != "task-empty-async" || result.Status != "accepted" {
		t.Fatalf("unexpected empty async result: %#v", result)
	}
}

func TestA2AClientDecodesTaskEnvelopeAcceptedPayload(t *testing.T) {
	client := NewA2AClientWithHTTPClient("http://task-envelope.local", newRouteClient(map[string]http.Handler{
		"http://task-envelope.local/a2a": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task": map[string]any{
					"id":     "task-envelope-async",
					"status": "queued",
				},
				"message": "accepted",
			})
		}),
	}))

	result, err := client.SendTask(context.Background(), "caller", "task-envelope", A2ATask{
		ID:          "task-envelope-async",
		Name:        "async",
		Description: "task envelope async task",
	})
	if err != nil {
		t.Fatalf("SendTask failed: %v", err)
	}
	if result.TaskID != "task-envelope-async" || result.Status != "accepted" {
		t.Fatalf("unexpected task envelope async result: %#v", result)
	}
}

func TestA2AHandlerAuthToken(t *testing.T) {
	handler := NewA2AHandler("agent-c")
	handler.SetAuthToken("secret")
	handler.OnTask(func(ctx context.Context, task A2ATask) (A2AResult, error) {
		return A2AResult{TaskID: task.ID, Status: "success"}, nil
	})

	task := A2ATask{ID: "task-auth", Name: "demo", Description: "secured"}
	payload, _ := json.Marshal(task)
	msg := A2AMessage{ID: "msg-auth", Type: "task", Payload: payload}
	body, _ := json.Marshal(msg)

	req := httptest.NewRequest(http.MethodPost, "http://agent.local/a2a", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://agent.local/a2a", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected success with token, got %d", rec.Code)
	}
}

func TestDiscoverySyncRegistryOnce(t *testing.T) {
	var registered AgentCard
	registry := routeRoundTripper{
		"http://registry.local/register": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&registered); err != nil {
				t.Fatalf("decode register body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		}),
		"http://registry.local/peers": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]AgentCard{{
				Name:       "peer-b",
				Endpoint:   "http://peer-b.local/a2a",
				Version:    "1.0.0",
				LastSeenAt: time.Now().UTC(),
			}})
		}),
	}

	d := NewDiscovery(AgentCard{
		Name:       "self",
		Endpoint:   "http://self.local/a2a",
		Version:    "1.0.0",
		LastSeenAt: time.Now().UTC(),
	}, "http://registry.local")

	client := &http.Client{Transport: registry}
	d.syncRegistryOnce(context.Background(), client)

	if registered.Endpoint != "http://self.local/a2a" {
		t.Fatalf("unexpected registered card: %#v", registered)
	}
	peers := d.ListPeers()
	if len(peers) != 1 || peers[0].Endpoint != "http://peer-b.local/a2a" {
		t.Fatalf("unexpected synced peers: %#v", peers)
	}
}

type routeRoundTripper map[string]http.Handler

func (rt routeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.URL.String()
	handler, ok := rt[key]
	if !ok {
		for pattern, item := range rt {
			if strings.HasSuffix(pattern, "*") && strings.HasPrefix(key, strings.TrimSuffix(pattern, "*")) {
				handler = item
				ok = true
				break
			}
		}
	}
	if !ok {
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusNotFound)
		return rec.Result(), nil
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func newRouteClient(routes map[string]http.Handler) *http.Client {
	return &http.Client{Transport: routeRoundTripper(routes)}
}
