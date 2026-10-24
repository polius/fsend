package transfer

import (
	"encoding/binary"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/wire"
)

// File hash tree. A file's root is
//
//	BLAKE3-derive-key("fsend v3 file root", h0 ‖ h1 ‖ … ‖ uint64le(size))
//
// where hi is the plain BLAKE3 of the i-th hashBlockSize block. Blocks hash
// independently, so both peers spread the work across cores and hash each
// byte once; a resume offset is block-aligned, so a prefix root is the same
// fold over the first k blocks.
const hashBlockSize = wire.MaxChunkSize

const rootContext = "fsend v3 file root"

// inlineHashMax is the block size below which hashing on the caller's
// goroutine beats handing the block to a worker.
const inlineHashMax = 64 << 10

// hashSem bounds concurrent block hashing process-wide.
var hashSem = make(chan struct{}, runtime.GOMAXPROCS(0))

// blockPool recycles hashBlockSize buffers across reads, hashing and chunks.
var blockPool = sync.Pool{New: func() any {
	b := make([]byte, hashBlockSize)
	return &b
}}

func getBlock() []byte { return (*blockPool.Get().(*[]byte))[:hashBlockSize] }

func putBlock(b []byte) {
	if cap(b) == hashBlockSize {
		b = b[:hashBlockSize]
		blockPool.Put(&b)
	}
}

func blockHash(b []byte) [32]byte {
	var out [32]byte
	h := blake3.New()
	_, _ = h.Write(b)
	h.Sum(out[:0])
	return out
}

// foldRoot folds block hashes and the byte count into the tree root.
func foldRoot(blocks [][32]byte, size uint64) [32]byte {
	h := blake3.NewDeriveKey(rootContext)
	for i := range blocks {
		_, _ = h.Write(blocks[i][:])
	}
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], size)
	_, _ = h.Write(n[:])
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// fileHasher computes a file's tree root from bytes written in order.
// Full blocks are hashed on worker goroutines; Sum waits for them.
type fileHasher struct {
	results []*[32]byte
	cur     []byte // pooled buffer of the block being filled
	n       uint64
	wg      sync.WaitGroup
}

func newFileHasher() *fileHasher { return &fileHasher{} }

// seed starts the hasher after an already-hashed, block-aligned prefix.
func (h *fileHasher) seed(blocks [][32]byte) {
	for i := range blocks {
		b := blocks[i]
		h.results = append(h.results, &b)
	}
	h.n = uint64(len(blocks)) * hashBlockSize
}

func (h *fileHasher) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if h.cur == nil {
			h.cur = getBlock()[:0]
		}
		take := min(len(p), hashBlockSize-len(h.cur))
		h.cur = append(h.cur, p[:take]...)
		p = p[take:]
		h.n += uint64(take)
		if len(h.cur) == hashBlockSize {
			h.dispatch(h.cur, true)
			h.cur = nil
		}
	}
	return n, nil
}

// addBlock hands over a full, block-aligned buffer the caller won't touch
// again; pooled buffers go back to the pool once hashed.
func (h *fileHasher) addBlock(b []byte) {
	if h.cur != nil {
		panic("fileHasher: addBlock after a partial Write")
	}
	h.n += uint64(len(b))
	h.dispatch(b, true)
}

func (h *fileHasher) dispatch(b []byte, pooled bool) {
	out := new([32]byte)
	h.results = append(h.results, out)
	if len(b) <= inlineHashMax {
		*out = blockHash(b)
		if pooled {
			putBlock(b)
		}
		return
	}
	h.wg.Add(1)
	hashSem <- struct{}{}
	go func() {
		*out = blockHash(b)
		<-hashSem
		if pooled {
			putBlock(b)
		}
		h.wg.Done()
	}()
}

// blocks flushes the trailing partial block and returns every block hash.
func (h *fileHasher) blocks() [][32]byte {
	if h.cur != nil {
		h.dispatch(h.cur, true)
		h.cur = nil
	}
	h.wg.Wait()
	out := make([][32]byte, len(h.results))
	for i, r := range h.results {
		out[i] = *r
	}
	return out
}

// Sum returns the root of everything written so far. Call once.
func (h *fileHasher) Sum() [32]byte { return foldRoot(h.blocks(), h.n) }

// discard waits for in-flight blocks so their buffers return to the pool.
func (h *fileHasher) discard() {
	if h.cur != nil {
		putBlock(h.cur)
		h.cur = nil
	}
	h.wg.Wait()
}

// hashBlocks reads the first n bytes of f (from offset 0) and returns their
// block hashes, reading sequentially and hashing in parallel. progress, when
// set, receives bytes read so far.
func hashBlocks(f *os.File, n int64, progress func(int64)) ([][32]byte, error) {
	h := newFileHasher()
	var done int64
	for done < n {
		b := getBlock()[:min(int64(hashBlockSize), n-done)]
		if _, err := io.ReadFull(io.NewSectionReader(f, done, int64(len(b))), b); err != nil {
			putBlock(b)
			h.discard()
			return nil, err
		}
		h.addBlock(b)
		done += int64(len(b))
		if progress != nil {
			progress(done)
		}
	}
	return h.blocks(), nil
}

// hashFileRoot returns the tree root of the file at path (--checksum).
func hashFileRoot(path string, progress func(int64)) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return [32]byte{}, err
	}
	blocks, err := hashBlocks(f, st.Size(), progress)
	if err != nil {
		return [32]byte{}, err
	}
	return foldRoot(blocks, uint64(st.Size())), nil
}

// bytesRoot is the tree root of an in-memory buffer (tests, small inputs).
func bytesRoot(b []byte) [32]byte {
	h := newFileHasher()
	_, _ = h.Write(b)
	return h.Sum()
}
