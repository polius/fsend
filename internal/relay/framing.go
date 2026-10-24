// Package relay implements the relay-fallback path used by `fsend server`
// (UDP, plus a WebSocket fallback for networks that block UDP) AND the
// client-side framing/unframing used to send through it. The byte-level
// format is documented inline in this file.
package relay

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
)

// HeaderSize is the per-datagram prefix: 1 byte version + 16 bytes token.
const HeaderSize = 17

// ProtocolVersion is the relay datagram version byte.
const ProtocolVersion = 0x01

// TokenSize is the fixed token length in bytes (128 bits).
const TokenSize = 16

// TokenEncoding is Crockford base32 (no padding) for the human-readable
// form used in HTTP responses.
var TokenEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").
	WithPadding(base32.NoPadding)

// Token is the 16-byte allocation token the server issues to clients.
type Token [TokenSize]byte

// String returns the Crockford-base32 representation (no padding, 26 chars).
func (t Token) String() string { return TokenEncoding.EncodeToString(t[:]) }

// ParseToken decodes a base32 token string back into bytes.
func ParseToken(s string) (Token, error) {
	var t Token
	b, err := TokenEncoding.DecodeString(s)
	if err != nil {
		return t, err
	}
	if len(b) != TokenSize {
		return t, errors.New("relay: token wrong length")
	}
	copy(t[:], b)
	return t, nil
}

// Frame prepends the relay header to payload, writing into out (which must
// be at least HeaderSize+len(payload) bytes). Returns the number of bytes
// written. Designed to avoid allocations in the hot path.
func Frame(out []byte, token Token, payload []byte) int {
	out[0] = ProtocolVersion
	copy(out[1:1+TokenSize], token[:])
	copy(out[HeaderSize:], payload)
	return HeaderSize + len(payload)
}

// Parse extracts version and token from datagram. Returns the inner
// payload as a sub-slice of datagram (no copy). Returns ok=false on
// short/malformed datagrams.
func Parse(datagram []byte) (version byte, token Token, payload []byte, ok bool) {
	if len(datagram) < HeaderSize {
		return 0, Token{}, nil, false
	}
	version = datagram[0]
	copy(token[:], datagram[1:1+TokenSize])
	payload = datagram[HeaderSize:]
	ok = true
	return
}

// MaxDatagram is the largest relay datagram (header included) the server
// accepts or forwards. fsend's QUIC packets stay under 1500 bytes; the
// headroom means a larger one is forwarded whole. Anything bigger is
// dropped and counted, never silently truncated.
const MaxDatagram = 2048

// Control frames carry the registration handshake, which proves a peer
// can receive at the address it registers, so a spoofed source can't aim
// the relay at a third party. Fixed layout:
//
//	[ControlVersion][token 16][type 1][peer id 8][cookie 16]
//
// Every control frame is ControlSize bytes, so a reply is never larger
// than the request that triggered it: no amplification.
const (
	ControlVersion = 0x02
	PeerIDSize     = 8
	cookieSize     = 16
	ControlSize    = HeaderSize + 1 + PeerIDSize + cookieSize
)

// Control frame types.
const (
	msgHello      byte = 1 // client → relay: start (or refresh) registration
	msgChallenge  byte = 2 // relay → client: echo this cookie from the same address
	msgRegister   byte = 3 // client → relay: cookie echo; records the source address
	msgRegistered byte = 4 // relay → client: address recorded
)

// PeerID names one side of an allocation across address changes, so the
// relay can re-learn a peer's address (NAT rebinding, a UDP→TCP switch)
// without anyone else being able to take over that side's slot.
type PeerID [PeerIDSize]byte

// PeerIDFor derives a stable, private PeerID from a per-side secret (the
// session role token): stable so a re-dial lands in the same slot,
// private because the other side never learns our role token.
func PeerIDFor(secret string) PeerID {
	sum := sha256.Sum256([]byte("fsend relay peer id\x00" + secret))
	var id PeerID
	copy(id[:], sum[:])
	return id
}

func frameControl(token Token, typ byte, id PeerID, cookie *[cookieSize]byte) []byte {
	out := make([]byte, ControlSize)
	out[0] = ControlVersion
	copy(out[1:HeaderSize], token[:])
	out[HeaderSize] = typ
	copy(out[HeaderSize+1:], id[:])
	if cookie != nil {
		copy(out[HeaderSize+1+PeerIDSize:], cookie[:])
	}
	return out
}

func parseControl(b []byte) (token Token, typ byte, id PeerID, cookie [cookieSize]byte, ok bool) {
	if len(b) != ControlSize || b[0] != ControlVersion {
		return token, 0, id, cookie, false
	}
	copy(token[:], b[1:HeaderSize])
	typ = b[HeaderSize]
	copy(id[:], b[HeaderSize+1:])
	copy(cookie[:], b[HeaderSize+1+PeerIDSize:])
	return token, typ, id, cookie, true
}
