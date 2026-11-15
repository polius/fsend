package transfer

import (
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polius/fsend/internal/conv"
)

// A transfer retried after a connection drop starts over on a new
// connection, in the same process: the receiver hashes the prefix of its
// partial again, the sender the same prefix of its source, and under
// --checksum both hash every file they compare. For a 50 GB file dropped
// at 40 GB that was 80 GB of reading before the first new byte moved.
// blockCache keeps the block hashes this process has already computed, so
// a retry reads only what it has not hashed yet.
//
// An entry is used only for the same file in the same state: same inode,
// size, modification time and (where the OS reports it) inode change time
// — what the sender's change detection compares, plus the change time,
// which a tool that resets the modification time can't forge. Timestamps
// can't tell two changes within one clock tick apart, so a file touched
// in the moments before it was hashed is not cached at all (settled).
// The exceptions are changes this process makes itself: the receiver's
// partial, which it wrote in its owner-only state folder (rememberBlocks),
// and the modification time --checksum sets on a file it has just found
// identical (retimed).
type blockCache struct {
	mu    sync.Mutex
	files map[string]*cachedBlocks
	cost  int // bytes held, bounded by hashCacheBudget
	off   bool
}

// cachedBlocks is the hashes of a file's leading blocks. Every block is
// whole, except that the last may end at the file's end.
type cachedBlocks struct {
	id     fileID
	blocks [][32]byte
}

// hashCache serves every transfer in this process.
var hashCache = &blockCache{}

// hashCacheBudget bounds the cache's memory: 32 bytes per MiB hashed, so
// this covers hundreds of GiB in a few files, or a hundred thousand small
// files of a tree compared under --checksum. Past it the cache starts over:
// what a retry needs most is what was hashed last, the file in flight when
// the connection dropped.
const hashCacheBudget = 32 << 20

// racyWindow is how long before hashing a file must have been left alone
// for its hashes to be remembered: longer than the coarsest timestamps in
// use (2 s on FAT), so a later change always shows in them. A variable so
// tests can hash files they have just made.
var racyWindow = 3 * time.Second

// hashedBytes counts the bytes read from disk and hashed by hashBlocksFrom
// — what the cache exists to avoid. Tests and benchmarks read it.
var hashedBytes atomic.Int64

// fileID is a file in one state.
type fileID struct {
	fi    os.FileInfo
	ctime int64 // inode change time in ns; 0 where the OS doesn't report it
}

func identify(fi os.FileInfo) fileID { return fileID{fi: fi, ctime: changeTime(fi)} }

func (a fileID) same(b fileID) bool {
	return a.fi != nil && b.fi != nil && a.fi.Size() == b.fi.Size() && a.fi.ModTime().Equal(b.fi.ModTime()) &&
		a.ctime == b.ctime && os.SameFile(a.fi, b.fi)
}

// settled reports whether the file was last touched more than racyWindow
// before t, the moment its hashing began.
func (a fileID) settled(t time.Time) bool {
	limit := t.Add(-racyWindow)
	return a.fi.ModTime().Before(limit) && (a.ctime == 0 || time.Unix(0, a.ctime).Before(limit))
}

// get returns up to want cached block hashes for path, if it is still the
// file they were computed from.
func (c *blockCache) get(path string, id fileID, want int) [][32]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.files[path]
	if e == nil || c.off {
		return nil
	}
	if !e.id.same(id) {
		c.drop(path, e)
		return nil
	}
	return append([][32]byte(nil), e.blocks[:min(want, len(e.blocks))]...)
}

// put remembers blocks for path in state id, unless more is already known
// for that state.
func (c *blockCache) put(path string, id fileID, blocks [][32]byte) {
	if len(blocks) == 0 || id.fi == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.off {
		return
	}
	if e := c.files[path]; e != nil {
		if e.id.same(id) && len(e.blocks) >= len(blocks) {
			return
		}
		c.drop(path, e)
	}
	e := &cachedBlocks{id: id, blocks: append([][32]byte(nil), blocks...)}
	if entryCost(path, e) > hashCacheBudget {
		return
	}
	if c.files == nil || c.cost+entryCost(path, e) > hashCacheBudget {
		c.files, c.cost = make(map[string]*cachedBlocks), 0
	}
	c.files[path] = e
	c.cost += entryCost(path, e)
}

func (c *blockCache) drop(path string, e *cachedBlocks) {
	delete(c.files, path)
	c.cost -= entryCost(path, e)
}

// entryCost estimates the memory one entry holds.
func entryCost(path string, e *cachedBlocks) int { return len(path) + 32*len(e.blocks) + 256 }

