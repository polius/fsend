//go:build !linux

package relay

import "net"

// newBatchConn is nil off Linux: x/net reads and writes one datagram per
// batch call there (and not at all on Windows), so the plain loop is as
// good.
func newBatchConn(net.PacketConn) batchConn { return nil }
