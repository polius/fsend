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
	"time"

	"github.com/spf13/cobra"

	codepkg "github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/landisc"
	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
	"github.com/polius/fsend/internal/syserr"
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
	var server string
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
			if runDoctor(server) > 0 {
				return fserrors.ErrDoctorFailed
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "check this server instead of the saved one (or FSEND_SERVER)")
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
func runDoctor(serverFlag string) (failed int) {
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
	// --server / FSEND_SERVER redirect the run, so check that server.
	src := "FSEND_SERVER"
	if serverFlag != "" {
		src = "--server"
	}
	if err := setServerOverride(serverFlag); err != nil {
		sayf(false, "config", "%s is invalid (%v)", src, err)
	} else if serverOverride != "" {
		srv = serverOverride
		sayf(true, "config", "%s overrides it: %s", src, srv)
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
		if c, ok := clockCheck(resp.Header, start); ok {
			say(c)
		}
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	if serverUp {
		checks, mapped := relayChecks(serverBaseURL(srv), pw)
		for _, c := range checks {
			say(c)
		}
		if c, ok := natCheck(mapped, localAddrs()); ok {
			say(c)
		}
	}

	ip := landisc.PreferredLocalIP()
	if ip.IsLoopback() {
		sayf(false, "network", "no non-loopback address found — only same-machine transfers will work")
	} else {
		sayf(true, "network", "LAN address %s", ip)
	}
	say(ipv6Check(localAddrs()))
	if c, ok := firewallCheck(); ok {
		say(c)
	}

	// mDNS: prove the announce→query roundtrip works from this host, on
	// two tiers:
	//
	//   1. loopback — finds a sender on this same machine. Linux brings lo
	//      up without the multicast flag, and Windows never answers
	//      multicast on loopback, so a miss here alone is normal there.
	//   2. LAN — the physical interface, which is what reaching other
	//      devices needs. macOS 15+ (Local Network Privacy) and some Wi-Fi
	//      APs silently drop multicast there; discovery then falls back to
	//      the pairing server (transfers still complete).
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
	loopStart := time.Now()
	_, loopErr := landisc.QueryLoopback(ctx, doctorCode, 500*time.Millisecond)
	loopTook := time.Since(loopStart)

	// The LAN tier runs whatever loopback did: only a query that leaves
	// via the physical interface tells whether other devices can be found.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	lanStart := time.Now()
	_, lanErr := landisc.QueryInterface(ctx2, doctorCode, 500*time.Millisecond, ip)
	lanTook := time.Since(lanStart)

	for _, c := range mdnsChecks(loopErr, lanErr, loopTook, lanTook, ip) {
		say(c)
	}
	return failed
}

// mdnsChecks turns the two mDNS tiers into report rows. A loopback miss
// alone is no failure: it only means a sender on this same machine is
// found through the LAN interface instead (see the tiers in runDoctor).
func mdnsChecks(loopErr, lanErr error, loopTook, lanTook time.Duration, ip net.IP) []doctorCheck {
	ms := func(d time.Duration) time.Duration { return d.Round(time.Millisecond) }
	switch {
	case loopErr == nil && lanErr == nil:
		return []doctorCheck{
			{true, "mDNS", fmt.Sprintf("local roundtrip ok in %s", ms(loopTook))},
			{true, "mDNS", fmt.Sprintf("answered on the LAN in %s", ms(lanTook))},
		}
	case lanErr == nil:
		return []doctorCheck{{true, "mDNS", fmt.Sprintf("answered on the LAN in %s (loopback has no multicast on this system; not needed)", ms(lanTook))}}
	case loopErr == nil:
		return []doctorCheck{
			{true, "mDNS", fmt.Sprintf("local roundtrip ok in %s", ms(loopTook))},
			{false, "mDNS", fmt.Sprintf("no multicast answer via %s (%s) — discovering other devices falls back to the pairing server; check Local Network permission / Wi-Fi", ip, ms(lanTook))},
		}
	}
	return []doctorCheck{{false, "mDNS", fmt.Sprintf("no multicast answer on loopback or via %s (%v) — LAN discovery unavailable; transfers use the pairing server", ip, lanErr)}}
}

// clockCheck compares this device's clock with the server's Date header,
// read at sentAt. Codes are salted with the UTC day, so a clock that is
// hours off finds no sender on the local network, and an older fsend on
// the other side finds none through the server either. ok is false when
// the server sent no usable Date.
func clockCheck(h http.Header, sentAt time.Time) (doctorCheck, bool) {
	skew, ok := signaling.ServerSkew(h, sentAt)
	if !ok {
		return doctorCheck{}, false
	}
	if skew > signaling.ClockTolerance || skew < -signaling.ClockTolerance {
		return doctorCheck{false, "clock", signaling.ClockSkewPhrase(skew) +
			" the server's — codes may not be found on the local network until the date and time are fixed"}, true
	}
	return doctorCheck{true, "clock", "matches the server's"}, true
}

// relayChecks tests the two ways a relayed transfer reaches the server:
// the TCP fallback (a WebSocket echo on the HTTPS port, which also names
// the relay's UDP address) and UDP itself (a STUN request to the relay).
//
// It also returns this machine's public address as the relay saw it (nil
// without a UDP answer), for natCheck.
func relayChecks(baseURL, pw string) ([]doctorCheck, *net.UDPAddr) {
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
	mapped, udpErr := relay.ProbeSTUN(uctx, relayAddr)
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
	return []doctorCheck{udp, tcp}, mapped
}

// localAddrs lists this machine's interface addresses.
func localAddrs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			out = append(out, ipnet.IP)
		}
	}
	return out
}

