package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/wire"
)

// pipePair returns two Streams that talk to each other over in-memory pipes.
// Returned as (alice, bob): alice.X is connected to bob.X.
func pipePair() (Streams, Streams) {
	ctlA, ctlB := newDuplexPipe()
	dataA, dataB := newDuplexPipe()
	rcA, rcB := newDuplexPipe()
	return Streams{Control: ctlA, Data: dataA, ReceiverControl: rcA},
		Streams{Control: ctlB, Data: dataB, ReceiverControl: rcB}
}

// duplexPipe is a bidirectional io.ReadWriteCloser pair built from two
// io.Pipe pairs.
type duplexPipe struct {
	io.Reader
	io.Writer
	closer func() error
}

func (d *duplexPipe) Close() error { return d.closer() }

func newDuplexPipe() (*duplexPipe, *duplexPipe) {
	a2bR, a2bW := io.Pipe()
	b2aR, b2aW := io.Pipe()
	a := &duplexPipe{
		Reader: b2aR,
		Writer: a2bW,
		closer: func() error {
			_ = a2bW.Close()
			_ = b2aR.Close()
			return nil
		},
	}
	b := &duplexPipe{
		Reader: a2bR,
		Writer: b2aW,
		closer: func() error {
			_ = b2aW.Close()
			_ = a2bR.Close()
			return nil
		},
	}
	return a, b
}

// TestSingleFileTransfer_Roundtrip is the canonical end-to-end happy-path
// test: send a 5 MB random file, receive it on the other side, assert
// byte-perfect equality and BLAKE3 verification.
func TestSingleFileTransfer_Roundtrip(t *testing.T) {
	const fileSize = 5 * 1024 * 1024 // 5 MB — exercises multiple chunks

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "report.pdf")

	payload := make([]byte, fileSize)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := Walk([]string{srcPath})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	senderStreams, receiverStreams := pipePair()
	defer senderStreams.Close()
	defer receiverStreams.Close()

	var wg sync.WaitGroup
	var sendErr, recvErr error
	wg.Add(2)

	go func() {
		defer wg.Done()
		sendErr = Send(context.Background(), &senderStreams, SendOptions{
			Items:         items,
			Hostname:      "alice",
			OS:            "darwin",
			ClientVersion: "0.1.0-test",
			TransferKind:  wire.TransferSingleFile,
		})
	}()
	go func() {
		defer wg.Done()
		recvErr = Recv(context.Background(), &receiverStreams, RecvOptions{
			Hostname:      "bob",
			OS:            "linux",
			ClientVersion: "0.1.0-test",
			TargetDir:     dstDir,
		})
	}()

	wg.Wait()

	if sendErr != nil {
		t.Errorf("Send: %v", sendErr)
	}
	if recvErr != nil {
		t.Errorf("Recv: %v", recvErr)
	}

	dst, err := os.ReadFile(filepath.Join(dstDir, "report.pdf"))
	if err != nil {
		t.Fatalf("ReadFile destination: %v", err)
	}
	if !bytes.Equal(dst, payload) {
		t.Fatalf("destination bytes differ from source (lengths: src=%d dst=%d)", len(payload), len(dst))
	}

	// Independent BLAKE3 check of the received file.
	want := blakeFile(t, srcPath)
	got := blakeFile(t, filepath.Join(dstDir, "report.pdf"))
	if want != got {
		t.Errorf("BLAKE3 mismatch")
	}
}

// TestEmptyFileTransfer makes sure 0-byte files round-trip correctly.
func TestEmptyFileTransfer(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "empty.txt")
	if err := os.WriteFile(srcPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := Walk([]string{srcPath})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	a, b := pipePair()
	defer a.Close()
	defer b.Close()

	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sendErr = Send(context.Background(), &a, SendOptions{Items: items, TransferKind: wire.TransferSingleFile}) }()
	go func() { defer wg.Done(); recvErr = Recv(context.Background(), &b, RecvOptions{TargetDir: dstDir}) }()
	wg.Wait()
	if sendErr != nil {
		t.Errorf("Send: %v", sendErr)
	}
	if recvErr != nil {
		t.Errorf("Recv: %v", recvErr)
	}
	info, err := os.Stat(filepath.Join(dstDir, "empty.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("expected 0-byte file, got %d bytes", info.Size())
	}
}

// TestDirectoryTransfer sends a small directory tree and checks structure.
func TestDirectoryTransfer(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	tree := map[string][]byte{
		"src/main.go":             []byte("package main\nfunc main() {}\n"),
		"src/sub/helper.go":       bytes.Repeat([]byte("h"), 1024),
		"docs/README.md":          []byte("# hi\n"),
		"assets/big.bin":          bytes.Repeat([]byte{0xAB}, 2*1024*1024),
	}
	for rel, content := range tree {
		full := filepath.Join(srcDir, "myproject", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	items, err := Walk([]string{filepath.Join(srcDir, "myproject")})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	a, b := pipePair()
	defer a.Close()
	defer b.Close()

	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sendErr = Send(context.Background(), &a, SendOptions{Items: items, TransferKind: wire.TransferDirectory}) }()
	go func() { defer wg.Done(); recvErr = Recv(context.Background(), &b, RecvOptions{TargetDir: dstDir}) }()
	wg.Wait()
	if sendErr != nil {
		t.Errorf("Send: %v", sendErr)
	}
	if recvErr != nil {
		t.Errorf("Recv: %v", recvErr)
	}

	for rel, want := range tree {
		got, err := os.ReadFile(filepath.Join(dstDir, "myproject", rel))
		if err != nil {
			t.Errorf("missing destination file %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("content mismatch for %s", rel)
		}
	}
}

// TestReceiverDeclines exercises the receiver-aborts path.
func TestReceiverDeclines(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "x.txt")
	_ = os.WriteFile(srcPath, []byte("hi"), 0o644)
	items, _ := Walk([]string{srcPath})

	a, b := pipePair()
	defer a.Close()
	defer b.Close()

	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sendErr = Send(context.Background(), &a, SendOptions{Items: items, TransferKind: wire.TransferSingleFile})
	}()
	go func() {
		defer wg.Done()
		recvErr = Recv(context.Background(), &b, RecvOptions{
			TargetDir: t.TempDir(),
			Accept:    func(wire.SenderHello) bool { return false },
		})
	}()
	wg.Wait()
	if sendErr == nil {
		t.Error("expected Send to fail when receiver declines")
	}
	if recvErr == nil {
		t.Error("expected Recv to surface decline as error")
	}
}

// TestPathTraversalRejected confirms that a malicious peer attempting to
// write outside the target dir is blocked by SanitizeRelativePath.
func TestSanitizeRelativePath(t *testing.T) {
	bad := []string{
		"../etc/passwd",
		"/etc/passwd",
		"a/../../etc/passwd",
		"",
		"C:\\Windows\\System32\\evil.dll",
	}
	for _, p := range bad {
		if _, err := SanitizeRelativePath(p); err == nil {
			t.Errorf("expected SanitizeRelativePath(%q) to fail", p)
		}
	}
	good := []string{
		"file.txt",
		"sub/dir/file.txt",
		"./file.txt", // gets cleaned to "file.txt"
	}
	for _, p := range good {
		if _, err := SanitizeRelativePath(p); err != nil {
			t.Errorf("SanitizeRelativePath(%q) unexpectedly failed: %v", p, err)
		}
	}
}

func blakeFile(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
