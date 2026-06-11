package transfer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// TestRecv_RejectsStreamingFileInNonStdinTransfer locks in the guard
// against the size-cap/hash-check bypass: Streaming exempts a file from
// the declared-size cap and the BLAKE3 root check, so a malicious
// sender could advertise a small single-file transfer at the accept
// prompt and then mark its FILE_INFO Streaming to pour unbounded,
// unverified bytes onto the receiver's disk. Only TransferStdin may
// carry streaming items.
func TestRecv_RejectsStreamingFileInNonStdinTransfer(t *testing.T) {
	dstDir := t.TempDir()
	sender, receiver := pipePair()

	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		TransferKind:    wire.TransferSingleFile,
		TotalFiles:      1,
		TotalBytes:      5, // what the accept prompt would show
		DisplayName:     "small.bin",
	}

	var wg sync.WaitGroup
	wg.Add(2)

	var recvErr error
	go func() {
		defer wg.Done()
		recvErr = Recv(context.Background(), &receiver, RecvOptions{
			TargetDir: dstDir,
			Accept:    func(_ wire.SenderHello) bool { return true },
		})
		_ = receiver.Control.Close()
		_ = receiver.Data.Close()
	}()

	var gotFrameType wire.FrameType
	go func() {
		defer wg.Done()
		defer sender.Control.Close()
		defer sender.Data.Close()
		_ = wire.WriteControl(sender.Control, wire.TypeHello, hello)

		var ack wire.ReceiverHello
		_, _ = wire.ReadControl(sender.Control, &ack)
		if !ack.Accepts {
			return
		}

		// Malicious: Streaming on a single-file transfer (zero root,
		// non-resumable → would also skip the final hash check).
		fi := wire.FileInfo{
			Index:        0,
			RelativePath: "small.bin",
			Size:         5,
			Mode:         0o644,
			Streaming:    true,
			Resumable:    false,
		}
		_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &fi)

		var ef wire.ErrorFrame
		gotFrameType, _ = wire.ReadControl(sender.Control, &ef)
	}()

	wg.Wait()

	if !errors.Is(recvErr, fserrors.ErrProtocolError) {
		t.Errorf("expected ErrProtocolError for streaming FILE_INFO in single-file transfer, got %v", recvErr)
	}
	if gotFrameType != wire.TypeError {
		t.Errorf("sender should receive an ERROR frame, got frame type %v", gotFrameType)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "small.bin"+partialSuffix)); err == nil {
		t.Error("receiver opened a partial for the rejected file")
	}
}

// runMaliciousSender drives Recv against a hand-scripted sender: HELLO
// goes out, the ack is drained, script runs on the sender's streams, and
// the next control frame the sender reads back is returned (TypeError on
// a guard violation) along with Recv's error.
func runMaliciousSender(t *testing.T, hello *wire.SenderHello, opts RecvOptions, script func(sender *Streams)) (wire.FrameType, error) {
	t.Helper()
	sender, receiver := pipePair()
	opts.Accept = func(_ wire.SenderHello) bool { return true }

	var wg sync.WaitGroup
	wg.Add(2)

	var recvErr error
	go func() {
		defer wg.Done()
		recvErr = Recv(context.Background(), &receiver, opts)
		_ = receiver.Control.Close()
		_ = receiver.Data.Close()
	}()

	var gotFrameType wire.FrameType
	go func() {
		defer wg.Done()
		defer sender.Control.Close()
		defer sender.Data.Close()
		_ = wire.WriteControl(sender.Control, wire.TypeHello, hello)

		var ack wire.ReceiverHello
		_, _ = wire.ReadControl(sender.Control, &ack)
		if !ack.Accepts {
			return
		}
		script(&sender)

		var ef wire.ErrorFrame
		gotFrameType, _ = wire.ReadControl(sender.Control, &ef)
	}()

	wg.Wait()
	return gotFrameType, recvErr
}

// sendEmptyFile completes one legitimate zero-byte file so the scripted
// sender can probe what happens after the consented transfer is done.
func sendEmptyFile(sender *Streams, index uint32, name string) {
	fi := wire.FileInfo{Index: index, RelativePath: name, Mode: 0o644}
	_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &fi)
	var d wire.FileAcceptDecision
	_, _ = wire.ReadControl(sender.Control, &d)
	_ = wire.WriteChunk(sender.Data, &wire.Chunk{
		FileIndex: index, Flags: wire.FlagLastChunk, Blake3Hash: blakeHash32(nil),
	})
}

