package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestInternet_RelayOverTCP drives a real relayed transfer with both peers
// on the relay's WebSocket (TCP) fallback, through the real server binary.
// Loopback can't block UDP, so FSEND_RELAY_TRANSPORT=tcp skips the UDP try.
func TestInternet_RelayOverTCP(t *testing.T) {
	requireE2E(t)
	t.Setenv("FSEND_RELAY_TRANSPORT", "tcp")
	src, dst := t.TempDir(), t.TempDir()
	payload := filepath.Join(src, "payload.bin")
	writeRandom(t, payload, 8*1024*1024)

	r := h.runPair(t, []string{"--mode=relay", payload}, dst, []string{"--yes"}, "")
	r.requireSuccess(t)
	assertFilesEqual(t, payload, filepath.Join(dst, "payload.bin"))
	if !strings.Contains(r.senderErr+r.receiverErr, "over TCP") {
		t.Errorf("transfer did not report the TCP relay path\n--- sender ---\n%s\n--- receiver ---\n%s", r.senderErr, r.receiverErr)
	}
}
