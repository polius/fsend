// Package relay implements fsend-server's UDP relay-fallback path AND
// the client-side framing/unframing used to send through it. The
// byte-level format is documented inline in this file.
package relay

import (
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
