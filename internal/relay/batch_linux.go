package relay

import (
	"net"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// newBatchConn gives the relay socket recvmmsg and sendmmsg: one system
// call reads or writes a whole batch of datagrams, where ReadFrom and
// WriteTo cost one each. Per-datagram system calls were most of the
// relay's CPU (73% on a 64 MiB transfer). nil when c isn't a UDP socket.
func newBatchConn(c net.PacketConn) batchConn {
	uc, ok := c.(*net.UDPConn)
	if !ok {
		return nil
	}
	if la, ok := uc.LocalAddr().(*net.UDPAddr); ok && la.IP.To4() == nil {
		return ipv6.NewPacketConn(uc) // also a dual-stack socket (":443")
	}
	return ipv4.NewPacketConn(uc)
}
