package relay

import (
	"net"
	"testing"
)

// discardConn is a net.PacketConn that accepts writes and never reads. It
// lets the relay's datagram handler run without a real socket.
type discardConn struct{ net.PacketConn }

func (discardConn) WriteTo([]byte, net.Addr) (int, error) { return 0, nil }

// FuzzHandleDatagram feeds arbitrary bytes at the relay's per-datagram
// demux/forward path. handleSafe recovers panics in production, but a panic
// is still a bug (it drops the datagram and logs), so this target asserts a
// crafted datagram can never panic handle or handleSTUN. A live allocation
// is seeded so datagrams carrying its token exercise the forward branches,
// not just the unknown-token early return.
func FuzzHandleDatagram(f *testing.F) {
	s := NewServer(discardConn{}, ServerConfig{})
	tok, err := s.Allocate()
	if err != nil {
		f.Fatal(err)
	}
	good := make([]byte, HeaderSize+4)
	Frame(good, tok, []byte("ping"))
	f.Add(good)
	f.Add([]byte{ProtocolVersion})
	f.Add([]byte{0x00, 0x00})             // truncated STUN header
	f.Add([]byte{0x01, 0x00, 0x00, 0x01}) // relay version, short frame
	f.Add([]byte{})

	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4242}
	f.Fuzz(func(t *testing.T, data []byte) {
		s.handle(data, src)
	})
}
