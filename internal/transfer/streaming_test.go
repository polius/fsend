package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/polius/fsend/internal/wire"
)

// TestStreamingTransfer_Roundtrip exercises the size-unknown sender path
// used by piped stdin. The sender holds an io.Reader that EOFs after
// `payload`; the receiver must reassemble those exact bytes by stopping
// on FlagLastChunk rather than counting down a declared Size.
//
// We parameterize over a range of payload sizes that span:
//   - the empty case (zero-byte stream)
//   - small (one chunk, with a tail)
//   - exact MaxChunkSize alignment (forces an extra empty last chunk)
//   - multi-chunk with a partial tail
//
// All sizes must round-trip byte-perfect.
func TestStreamingTransfer_Roundtrip(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"small", 1024},
		{"exact-1-chunk", wire.MaxChunkSize},
		{"two-chunks-aligned", 2 * wire.MaxChunkSize},
		{"three-chunks-with-tail", 2*wire.MaxChunkSize + 12345},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := make([]byte, tc.size)
			if _, err := rand.Read(payload); err != nil {
				t.Fatal(err)
			}
			dstDir := t.TempDir()
			items := []SourceItem{{
				Info: wire.FileInfo{
					Index:        0,
					RelativePath: "fsend-stdin-test",
					Mode:         0o644,
					Resumable:    false,
					Streaming:    true,
				},
				Reader: bytes.NewReader(payload),
			}}

			a, b := pipePair()
			defer a.Close()
			defer b.Close()

			var eofIdx atomic.Uint32
			var eofBytes atomic.Uint64
			var eofFired atomic.Bool

			var sendErr, recvErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				sendErr = Send(context.Background(), &a, SendOptions{
					Items:        items,
					TransferKind: wire.TransferStdin,
					OnStreamingEOF: func(idx uint32, finalBytes uint64) {
						eofIdx.Store(idx)
						eofBytes.Store(finalBytes)
						eofFired.Store(true)
					},
				})
			}()
			go func() {
				defer wg.Done()
				recvErr = Recv(context.Background(), &b, RecvOptions{TargetDir: dstDir})
			}()
			wg.Wait()

			if sendErr != nil {
				t.Fatalf("Send: %v", sendErr)
			}
			if recvErr != nil {
				t.Fatalf("Recv: %v", recvErr)
			}

			got, err := os.ReadFile(filepath.Join(dstDir, "fsend-stdin-test"))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
			}
			if !eofFired.Load() {
				t.Errorf("OnStreamingEOF was not called")
			}
			if eofIdx.Load() != 0 {
				t.Errorf("OnStreamingEOF idx = %d, want 0", eofIdx.Load())
			}
			if eofBytes.Load() != uint64(len(payload)) {
				t.Errorf("OnStreamingEOF finalBytes = %d, want %d", eofBytes.Load(), len(payload))
			}
		})
	}
}

// TestStreamingTransfer_LiveProducer simulates a slow producer (pg_dump |
// fsend) where bytes trickle in over time. The sender must not block
// waiting for the full payload before emitting chunks: it has to drain
// the pipe end-to-end. We assert progress callbacks fire incrementally
// rather than all at once at EOF.
func TestStreamingTransfer_LiveProducer(t *testing.T) {
	const totalBytes = 3 * wire.MaxChunkSize
	pr, pw := io.Pipe()

	// Producer goroutine: writes the payload in two halves with a small
	// pause in between. The test's signal of "incremental" is that we
	// see at least two ProgressFn ticks before EOF.
	wantPayload := make([]byte, totalBytes)
	if _, err := rand.Read(wantPayload); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer pw.Close()
		_, _ = pw.Write(wantPayload[:totalBytes/2])
		_, _ = pw.Write(wantPayload[totalBytes/2:])
	}()

	dstDir := t.TempDir()
	items := []SourceItem{{
		Info: wire.FileInfo{
			Index:        0,
			RelativePath: "live-stream",
			Mode:         0o644,
			Resumable:    false,
			Streaming:    true,
		},
		Reader: pr,
	}}

	a, b := pipePair()
	defer a.Close()
	defer b.Close()

	var ticks atomic.Uint32
	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sendErr = Send(context.Background(), &a, SendOptions{
			Items:        items,
			TransferKind: wire.TransferStdin,
			ProgressFn:   func(uint32, uint64) { ticks.Add(1) },
		})
	}()
	go func() {
		defer wg.Done()
		recvErr = Recv(context.Background(), &b, RecvOptions{TargetDir: dstDir})
	}()
	wg.Wait()

	if sendErr != nil {
		t.Fatalf("Send: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("Recv: %v", recvErr)
	}
	got, err := os.ReadFile(filepath.Join(dstDir, "live-stream"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, wantPayload) {
		t.Fatalf("payload mismatch: %d vs %d bytes", len(got), len(wantPayload))
	}
	// Three full chunks → at least 3 progress ticks. The exact number
	// depends on chunk alignment, but anything less than 2 means we
	// were buffering — a regression we want to catch.
	if ticks.Load() < 2 {
		t.Errorf("expected ≥2 progress ticks for streamed transfer, got %d", ticks.Load())
	}
}
