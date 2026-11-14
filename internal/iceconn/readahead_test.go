package iceconn_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/iceconn"
)

// pairAgents runs ICE between two loopback agents and returns each side's
// PacketConn (sender = controlling).
func pairAgents(t *testing.T) (snd, rcv net.PacketConn) {
	t.Helper()
	const (
		sUfrag, sPwd = "Suuuu", "spppppppppppppppppppppppp"
		rUfrag, rPwd = "Ruuuu", "rpppppppppppppppppppppppp"
	)
	sa, err := iceconn.New(iceconn.Options{LocalUfrag: sUfrag, LocalPwd: sPwd, RemoteUfrag: rUfrag, RemotePwd: rPwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sa.Close() })
	ra, err := iceconn.New(iceconn.Options{LocalUfrag: rUfrag, LocalPwd: rPwd, RemoteUfrag: sUfrag, RemotePwd: sPwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ra.Close() })
	go func() {
		for c := range sa.LocalCandidates() {
			_ = ra.AddRemoteCandidate(c)
		}
	}()
	go func() {
		for c := range ra.LocalCandidates() {
			_ = sa.AddRemoteCandidate(c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var serr, rerr error
	wg.Add(2)
	go func() { defer wg.Done(); snd, serr = sa.Dial(ctx) }()
	go func() { defer wg.Done(); rcv, rerr = ra.Accept(ctx) }()
	wg.Wait()
	if serr != nil || rerr != nil {
		t.Skipf("no ICE pair on this host: dial %v, accept %v", serr, rerr)
	}
	return snd, rcv
}

// A burst well past pion's 1 MB receive buffer, arriving while the reader
// is busy, is not dropped: the read-ahead keeps pion's buffer drained.
// Before, about 1 MB of it survived, and QUIC's congestion window
// collapsed on the losses (3-9 MB/s on the direct route over loopback).
func TestPacketConn_BurstPastPionsBufferArrives(t *testing.T) {
	snd, rcv := pairAgents(t)
	const n, size = 3000, 1200 // 3.6 MB
	payload := make([]byte, size)
	// Paced (about 12 MB/s) so the kernel's socket buffer, capped near
	// 208 KB on stock Linux, never overflows: what this checks is pion's
	// buffer, which fills because the reader is busy elsewhere.
	for i := range n {
		if _, err := snd.WriteTo(payload, nil); err != nil {
			t.Fatal(err)
		}
		if i%50 == 49 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(100 * time.Millisecond)
	buf := make([]byte, 2048)
	got := 0
	for {
		_ = rcv.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if _, _, err := rcv.ReadFrom(buf); err != nil {
			break
		}
		got++
	}
	if got < n*9/10 {
		t.Fatalf("received %d of %d datagrams", got, n)
	}
}

// The read deadline applies to the read-ahead queue and can be lifted.
func TestPacketConn_ReadDeadline(t *testing.T) {
	snd, rcv := pairAgents(t)
	_ = rcv.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := rcv.ReadFrom(make([]byte, 2048)); err == nil {
		t.Fatal("ReadFrom returned before its deadline with nothing sent")
	}
	_ = rcv.SetReadDeadline(time.Time{})
	if _, err := snd.WriteTo([]byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	done := make(chan error, 1)
	go func() {
		_, _, err := rcv.ReadFrom(buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil || buf[0] != 'x' {
			t.Fatalf("read after lifting the deadline: %q %v", buf[:1], err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no datagram after lifting the deadline")
	}
}
