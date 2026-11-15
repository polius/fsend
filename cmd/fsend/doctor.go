package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

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
// and over its TCP fallback, local address, the local-network listener and
// the firewall in front of it, mDNS roundtrip. It changes no state. It
// exits 0 when every check passes and with E043 when any is degraded, so
// scripts can gate on it; the report says which fallback will kick in.
func doctorCmd() *cobra.Command {
	var server string
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check config, server and relay reachability, the local listener and firewall, and LAN discovery",
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

// doctorCheck is one report row: a pass (✓), a problem (!, which fails the
// run), or a note. A note is for what doctor could not find out — a
// firewall whose rules need root to read — so it has no glyph, is not
// counted as a check and never fails the run.
type doctorCheck struct {
	ok         bool
	label, msg string
	note       bool
	// detail prints under the row in the faint tier: what a result does
	// not prove, or how to fix it.
	detail []detailLine
}

// detailLine is one faint line under a row: a sentence, or a command to
// copy.
type detailLine struct {
	text string
	cmd  bool
}

func passRow(label, msg string) doctorCheck { return doctorCheck{ok: true, label: label, msg: msg} }
func warnRow(label, msg string) doctorCheck { return doctorCheck{label: label, msg: msg} }
func noteRow(label, msg string) doctorCheck { return doctorCheck{note: true, label: label, msg: msg} }

// with adds sentences under the row.
func (c doctorCheck) with(text ...string) doctorCheck {
	for _, t := range text {
		c = c.add(detailLine{text: t})
	}
	return c
}

// run adds commands under the row.
func (c doctorCheck) run(cmds ...string) doctorCheck {
	for _, t := range cmds {
		c = c.add(detailLine{text: t, cmd: true})
	}
	return c
}

func (c doctorCheck) add(d ...detailLine) doctorCheck {
	c.detail = append(c.detail[:len(c.detail):len(c.detail)], d...)
	return c
}

// doctorLabelWidth pads the label column: the glyph sits in column 0, the
// label starts in column 2 and the message in column 11.
const doctorLabelWidth = 8

// printDoctorRow writes one row to w on the grid. On a terminal a long
// message breaks at spaces and continues in the message column, where
// the detail lines start too, instead of running back into the glyph's
// gutter; a command is left whole so it copies as one line. Off a
// terminal each is one line. In plain output, where the marker is "[OK]"
// or "[!]" rather than one column, the message column moves with it.
func printDoctorRow(w io.Writer, c doctorCheck) {
	glyph, width := uxlog.Warn(), 1
	switch {
	case c.note:
		glyph = uxlog.Info()
	case c.ok:
		glyph = uxlog.Check()
	}
	if !uxlog.CanEscape(os.Stderr) {
		width = len(glyph) // an ASCII marker, no escapes
	}
	col := width + 1 + doctorLabelWidth + 1
	pad := strings.Repeat(" ", col)
	avail := doctorTextWidth(col)
	msg := doctorWrap(c.msg, avail)
	_, _ = fmt.Fprintf(w, "%s %-*s %s\n", glyph, doctorLabelWidth, c.label, msg[0])
	for _, line := range msg[1:] {
		_, _ = fmt.Fprintln(w, pad+line)
	}
	for _, d := range c.detail {
		lines := []string{d.text}
		if !d.cmd {
			lines = doctorWrap(d.text, avail)
		}
		for _, line := range lines {
			_, _ = fmt.Fprintln(w, pad+uxlog.Muted(line))
		}
	}
}

// doctorTextWidth is how many columns a row's text may take when it
// starts in column col: up to the terminal's last column but one, and
// never under 20. Off a terminal there is no limit.
func doctorTextWidth(col int) int {
	width, _, err := term.GetSize(int(os.Stderr.Fd())) //nolint:gosec // G115: a file descriptor
	if err != nil || width <= 0 {
		return 1 << 16
	}
	return max(20, width-1-col)
}

// doctorWrap breaks text at spaces into lines of at most width columns;
// a longer word stays whole on a line of its own. It always returns at
// least one line.
func doctorWrap(text string, width int) []string {
	var lines []string
	line, used := "", 0
	for _, word := range strings.Fields(text) {
		n := utf8.RuneCountInString(word)
		switch {
		case line == "":
			line, used = word, n
		case used+1+n > width:
			lines = append(lines, line)
			line, used = word, n
		default:
			line += " " + word
			used += 1 + n
		}
	}
	return append(lines, line)
}

// runDoctor prints the report and returns how many checks failed.
func runDoctor(serverFlag string) (failed int) {
	passed := 0
	say := func(c doctorCheck) {
		switch {
		case c.note:
		case c.ok:
			passed++
		default:
			failed++
		}
		printDoctorRow(os.Stderr, c)
	}
	sayf := func(ok bool, label, format string, args ...any) {
		say(doctorCheck{ok: ok, label: label, msg: fmt.Sprintf(format, args...)})
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
		sayf(true, "server", "reachable in %s", uxlog.HumanDuration(time.Since(start)))
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
	// The listener first, the firewall after: opening the listener is what
	// makes macOS and Windows ask whether fsend may accept connections, and
	// the firewall row reads the answer.
	answered := false
	if !ip.IsLoopback() {
		p := probeLANListener(ip)
		answered = p.listenErr == nil && (p.tcpErr == nil || p.quicErr == nil)
		say(listenerCheck(p))
	}
	if c, ok := firewallCheck(ip, answered); ok {
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
	ms := uxlog.HumanDuration
	switch {
	case loopErr == nil && lanErr == nil:
		return []doctorCheck{
			passRow("mDNS", fmt.Sprintf("local roundtrip ok in %s", ms(loopTook))),
			passRow("mDNS", fmt.Sprintf("answered on the LAN in %s", ms(lanTook))),
		}
	case lanErr == nil:
		return []doctorCheck{passRow("mDNS", fmt.Sprintf("answered on the LAN in %s (loopback has no multicast on this system; not needed)", ms(lanTook)))}
	case loopErr == nil:
		return []doctorCheck{
			passRow("mDNS", fmt.Sprintf("local roundtrip ok in %s", ms(loopTook))),
			warnRow("mDNS", fmt.Sprintf("no multicast answer via %s (%s) — discovering other devices falls back to the pairing server; check Local Network permission / Wi-Fi", ip, ms(lanTook))),
		}
	}
	return []doctorCheck{warnRow("mDNS", fmt.Sprintf("no multicast answer on loopback or via %s (%v) — LAN discovery unavailable; transfers use the pairing server", ip, lanErr))}
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
		return warnRow("clock", signaling.ClockSkewPhrase(skew)+
			" the server's — codes may not be found on the local network until the date and time are fixed"), true
	}
	return passRow("clock", "matches the server's"), true
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
		tcp.msg = fmt.Sprintf("relay fallback over TCP works (echo in %s)", uxlog.HumanDuration(time.Since(start)))
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
		udp.msg = fmt.Sprintf("relay %s answered in %s", relayAddr, uxlog.HumanDuration(time.Since(start)))
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
			return passRow("NAT", fmt.Sprintf("none — this machine has the public address %s", mapped.IP)), true
		}
	}
	return passRow("NAT", fmt.Sprintf("behind NAT, public address %s — direct paths need hole punching; when it fails, transfers use the relay", mapped.IP)), true
}

// ipv6Check reports whether this machine has a global IPv6 address, which
// lets direct paths skip NAT entirely when the other side has one too.
// Informational: IPv4 alone works.
func ipv6Check(local []net.IP) doctorCheck {
	for _, ip := range local {
		if ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			return passRow("IPv6", fmt.Sprintf("global address %s", ip))
		}
	}
	return passRow("IPv6", "no global address (IPv4 only) — fine; direct paths use IPv4")
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
(degraded, with the consequence). A row with no mark is something
doctor could not find out, such as a firewall whose rules need root
to read: it fails nothing. mDNS reports its two tiers — loopback,
then the physical interface — as separate rows:

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
  listener  opens the local-network listener as a send does (UDP for
            QUIC, and TCP) and connects to it through this machine's
            LAN address. Shows that fsend can listen and complete a
            handshake there; it cannot show that another device gets
            past a firewall, because the connection never leaves this
            machine
  firewall  whether this machine's firewall lets devices on the
            network connect to that listener (UDP and TCP ports
            50000-50999), and the command that opens it: the macOS
            firewall, Windows Defender Firewall, and on Linux ufw,
            firewalld or nftables/iptables. Rules that need root to
            read are reported as unread, not guessed
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
