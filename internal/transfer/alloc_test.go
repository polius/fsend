package transfer

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/polius/fsend/internal/wire"
)

// repeatReader serves block over and over, n bytes in all, allocating
// nothing itself.
type repeatReader struct {
	block []byte
	n     int
	off   int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	c := copy(p[:min(len(p), r.n)], r.block[r.off:])
	r.off = (r.off + c) % len(r.block)
	r.n -= c
	return c, nil
}

// Moving bytes must not cost memory in proportion to them: every buffer a
// chunk passes through (read, compressed copy, frame, received payload,
// decompressed copy) comes from a pool and goes back. Before that held for
// the stream paths, `… | fsend` into `--out -` allocated 1.6 bytes per byte
// moved, and a file about 0.6.
func TestTransfer_AllocationDoesNotGrowWithTheBytesMoved(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops buffers at random under the race detector")
	}
	const size = 128 << 20
	random := randBytes(1 << 20) // the same block again: still incompressible, chunk by chunk
	text := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog 0123456789\n"), (1<<20)/55)
	file := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(file, bytes.Repeat(random, size>>20), 0o644); err != nil {
		t.Fatal(err)
	}
	sources, err := Walk([]string{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := func(block []byte, n int) SendOptions {
		return SendOptions{Mode: wire.ModeStream, Stream: &repeatReader{block: block, n: n}, DisplayName: "stdin"}
	}
	cases := []struct {
		name string
		send func(n int) SendOptions
		recv func() RecvOptions
	}{
		{"stdin to --out -, incompressible", func(n int) SendOptions { return stream(random, n) },
			func() RecvOptions { return RecvOptions{Sink: io.Discard} }},
		{"stdin to --out -, text", func(n int) SendOptions { return stream(text, n) },
			func() RecvOptions { return RecvOptions{Sink: io.Discard} }},
		{"stdin to a file", func(n int) SendOptions { return stream(random, n) },
			func() RecvOptions { return RecvOptions{TargetDir: t.TempDir()} }},
		{"file to --out -", func(int) SendOptions { return SendOptions{Mode: wire.ModeFiles, Sources: sources} },
			func() RecvOptions { return RecvOptions{Sink: io.Discard} }},
		{"file to a file", func(int) SendOptions { return SendOptions{Mode: wire.ModeFiles, Sources: sources} },
			func() RecvOptions { return RecvOptions{TargetDir: t.TempDir()} }},
	}
	// The first transfers of a process build the compressor (once, about
	// 30 MB when every worker has run) and fill the pools: not part of
	// what a byte costs.
	if se, re := runTransfer(t, stream(text, 32<<20), RecvOptions{Sink: io.Discard}); se != nil || re != nil {
		t.Fatalf("warm-up: send=%v recv=%v", se, re)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			se, re := runTransfer(t, c.send(size), c.recv())
			runtime.ReadMemStats(&after)
			if se != nil || re != nil {
				t.Fatalf("send=%v recv=%v", se, re)
			}
			got := after.TotalAlloc - before.TotalAlloc
			t.Logf("%d MiB moved, %.1f MiB allocated", size>>20, float64(got)/(1<<20))
			// A buffer per chunk anywhere on the path is at least size.
			if got > size/2 {
				t.Errorf("allocated %d MiB to move %d MiB", got>>20, size>>20)
			}
		})
	}
}
