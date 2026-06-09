package landisc

import (
	"strings"
	"testing"
)

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
