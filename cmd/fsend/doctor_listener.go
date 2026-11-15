package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"runtime"
	"strconv"
	"time"

	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/uxlog"
)

// The listener row of `fsend doctor`: a self-test of the local-network
// listener, the socket a receiver on the same network connects to.
//
// What it proves: this machine can bind the listener the way a send does
// (every interface, UDP for QUIC and TCP for the TCP route, a port of the
// 50000-50999 range), the LAN address routes to it, the peer filter admits
// that address, and both routes complete the whole handshake — TLS 1.3
// with the route's ALPN, then the PAKE.
//
// What it cannot prove: that another device gets in. The kernel delivers
// traffic to one of its own addresses internally, without putting it on
// the network, so it crosses neither the switch or access point between
// two devices nor, on Linux, the firewall: ufw and firewalld accept the
// loopback interface outright (checked in Docker: the self-test passes on
// a machine whose ufw or firewalld turns every other container away). The
// macOS application firewall is the exception — it screens these
// connections too, see doctor_firewall_macos.go — and its row uses that.
// No test run from one machine alone, without privileges, reaches the
// path another device's packets take: the firewall row reads the rules
// instead, and only a transfer from a second device settles it.

// lanProbe is what the listener self-test found.
type lanProbe struct {
	addr              string // the LAN address and port that were dialled
	listenErr         error  // the listener could not be opened
	tcpErr, quicErr   error  // each route's connect and handshake
	tcpTook, quicTook time.Duration
}

// The self-test's time limits, per route. A connection to this machine's
// own address takes a few milliseconds; the limits are for a machine that
// drops it (probeDialTimeout, the connect) or holds it — the macOS
// firewall does while it asks about a program it has not seen
// (probeRouteTimeout, the connect and the handshake together).
const (
	probeDialTimeout  = 2 * time.Second
	probeRouteTimeout = 5 * time.Second
)

// probeLANListener opens the local-network listener as a send does
// (listenLANWithRetry, the function a send calls) on a random port of the
// range senders use, and connects to it as a receiver does, through ip —
// this machine's LAN address, not loopback: first the TCP route, then
// QUIC, each through the PAKE with a secret made up for this run.
func probeLANListener(ip net.IP) lanProbe {
	var p lanProbe
	port := freeLANPort()
	p.addr = net.JoinHostPort(ip.String(), strconv.Itoa(port))
	secret := probeSecret()

	lctx, lcancel := context.WithTimeout(context.Background(), 5*time.Second)
	ln, err := listenLANWithRetry(lctx, port, secret)
	lcancel()
	if err != nil {
		p.listenErr = err
		return p
	}
	defer func() { _ = ln.Close() }()

	// The sender side of each handshake runs in the listener; collect
	// what it accepts so both ends close cleanly.
	actx, stop := context.WithCancel(context.Background())
	defer stop()
	accepted := make(chan *quicconn.AcceptResult, 2)
	go func() {
		for {
			res, err := ln.Accept(actx)
			switch {
			case err == nil:
				accepted <- res
			case errors.Is(err, quicconn.ErrHandshakeFailed):
			default:
				return
			}
		}
	}()

	dial := func(route func(context.Context) (*quicconn.AcceptResult, error)) (time.Duration, error) {
		ctx, cancel := context.WithTimeout(context.Background(), probeRouteTimeout)
		defer cancel()
		start := time.Now()
		res, err := route(ctx)
		took := time.Since(start)
		if err != nil {
			return took, err
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			select {
			case sender := <-accepted:
				sender.Close()
			case <-time.After(time.Second):
			}
		}()
		res.Close()
		<-done
		return took, nil
	}
	p.tcpTook, p.tcpErr = dial(func(ctx context.Context) (*quicconn.AcceptResult, error) {
		return quicconn.DialTCP(ctx, p.addr, secret, probeDialTimeout)
	})
	p.quicTook, p.quicErr = dial(func(ctx context.Context) (*quicconn.AcceptResult, error) {
		return quicconn.DialTimeout(ctx, p.addr, secret, probeDialTimeout)
	})
	return p
}

// freeLANPort picks a port of the listener's range that is free for UDP
// right now, so a port another transfer holds doesn't cost
// listenLANWithRetry its two seconds of retries. When none of a few tries
// is free, the last one is returned and the listen reports why.
func freeLANPort() int {
	port := lanPortFirst
	for range 8 {
		port = lanPortFirst + mrand.IntN(lanPortLast-lanPortFirst+1)
		pc, err := net.ListenPacket("udp", ":"+strconv.Itoa(port))
		if err == nil {
			_ = pc.Close()
			return port
		}
	}
	return port
}

// probeSecret is the PAKE secret of one self-test: random, so nothing
// else on the network can complete a handshake with the listener while it
// is open.
func probeSecret() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "doctor-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// listenerScope is the faint note under a passing listener row: what the
// self-test does not show. On macOS the application firewall has screened
// the connection (its row says what it found), which leaves the network
// between two devices; elsewhere a firewall is not shown either.
func listenerScope(goos string) string {
	const dialled = "dialled from this machine: the connection never left it, so this shows the listener works, "
	if goos == "darwin" {
		return dialled + "not that the network lets other devices reach it"
	}
	return dialled + "not that other devices get past a firewall"
}

// listenerCheck turns the self-test into its report row.
func listenerCheck(p lanProbe) doctorCheck {
	const viaServer = "transfers to devices on this network go through the server"
	ms := uxlog.HumanDuration
	switch {
	case p.listenErr != nil:
		return warnRow("listener", fmt.Sprintf("cannot open the local-network listener (%s) — %s",
			plainListenErr(p.listenErr), viaServer))
	case p.tcpErr == nil && p.quicErr == nil:
		return passRow("listener", fmt.Sprintf("answered at %s over TCP in %s and QUIC in %s",
			p.addr, ms(p.tcpTook), ms(p.quicTook))).with(listenerScope(runtime.GOOS))
	case p.quicErr == nil:
		return warnRow("listener", fmt.Sprintf("answered at %s over QUIC in %s but not over TCP (%s) — same-network transfers take the slower QUIC route",
			p.addr, ms(p.quicTook), plainNetErr(p.tcpErr)))
	case p.tcpErr == nil:
		return warnRow("listener", fmt.Sprintf("answered at %s over TCP in %s but not over QUIC (%s) — receivers that can't use the TCP route go through the server",
			p.addr, ms(p.tcpTook), plainNetErr(p.quicErr)))
	}
	return warnRow("listener", fmt.Sprintf("did not answer at %s, even from this machine (TCP: %s, QUIC: %s) — %s",
		p.addr, plainNetErr(p.tcpErr), plainNetErr(p.quicErr), viaServer))
}

// plainListenErr names a failed bind in plain words; --debug appends the
// raw error.
func plainListenErr(err error) string {
	s := "the port could not be bound"
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		s = "binding the port timed out"
	}
	if debugRequested() {
		s += ": " + err.Error()
	}
	return s
}
