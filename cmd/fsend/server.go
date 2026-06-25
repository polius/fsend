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
	"sync"
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

	sht := boldHelpHeaders(serverHelpTemplate)
	c.SetHelpTemplate(sht)
	c.SetUsageTemplate(sht)

	return c
}

// runServer wires up the HTTP signaling listener and UDP relay listener
// from environment-driven config, traps SIGINT/SIGTERM, and blocks until
// shutdown.
func runServer() error {
	cfg, err := loadServerConfig()
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrServerStartup, err)
	}
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
		DisableForwarding:  !cfg.enableRelay,
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
	if !cfg.enableRelay {
		logger.Info("relay forwarding disabled (FSEND_ENABLE_RELAY=false); STUN/hole-punching still active")
	}

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

	// Graceful shutdown: stop new relay allocations, then let in-flight HTTP
	// (including 25s /wait long-polls) and in-flight relay transfers finish
	// before tearing down. Both are bounded by shutdownGrace so a wedged
	// session can't block shutdown forever. For the drain to complete under a
	// container runtime, set stop_grace_period above shutdownGrace; otherwise
	// the default (Docker: 10s) cuts it short.
	relaySrv.Drain()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer shutdownCancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = httpSrv.Shutdown(shutdownCtx) }()
	go func() { defer wg.Done(); drainRelay(shutdownCtx, relaySrv, logger) }()
	wg.Wait()
	cancel()
	return nil
}

// shutdownGrace bounds graceful shutdown. Set above the 25s /wait long-poll so
// in-flight polls return naturally rather than being severed; the deployment's
// stop_grace_period must exceed it.
const shutdownGrace = 30 * time.Second

// drainRelay blocks until no relay transfer has moved bytes recently (so the
// socket can close without cutting an in-flight transfer) or ctx expires. It
// returns immediately when the relay is idle, so a quiet server still restarts
// fast.
func drainRelay(ctx context.Context, r *relay.Server, logger *slog.Logger) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		if n := r.ActiveAllocations(); n == 0 {
			return
		} else if ctx.Err() != nil {
			logger.Warn("relay drain deadline reached; dropping active sessions", "active", n)
			return
		}
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
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
	enableRelay          bool
}

func loadServerConfig() (serverRuntimeConfig, error) {
	cfg := serverRuntimeConfig{
		httpAddr:       envOr("FSEND_HTTP_ADDR", ":8080"),
		udpAddr:        envOr("FSEND_UDP_ADDR", ":443"),
		serverPassword: os.Getenv("FSEND_SERVER_PASSWORD"),
	}
	var err error
	if cfg.maxSessionsPerIP, err = envInt("FSEND_MAX_SESSIONS_PER_IP", 5); err != nil {
		return cfg, err
	}
	if cfg.maxNewSessionsPerMin, err = envInt("FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN", 30); err != nil {
		return cfg, err
	}
	if cfg.maxBytesPerSession, err = envBytes("FSEND_MAX_RELAY_BYTES_PER_SESSION", 100*1000*1000); err != nil {
		return cfg, err
	}
	if cfg.enableRelay, err = envBool("FSEND_ENABLE_RELAY", true); err != nil {
		return cfg, err
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
	return cfg, nil
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

// envBool reads a boolean (strconv.ParseBool: 1/t/true, 0/f/false…) from
// name. Unset/blank → def; anything unparseable is an error rather than a
// silent default, so a typo'd toggle fails loudly instead of misbehaving.
func envBool(name string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q: not a boolean (use true/false)", name, v)
	}
	return b, nil
}

func envInt(name string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q: not a non-negative integer", name, v)
	}
	return n, nil
}

// envBytes reads a byte count with optional decimal unit suffix
// ("500MB", "1.5gb", "104857600") from name. Suffixes are
// case-insensitive and decimal (KB = 1000) to match the units fsend
// displays everywhere; binary forms ("MiB", "1ki") are rejected rather
// than guessed at. Unset/blank → def; anything else unparseable is an
// error — silently running with the default would hide an operator's
// typo'd cap.
func envBytes(name string, def uint64) (uint64, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	i := 0
	for i < len(v) && (v[i] == '.' || (v[i] >= '0' && v[i] <= '9')) {
		i++
	}
	num, suf := v[:i], strings.ToLower(strings.TrimSpace(v[i:]))
	var mul float64
	switch suf {
	case "", "b":
		mul = 1
	case "kb":
		mul = 1000
	case "mb":
		mul = 1000 * 1000
	case "gb":
		mul = 1000 * 1000 * 1000
	case "tb":
		mul = 1000 * 1000 * 1000 * 1000
	default:
		return 0, fmt.Errorf("%s=%q: unknown unit %q (use B, KB, MB, GB, TB, or a plain byte count)", name, v, suf)
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q: not a byte count (use B, KB, MB, GB, TB, or a plain byte count)", name, v)
	}
	return uint64(n * mul), nil
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
  pairing channel carries session slots, bearer tokens, and the
  FSEND_SERVER_PASSWORD header in plaintext.
  See deploy/compose/docker-compose.yml for a Caddy + Let's Encrypt setup.

CONFIGURATION (environment variables — all optional)
  FSEND_HTTP_ADDR                       Default :8080
  FSEND_UDP_ADDR                        Default :443
  FSEND_LOG_LEVEL                       Default info
  FSEND_MAX_SESSIONS_PER_IP             Default 5
  FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN Default 30
  FSEND_MAX_RELAY_BYTES_PER_SESSION     Default 100MB, counted in wire bytes
                                        after compression (accepts B, KB, MB,
                                        GB, TB, or a plain byte count, e.g.
                                        "100MB", "500KB", "104857600")
  FSEND_ENABLE_RELAY                    Default true. Set false for pairing +
                                        STUN only: peers still hole-punch, but
                                        the server carries no data (symmetric-NAT
                                        transfers fail instead of relaying)
  FSEND_SERVER_PASSWORD                 Optional shared secret. When set, every
                                        endpoint except /v1/health requires the
                                        X-Fsend-Auth header to match. Clients:
                                        fsend --connect <host:port>,<password>

LEARN MORE
  https://github.com/polius/fsend
`
