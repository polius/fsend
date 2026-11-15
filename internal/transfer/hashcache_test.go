package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// cutConn fails the data stream after it has carried a number of bytes,
// closing both of the sender's streams as a dropped connection would.
type cutConn struct {
	io.ReadWriteCloser
	left  atomic.Int64
	drops []io.Closer
}

func (c *cutConn) Write(p []byte) (int, error) {
	if c.left.Add(-int64(len(p))) < 0 {
		for _, d := range c.drops {
			_ = d.Close()
		}
		return 0, io.ErrClosedPipe
	}
	return c.ReadWriteCloser.Write(p)
}

// attempt runs one transfer of src into dst. cutAfter > 0 drops the
// connection once that many data bytes have gone out. It returns both
// errors and the offset the transfer resumed from (0 when it didn't).
func attempt(t *testing.T, src, dst string, cutAfter int64, checksum bool) (resumedAt uint64, se, re error) {
	t.Helper()
	sources, err := Walk([]string{src}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctrlA, ctrlB := net.Pipe()
	dataA, dataB := net.Pipe()
	var data io.ReadWriteCloser = dataA
	if cutAfter > 0 {
		c := &cutConn{ReadWriteCloser: dataA, drops: []io.Closer{dataA, ctrlA}}
		c.left.Store(cutAfter)
		data = c
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s := &Streams{Control: ctrlA, Data: data}
		se = Send(ctx, s, SendOptions{Mode: wire.ModeFiles, Sources: sources})
		s.Close()
	}()
	go func() {
		defer wg.Done()
		s := &Streams{Control: ctrlB, Data: dataB}
		re = Recv(ctx, s, RecvOptions{TargetDir: dst, Checksum: checksum,
			OnResume: func(_ uint32, off, _ uint64) { resumedAt = off }})
		s.Close()
	}()
	wg.Wait()
	return resumedAt, se, re
}

// The effect the cache exists for: a transfer dropped part-way and retried
// in the same process resumes without either side reading the prefix
// again. Without the cache each side reads it once more (the previous
// behaviour, shown by turning the cache off).
func TestRetry_DoesNotHashThePrefixAgain(t *testing.T) {
	const size = 24 << 20
	for _, cached := range []bool{true, false} {
		t.Run(fmt.Sprintf("cache=%v", cached), func(t *testing.T) {
			freshHashCache(t)
			hashCache.off = !cached
			src, dst := filepath.Join(t.TempDir(), "big.bin"), t.TempDir()
			data := randBytes(size)
			writeFile(t, src, data)

			_, se, re := attempt(t, src, dst, 16<<20, false)
			if se == nil || re == nil {
				t.Fatalf("the cut attempt finished: send=%v recv=%v", se, re)
			}
			before := hashedBytes.Load()
			off, se, re := attempt(t, src, dst, 0, false)
			if se != nil || re != nil {
				t.Fatalf("retry: send=%v recv=%v", se, re)
			}
			read := hashedBytes.Load() - before
			if off < 8<<20 {
				t.Fatalf("the retry resumed at %d; want most of the 16 MiB that crossed before the cut", off)
			}
			if !bytes.Equal(mustRead(t, filepath.Join(dst, "big.bin")), data) {
				t.Fatal("content mismatch after the retry")
			}
			t.Logf("resumed at %d MiB; the retry read %d MiB back to hash the prefix", off>>20, read>>20)
			if cached && read != 0 {
				t.Errorf("the retry read %d bytes to hash a prefix both sides had already hashed", read)
			}
			if !cached && read != 2*int64(off) {
				t.Errorf("without the cache the retry read %d bytes, want the prefix once per side (%d)", read, 2*off)
			}
		})
	}
}

// A source changed between attempts is hashed again, however it changed:
// the retry must not resume onto bytes that no longer match.
func TestRetry_ChangedSourceIsHashedAgain(t *testing.T) {
	freshHashCache(t)
	const size = 12 << 20
	src, dst := filepath.Join(t.TempDir(), "big.bin"), t.TempDir()
	data := randBytes(size)
	writeFile(t, src, data)
	if _, se, re := attempt(t, src, dst, 8<<20, false); se == nil || re == nil {
		t.Fatalf("the cut attempt finished: send=%v recv=%v", se, re)
	}

	// Same size, one byte of the already-sent prefix differs. Within the
	// same second the receiver still offers its partial (the listing
	// carries whole seconds), so only the sender's own check of the prefix
	// stands between the retry and a file made of two versions.
	was, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	data[100] ^= 0xFF
	f, err := os.OpenFile(src, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(data[100:101], 100); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	now, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if identify(now).same(identify(was)) {
		// Production never caches a file this fresh (racyWindow), which
		// freshHashCache turned off for the test.
		t.Skip("this filesystem's timestamps don't tell the two versions apart")
	}
	off, se, re := attempt(t, src, dst, 0, false)
	if se != nil || re != nil {
		t.Fatalf("retry: send=%v recv=%v", se, re)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "big.bin")), data) {
		t.Fatalf("the retry delivered stale bytes (resumed at %d)", off)
	}
	if off != 0 {
		t.Errorf("the retry resumed at %d onto a prefix that no longer matches the source", off)
	}
}

