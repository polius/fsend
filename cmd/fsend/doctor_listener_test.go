package main

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	codepkg "github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/landisc"
)

// The self-test opens the real listener and completes both routes'
// handshakes against it. Loopback stands in for the LAN address here:
// tests stay on local addresses.
func TestProbeLANListener(t *testing.T) {
	p := probeLANListener(net.IPv4(127, 0, 0, 1))
	if p.listenErr != nil {
		t.Fatalf("listen: %v", p.listenErr)
	}
	if p.tcpErr != nil || p.quicErr != nil {
		t.Fatalf("tcp: %v, quic: %v", p.tcpErr, p.quicErr)
	}
	host, port, err := net.SplitHostPort(p.addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("dialled %q, want 127.0.0.1:<port>", p.addr)
	}
	if n, _ := strconv.Atoi(port); n < lanPortFirst || n > lanPortLast {
		t.Errorf("port %s is outside the listener's range %s", port, lanPorts)
	}
	c := listenerCheck(p)
	if !c.ok || c.note || !strings.Contains(c.msg, "over TCP in") || !strings.Contains(c.msg, "QUIC in") {
		t.Errorf("row: %+v", c)
	}
	// The listener is gone afterwards: its port binds again, for both.
	// The UDP socket is released once the closed QUIC connection has
	// drained, a moment after Close (see listenLANWithRetry).
	deadline := time.Now().Add(5 * time.Second)
	for {
		pc, err := net.ListenPacket("udp", ":"+port)
		if err == nil {
			_ = pc.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the UDP port is still held: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	l, err := net.Listen("tcp", ":"+port)
	if err != nil {
		t.Fatalf("the TCP port is still held: %v", err)
	}
	_ = l.Close()
}

// The range doctor names is the range senders derive their port from.
func TestLANPortRangeMatchesSenders(t *testing.T) {
	epoch := codepkg.Epoch(time.Now())
	for i := range 8 {
		if p := landisc.PortForCode("code-"+strconv.Itoa(i), epoch); p < lanPortFirst || p > lanPortLast {
			t.Fatalf("a sender's port %d is outside %s", p, lanPorts)
		}
	}
	if p := freeLANPort(); p < lanPortFirst || p > lanPortLast {
		t.Fatalf("the self-test's port %d is outside %s", p, lanPorts)
	}
}

func TestListenerCheck(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: errors.New("refused")}
	const addr = "192.168.1.20:50123"
	for _, tc := range []struct {
		name     string
		p        lanProbe
		ok       bool
		want     string
		detailed bool
	}{
		{"both routes", lanProbe{addr: addr, tcpTook: 4 * time.Millisecond, quicTook: 1200 * time.Millisecond}, true,
			"answered at 192.168.1.20:50123 over TCP in 4 ms and QUIC in 1.2 s", true},
		{"no TCP", lanProbe{addr: addr, tcpErr: refused, quicTook: 9 * time.Millisecond}, false,
			"over QUIC in 9 ms but not over TCP (connection failed) — same-network transfers take the slower QUIC route", false},
		{"no QUIC", lanProbe{addr: addr, quicErr: context.DeadlineExceeded, tcpTook: 3 * time.Millisecond}, false,
			"over TCP in 3 ms but not over QUIC (timed out)", false},
		{"neither", lanProbe{addr: addr, tcpErr: refused, quicErr: context.DeadlineExceeded}, false,
			"did not answer at 192.168.1.20:50123, even from this machine (TCP: connection failed, QUIC: timed out)", false},
		{"no listener", lanProbe{addr: addr, listenErr: errors.New("bind: address already in use")}, false,
			"cannot open the local-network listener (the port could not be bound) — transfers to devices on this network go through the server", false},
	} {
		c := listenerCheck(tc.p)
		if c.ok != tc.ok || c.note || c.label != "listener" || !strings.Contains(c.msg, tc.want) {
			t.Errorf("%s: got ok=%v %q, want ok=%v containing %q", tc.name, c.ok, c.msg, tc.ok, tc.want)
		}
		// Only a pass needs the reminder of what a self-test can't show;
		// a failure is a finding as it stands.
		if got := len(c.detail) > 0; got != tc.detailed {
			t.Errorf("%s: detail %q", tc.name, detailText(c))
		}
		if tc.detailed && detailText(c) != listenerScope(runtime.GOOS) {
			t.Errorf("%s: the pass doesn't say what it can't prove: %q", tc.name, detailText(c))
		}
	}
	// What a pass leaves open: a firewall, except on macOS, whose
	// application firewall screens the self-test's connection too.
	for goos, want := range map[string]string{
		"linux":   "the connection never left it, so this shows the listener works, not that other devices get past a firewall",
		"windows": "not that other devices get past a firewall",
		"darwin":  "the connection never left it, so this shows the listener works, not that the network lets other devices reach it",
	} {
		if got := listenerScope(goos); !strings.Contains(got, want) {
			t.Errorf("%s: scope %q", goos, got)
		}
	}
}
