package transfer

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// awaitReject drains the sender's streams (so the receiver's teardown — which
// posts an error frame on control with nobody reading otherwise — can complete
// over the synchronous in-memory pipe) and returns the receiver's result.
func awaitReject(sender *Streams, recvErr <-chan error) error {
	go func() { _, _ = io.Copy(io.Discard, sender.Control) }()
	go func() { _, _ = io.Copy(io.Discard, sender.Data) }()
	return <-recvErr
}

// hostileSetup starts a real receiver against dstDir and hands back the
// sender-side streams, driven manually so tests can inject malformed frames.
// It runs HELLO/HELLO_ACK, sends the given listing, and returns the receiver's
// decision vector — leaving the data phase for the test to abuse.
func hostileSetup(t *testing.T, dstDir string, entries []wire.ListingEntry, opts RecvOptions) (*Streams, map[uint32]wire.Decision, <-chan error) {
	t.Helper()
	ctrlA, ctrlB := net.Pipe()
	dataA, dataB := net.Pipe()
	sender := &Streams{Control: ctrlA, Data: dataA}
	receiver := &Streams{Control: ctrlB, Data: dataB}
	opts.TargetDir = dstDir

	recvErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		recvErr <- Recv(ctx, receiver, opts)
		receiver.Close()
	}()

	if err := wire.WriteControl(sender.Control, wire.TypeHello, &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion, Mode: wire.ModeFiles, Hostname: "evil",
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	var ack wire.ReceiverHello
	if _, err := wire.ReadControl(sender.Control, &ack); err != nil {
		t.Fatalf("hello-ack: %v", err)
	}
	if err := wire.WriteControl(sender.Control, wire.TypeListingBatch, entries); err != nil {
		t.Fatalf("listing: %v", err)
	}
	if err := wire.WriteControl(sender.Control, wire.TypeListingEnd, nil); err != nil {
		t.Fatalf("listing-end: %v", err)
	}
	decisions, err := recvDecisions(sender.Control)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	return sender, decisions, recvErr
}

// mkChunk builds a well-formed single-segment chunk for one file.
func mkChunk(index uint32, payload []byte, eof bool) *wire.Chunk {
	seg := wire.Segment{FileIndex: index, Length: uint32(len(payload)), EOF: eof}
	if eof {
		seg.RootHash = blakeHash32(payload)
	}
	return &wire.Chunk{ChunkHash: blakeHash32(payload), Segments: []wire.Segment{seg}, Payload: payload}
}

// A chunk for a file the receiver chose to keep (a differing file, no
// --overwrite) must be rejected — never written over the protected local copy.
func TestHostile_ChunkForKeptFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	_ = src
	writeFile(t, filepath.Join(dst, "keep.txt"), []byte("LOCAL-ORIGINAL"))

	entries := []wire.ListingEntry{
		{Index: 0, RelativePath: "keep.txt", Size: 3, Type: wire.EntryFile}, // size differs → kept
		{Index: 1, RelativePath: "send.txt", Size: 4, Type: wire.EntryFile}, // genuinely new
	}
	sender, decisions, recvErr := hostileSetup(t, dst, entries, RecvOptions{}) // no Overwrite → keep
	if decisions[0].Action != wire.DecisionSkip {
		t.Fatalf("kept file should be DecisionSkip, got %v", decisions[0].Action)
	}

	// Hostile: stream data for the kept file index 0.
	go func() { _ = wire.WriteChunk(sender.Data, mkChunk(0, []byte("EVIL"), true)) }()

	if err := awaitReject(sender, recvErr); err == nil {
		t.Fatal("receiver accepted a chunk for a kept file")
	}
	if got := mustRead(t, filepath.Join(dst, "keep.txt")); string(got) != "LOCAL-ORIGINAL" {
		t.Errorf("protected local file was clobbered: %q", got)
	}
	sender.Close()
}

// A segment longer than the file's declared size must be rejected (bounds the
// bytes a peer can write past what the user consented to).
func TestHostile_OversizedSegment(t *testing.T) {
	dst := t.TempDir()
	entries := []wire.ListingEntry{{Index: 0, RelativePath: "f.bin", Size: 4, Type: wire.EntryFile}}
	sender, _, recvErr := hostileSetup(t, dst, entries, RecvOptions{})

	// Declared size 4, but stream 100 bytes.
	go func() { _ = wire.WriteChunk(sender.Data, mkChunk(0, make([]byte, 100), true)) }()

	if err := awaitReject(sender, recvErr); err == nil {
		t.Fatal("receiver accepted bytes exceeding the declared size")
	}
	sender.Close()
}

