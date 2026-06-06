// Package pake wraps a symmetric SPAKE2 (RFC 9382) handshake.
//
// Both peers feed the same short code in; after exchanging one message
// each, both derive the same 32-byte key. The code itself never crosses
// the wire.
package pake

import (
	"fmt"

	gospake2 "salsa.debian.org/vasudev/gospake2"
)

// KeySize is the length of the derived shared key.
const KeySize = 32

// identity is the symmetric-SPAKE2 identity string. Both peers must use
// the same value; it scopes the protocol to fsend's namespace.
const identity = "fsend"

// PAKE is a one-shot symmetric SPAKE2 state. Call Start once, exchange
// the message with the peer, then call Finish with the peer's message
// to obtain the shared key.
type PAKE struct{ s gospake2.SPAKE2 }

// New constructs a fresh PAKE seeded with the shared short code.
func New(code string) *PAKE {
	return &PAKE{s: gospake2.SPAKE2Symmetric(
		gospake2.NewPassword(code),
		gospake2.NewIdentityS(identity),
	)}
}

// Start returns the message to send to the peer.
func (p *PAKE) Start() []byte { return p.s.Start() }

// Finish consumes the peer's message and returns the derived 32-byte key.
func (p *PAKE) Finish(peer []byte) ([]byte, error) {
	key, err := p.s.Finish(peer)
	if err != nil {
		return nil, fmt.Errorf("pake: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("pake: derived key length %d, want %d", len(key), KeySize)
	}
	return key, nil
}
