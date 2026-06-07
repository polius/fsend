package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

const (
	srvKiB = 1024
	srvMiB = 1024 * 1024
	srvGiB = 1024 * 1024 * 1024
	srvTiB = 1024 * 1024 * 1024 * 1024
)

func srvFreePortTCP(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func srvFreePortUDP(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()
	return port
}

func TestServerEnvOr(t *testing.T) {
	const k = "FSEND_TEST_ENV_OR"
	os.Unsetenv(k)
	if got := envOr(k, "default"); got != "default" {
		t.Errorf("unset: got %q, want default", got)
	}
	t.Setenv(k, "value")
	if got := envOr(k, "default"); got != "value" {
		t.Errorf("set: got %q, want value", got)
	}
	t.Setenv(k, "")
	if got := envOr(k, "default"); got != "default" {
		t.Errorf("empty: got %q, want default", got)
	}
}

func TestServerEnvInt(t *testing.T) {
	const k = "FSEND_TEST_ENV_INT"
	os.Unsetenv(k)
	if got := envInt(k, 42); got != 42 {
		t.Errorf("unset: got %d, want 42", got)
	}
	t.Setenv(k, "7")
	if got := envInt(k, 42); got != 7 {
		t.Errorf("set: got %d, want 7", got)
	}
	t.Setenv(k, "not-a-number")
	if got := envInt(k, 42); got != 42 {
		t.Errorf("bad: got %d, want 42", got)
	}
}

func TestServerEnvDuration(t *testing.T) {
	const k = "FSEND_TEST_ENV_DURATION"
	os.Unsetenv(k)
	if got := envDuration(k, 5*time.Second); got != 5*time.Second {
		t.Errorf("unset: got %v, want 5s", got)
	}
	t.Setenv(k, "30s")
	if got := envDuration(k, 5*time.Second); got != 30*time.Second {
		t.Errorf("30s: got %v, want 30s", got)
	}
	t.Setenv(k, "1h30m")
	if got := envDuration(k, 5*time.Second); got != 90*time.Minute {
		t.Errorf("1h30m: got %v, want 90m", got)
	}
	t.Setenv(k, "garbage")
	if got := envDuration(k, 5*time.Second); got != 5*time.Second {
		t.Errorf("bad: got %v, want 5s", got)
	}
}

func TestServerEnvBytes(t *testing.T) {
	const k = "FSEND_TEST_ENV_BYTES"
	const def uint64 = 100
	cases := []struct {
		in   string
		want uint64
	}{
		{"", def},
		{"500", 500},
		{"500b", 500},
		{"500B", 500},
		{"1k", 1000},
		{"1kb", 1000},
		{"1KB", 1000},
		{"1ki", 1024},
		{"1KiB", srvKiB},
		{"1m", 1000 * 1000},
		{"1MB", 1000 * 1000},
		{"1MiB", srvMiB},
		{"100MiB", 100 * srvMiB},
		{"1g", 1000 * 1000 * 1000},
		{"1GiB", srvGiB},
		{"1t", 1000 * 1000 * 1000 * 1000},
		{"1TiB", srvTiB},
		{"  10MiB  ", 10 * srvMiB},
		{"1.5MiB", uint64(1.5 * float64(srvMiB))},
		{"0", 0},
		{"-1", def},
		{"not-a-number", def},
		{"100ZZ", def},
		{"M", def},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if c.in == "" {
				os.Unsetenv(k)
			} else {
				t.Setenv(k, c.in)
			}
			if got := envBytes(k, def); got != c.want {
				t.Errorf("envBytes(%q): got %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// clearServerEnv unsets every FSEND_* var loadServerConfig consults, so
// individual tests get a clean baseline regardless of host shell state.
func clearServerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"FSEND_HTTP_ADDR",
		"FSEND_UDP_ADDR",
		"FSEND_PUBLIC_ADDR",
		"FSEND_LOG_LEVEL",
		"FSEND_MAX_SESSIONS_PER_IP",
		"FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN",
		"FSEND_MAX_RELAY_BYTES_PER_SESSION",
		"FSEND_SESSION_IDLE_TIMEOUT",
	} {
		os.Unsetenv(k)
	}
}

func TestServerLoadConfigDefaults(t *testing.T) {
	clearServerEnv(t)
	cfg := loadServerConfig()
	if cfg.httpAddr != ":8080" {
		t.Errorf("httpAddr: got %q, want :8080", cfg.httpAddr)
	}
	if cfg.udpAddr != ":443" {
		t.Errorf("udpAddr: got %q, want :443", cfg.udpAddr)
	}
	if cfg.maxSessionsPerIP != 5 {
		t.Errorf("maxSessionsPerIP: got %d, want 5", cfg.maxSessionsPerIP)
	}
	if cfg.maxNewSessionsPerMin != 30 {
		t.Errorf("maxNewSessionsPerMin: got %d, want 30", cfg.maxNewSessionsPerMin)
	}
	if cfg.maxBytesPerSession != 100*srvMiB {
		t.Errorf("maxBytesPerSession: got %d, want %d", cfg.maxBytesPerSession, 100*srvMiB)
	}
	if cfg.sessionIdleTimeout != 60*time.Second {
		t.Errorf("sessionIdleTimeout: got %v, want 60s", cfg.sessionIdleTimeout)
	}
	if cfg.logLevel != slog.LevelInfo {
		t.Errorf("logLevel: got %v, want info", cfg.logLevel)
	}
}

func TestServerLoadConfigOverrides(t *testing.T) {
	clearServerEnv(t)
	t.Setenv("FSEND_HTTP_ADDR", ":19999")
	t.Setenv("FSEND_UDP_ADDR", ":29999")
	t.Setenv("FSEND_MAX_SESSIONS_PER_IP", "12")
	t.Setenv("FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN", "120")
	t.Setenv("FSEND_MAX_RELAY_BYTES_PER_SESSION", "250MiB")
	t.Setenv("FSEND_SESSION_IDLE_TIMEOUT", "90s")
	cfg := loadServerConfig()
	if cfg.httpAddr != ":19999" {
		t.Errorf("httpAddr: got %q", cfg.httpAddr)
	}
	if cfg.udpAddr != ":29999" {
		t.Errorf("udpAddr: got %q", cfg.udpAddr)
	}
	if cfg.maxSessionsPerIP != 12 {
		t.Errorf("maxSessionsPerIP: got %d", cfg.maxSessionsPerIP)
	}
	if cfg.maxNewSessionsPerMin != 120 {
		t.Errorf("maxNewSessionsPerMin: got %d", cfg.maxNewSessionsPerMin)
	}
	if cfg.maxBytesPerSession != 250*srvMiB {
		t.Errorf("maxBytesPerSession: got %d", cfg.maxBytesPerSession)
	}
	if cfg.sessionIdleTimeout != 90*time.Second {
		t.Errorf("sessionIdleTimeout: got %v", cfg.sessionIdleTimeout)
	}
}

func TestServerLoadConfigLogLevels(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"WARN":    slog.LevelWarn,
		"error":   slog.LevelError,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"garbage": slog.LevelInfo,
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			clearServerEnv(t)
			if input != "" {
				t.Setenv("FSEND_LOG_LEVEL", input)
			}
			cfg := loadServerConfig()
			if cfg.logLevel != want {
				t.Errorf("FSEND_LOG_LEVEL=%q: got %v, want %v", input, cfg.logLevel, want)
			}
		})
	}
}

