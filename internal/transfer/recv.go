package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// ConflictPolicy says what happens to incoming entries that differ from what
// is already on disk.
type ConflictPolicy uint8

const (
	ConflictKeep    ConflictPolicy = iota // keep the local copy; skip the incoming one
	ConflictReplace                       // replace the local copy (never a non-empty folder)
	ConflictRename                        // keep both: save the incoming one as "name (1).ext"
)

// RecvOptions configures one receive invocation.
type RecvOptions struct {
	Hostname      string
	OS            string
	ClientVersion string
	TargetDir     string // where files are written
	// Conflict applies to differing entries when ResolveConflicts is nil.
	Conflict ConflictPolicy
	// Overwrite is shorthand for Conflict: ConflictReplace.
	Overwrite bool
	Checksum  bool // verify same-size files by content hash, not mtime

	// Accept is shown the HELLO and (ModeFiles) the classification breakdown,
	// including any conflicts. nil → auto-accept. false declines everything.
	Accept func(hello wire.SenderHello, summary ClassifySummary) bool
	// ResolveConflicts decides, once, what to do with all differing entries.
	// It runs right after Accept, so a UI can ask both in one prompt.
	ResolveConflicts func(conflicts []Conflict) ConflictPolicy

	Password string
	// PromptPass asks the user for the sender's password. attempt starts at
	// 1 and grows when the sender rejects a try (up to PasswordAttempts),
	// so the prompt can say "wrong password — try again".
	PromptPass func(attempt int) (string, error)

	ProgressFn func(index uint32, bytesWritten uint64)
	OnResume   func(index uint32, offset, total uint64)
	OnSkip     func(index uint32)
	// OnFileDone fires once per file once it is durable on disk (including
	// empty ones) with its absolute path and, when OnManifest is set, its
	// SHA-256 hex digest. Not called for sink streams.
	OnFileDone func(path, sha256Hex string)
	// OnConflictKept fires once per differing/conflicting entry left
	// untouched (no consent). The CLI uses it to set a non-zero exit code.
	OnConflictKept func(rel string)
	// OnEntrySkipped fires for an entry that was not saved for a reason
	// other than the user's choice: the sender could not read it, or it
	// can't be placed safely here (name collision, folder in the way).
	OnEntrySkipped func(rel, reason string)
	// OnSourceChanged fires when the sender reports that a file changed
	// while it was being read, so the copy may be inconsistent.
	OnSourceChanged func(rel string)
	// OnHashProgress reports local hashing before the accept prompt
	// (--checksum comparisons, resume-prefix checks).
	OnHashProgress func(done, total uint64)
	// OnManifest, when set, receives one row per file after a successful
	// receive — its path, size, and what fsend did with it — backing --manifest.
	OnManifest func(entries []ManifestEntry)

	// Sink, when non-nil, streams the payload here instead of writing under
	// TargetDir: raw bytes for a single file or stream, a tar archive for a
	// folder or several files.
	Sink io.Writer
}

func (o *RecvOptions) policy() ConflictPolicy {
	if o.Overwrite {
		return ConflictReplace
	}
	return o.Conflict
}

// ClassifySummary is the breakdown surfaced to the accept prompt.
type ClassifySummary = classifySummary

// ManifestEntry is one row of the post-transfer --manifest record: a file and
// what fsend did with it.
type ManifestEntry struct {
	RelativePath string // where it was saved, relative to the target dir
	Size         uint64
	Status       string // new | identical | overwritten | kept | resumed | renamed | skipped
	// SHA256 hex digest of the content fsend wrote, empty when no bytes
	// moved this run (identical/kept files) — not recomputed from disk.
	SHA256 string
}

// Recv executes the full receiver-side protocol over the supplied streams.
func Recv(ctx context.Context, s *Streams, opts RecvOptions) error {
	// Bind the streams to ctx so a Ctrl-C parked in a blocked QUIC read
	// unblocks promptly instead of waiting out the idle timeout.
	s, stop := bindCtx(ctx, s)
	defer stop()

	var hello wire.SenderHello
	ft, err := wire.ReadControl(s.Control, &hello)
	if err != nil {
		if errors.Is(err, wire.ErrUnsupportedVersion) {
			// The peer speaks a different wire protocol (a breaking release).
			// Reply with our own version-stamped frame so the sender's read
			// trips the same check and both sides report it clearly.
			ackProceed(s, opts, false)
			drainControl(s)
			return fserrors.ErrIncompatibleVersion
		}
		return fmt.Errorf("recv: hello: %w", err)
	}
	if ft != wire.TypeHello {
		return fmt.Errorf("%w: expected HELLO, got %v", fserrors.ErrProtocolError, ft)
	}
	if _, ok := wire.Negotiate(hello.MinVersion, hello.MaxVersion); !ok {
		ackProceed(s, opts, false)
		drainControl(s)
		return fserrors.ErrIncompatibleVersion
	}

	if hello.Mode == wire.ModeStream {
		return recvStream(ctx, s, &hello, opts)
	}
	return recvFiles(ctx, s, &hello, opts)
}

// recvFiles runs the listing → classify → consent → data flow.
func recvFiles(ctx context.Context, s *Streams, hello *wire.SenderHello, opts RecvOptions) error {
	// Proceed to listing (real accept happens after the breakdown is known).
	ackProceed(s, opts, true)

	if hello.HasPassword {
		if err := receiverPasswordHandshake(s, opts); err != nil {
			return err
		}
	}

	entries, err := recvListing(s.Control)
	if err != nil {
		// Tell the sender why, so it doesn't misread the close as a drop.
		switch {
		case errors.Is(err, fserrors.ErrTooManyEntries):
			declineTransfer(s, wire.ErrCodeTooManyEntries, err.Error())
		case errors.Is(err, fserrors.ErrProtocolError):
			declineTransfer(s, wire.ErrCodeListingInvalid, "invalid listing")
		}
		return err
	}

	if opts.Sink != nil {
		return recvFilesToSink(ctx, s, hello, entries, opts)
	}
	r := newReceiver(s, opts)
	return r.run(ctx, hello, entries)
}

// receiver is one ModeFiles receive into TargetDir.
type receiver struct {
	s       *Streams
	opts    RecvOptions
	cls     *classifier
	ps      *partialStore
	dirs    *dirUnlocker
	commit  *committer
	wantSHA bool
	plans   []entryPlan
}

