package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xclaw/cli/config"
	"xclaw/cli/db"
	"xclaw/cli/protocol"
)

func TestHandleA2ADispatchStoresLifecycleAndResult(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{
		cfg: config.RuntimeConfig{
			Server: config.ServerConfig{Host: "127.0.0.1", Port: 5310},
		},
		store: store,
		a2aClientFn: func(baseURL string) *protocol.A2AClient {
			return protocol.NewA2AClientWithHTTPClient(baseURL, &http.Client{
				Transport: a2aRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					respBody := `{"task_id":"a2a_1","status":"success","output":"remote done"}`
					return jsonResp(http.StatusOK, []byte(respBody)), nil
				}),
			})
		},
	}

	body := `{"peer_url":"http://peer.local","from":"local","to":"remote","task":"do work","inputs":{"agent_id":"agent-1"}}`
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/a2a/dispatch", bytes.NewBufferString(body))
	req.Host = "localhost:5310"
	rec := httptest.NewRecorder()

	server.handleA2ADispatch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var created a2aTaskRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	item := waitForA2ACompletion(t, server, created.ID)
	if item.Status != "completed" || item.Output != "remote done" {
		t.Fatalf("unexpected task record: %#v", item)
	}
}

func TestHandleA2ATaskStatusUpdatesRecord(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{store: store}
	err = server.upsertA2ATask(context.Background(), a2aTaskRecord{
		ID:        "task-1",
		Status:    "queued",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	statusBody := `{"task_id":"task-1","state":"completed","result":{"task_id":"task-1","status":"success","output":"done"}}`
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/a2a/tasks/task-1/status", bytes.NewBufferString(statusBody))
	rec := httptest.NewRecorder()

	server.handleA2ATaskByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	item, ok, err := server.findA2ATask(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("find task: %v", err)
	}
	if !ok || item.Status != "completed" || item.Output != "done" {
		t.Fatalf("unexpected task record: %#v", item)
	}
}

func TestHandleA2ATaskStatusAcceptsStatusAlias(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{store: store}
	err = server.upsertA2ATask(context.Background(), a2aTaskRecord{
		ID:        "task-alias",
		Status:    "queued",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	statusBody := `{"task_id":"task-alias","status":"running","message":"started"}`
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/a2a/tasks/task-alias/status", bytes.NewBufferString(statusBody))
	rec := httptest.NewRecorder()

	server.handleA2ATaskByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	item, ok, err := server.findA2ATask(context.Background(), "task-alias")
	if err != nil {
		t.Fatalf("find task: %v", err)
	}
	if !ok || item.Status != "running" {
		t.Fatalf("unexpected task record: %#v", item)
	}
}

func TestHandleA2ATaskStatusNormalizesSuccessAlias(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{store: store}
	err = server.upsertA2ATask(context.Background(), a2aTaskRecord{
		ID:        "task-success-alias",
		Status:    "running",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	statusBody := `{"task_id":"task-success-alias","status":"success","result":{"task_id":"task-success-alias","status":"success","output":"done"}}`
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/a2a/tasks/task-success-alias/status", bytes.NewBufferString(statusBody))
	rec := httptest.NewRecorder()

	server.handleA2ATaskByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	item, ok, err := server.findA2ATask(context.Background(), "task-success-alias")
	if err != nil {
		t.Fatalf("find task: %v", err)
	}
	if !ok || item.Status != "completed" || item.Output != "done" {
		t.Fatalf("unexpected task record: %#v", item)
	}
}

func TestHandleA2ATaskStatusInfersStateFromResultStatus(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{store: store}
	err = server.upsertA2ATask(context.Background(), a2aTaskRecord{
		ID:        "task-result-only",
		Status:    "queued",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	statusBody := `{"task_id":"task-result-only","result":{"task_id":"task-result-only","status":"partial","output":"half done"}}`
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/a2a/tasks/task-result-only/status", bytes.NewBufferString(statusBody))
	rec := httptest.NewRecorder()

	server.handleA2ATaskByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	item, ok, err := server.findA2ATask(context.Background(), "task-result-only")
	if err != nil {
		t.Fatalf("find task: %v", err)
	}
	if !ok || item.Status != "partial" || item.Output != "half done" {
		t.Fatalf("unexpected task record: %#v", item)
	}
}

func TestHandleA2ATaskStatusAcceptsNestedTaskEnvelope(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{store: store}
	err = server.upsertA2ATask(context.Background(), a2aTaskRecord{
		ID:        "task-envelope",
		Status:    "queued",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	statusBody := `{"task":{"id":"task-envelope","status":"succeeded"},"result":{"task_id":"task-envelope","status":"success","output":"enveloped"}}`
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/a2a/tasks/task-envelope/status", bytes.NewBufferString(statusBody))
	rec := httptest.NewRecorder()

	server.handleA2ATaskByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	item, ok, err := server.findA2ATask(context.Background(), "task-envelope")
	if err != nil {
		t.Fatalf("find task: %v", err)
	}
	if !ok || item.Status != "completed" || item.Output != "enveloped" {
		t.Fatalf("unexpected task record: %#v", item)
	}
}

func TestHandleA2ARegistryRegisterAndPeers(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{store: store}

	registerReq := httptest.NewRequest(http.MethodPost, "http://localhost/register", bytes.NewBufferString(`{
		"name":"peer-a",
		"endpoint":"http://peer-a.local/a2a",
		"version":"1.0.0",
		"capabilities":["task"]
	}`))
	registerRec := httptest.NewRecorder()
	server.handleA2ARegistryRegister(registerRec, registerReq)
	if registerRec.Code != http.StatusOK {
		t.Fatalf("unexpected register status: %d body=%s", registerRec.Code, registerRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "http://localhost/peers", nil)
	listRec := httptest.NewRecorder()
	server.handleA2ARegistryPeers(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("unexpected list status: %d body=%s", listRec.Code, listRec.Body.String())
	}
	var items []protocol.AgentCard
	if err := json.Unmarshal(listRec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode peers: %v", err)
	}
	if len(items) != 1 || items[0].Endpoint != "http://peer-a.local/a2a" {
		t.Fatalf("unexpected peers: %#v", items)
	}
}

func TestHandleA2ADispatchResolvesPeerNameAndToken(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{
		store: store,
		a2aClientFn: func(baseURL string) *protocol.A2AClient {
			return protocol.NewA2AClientWithHTTPClient(baseURL, &http.Client{
				Transport: a2aRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.String() != "http://peer-name.local/a2a" {
						t.Fatalf("unexpected request url: %s", req.URL.String())
					}
					if got := req.Header.Get("Authorization"); got != "Bearer peer-token" {
						t.Fatalf("unexpected auth header: %s", got)
					}
					return jsonResp(http.StatusOK, []byte(`{"task_id":"a2a_name","status":"success","output":"name matched"}`)), nil
				}),
			})
		},
	}
	if err := server.savePeers(context.Background(), []map[string]string{
		{
			"name":  "peer-name",
			"url":   "http://peer-name.local/a2a",
			"token": "peer-token",
		},
	}); err != nil {
		t.Fatalf("save peers: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/a2a/dispatch", bytes.NewBufferString(`{
		"peer_name":"peer-name",
		"task":"route by name"
	}`))
	rec := httptest.NewRecorder()
	server.handleA2ADispatch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var created a2aTaskRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.PeerURL != "http://peer-name.local" {
		t.Fatalf("unexpected peer url: %s", created.PeerURL)
	}
	if created.To != "peer-name" {
		t.Fatalf("unexpected target name: %s", created.To)
	}
	item := waitForA2ACompletion(t, server, created.ID)
	if item.Status != "completed" || item.Output != "name matched" {
		t.Fatalf("unexpected task record: %#v", item)
	}
}

func TestHandleA2ADispatchResolvesCapabilityFromRegistry(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var (
		mu      sync.Mutex
		baseURL string
	)
	server := &Server{
		store: store,
		a2aClientFn: func(candidate string) *protocol.A2AClient {
			mu.Lock()
			baseURL = candidate
			mu.Unlock()
			return protocol.NewA2AClientWithHTTPClient(candidate, &http.Client{
				Transport: a2aRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					return jsonResp(http.StatusOK, []byte(`{"task_id":"a2a_cap","status":"success","output":"cap matched"}`)), nil
				}),
			})
		},
	}
	if err := server.upsertA2ARegistryPeer(context.Background(), protocol.AgentCard{
		ID:           "viz-peer",
		Name:         "viz-peer",
		Endpoint:     "http://viz.local/a2a",
		Capabilities: []string{"visualization"},
		TaskTypes:    []string{"chart"},
		LastSeenAt:   time.Now().UTC(),
		Source:       "registry",
	}); err != nil {
		t.Fatalf("save registry peer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/a2a/dispatch", bytes.NewBufferString(`{
		"capability":"visualization",
		"task":"render chart"
	}`))
	rec := httptest.NewRecorder()
	server.handleA2ADispatch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var created a2aTaskRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.To != "viz-peer" {
		t.Fatalf("unexpected target name: %s", created.To)
	}
	item := waitForA2ACompletion(t, server, created.ID)
	if item.Status != "completed" || item.Output != "cap matched" {
		t.Fatalf("unexpected task record: %#v", item)
	}
	mu.Lock()
	defer mu.Unlock()
	if baseURL != "http://viz.local" {
		t.Fatalf("unexpected base url: %s", baseURL)
	}
}

func TestHandleA2APeersMergesManualAndRegistryViews(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{store: store}
	if err := server.savePeers(context.Background(), []map[string]string{
		{
			"name":  "peer-a",
			"url":   "http://peer-a.local",
			"token": "secret",
		},
	}); err != nil {
		t.Fatalf("save peers: %v", err)
	}
	if err := server.upsertA2ARegistryPeer(context.Background(), protocol.AgentCard{
		ID:           "peer-a-card",
		Name:         "peer-a-registry",
		Endpoint:     "http://peer-a.local/a2a",
		Description:  "registry peer",
		Capabilities: []string{"task"},
		LastSeenAt:   time.Now().UTC(),
		Source:       "registry",
	}); err != nil {
		t.Fatalf("save registry peer: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/a2a/peers", nil)
	rec := httptest.NewRecorder()
	server.handleA2APeers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var items []a2aPeerView
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode peers: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("unexpected peer count: %#v", items)
	}
	if items[0].URL != "http://peer-a.local" || items[0].Endpoint != "http://peer-a.local/a2a" {
		t.Fatalf("unexpected merged peer: %#v", items[0])
	}
	if !items[0].HasToken {
		t.Fatalf("expected token marker on merged peer: %#v", items[0])
	}
	if items[0].Source != "manual,registry" {
		t.Fatalf("unexpected sources: %#v", items[0])
	}
}

func TestHandleA2ADispatchKeepsAcceptedAsyncState(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "xclaw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	server := &Server{
		store: store,
		a2aClientFn: func(baseURL string) *protocol.A2AClient {
			return protocol.NewA2AClientWithHTTPClient(baseURL, &http.Client{
				Transport: a2aRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					return jsonResp(http.StatusAccepted, []byte(`{"task_id":"remote-accepted","state":"accepted","message":"queued"}`)), nil
				}),
			})
		},
	}

	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/a2a/dispatch", bytes.NewBufferString(`{
		"peer_url":"http://peer.local/a2a",
		"task":"async remote work"
	}`))
	rec := httptest.NewRecorder()
	server.handleA2ADispatch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var created a2aTaskRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		item, ok, err := server.findA2ATask(context.Background(), created.ID)
		if err != nil {
			t.Fatalf("find task: %v", err)
		}
		if ok && item.Status == "accepted" {
			if item.Progress != 20 {
				t.Fatalf("unexpected progress for accepted task: %#v", item)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	item, _, err := server.findA2ATask(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("find final task: %v", err)
	}
	t.Fatalf("task did not settle to accepted: %#v", item)
}

func waitForA2ACompletion(t *testing.T, server *Server, taskID string) a2aTaskRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		item, ok, err := server.findA2ATask(context.Background(), taskID)
		if err != nil {
			t.Fatalf("find task: %v", err)
		}
		if ok && (item.Status == "completed" || item.Status == "failed" || item.Status == "partial") {
			return item
		}
		time.Sleep(20 * time.Millisecond)
	}
	item, ok, err := server.findA2ATask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("find final task: %v", err)
	}
	if !ok {
		t.Fatalf("task not found: %s", taskID)
	}
	t.Fatalf("task did not complete: %#v", item)
	return a2aTaskRecord{}
}

type a2aRoundTripFunc func(req *http.Request) (*http.Response, error)

func (fn a2aRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func jsonResp(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloser{bytes.NewReader(body)},
	}
}

type ioNopCloser struct {
	*bytes.Reader
}

func (ioNopCloser) Close() error { return nil }
