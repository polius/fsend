package transfer

import (
	"encoding/binary"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/wire"

	"github.com/polius/fsend/internal/conv"
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

// blockSlack is the room a pooled block has past hashBlockSize, for the
// zstd framing of a chunk that doesn't compress: 37 bytes measured on a
// full chunk, and zstd's own bound is size/256. Without it the compressed
// copy of every incompressible chunk outgrew its block and was allocated
// afresh (and, at the wrong size for the pool, thrown away).
const blockSlack = hashBlockSize >> 8

// freeBlocks recycles hashBlockSize buffers across reads, hashing and
// chunks. It is a plain free list, not a sync.Pool: a pool is emptied at
// every garbage collection, so a transfer kept dropping its 1 MiB blocks
// and allocating them again, and the heap ran at about twice the blocks in
// use. The list keeps what a transfer has in flight at its peak (about a
// dozen) for the next block; past keepBlocks, freed blocks are left to the
// collector.
var freeBlocks = make(chan []byte, keepBlocks)

// keepBlocks bounds the free list: the blocks in flight on one side of a
// transfer, with room for a sender and receiver in one process.
const keepBlocks = 32

func getBlock() []byte {
	select {
	case b := <-freeBlocks:
		return b[:hashBlockSize]
	default:
		return make([]byte, hashBlockSize, hashBlockSize+blockSlack)
	}
}

func putBlock(b []byte) {
	if cap(b) != hashBlockSize+blockSlack {
		return
	}
	select {
	case freeBlocks <- b[:hashBlockSize]:
	default:
	}
}

func blockHash(b []byte) [32]byte {
	var out [32]byte
	h := blake3.New()
	_, _ = h.Write(b)
	h.Sum(out[:0])
	return out
}

// rootKeyed is a hasher keyed for rootContext, never written to: foldRoot
// starts each fold from a copy. Keying hashes the context string, and a
// hasher is about 10 KB: a fresh one per file was most of a folder send's
// garbage (over 120 MB for 12,000 files), and the collections it set off
// emptied the block pool, so 1 MiB buffers were allocated again and again.
var rootKeyed = blake3.NewDeriveKey(rootContext)

var rootHashers = sync.Pool{New: func() any { return new(blake3.Hasher) }}

// foldRoot folds block hashes and the byte count into the tree root.
func foldRoot(blocks [][32]byte, size uint64) [32]byte {
	h := rootHashers.Get().(*blake3.Hasher)
	defer rootHashers.Put(h)
	*h = *rootKeyed
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
	ahead   chan struct{} // bounds this file's blocks in flight; nil: hashSem alone
	results []*[32]byte
	cur     []byte // pooled buffer of the block being filled
	n       uint64
	wg      sync.WaitGroup
}

// newFileHasher hashes bytes on their way across the network, bounded by
// s's side of the transfer (Streams.hashAhead).
func newFileHasher(s *Streams) *fileHasher { return &fileHasher{ahead: s.hashAhead()} }

// newDiskHasher hashes a file read from disk for its own sake (resume,
// --checksum), on every core: nothing else limits it.
func newDiskHasher() *fileHasher { return &fileHasher{} }

// streamHashAhead bounds the blocks one side of a transfer has in hashing
// while they cross the network, across its files (a folder of files under a
// block each hashes many at once). Each holds a 1 MiB buffer, and the reader runs
// ahead of hashing until the bound stops it: a core count of them only
// held memory. Four out-hash the fastest link (one core does about
// 700 MB/s on arm64, more with AVX2). It bounds each side of a transfer,
// not the process: a sender and receiver in one process (tests,
// benchmarks) don't share it.
const streamHashAhead = 4

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
		h.n += conv.Uint64(int64(take))
		if len(h.cur) == hashBlockSize {
			h.dispatch(h.cur, putWhenHashed(h.cur))
			h.cur = nil
		}
	}
	return n, nil
}

// addBlock hands over a full, block-aligned buffer the caller won't touch
// again; pooled buffers go back to the pool once hashed.
func (h *fileHasher) addBlock(b []byte) { h.addShared(b, putWhenHashed(b)) }

// addShared hashes a block-aligned buffer that others read too (nobody
// writes it), in place of copying it in; done runs once it is hashed. It
// is a whole block, or the file's last (no more bytes are added after it). It is how a block on its way across the network is hashed
// without a second copy of it held.
func (h *fileHasher) addShared(b []byte, done func()) {
	if h.cur != nil {
		panic("fileHasher: addShared after a partial Write")
	}
	h.n += uint64(len(b))
	h.dispatch(b, done)
}

// aligned reports whether the next byte starts a block: a whole block can
// then be handed over (addShared) instead of written.
func (h *fileHasher) aligned() bool { return h.cur == nil }

func (h *fileHasher) dispatch(b []byte, done func()) {
	out := new([32]byte)
	h.results = append(h.results, out)
	if len(b) <= inlineHashMax {
		*out = blockHash(b)
		done()
		return
	}
	h.wg.Add(1)
	if h.ahead != nil {
		h.ahead <- struct{}{}
	}
	hashSem <- struct{}{}
	go func() {
		*out = blockHash(b)
		<-hashSem
		if h.ahead != nil {
			<-h.ahead
		}
		done()
		h.wg.Done()
	}()
}

// shareBlock returns a func that each of n users of the pooled block b
// calls when done with it; the last call returns b to the pool.
func shareBlock(b []byte, n int32) func() {
	var refs atomic.Int32
	refs.Store(n)
	return func() {
		if refs.Add(-1) == 0 {
			putBlock(b)
		}
	}
}

// putWhenHashed returns b to the pool once hashed.
func putWhenHashed(b []byte) func() { return func() { putBlock(b) } }

// blocks flushes the trailing partial block and returns every block hash.
func (h *fileHasher) blocks() [][32]byte {
	if h.cur != nil {
		h.dispatch(h.cur, putWhenHashed(h.cur))
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
