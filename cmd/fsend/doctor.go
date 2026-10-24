package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	codepkg "github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/landisc"
	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/version"
)

// doctorCmd implements `fsend doctor`: a read-only diagnostic covering the
// pieces a transfer depends on — config, pairing server, the relay over UDP
// and over its TCP fallback, local address, mDNS roundtrip. It changes no
// state. It exits 0 when every check passes and with E043 when any is
// degraded, so scripts can gate on it; the report says which fallback
// will kick in.
func doctorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check config, server and relay reachability, and LAN discovery",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("%w: doctor takes no positional arguments (got %q)", fserrors.ErrUsage, args[0])
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runDoctor() > 0 {
				return fserrors.ErrDoctorFailed
			}
			return nil
		},
	}
	dht := boldHelpHeaders(doctorHelpTemplate)
	c.SetHelpTemplate(dht)
	c.SetUsageTemplate(dht)
	return c
}

// doctorCode drives the self-announce/query roundtrip. It deliberately
// falls outside the share-code alphabet ('o' is never generated), so the
// derived mDNS name can't collide with a real transfer's.
const doctorCode = "doctor-check"

// doctorTitle is the heading line. Bold is unconditional in uxlog, so the
// gate lives here: stderr is where doctor writes, and ColorFor(stderr) is
// the matching probe.
func doctorTitle() string {
	const title = "fsend doctor"
	if uxlog.ColorFor(os.Stderr) {
		return uxlog.Bold(title)
	}
	return title
}

// doctorCheck is one report row.
type doctorCheck struct {
	ok         bool
	label, msg string
}

