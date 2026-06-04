// Command fsend-server is the rendezvous + relay-fallback service that
// fsend CLIs use when peers are not on the same LAN.
//
// Configuration is entirely env-var driven (see docs/decisions/
// implementation-defaults.md and PROJECT_SPEC.md "Server operation").
// There are no operational flags — just --version, --help, --health-check.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"net"

	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fsend-server:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	for _, a := range args {
		switch a {
		case "--version", "-v":
			fmt.Println(version.String())
			return nil
		case "--help", "-h":
			printHelp()
			return nil
		case "--health-check":
			// Used as a Docker HEALTHCHECK probe — see deploy/compose/.
			return healthCheck()
		default:
			return fmt.Errorf("unknown argument: %s (use --help)", a)
		}
	}

	cfg := loadConfig()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))
	slog.SetDefault(logger)

	s := server.New(server.Config{
		ServerVersion:        version.Version,
		MaxSessionsPerIP:     cfg.maxSessionsPerIP,
		MaxNewSessionsPerMin: cfg.maxNewSessionsPerMin,
		Logger:               logger,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.StartJanitor(ctx)

	// Bring up the UDP relay listener on cfg.udpAddr and wire it into
	// the signaling layer so /v1/relay/allocate works.
	udpListener, err := net.ListenPacket("udp", cfg.udpAddr)
	if err != nil {
		return fmt.Errorf("relay UDP listen on %s: %w", cfg.udpAddr, err)
	}
	defer udpListener.Close()
	relaySrv := relay.NewServer(udpListener, relay.ServerConfig{
		MaxBytesPerSession: cfg.maxBytesPerSession,
		SessionIdleTimeout: time.Duration(cfg.sessionIdleSecs) * time.Second,
		Logger:             logger,
	})
	// External address: the operator should set FSEND_PUBLIC_ADDR to the
	// host:port clients dial. Default to udpAddr (only sensible on dev).
	publicAddr := envOr("FSEND_PUBLIC_ADDR", cfg.udpAddr)
	s.WithRelay(relaySrv, publicAddr)
	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()
	go func() {
		if err := relaySrv.Run(relayCtx); err != nil {
			logger.Error("relay loop ended", "err", err)
		}
	}()
	logger.Info("relay UDP listener up", "addr", cfg.udpAddr, "public", publicAddr)

	httpSrv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Trap SIGINT/SIGTERM for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("fsend-server starting", "http", cfg.httpAddr, "version", version.Version)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-sigCh:
		logger.Info("shutdown requested")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	cancel()
	return nil
}

// runtimeConfig is the parsed environment.
type runtimeConfig struct {
	httpAddr             string
	udpAddr              string
	logLevel             slog.Level
	maxSessionsPerIP     int
	maxNewSessionsPerMin int
	maxBytesPerSession   uint64
	sessionIdleSecs      int
}

func loadConfig() runtimeConfig {
	cfg := runtimeConfig{
		httpAddr:             envOr("FSEND_HTTP_ADDR", ":8080"),
		udpAddr:              envOr("FSEND_UDP_ADDR", ":443"),
		maxSessionsPerIP:     envInt("FSEND_MAX_SESSIONS_PER_IP", 5),
		maxNewSessionsPerMin: envInt("FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN", 30),
		maxBytesPerSession:   uint64(envInt("FSEND_MAX_RELAY_BYTES_PER_SESSION", 100*1024*1024)),
		sessionIdleSecs:      envInt("FSEND_SESSION_IDLE_TIMEOUT_SECS", 60),
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

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: status %d", resp.StatusCode)
	}
	return nil
}

func printHelp() {
	fmt.Print(`fsend-server — rendezvous + relay server for fsend

USAGE
  fsend-server                 Run the server (config via env vars)

EXAMPLE
  Standard Docker run (zero-config):
    docker run -p 443:443/udp -p 8080:8080/tcp ghcr.io/polius/fsend-server

CONFIGURATION (environment variables — all optional)
  FSEND_HTTP_ADDR                       Default :8080
  FSEND_UDP_ADDR                        Default :443
  FSEND_LOG_LEVEL                       Default info
  FSEND_MAX_SESSIONS_PER_IP             Default 5
  FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN Default 30

FLAGS
  --help          Show this help
  --version       Show version
  --health-check  Probe /v1/health and exit 0 if healthy (for Docker)

LEARN MORE
  https://github.com/polius/fsend
`)
}
