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
	"strings"
	"sync"
	"time"

	"github.com/pion/mdns/v2"
	"golang.org/x/crypto/argon2"
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

	cfg := &mdns.Config{
		Name:            "fsend-sender",
		LocalNames:      []string{name},
		LocalAddress:    ip,
		IncludeLoopback: true,
		QueryInterval:   time.Second,
	}
	conn, err := mdns.Server(v4Conn, v6Conn, cfg)
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

// Query searches for a sender announcing the given code, up to timeout.
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

	type outcome struct {
		addr netip.Addr
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		cfg := &mdns.Config{
			Name:            "fsend-receiver",
			IncludeLoopback: true,
			QueryInterval:   100 * time.Millisecond,
		}
		conn, err := mdns.Server(v4Conn, v6Conn, cfg)
		if err != nil {
			closePacketConns(v4Conn, v6Conn)
			ch <- outcome{err: fmt.Errorf("landisc: mdns server: %w", err)}
			return
		}
		_, addr, err := conn.QueryAddr(qCtx, name)
		// Deliver the result before Close: a wedged Close leaks this
		// goroutine until process exit but can no longer hang the caller.
		ch <- outcome{addr: addr, err: err}
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
		closePacketConns(v4Conn, v6Conn)
		return nil, fmt.Errorf("landisc: query: %w", ctx.Err())
	case <-watchdog.C:
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

// virtualIface reports whether the interface looks like a VPN tunnel or
// container bridge rather than LAN hardware. Announcing such an address
// would hand receivers an IP they cannot reach, silently losing the LAN
// shortcut. Prefix list covers macOS (utun*), WireGuard (wg*, tun*),
// Tailscale, OpenVPN (tun*/tap*), ZeroTier, and Linux bridges (docker*,
// virbr*, veth*, br-*).
func virtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"utun", "tun", "tap", "wg", "tailscale", "zt", "ipsec", "ppp", "docker", "virbr", "veth", "br-"} {
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
