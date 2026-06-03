// Package pake defines the PAKE (password-authenticated key exchange)
// interface used by fsend.
//
// The interface is deliberately tiny so the implementation (currently
// SPAKE2 via gospake2) can be swapped without touching call sites. See
// docs/decisions/pake.md for the rationale.
//
// fsend uses the *symmetric* variant of SPAKE2: both peers play
// indistinguishable roles, with the short code as the shared low-entropy
// secret. After exchanging one message each, both sides derive the same
// 32-byte shared key.
//
// Usage from both sides (same code):
//
//	p := pake.New(code, sessionID)
//	myMsg, _ := p.Start()
//	// send myMsg to peer, receive peerMsg
//	key, err := p.Finish(peerMsg)
package pake

// KeySize is the length in bytes of the key derived by SPAKE2.
const KeySize = 32

// PAKE is the symmetric two-message key-exchange protocol.
//
// Both peers create an instance with identical (code, sessionID), each
// calls Start to produce a message, exchanges it with the peer, and then
// calls Finish with the peer's message to derive the shared key.
//
// Finish may only be called once per instance. Calling Start more than
// once is also undefined.
type PAKE interface {
	// Start returns the message to send to the peer.
	Start() ([]byte, error)

	// Finish consumes the peer's message and returns the derived key.
	// On any decoding / verification failure, returns a non-nil error and
	// no key (the caller MUST treat this as a protocol error and abort).
	Finish(peerMessage []byte) (key []byte, err error)
}

// New returns a fresh PAKE instance initialized with the given short code
// and a per-session identifier.
//
// sessionID is mixed into the SPAKE2 transcript so two sessions using the
// same code derive different keys — essential for replay resistance and
// for the threat-model property that a guessed code is only useful for the
// session it was issued for.
//
// In production, sessionID should be the random ULID issued by the
// rendezvous server.
var New = func(code, sessionID string) PAKE {
	return newSpake2Impl(code, sessionID)
}
