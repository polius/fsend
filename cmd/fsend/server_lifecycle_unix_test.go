//go:build unix

package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestServerLifecycle boots the real server in-process, verifies
// /v1/health responds 200 (both directly and via healthCheck()), then
// triggers a graceful SIGTERM shutdown and waits for runServer() to
// return.
//
// SIGTERM is also trapped by this test so the Go runtime's default
// "die on SIGTERM" action is canceled — runServer()'s signal.Notify
// still wins.
//
// Unix-only because Windows has no SIGTERM and syscall.Kill is undefined
// there; runServer's graceful-shutdown path on Windows is covered
// implicitly by the E2E suite.
func TestServerLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a real server on loopback; skipped in -short")
	}

	clearServerEnv(t)
	httpPort := srvFreePortTCP(t)
	udpPort := srvFreePortUDP(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
	udpAddr := fmt.Sprintf("127.0.0.1:%d", udpPort)
	t.Setenv("FSEND_HTTP_ADDR", httpAddr)
	t.Setenv("FSEND_UDP_ADDR", udpAddr)
	t.Setenv("FSEND_LOG_LEVEL", "error")

	sink := make(chan os.Signal, 1)
	signal.Notify(sink, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(sink) })

	done := make(chan error, 1)
	go func() { done <- runServer() }()

	srvWaitForPort(t, httpAddr, 5*time.Second)

	resp, err := http.Get("http://" + httpAddr + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/health: status %d", resp.StatusCode)
	}

	if err := healthCheck(); err != nil {
		t.Fatalf("healthCheck(): %v", err)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill self: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServer did not return within 10s of SIGTERM")
	}
}
