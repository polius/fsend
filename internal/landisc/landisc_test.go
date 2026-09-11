package landisc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestQuery_MissIsBounded pins the watchdog contract: with no sender
// announcing the code, Query must report a miss within timeout +
// watchdogGrace — never hang. A wedged pion/mdns query used to block
// forever (and keep multicasting, poisoning later receivers); callers
// rely on a prompt miss to fall through to the pairing-server path.
func TestQuery_MissIsBounded(t *testing.T) {
	const timeout = 300 * time.Millisecond
	start := time.Now()
	res, err := Query(context.Background(), "zzz-wdog-tst", timeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Query found a sender for a code nobody announces: %+v", res)
	}
	// Generous bound: timeout + watchdogGrace plus slack for -race CI.
	if limit := timeout + watchdogGrace + 2*time.Second; elapsed > limit {
		t.Errorf("Query took %v, want < %v", elapsed, limit)
	}
}

// TestQuery_CancelledContext documents shutdown behavior: a cancelled
// context must surface as a prompt error, not wait out the watchdog —
// SIGINT during the LAN probe should not stall the exit path.
func TestQuery_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := Query(ctx, "zzz-wdog-tst", 300*time.Millisecond); err == nil {
		t.Fatal("Query with cancelled context returned no error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancelled Query took %v, want < 1s", elapsed)
	}
}

// TestAnnounceQuery_Roundtrip exercises the LAN-discovery path on the
// loopback-capable host: announce under a code-derived name, then query
// for it and get our own address back (pion/mdns enables multicast
// loopback, so a same-host pair converse). Multicast itself drops frames
// capriciously (Wi-Fi APs especially), so the query is retried; the
// contract under test is that *a legitimate answer passes validation* and
// carries the announced address — an over-strict check here would
// silently kill LAN discovery. A miss after all retries is an
// environment limitation, not a code regression, so it skips.
func TestAnnounceQuery_Roundtrip(t *testing.T) {
	ip := PreferredLocalIP()
	ann, err := Announce("round-trip-chk", ip)
	if err != nil {
		t.Skipf("multicast unavailable on this host: %v", err)
	}
	defer StopAnnounce(ann)

	// Give the announcer a beat to join the multicast group before asking.
	time.Sleep(200 * time.Millisecond)

	var res *QueryResult
	for attempt := 0; attempt < 3 && res == nil; attempt++ {
		res, err = Query(context.Background(), "round-trip-chk", 2*time.Second)
	}
	if res == nil {
		t.Skipf("no mDNS answer after 3 queries (%v) — multicast dropped on this host", err)
	}
	if !res.IP.Equal(ip) {
		t.Errorf("Query returned %v, want the announced %v", res.IP, ip)
	}
	if res.Port != PortForCode("round-trip-chk") {
		t.Errorf("Query port = %d, want %d", res.Port, PortForCode("round-trip-chk"))
	}
}

// TestPortForCode_Deterministic locks in the property both peers depend
// on: same code derives the same UDP port, so the receiver knows where
// to dial without a second mDNS round-trip.
func TestPortForCode_Deterministic(t *testing.T) {
	const code = "abc-defg-jkm"
	a, b := PortForCode(code), PortForCode(code)
	if a != b {
		t.Errorf("PortForCode is not deterministic: %d vs %d", a, b)
	}
}

// TestStopAnnounce_BoundedOnWedgedClose pins the watchdog: a Close that
// never returns (a wedged pion/mdns) must not hang the caller past the grace.
func TestStopAnnounce_BoundedOnWedgedClose(t *testing.T) {
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) }) // release the abandoned goroutine
	start := time.Now()
	StopAnnounce(blockingCloser{unblock})
	elapsed := time.Since(start)
	if elapsed < watchdogGrace {
		t.Errorf("returned in %v, before the %v grace — watchdog skipped?", elapsed, watchdogGrace)
	}
	if limit := watchdogGrace + 2*time.Second; elapsed > limit {
		t.Errorf("took %v, want < %v", elapsed, limit)
	}
}

