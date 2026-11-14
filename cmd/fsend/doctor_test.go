package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
			checks, _ := relayChecks(doctorServer(t, tc.port, tc.withWS), "")
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

// The clock row warns when this device is more than the tolerance off the
// server's Date header, and is absent when the server sends none.
func TestClockCheck(t *testing.T) {
	now := time.Now()
	h := func(t time.Time) http.Header {
		return http.Header{"Date": []string{t.UTC().Format(http.TimeFormat)}}
	}
	if c, ok := clockCheck(h(now.Add(time.Minute)), now); !ok || !c.ok {
		t.Errorf("a 1-minute skew: got %+v, %v; want a passing row", c, ok)
	}
	c, ok := clockCheck(h(now.Add(-3*time.Hour)), now)
	if !ok || c.ok || !strings.Contains(c.msg, "3 h ahead of the server's") {
		t.Errorf("a clock 3 h ahead: got %+v, %v; want a warning naming it", c, ok)
	}
	if _, ok := clockCheck(http.Header{}, now); ok {
		t.Error("a row without a Date header")
	}
}

func TestNATAndIPv6Checks(t *testing.T) {
	local := []net.IP{net.ParseIP("192.168.1.20"), net.ParseIP("fe80::1"), net.ParseIP("fd00::7"), net.ParseIP("198.51.100.4")}
	if c, ok := natCheck(&net.UDPAddr{IP: net.ParseIP("203.0.113.9")}, local); !ok || !c.ok || !strings.Contains(c.msg, "behind NAT, public address 203.0.113.9") {
		t.Errorf("NAT: %+v", c)
	}
	if c, ok := natCheck(&net.UDPAddr{IP: net.ParseIP("198.51.100.4")}, local); !ok || !strings.Contains(c.msg, "none") {
		t.Errorf("no NAT: %+v", c)
	}
	for _, mapped := range []*net.UDPAddr{nil, {IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("192.168.1.20")}} {
		if _, ok := natCheck(mapped, local); ok {
			t.Errorf("a NAT row for %v, which says nothing about the internet", mapped)
		}
	}
	if c := ipv6Check(local); !c.ok || !strings.Contains(c.msg, "IPv4 only") {
		t.Errorf("link-local and ULA only: %+v", c)
	}
	if c := ipv6Check(append(local, net.ParseIP("2001:db8::5"))); !strings.Contains(c.msg, "global address 2001:db8::5") {
		t.Errorf("global IPv6: %+v", c)
	}
}

// A loopback miss alone is not a failure — Linux's lo has no multicast
// flag by default and Windows never answers on loopback — and it doesn't
// hide the LAN tier, which decides whether other devices can be found.
func TestMDNSChecks_LoopbackMissAloneIsFine(t *testing.T) {
	miss := errors.New("no answer")
	ip := net.IPv4(192, 168, 1, 20)
	for _, c := range []struct {
		name          string
		loop, lan     error
		wantOK        []bool
		wantSubstring string
	}{
		{"both answer", nil, nil, []bool{true, true}, "answered on the LAN"},
		{"loopback misses", miss, nil, []bool{true}, "loopback has no multicast on this system"},
		{"LAN misses", nil, miss, []bool{true, false}, "falls back to the pairing server"},
		{"both miss", miss, miss, []bool{false}, "LAN discovery unavailable"},
	} {
		rows := mdnsChecks(c.loop, c.lan, 3*time.Millisecond, 4*time.Millisecond, ip)
		if len(rows) != len(c.wantOK) {
			t.Fatalf("%s: %d rows, want %d: %+v", c.name, len(rows), len(c.wantOK), rows)
		}
		for i, r := range rows {
			if r.ok != c.wantOK[i] {
				t.Errorf("%s: row %d ok=%v, want %v (%q)", c.name, i, r.ok, c.wantOK[i], r.msg)
			}
		}
		if last := rows[len(rows)-1]; !strings.Contains(last.msg, c.wantSubstring) {
			t.Errorf("%s: last row %q, want it to mention %q", c.name, last.msg, c.wantSubstring)
		}
	}
}