func newReceiver(s *Streams, opts RecvOptions) *receiver {
	ps := newPartialStore(opts.TargetDir)
	r := &receiver{s: s, opts: opts, ps: ps, wantSHA: opts.OnManifest != nil,
		dirs: newDirUnlocker(opts.TargetDir)}
	r.cls = &classifier{targetDir: opts.TargetDir, checksum: opts.Checksum, sem: probeFS(opts.TargetDir),
		ps: ps, progress: opts.OnHashProgress}
	r.commit = &committer{r: r}
	return r
}

func (r *receiver) run(ctx context.Context, hello *wire.SenderHello, entries []wire.ListingEntry) error {
	s, opts := r.s, r.opts
	r.ps.sweep()
	plans, err := r.cls.run(entries)
	if err != nil {
		declineTransfer(s, wire.ErrCodeListingInvalid, "invalid listing")
		return err
	}
	r.plans = plans

	// --checksum: resolve same-size candidates by content hash before the
	// breakdown is shown or any decision is made.
	if opts.Checksum {
		if err := resolveVerify(s, plans, r.cls.sem, opts.OnHashProgress); err != nil {
			return err
		}
	}

	summary := summarize(plans)
	if err := r.checkSpace(summary.BytesToRecv); err != nil {
		return err
	}
	if opts.Accept != nil && !opts.Accept(*hello, summary) {
		declineTransfer(s, wire.ErrCodeDeclined, "receiver declined")
		return fserrors.ErrReceiverDeclined
	}

	policy := opts.policy()
	if len(summary.Conflicts) > 0 {
		if opts.ResolveConflicts != nil {
			policy = opts.ResolveConflicts(summary.Conflicts)
		}
		if policy != ConflictKeep {
			applyPolicy(plans, policy, r.cls)
			if err := r.checkSpace(summary.BytesToRecv + summary.DifferingBytes); err != nil {
				return err
			}
		}
	}

	decisions := make([]wire.Decision, len(plans))
	for i := range plans {
		decisions[i] = planToDecision(&plans[i], policy == ConflictReplace, opts)
	}
	if err := sendDecisions(s.Control, decisions); err != nil {
		return fmt.Errorf("recv: decisions: %w", err)
	}

	// Verified files are committed even when a later one fails, so a retry
	// only re-sends what is really missing; then folder modes go back.
	defer r.dirs.restore()
	defer func() { _ = r.commit.flush() }()

	// Materialize structural entries (dirs, empty files) we're creating;
	// data files are opened lazily during the data phase.
	expected := 0
	byIndex := make(map[uint32]*entryPlan, len(plans))
	for i := range plans {
		p := &plans[i]
		byIndex[p.entry.Index] = p
		if decisions[i].Action == wire.DecisionSkip {
			// An identical file skips the data transfer, but a permission-only
			// change (chmod +x) leaves size+mtime untouched — repair the mode
			// locally or it never propagates. Kept conflicts stay as-is.
			if p.disp == dispIdentical {
				repairIdenticalMode(p)
			}
			continue
		}
		if p.entry.Type == wire.EntryFile && p.entry.Size > 0 {
			expected++
			continue
		}
		if err := r.materialize(p); err != nil {
			return err
		}
	}

	if err := r.receiveData(ctx, byIndex, decisions, expected); err != nil {
		return err
	}
	if err := r.commit.flush(); err != nil {
		return err
	}
	r.dirs.restore()
	restoreDirModes(plans)
	if err := finishRecv(s); err != nil {
		return err
	}
	if opts.OnManifest != nil {
		opts.OnManifest(manifestEntries(plans, decisions))
	}
	r.ps.tidy()
	return nil
}

// checkSpace declines up front when the target filesystem can't hold need
// bytes, instead of filling the disk and failing halfway.
func (r *receiver) checkSpace(need uint64) error {
	free, ok := freeSpace(r.opts.TargetDir)
	if !ok || need <= free {
		return nil
	}
	msg := fmt.Sprintf("needs %s, %s free", humanSize(need), humanSize(free))
	declineTransfer(r.s, wire.ErrCodeDiskFull, msg)
	return fmt.Errorf("%w: %s", fserrors.ErrDiskFull, msg)
}

// finishRecv reads the sender's TRANSFER_COMPLETE, acks it, and waits for the
// sender's FIN so both sides finish before any transport close.
func finishRecv(s *Streams) error {
	ft, body, err := wire.ReadControlRaw(s.Control)
	if err != nil {
		return fmt.Errorf("recv: read complete: %w", err)
	}
	if ft == wire.TypeError {
		return peerError(body)
	}
	if ft != wire.TypeTransferComplete {
		return fmt.Errorf("%w: expected TRANSFER_COMPLETE, got %v", fserrors.ErrProtocolError, ft)
	}
	if err := wire.WriteControl(s.Control, wire.TypeTransferAck, nil); err != nil {
		return err
	}
	drainControl(s)
	return nil
}

// maxVerifyIDsPerFrame bounds one VERIFY_REQUEST batch. Both frames of the
// round trip must fit MaxControlFrameSize (64 KiB): the gob []uint32 request
// grows ~2.5 B/idx, but the sender's []FileHash response (senderNegotiate
// answers one response per request) is the long pole — a 32-byte root plus
// per-field overhead, and gob encodes each hash byte as a varint so an
// all-0xFF (worst-case) root costs the most. Measured gob bodies, worst case:
//
//	idx    request    response
//	 100    ~0.4 KB      7.0 KB
//	 500    ~1.4 KB     35.4 KB
//	 800    ~2.1 KB     56.5 KB   ← this bound
//	 900    ~2.4 KB     63.6 KB   (3% headroom)
//	1000    ~2.6 KB     70.7 KB   (overflow)
//
// so the response, not the request, sets the ceiling. 800 keeps the response
// near 57 KB; a larger tree just becomes more round trips, each bounded (the
// sender caps the count at maxVerifyRounds).
const maxVerifyIDsPerFrame = 800

// verifyWorkers bounds how many local files --checksum hashes at once (each
// file's blocks are hashed in parallel as well).
const verifyWorkers = 4

