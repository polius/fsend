// Package landisc handles same-LAN peer discovery via mDNS.
//
// The sender announces a service name derived from the code
// (`fsend-<hash>.local`); the receiver queries for the same name and gets
// back the sender's LAN address. The name is an argon2id stretch of the
// code, not the code itself: the name is multicast to the whole broadcast
// domain, the code is the PAKE secret, and the code has only ~45 bits of
// entropy — a fast hash would let a passive LAN observer recover it by
// offline brute force. argon2id makes each guess cost ~64 MiB of memory
// and tens of milliseconds, putting a full sweep out of practical reach.
//
// This package only does discovery — the actual connection is established
// via internal/quicconn after we have an address.
//
// LAN discovery short-circuits the pairing-server + ICE path entirely: when
// both peers are on the same broadcast domain, we never hit the internet.
package landisc

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/mdns/v2"
	"golang.org/x/crypto/argon2"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// derive stretches the code with argon2id and returns 18 key bytes:
// 16 for the service name, 2 for the port. The salt is a fixed domain
// label — the peers share no prior state, so a per-session salt is
// impossible; the memory-hardness is what carries the defense (see the
// package comment). Memoized because announce/query each need the same
// key for both the name and the port.
func derive(code string) []byte {
	deriveMu.Lock()
	defer deriveMu.Unlock()
	if code == derivedCode {
		return derivedKey
	}
	key := argon2.IDKey([]byte(code), []byte("fsend-landisc-v2"), 2, 64*1024, 4, 18)
	derivedCode, derivedKey = code, key
	return key
}

var (
	deriveMu    sync.Mutex
	derivedCode string
	derivedKey  []byte
)

// serviceName builds the mDNS name we publish/query for a given code.
//
// We use `.local` so the name lives in the .local domain that every mDNS
// stack on the planet honors. Both peers derive identically, so announce
// and query still line up.
func serviceName(code string) string {
	return "fsend-" + hex.EncodeToString(derive(code)[:16]) + ".local"
}

// unusableIfacePrefixes extends virtualIface with Apple-only interfaces
// that can never carry a peer we could usefully dial: AWDL (AirDrop) and
// its low-latency sibling, Apple's network probe interfaces, and the
// internet-sharing bridges.
var unusableIfacePrefixes = append([]string{
	"awdl", "llw", "anpi", "bridge", "ap",
}, virtualIfacePrefixes...)

// discoveryIface filters one interface for mDNS use. Only interfaces a
// peer could actually live on are kept: up, multicast-capable, not a
// VPN tunnel / container bridge / Apple virtual link, and carrying an IP
// address fsend could announce or dial (any IPv4, or a global-unicast
// IPv6 for IPv6-only networks).
func discoveryIface(ifc net.Interface, addrs []net.Addr) bool {
	if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 {
		return false
	}
	if virtualIface(ifc.Name) {
		return false
	}
	n := strings.ToLower(ifc.Name)
	for _, p := range unusableIfacePrefixes {
		if strings.HasPrefix(n, p) {
			return false
		}
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipnet.IP.To4() != nil {
			return true
		}
		if ipnet.IP.IsGlobalUnicast() && !ipnet.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// discoveryInterfaces returns the interface list mDNS should join, announce
// and query on, loopback first. Restricting the list matters for
// reliability, not just tidiness: pion/mdns writes each question once per
// interface, sequentially, with no write deadline. Two failure modes were
// observed on macOS 15 (Local Network Privacy enforcement):
//
//   - A stalled interface write parks the query goroutine forever,
//     ignoring the caller's context. Anything written before the stall is
//     already on the wire, so ordering loopback first guarantees the
//     self-test/same-machine copy always flies.
//   - Writes to the blocked physical interface stall rather than error,
//     delaying every interface after it in the list.
//
// Fewer interfaces, fewer stalls — and none of the skipped ones can carry
// a legitimate fsend peer anyway.
func discoveryInterfaces() []net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		if discoveryIface(ifaces[i], addrs) {
			out = append(out, ifaces[i])
		}
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i].Flags&net.FlagLoopback != 0, out[j].Flags&net.FlagLoopback != 0
		if li != lj {
			return li
		}
		return out[i].Index < out[j].Index
	})
	return out
}

// closePacketConns tears down the multicast sockets. pion/mdns does not
// close caller-supplied conns on its own error paths, so every failure
// return after openMulticast must do it here or leak one UDP socket per
// call.
func closePacketConns(v4 *ipv4.PacketConn, v6 *ipv6.PacketConn) {
	_ = v4.Close()
	if v6 != nil {
		_ = v6.Close()
	}
}

