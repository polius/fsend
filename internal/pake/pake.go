// Package pake implements CPace (draft-irtf-cfrg-cpace-21), the balanced
// PAKE selected by the IRTF CFRG, with the CPACE-X25519-SHA512 suite.
//
// Both peers feed the same low-entropy secret (PRS) in; after exchanging
// one 32-byte share each, both derive the same 64-byte intermediate session
// key (ISK). An active attacker gets one online guess per run; a passive
// one learns nothing about the secret. The secret never crosses the wire.
//
// Building blocks are all constant-time and maintained upstream: X25519
// comes from the standard library (crypto/ecdh), and the Elligator2 map to
// the curve uses filippo.io/edwards25519/field (the field package the Go
// standard library itself vendors). Peer shares are fixed-length and
// low-order points abort the run, so malformed input is an error, never a
// panic.
package pake

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"

	"filippo.io/edwards25519/field"
)

// ShareSize is the length of one CPace share (an X25519 u-coordinate).
const ShareSize = 32

// KeySize is the length of the derived ISK (SHA-512 output).
const KeySize = 64

// dsi is the G_X25519 domain-separation identifier.
const dsi = "CPace255"

// sha512BlockSize is H.s_in_bytes for SHA-512.
const sha512BlockSize = 128

// ErrInvalidShare is returned when the peer's share is malformed or encodes
// a low-order point (the run must abort).
var ErrInvalidShare = errors.New("pake: invalid peer share")

// Role fixes the transcript order. CPace's initiator-responder setting is
// used: fsend always knows who is the sender.
type Role uint8

const (
	// Initiator is party A (fsend's sender).
	Initiator Role = iota
	// Responder is party B (fsend's receiver).
	Responder
)

// Params are the session inputs both peers must agree on. PRS is the
// secret; CI and SID bind the run to a channel and session (either may be
// empty, though fsend always sets SID to the TLS exporter); AD is this
// side's associated data, sent in clear inside the transcript.
type Params struct {
	PRS []byte
	CI  []byte
	SID []byte
	AD  []byte
}

// State is one CPace run. Create it with New, send Share() to the peer,
// then call Finish with the peer's share and associated data.
type State struct {
	role   Role
	sid    []byte
	ad     []byte
	priv   *ecdh.PrivateKey
	share  []byte
	usedUp bool
}

// New samples a fresh scalar and computes this side's share.
func New(r Role, p Params) (*State, error) {
	return newWithScalar(r, p, nil)
}

// newWithScalar lets tests inject the draft's fixed scalars.
func newWithScalar(r Role, p Params, scalar []byte) (*State, error) {
	if scalar == nil {
		scalar = make([]byte, ShareSize)
		if _, err := io.ReadFull(rand.Reader, scalar); err != nil {
			return nil, fmt.Errorf("pake: sample scalar: %w", err)
		}
	}
	priv, err := ecdh.X25519().NewPrivateKey(scalar)
	if err != nil {
		return nil, fmt.Errorf("pake: scalar: %w", err)
	}
	g := calculateGenerator(p.PRS, p.CI, p.SID)
	share, err := scalarMult(priv, g)
	if err != nil {
		// Only possible if the generator itself is low-order, which the
		// Elligator2 map makes negligible; treat it as a failed run.
		return nil, err
	}
	return &State{role: r, sid: clone(p.SID), ad: clone(p.AD), priv: priv, share: share}, nil
}

// Share returns the 32-byte share to send to the peer.
func (s *State) Share() []byte { return clone(s.share) }

