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

const srvMB = 1000 * 1000

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
	if got, err := envInt(k, 42); err != nil || got != 42 {
		t.Errorf("unset: got %d, %v; want 42", got, err)
	}
	t.Setenv(k, "7")
	if got, err := envInt(k, 42); err != nil || got != 7 {
		t.Errorf("set: got %d, %v; want 7", got, err)
	}
	for _, bad := range []string{"not-a-number", "-1", "1.5"} {
		t.Setenv(k, bad)
		if _, err := envInt(k, 42); err == nil {
			t.Errorf("%q: want error, got nil", bad)
		}
	}
}

func TestServerEnvBool(t *testing.T) {
	const k = "FSEND_TEST_ENV_BOOL"
	os.Unsetenv(k)
	if got, err := envBool(k, true); err != nil || !got {
		t.Errorf("unset: got %v, %v; want true", got, err)
	}
	t.Setenv(k, "false")
	if got, err := envBool(k, true); err != nil || got {
		t.Errorf("set false: got %v, %v; want false", got, err)
	}
	t.Setenv(k, "maybe")
	if _, err := envBool(k, true); err == nil {
		t.Error("typo: want error, got nil")
	}
}

func TestServerEnvBytes(t *testing.T) {
	const k = "FSEND_TEST_ENV_BYTES"
	const def uint64 = 100
	cases := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{in: "", want: def},
		{in: "500", want: 500},
		{in: "500b", want: 500},
		{in: "500B", want: 500},
		{in: "1kb", want: 1000},
		{in: "1KB", want: 1000},
		{in: "1MB", want: 1000 * 1000},
		{in: "100mb", want: 100 * srvMB},
		{in: "1GB", want: 1000 * 1000 * 1000},
		{in: "1TB", want: 1000 * 1000 * 1000 * 1000},
		{in: "  10MB  ", want: 10 * srvMB},
		{in: "1.5MB", want: uint64(1.5 * float64(srvMB))},
		{in: "0", want: 0},
		// Binary and single-letter suffixes are rejected, not guessed at:
		// fsend displays decimal units everywhere, and a silent fallback
		// would hide the misconfiguration.
		{in: "1KiB", wantErr: true},
		{in: "100MiB", wantErr: true},
		{in: "1GiB", wantErr: true},
		{in: "500m", wantErr: true},
		{in: "1k", wantErr: true},
		{in: "1g", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "not-a-number", wantErr: true},
		{in: "100ZZ", wantErr: true},
		{in: "M", wantErr: true},
		{in: "MB", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if c.in == "" {
				os.Unsetenv(k)
			} else {
				t.Setenv(k, c.in)
			}
			got, err := envBytes(k, def)
			if c.wantErr {
				if err == nil {
					t.Fatalf("envBytes(%q): want error, got %d", c.in, got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("envBytes(%q): got %d, %v; want %d", c.in, got, err, c.want)
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
		"FSEND_LOG_LEVEL",
		"FSEND_MAX_SESSIONS_PER_IP",
		"FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN",
		"FSEND_MAX_RELAY_BYTES_PER_SESSION",
	} {
		os.Unsetenv(k)
	}
}

func TestServerLoadConfigDefaults(t *testing.T) {
	clearServerEnv(t)
	cfg, err := loadServerConfig()
	if err != nil {
		t.Fatalf("loadServerConfig: %v", err)
	}
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
	if cfg.maxBytesPerSession != 100*srvMB {
		t.Errorf("maxBytesPerSession: got %d, want %d", cfg.maxBytesPerSession, 100*srvMB)
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
	t.Setenv("FSEND_MAX_RELAY_BYTES_PER_SESSION", "250MB")
	cfg, err := loadServerConfig()
	if err != nil {
		t.Fatalf("loadServerConfig: %v", err)
	}
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
	if cfg.maxBytesPerSession != 250*srvMB {
		t.Errorf("maxBytesPerSession: got %d", cfg.maxBytesPerSession)
	}
}

func TestServerLoadConfigRejectsBadValues(t *testing.T) {
	// A typo'd limit must stop the server, not silently run the default.
	cases := map[string]string{
		"FSEND_MAX_RELAY_BYTES_PER_SESSION":     "1GiB",
		"FSEND_MAX_SESSIONS_PER_IP":             "many",
		"FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN": "-1",
	}
	for k, v := range cases {
		t.Run(k+"="+v, func(t *testing.T) {
			clearServerEnv(t)
			t.Setenv(k, v)
			if _, err := loadServerConfig(); err == nil {
				t.Fatalf("%s=%q: want error, got nil", k, v)
			}
		})
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
			cfg, err := loadServerConfig()
			if err != nil {
				t.Fatalf("loadServerConfig: %v", err)
			}
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