// The receiver's remembered partial is used only while the partial is the
// file it wrote: changed behind its back, it is read again and the bad
// prefix is caught (the file restarts) rather than resumed onto.
func TestRetry_TamperedPartialIsHashedAgain(t *testing.T) {
	for _, resetMtime := range []bool{false, true} {
		t.Run(fmt.Sprintf("mtime reset=%v", resetMtime), func(t *testing.T) {
			freshHashCache(t)
			const size = 12 << 20
			src, dst := filepath.Join(t.TempDir(), "big.bin"), t.TempDir()
			data := randBytes(size)
			writeFile(t, src, data)
			if _, se, re := attempt(t, src, dst, 8<<20, false); se == nil || re == nil {
				t.Fatalf("the cut attempt finished: send=%v recv=%v", se, re)
			}
			partials, _ := filepath.Glob(filepath.Join(PartialDir(dst), "*.partial"))
			if len(partials) != 1 {
				t.Fatalf("partials = %v", partials)
			}
			st, err := os.Stat(partials[0])
			if err != nil {
				t.Fatal(err)
			}
			if resetMtime && changeTime(st) == 0 {
				t.Skip("no change time on this OS: a forged modification time is not detectable")
			}
			f, err := os.OpenFile(partials[0], os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte{data[5] ^ 0xFF}, 5); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			if resetMtime {
				if err := os.Chtimes(partials[0], st.ModTime(), st.ModTime()); err != nil {
					t.Fatal(err)
				}
				if now, err := os.Stat(partials[0]); err != nil || changeTime(now) == changeTime(st) {
					t.Skip("this filesystem's change time is too coarse to show the edit")
				}
			} else {
				later := st.ModTime().Add(2 * time.Second)
				if err := os.Chtimes(partials[0], later, later); err != nil {
					t.Fatal(err)
				}
			}
			off, se, re := attempt(t, src, dst, 0, false)
			if se != nil || re != nil {
				t.Fatalf("retry: send=%v recv=%v", se, re)
			}
			if off != 0 {
				t.Errorf("the retry resumed at %d onto a tampered partial", off)
			}
			if !bytes.Equal(mustRead(t, filepath.Join(dst, "big.bin")), data) {
				t.Fatal("the tampered byte reached the delivered file")
			}
		})
	}
}

