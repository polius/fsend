package e2e

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polius/fsend/internal/landisc"
)

// TestReceive_LANBlackholeFallsBackFast reproduces the Windows Defender
// failure mode: mDNS discovery works (Windows ships a built-in allow
// rule for UDP 5353) but the sender's LAN QUIC port silently drops
// inbound packets. The receiver's first LAN dial must be bounded at an
// RTT-scale budget and fall back to the server path — not burn the full
// 10 s HandshakeTimeout staring at a black hole.
func TestReceive_LANBlackholeFallsBackFast(t *testing.T) {
	requireE2E(t)

	xdg := h.newXDG(t)
	src := t.TempDir()
	payload := filepath.Join(src, "payload.bin")
	writeRandom(t, payload, 64*1024)

	// --mode direct: internet path only, so the mDNS announce below is
	// the only one for this code.
	s := h.startSender(t, xdg, "--mode", "direct", payload)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	// Blackhole the code-derived LAN port: bind it, read, drop.
	port := landisc.PortForCode(code)
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
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
	mdnsConn, err := landisc.Announce(code, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Skipf("mDNS announce unavailable: %v", err)
	}
	defer landisc.StopAnnounce(mdnsConn)

	recvDir := t.TempDir()
	recvCmd := h.fsendCmd(xdg, code, "--yes", "--debug", "--out", recvDir)
	var out, errb bytes.Buffer
	recvCmd.Stdout = &out
	recvCmd.Stderr = &errb
	start := time.Now()
	recvErr := recvCmd.Run()
	elapsed := time.Since(start)
	stderr := errb.String()

	// Without discovery the run never exercises the doomed dial and the
	// test would pass vacuously.
	if !strings.Contains(stderr, "sender address:") {
		t.Skipf("mDNS discovery did not resolve in this environment\n%s", stderr)
	}
	if code := exitCodeOf(t, recvErr); code != 0 {
		t.Fatalf("receiver exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "Local sender unreachable") {
		t.Fatalf("expected the LAN-dial fallback notice\n%s", stderr)
	}
	// Post-fix this completes in ~2-3 s; pre-fix the dial alone was 10 s.
	if elapsed > 8*time.Second {
		t.Fatalf("receive took %v; the first LAN dial must stay RTT-scale", elapsed)
	}
	if _, err := os.Stat(filepath.Join(recvDir, "payload.bin")); err != nil {
		t.Fatalf("payload not received: %v", err)
	}
}
