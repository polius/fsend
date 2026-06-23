package quicconn

import (
	"crypto/rand"
	"encoding/binary"
	"net"

	"github.com/quic-go/quic-go"
)

// stunMagicCookie is the 32-bit constant every STUN message carries at
// byte offset 4 (RFC 5389 §6).
//
// pion/ice multiplexes STUN connectivity checks and application data on a
// single socket and tells them apart with stun.IsMessage, which checks
// *only* len>=20 && bytes[4:8]==cookie — it ignores the QUIC header bits.
// So any QUIC datagram whose bytes 4..7 happen to equal this value is
// misread as a stray STUN packet, and ice.Conn.Write rejects it with
// "failed to write STUN message to ICE connection", tearing the
// connection down mid-transfer. The per-packet odds are ~2^-32, but over
// the tens of millions of datagrams in a multi-GB transfer that becomes a
// real, if rare, failure.
const stunMagicCookie = 0x2112A442

// connIDLen is the length of the connection IDs we issue. A QUIC
// short-header (1-RTT) packet is [flags:1][DCID:connIDLen][...], so packet
// bytes 4..7 — the exact span stun.IsMessage inspects — fall wholly inside
// the destination connection ID once connIDLen >= 7. We use 8 (vs
// quic-go's default 4) so those four bytes live in a stable field we
// control, letting GenerateConnectionID guarantee they never spell the
// cookie. The four extra header bytes per packet are negligible.
const connIDLen = 8

// placesMagicCookie reports whether a packet carrying id as its
// destination connection ID would expose the STUN magic cookie at the
// bytes stun.IsMessage checks — packet offset 4..7, which is id[3:7].
func placesMagicCookie(id []byte) bool {
	return len(id) >= 7 && binary.BigEndian.Uint32(id[3:7]) == stunMagicCookie
}

// stunSafeConnIDGenerator issues random connection IDs that can never put
// the STUN magic cookie at packet offset 4..7, so a 1-RTT data packet can
// never be mistaken for STUN by pion's demux.
//
// pion's write guard fires on the *sending* side, and the packet carries
// the *peer's* connection ID as its destination — so our generator only
// protects writes aimed at a peer that also issues STUN-safe IDs. When
// both peers run this code (the common case) the collision is removed in
// both directions at the source. Against an un-upgraded peer (or on the
// consumable-input path, which gets a single attempt), the retry
// classifier treating the write error as transient is the load-bearing
// fallback. See internal/retry.IsTransient.
type stunSafeConnIDGenerator struct{}

func (stunSafeConnIDGenerator) GenerateConnectionID() (quic.ConnectionID, error) {
	var b [connIDLen]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return quic.ConnectionID{}, err
		}
		// Re-roll on the ~2^-32 chance of a collision; in practice this
		// loop never iterates a second time.
		if !placesMagicCookie(b[:]) {
			return quic.ConnectionIDFromBytes(b[:]), nil
		}
	}
}

func (stunSafeConnIDGenerator) ConnectionIDLen() int { return connIDLen }

// NewTransport builds the quic.Transport used on the internet paths
// (direct-over-ICE and relay), wiring in the STUN-safe connection-ID
// generator. Centralized so the sender and receiver construction sites
// can't drift. The generator only matters on the ICE path; on the relay
// path it is simply a harmless longer connection ID.
func NewTransport(pc net.PacketConn) *quic.Transport {
	return &quic.Transport{
		Conn:                  pc,
		ConnectionIDGenerator: stunSafeConnIDGenerator{},
	}
}
