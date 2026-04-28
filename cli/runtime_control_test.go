package main

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"xclaw/cli/config"
)

func TestNormalizeLoopbackHost(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"::":        "127.0.0.1",
		"[::]":      "127.0.0.1",
		"127.0.0.1": "127.0.0.1",
		"localhost": "localhost",
	}

	for input, want := range cases {
		if got := normalizeLoopbackHost(input); got != want {
			t.Fatalf("normalizeLoopbackHost(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLocalHealthURL(t *testing.T) {
	t.Parallel()

	got := localHealthURL(config.ServerConfig{
		Host: "0.0.0.0",
		Port: 5310,
		TLS:  true,
	})
	want := "https://127.0.0.1:5310/healthz"
	if got != want {
		t.Fatalf("localHealthURL() = %q, want %q", got, want)
	}
}

func TestHealthResponseMatches(t *testing.T) {
	t.Parallel()

	if !healthResponseMatches(http.StatusOK, "43210", 43210) {
		t.Fatalf("expected matching pid to pass")
	}
	if healthResponseMatches(http.StatusOK, "123", 456) {
		t.Fatalf("expected mismatched pid to fail")
	}
	if healthResponseMatches(http.StatusBadGateway, "43210", 43210) {
		t.Fatalf("expected non-200 status to fail")
	}
	if !healthResponseMatches(http.StatusOK, "", 0) {
		t.Fatalf("expected wildcard pid check to pass")
	}
}

func TestWaitForHTTPDrain(t *testing.T) {
	t.Parallel()

	var active atomic.Int64
	active.Store(1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		active.Store(0)
	}()

	start := time.Now()
	waitForHTTPDrain(func() int64 { return active.Load() }, 300*time.Millisecond)
	if active.Load() != 0 {
		t.Fatalf("expected active requests to drain to zero")
	}
	if time.Since(start) >= 300*time.Millisecond {
		t.Fatalf("waitForHTTPDrain should return before timeout")
	}
}
