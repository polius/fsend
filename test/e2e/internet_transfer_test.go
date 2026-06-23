package e2e

import (
	"path/filepath"
	"testing"
)

// TestInternet_Transfer drives real transfers over the two internet paths
// that build their quic.Transport via quicconn.NewTransport — and thus
// exercise the STUN-safe connection-ID generator on both ends:
//
//   - --mode=direct: the ICE path, where the original STUN-demux collision
//     occurred. ICE establishes over loopback host candidates here.
//   - --mode=relay:  the server-relayed path.
//
// The LAN happy-path tests use quicconn.ListenAddr/Dial and never touch
// that wiring, so without these cases the suite would not prove the
// internet paths still transfer end-to-end after the change. Payloads are
// random (incompressible) so the wire moves the full byte count rather
// than a zstd-collapsed stub.
func TestInternet_Transfer(t *testing.T) {
	requireE2E(t)
	for _, mode := range []string{"direct", "relay"} {
		t.Run(mode, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			payload := filepath.Join(src, "payload.bin")
			writeRandom(t, payload, 8*1024*1024)

			r := h.runPair(t,
				[]string{"--mode=" + mode, payload}, dst, []string{"--yes"}, "")
			r.requireSuccess(t)

			assertFilesEqual(t, payload, filepath.Join(dst, "payload.bin"))
		})
	}
}