// natCheck says whether this machine sits behind NAT, from the public
// address the relay saw. Behind NAT a direct path across networks needs
// hole punching, which some NATs (symmetric, carrier-grade) defeat; those
// transfers use the relay. Telling NAT types apart needs a second STUN
// server, which an fsend server doesn't run, so the row reports only the
// address. Informational: either answer is fine. No row without an
// answer, or when the relay is on this network (it then sees a local
// address, which says nothing about the internet).
func natCheck(mapped *net.UDPAddr, local []net.IP) (doctorCheck, bool) {
	if mapped == nil || mapped.IP.IsLoopback() || mapped.IP.IsPrivate() || mapped.IP.IsLinkLocalUnicast() {
		return doctorCheck{}, false
	}
	for _, ip := range local {
		if ip.Equal(mapped.IP) {
			return doctorCheck{true, "NAT", fmt.Sprintf("none — this machine has the public address %s", mapped.IP)}, true
		}
	}
	return doctorCheck{true, "NAT", fmt.Sprintf("behind NAT, public address %s — direct paths need hole punching; when it fails, transfers use the relay", mapped.IP)}, true
}

// ipv6Check reports whether this machine has a global IPv6 address, which
// lets direct paths skip NAT entirely when the other side has one too.
// Informational: IPv4 alone works.
func ipv6Check(local []net.IP) doctorCheck {
	for _, ip := range local {
		if ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			return doctorCheck{true, "IPv6", fmt.Sprintf("global address %s", ip)}
		}
	}
	return doctorCheck{true, "IPv6", "no global address (IPv4 only) — fine; direct paths use IPv4"}
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
	case syserr.IsConnRefused(err):
		s = "connection refused"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		s = "timed out"
	case syserr.IsNoRoute(err):
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

Runs read-only checks and reports each as ✓ (working) or !
(degraded, with the consequence). mDNS reports its two tiers —
loopback, then the physical interface — as separate rows:

  config    which pairing server is configured (never prints the password)
  server    HTTPS reachability of that server's /health
  clock     this device's clock against the server's (codes are salted
            with the date, so a wrong clock can make a code unfindable)
  UDP       the server's relay answers over UDP (needed unless the TCP
            fallback below works)
  TCP       the relay's TCP fallback (a WebSocket on the server's port),
            used when a network blocks UDP
  NAT       whether this machine is behind NAT, from the public
            address the relay saw (informational)
  network   the local LAN address fsend would announce
  IPv6      whether this machine has a global IPv6 address
            (informational)
  firewall  macOS only: whether the firewall lets other devices on
            the network connect to fsend
  mDNS      a real announce→query roundtrip on this machine, in two
            tiers: loopback (fsend's mDNS machinery) and the physical
            LAN interface (discovering other devices)

Exits 0 when every check passes and 43 (E043) when any is degraded.
Every degraded check has a graceful fallback: without a server,
same-LAN transfers still work; without mDNS, transfers use the
pairing server; without UDP, the relay's TCP fallback.

  --server <host>   Check this server instead of the saved one
                    (or FSEND_SERVER)
`