// resolveVerify (--checksum) asks the sender for the root of every same-size
// candidate while hashing the local copies concurrently, then rewrites each
// dispVerify to dispIdentical (match) or dispDiffers. A match also takes the
// sender's mtime, so the next plain run sees the file as identical. Hashing
// failures fall to dispDiffers so an unreadable/changed file is re-sent
// rather than wrongly skipped.
func resolveVerify(s *Streams, plans []entryPlan, sem fsSemantics, progress func(done, total uint64)) error {
	var idx []uint32
	var cand []*entryPlan
	var total uint64
	for i := range plans {
		if plans[i].disp == dispVerify {
			idx = append(idx, plans[i].entry.Index)
			cand = append(cand, &plans[i])
			total += plans[i].entry.Size
		}
	}
	if len(idx) == 0 {
		return nil
	}

	// Local hashing runs alongside the request/response rounds.
	local := make(map[uint32][32]byte, len(cand))
	var mu sync.Mutex
	var wg sync.WaitGroup
	meter := newProgressMeter(total, progress)
	work := make(chan *entryPlan)
	for range min(verifyWorkers, len(cand)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				var last int64
				root, err := hashFileRoot(p.target, func(done int64) { meter.add(done - last); last = done })
				if err != nil {
					continue
				}
				mu.Lock()
				local[p.entry.Index] = root
				mu.Unlock()
			}
		}()
	}
	go func() {
		for _, p := range cand {
			work <- p
		}
		close(work)
	}()

	remote := make(map[uint32][32]byte, len(idx))
	var reqErr error
	for start := 0; start < len(idx) && reqErr == nil; start += maxVerifyIDsPerFrame {
		reqErr = verifyRound(s, idx[start:min(start+maxVerifyIDsPerFrame, len(idx))], remote)
	}
	wg.Wait()
	if reqErr != nil {
		return reqErr
	}
	for _, p := range cand {
		lr, lok := local[p.entry.Index]
		rr, rok := remote[p.entry.Index]
		if !lok || !rok || lr != rr {
			p.disp = dispDiffers
			continue
		}
		p.disp = dispIdentical
		if st, err := os.Lstat(p.target); err == nil && !sem.sameMtime(st.ModTime(), p.entry.ModTimeSec) {
			t := time.Unix(p.entry.ModTimeSec, 0)
			_ = os.Chtimes(p.target, t, t)
		}
	}
	return nil
}

func verifyRound(s *Streams, batch []uint32, into map[uint32][32]byte) error {
	if err := wire.WriteControl(s.Control, wire.TypeVerifyRequest, batch); err != nil {
		return fmt.Errorf("recv: verify request: %w", err)
	}
	ft, body, err := wire.ReadControlRaw(s.Control)
	if err != nil {
		return fmt.Errorf("recv: verify response: %w", err)
	}
	if ft == wire.TypeError {
		return peerError(body)
	}
	if ft != wire.TypeVerifyResponse {
		return fmt.Errorf("%w: expected VERIFY_RESPONSE, got %v", fserrors.ErrProtocolError, ft)
	}
	var resp []wire.FileHash
	if err := wire.Decode(body, &resp); err != nil {
		return fmt.Errorf("recv: verify decode: %w", err)
	}
	for _, fh := range resp {
		into[fh.Index] = fh.Hash
	}
	return nil
}

// inChunk is one decoded data frame handed from the network reader to the
// disk writer. buf, when set, goes back to the block pool after use.
type inChunk struct {
	segs  []wire.Segment
	plain []byte
	bufs  [2][]byte
	err   error
}

func (c *inChunk) release() {
	for _, b := range c.bufs {
		if b != nil {
			putBlock(b)
		}
	}
}

// readChunks reads and decompresses frames on its own goroutine, so network
// reads and decompression overlap the disk writes. It stops on the first
// error (delivered in order, after any chunks before it) or when stop closes.
func readChunks(data io.Reader, out chan<- inChunk, stop <-chan struct{}) {
	var dec *zstd.Decoder
	defer func() {
		if dec != nil {
			dec.Close()
		}
	}()
	for {
		raw := getBlock()
		c, err := wire.ReadChunkBuf(data, raw)
		var ic inChunk
		if err != nil {
			putBlock(raw)
			ic.err = err
		} else {
			ic.bufs[0] = raw
			var plainBuf []byte
			if c.Compressed {
				plainBuf = getBlock()
				ic.bufs[1] = plainBuf
			}
			ic.plain, ic.err = decodeChunkPayload(c, &dec, plainBuf)
			ic.segs = c.Segments
		}
		select {
		case out <- ic:
		case <-stop:
			ic.release()
			return
		}
		if ic.err != nil {
			return
		}
	}
}