// rememberBlocks records blocks as the leading whole blocks of the file at
// path, as it is now, for a file this process has just written itself: the
// hashes are of the bytes it wrote, not read back. Used for the receiver's
// partial when a connection drops, so the retry needn't read it again.
// They replace whatever was known of the file: it may have been rewritten
// (a restart) to the same size within one tick of a coarse clock, which
// its timestamps would not show.
func (c *blockCache) rememberBlocks(path string, blocks [][32]byte) {
	c.mu.Lock()
	if e := c.files[path]; e != nil {
		c.drop(path, e)
	}
	c.mu.Unlock()
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	c.put(path, identify(fi), blocks[:min(len(blocks), int(fi.Size()/hashBlockSize))])
}

// retimed carries what is known of the file at path over a change of its
// modification time made by this process; was is the file just before that
// change. --checksum gives a file it found identical the sender's time,
// which would otherwise make the file look changed and have a retry hash
// it a second time. The hashes are kept only if they are those of the file
// as was describes it, and it is still that file at that size.
func (c *blockCache) retimed(path string, was os.FileInfo) {
	now, err := os.Lstat(path)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.files[path]
	if e == nil {
		return
	}
	if !e.id.same(identify(was)) || !os.SameFile(was, now) || was.Size() != now.Size() {
		c.drop(path, e)
		return
	}
	e.id = identify(now)
}

// hashPrefix returns the block hashes of the first n bytes of the file at
// path, reading only the blocks not hashed earlier in this process, and
// the state the file was in when it began. It stops within one block of
// ctx ending, and what it had hashed by then is remembered for the next
// attempt. progress, when set, receives the bytes covered so far (cached
// ones included).
func (c *blockCache) hashPrefix(ctx context.Context, path string, n int64, progress func(int64)) ([][32]byte, fileID, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fileID{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fileID{}, err
	}
	blocks, err := c.hashOpen(ctx, f, fi, path, n, progress)
	return blocks, identify(fi), err
}

// hashOpen is hashPrefix on an open file whose Stat is fi.
func (c *blockCache) hashOpen(ctx context.Context, f *os.File, fi os.FileInfo, path string, n int64, progress func(int64)) ([][32]byte, error) {
	began := time.Now()
	id := identify(fi)
	// Cached blocks are whole ones; the short block that ends the file is
	// only the one wanted when n is the file's size.
	want := int(n / hashBlockSize)
	if n == fi.Size() && n%hashBlockSize != 0 {
		want++
	}
	blocks := c.get(path, id, want)
	done := min(int64(len(blocks))*hashBlockSize, n)
	if progress != nil && done > 0 {
		progress(done)
	}
	if done == n {
		return blocks, nil
	}
	rest, err := hashBlocksFrom(ctx, f, done, n, progress)
	blocks = append(blocks, rest...)
	// Remember what was read, if the file kept still: whole blocks, and a
	// short last one only when it ends the file.
	keep := blocks
	if whole := int(n / hashBlockSize); err == nil && n != fi.Size() && len(keep) > whole {
		keep = keep[:whole]
	}
	if len(rest) > 0 && id.settled(began) {
		if after, serr := f.Stat(); serr == nil && identify(after).same(id) {
			c.put(path, id, keep)
		}
	}
	if err != nil {
		return nil, err
	}
	return blocks, nil
}

// hashBlocksFrom reads bytes [from, to) of f — from is block-aligned — and
// returns their block hashes, reading sequentially and hashing in
// parallel. When ctx ends it stops before the next block and returns the
// hashes of the blocks it finished along with the context's error.
func hashBlocksFrom(ctx context.Context, f *os.File, from, to int64, progress func(int64)) ([][32]byte, error) {
	h := newFileHasher()
	done := from
	var err error
	for done < to {
		if err = ctx.Err(); err != nil {
			break
		}
		b := getBlock()[:min(int64(hashBlockSize), to-done)]
		if _, err = io.ReadFull(io.NewSectionReader(f, done, int64(len(b))), b); err != nil {
			putBlock(b)
			break
		}
		h.addBlock(b)
		hashedBytes.Add(int64(len(b)))
		done += int64(len(b))
		if progress != nil {
			progress(done)
		}
	}
	return h.blocks(), err
}

// fileRoot returns the tree root of the whole file at path (--checksum),
// through the cache.
func (c *blockCache) fileRoot(ctx context.Context, path string, progress func(int64)) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return [32]byte{}, err
	}
	blocks, err := c.hashOpen(ctx, f, st, path, st.Size(), progress)
	if err != nil {
		return [32]byte{}, err
	}
	return foldRoot(blocks, conv.Uint64(st.Size())), nil
}