func TestServerHealthCheckUnreachable(t *testing.T) {
	// Bind+close to take a known-free port, then probe it (nothing listens).
	port := srvFreePortTCP(t)
	t.Setenv("FSEND_HTTP_ADDR", fmt.Sprintf("127.0.0.1:%d", port))
	if err := healthCheck(); err == nil {
		t.Fatal("expected error against closed port")
	}
}

func TestServerHealthCheckBadStatus(t *testing.T) {
	addr := startStubHealth(t, http.StatusInternalServerError, nil)
	t.Setenv("FSEND_HTTP_ADDR", addr)
	err := healthCheck()
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestServerHealthCheckOK(t *testing.T) {
	addr := startStubHealth(t, http.StatusOK, []byte(`{"ok":true}`))
	t.Setenv("FSEND_HTTP_ADDR", addr)
	if err := healthCheck(); err != nil {
		t.Errorf("healthCheck: %v", err)
	}
}

// startStubHealth boots a minimal HTTP server that answers /v1/health with
// the given status + body, and shuts it down when the test ends. Returns
// the host:port the test should set as FSEND_HTTP_ADDR.
func startStubHealth(t *testing.T, status int, body []byte) string {
	t.Helper()
	port := srvFreePortTCP(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	done := make(chan struct{})
	go func() {
		_ = srv.ListenAndServe()
		close(done)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	})
	srvWaitForPort(t, addr, 2*time.Second)
	return addr
}

func srvWaitForPort(t *testing.T, addr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %s did not accept connections within %v", addr, within)
}
