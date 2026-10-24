// Package transfer implements fsend's end-to-end transfer protocol: HELLO
// negotiation, optional password gating, per-file metadata + acceptance,
// chunked data streaming with BLAKE3 verification, and resume from partials
// whose received prefix is verified against the source. Directories travel
// as per-file entries, not a tar.
//
// The Send and Recv entry points operate on a Streams pair (one control,
// one data) — production wires them to QUIC streams, tests wire them to
// in-memory io.Pipes.
package transfer

import "io"

// Streams is the minimal abstraction the transfer engine needs from its
// underlying transport. In production this is wired to QUIC streams. In
// tests we use io.Pipe pairs.
//
//   - Control: bidirectional, control frames (gob payloads).
//   - Data:    sender→receiver only, carries chunk frames.
type Streams struct {
	Control io.ReadWriteCloser
	Data    io.ReadWriteCloser
	// Binding is a secret only the two authenticated peers share (derived
	// from the code PAKE). In-session PAKEs such as --password bind to it.
	Binding []byte
}

// Close shuts both streams, swallowing individual close errors.
func (s *Streams) Close() {
	if s.Control != nil {
		_ = s.Control.Close()
	}
	if s.Data != nil {
		_ = s.Data.Close()
	}
}
