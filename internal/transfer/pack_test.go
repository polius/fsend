package transfer

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

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

// slowWriter stands in for a link slower than compression.
type slowWriter struct {
	bytes.Buffer
	per time.Duration
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.per)
	return w.Buffer.Write(p)
}

// On a link slower than one encoder, compression stays on one: more
// encoders would only hold memory while the network is the limit.
func TestPack_SlowLinkKeepsOneEncoder(t *testing.T) {
	data := textBytes(10 * hashBlockSize)
	// Slower than one encoder however fast this machine (or the race
	// detector) compresses: a write takes three times a chunk's compression.
	pool := newEncoderPool()
	start := time.Now()
	_ = pool.compress(data[:hashBlockSize], nil)
	w := &slowWriter{per: max(10*time.Millisecond, 3*time.Since(start))}
	made, err := packAll(t, w, data)
	if err != nil {
		t.Fatal(err)
	}
	if made != 1 {
		t.Errorf("encoders made = %d, want 1", made)
	}
	chunks, compressed, plain := readPacked(t, &w.Buffer)
	if compressed != chunks || !bytes.Equal(plain, data) {
		t.Errorf("compressed %d of %d chunks, round trip equal %v", compressed, chunks, bytes.Equal(plain, data))
	}
}

// A chunk that compresses far less than the one before outgrows the
// buffer its copy was given: it is grown, and the bytes still round-trip.
func TestPack_CompressedCopyOutgrowsItsBuffer(t *testing.T) {
	var data []byte
	for i := range 6 {
		chunk := textBytes(hashBlockSize)
		if i%2 == 1 { // about half incompressible: shrinks to a little over half
			copy(chunk[:hashBlockSize/2], randBytes(hashBlockSize/2))
		}
		data = append(data, chunk...)
	}
	var out bytes.Buffer
	if _, err := packAll(t, &out, data); err != nil {
		t.Fatal(err)
	}
	chunks, compressed, plain := readPacked(t, &out)
	if !bytes.Equal(plain, data) {
		t.Fatal("round trip differs")
	}
	if compressed != chunks {
		t.Errorf("compressed %d of %d chunks", compressed, chunks)
	}
}

func TestZFor(t *testing.T) {
	if got := zFor(hashBlockSize, 0); got != hashBlockSize+blockSlack {
		t.Errorf("no history: %d, want a full block", got)
	}
	if got := zFor(hashBlockSize, 180<<10); got > 256<<10 || got < 180<<10 {
		t.Errorf("after a 180 KiB copy: %d, want room for it in the 256 KiB class", got)
	}
	if got := zFor(1000, 200<<10); got > 1000+1000>>8+64 {
		t.Errorf("small chunk: %d, want at most its own bound", got)
	}
	if b := getZ(100 << 10); cap(b) != 128<<10 {
		t.Errorf("getZ(100 KiB) cap = %d", cap(b))
	}
	if b := getZ(900 << 10); cap(b) != hashBlockSize+blockSlack {
		t.Errorf("getZ(900 KiB) cap = %d, want a block", cap(b))
	}
}
