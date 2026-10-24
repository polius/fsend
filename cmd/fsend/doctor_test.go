package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/server"
)

// doctorServer runs a pairing server whose relay advertises udpPort.
func doctorServer(t *testing.T, udpPort func(real int) int, withWS bool) string {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rs := relay.NewServer(udp, relay.ServerConfig{Logger: logger})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rs.Run(ctx); close(done) }()
	var alloc server.RelayAllocator = rs
	if !withWS {
		alloc = udpOnlyRelay{rs}
	}
	srv := server.New(server.Config{Logger: logger}).WithRelay(alloc, udpPort(udp.LocalAddr().(*net.UDPAddr).Port))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		cancel()
		_ = udp.Close()
		<-done
	})
	return ts.URL
}

func TestRelayChecks(t *testing.T) {
	blackhole, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	real := func(p int) int { return p }
	blocked := func(int) int { return blackhole.LocalAddr().(*net.UDPAddr).Port }

	cases := []struct {
		name           string
		port           func(int) int
		withWS         bool
		udpOK, tcpOK   bool
		udpMsg, tcpMsg string
	}{
		{"healthy", real, true, true, true, "answered", "works"},
		{"udp_blocked", blocked, true, false, true, "slower TCP fallback", "works"},
		// No probe header: the UDP check falls back to the stock host:443.
		{"old_server", real, false, false, false, "127.0.0.1:443", "no TCP fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks := relayChecks(doctorServer(t, tc.port, tc.withWS), "")
			if len(checks) != 2 {
				t.Fatalf("got %d checks, want 2", len(checks))
			}
			udp, tcp := checks[0], checks[1]
			if udp.ok != tc.udpOK || !strings.Contains(udp.msg, tc.udpMsg) {
				t.Errorf("UDP = %v %q, want %v containing %q", udp.ok, udp.msg, tc.udpOK, tc.udpMsg)
			}
			if tcp.ok != tc.tcpOK || !strings.Contains(tcp.msg, tc.tcpMsg) {
				t.Errorf("TCP = %v %q, want %v containing %q", tcp.ok, tcp.msg, tc.tcpOK, tc.tcpMsg)
			}
		})
	}
}