// Finish combines the peer's share and associated data into the ISK. It
// returns ErrInvalidShare for a wrong-length share or a low-order point.
// A State can be finished only once.
func (s *State) Finish(peerShare, peerAD []byte) ([]byte, error) {
	if s.usedUp {
		return nil, errors.New("pake: state already finished")
	}
	s.usedUp = true
	if len(peerShare) != ShareSize {
		return nil, ErrInvalidShare
	}
	k, err := scalarMult(s.priv, peerShare)
	if err != nil {
		return nil, ErrInvalidShare
	}
	ya, ada, yb, adb := s.share, s.ad, peerShare, peerAD
	if s.role == Responder {
		ya, ada, yb, adb = peerShare, peerAD, s.share, s.ad
	}
	h := sha512.New()
	h.Write(lvCat([]byte(dsi+"_ISK"), s.sid, k))
	h.Write(transcriptIR(ya, ada, yb, adb))
	return h.Sum(nil), nil
}

// scalarMult is G_X25519.scalar_mult_vfy: X25519(y, u), aborting on the
// neutral element. crypto/ecdh clears bit 255 of u (RFC 7748) and rejects
// an all-zero result in constant time.
func scalarMult(priv *ecdh.PrivateKey, u []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(u)
	if err != nil {
		return nil, ErrInvalidShare
	}
	out, err := priv.ECDH(pub)
	if err != nil {
		return nil, ErrInvalidShare
	}
	return out, nil
}

// calculateGenerator hashes the generator string and maps it to a curve
// point with Elligator2 (RFC 9380, Z = 2). SHA-512 is the suite's mandated
// hash-to-curve step, not password storage: a PAKE run leaks nothing to
// test guesses against offline, so a slow KDF would add no protection.
func calculateGenerator(prs, ci, sid []byte) []byte {
	sum := sha512.Sum512(generatorString(prs, ci, sid))
	return elligator2(sum[:ShareSize])
}

func generatorString(prs, ci, sid []byte) []byte {
	zpad := sha512BlockSize - 1 - len(prependLen(prs)) - len(prependLen([]byte(dsi)))
	if zpad < 0 {
		zpad = 0
	}
	return lvCat([]byte(dsi), prs, make([]byte, zpad), ci, sid)
}

// curveA is the Montgomery A coefficient of Curve25519 (486662).
var curveA = new(field.Element).Mult32(new(field.Element).One(), 486662)

// elligator2 maps a 32-byte string to the u-coordinate of a Curve25519
// point: v = -A/(1+2r²); u = v if v³+Av²+v is square, else -v-A.
func elligator2(b []byte) []byte {
	var buf [32]byte
	copy(buf[:], b)
	buf[31] &= 0x7f // decodeUCoordinate: drop bit 255
	r, err := new(field.Element).SetBytes(buf[:])
	if err != nil {
		panic("pake: field decode of 32 bytes failed") // unreachable: length is fixed
	}
	one := new(field.Element).One()

	den := new(field.Element).Square(r)
	den.Add(den, den) // 2r²
	den.Add(den, one) // 1 + 2r²
	den.Invert(den)   // 1/(1+2r²); never zero since -1/2 is a non-square
	v := new(field.Element).Multiply(curveA, den)
	v.Negate(v) // -A/(1+2r²)

	// gx = v³ + A·v² + v = v·(v·(v + A) + 1)
	gx := new(field.Element).Add(v, curveA)
	gx.Multiply(gx, v)
	gx.Add(gx, one)
	gx.Multiply(gx, v)
	_, isSquare := new(field.Element).SqrtRatio(gx, one)

	alt := new(field.Element).Negate(v)
	alt.Subtract(alt, curveA) // -v - A
	u := new(field.Element).Select(v, alt, isSquare)
	return u.Bytes()
}

// prependLen prefixes data with its LEB128-encoded length.
func prependLen(data []byte) []byte {
	n := len(data)
	var out []byte
	for {
		if n < 128 {
			out = append(out, byte(n))
		} else {
			out = append(out, byte(n&0x7f)|0x80)
		}
		n >>= 7
		if n == 0 {
			break
		}
	}
	return append(out, data...)
}

func lvCat(args ...[]byte) []byte {
	var out []byte
	for _, a := range args {
		out = append(out, prependLen(a)...)
	}
	return out
}

func transcriptIR(ya, ada, yb, adb []byte) []byte {
	return append(lvCat(ya, ada), lvCat(yb, adb)...)
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }
