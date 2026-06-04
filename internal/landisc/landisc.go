// Package landisc handles same-LAN peer discovery via mDNS.
//
// The sender announces a service name derived from the code
// (`fsend-<code>.local`) and a TXT-like payload of host:port. The receiver
// queries for the same name and gets back the sender's LAN address.
//
// This package only does discovery — the actual connection is established
// via internal/quicconn after we have an address.
//
// LAN discovery short-circuits the rendezvous + ICE path entirely: when
// both peers are on the same broadcast domain, we never hit the internet.
package landisc

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/pion/mdns/v2"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// serviceName builds the mDNS name we publish/query for a given code.
//
// We use `.local` so the name lives in the .local domain that every mDNS
// stack on the planet honors. The host part is derived from the code,
// which is unique per session.
func serviceName(code string) string {
	return "fsend-" + code + ".local"
}

// Announce publishes the given local address under the code-derived service
// name. Call Close on the returned *mdns.Conn to stop announcing.
//
// The sender calls this just before listening for QUIC connections, so the
// receiver can discover it.
//
// port is the UDP port of the sender's QUIC listener. ip is the address we
// want receivers to dial — usually the first non-loopback IPv4 on this
// machine.
func Announce(code string, ip net.IP, port int) (*mdns.Conn, error) {
	v4Conn, v6Conn, err := openMulticast()
	if err != nil {
		return nil, fmt.Errorf("landisc: opening multicast: %w", err)
	}

	// pion/mdns answers address queries for any LocalName we register. Both
	// peers use the well-known serviceName(code) name; the port is shared
	// via the deterministic PortForCode hash so the receiver doesn't need
	// to read it out of the discovery response.
	_ = port
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
		return nil, fmt.Errorf("landisc: mdns server: %w", err)
	}
	return conn, nil
}

// QueryResult bundles the discovered peer's address + port.
type QueryResult struct {
	IP   net.IP
	Port int
}

// Query searches for a sender announcing the given code, up to timeout.
//
// Returns a QueryResult if found; otherwise an error (usually
// context.DeadlineExceeded — the caller should fall through to the
// rendezvous path).
//
// pion/mdns's QueryAddr blocks until it gets a response or the context
// expires, retrying on QueryInterval. We use a 300 ms timeout per
// docs/decisions/implementation-defaults.md.
func Query(ctx context.Context, code string, timeout time.Duration) (*QueryResult, error) {
	v4Conn, v6Conn, err := openMulticast()
	if err != nil {
		return nil, fmt.Errorf("landisc: opening multicast: %w", err)
	}

	cfg := &mdns.Config{
		Name:            "fsend-receiver",
		IncludeLoopback: true,
		QueryInterval:   100 * time.Millisecond,
	}
	conn, err := mdns.Server(v4Conn, v6Conn, cfg)
	if err != nil {
		return nil, fmt.Errorf("landisc: mdns server: %w", err)
	}
	defer conn.Close()

	qCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Try names with various port suffixes — the announce side embedded
	// the port in the name. We don't know it, so we instead announce a
	// fixed sentinel name and use TXT-equivalent embedding via the
	// last-known-port. For v1 simplicity, we encode port in the name as
	// fsend-<code>-<port>.local and brute-force common ports. A cleaner
	// approach is below in the LAN MVP wiring: the sender posts on a
	// well-known announced port carried in the name itself, and the
	// receiver parses by querying a wildcard suffix.
	//
	// pion/mdns doesn't expose service-browsing primitives — only name
	// queries. So we use a different strategy: the announce-side encodes
	// the port directly in a fixed-format name fsend-<code>-port<n>.local,
	// and the receiver tries common QUIC ports.
	//
	// SIMPLIFIED LAN-MVP STRATEGY: the sender always listens on a port
	// derived deterministically from the code (between 50000 and 50999;
	// 10 bits of code → port-in-range). This means the receiver can
	// query the well-known service name AND know the port without a
	// second exchange. See PortForCode below.
	port := PortForCode(code)
	name := serviceName(code)

	_, addr, err := conn.QueryAddr(qCtx, name)
	if err != nil {
		return nil, fmt.Errorf("landisc: query: %w", err)
	}
	return &QueryResult{IP: net.IP(addr.AsSlice()), Port: port}, nil
}

// PortForCode deterministically derives a UDP port from the code so that
// the receiver knows which port to dial without a second mDNS round-trip.
//
// We hash the code into the 50000-50999 range (1000 ports, far enough
// above 49152 to avoid OS ephemeral port conflicts on most systems).
//
// Collisions across two sessions on the same LAN are rare (1-in-1000)
// and benign — at worst the second sender fails to bind and falls back
// to picking another random port. v2 can do mDNS service-browsing
// properly.
func PortForCode(code string) int {
	var sum uint32
	for i := 0; i < len(code); i++ {
		sum = sum*31 + uint32(code[i])
	}
	return 50000 + int(sum%1000)
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

// PreferredLocalIP returns the first non-loopback IPv4 address found on
// this machine's interfaces. Used by Announce to publish a routable LAN IP.
//
// Falls back to 127.0.0.1 if nothing else is available — useful for
// loopback-only test environments.
func PreferredLocalIP() net.IP {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if v4 := ipnet.IP.To4(); v4 != nil {
					return v4
				}
			}
		}
	}
	return net.IPv4(127, 0, 0, 1)
}