// A second EOF for an already-finalized file (while another file is still
// pending) must be rejected, not re-open and re-write the finalized target.
func TestHostile_DuplicateEOF(t *testing.T) {
	dst := t.TempDir()
	entries := []wire.ListingEntry{
		{Index: 0, RelativePath: "a.bin", Size: 4, Type: wire.EntryFile},
		{Index: 1, RelativePath: "b.bin", Size: 4, Type: wire.EntryFile}, // kept pending
	}
	sender, _, recvErr := hostileSetup(t, dst, entries, RecvOptions{})

	go func() {
		_ = wire.WriteChunk(sender.Data, mkChunk(0, []byte("GOOD"), true)) // finalizes a.bin
		_ = wire.WriteChunk(sender.Data, mkChunk(0, []byte("EVIL"), true)) // duplicate EOF for a.bin
	}()

	if err := awaitReject(sender, recvErr); err == nil {
		t.Fatal("receiver accepted a duplicate EOF for a finalized file")
	}
	if got, rerr := os.ReadFile(filepath.Join(dst, "a.bin")); rerr == nil && string(got) != "GOOD" {
		t.Errorf("finalized file overwritten by duplicate EOF: %q", got)
	}
	sender.Close()
}

// A chunk whose ChunkHash doesn't match its payload must be rejected.
func TestHostile_CorruptChunkHash(t *testing.T) {
	dst := t.TempDir()
	entries := []wire.ListingEntry{{Index: 0, RelativePath: "f.bin", Size: 4, Type: wire.EntryFile}}
	sender, _, recvErr := hostileSetup(t, dst, entries, RecvOptions{})

	c := mkChunk(0, []byte("GOOD"), true)
	c.ChunkHash[0] ^= 0xFF // corrupt the transport hash
	go func() { _ = wire.WriteChunk(sender.Data, c) }()

	if err := awaitReject(sender, recvErr); err == nil {
		t.Fatal("receiver accepted a chunk with a bad transport hash")
	}
	sender.Close()
}

// A peer speaking a different wire-protocol version (a breaking release) must
// surface the clear "incompatible version" error on the receiver, not the
// generic "file a bug" catch-all.
func TestVersionMismatch_ReceiverReportsClearly(t *testing.T) {
	ctrlA, ctrlB := net.Pipe()
	dataA, dataB := net.Pipe()
	receiver := &Streams{Control: ctrlB, Data: dataB}

	recvErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		recvErr <- Recv(ctx, receiver, RecvOptions{TargetDir: t.TempDir()})
	}()
	go func() { _, _ = io.Copy(io.Discard, ctrlA) }() // drain the receiver's reply
	go func() { _, _ = io.Copy(io.Discard, dataA) }()

	// A HELLO frame stamped with an incompatible protocol version.
	badVer := byte(wire.ProtocolVersion + 1)
	if _, err := ctrlA.Write([]byte{badVer, byte(wire.TypeHello), 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := <-recvErr; !errors.Is(err, fserrors.ErrIncompatibleVersion) {
		t.Fatalf("recv error = %v, want ErrIncompatibleVersion", err)
	}
	_ = ctrlA.Close()
	_ = dataA.Close()
}

// And the sender must report it clearly too, when the peer's reply is stamped
// with an incompatible version.
func TestVersionMismatch_SenderReportsClearly(t *testing.T) {
	ctrlA, ctrlB := net.Pipe()
	dataA, dataB := net.Pipe()
	sender := &Streams{Control: ctrlA, Data: dataA}

	sendErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sendErr <- Send(ctx, sender, SendOptions{Mode: wire.ModeFiles})
	}()
	go func() { _, _ = io.Copy(io.Discard, dataB) }()

	// Consume the sender's HELLO, then reply with an incompatible-version ack.
	if _, _, err := wire.ReadControlRaw(ctrlB); err != nil {
		t.Fatal(err)
	}
	badVer := byte(wire.ProtocolVersion + 1)
	if _, err := ctrlB.Write([]byte{badVer, byte(wire.TypeHelloAck), 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := <-sendErr; !errors.Is(err, fserrors.ErrIncompatibleVersion) {
		t.Fatalf("send error = %v, want ErrIncompatibleVersion", err)
	}
	_ = ctrlB.Close()
	_ = dataB.Close()
}