// receiveData consumes chunks until every expected file reaches EOF or is
// skipped by the sender.
func (r *receiver) receiveData(ctx context.Context, byIndex map[uint32]*entryPlan, decisions []wire.Decision, expected int) error {
	s, opts := r.s, r.opts
	// Consent enforcement: data may land only for files the receiver actually
	// asked for (Send/Resume, regular, non-empty). Without this gate a peer
	// could stream bytes for a file the user chose to skip or keep and clobber
	// the protected local copy.
	allowed := make(map[uint32]bool, expected)
	for i := range decisions {
		d := decisions[i]
		if d.Action != wire.DecisionSend && d.Action != wire.DecisionResume {
			continue
		}
		if p, ok := byIndex[d.Index]; ok && p.entry.Type == wire.EntryFile && p.entry.Size > 0 {
			allowed[d.Index] = true
		}
	}
	done := make(map[uint32]bool, expected)
	if expected == 0 {
		return nil
	}

	in := make(chan inChunk, 4)
	stop := make(chan struct{})
	go readChunks(s.Data, in, stop)
	var cur *recvFile // an honest sender streams one file at a time
	defer func() {
		close(stop)
		if cur != nil {
			cur.abandon(true)
		}
	}()

	for expected > 0 {
		var c inChunk
		select {
		case c = <-in:
		case <-ctx.Done():
			return ctx.Err()
		}
		if c.err != nil {
			err := c.err
			if errors.Is(err, io.EOF) {
				if reason := tryReadPeerError(s.Control); reason != nil {
					// A content-correctness failure means the bytes on disk can't
					// be trusted; discard the open partial so a re-run starts clean.
					if errors.Is(reason, fserrors.ErrHashMismatch) && cur != nil {
						cur.abandon(false)
						cur = nil
					}
					return reason
				}
				return fmt.Errorf("recv: stream closed mid-file: %w", fserrors.ErrConnectFailed)
			}
			if errors.Is(err, fserrors.ErrProtocolError) || errors.Is(err, wire.ErrMalformedChunk) || errors.Is(err, wire.ErrChunkTooLarge) {
				declineTransfer(s, wire.ErrCodeProtocolError, "malformed chunk")
			}
			return fmt.Errorf("recv: chunk: %w", err)
		}
		err := func() error {
			defer c.release()
			off := 0
			for _, seg := range c.segs {
				data := c.plain[off : off+int(seg.Length)]
				off += int(seg.Length)
				idx := seg.FileIndex
				if !allowed[idx] || done[idx] || (cur != nil && cur.plan.entry.Index != idx) {
					declineTransfer(s, wire.ErrCodeListingInvalid, "chunk for a file not awaiting data")
					return fmt.Errorf("%w: chunk for file index %d not awaiting data", fserrors.ErrProtocolError, idx)
				}
				p := byIndex[idx]
				if seg.Skip {
					if cur != nil {
						cur.abandon(false)
						cur = nil
					}
					done[idx] = true
					expected--
					p.skipReason = "the sender could not read it"
					if opts.OnEntrySkipped != nil {
						opts.OnEntrySkipped(filepath.ToSlash(p.rel), p.skipReason)
					}
					continue
				}
				if cur == nil {
					rf, err := r.openFile(p, seg.Restart)
					if err != nil {
						return err
					}
					cur = rf
				} else if seg.Restart {
					declineTransfer(s, wire.ErrCodeProtocolError, "restart mid-file")
					return fmt.Errorf("%w: restart flag mid-file", fserrors.ErrProtocolError)
				}
				if err := cur.write(data); err != nil {
					return err
				}
				if seg.EOF {
					rf := cur
					cur = nil
					if err := rf.finalize(seg.RootHash); err != nil {
						return err
					}
					done[idx] = true
					expected--
					if seg.Changed && opts.OnSourceChanged != nil {
						opts.OnSourceChanged(filepath.ToSlash(p.rel))
					}
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// sparseBlock is the granularity of hole detection: an aligned all-zero run
// this long is skipped (left as a hole) instead of written.
const sparseBlock = 64 << 10

var zeroBlock [sparseBlock]byte

// periodicSync bounds how much unsynced data a partial holds, so an
// interrupted large file resumes from close to where it stopped.
const periodicSync = 64 << 20

// recvFile is one in-flight file's write state.
type recvFile struct {
	r        *receiver
	plan     *entryPlan
	f        *os.File
	partial  string
	hasher   *fileHasher
	sha      hash.Hash // only with --manifest
	written  uint64
	unsynced uint64
	tailHole bool
	closed   bool
}

// guardRegular fails closed when a non-regular file (a planted symlink or
// directory) sits where fsend is about to write or rename into. Needs local
// write access to the output dir, so it is defense in depth, not a
// remote-exploitable hole.
func guardRegular(s *Streams, path, what string) error {
	if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
		declineTransfer(s, wire.ErrCodeProtocolError, what+" not a regular file")
		return fmt.Errorf("%w: %s %s not a regular file", fserrors.ErrWriteFailed, what, path)
	}
	return nil
}

func (r *receiver) openFile(p *entryPlan, restart bool) (*recvFile, error) {
	s, opts := r.s, r.opts
	if restart && p.disp != dispResume {
		declineTransfer(s, wire.ErrCodeProtocolError, "restart for a file not resumed")
		return nil, fmt.Errorf("%w: restart for a file not resumed", fserrors.ErrProtocolError)
	}
	rf := &recvFile{r: r, plan: p, hasher: newFileHasher()}
	if r.wantSHA {
		rf.sha = sha256.New()
	}
	if p.disp == dispResume && !restart {
		if err := guardRegular(s, p.partial, "partial"); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(p.partial, os.O_RDWR, 0)
		if err != nil {
			return nil, r.writeFail("open partial", p.rel, err)
		}
		rf.f, rf.partial = f, p.partial
		off := int64(p.resumeOffset)
		if err := f.Truncate(off); err != nil {
			rf.abandon(false)
			return nil, r.writeFail("truncate", p.rel, err)
		}
		rf.hasher.seed(p.prefixBlocks)
		if rf.sha != nil {
			// The manifest digest covers the whole file, prefix included.
			if _, err := io.Copy(rf.sha, io.NewSectionReader(f, 0, off)); err != nil {
				rf.abandon(false)
				return nil, r.writeFail("read partial", p.rel, err)
			}
		}
		rf.written = p.resumeOffset
		if opts.OnResume != nil {
			opts.OnResume(p.entry.Index, p.resumeOffset, p.entry.Size)
		}
		return rf, nil
	}
	if restart {
		// The sender's source no longer matches the partial: start over.
		p.disp = dispNew
		p.prefixBlocks = nil
	}
	f, path, err := r.ps.create(p.rel, p.entry)
	if err != nil {
		return nil, r.writeFail("create partial", p.rel, err)
	}
	rf.f, rf.partial = f, path
	return rf, nil
}

// abandon closes the file; drop also deletes the partial.
func (rf *recvFile) abandon(keep bool) {
	if rf.closed {
		return
	}
	rf.closed = true
	rf.hasher.discard()
	_ = rf.f.Close()
	if !keep {
		rf.r.ps.remove(rf.plan.rel)
	}
}

func (rf *recvFile) write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	r := rf.r
	if rf.written+uint64(len(data)) > rf.plan.entry.Size {
		declineTransfer(r.s, wire.ErrCodeProtocolError, "received bytes exceed declared size")
		return fmt.Errorf("%w: received bytes exceed size %d", fserrors.ErrProtocolError, rf.plan.entry.Size)
	}
	if err := rf.writeSparse(data); err != nil {
		code := wire.ErrCodeWriteFailed
		if errors.Is(err, syscall.ENOSPC) {
			// Free the space we took: a full disk breaks other apps too.
			code = wire.ErrCodeDiskFull
			rf.abandon(false)
		}
		_ = r.s.Data.Close()
		declineTransfer(r.s, code, peerSafe("write", rf.plan.rel, err))
		return classifyWriteErr("write", err)
	}
	_, _ = rf.hasher.Write(data)
	if rf.sha != nil {
		_, _ = rf.sha.Write(data)
	}
	rf.written += uint64(len(data))
	if rf.unsynced += uint64(len(data)); rf.unsynced >= periodicSync {
		_ = fsyncData(rf.f)
		rf.unsynced = 0
	}
	if r.opts.ProgressFn != nil {
		r.opts.ProgressFn(rf.plan.entry.Index, rf.written)
	}
	return nil
}

// writeSparse writes data at the current offset, leaving aligned all-zero
// blocks as holes so sparse images stay sparse.
func (rf *recvFile) writeSparse(data []byte) error {
	pos := int64(rf.written)
	start := 0 // first byte of the pending non-zero run
	flush := func(end int) error {
		if end > start {
			if _, err := rf.f.WriteAt(data[start:end], pos+int64(start)); err != nil {
				return err
			}
		}
		return nil
	}
	i := 0
	if lead := int((sparseBlock - pos%sparseBlock) % sparseBlock); lead > 0 {
		i = min(lead, len(data))
	}
	for i+sparseBlock <= len(data) {
		if bytes.Equal(data[i:i+sparseBlock], zeroBlock[:]) {
			if err := flush(i); err != nil {
				return err
			}
			start = i + sparseBlock
		}
		i += sparseBlock
	}
	if err := flush(len(data)); err != nil {
		return err
	}
	rf.tailHole = start == len(data)
	return nil
}

func (rf *recvFile) finalize(root [32]byte) error {
	r, p := rf.r, rf.plan
	if rf.written != p.entry.Size {
		rf.abandon(false)
		declineTransfer(r.s, wire.ErrCodeProtocolError, "EOF before declared size reached")
		return fmt.Errorf("%w: got %d of %d declared bytes", fserrors.ErrProtocolError, rf.written, p.entry.Size)
	}
	if rf.hasher.Sum() != root {
		rf.abandon(false)
		declineTransfer(r.s, wire.ErrCodeFileHashMismatch, "root hash mismatch")
		return fserrors.ErrHashMismatch
	}
	if rf.tailHole {
		if err := rf.f.Truncate(int64(p.entry.Size)); err != nil {
			rf.abandon(true)
			return r.writeFail("extend", p.rel, err)
		}
	}
	if rf.sha != nil {
		p.sha256 = hex.EncodeToString(rf.sha.Sum(nil))
	}
	// Mode and mtime go on before the sync, so a crash can't leave a
	// committed file with today's date (which a later run would distrust).
	_ = rf.f.Chmod(fileMode(p.entry.Mode))
	t := time.Unix(p.entry.ModTimeSec, 0)
	_ = os.Chtimes(rf.partial, t, t)
	return r.commit.add(pendingFile{f: rf.f, partial: rf.partial, plan: p, size: p.entry.Size})
}

// pendingFile is a verified partial waiting for its batch commit.
type pendingFile struct {
	f       *os.File
	partial string
	plan    *entryPlan
	size    uint64
}

// committer makes verified files durable in batches: every file in a batch is
// fsynced (in parallel), then renamed into place, then each parent folder is
// synced once. A file counts as saved (OnFileDone) only after all of that,
// so what fsend reports as done survives a crash, without paying a full
// drive-cache flush per small file.
type committer struct {
	r       *receiver
	pending []pendingFile
	bytes   uint64
}

const (
	commitMaxFiles = 256
	commitMaxBytes = 64 << 20
)

func (c *committer) add(pf pendingFile) error {
	c.pending = append(c.pending, pf)
	c.bytes += pf.size
	if len(c.pending) >= commitMaxFiles || c.bytes >= commitMaxBytes {
		return c.flush()
	}
	return nil
}

func (c *committer) flush() error {
	batch := c.pending
	c.pending, c.bytes = nil, 0
	if len(batch) == 0 {
		return nil
	}
	r := c.r
	syncErr := parallelSync(batch)
	if syncErr == nil {
		syncErr = syncBarrier(batch[len(batch)-1].f)
	}
	for _, pf := range batch {
		_ = pf.f.Close()
	}
	if syncErr != nil {
		return r.writeFail("sync", "", syncErr)
	}
	dirs := make(map[string]struct{})
	var done []pendingFile
	var firstErr error
	for _, pf := range batch {
		placed, err := r.place(pf)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if placed {
			dirs[filepath.Dir(pf.plan.target)] = struct{}{}
			done = append(done, pf)
		}
	}
	syncDirs(dirs)
	for _, pf := range done {
		if r.opts.OnFileDone != nil {
			r.opts.OnFileDone(pf.plan.target, pf.plan.sha256)
		}
	}
	if firstErr != nil {
		return r.writeFail("finalize", "", firstErr)
	}
	return nil
}

// place renames a synced partial onto its target. The slot may hold only
// what the user agreed to replace: a file, a symlink or an empty folder.
func (r *receiver) place(pf pendingFile) (bool, error) {
	p := pf.plan
	target := p.target
	if st, err := os.Lstat(target); err == nil && p.needsConsent() {
		switch {
		case st.IsDir():
			// Never RemoveAll: os.Remove only succeeds on an empty folder.
			if err := os.Remove(target); err != nil {
				r.ps.remove(p.rel)
				p.disp, p.skipReason = dispRefused, "a folder with files is in the way"
				if r.opts.OnEntrySkipped != nil {
					r.opts.OnEntrySkipped(filepath.ToSlash(p.rel), p.skipReason)
				}
				return false, nil
			}
		case st.Mode()&os.ModeSymlink != 0:
			_ = os.Remove(target) // the link itself, never its target
		}
	}
	parent := filepath.Dir(target)
	r.dirs.unlock(parent)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, err
	}
	if err := moveFile(pf.partial, target); err != nil {
		return false, err
	}
	r.ps.dropMeta(p.rel)
	if runtime.GOOS == "windows" {
		// NTFS may stamp the write time again when the handle closes.
		t := time.Unix(p.entry.ModTimeSec, 0)
		_ = os.Chtimes(target, t, t)
	}
	return true, nil
}

// parallelSync flushes every file in batch to the device, a few at a time.
func parallelSync(batch []pendingFile) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	sem := make(chan struct{}, 8)
	for i := range batch {
		wg.Add(1)
		sem <- struct{}{}
		go func(f *os.File) {
			defer func() { <-sem; wg.Done() }()
			if err := fsyncData(f); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(batch[i].f)
	}
	wg.Wait()
	return first
}

// syncDirs makes the renames durable: each folder once, plus one barrier.
func syncDirs(dirs map[string]struct{}) {
	var last *os.File
	for d := range dirs {
		f, err := os.Open(d)
		if err != nil {
			continue
		}
		_ = fsyncData(f)
		if last != nil {
			_ = last.Close()
		}
		last = f
	}
	if last != nil {
		_ = syncBarrier(last)
		_ = last.Close()
	}
}

// dirUnlocker temporarily makes read-only folders writable so a re-send
// into a tree that arrived read-only works, and restores them afterwards.
type dirUnlocker struct {
	root string
	orig map[string]os.FileMode
}

func newDirUnlocker(root string) *dirUnlocker {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return &dirUnlocker{root: abs, orig: make(map[string]os.FileMode)}
}

func (d *dirUnlocker) unlock(dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil || !pathIsUnder(abs, d.root) {
		return
	}
	for cur := abs; ; cur = filepath.Dir(cur) {
		if _, seen := d.orig[cur]; !seen {
			// Lstat: never chmod through a symlink.
			if st, err := os.Lstat(cur); err == nil && st.IsDir() && st.Mode().Perm()&0o300 != 0o300 {
				if os.Chmod(cur, st.Mode().Perm()|0o700) == nil {
					d.orig[cur] = st.Mode().Perm()
				}
			}
		}
		if cur == d.root || filepath.Dir(cur) == cur {
			return
		}
	}
}

func (d *dirUnlocker) restore() {
	for dir, m := range d.orig {
		_ = os.Chmod(dir, m)
	}
	clear(d.orig)
}

// materialize creates a structural entry (dir / empty file).
func (r *receiver) materialize(p *entryPlan) error {
	target := p.target
	switch p.entry.Type {
	case wire.EntryDir:
		if p.needsConsent() {
			// Approved replace of a file or symlink in the folder's slot. A
			// blocked ancestor was cleared by its own (earlier) entry.
			if st, err := os.Lstat(target); err == nil && !st.IsDir() {
				_ = os.Remove(target)
			}
		}
		r.dirs.unlock(filepath.Dir(target))
		if err := os.MkdirAll(target, dirMode(p.entry.Mode)); err != nil {
			return r.writeFail("mkdir", p.rel, err)
		}
		return nil
	case wire.EntrySymlink:
		// Defense in depth: classify already rejects peer symlinks. Never
		// create one from peer input — a planted link lets a later write
		// traverse out of the receive dir. Fail closed.
		declineTransfer(r.s, wire.ErrCodeProtocolError, "symlink entries are not accepted")
		return fmt.Errorf("%w: symlink entry %q", fserrors.ErrPathTraversal, p.entry.RelativePath)
	default:
		// Empty regular file: same partial → sync → rename path as any other,
		// so an approved replace never truncates the old inode (or its
		// hardlinks) in place.
		f, path, err := r.ps.create(p.rel, p.entry)
		if err != nil {
			return r.writeFail("create", p.rel, err)
		}
		p.sha256 = emptySHA256
		_ = f.Chmod(fileMode(p.entry.Mode))
		t := time.Unix(p.entry.ModTimeSec, 0)
		_ = os.Chtimes(path, t, t)
		return r.commit.add(pendingFile{f: f, partial: path, plan: p})
	}
}

// restoreDirModes sets each received directory to the sender's mode, after
// its children are written. Applied last, deepest first. dirMode keeps the
// owner's rwx, so a later run can still write into the folder.
func restoreDirModes(plans []entryPlan) {
	for i := len(plans) - 1; i >= 0; i-- {
		p := &plans[i]
		if p.entry.Type != wire.EntryDir || p.blockedAncestor || p.disp == dispRefused {
			continue
		}
		want := dirMode(p.entry.Mode)
		// Lstat, not Stat: if a receiver-side symlink sits in the slot (a kept
		// dir↔symlink conflict), Stat would follow it and chmod the link's
		// target anywhere on disk. Lstat reports the link itself, so IsDir is
		// false and we skip it — we only ever chmod a real directory we own.
		if st, err := os.Lstat(p.target); err == nil && st.IsDir() && st.Mode()&os.ModePerm != want {
			_ = os.Chmod(p.target, want)
		}
	}
}

// repairIdenticalMode propagates a permission-only change onto an
// already-identical regular file, whose unchanged size+mtime (or content
// hash under --checksum) would otherwise skip it and never pick up the new
// mode. Best-effort; a chmod failure isn't worth failing the transfer.
func repairIdenticalMode(p *entryPlan) {
	if p.entry.Type != wire.EntryFile || p.blockedAncestor {
		return
	}
	want := fileMode(p.entry.Mode)
	// Lstat, not Stat: never chmod through a symlink swapped into the slot.
	if st, err := os.Lstat(p.target); err == nil && st.Mode().IsRegular() && st.Mode()&os.ModePerm != want {
		_ = os.Chmod(p.target, want)
	}
}

// planToDecision maps a plan to its wire decision and fires receiver-side
// notifications (skip / conflict-kept / refused).
func planToDecision(p *entryPlan, approveOverwrite bool, opts RecvOptions) wire.Decision {
	d := wire.Decision{Index: p.entry.Index}
	// A target reachable only through a receiver-side symlink is kept untouched
	// regardless of overwrite — sending or materializing it would write through
	// the link, out of the receive tree.
	if p.blockedAncestor {
		d.Action = wire.DecisionSkip
		d.Kept = true
		if opts.OnConflictKept != nil && p.entry.Type != wire.EntryDir {
			opts.OnConflictKept(p.entry.RelativePath)
		}
		return d
	}
	switch p.disp {
	case dispIdentical:
		d.Action = wire.DecisionSkip
		// Count only files/symlinks as "skipped" — an already-present
		// directory isn't a file, and counting it would inflate the
		// "N files unchanged" tally past the sender's file count.
		if opts.OnSkip != nil && p.entry.Type != wire.EntryDir {
			opts.OnSkip(p.entry.Index)
		}
	case dispResume:
		d.Action = wire.DecisionResume
		d.ResumeOffset = p.resumeOffset
		root := foldRoot(p.prefixBlocks, p.resumeOffset)
		d.PrefixRoot = root[:]
	case dispNew:
		d.Action = wire.DecisionSend
	case dispRefused:
		d.Action = wire.DecisionSkip
		d.Kept = true
		if opts.OnEntrySkipped != nil {
			opts.OnEntrySkipped(filepath.ToSlash(p.rel), p.skipReason)
		}
	case dispDiffers, dispConflict:
		if approveOverwrite {
			d.Action = wire.DecisionSend
		} else {
			d.Action = wire.DecisionSkip
			d.Kept = true
			// Count only files/symlinks as kept — a kept directory isn't a
			// user-facing file, and counting it would inflate the tally past
			// the sender's (which excludes dirs the same way).
			if opts.OnConflictKept != nil && p.entry.Type != wire.EntryDir {
				opts.OnConflictKept(p.entry.RelativePath)
			}
		}
	}
	return d
}

// StreamFileName is the filename a ModeStream payload lands under: the
// sanitized base of the peer-supplied display name, or a fixed fallback.
// Exported so the CLI accept prompt can show the exact name recvStream
// will write.
func StreamFileName(displayName string) string {
	name, err := SanitizeRelativePath(displayName)
	if err != nil {
		return "fsend-received"
	}
	base := filepath.Base(name)
	if base == "." || base == string(filepath.Separator) {
		return "fsend-received"
	}
	return base
}

// recvStream receives one ModeStream payload (stdin/--text) to a sink or a
// single file under TargetDir.
func recvStream(ctx context.Context, s *Streams, hello *wire.SenderHello, opts RecvOptions) error {
	accept := opts.Accept == nil || opts.Accept(*hello, ClassifySummary{})
	ackProceed(s, opts, accept)
	if !accept {
		drainControl(s)
		return fserrors.ErrReceiverDeclined
	}
	if hello.HasPassword {
		if err := receiverPasswordHandshake(s, opts); err != nil {
			return err
		}
	}

	w := opts.Sink
	var target, partial, rel string
	var f *os.File
	var sha hash.Hash
	ps := newPartialStore(opts.TargetDir)
	if w == nil {
		rel = StreamFileName(hello.DisplayName)
		target = filepath.Join(opts.TargetDir, rel)
		// An honest sender names streams with a random suffix, so an existing
		// target means a crafted DisplayName (or a freak collision) — refuse
		// to clobber it unless the user opted in.
		if _, lerr := os.Lstat(target); lerr == nil {
			switch opts.policy() {
			case ConflictReplace:
				if err := guardRegular(s, target, "target"); err != nil {
					return err
				}
			case ConflictRename:
				c := &classifier{targetDir: opts.TargetDir, sem: probeFS(opts.TargetDir)}
				rel = freeName(rel, c, map[string]struct{}{})
				target = filepath.Join(opts.TargetDir, rel)
			default:
				declineTransfer(s, wire.ErrCodeTargetExists, rel)
				return fmt.Errorf("%w: %s (use --overwrite to replace it)", fserrors.ErrTargetExists, rel)
			}
		}
		var err error
		f, partial, err = ps.create(rel, wire.ListingEntry{})
		if err != nil {
			declineTransfer(s, wire.ErrCodeWriteFailed, peerSafe("create", rel, err))
			return fmt.Errorf("%w: create stream file: %v", fserrors.ErrWriteFailed, err)
		}
		sha = sha256.New()
		w = io.MultiWriter(f, sha)
	}

	err := streamPayload(ctx, s, w, func(n uint64) {
		if opts.ProgressFn != nil {
			opts.ProgressFn(0, n)
		}
	})
	if f != nil {
		// Flush data before the rename so a crash can't commit a
		// correctly-named file with unpersisted contents.
		if err == nil {
			if serr := f.Sync(); serr != nil {
				err = fmt.Errorf("%w: sync stream file: %v", fserrors.ErrWriteFailed, serr)
			}
		}
		_ = f.Close()
		if err != nil {
			ps.remove(rel)
		}
	}
	if err != nil {
		return err
	}
	if f != nil {
		if err := moveFile(partial, target); err != nil {
			ps.remove(rel)
			declineTransfer(s, wire.ErrCodeWriteFailed, peerSafe("finalize", rel, err))
			return fmt.Errorf("%w: finalize: %v", fserrors.ErrWriteFailed, err)
		}
		ps.dropMeta(rel)
		ps.tidy()
		fsyncDir(filepath.Dir(target)) // make the rename itself durable
		if opts.OnFileDone != nil {
			opts.OnFileDone(target, hex.EncodeToString(sha.Sum(nil)))
		}
	}
	return finishRecv(s)
}

// streamPayload reads chunks for a single file/stream and writes their bytes
// to w, verifying the tree root on the EOF segment. Used for sink and stream
// receives, where no partial/resume applies.
func streamPayload(ctx context.Context, s *Streams, w io.Writer, progress func(uint64)) error {
	verifier := newFileHasher()
	defer verifier.discard()
	var written uint64
	var dec *zstd.Decoder
	defer func() {
		if dec != nil {
			dec.Close()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := wire.ReadChunk(s.Data)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if reason := tryReadPeerError(s.Control); reason != nil {
					return reason
				}
				return fmt.Errorf("recv: stream closed mid-file: %w", fserrors.ErrConnectFailed)
			}
			return fmt.Errorf("recv: chunk: %w", err)
		}
		plain, err := decodeChunkPayload(c, &dec, nil)
		if err != nil {
			return err
		}
		off := 0
		for _, seg := range c.Segments {
			data := plain[off : off+int(seg.Length)]
			off += int(seg.Length)
			if seg.FileIndex != 0 || seg.Restart {
				declineTransfer(s, wire.ErrCodeProtocolError, "unexpected segment in a single-file stream")
				return fmt.Errorf("%w: unexpected segment for index %d", fserrors.ErrProtocolError, seg.FileIndex)
			}
			if seg.Skip {
				return fmt.Errorf("%w: the sender could not read the file", fserrors.ErrReadFailed)
			}
			if len(data) > 0 {
				if _, err := w.Write(data); err != nil {
					_ = s.Data.Close()
					code := wire.ErrCodeWriteFailed
					if errors.Is(err, syscall.ENOSPC) {
						code = wire.ErrCodeDiskFull
					}
					declineTransfer(s, code, peerSafe("write", "", err))
					return classifyWriteErr("sink write", err)
				}
				_, _ = verifier.Write(data)
				written += uint64(len(data))
				if progress != nil {
					progress(written)
				}
			}
			if seg.EOF {
				if verifier.Sum() != seg.RootHash {
					declineTransfer(s, wire.ErrCodeFileHashMismatch, "root hash mismatch")
					return fserrors.ErrHashMismatch
				}
				return nil
			}
		}
	}
}

// --- helpers ---

// manifestEntries pairs each non-directory entry with what fsend did to it,
// for the --manifest record. Directories are structural, not user files.
func manifestEntries(plans []entryPlan, decisions []wire.Decision) []ManifestEntry {
	out := make([]ManifestEntry, 0, len(plans))
	for i := range plans {
		if plans[i].entry.Type == wire.EntryDir {
			continue
		}
		out = append(out, ManifestEntry{
			RelativePath: filepath.ToSlash(plans[i].rel),
			Size:         plans[i].entry.Size,
			Status:       manifestStatus(&plans[i], decisions[i].Action),
			SHA256:       plans[i].sha256,
		})
	}
	return out
}

// manifestStatus names the outcome from the plan and the decision the
// receiver sent (which already folded in the conflict choice).
func manifestStatus(p *entryPlan, a wire.DecisionAction) string {
	switch {
	case p.skipReason != "":
		return "skipped"
	case p.renamed:
		return "renamed"
	}
	switch a {
	case wire.DecisionResume:
		return "resumed"
	case wire.DecisionSkip:
		if p.disp == dispIdentical {
			return "identical"
		}
		return "kept" // differs/conflict left untouched (no consent)
	default: // DecisionSend
		if p.needsConsent() {
			return "overwritten"
		}
		return "new"
	}
}

func ackProceed(s *Streams, opts RecvOptions, accepts bool) {
	ack := &wire.ReceiverHello{
		MinVersion: wire.MinProtocolVersion, MaxVersion: wire.ProtocolVersion, Caps: wire.CapsSupported,
		Hostname: opts.Hostname, OS: opts.OS, ClientVersion: opts.ClientVersion, Accepts: accepts,
	}
	_ = wire.WriteControl(s.Control, wire.TypeHelloAck, ack)
}

func drainControl(s *Streams) {
	_ = s.Control.Close()
	_, _ = io.Copy(io.Discard, s.Control)
}

// writeFail reports a local filesystem failure to the sender (relative path
// and error class only) and returns the matching catalog error.
func (r *receiver) writeFail(op, rel string, err error) error {
	code := wire.ErrCodeWriteFailed
	if errors.Is(err, syscall.ENOSPC) {
		code = wire.ErrCodeDiskFull
	}
	if r.s.Data != nil {
		_ = r.s.Data.Close()
	}
	declineTransfer(r.s, code, peerSafe(op, filepath.ToSlash(rel), err))
	return classifyWriteErr(op, err)
}

// classifyWriteErr maps a filesystem write failure to the right catalog error.
func classifyWriteErr(op string, err error) error {
	base := fserrors.ErrWriteFailed
	if errors.Is(err, syscall.ENOSPC) {
		base = fserrors.ErrDiskFull
	}
	// err stays wrapped so the CLI can spot EPIPE on the stdout sink
	// (`--out -`) and exit 141 instead of rendering E009.
	return fmt.Errorf("%w: %s: %w", base, op, err)
}

// declineTransfer posts an ERROR frame and runs the symmetric shutdown. The
// data stream is closed first so a sender blocked writing chunks unblocks and
// falls through to read the reason on control, instead of deadlocking.
func declineTransfer(s *Streams, code wire.ErrorCode, msg string) {
	if s.Data != nil {
		_ = s.Data.Close()
	}
	_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{Code: code, Message: msg})
	_ = s.Control.Close()
	_, _ = io.Copy(io.Discard, s.Control)
}

