package relay

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// startRelayOn is startRelay on a socket from net.ListenPacket(network,
// addr), as `fsend server` opens it.
func startRelayOn(t *testing.T, network, addr string) (*Server, int) {
	t.Helper()
	conn, err := net.ListenPacket(network, addr)
	if err != nil {
		t.Skipf("listen %s %s: %v", network, addr, err)
	}
	srv := NewServer(conn, ServerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		<-done
	})
	return srv, conn.LocalAddr().(*net.UDPAddr).Port
}

// A dual-stack listener (the default ":443") forwards between IPv4
// peers: on Linux the batched writes carry IPv4 destinations on an IPv6
// socket.
func TestRelay_DualStackSocketForwardsIPv4(t *testing.T) {
	srv, port := startRelayOn(t, "udp", ":0")
	tok, err := srv.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	a, b := registered(t, relayAddr, tok), registered(t, relayAddr, tok)
	if _, err := a.WriteTo([]byte("a→b"), nil); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, b, []byte("a→b"))
	if _, err := b.WriteTo([]byte("b→a"), nil); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, a, []byte("b→a"))
}

// Bursts fill read batches (several datagrams per call): every datagram
// is forwarded once, none lost or repeated, in both directions at once.
// Bursts of 20 stay well inside the sockets' default buffers (the test
// doesn't size them like `fsend server` does), so a loss is the relay's.
func TestRelay_BurstForwardsEveryDatagram(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{})
	tok, err := srv.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	a, b := registered(t, relayAddr, tok), registered(t, relayAddr, tok)
	const n = 200
	recv := func(c *Conn, prefix string) <-chan error {
		errc := make(chan error, 1)
		go func() {
			seen := map[string]bool{}
			buf := make([]byte, MaxDatagram)
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			for len(seen) < n {
				k, _, err := c.ReadFrom(buf)
				if err != nil {
					errc <- fmt.Errorf("%s: got %d of %d: %w", prefix, len(seen), n, err)
					return
				}
				if s := string(buf[:k]); seen[s] {
					errc <- fmt.Errorf("%s: %q forwarded twice", prefix, s)
					return
				} else {
					seen[s] = true
				}
			}
			errc <- nil
		}()
		return errc
	}
	toB, toA := recv(b, "a→b"), recv(a, "b→a")
	for i := range n {
		if i%20 == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		if _, err := a.WriteTo(fmt.Appendf(nil, "a→b %03d", i), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := b.WriteTo(fmt.Appendf(nil, "b→a %03d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []<-chan error{toB, toA} {
		if err := <-c; err != nil {
			t.Fatal(err)
		}
	}
}
