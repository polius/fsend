package quicconn

import (
	"encoding/binary"
	"testing"

	"github.com/pion/stun/v3"
)

// shortHeaderPacket builds a minimal QUIC 1-RTT (short-header) datagram
// carrying dcid as its destination connection ID: [flags][DCID][padding].
// stun.IsMessage only looks at len>=20 and bytes[4:8], so the padding's
// content is irrelevant.
func shortHeaderPacket(dcid []byte) []byte {
	pkt := make([]byte, 1200)
	pkt[0] = 0x40 // short header, fixed bit set; top two bits = 01
	copy(pkt[1:], dcid)
	return pkt
}

// TestGeneratedConnIDsNeverLookLikeSTUN is the end-to-end property test:
// every connection ID our generator issues, when placed as the DCID of a
// short-header packet, must be invisible to pion's stun.IsMessage. If this
// holds, ice.Conn.Write can never reject a 1-RTT data packet as STUN.
func TestGeneratedConnIDsNeverLookLikeSTUN(t *testing.T) {
	var gen stunSafeConnIDGenerator
	for i := 0; i < 200_000; i++ {
		id, err := gen.GenerateConnectionID()
		if err != nil {
			t.Fatalf("GenerateConnectionID: %v", err)
		}
		b := id.Bytes()
		if len(b) != connIDLen {
			t.Fatalf("connID length = %d, want %d", len(b), connIDLen)
		}
		if stun.IsMessage(shortHeaderPacket(b)) {
			t.Fatalf("generated connID %x produces a STUN-looking packet", b)
		}
	}
}

// TestSentinelControl guards the test itself: a connID deliberately
// carrying the magic cookie at offset 3..6 *must* make stun.IsMessage
// fire. Without this, the test above could pass vacuously (e.g. if the
// packet builder were wrong) and hide a real regression.
func TestSentinelControl(t *testing.T) {
	bad := make([]byte, connIDLen)
	binary.BigEndian.PutUint32(bad[3:7], stunMagicCookie)
	if !placesMagicCookie(bad) {
		t.Fatal("placesMagicCookie should flag a cookie at offset 3..7")
	}
	if !stun.IsMessage(shortHeaderPacket(bad)) {
		t.Fatal("control packet must be seen as STUN; packet builder is wrong")
	}
}

func TestConnectionIDLen(t *testing.T) {
	if got := (stunSafeConnIDGenerator{}).ConnectionIDLen(); got != connIDLen {
		t.Fatalf("ConnectionIDLen = %d, want %d", got, connIDLen)
	}
	if connIDLen < 7 {
		t.Fatalf("connIDLen must be >=7 so bytes 4..7 fall inside the DCID; got %d", connIDLen)
	}
}

func TestNewTransportWiresGenerator(t *testing.T) {
	tr := NewTransport(nil)
	if tr.ConnectionIDGenerator == nil {
		t.Fatal("NewTransport must set the STUN-safe ConnectionIDGenerator")
	}
	if _, ok := tr.ConnectionIDGenerator.(stunSafeConnIDGenerator); !ok {
		t.Fatalf("ConnectionIDGenerator = %T, want stunSafeConnIDGenerator", tr.ConnectionIDGenerator)
	}
}
