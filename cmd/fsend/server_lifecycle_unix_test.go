//go:build unix

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/polius/fsend/internal/signaling"
)

// TestServerLifecycle boots the real server in-process, verifies
// /health responds 200 (both directly and via healthCheck()), then
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
	t.Setenv("FSEND_SERVER_ADDR", httpAddr)
	t.Setenv("FSEND_RELAY_ADDR", udpAddr)
	t.Setenv("FSEND_LOG_LEVEL", "error")

	sink := make(chan os.Signal, 1)
	signal.Notify(sink, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(sink) })

	done := make(chan error, 1)
	go func() { done <- runServer() }()

	srvWaitForPort(t, httpAddr, 5*time.Second)

	resp, err := http.Get("http://" + httpAddr + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health: status %d", resp.StatusCode)
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

// TestServerRestartKeepsPendingCode: with FSEND_STATE_FILE set, a code
// created before a SIGTERM restart is still waitable afterwards.
func TestServerRestartKeepsPendingCode(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a real server on loopback; skipped in -short")
	}
	clearServerEnv(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", srvFreePortTCP(t))
	t.Setenv("FSEND_SERVER_ADDR", httpAddr)
	t.Setenv("FSEND_RELAY_ADDR", fmt.Sprintf("127.0.0.1:%d", srvFreePortUDP(t)))
	t.Setenv("FSEND_LOG_LEVEL", "error")
	t.Setenv("FSEND_STATE_FILE", filepath.Join(t.TempDir(), "sessions.json"))

	sink := make(chan os.Signal, 1)
	signal.Notify(sink, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(sink) })

	boot := func() chan error {
		done := make(chan error, 1)
		go func() { done <- runServer() }()
		srvWaitForPort(t, httpAddr, 5*time.Second)
		return done
	}
	stop := func(done chan error) {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("runServer: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("runServer did not stop")
		}
	}

	ctx := context.Background()
	client := signaling.New("http://"+httpAddr, "test")
	done := boot()
	created, err := client.Create(ctx, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stop(done)

	done = boot()
	defer stop(done)
	if _, err := signaling.New("http://"+httpAddr, "test").Join(ctx, created.Code); err != nil {
		t.Fatalf("join after restart: %v (the pending code was lost)", err)
	}
	if resp, err := client.Wait(ctx, created.Code); err != nil || resp == nil {
		t.Fatalf("sender wait after restart: %v, %v", resp, err)
	}
}
