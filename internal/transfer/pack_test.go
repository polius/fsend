package transfer

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/wire"
)

// packAll runs data through a packer on a fresh encoder pool and returns
// the chunks written and how many encoders the pool made.
func packAll(t *testing.T, w io.Writer, data []byte) (made int, err error) {
	t.Helper()
	pool := newEncoderPool()
	p := &chunkPacker{buf: getBlock()[:0], out: make(chan packed, 2), done: make(chan struct{})}
	go p.writeLoop(p.out, w, pool)
	for len(data) > 0 {
		n := min(len(data), hashBlockSize)
		if err = p.appendBytes(0, data[:n]); err != nil {
			break
		}
		data = data[n:]
	}
	if err == nil {
		err = p.flush()
	}
	if cerr := p.Close(); err == nil {
		err = cerr
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.made, err
}

func readPacked(t *testing.T, r io.Reader) (chunks, compressed int, plain []byte) {
	t.Helper()
	var dec *zstd.Decoder
	defer func() {
		if dec != nil {
			dec.Close()
		}
	}()
	for {
		c, err := wire.ReadChunk(r)
		if errors.Is(err, io.EOF) {
			return chunks, compressed, plain
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks++
		if c.Compressed {
			compressed++
		}
		out, err := decodeChunkPayload(c, &dec, make([]byte, 0, hashBlockSize))
		if err != nil {
			t.Fatal(err)
		}
		plain = append(plain, out...)
	}
}

// Data that doesn't compress wakes no encoder: one chunk probes it (its
// bytes alone answer), and the others wait for the answer instead of each
// trying at once.
func TestPack_IncompressibleWakesNoEncoder(t *testing.T) {
	data := randBytes(24 * hashBlockSize)
	var out bytes.Buffer
	made, err := packAll(t, &out, data)
	if err != nil {
		t.Fatal(err)
	}
	if made != 0 {
		t.Errorf("encoders made = %d, want 0", made)
	}
	chunks, compressed, plain := readPacked(t, &out)
	if compressed != 0 || chunks != 24 || !bytes.Equal(plain, data) {
		t.Errorf("chunks %d (compressed %d), round trip equal %v", chunks, compressed, bytes.Equal(plain, data))
	}
}

// Data that compresses still goes compressed, on more than one worker.
func TestPack_CompressibleUsesWorkers(t *testing.T) {
	if packWorkers() < 2 {
		t.Skip("one core")
	}
	data := textBytes(24 * hashBlockSize)
	var out bytes.Buffer
	made, err := packAll(t, &out, data)
	if err != nil {
		t.Fatal(err)
	}
	if made < 2 {
		t.Errorf("encoders made = %d, want several", made)
	}
	chunks, compressed, plain := readPacked(t, &out)
	if compressed != chunks || !bytes.Equal(plain, data) {
		t.Errorf("compressed %d of %d chunks, round trip equal %v", compressed, chunks, bytes.Equal(plain, data))
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("connection lost") }

// A failed connection doesn't strand a chunk waiting on a probe's answer.
func TestPack_WriteFailureDoesNotHang(t *testing.T) {
	_, err := packAll(t, failWriter{}, randBytes(16*hashBlockSize))
	if err == nil {
		t.Fatal("want the write error")
	}
}

// foldRoot starts each fold from a copy of one keyed hasher: reusing a
// pooled hasher must give the same root as a freshly keyed one, every time.
func TestFoldRoot_SameAsFreshHasher(t *testing.T) {
	blocks := [][32]byte{blockHash([]byte("a")), blockHash([]byte("b"))}
	h := blake3.NewDeriveKey(rootContext)
	for i := range blocks {
		_, _ = h.Write(blocks[i][:])
	}
	_, _ = h.Write([]byte{3, 0, 0, 0, 0, 0, 0, 0})
	var want [32]byte
	h.Sum(want[:0])
	for range 3 {
		if got := foldRoot(blocks, 3); got != want {
			t.Fatalf("foldRoot = %x, want %x", got, want)
		}
	}
	if got := foldRoot(nil, 0); got == want {
		t.Fatal("a different input gave the same root")
	}
}

func TestLooksIncompressible(t *testing.T) {
	if !looksIncompressible(randBytes(hashBlockSize)) {
		t.Error("random bytes: want incompressible")
	}
	if looksIncompressible(textBytes(hashBlockSize)) {
		t.Error("text: want compressible")
	}
	if looksIncompressible(make([]byte, hashBlockSize)) {
		t.Error("zeros: want compressible")
	}
	if looksIncompressible(randBytes(1000)) {
		t.Error("a short chunk is left to zstd")
	}
}