// tryReadPeerError reads at most one frame and maps a TypeError to a sentinel.
func tryReadPeerError(r io.Reader) error {
	var ef wire.ErrorFrame
	ft, err := wire.ReadControl(r, &ef)
	if err != nil || ft != wire.TypeError {
		return nil
	}
	return mapPeerError(ef)
}

// mapPeerError translates a peer ERROR frame into a user-visible sentinel.
func mapPeerError(ef wire.ErrorFrame) error {
	switch ef.Code {
	case wire.ErrCodeWrongPassword:
		return fserrors.ErrWrongPassword
	case wire.ErrCodePasswordRequired:
		return fserrors.ErrPasswordRequired
	case wire.ErrCodeFileHashMismatch:
		return fserrors.ErrHashMismatch
	case wire.ErrCodePartialMismatch:
		return fserrors.ErrPartialMismatch
	case wire.ErrCodeTargetExists:
		return fserrors.ErrTargetExists
	case wire.ErrCodeListingInvalid:
		return fmt.Errorf("%w: %s", fserrors.ErrProtocolError, ef.Message)
	case wire.ErrCodeDeclined:
		return fserrors.ErrReceiverDeclined
	case wire.ErrCodeWriteFailed:
		return fmt.Errorf("%w: receiver: %s", fserrors.ErrWriteFailed, ef.Message)
	case wire.ErrCodeReadFailed:
		return fmt.Errorf("%w: sender: %s", fserrors.ErrReadFailed, ef.Message)
	case wire.ErrCodeDiskFull:
		return fmt.Errorf("%w: receiver: %s", fserrors.ErrDiskFull, ef.Message)
	case wire.ErrCodeTooManyEntries:
		return fmt.Errorf("%w: %s", fserrors.ErrTooManyEntries, ef.Message)
	case wire.ErrCodeCancelled:
		return fserrors.ErrPeerCancelled
	default:
		return fmt.Errorf("%w: peer reported %d: %s", fserrors.ErrProtocolError, ef.Code, ef.Message)
	}
}

// humanSize renders a byte count for messages ("1.5 GB").
func humanSize(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
