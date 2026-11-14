package e2e

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/polius/fsend/internal/landisc"

	codepkg "github.com/polius/fsend/internal/code"
)

// TestReceive_LANBlackholeFallsBackFast reproduces the Windows Defender
// failure mode: mDNS discovery works (Windows ships a built-in allow
// rule for UDP 5353) but the sender's LAN QUIC port silently drops
// inbound packets. The receiver's first LAN dial must be bounded at an
// RTT-scale budget and fall back to the server path — not burn the full
// 10 s HandshakeTimeout staring at a black hole.
func TestReceive_LANBlackholeFallsBackFast(t *testing.T) {
	requireE2E(t)

	// The announce and the blackhole sit on this machine's LAN address,
	// not loopback: loopback multicast is off on Linux by default, never
	// answers on Windows, and on the macos-15-intel runner missed the
	// receiver's 1.5 s query window three times in a row. A miss on the LAN
	// address still gets a fresh sender and receiver, up to three tries.
	var r blackholeRun
	for try := 1; ; try++ {
		r = blackholeAttempt(t)
		if strings.Contains(r.stderr, "sender address:") || try == 3 {
			break
		}
		t.Logf("try %d: mDNS discovery did not resolve; trying again", try)
	}

	stderr := r.stderr

	// Without discovery the run never exercises the doomed dial and the
	// test would pass vacuously.
	if !strings.Contains(stderr, "sender address:") {
		skipNoMulticast(t, "mDNS discovery did not resolve in this environment\n%s", stderr)
	}
	if code := exitCodeOf(t, r.err); code != 0 {
		t.Fatalf("receiver exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "Local sender unreachable") {
		t.Fatalf("expected the LAN-dial fallback notice\n%s", stderr)
	}
	// Post-fix this completes in ~2-3 s plus the 3 s held join; pre-fix the
	// dial alone was 10 s.
	if r.elapsed > 11*time.Second {
		t.Fatalf("receive took %v; the first LAN dial must stay RTT-scale", r.elapsed)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "payload.bin")); err != nil {
		t.Fatalf("payload not received: %v", err)
	}
}

// skipNoMulticast skips a test that needs working multicast, unless
// FSEND_TEST_REQUIRE_MULTICAST is set. CI sets it on the runners where
// multicast works, so LAN discovery can't pass there by not running.
func skipNoMulticast(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("FSEND_TEST_REQUIRE_MULTICAST") != "" {
		t.Fatalf("FSEND_TEST_REQUIRE_MULTICAST is set, but: "+format, args...)
	}
	t.Skipf(format, args...)
}

// delayJoins is a proxy to the harness server on port that holds every
// join for d. It returns the proxy's host:port.
func delayJoins(t *testing.T, port int, d time.Duration) string {
	t.Helper()
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/join") {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "http://")
}

// blackholeRun is what one attempt's receiver did.
type blackholeRun struct {
	stderr  string
	err     error
	elapsed time.Duration
	dir     string
}

// blackholeAttempt runs one sender, one blackholed LAN port and one
// receiver.
func blackholeAttempt(t *testing.T) blackholeRun {
	t.Helper()
	xdg := h.newXDG(t)
	src := t.TempDir()
	payload := filepath.Join(src, "payload.bin")
	writeRandom(t, payload, 64*1024)

	// --mode direct: internet path only, so the mDNS announce below is
	// the only one for this code.
	s := h.startSender(t, xdg, "--mode", "direct", payload)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	// Blackhole the code-derived LAN port on the LAN address: bind it,
	// read, drop.
	ip := landisc.PreferredLocalIP()
	if ip == nil || ip.IsLoopback() {
		skipNoMulticast(t, "no LAN address to announce on (%v)", ip)
	}
	port := landisc.PortForCode(code, codepkg.Epoch(time.Now()))
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		t.Skipf("cannot bind blackhole port %d: %v", port, err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, _, err := pc.ReadFromUDP(buf); err != nil {
				return
			}
		}
	}()
	mdnsConn, err := landisc.Announce(code, codepkg.Epoch(time.Now()), ip)
	if err != nil {
		skipNoMulticast(t, "mDNS announce unavailable: %v", err)
	}
	defer landisc.StopAnnounce(mdnsConn)

	// The receiver tries the local network and the server at once. Hold
	// its join at the server for a while so discovery always wins the
	// race: on a slow runner the server path used to win first (it found
	// a direct route over a real interface), and the doomed dial never ran.
	recvXDG := h.newXDGVia(t, delayJoins(t, h.httpPort, 3*time.Second))
	recvDir := t.TempDir()
	recvCmd := h.fsendCmd(recvXDG, code, "--yes", "--debug", "--out", recvDir)
	var out, errb bytes.Buffer
	recvCmd.Stdout = &out
	recvCmd.Stderr = &errb
	start := time.Now()
	recvErr := recvCmd.Run()
	elapsed := time.Since(start)
	return blackholeRun{stderr: errb.String(), err: recvErr, elapsed: elapsed, dir: recvDir}

}