// TestRecv_RejectsOversoldFileCount: the accept prompt showed "1 file";
// after that file completes, any further FILE_INFO is a consent bypass
// and must be refused before a partial is even opened.
func TestRecv_RejectsOversoldFileCount(t *testing.T) {
	dstDir := t.TempDir()
	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		TransferKind:    wire.TransferSingleFile,
		TotalFiles:      1,
		DisplayName:     "tiny.bin",
	}

	gotFrameType, recvErr := runMaliciousSender(t, hello, RecvOptions{TargetDir: dstDir}, func(sender *Streams) {
		sendEmptyFile(sender, 0, "tiny.bin")
		// Malicious: a second FILE_INFO the user never consented to.
		fi := wire.FileInfo{Index: 1, RelativePath: "extra.bin", Mode: 0o644}
		_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &fi)
	})

	if !errors.Is(recvErr, fserrors.ErrProtocolError) {
		t.Errorf("expected ErrProtocolError for FILE_INFO beyond HELLO count, got %v", recvErr)
	}
	if gotFrameType != wire.TypeError {
		t.Errorf("sender should receive an ERROR frame, got frame type %v", gotFrameType)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "extra.bin"+partialSuffix)); err == nil {
		t.Error("receiver opened a partial for the oversold file")
	}
}

// TestRecv_RejectsOversoldTotalBytes: the prompt showed 5 B; a FILE_INFO
// declaring more must be refused before any bytes hit the disk.
func TestRecv_RejectsOversoldTotalBytes(t *testing.T) {
	dstDir := t.TempDir()
	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		TransferKind:    wire.TransferSingleFile,
		TotalFiles:      1,
		TotalBytes:      5, // what the accept prompt showed
		DisplayName:     "small.bin",
	}

	gotFrameType, recvErr := runMaliciousSender(t, hello, RecvOptions{TargetDir: dstDir}, func(sender *Streams) {
		fi := wire.FileInfo{
			Index: 0, RelativePath: "small.bin", Size: 1 << 40, Mode: 0o644,
		}
		_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &fi)
	})

	if !errors.Is(recvErr, fserrors.ErrProtocolError) {
		t.Errorf("expected ErrProtocolError for declared size beyond HELLO total, got %v", recvErr)
	}
	if gotFrameType != wire.TypeError {
		t.Errorf("sender should receive an ERROR frame, got frame type %v", gotFrameType)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "small.bin"+partialSuffix)); err == nil {
		t.Error("receiver opened a partial for the oversold file")
	}
}

// TestRecv_RejectsNestedPathInMultiFile: legitimate multi-file senders
// only ever put bare basenames in RelativePath; a separator means the
// peer is creating directory trees the user never saw at the prompt.
func TestRecv_RejectsNestedPathInMultiFile(t *testing.T) {
	dstDir := t.TempDir()
	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		TransferKind:    wire.TransferMultiFile,
		TotalFiles:      2,
		TotalBytes:      1024,
		DisplayName:     "2 files",
	}

	gotFrameType, recvErr := runMaliciousSender(t, hello, RecvOptions{TargetDir: dstDir}, func(sender *Streams) {
		fi := wire.FileInfo{
			Index: 0, RelativePath: "nested/evil.bin", Size: 5, Mode: 0o644,
		}
		_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &fi)
	})

	if !errors.Is(recvErr, fserrors.ErrProtocolError) {
		t.Errorf("expected ErrProtocolError for nested RelativePath in multi-file transfer, got %v", recvErr)
	}
	if gotFrameType != wire.TypeError {
		t.Errorf("sender should receive an ERROR frame, got frame type %v", gotFrameType)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "nested")); err == nil {
		t.Error("receiver created a directory the user never consented to")
	}
}

// TestRecv_SinkRejectsSecondPayload: sink mode emits bytes straight to
// the caller's writer, so a second payload would silently concatenate —
// the count guard must cover recvPayloadToSink too.
func TestRecv_SinkRejectsSecondPayload(t *testing.T) {
	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		TransferKind:    wire.TransferSingleFile,
		TotalFiles:      1,
		DisplayName:     "tiny.bin",
	}

	gotFrameType, recvErr := runMaliciousSender(t, hello, RecvOptions{Sink: io.Discard}, func(sender *Streams) {
		sendEmptyFile(sender, 0, "tiny.bin")
		fi := wire.FileInfo{Index: 1, RelativePath: "extra.bin", Mode: 0o644}
		_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &fi)
	})

	if !errors.Is(recvErr, fserrors.ErrProtocolError) {
		t.Errorf("expected ErrProtocolError for second payload in sink mode, got %v", recvErr)
	}
	if gotFrameType != wire.TypeError {
		t.Errorf("sender should receive an ERROR frame, got frame type %v", gotFrameType)
	}
}
