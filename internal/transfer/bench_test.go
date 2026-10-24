package transfer

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/wire"
)

// tcpPair returns a connected loopback TCP pair: kernel-buffered like a real
// QUIC stream, unlike net.Pipe's lockstep handoff, so benchmarks measure the
// engine rather than the pipe.
func tcpPair(tb testing.TB) (net.Conn, net.Conn) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	return a, <-ch
}

func benchTransfer(tb testing.TB, src []string, dst string) {
	tb.Helper()
	sources, err := Walk(src, nil)
	if err != nil {
		tb.Fatal(err)
	}
	ca, cb := tcpPair(tb)
	da, db := tcpPair(tb)
	sender := &Streams{Control: ca, Data: da}
	receiver := &Streams{Control: cb, Data: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	var se, re error
	wg.Add(2)
	go func() {
		defer wg.Done()
		se = Send(ctx, sender, SendOptions{Mode: wire.ModeFiles, Sources: sources})
		sender.Close()
	}()
	go func() {
		defer wg.Done()
		re = Recv(ctx, receiver, RecvOptions{TargetDir: dst})
		receiver.Close()
	}()
	wg.Wait()
	if se != nil || re != nil {
		tb.Fatalf("send=%v recv=%v", se, re)
	}
}

// BenchmarkSmallFiles2000 receives 2,000 4 KiB files onto the real disk; it
// is dominated by per-file durability work (fsync, rename, dir sync).
func BenchmarkSmallFiles2000(b *testing.B) {
	src := b.TempDir()
	root := filepath.Join(src, "tree")
	for i := range 2000 {
		p := filepath.Join(root, fmt.Sprintf("d%02d", i%20), fmt.Sprintf("f%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, randBytes(4096), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for b.Loop() {
		benchTransfer(b, []string{root}, b.TempDir())
	}
}

// BenchmarkLargeFile streams one incompressible 512 MiB file end to end.
func BenchmarkLargeFile(b *testing.B) {
	src := b.TempDir()
	p := filepath.Join(src, "big.bin")
	const size = 512 << 20
	if err := os.WriteFile(p, randBytes(size), 0o644); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(size)
	b.ResetTimer()
	for b.Loop() {
		benchTransfer(b, []string{p}, b.TempDir())
	}
}
