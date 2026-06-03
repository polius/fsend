package transfer

import "io"

// Streams is the minimal abstraction the transfer engine needs from its
// underlying transport. In production this is wired to QUIC streams. In
// tests we use io.Pipe pairs.
//
// The three streams correspond to the layout in
// docs/decisions/wire-protocol.md:
//
//   - Control: bidirectional, control frames (gob payloads).
//   - DataSend: write-only from the sender's perspective (the receiver
//     reads via DataRecv on its side).
//   - ReceiverControl: from the receiver to the sender for progress acks
//     and (v2) resume requests.
//
// On the sender side: DataSend is a writable stream; ReceiverControl is
// readable (sender consumes acks).
// On the receiver side: DataSend is readable (receiver consumes chunks);
// ReceiverControl is writable (receiver emits acks).
//
// Because Go's io.ReadWriteCloser is symmetric, we represent each stream
// as one, and the package callers can ignore the direction they don't need.
type Streams struct {
	Control         io.ReadWriteCloser
	Data            io.ReadWriteCloser
	ReceiverControl io.ReadWriteCloser
}

// Close shuts down all three streams, swallowing individual close errors.
func (s *Streams) Close() {
	if s.Control != nil {
		_ = s.Control.Close()
	}
	if s.Data != nil {
		_ = s.Data.Close()
	}
	if s.ReceiverControl != nil {
		_ = s.ReceiverControl.Close()
	}
}
