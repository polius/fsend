package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/version"
)

// serverCmd is the pairing + relay-fallback service that fsend
// clients use when peers are not on the same LAN. Invoked as
// `fsend server`.
//
// Configuration is entirely env-var driven (see internal/server for the
// supported variables and their defaults). The only flag is
// --health-check, used by Docker HEALTHCHECK.
func serverCmd() *cobra.Command {
	var healthCheckFlag bool

	c := &cobra.Command{
		Use:   "server",
		Short: "Run the fsend pairing + relay server",
		// Map a stray positional to E024 rather than cobra's default error,
		// which would fall through to the E099 "file an issue" catchall.
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("%w: server takes no positional arguments (got %q)", fserrors.ErrUsage, args[0])
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if healthCheckFlag {
				// Used as a Docker HEALTHCHECK probe — see deploy/compose/.
				return healthCheck()
			}
			return runServer()
		},
	}

	c.Flags().BoolVar(&healthCheckFlag, "health-check", false,
		"probe /v1/health and exit 0 if healthy (for Docker)")

	c.SetHelpTemplate(serverHelpTemplate)
	c.SetUsageTemplate(serverHelpTemplate)

	return c
}

// runServer wires up the HTTP signaling listener and UDP relay listener
// from environment-driven config, traps SIGINT/SIGTERM, and blocks until
// shutdown.
func runServer() error {
	cfg := loadServerConfig()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))
	slog.SetDefault(logger)

	s := server.New(server.Config{
		ServerVersion:        version.Version,
		MaxSessionsPerIP:     cfg.maxSessionsPerIP,
		MaxNewSessionsPerMin: cfg.maxNewSessionsPerMin,
		Logger:               logger,
		ServerPassword:       cfg.serverPassword,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.StartJanitor(ctx)

	udpListener, err := net.ListenPacket("udp", cfg.udpAddr)
	if err != nil {
		return fmt.Errorf("%w: relay UDP listen on %s: %v", fserrors.ErrServerStartup, cfg.udpAddr, err)
	}
	defer func() { _ = udpListener.Close() }()
	relaySrv := relay.NewServer(udpListener, relay.ServerConfig{
		MaxBytesPerSession: cfg.maxBytesPerSession,
		Logger:             logger,
	})
	udpPort, err := udpPortFromAddr(cfg.udpAddr)
	if err != nil {
		return fmt.Errorf("%w: FSEND_UDP_ADDR %q: %v", fserrors.ErrServerStartup, cfg.udpAddr, err)
	}
	s.WithRelay(relaySrv, udpPort)
	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()
	go func() {
		if err := relaySrv.Run(relayCtx); err != nil {
			logger.Error("relay loop ended", "err", err)
		}
	}()
	logger.Info("relay UDP listener up", "addr", cfg.udpAddr, "udp_port", udpPort)

	httpSrv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// ReadTimeout also caps handler body reads — wanted: bodies are
		// ≤16 KiB, and without it a client trickling a declared body one
		// byte at a time parks a goroutine indefinitely (the rate limiter
		// only runs after decode). It does not affect response writes, so
		// /wait long-polls (25s before first byte) are untouched —
		// WriteTimeout stays zero for them, and IdleTimeout has to clear
		// the same bar comfortably.
		ReadTimeout:    15 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("fsend server starting", "http", cfg.httpAddr, "version", version.Version)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-sigCh:
		logger.Info("shutdown requested")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("%w: http listen on %s: %v", fserrors.ErrServerStartup, cfg.httpAddr, err)
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	cancel()
	return nil
}

// serverRuntimeConfig is the parsed environment for `fsend server`.
type serverRuntimeConfig struct {
	httpAddr             string
	udpAddr              string
	logLevel             slog.Level
	maxSessionsPerIP     int
	maxNewSessionsPerMin int
	maxBytesPerSession   uint64
	serverPassword       string
}

func loadServerConfig() serverRuntimeConfig {
	cfg := serverRuntimeConfig{
		httpAddr:             envOr("FSEND_HTTP_ADDR", ":8080"),
		udpAddr:              envOr("FSEND_UDP_ADDR", ":443"),
		maxSessionsPerIP:     envInt("FSEND_MAX_SESSIONS_PER_IP", 5),
		maxNewSessionsPerMin: envInt("FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN", 30),
		maxBytesPerSession:   envBytes("FSEND_MAX_RELAY_BYTES_PER_SESSION", 100*1024*1024),
		serverPassword:       os.Getenv("FSEND_SERVER_PASSWORD"),
	}
	switch strings.ToLower(os.Getenv("FSEND_LOG_LEVEL")) {
	case "debug":
		cfg.logLevel = slog.LevelDebug
	case "warn":
		cfg.logLevel = slog.LevelWarn
	case "error":
		cfg.logLevel = slog.LevelError
	default:
		cfg.logLevel = slog.LevelInfo
	}
	return cfg
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// udpPortFromAddr extracts the port from a listen-style address like
// ":443" or "0.0.0.0:18443". The host half is irrelevant — we only
// care about the port the relay listens on.
func udpPortFromAddr(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("port out of range")
	}
	return p, nil
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envBytes reads a byte count with optional unit suffix ("100MiB", "50k",
// "1048576") from name, falling back to def on missing or unparseable
// input. Suffixes are case-insensitive; b/k/m/g/t are decimal (1000),
// kib/mib/gib/tib are binary (1024).
func envBytes(name string, def uint64) uint64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	i := 0
	for i < len(v) && (v[i] == '.' || (v[i] >= '0' && v[i] <= '9')) {
		i++
	}
	num, suf := v[:i], strings.ToLower(strings.TrimSpace(v[i:]))
	if num == "" {
		return def
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n < 0 {
		return def
	}
	var mul float64
	switch suf {
	case "", "b":
		mul = 1
	case "k", "kb":
		mul = 1000
	case "ki", "kib":
		mul = 1024
	case "m", "mb":
		mul = 1000 * 1000
	case "mi", "mib":
		mul = 1024 * 1024
	case "g", "gb":
		mul = 1000 * 1000 * 1000
	case "gi", "gib":
		mul = 1024 * 1024 * 1024
	case "t", "tb":
		mul = 1000 * 1000 * 1000 * 1000
	case "ti", "tib":
		mul = 1024 * 1024 * 1024 * 1024
	default:
		return def
	}
	return uint64(n * mul)
}

// healthCheck pings the local server's /v1/health on the configured
// HTTP address. Exits 0 on healthy, 1 on anything else. Designed for
// Docker HEALTHCHECK.
func healthCheck() error {
	addr := envOr("FSEND_HTTP_ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	resp, err := http.Get("http://" + addr + "/v1/health")
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: status %d", resp.StatusCode)
	}
	return nil
}

// serverHelpTemplate is the hand-written help for `fsend server`. Same
// shape as the root help: examples-first, env vars called out, no cobra
// auto-flag-wall.
const serverHelpTemplate = `fsend server — pairing + relay server for fsend

USAGE
  fsend server                 Run the server (config via env vars)
  fsend server --health-check  Probe /v1/health and exit 0 if healthy
  fsend server --help          Show this help

EXAMPLE
  Local-network test (no TLS — do not expose to the internet):
    docker run -p 443:443/udp -p 8080:8080/tcp poliuscorp/fsend server

  Internet-exposed: put a TLS-terminating reverse proxy in front of :8080.
  File data over UDP/443 is already end-to-end encrypted, but the HTTP
  pairing channel carries the share code, bearer tokens, and the
  FSEND_SERVER_PASSWORD header in plaintext.
  See deploy/compose/docker-compose.yml for a Caddy + Let's Encrypt setup.

CONFIGURATION (environment variables — all optional)
  FSEND_HTTP_ADDR                       Default :8080
  FSEND_UDP_ADDR                        Default :443
  FSEND_LOG_LEVEL                       Default info
  FSEND_MAX_SESSIONS_PER_IP             Default 5
  FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN Default 30
  FSEND_MAX_RELAY_BYTES_PER_SESSION     Default 100MiB (accepts e.g. "100MiB", "500m", "104857600")
  FSEND_SERVER_PASSWORD                 Optional shared secret. When set, every endpoint except
                                        /v1/health requires the X-Fsend-Auth header to match.
                                        Clients set theirs with: fsend --connect <host:port>,<password>.

LEARN MORE
  https://github.com/polius/fsend
`
