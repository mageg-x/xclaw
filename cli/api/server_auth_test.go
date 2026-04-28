package api

import (
	"net/http/httptest"
	"testing"
)

func TestExtractRequestToken(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "http://example.com/api/sessions/1/ws?access_token=ws-token", nil)
	if got := extractRequestToken(req); got != "ws-token" {
		t.Fatalf("extractRequestToken(ws query) = %q, want %q", got, "ws-token")
	}

	req = httptest.NewRequest("GET", "http://example.com/api/sessions/1/events?access_token=bad", nil)
	if got := extractRequestToken(req); got != "" {
		t.Fatalf("extractRequestToken(non-ws query) = %q, want empty", got)
	}

	req = httptest.NewRequest("GET", "http://example.com/api/sessions/1/ws", nil)
	req.Header.Set("Authorization", "Bearer header-token")
	if got := extractRequestToken(req); got != "header-token" {
		t.Fatalf("extractRequestToken(header) = %q, want %q", got, "header-token")
	}
}
