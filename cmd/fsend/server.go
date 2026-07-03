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
	"github.com/polius/fsend/internal/uxlog"
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
				// An unreachable server is the routine result a probe exists
				// to detect, not an fsend bug: one line, exit 1 as documented
				// — never the E099 "file an issue" catchall (exit 99).
				if err := healthCheck(); err != nil {
					fmt.Fprintln(os.Stderr, uxlog.Cross(), err)
					os.Exit(1)
				}
				return nil
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

	// Print the effective config before the structured logs so an operator
	// can confirm their FSEND_* overrides took effect.
	_, _ = fmt.Fprint(os.Stdout, formatServerConfig(cfg))

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
		MaxBytesPerDay:     cfg.maxBytesPerDay,
		Logger:             logger,
		DisableForwarding:  !cfg.enableRelay,
	})
	udpPort, err := udpPortFromAddr(cfg.udpAddr)
	if err != nil {
		return fmt.Errorf("%w: FSEND_RELAY_ADDR %q: %v", fserrors.ErrServerStartup, cfg.udpAddr, err)
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
		logger.Info("relay forwarding disabled (FSEND_RELAY_ENABLED=false); STUN/hole-punching still active")
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
	// Marks a clean drain: absent from the logs when SIGKILL (e.g. a too-low
	// stop_grace_period) cut the shutdown short.
	logger.Info("shutdown complete")
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

// Server env-var names and their built-in defaults. Shared by the config
// loader and the startup summary so the two can't drift on a rename or a
// changed default.
const (
	envServerAddr           = "FSEND_SERVER_ADDR"
	envRelayAddr            = "FSEND_RELAY_ADDR"
	envMaxSessionsPerIP     = "FSEND_SERVER_MAX_SESSIONS_PER_IP"
	envMaxNewSessionsPerMin = "FSEND_SERVER_MAX_SESSIONS_PER_IP_PER_MINUTE"
	envMaxBytesPerSession   = "FSEND_RELAY_MAX_BYTES_PER_SESSION"
	envMaxBytesPerDay       = "FSEND_RELAY_MAX_BYTES_PER_DAY"
	envRelayEnabled         = "FSEND_RELAY_ENABLED"
	envLogLevel             = "FSEND_LOG_LEVEL"

	defaultServerAddr = ":8080"
	defaultRelayAddr  = ":443"
	// The caps default to 0 = unlimited: an unconfigured server imposes no
	// per-IP session limits and no relay byte caps. Operators opt into
	// protection by setting a positive value.
	defaultMaxSessionsPerIP     = 0
	defaultMaxNewSessionsPerMin = 0
	defaultMaxBytesPerSession   = 0
	defaultMaxBytesPerDay       = 0
	defaultRelayEnabled         = true
)

// serverRuntimeConfig is the parsed environment for `fsend server`.
type serverRuntimeConfig struct {
	httpAddr             string
	udpAddr              string
	logLevel             slog.Level
	maxSessionsPerIP     int
	maxNewSessionsPerMin int
	maxBytesPerSession   uint64
	maxBytesPerDay       uint64
	serverPassword       string
	enableRelay          bool
}

func loadServerConfig() (serverRuntimeConfig, error) {
	cfg := serverRuntimeConfig{
		httpAddr:       envOr(envServerAddr, defaultServerAddr),
		udpAddr:        envOr(envRelayAddr, defaultRelayAddr),
		serverPassword: os.Getenv("FSEND_SERVER_PASSWORD"),
	}
	var err error
	if cfg.maxSessionsPerIP, err = envInt(envMaxSessionsPerIP, defaultMaxSessionsPerIP); err != nil {
		return cfg, err
	}
	if cfg.maxNewSessionsPerMin, err = envInt(envMaxNewSessionsPerMin, defaultMaxNewSessionsPerMin); err != nil {
		return cfg, err
	}
	if cfg.maxBytesPerSession, err = envBytes(envMaxBytesPerSession, defaultMaxBytesPerSession); err != nil {
		return cfg, err
	}
	if cfg.maxBytesPerDay, err = envBytes(envMaxBytesPerDay, defaultMaxBytesPerDay); err != nil {
		return cfg, err
	}
	if cfg.enableRelay, err = envBool(envRelayEnabled, defaultRelayEnabled); err != nil {
		return cfg, err
	}
	// A typo'd level fails loudly like every other FSEND_* var — silently
	// running at info would hide the debug logs the operator asked for.
	switch v := strings.TrimSpace(os.Getenv(envLogLevel)); strings.ToLower(v) {
	case "", "info":
		cfg.logLevel = slog.LevelInfo
	case "debug":
		cfg.logLevel = slog.LevelDebug
	case "warn":
		cfg.logLevel = slog.LevelWarn
	case "error":
		cfg.logLevel = slog.LevelError
	default:
		return cfg, fmt.Errorf("%s=%q: not a log level (use debug, info, warn, or error)", envLogLevel, v)
	}
	return cfg, nil
}

// formatServerConfig renders the effective server configuration as a
// human-readable block, flagging each setting whose value differs from its
// built-in default with a leading "*". Printed once at startup so an
// operator can confirm at a glance that their FSEND_* overrides were
// picked up. The password is never printed — only whether one is set.
func formatServerConfig(cfg serverRuntimeConfig) string {
	type setting struct {
		name     string
		value    string
		modified bool
	}
	password := "(not set)"
	if cfg.serverPassword != "" {
		password = "(set)"
	}
	settings := []setting{
		// Grouped: server-wide (log, password), then the pairing control
		// plane (its listener + session limits), then the relay data plane
		// (its listener + byte limits). Matches the docs table order.
		{envLogLevel, logLevelName(cfg.logLevel), cfg.logLevel != slog.LevelInfo},
		// Env-var name inlined (not a shared const) so gosec G101 doesn't
		// mistake a password-named identifier for a hardcoded credential.
		{"FSEND_SERVER_PASSWORD", password, cfg.serverPassword != ""},
		{envServerAddr, cfg.httpAddr, cfg.httpAddr != defaultServerAddr},
		{envMaxSessionsPerIP, capCount(cfg.maxSessionsPerIP), cfg.maxSessionsPerIP != defaultMaxSessionsPerIP},
		{envMaxNewSessionsPerMin, capCount(cfg.maxNewSessionsPerMin), cfg.maxNewSessionsPerMin != defaultMaxNewSessionsPerMin},
		{envRelayEnabled, strconv.FormatBool(cfg.enableRelay), !cfg.enableRelay},
		{envRelayAddr, cfg.udpAddr, cfg.udpAddr != defaultRelayAddr},
		{envMaxBytesPerSession, capBytes(cfg.maxBytesPerSession), cfg.maxBytesPerSession != defaultMaxBytesPerSession},
		{envMaxBytesPerDay, capBytes(cfg.maxBytesPerDay), cfg.maxBytesPerDay != defaultMaxBytesPerDay},
	}

	width, modified := 0, 0
	for _, s := range settings {
		if len(s.name) > width {
			width = len(s.name)
		}
		if s.modified {
			modified++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "fsend server configuration (%d of %d customized; * = changed from default):\n",
		modified, len(settings))
	for _, s := range settings {
		mark := " "
		if s.modified {
			mark = "*"
		}
		fmt.Fprintf(&b, "  %s %-*s  %s\n", mark, width, s.name, s.value)
	}
	return b.String()
}

// capCount renders a session cap, showing 0 as "0 (unlimited)".
func capCount(n int) string {
	if n == 0 {
		return "0 (unlimited)"
	}
	return strconv.Itoa(n)
}

// capBytes renders the per-session byte cap, showing 0 as "0 (unlimited)".
func capBytes(n uint64) string {
	if n == 0 {
		return "0 (unlimited)"
	}
	return uxlog.HumanBytes(int64(n))
}

// logLevelName maps the parsed level back to the lowercase form operators
// set in FSEND_LOG_LEVEL (slog's own String() is uppercase).
func logLevelName(l slog.Level) string {
	switch l {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	default:
		return "info"
	}
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
// HTTP address. Returns an error when unhealthy; the caller reports it
// and exits 1 (the Docker HEALTHCHECK contract).
func healthCheck() error {
	addr := envOr(envServerAddr, defaultServerAddr)
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

  Server-wide:
    FSEND_LOG_LEVEL                   Default info (debug/info/warn/error).
    FSEND_SERVER_PASSWORD             Optional shared secret. When set, all
                                      endpoints except /v1/health require the
                                      X-Fsend-Auth header. Connect with
                                      fsend --connect <host:port>,<password>.

  Pairing (TCP signaling/control plane):
    FSEND_SERVER_ADDR                 Default :8080 (TCP).
    FSEND_SERVER_MAX_SESSIONS_PER_IP  How many sessions one IP may have
                                      alive at once (concurrency cap; gates
                                      relay access too). Default 0 =
                                      unlimited; set a positive value to cap.
    FSEND_SERVER_MAX_SESSIONS_PER_IP_PER_MINUTE
                                      How many new sessions one IP may create
                                      per minute (rate cap). Default 0 =
                                      unlimited; set a positive value to cap.

  Relay (UDP data plane — also answers STUN):
    FSEND_RELAY_ENABLED               Default true. false = pairing + STUN
                                      only: peers still hole-punch, but the
                                      server carries no file data.
    FSEND_RELAY_ADDR                  Default :443 (UDP). Also the STUN
                                      endpoint, so it stays in use even
                                      when forwarding is disabled.
    FSEND_RELAY_MAX_BYTES_PER_SESSION Per-session wire bytes after
                                      compression. Accepts B, KB, MB, GB,
                                      TB, or a plain byte count. Default 0 =
                                      unlimited; set a value to cap.
    FSEND_RELAY_MAX_BYTES_PER_DAY     Server-wide outbound bytes forwarded
                                      per UTC day — the egress/cost ceiling.
                                      Each byte counts once (a 1 MB relayed
                                      file ≈ 1 MB). Same units. Default 0 =
                                      unlimited; once hit, the relay stops
                                      forwarding until 00:00 UTC.

LEARN MORE
  https://github.com/polius/fsend
`
