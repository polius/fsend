package transfer

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
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
// is dominated by per-file filesystem work. On macOS that is creates and
// renames (fsync there skips the drive cache; one F_FULLFSYNC covers a
// batch); on Linux it was an fsync per file until one syncfs per batch.
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

// deepTree lists n files spread over folders six levels deep, as a large
// project tree arrives: each folder entry, then its files.
func deepTree(n int) []wire.ListingEntry {
	var es []wire.ListingEntry
	add := func(rel string, t wire.EntryType, size uint64) {
		es = append(es, wire.ListingEntry{Index: uint32(len(es)), RelativePath: rel, Type: t, Size: size, Mode: 0o644, ModTimeSec: 1717414800})
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		dir := fmt.Sprintf("proj/a%d/b%d/c%d/d%d/e%d", i/20000, i/4000, i/800, i/160, i/32)
		for d, parts := "", strings.Split(dir, "/"); len(parts) > 0; parts = parts[1:] {
			d = path.Join(d, parts[0])
			if !seen[d] {
				seen[d] = true
				add(d, wire.EntryDir, 0)
			}
		}
		add(fmt.Sprintf("%s/file%06d.go", dir, i), wire.EntryFile, 1024)
	}
	return es
}

// BenchmarkClassifyDeepTree classifies 100,000 files six folders deep into
// an empty directory: before the per-folder memo every entry re-examined
// each of its ancestors.
func BenchmarkClassifyDeepTree(b *testing.B) {
	es := deepTree(100_000)
	for b.Loop() {
		if _, err := classify(es, b.TempDir(), false); err != nil {
			b.Fatal(err)
		}
	}
}