// Announce publishes the given local address under the code-derived service
// name. Call Close on the returned *mdns.Conn to stop announcing.
//
// The sender calls this just before listening for QUIC connections, so the
// receiver can discover it. ip is the address we want receivers to dial —
// usually the first non-loopback IPv4 on this machine.
//
// The port is not part of the announcement: pion/mdns only carries the
// IP, and both sides derive the UDP port from the code via PortForCode.
func Announce(code string, ip net.IP) (*mdns.Conn, error) {
	v4Conn, v6Conn, err := openMulticast()
	if err != nil {
		return nil, fmt.Errorf("landisc: opening multicast: %w", err)
	}

	name := serviceName(code)

	conn, err := mdns.NewServer(v4Conn, v6Conn,
		mdns.WithName("fsend-sender"),
		mdns.WithLocalNames(name),
		mdns.WithLocalAddress(ip),
		mdns.WithIncludeLoopback(true),
		mdns.WithInterfaces(discoveryInterfaces()...),
		// Legacy parity: answer only A/AAAA (WebRTC/ICE-style naming),
		// no proactive cache refresh.
		mdns.WithRecordTypes(dnsmessage.TypeA, dnsmessage.TypeAAAA),
		mdns.WithCacheRefresh(false),
	)
	if err != nil {
		closePacketConns(v4Conn, v6Conn)
		return nil, fmt.Errorf("landisc: mdns server: %w", err)
	}
	return conn, nil
}

// StopAnnounce closes an announcement conn, but never blocks past
// watchdogGrace: pion/mdns's Close has been observed to wedge forever. Past
// the grace we abandon it, leaking the goroutine until process exit — the
// same tradeoff Query makes.
func StopAnnounce(conn io.Closer) {
	if conn == nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = conn.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(watchdogGrace):
	}
}

// QueryResult bundles the discovered peer's address + port.
type QueryResult struct {
	IP   net.IP
	Port int
}

// watchdogGrace is how long past the caller's timeout Query waits for
// pion/mdns before force-closing the sockets and declaring a LAN miss.
// QueryAddr is supposed to honor its context deadline, but a wedged
// read/close path inside the library has been observed to block forever
// while re-multicasting the query every QueryInterval — and that spam
// in turn wedges the next receiver querying the same name, so one stuck
// process poisons every later transfer with that code on the LAN.
const watchdogGrace = 2 * time.Second

// queryRetry is how often Query re-issues its question within one window
// (legacy QueryInterval: 100ms). See the retry loop in queryOn.
const queryRetry = 100 * time.Millisecond

// Query searches for a sender announcing the given code, up to timeout.
//
// Two sequential attempts on separate sockets:
//
//  1. loopback — finds a sender on THIS machine (tests, scripted pipes).
//     Loopback multicast is never filtered by the OS, so this attempt is
//     deterministic. It must run on its own socket: a sending socket that
//     also targets a privacy-blocked physical interface loses even its
//     loopback copies (observed on macOS 15 — 0/5 vs 5/5).
//  2. LAN — the physical interfaces, for senders on other devices. When
//     the OS blocks multicast there (macOS 15 Local Network Privacy) this
//     attempt simply misses and the caller falls through to the
//     pairing-server path.
//
// Returns a QueryResult if found; otherwise an error (usually
// context.DeadlineExceeded — the caller should fall through to the
// pairing-server path).
//
// pion/mdns's QueryAddr blocks until it gets a response or the context
// expires, retrying on QueryInterval. Callers pass a short timeout
// (300 ms is fsend's default) so a LAN miss doesn't block the
// pairing-server fallback. The library runs on its own goroutine behind
// a watchdog: if it blows past the deadline (see watchdogGrace), Query
// closes the multicast sockets out from under it — which both unblocks
// its read loop and stops the query spam — and reports a miss.
func Query(ctx context.Context, code string, timeout time.Duration) (*QueryResult, error) {
	loop := func(ifc net.Interface, _ []net.Addr) bool {
		return ifc.Flags&net.FlagLoopback != 0
	}
	lan := func(ifc net.Interface, _ []net.Addr) bool {
		return ifc.Flags&net.FlagLoopback == 0
	}
	half := timeout / 2
	if res, err := queryOn(ctx, code, half, loop); err == nil {
		return res, nil
	}
	return queryOn(ctx, code, timeout-half, lan)
}

// QueryLoopback is Query restricted to loopback interfaces: it only ever
// finds a sender on this machine, but it is deterministic — no OS or AP
// can filter loopback multicast. Used by `fsend doctor` to separate "is
// fsend's mDNS machinery working" from "does multicast cross the
// physical network".
func QueryLoopback(ctx context.Context, code string, timeout time.Duration) (*QueryResult, error) {
	return queryOn(ctx, code, timeout, func(ifc net.Interface, _ []net.Addr) bool {
		return ifc.Flags&net.FlagLoopback != 0
	})
}

