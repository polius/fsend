package transfer

import (
	"bytes"
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

// A malicious sender must not escape the receive dir via symlink indirection:
// two symlinks that look in-bounds to a lexical check but, once materialized,
// redirect a later file write one level ABOVE the receive dir.
//
//	p        -> .           (real path: <dst>)
//	p/q      -> ../outside  (real path: <base>/outside, above <dst>)
//	p/q/evil (regular file)  lexically <dst>/p/q/evil, really <base>/outside/evil
//
// Pre-fix, materialize created the links and openRecvFile followed them,
// writing attacker content into <base>/outside. The honest sender never emits
// symlinks, so the receiver now rejects any symlink entry outright.
func TestHostile_SymlinkListingRejected(t *testing.T) {
	base := t.TempDir()
	dst := filepath.Join(base, "target")
	outside := filepath.Join(base, "outside") // where a successful escape would land
	for _, d := range []string{dst, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctrlA, ctrlB := net.Pipe()
	dataA, dataB := net.Pipe()
	sender := &Streams{Control: ctrlA, Data: dataA}
	receiver := &Streams{Control: ctrlB, Data: dataB}

	recvErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		recvErr <- Recv(ctx, receiver, RecvOptions{TargetDir: dst})
		receiver.Close()
	}()
	// Drain the receiver's replies so its ack write and teardown decline can
	// complete over the synchronous pipe (writes are a separate direction).
	go func() { _, _ = io.Copy(io.Discard, sender.Control) }()
	go func() { _, _ = io.Copy(io.Discard, sender.Data) }()

	if err := wire.WriteControl(sender.Control, wire.TypeHello, &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion, Mode: wire.ModeFiles, Hostname: "evil",
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	entries := []wire.ListingEntry{
		{Index: 0, RelativePath: "p", Type: wire.EntrySymlink, SymlinkTarget: "."},
		{Index: 1, RelativePath: "p/q", Type: wire.EntrySymlink, SymlinkTarget: "../outside"},
		{Index: 2, RelativePath: "p/q/evil", Size: 4, Type: wire.EntryFile},
	}
	if err := wire.WriteControl(sender.Control, wire.TypeListingBatch, entries); err != nil {
		t.Fatalf("listing: %v", err)
	}
	if err := wire.WriteControl(sender.Control, wire.TypeListingEnd, nil); err != nil {
		t.Fatalf("listing-end: %v", err)
	}
	// Best-effort payload for the escaping file: a fixed receiver rejects at
	// classify before the data phase (the drain discards this); a vulnerable one
	// would materialize the links and write it into <base>/outside.
	go func() { _ = wire.WriteChunk(sender.Data, mkChunk(2, []byte("PWND"), true)) }()

	select {
	case err := <-recvErr:
		if err == nil {
			t.Fatal("receiver accepted a peer-supplied symlink listing")
		}
		if !errors.Is(err, fserrors.ErrPathTraversal) {
			t.Fatalf("want ErrPathTraversal, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("receiver did not reject the symlink listing")
	}

	// No file escaped, and no symlink was planted inside the receive dir.
	if _, err := os.Lstat(filepath.Join(outside, "evil")); !os.IsNotExist(err) {
		t.Fatalf("file escaped the receive dir into %s", filepath.Join(outside, "evil"))
	}
	if _, err := os.Lstat(filepath.Join(dst, "p")); !os.IsNotExist(err) {
		t.Fatal("receiver planted a peer-supplied symlink")
	}
	sender.Close()
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

// A hostile receiver must not be able to grow the sender's decision map past
// the number of entries it actually sent.
func TestSenderNegotiate_RejectsExcessDecisions(t *testing.T) {
	ctrlA, ctrlB := net.Pipe()
	sender := &Streams{Control: ctrlA}
	sources := []Source{
		{Entry: wire.ListingEntry{Index: 0}},
		{Entry: wire.ListingEntry{Index: 1}},
	}

	res := make(chan error, 1)
	go func() {
		_, err := senderNegotiate(sender, sources)
		res <- err
	}()
	go func() { _, _ = io.Copy(io.Discard, ctrlB) }() // drain any sender writes

	batch := make([]wire.Decision, 0, 100)
	for i := uint32(0); i < 100; i++ {
		batch = append(batch, wire.Decision{Index: i, Action: wire.DecisionSend})
	}
	if err := wire.WriteControl(ctrlB, wire.TypeClassifyBatch, batch); err != nil {
		t.Fatalf("write batch: %v", err)
	}

	select {
	case err := <-res:
		if !errors.Is(err, fserrors.ErrProtocolError) {
			t.Fatalf("want ErrProtocolError, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("senderNegotiate did not reject excess decisions")
	}
}

func TestEngine_StreamRefusesExistingTarget(t *testing.T) {
	dst := t.TempDir()
	precious := []byte("precious local content")
	writeFile(t, filepath.Join(dst, ".bashrc"), precious)

	// A crafted DisplayName pointing at an existing file must be declined,
	// not silently truncated and renamed over.
	se, re := runTransfer(t,
		SendOptions{Mode: wire.ModeStream, Stream: bytes.NewReader([]byte("evil")), DisplayName: ".bashrc"},
		RecvOptions{TargetDir: dst},
	)
	if !errors.Is(re, fserrors.ErrTargetExists) {
		t.Fatalf("recv: want ErrTargetExists, got %v", re)
	}
	if !errors.Is(se, fserrors.ErrTargetExists) {
		t.Fatalf("send: want ErrTargetExists, got %v", se)
	}
	if got := mustRead(t, filepath.Join(dst, ".bashrc")); !bytes.Equal(got, precious) {
		t.Fatalf("existing file was modified: %q", got)
	}
}

func TestEngine_StreamOverwriteReplacesExisting(t *testing.T) {
	dst := t.TempDir()
	writeFile(t, filepath.Join(dst, "out.txt"), []byte("old"))

	se, re := runTransfer(t,
		SendOptions{Mode: wire.ModeStream, Stream: bytes.NewReader([]byte("new")), DisplayName: "out.txt"},
		RecvOptions{TargetDir: dst, Overwrite: true},
	)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if got := mustRead(t, filepath.Join(dst, "out.txt")); string(got) != "new" {
		t.Fatalf("want overwritten content, got %q", got)
	}
}

func TestStreamFileName(t *testing.T) {
	cases := map[string]string{
		"msg.txt":       "msg.txt",
		"dir/inner.txt": "inner.txt",
		"":              "fsend-received",
		".":             "fsend-received",
		"../../.bashrc": "fsend-received",
		"/etc/passwd":   "fsend-received",
	}
	for in, want := range cases {
		if got := StreamFileName(in); got != want {
			t.Errorf("StreamFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
