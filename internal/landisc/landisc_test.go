package landisc

import (
	"context"
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
