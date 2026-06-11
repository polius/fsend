package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// TestSend_AdvertisesMultiFileNames asserts that a multi-file HELLO
// carries the bare file names, so the receiver's consent prompt can
// show what would land instead of a blind "N files".
func TestSend_AdvertisesMultiFileNames(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	var paths []string
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		p := filepath.Join(srcDir, n)
		if err := os.WriteFile(p, []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	items, err := Walk(paths)
	if err != nil {
		t.Fatal(err)
	}

	a, b := pipePair()
	defer a.Close()
	defer b.Close()

	var gotHello wire.SenderHello
	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sendErr = Send(context.Background(), &a, SendOptions{
			Items:        items,
			TransferKind: wire.TransferMultiFile,
		})
	}()
	go func() {
		defer wg.Done()
		recvErr = Recv(context.Background(), &b, RecvOptions{
			TargetDir: dstDir,
			Accept: func(h wire.SenderHello) bool {
				gotHello = h
				return true
			},
		})
	}()
	wg.Wait()

	if sendErr != nil || recvErr != nil {
		t.Fatalf("send=%v recv=%v", sendErr, recvErr)
	}
	want := []string{"a.txt", "b.txt", "c.txt"}
	if !reflect.DeepEqual(gotHello.FileNames, want) {
		t.Errorf("HELLO FileNames = %v, want %v", gotHello.FileNames, want)
	}
}

// TestRecv_RejectsUnadvertisedName locks in consent integrity: when the
// HELLO carried the complete name list, a FILE_INFO whose RelativePath
// is not among the advertised names is refused before any byte lands.
func TestRecv_RejectsUnadvertisedName(t *testing.T) {
	dstDir := t.TempDir()

	sender, receiver := pipePair()

	var recvErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		recvErr = Recv(context.Background(), &receiver, RecvOptions{
			TargetDir: dstDir,
			Accept:    func(wire.SenderHello) bool { return true },
		})
		_ = receiver.Control.Close()
		_ = receiver.Data.Close()
	}()
	go func() {
		defer wg.Done()
		defer sender.Control.Close()
		defer sender.Data.Close()
		hello := &wire.SenderHello{
			ProtocolVersion: wire.ProtocolVersion,
			TransferKind:    wire.TransferMultiFile,
			TotalFiles:      1,
			FileNames:       []string{"advertised.txt"},
		}
		_ = wire.WriteControl(sender.Control, wire.TypeHello, hello)
		var ack wire.ReceiverHello
		_, _ = wire.ReadControl(sender.Control, &ack)
		if !ack.Accepts {
			return
		}
		payload := []byte("not what you agreed to")
		f := wire.FileInfo{
			Index:        0,
			RelativePath: "sneaky.txt",
			Size:         uint64(len(payload)),
			Mode:         0o644,
			Blake3Root:   blakeHash32(payload),
			Resumable:    true,
		}
		_ = wire.WriteControl(sender.Control, wire.TypeFileInfo, &f)
		// Receiver should answer with ERROR, not FILE_ACCEPT.
		var d wire.FileAcceptDecision
		_, _ = wire.ReadControl(sender.Control, &d)
	}()
	wg.Wait()

	if !errors.Is(recvErr, fserrors.ErrProtocolError) {
		t.Errorf("want ErrProtocolError, got %v", recvErr)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "sneaky.txt")); !os.IsNotExist(err) {
		t.Errorf("unadvertised file must not land, stat err=%v", err)
	}
}
