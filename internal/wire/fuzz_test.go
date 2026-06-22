package wire

import (
	"bytes"
	"testing"
)

// FuzzReadControlRaw drives the control-frame parser with arbitrary bytes a
// malicious peer could send. The contract: reject with an error, never panic
// or hang. The seed corpus also runs during a normal `go test`.
func FuzzReadControlRaw(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{0x02})
	var buf bytes.Buffer
	if err := WriteControl(&buf, TypeHello, &SenderHello{ProtocolVersion: ProtocolVersion}); err == nil {
		f.Add(buf.Bytes())
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = ReadControlRaw(bytes.NewReader(data))
	})
}

// FuzzReadChunk drives the data-chunk parser the same way — this is the parser
// that runs once per chunk on every byte a peer sends.
func FuzzReadChunk(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	var buf bytes.Buffer
	seed := &Chunk{
		Segments: []Segment{{FileIndex: 0, Length: 3, EOF: true}},
		Payload:  []byte("abc"),
	}
	if err := WriteChunk(&buf, seed); err == nil {
		f.Add(buf.Bytes())
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadChunk(bytes.NewReader(data))
	})
}
