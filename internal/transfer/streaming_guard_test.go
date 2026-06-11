package transfer

import (
	"context"
	"errors"
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