// runDoctor prints the report and returns how many checks failed.
func runDoctor() (failed int) {
	passed := 0
	say := func(c doctorCheck) {
		glyph := uxlog.Warn()
		if c.ok {
			glyph = uxlog.Check()
			passed++
		} else {
			failed++
		}
		fmt.Fprintf(os.Stderr, "%s %-8s %s\n", glyph, c.label, c.msg)
	}
	sayf := func(ok bool, label, format string, args ...any) {
		say(doctorCheck{ok, label, fmt.Sprintf(format, args...)})
	}

	fmt.Fprintf(os.Stderr, "%s — checking what a transfer depends on\n\n", doctorTitle())
	// The success verdict closes the report on every path — including the
	// early returns below. A failure is reported by the caller (E043).
	defer func() {
		if failed == 0 {
			fmt.Fprintf(os.Stderr, "\n%s %s\n", uxlog.Check(), uxlog.Good(
				fmt.Sprintf("All %d checks passed. Transfers will try your local network first.", passed)))
		} else {
			fmt.Fprintln(os.Stderr)
		}
	}()

	srv := config.DefaultServer
	pw := ""
	cfg, err := config.Load()
	if err == nil {
		pw, _ = effectiveServerPassword(cfg)
	}
	switch {
	case err != nil:
		sayf(false, "config", "unreadable, using defaults (%v)", err)
	case cfg.IsDefault():
		sayf(true, "config", "server %s (default)", srv)
	default:
		srv = cfg.EffectiveServer()
		detail := "server " + srv + " (custom)"
		if pw != "" {
			detail += ", password set"
		}
		sayf(true, "config", "%s", detail)
	}
	// FSEND_SERVER redirects every run, so check the server runs would use.
	if err := setServerOverride(""); err != nil {
		sayf(false, "config", "FSEND_SERVER is invalid (%v)", err)
	} else if serverOverride != "" {
		srv = serverOverride
		sayf(true, "config", "FSEND_SERVER overrides it: %s", srv)
	}

	// Server reachability: /health is the one endpoint that never requires
	// the self-hosted password, so it works against any server.
	start := time.Now()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(serverBaseURL(srv) + "/health")
	serverUp := false
	switch {
	case err != nil:
		sayf(false, "server", "%s unreachable (%s) — cross-network transfers will fail, LAN transfers unaffected", srv, plainNetErr(err))
	case resp.StatusCode != http.StatusOK:
		sayf(false, "server", "%s answered HTTP %d — cross-network transfers may fail, LAN transfers unaffected", srv, resp.StatusCode)
	default:
		serverUp = true
		sayf(true, "server", "reachable in %s", time.Since(start).Round(time.Millisecond))
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	if serverUp {
		for _, c := range relayChecks(serverBaseURL(srv), pw) {
			say(c)
		}
	}

	ip := landisc.PreferredLocalIP()
	if ip.IsLoopback() {
		sayf(false, "network", "no non-loopback address found — only same-machine transfers will work")
	} else {
		sayf(true, "network", "LAN address %s", ip)
	}

	// mDNS: prove the full announce→query roundtrip works from this host.
	// The check is two-tier so each outcome is deterministic and honest:
	//
	//   1. machinery — loopback multicast, which the OS never filters.
	//      A miss here means fsend's mDNS itself is broken on this host.
	//   2. LAN — the physical interface. macOS 15+ (Local Network
	//      Privacy) and some Wi-Fi APs silently drop or stall multicast
	//      there; when that happens, discovering *other* devices falls
	//      back to the pairing server (transfers still complete, and
	//      same-device transfers keep working via loopback).
	ann, err := landisc.Announce(doctorCode, codepkg.Epoch(time.Now()), ip)
	if err != nil {
		sayf(false, "mDNS", "cannot open multicast sockets (%v) — LAN discovery unavailable", err)
		return failed
	}
	defer landisc.StopAnnounce(ann)

	// Let the announcer settle before asking: a query fired the instant
	// the group join completes is racy (observed on macOS/Wi-Fi).
	time.Sleep(250 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	machStart := time.Now()
	if _, err := landisc.QueryLoopback(ctx, doctorCode, 500*time.Millisecond); err != nil {
		sayf(false, "mDNS", "roundtrip failed even on loopback (%v) — LAN discovery unavailable", err)
		return failed
	}
	sayf(true, "mDNS", "local roundtrip ok in %s", time.Since(machStart).Round(time.Millisecond))

	// LAN tier: a query that only leaves via the physical interface. If
	// the OS or the AP blocks multicast there, this is the honest signal.
	lanStart := time.Now()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := landisc.QueryInterface(ctx2, doctorCode, 500*time.Millisecond, ip); err != nil {
		sayf(false, "mDNS", "no multicast answer via %s (%s) — discovering other devices falls back to the pairing server; check Local Network permission / Wi-Fi", ip, time.Since(lanStart).Round(time.Millisecond))
	} else {
		sayf(true, "mDNS", "answered on the LAN in %s", time.Since(lanStart).Round(time.Millisecond))
	}
	return failed
}

// relayChecks tests the two ways a relayed transfer reaches the server:
// the TCP fallback (a WebSocket echo on the HTTPS port, which also names
// the relay's UDP address) and UDP itself (a STUN request to the relay).
func relayChecks(baseURL, pw string) []doctorCheck {
	h := http.Header{}
	h.Set("User-Agent", "fsend/"+version.Version)
	if pw != "" {
		h.Set(server.AuthHeader, pw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	hdr, tcpErr := relay.ProbeWS(ctx, baseURL+"/v1/relay/ws", h)
	tcp := doctorCheck{ok: tcpErr == nil, label: "TCP"}
	var se *relay.HTTPStatusError
	switch {
	case tcpErr == nil:
		tcp.msg = fmt.Sprintf("relay fallback over TCP works (echo in %s)", time.Since(start).Round(time.Millisecond))
	case errors.As(tcpErr, &se) && se.Code == http.StatusUnauthorized:
		tcp.msg = "the server wants a password — set it with: fsend --connect <host:port>,<password>"
	case errors.As(tcpErr, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusServiceUnavailable):
		tcp.msg = "this server has no TCP fallback for its relay — on a network that blocks UDP, cross-network transfers will fail"
	default:
		tcp.msg = fmt.Sprintf("relay fallback over TCP failed (%s) — on a network that blocks UDP, cross-network transfers will fail", plainNetErr(tcpErr))
	}

	host := ""
	if u, err := url.Parse(baseURL); err == nil {
		host = u.Hostname()
	}
	relayAddr := ""
	if hdr != nil {
		relayAddr = hdr.Get(server.RelayAddrHeader)
	}
	if relayAddr == "" || validateRelayAddr(relayAddr, host) != nil {
		relayAddr = net.JoinHostPort(host, "443") // the stock layout
	}
	uctx, ucancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ucancel()
	start = time.Now()
	_, udpErr := relay.ProbeSTUN(uctx, relayAddr)
	udp := doctorCheck{ok: udpErr == nil, label: "UDP"}
	switch {
	case udpErr == nil:
		udp.msg = fmt.Sprintf("relay %s answered in %s", relayAddr, time.Since(start).Round(time.Millisecond))
	case errors.Is(udpErr, relay.ErrNoReply) && tcp.ok:
		udp.msg = fmt.Sprintf("no UDP reply from %s — this network seems to block UDP; transfers will use the slower TCP fallback", relayAddr)
	case errors.Is(udpErr, relay.ErrNoReply):
		udp.msg = fmt.Sprintf("no UDP reply from %s — this network seems to block UDP", relayAddr)
	default:
		udp.msg = fmt.Sprintf("cannot reach relay %s over UDP (%s)", relayAddr, plainNetErr(udpErr))
	}
	return []doctorCheck{udp, tcp}
}

// plainNetErr names a network failure in plain words; --debug appends the
// raw error.
func plainNetErr(err error) string {
	var (
		dnsErr  *net.DNSError
		certErr *tls.CertificateVerificationError
		netErr  net.Error
		s       string
	)
	switch {
	case errors.As(err, &dnsErr):
		s = "could not resolve the name"
	case errors.As(err, &certErr):
		s = "its TLS certificate is not trusted"
	case errors.Is(err, syscall.ECONNREFUSED):
		s = "connection refused"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		s = "timed out"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		s = "no route to it"
	default:
		s = "connection failed"
	}
	if debugRequested() {
		s += ": " + err.Error()
	}
	return s
}

const doctorHelpTemplate = `fsend doctor — check what a transfer depends on

Runs read-only checks and reports each as ✓ (working) or ⚠
(degraded, with the consequence). mDNS reports its two tiers —
loopback, then the physical interface — as separate rows:

  config    which pairing server is configured (never prints the password)
  server    HTTPS reachability of that server's /health
  UDP       the server's relay answers over UDP (needed unless the TCP
            fallback below works)
  TCP       the relay's TCP fallback (a WebSocket on the server's port),
            used when a network blocks UDP
  network   the local LAN address fsend would announce
  mDNS      a real announce→query roundtrip on this machine, in two
            tiers: loopback (fsend's mDNS machinery) and the physical
            LAN interface (discovering other devices)

Exits 0 when every check passes and 43 (E043) when any is degraded.
Every degraded check has a graceful fallback: without a server,
same-LAN transfers still work; without mDNS, transfers use the
pairing server; without UDP, the relay's TCP fallback.
`