// QueryInterface is Query restricted to the interface that owns ip (the
// interface whose address list contains it). Used by `fsend doctor` to
// test the physical LAN path in isolation: loopback multicast always
// works, so only a scoped query tells the truth about the Wi-Fi/ethernet
// interface — macOS 15's Local Network Privacy and some APs silently
// block or stall multicast there.
func QueryInterface(ctx context.Context, code string, timeout time.Duration, ip net.IP) (*QueryResult, error) {
	return queryOn(ctx, code, timeout, func(ifc net.Interface, addrs []net.Addr) bool {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.Contains(ip) {
				return true
			}
		}
		return false
	})
}

func queryOn(ctx context.Context, code string, timeout time.Duration, extra func(net.Interface, []net.Addr) bool) (*QueryResult, error) {
	v4Conn, v6Conn, err := openMulticast()
	if err != nil {
		return nil, fmt.Errorf("landisc: opening multicast: %w", err)
	}

	qCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// pion/mdns doesn't expose service-browsing — only name queries. So
	// the port is not part of the announcement: both sides derive it
	// from the code via PortForCode, and we just query the well-known
	// service name to discover the sender's IP.
	port := PortForCode(code)
	name := serviceName(code)

	ifaces := discoveryInterfaces()
	if extra != nil {
		kept := ifaces[:0]
		for _, ifc := range ifaces {
			addrs, err := ifc.Addrs()
			if err != nil {
				continue
			}
			if extra(ifc, addrs) {
				kept = append(kept, ifc)
			}
		}
		ifaces = kept
	}
	if len(ifaces) == 0 {
		closePacketConns(v4Conn, v6Conn)
		return nil, fmt.Errorf("landisc: query: no usable interface for %s", name)
	}

	type outcome struct {
		addr netip.Addr
		err  error
	}
	ch := make(chan outcome, 1)
	// connCh hands the mDNS conn to the watchdog as soon as it exists, so
	// a wedged query (pion parks in a deadline-less interface write) can
	// be fully torn down: conn.Close unblocks QueryAddr and kills the
	// question loop, instead of leaking a goroutine that spams the LAN
	// until process exit.
	connCh := make(chan *mdns.Conn, 1)
	go func() {
		conn, err := mdns.NewServer(v4Conn, v6Conn,
			mdns.WithName("fsend-receiver"),
			mdns.WithIncludeLoopback(true),
			mdns.WithInterfaces(ifaces...),
			// Legacy parity: answer only A/AAAA, no proactive cache refresh.
			mdns.WithRecordTypes(dnsmessage.TypeA, dnsmessage.TypeAAAA),
			mdns.WithCacheRefresh(false),
		)
		if err != nil {
			closePacketConns(v4Conn, v6Conn)
			ch <- outcome{err: fmt.Errorf("landisc: mdns server: %w", err)}
			return
		}
		connCh <- conn

		// pion/mdns v2.2.0 has no functional option for the query retry
		// interval (the deprecated Server shim read it from Config;
		// NewServer pins it to 1s). Preserve the 100ms cadence by
		// re-issuing QueryAddr with fresh sub-contexts — each call sends
		// one question immediately, so this behaves exactly like the old
		// 100ms ticker. Loop only while the miss came from the sub-window
		// itself: any other error, or the caller's window closing, ends
		// the attempt.
		var addr netip.Addr
		var qerr error
		for {
			sub, subCancel := context.WithTimeout(qCtx, queryRetry)
			_, addr, qerr = conn.QueryAddr(sub, name)
			subCancel()
			if qerr == nil || qCtx.Err() != nil || sub.Err() == nil {
				break
			}
		}

		// Deliver the result before Close: a wedged Close leaks this
		// goroutine until process exit but can no longer hang the caller.
		ch <- outcome{addr: addr, err: qerr}
		_ = conn.Close()
		closePacketConns(v4Conn, v6Conn)
	}()

	watchdog := time.NewTimer(timeout + watchdogGrace)
	defer watchdog.Stop()
	select {
	case o := <-ch:
		if o.err != nil {
			return nil, fmt.Errorf("landisc: query: %w", o.err)
		}
		ip := net.IP(o.addr.AsSlice())
		// mDNS is link-scoped multicast, so a legitimate sender's address
		// must sit on one of this machine's own subnets. Anything else is
		// a spoofed answer steering us at an arbitrary third party — drop
		// it and let the caller fall through to the pairing-server path.
		if !o.addr.IsValid() || !onLinkSubnet(ip) {
			return nil, fmt.Errorf("landisc: query: answer %v is not on-link", o.addr)
		}
		return &QueryResult{IP: ip, Port: port}, nil
	case <-ctx.Done():
		// Caller is shutting down (e.g. SIGINT). Don't wait out the
		// watchdog — kill the sockets so the query goroutine unblocks.
		select {
		case c := <-connCh:
			go func() { _ = c.Close() }()
		default:
		}
		closePacketConns(v4Conn, v6Conn)
		return nil, fmt.Errorf("landisc: query: %w", ctx.Err())
	case <-watchdog.C:
		select {
		case c := <-connCh:
			go func() { _ = c.Close() }()
		default:
		}
		closePacketConns(v4Conn, v6Conn)
		return nil, fmt.Errorf("landisc: query stuck past its %v deadline (watchdog)", timeout)
	}
}

