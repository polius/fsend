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
