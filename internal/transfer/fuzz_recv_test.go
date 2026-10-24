package transfer

import (
	"bytes"
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/polius/fsend/internal/wire"
)

// FuzzDecodeChunkPayload drives the receiver's chunk decoding (zstd and the
// segment-sum check) with arbitrary frames. Errors are fine; panics or
// oversize output are not.
func FuzzDecodeChunkPayload(f *testing.F) {
	enc, _ := zstd.NewWriter(nil)
	plain := bytes.Repeat([]byte("fsend"), 1000)
	for _, c := range []*wire.Chunk{
		{Segments: []wire.Segment{{FileIndex: 0, Length: 5, EOF: true}}, Payload: []byte("hello")},
		{Compressed: true, Segments: []wire.Segment{{FileIndex: 1, Length: uint32(len(plain))}}, Payload: enc.EncodeAll(plain, nil)},
		{Segments: []wire.Segment{{FileIndex: 2, Skip: true}}},
	} {
		var buf bytes.Buffer
		if err := wire.WriteChunk(&buf, c); err != nil {
			f.Fatal(err)
		}
		f.Add(buf.Bytes())
	}
	f.Fuzz(func(t *testing.T, frame []byte) {
		c, err := wire.ReadChunk(bytes.NewReader(frame))
		if err != nil {
			return
		}
		var dec *zstd.Decoder
		defer func() {
			if dec != nil {
				dec.Close()
			}
		}()
		out, err := decodeChunkPayload(c, &dec, nil)
		if err == nil && (len(out) > wire.MaxChunkSize || uint64(len(out)) != c.SegmentsSum()) {
			t.Fatalf("accepted %d bytes for segments summing to %d", len(out), c.SegmentsSum())
		}
	})
}

// fuzzConn replays fuzzer bytes as a peer stream and discards our writes.
type fuzzConn struct{ r *bytes.Reader }

func (c *fuzzConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *fuzzConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *fuzzConn) Close() error                { return nil }

// FuzzRecv runs the whole receiver against a hostile sender's control and
// data bytes. Whatever it does, it must not panic, hang, or leave anything
// but regular files and folders in the receive dir.
func FuzzRecv(f *testing.F) {
	ctrl, data := fuzzSession(f)
	f.Add(ctrl, data)
	f.Add(ctrl[:len(ctrl)/2], data)
	f.Add(ctrl, data[:len(data)/2])
	f.Fuzz(func(t *testing.T, ctrl, data []byte) {
		dir := t.TempDir()
		s := &Streams{Control: &fuzzConn{bytes.NewReader(ctrl)}, Data: &fuzzConn{bytes.NewReader(data)}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = Recv(ctx, s, RecvOptions{TargetDir: dir})
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && !d.Type().IsRegular() {
				t.Fatalf("receiver created a non-regular file: %s", p)
			}
			return nil
		})
	})
}

// fuzzSession builds a valid sender session: a folder with a small file and
// a two-chunk file, then TRANSFER_COMPLETE.
func fuzzSession(f *testing.F) (ctrl, data []byte) {
	big := bytes.Repeat([]byte{7}, wire.MaxChunkSize+10)
	small := []byte("hello")
	var c, d bytes.Buffer
	must := func(err error) {
		if err != nil {
			f.Fatal(err)
		}
	}
	must(wire.WriteControl(&c, wire.TypeHello, &wire.SenderHello{
		MinVersion: wire.MinProtocolVersion, MaxVersion: wire.ProtocolVersion, Mode: wire.ModeFiles}))
	must(wire.WriteControl(&c, wire.TypeListingBatch, []wire.ListingEntry{
		{Index: 0, RelativePath: "d", Type: wire.EntryDir, Mode: 0o755},
		{Index: 1, RelativePath: "d/s.txt", Type: wire.EntryFile, Size: uint64(len(small)), Mode: 0o644},
		{Index: 2, RelativePath: "d/b.bin", Type: wire.EntryFile, Size: uint64(len(big)), Mode: 0o644},
	}))
	must(wire.WriteControl(&c, wire.TypeListingEnd, nil))
	must(wire.WriteControl(&c, wire.TypeTransferComplete, nil))
	must(wire.WriteChunk(&d, &wire.Chunk{Segments: []wire.Segment{
		{FileIndex: 1, Length: uint32(len(small)), EOF: true, RootHash: bytesRoot(small)},
		{FileIndex: 2, Length: wire.MaxChunkSize - uint32(len(small))},
	}, Payload: append(append([]byte(nil), small...), big[:wire.MaxChunkSize-len(small)]...)}))
	rest := big[wire.MaxChunkSize-len(small):]
	must(wire.WriteChunk(&d, &wire.Chunk{Segments: []wire.Segment{
		{FileIndex: 2, Length: uint32(len(rest)), EOF: true, RootHash: bytesRoot(big)},
	}, Payload: rest}))
	return c.Bytes(), d.Bytes()
}

// The seed session is a real, complete transfer: both files land.
func TestFuzzSession_IsValid(t *testing.T) {
	ctrl, data := fuzzSession(&testing.F{})
	dir := t.TempDir()
	s := &Streams{Control: &fuzzConn{bytes.NewReader(ctrl)}, Data: &fuzzConn{bytes.NewReader(data)}}
	if err := Recv(context.Background(), s, RecvOptions{TargetDir: dir}); err != nil {
		t.Fatalf("seed session failed: %v", err)
	}
	if got := mustRead(t, filepath.Join(dir, "d", "s.txt")); string(got) != "hello" {
		t.Fatalf("s.txt = %q", got)
	}
}