// Under --checksum the sender is asked for the roots of the files already
// delivered when a transfer is retried; it answers from what it hashed
// while sending them.
func TestRetry_ChecksumRootsComeFromTheFirstSend(t *testing.T) {
	freshHashCache(t)
	src, dst := t.TempDir(), t.TempDir()
	root := filepath.Join(src, "tree")
	for i := range 6 {
		writeFile(t, filepath.Join(root, fmt.Sprintf("f%d.bin", i)), randBytes(3<<20))
	}
	// A first --checksum run into a folder that already holds one of the
	// files, so the sender sees a verify round and remembers what it sends.
	writeFile(t, filepath.Join(dst, "tree", "f0.bin"), mustRead(t, filepath.Join(root, "f0.bin")))
	if _, se, re := attempt(t, root, dst, 0, true); se != nil || re != nil {
		t.Fatalf("first run: send=%v recv=%v", se, re)
	}
	sources, err := Walk([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := hashedBytes.Load()
	for _, s := range sources {
		if s.Entry.Type != wire.EntryFile {
			continue
		}
		if _, err := hashCache.fileRoot(context.Background(), s.AbsPath, nil); err != nil {
			t.Fatal(err)
		}
	}
	if read := hashedBytes.Load() - before; read != 0 {
		t.Errorf("the sender read %d bytes to hash files it had just sent", read)
	}
}

// --checksum gives a file it found identical the sender's modification
// time. That is the receiver's own change, not a new version: a retry
// compares the file again without reading it again, while a file somebody
// else changed afterwards is read.
func TestRetry_ChecksumKeepsHashesOfFilesItRetimed(t *testing.T) {
	freshHashCache(t)
	src, dst := t.TempDir(), t.TempDir()
	root := filepath.Join(src, "tree")
	const files, size = 4, 3 << 20
	old := time.Now().Add(-48 * time.Hour)
	for i := range files {
		name := fmt.Sprintf("f%d.bin", i)
		data := randBytes(size)
		writeFile(t, filepath.Join(root, name), data)
		// The same bytes are already there, under another time.
		writeFile(t, filepath.Join(dst, "tree", name), data)
		if err := os.Chtimes(filepath.Join(dst, "tree", name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	before := hashedBytes.Load()
	if _, se, re := attempt(t, root, dst, 0, true); se != nil || re != nil {
		t.Fatalf("first run: send=%v recv=%v", se, re)
	}
	if read := hashedBytes.Load() - before; read != 2*files*size {
		t.Fatalf("the first run read %d bytes, want every file once per side (%d)", read, 2*files*size)
	}
	f0 := filepath.Join(dst, "tree", "f0.bin")
	want, err := os.Stat(filepath.Join(root, "f0.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(f0); err != nil || st.ModTime().Unix() != want.ModTime().Unix() {
		t.Fatalf("f0.bin did not take the sender's time: %v", err)
	}

	before = hashedBytes.Load()
	if _, se, re := attempt(t, root, dst, 0, true); se != nil || re != nil {
		t.Fatalf("second run: send=%v recv=%v", se, re)
	}
	if read := hashedBytes.Load() - before; read != 0 {
		t.Errorf("the second run read %d bytes to compare files it had just compared", read)
	}

	// Another program rewrites one of them and puts the time back: on a
	// system with change times that file, and only that file, is read.
	st, err := os.Stat(f0)
	if err != nil {
		t.Fatal(err)
	}
	if changeTime(st) == 0 {
		return // not detectable here: the state is identity, size and time
	}
	other := randBytes(size)
	if err := os.WriteFile(f0, other, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f0, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if now, err := os.Stat(f0); err != nil || changeTime(now) == changeTime(st) {
		t.Skip("this filesystem's change time is too coarse to show the edit")
	}
	before = hashedBytes.Load()
	if _, se, re := attempt(t, root, dst, 0, true); se != nil || re != nil {
		t.Fatalf("third run: send=%v recv=%v", se, re)
	}
	if read := hashedBytes.Load() - before; read != size {
		t.Errorf("after one file was rewritten the run read %d bytes, want that file (%d)", read, size)
	}
	if !bytes.Equal(mustRead(t, f0), other) {
		t.Error("the rewritten file was replaced without consent")
	}
}

// What the receiver remembers of its own partial is what it wrote last, and
// only whole blocks. A partial rewritten to the same size inside one tick
// of a coarse clock (FAT keeps two seconds) shows the timestamps it had:
// the hashes of the bytes it holds now must still win.
func TestBlockCache_RememberedPartialIsTheLastWritten(t *testing.T) {
	freshHashCache(t)
	p := filepath.Join(t.TempDir(), "x.partial")
	writeFile(t, p, make([]byte, 2*hashBlockSize+5))
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	first := [][32]byte{{1}, {2}, {3}}
	hashCache.rememberBlocks(p, first)
	if got := hashCache.get(p, identify(fi), 9); len(got) != 2 || got[0] != first[0] || got[1] != first[1] {
		t.Fatalf("remembered %v, want the two whole blocks", got)
	}
	// The file is, as far as its timestamps show, in the state it was.
	second := [][32]byte{{7}, {8}}
	hashCache.rememberBlocks(p, second)
	if got := hashCache.get(p, identify(fi), 9); len(got) != 2 || got[0] != second[0] || got[1] != second[1] {
		t.Errorf("after a rewrite the cache holds %v, want the hashes of what was written last", got)
	}
	// Nothing is kept for a path that is not a regular file any more.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	hashCache.rememberBlocks(p, first)
	if got := hashCache.get(p, identify(fi), 9); got != nil {
		t.Errorf("a removed partial is remembered: %v", got)
	}
}

// What is remembered, and when it stops being trusted.
func TestBlockCache(t *testing.T) {
	freshHashCache(t)
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "f.bin")
	data := randBytes(5<<20 + 123)
	writeFile(t, p, data)
	want := func(n int) [][32]byte {
		h := newFileHasher()
		_, _ = h.Write(data[:n])
		return h.blocks()
	}
	same := func(a, b [][32]byte) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	read := func(f func()) int64 {
		before := hashedBytes.Load()
		f()
		return hashedBytes.Load() - before
	}

	// A prefix, then a longer one: only the new blocks are read.
	var got [][32]byte
	if n := read(func() { got, _, _ = hashCache.hashPrefix(ctx, p, 2<<20, nil) }); n != 2<<20 || !same(got, want(2<<20)) {
		t.Fatalf("first prefix: read %d, hashes ok=%v", n, same(got, want(2<<20)))
	}
	var progress int64
	if n := read(func() { got, _, _ = hashCache.hashPrefix(ctx, p, 4<<20, func(d int64) { progress = d }) }); n != 2<<20 || !same(got, want(4<<20)) {
		t.Fatalf("longer prefix: read %d, hashes ok=%v", n, same(got, want(4<<20)))
	}
	if progress != 4<<20 {
		t.Errorf("progress ended at %d, want %d (cached bytes count)", progress, 4<<20)
	}
	// The whole file: the short last block is hashed and remembered; a
	// shorter prefix and the root then cost nothing.
	rootWant := bytesRoot(data)
	var rootGot [32]byte
	if n := read(func() { rootGot, _ = hashCache.fileRoot(ctx, p, nil) }); n != int64(len(data))-4<<20 || rootGot != rootWant {
		t.Fatalf("whole file: read %d, root ok=%v", n, rootGot == rootWant)
	}
	if n := read(func() {
		rootGot, _ = hashCache.fileRoot(ctx, p, nil)
		got, _, _ = hashCache.hashPrefix(ctx, p, 3<<20, nil)
	}); n != 0 || rootGot != rootWant || !same(got, want(3<<20)) {
		t.Fatalf("cached: read %d", n)
	}

	// A change of content with a new modification time: everything again.
	data[0] ^= 1
	writeFile(t, p, data)
	later := time.Now().Add(5 * time.Second)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	if n := read(func() { rootGot, _ = hashCache.fileRoot(ctx, p, nil) }); n != int64(len(data)) || rootGot != bytesRoot(data) {
		t.Fatalf("after a change: read %d, root ok=%v", n, rootGot == bytesRoot(data))
	}

	// A file touched moments before it is hashed is not remembered at all:
	// its timestamps could not show a second change in the same tick.
	racyWindow = 3 * time.Second
	q := filepath.Join(t.TempDir(), "fresh.bin")
	writeFile(t, q, data)
	for range 2 {
		if n := read(func() { _, _ = hashCache.fileRoot(ctx, q, nil) }); n != int64(len(data)) {
			t.Fatalf("a just-written file was served from the cache (read %d)", n)
		}
	}
	// Left alone for longer than the window, it is.
	racyWindow = 100 * time.Millisecond
	time.Sleep(150 * time.Millisecond)
	_, _ = hashCache.fileRoot(ctx, q, nil)
	if n := read(func() { _, _ = hashCache.fileRoot(ctx, q, nil) }); n != 0 {
		t.Errorf("a settled file was read again (%d bytes)", n)
	}
}

// The cache's memory is bounded: past the budget it starts over.
func TestBlockCache_Bounded(t *testing.T) {
	freshHashCache(t)
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blocks := make([][32]byte, 1024) // ~32 KiB an entry
	for i := range 3 * hashCacheBudget / (32 * len(blocks)) {
		hashCache.put(fmt.Sprintf("/x/%d", i), identify(fi), blocks)
		if hashCache.cost > hashCacheBudget {
			t.Fatalf("the cache holds %d bytes, over its %d budget", hashCache.cost, hashCacheBudget)
		}
	}
	if len(hashCache.files) == 0 {
		t.Error("the cache is empty after the newest entries went in")
	}
}

// Hashing stops within a block of its context ending, instead of reading
// the rest of a large file for a transfer that is over; what it finished is
// kept for the next attempt.
func TestHashPrefix_StopsWhenTheContextEnds(t *testing.T) {
	freshHashCache(t)
	const size = 1 << 30
	p := filepath.Join(t.TempDir(), "sparse.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	before := hashedBytes.Load()
	start := time.Now()
	_, _, err = hashCache.hashPrefix(ctx, p, size, func(done int64) {
		if done >= 8<<20 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("hashPrefix = %v, want context.Canceled", err)
	}
	read := hashedBytes.Load() - before
	if read > 16<<20 {
		t.Errorf("hashing read %d MiB after being cancelled at 8 MiB (in %v)", read>>20, time.Since(start))
	}
	// The next attempt picks up where this one stopped.
	before = hashedBytes.Load()
	if _, _, err := hashCache.hashPrefix(context.Background(), p, 16<<20, nil); err != nil {
		t.Fatal(err)
	}
	if again := hashedBytes.Load() - before; again > 16<<20-read {
		t.Errorf("the next attempt read %d bytes; %d were already hashed", again, read)
	}
}

// A sender waiting for a resume prefix to be hashed stops as soon as its
// connection is gone, and reports the connection's error — not minutes
// later, when the hash is done and the first write fails.
func TestSend_PrefixHashingEndsWithTheConnection(t *testing.T) {
	freshHashCache(t)
	const size = 2 << 30
	src := filepath.Join(t.TempDir(), "sparse.bin")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	sources, err := Walk([]string{src}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctrlA, ctrlB := net.Pipe()
	dataA, dataB := net.Pipe()
	defer func() { _ = ctrlB.Close(); _ = dataB.Close() }()
	conn, drop := context.WithCancelCause(context.Background())
	dropped := errors.New("the peer went away")
	sendErr := make(chan error, 1)
	before := hashedBytes.Load()
	go func() {
		s := &Streams{Control: ctrlA, Data: dataA}
		sendErr <- Send(context.Background(), s, SendOptions{Mode: wire.ModeFiles, Sources: sources, Conn: conn})
		s.Close()
	}()

	// A receiver that asks to resume almost the whole file, then vanishes.
	var hello wire.SenderHello
	if _, err := wire.ReadControl(ctrlB, &hello); err != nil {
		t.Fatal(err)
	}
	peer := versionedConn{ReadWriteCloser: ctrlB, v: wire.ProtocolVersion}
	if err := wire.WriteControl(ctrlB, wire.TypeHelloAck, &wire.ReceiverHello{
		MinVersion: wire.MinProtocolVersion, MaxVersion: wire.ProtocolVersion, Accepts: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := recvListing(peer); err != nil {
		t.Fatal(err)
	}
	if err := sendDecisions(peer, []wire.Decision{{Index: 0, Action: wire.DecisionResume,
		ResumeOffset: size - hashBlockSize, PrefixRoot: make([]byte, 32)}}); err != nil {
		t.Fatal(err)
	}
	for hashedBytes.Load() == before { // the prefix hasher is at work
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	drop(dropped)
	_ = ctrlB.Close()
	_ = dataB.Close()

	select {
	case err := <-sendErr:
		if !errors.Is(err, dropped) {
			t.Errorf("Send = %v, want the connection's error", err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, fserrors.ErrReadFailed) {
			t.Errorf("Send = %v: a dropped connection reported as a cancel or an unreadable source", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send was still hashing 10 s after its connection went away")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Send took %v to notice the drop", took)
	}
	if read := hashedBytes.Load() - before; read > size/2 {
		t.Errorf("the hasher read %d MiB of a %d MiB prefix for a connection that was gone", read>>20, size>>20)
	}
}

// BenchmarkRetryAfterDrop times the retry of a 512 MiB file whose first
// attempt was cut at 448 MiB: with the cache the retry only moves the last
// 64 MiB; without it both sides first read 448 MiB back to hash it.
func BenchmarkRetryAfterDrop(b *testing.B) {
	const size, cut = 512 << 20, 448 << 20
	src := filepath.Join(b.TempDir(), "big.bin")
	if err := os.WriteFile(src, randBytes(size), 0o644); err != nil {
		b.Fatal(err)
	}
	sources, err := Walk([]string{src}, nil)
	if err != nil {
		b.Fatal(err)
	}
	run := func(dst string, cutAfter int64) (error, error) {
		ca, cb := tcpPair(b)
		da, db := tcpPair(b)
		var data io.ReadWriteCloser = da
		if cutAfter > 0 {
			c := &cutConn{ReadWriteCloser: da, drops: []io.Closer{da, ca}}
			c.left.Store(cutAfter)
			data = c
		}
		var wg sync.WaitGroup
		var se, re error
		wg.Add(2)
		go func() {
			defer wg.Done()
			s := &Streams{Control: ca, Data: data}
			se = Send(context.Background(), s, SendOptions{Mode: wire.ModeFiles, Sources: sources})
			s.Close()
		}()
		go func() {
			defer wg.Done()
			s := &Streams{Control: cb, Data: db}
			re = Recv(context.Background(), s, RecvOptions{TargetDir: dst})
			s.Close()
		}()
		wg.Wait()
		return se, re
	}
	for _, cached := range []bool{true, false} {
		b.Run(fmt.Sprintf("cache=%v", cached), func(b *testing.B) {
			old := racyWindow
			racyWindow = -time.Hour
			defer func() { racyWindow = old; hashCache.reset() }()
			var reread int64
			for b.Loop() {
				b.StopTimer()
				hashCache.reset()
				hashCache.off = !cached
				dst := b.TempDir()
				if se, re := run(dst, cut); se == nil || re == nil {
					b.Fatalf("the cut attempt finished: send=%v recv=%v", se, re)
				}
				before := hashedBytes.Load()
				b.StartTimer()
				if se, re := run(dst, 0); se != nil || re != nil {
					b.Fatalf("retry: send=%v recv=%v", se, re)
				}
				reread += hashedBytes.Load() - before
			}
			b.ReportMetric(float64(reread)/float64(b.N)/(1<<20), "MiB-reread/op")
		})
	}
}