// PortForCode deterministically derives a UDP port from the code so that
// the receiver knows which port to dial without a second mDNS round-trip.
//
// We hash the code into the 50000-50999 range (1000 ports, far enough
// above 49152 to avoid OS ephemeral port conflicts on most systems).
//
// Collisions across two concurrent sessions on the same LAN are rare
// (1-in-1000). When they happen, the second sender's bind fails and the
// LAN path returns ErrLANListenerFailed; the internet path keeps running
// (sendpair.go always races the two) so the transfer still completes,
// just without the same-LAN shortcut. A proper fix would require mDNS
// service-browsing, which pion/mdns doesn't expose today.
func PortForCode(code string) int {
	return 50000 + int(binary.BigEndian.Uint16(derive(code)[16:18]))%1000
}

// openMulticast creates the IPv4 and IPv6 multicast packet conns mDNS needs.
// On systems without IPv6, v6 will be nil but mDNS handles that.
func openMulticast() (*ipv4.PacketConn, *ipv6.PacketConn, error) {
	addr4, err := net.ResolveUDPAddr("udp4", mdns.DefaultAddressIPv4)
	if err != nil {
		return nil, nil, err
	}
	l4, err := net.ListenUDP("udp4", addr4)
	if err != nil {
		return nil, nil, fmt.Errorf("listen v4: %w", err)
	}
	v4 := ipv4.NewPacketConn(l4)

	// IPv6 is best-effort; if we can't bind, just go v4-only.
	var v6 *ipv6.PacketConn
	addr6, err := net.ResolveUDPAddr("udp6", mdns.DefaultAddressIPv6)
	if err == nil {
		l6, err6 := net.ListenUDP("udp6", addr6)
		if err6 == nil {
			v6 = ipv6.NewPacketConn(l6)
		}
	}
	return v4, v6, nil
}

// virtualIfacePrefixes are the name prefixes of VPN tunnels and container
// bridges: macOS (utun*), WireGuard (wg*, tun*), Tailscale, OpenVPN
// (tun*/tap*), ZeroTier, IPsec, PPP, and Linux bridges (docker*, virbr*,
// veth*, br-*).
var virtualIfacePrefixes = []string{
	"utun", "tun", "tap", "wg", "tailscale", "zt", "ipsec", "ppp",
	"docker", "virbr", "veth", "br-",
}

// virtualIface reports whether the interface looks like a VPN tunnel or
// container bridge rather than LAN hardware. Announcing such an address
// would hand receivers an IP they cannot reach, silently losing the LAN
// shortcut. See virtualIfacePrefixes.
func virtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// onLinkSubnet reports whether ip falls within one of this machine's own
// interface subnets (loopback included). Used to vet mDNS answers, which
// are link-scoped by construction.
func onLinkSubnet(ip net.IP) bool {
	if ip == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// PreferredLocalIP returns the best IPv4 address to publish for LAN
// discovery, or a global-unicast IPv6 on IPv6-only networks.
//
// Selection, in order: the first private IPv4 (RFC 1918) on a physical
// interface, then any other IPv4 (public or CGNAT), then a global-unicast
// IPv6. Loopback, link-local v6, and virtual interfaces (VPN tunnels,
// container bridges) are skipped — see virtualIface.
//
// Falls back to 127.0.0.1 if nothing else is available — useful for
// loopback-only test environments.
func PreferredLocalIP() net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return net.IPv4(127, 0, 0, 1)
	}
	var v4, v4Any, v6 net.IP
	for i := range ifaces {
		if virtualIface(ifaces[i].Name) {
			continue
		}
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			if v := ipnet.IP.To4(); v != nil {
				if v4 == nil && v.IsPrivate() {
					v4 = v
				}
				if v4Any == nil {
					v4Any = v
				}
				continue
			}
			if v6 == nil && ipnet.IP.IsGlobalUnicast() && !ipnet.IP.IsLinkLocalUnicast() {
				v6 = ipnet.IP
			}
		}
	}
	if v4 != nil {
		return v4
	}
	if v4Any != nil {
		return v4Any
	}
	if v6 != nil {
		return v6
	}
	return net.IPv4(127, 0, 0, 1)
}
