package transfer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/polius/fsend/internal/wire"
)

// TestRecv_RefusesSymlinkPointingOutsideTargetDir is a security-focused
// test that confirms the receiver refuses to honour a peer-supplied
// symlink whose target lives outside TargetDir.
//
// Attack scenario: a malicious sender emits a FILE_INFO for a symlink
// "evil → /some/sensitive/dir", followed by a FILE_INFO for
// "evil/foo.txt". If the receiver creates the symlink, the subsequent
// file write resolves through it and writes to /some/sensitive/dir/foo.txt
// — outside TargetDir, which is a real path-traversal escalation.
//
// The receiver MUST either:
//   - refuse the symlink (return ErrPathTraversal), or
//   - reject the second write (return ErrPathTraversal at evil/foo.txt).
func TestRecv_RefusesSymlinkPointingOutsideTargetDir(t *testing.T) {
	dstDir := t.TempDir()
	sensitiveDir := t.TempDir() // a stand-in for "anywhere outside dstDir"

	sender, receiver := pipePair()

	// Craft a HELLO advertising a multi-file transfer.
	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		TransferKind:    wire.TransferMultiFile,
		TotalFiles:      2,
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
		// Close our end so a blocked sender unblocks on a write error.
		_ = receiver.Control.Close()
		_ = receiver.Data.Close()
	}()

	go func() {
		defer wg.Done()
		// Close sender streams when we're done so the receiver doesn't
		// hang reading next FILE_INFO if it accepted everything.
		defer sender.Control.Close()
		defer sender.Data.Close()
		_ = wire.WriteControl(sender.Control, wire.TypeHello, hello)

		// Read HELLO_ACK.
		var ack wire.ReceiverHello
		_, _ = wire.ReadControl(sender.Control, &ack)
		if !ack.Accepts {
			return
		}

		// 1) Symlink pointing into the sensitive area.
		sl := wire.FileInfo{
			Index:         0,
			RelativePath:  "evil",
			IsSymlink:     true,
			SymlinkTarget: sensitiveDir,
		}
		_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &sl)

		// Read FILE_ACCEPT (we don't actually use it; just drain).
		var d wire.FileAcceptDecision
		_, _ = wire.ReadControl(sender.Control, &d)

		// 2) Regular file whose path traverses through the symlink.
		// Sender claims this should land at <dstDir>/evil/leaked.txt,
		// which resolves through the symlink to sensitiveDir/leaked.txt.
		payload := []byte("PWND!")
		// Pre-compute the BLAKE3 root the receiver will verify against.
		root := blakeHash32(payload)
		f := wire.FileInfo{
			Index:        1,
			RelativePath: "evil/leaked.txt",
			Size:         uint64(len(payload)),
			Mode:         0o644,
			Blake3Root:   root,
			Resumable:    true,
		}
		_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &f)

		// Read the second FILE_ACCEPT (or ERROR).
		_, _ = wire.ReadControl(sender.Control, &d)

		// Send one chunk with the full payload + last-flag.
		chunk := &wire.Chunk{
			FileIndex:  1,
			ChunkIndex: 0,
			Flags:      wire.FlagLastChunk,
			Blake3Hash: blakeHash32(payload),
			Payload:    payload,
		}
		_ = wire.WriteChunk(sender.Data, chunk)

		_ = wire.WriteControl(sender.Control, wire.TypeTransferComplete, nil)
		_ = sender.Control.Close()
		_ = sender.Data.Close()
	}()

	wg.Wait()

	// Verdict: nothing under sensitiveDir.
	leaked := filepath.Join(sensitiveDir, "leaked.txt")
	if _, err := os.Stat(leaked); err == nil {
		t.Fatalf("SECURITY: receiver wrote outside TargetDir via symlink: %s", leaked)
	}
	// The receiver should have returned a path-traversal error somewhere.
	t.Logf("recv returned: %v", recvErr)
}

// TestRecv_RefusesPreplantedPartialSymlink locks in the Lstat gate on
// the .fsend-partial sidecar. Without it, a process with write access
// to TargetDir can plant the sidecar as a symlink and have chunk writes
// land on the link's target — outside TargetDir, on any file the
// receiver process can write.
//
// Uses a payload larger than the victim so the "stale partial — discard"
// branch in recv can't mask the bug.
func TestRecv_RefusesPreplantedPartialSymlink(t *testing.T) {
	dstDir := t.TempDir()
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "secret.txt")
	original := []byte("VICTIM_ORIGINAL_DATA")
	if err := os.WriteFile(victim, original, 0o600); err != nil {
		t.Fatal(err)
	}

	// Pre-plant the sidecar as a symlink to the victim.
	partial := filepath.Join(dstDir, "payload.bin"+partialSuffix)
	if err := os.Symlink(victim, partial); err != nil {
		t.Fatal(err)
	}

	sender, receiver := pipePair()
	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		TransferKind:    wire.TransferSingleFile,
		TotalFiles:      1,
	}

	payload := make([]byte, len(original)+64)
	for i := range payload {
		payload[i] = byte(i)
	}
	root := blakeHash32(payload)

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
		_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &wire.FileInfo{
			Index:        0,
			RelativePath: "payload.bin",
			Size:         uint64(len(payload)),
			Mode:         0o644,
			Blake3Root:   root,
			Resumable:    true,
		})
		// Drain FILE_ACCEPT or ERROR; verdict is the victim's contents.
		var d wire.FileAcceptDecision
		_, _ = wire.ReadControl(sender.Control, &d)
		_ = wire.WriteChunk(sender.Data, &wire.Chunk{
			FileIndex:  0,
			ChunkIndex: 0,
			Flags:      wire.FlagLastChunk,
			Blake3Hash: blakeHash32(payload),
			Payload:    payload,
		})
		_ = wire.WriteControl(sender.Control, wire.TypeTransferComplete, nil)
	}()

	wg.Wait()

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("victim disappeared: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("SECURITY: victim file was modified through pre-planted symlink\n  before: %q\n  after:  %q", original, got)
	}
	if recvErr == nil {
		t.Fatalf("expected receiver to surface an error, got nil")
	}
	t.Logf("recv returned: %v", recvErr)
}