// TestStopAnnounce_FastPath: a Close that returns promptly must not wait out
// the grace (the common case — the watchdog is only for a wedge).
func TestStopAnnounce_FastPath(t *testing.T) {
	start := time.Now()
	StopAnnounce(nopCloser{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("blocked %v on a fast Close, want < 1s", elapsed)
	}
}

type blockingCloser struct{ ch chan struct{} }

func (b blockingCloser) Close() error { <-b.ch; return nil }

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// TestPortForCode_InRange guards the 50000–50999 window the package
// promises: the lower bound stays clear of the OS ephemeral range on
// most systems, and the upper bound stops the hash from wrapping into
// unallocated territory.
func TestPortForCode_InRange(t *testing.T) {
	cases := []string{"", "a", "abc-defg-jkm", "ZZZ-zzzz-ZZZ", "0123456789", strings.Repeat("x", 64)}
	for _, c := range cases {
		p := PortForCode(c)
		if p < 50000 || p >= 51000 {
			t.Errorf("PortForCode(%q) = %d, want [50000, 51000)", c, p)
		}
	}
}

// TestServiceName keeps the announce/query naming aligned. A drift here
// would silently desync the two peers — they'd both run cleanly and
// never find each other. The code must NOT appear in the name: it is the
// PAKE secret and the name is multicast across the LAN.
func TestServiceName(t *testing.T) {
	const code = "abc-defg-jkm"
	got := serviceName(code)
	if !strings.HasSuffix(got, ".local") {
		t.Errorf("serviceName(%q) = %q; missing .local suffix", code, got)
	}
	if strings.Contains(got, code) {
		t.Errorf("serviceName(%q) = %q leaks the code in cleartext", code, got)
	}
	if got != serviceName(code) {
		t.Errorf("serviceName(%q) is not deterministic", code)
	}
}

// TestPreferredLocalIP documents the fallback contract: even on a
// loopback-only host (some CI sandboxes), the function returns a usable
// IPv4 address rather than nil — Announce relies on that.
func TestPreferredLocalIP(t *testing.T) {
	ip := PreferredLocalIP()
	if ip == nil {
		t.Fatal("PreferredLocalIP returned nil")
	}
	if ip.To4() == nil {
		t.Errorf("PreferredLocalIP returned non-IPv4: %v", ip)
	}
}

// TestVirtualIface pins the tunnel/bridge filter: VPN and container
// interfaces must be excluded from announcement, real hardware kept.
func TestVirtualIface(t *testing.T) {
	cases := map[string]bool{
		"utun0": true, "tun0": true, "tap9": true, "wg0": true,
		"tailscale0": true, "ztwyifhajk": true, "ipsec0": true, "ppp0": true,
		"docker0": true, "virbr1": true, "veth8a2b1c0": true, "br-4f2a": true,
		"en0": false, "eth0": false, "wlan0": false, "Ethernet 2": false,
		"enp5s0": false, "bridge0": false, "awdl0": false, "llw0": false,
	}
	for name, want := range cases {
		if got := virtualIface(name); got != want {
			t.Errorf("virtualIface(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestDiscoveryIface pins the mDNS interface filter: only interfaces a
// peer could live on (and fsend could dial) are kept. Beyond the virtual
// prefixes, Apple-only links (AWDL/AirDrop, its low-latency sibling, the
// network probe interfaces) and internet-sharing bridges are excluded,
// and an interface must carry an address to announce.
func TestDiscoveryIface(t *testing.T) {
	v4 := func(ip string) []net.Addr {
		return []net.Addr{&net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(24, 32)}}
	}
	up := net.FlagUp | net.FlagMulticast
	cases := []struct {
		name string
		ifc  net.Interface
		want bool
	}{
		{"en0 with LAN v4", net.Interface{Name: "en0", Flags: up}, true},
		{"loopback", net.Interface{Name: "lo0", Flags: up | net.FlagLoopback}, true},
		{"ethernet with LAN v4", net.Interface{Name: "enp5s0", Flags: up}, true},
		{"awdl (AirDrop)", net.Interface{Name: "awdl0", Flags: up}, false},
		{"llw (AWDL low-latency)", net.Interface{Name: "llw0", Flags: up}, false},
		{"anpi (Apple probe)", net.Interface{Name: "anpi0", Flags: up}, false},
		{"bridge (internet sharing)", net.Interface{Name: "bridge0", Flags: up}, false},
		{"utun (VPN)", net.Interface{Name: "utun3", Flags: up}, false},
		{"down interface", net.Interface{Name: "en0"}, false},
	}
	for _, tc := range cases {
		addrs := v4("192.168.1.115")
		if tc.name == "loopback" {
			addrs = v4("127.0.0.1")
		}
		if got := discoveryIface(tc.ifc, addrs); got != tc.want {
			t.Errorf("discoveryIface(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Without any address, an interface can't announce or be dialed —
	// only skip it (loopback in practice always carries 127.0.0.1, so
	// this only bites synthetic interfaces).
	if discoveryIface(net.Interface{Name: "en0", Flags: up}, nil) {
		t.Error("discoveryIface(en0, no addrs) = true, want false")
	}
}

// TestDiscoveryInterfaces_LoopbackFirst pins the ordering contract the
// query ladder depends on: loopback interfaces come first, so the
// copy that never gets filtered by the OS is already on the wire before
// any physical-interface write can stall behind Local Network Privacy.
func TestDiscoveryInterfaces_LoopbackFirst(t *testing.T) {
	ifaces := discoveryInterfaces()
	if len(ifaces) == 0 {
		t.Skip("no usable interfaces on this host")
	}
	for i, ifc := range ifaces {
		isLoop := ifc.Flags&net.FlagLoopback != 0
		if !isLoop {
			continue
		}
		for _, prev := range ifaces[:i] {
			if prev.Flags&net.FlagLoopback == 0 {
				t.Errorf("loopback %s listed after non-loopback %s", ifc.Name, prev.Name)
			}
		}
	}
}

// TestOnLinkSubnet vets the mDNS-answer filter: loopback (the loopback
// interface's own subnet) is on-link everywhere; TEST-NET-3 is reserved
// documentation space that can never be a local subnet.
func TestOnLinkSubnet(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1"} {
		if !onLinkSubnet(net.ParseIP(ip)) {
			t.Errorf("onLinkSubnet(%s) = false, want true", ip)
		}
	}
	for _, ip := range []string{"203.0.113.9", "198.51.100.7"} {
		if onLinkSubnet(net.ParseIP(ip)) {
			t.Errorf("onLinkSubnet(%s) = true, want false", ip)
		}
	}
	if onLinkSubnet(nil) {
		t.Error("onLinkSubnet(nil) = true, want false")
	}
}
