package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// RecvOptions configures one receive invocation.
type RecvOptions struct {
	Hostname      string
	OS            string
	ClientVersion string
	TargetDir     string // where files are written
	Overwrite     bool
	Checksum      bool // verify same-size files by content hash, not mtime

	// Accept is shown the HELLO and (ModeFiles) the classification breakdown.
	// nil → auto-accept. Returning false declines the whole transfer.
	Accept func(hello wire.SenderHello, summary ClassifySummary) bool
	// ConfirmOverwrite is the consolidated prompt for differing/conflicting
	// entries. nil or false → keep them (skip). Only consulted when
	// Overwrite is false and there is a terminal to ask.
	ConfirmOverwrite func(conflicts []Conflict) bool

	Password   string
	PromptPass func() (string, error)

	ProgressFn func(index uint32, bytesWritten uint64)
	OnResume   func(index uint32, offset, total uint64)
	OnSkip     func(index uint32)
	OnFileDone func(path string)
	// OnConflictKept fires once per differing/conflicting entry left
	// untouched (no consent). The CLI uses it to set a non-zero exit code.
	OnConflictKept func(rel string)
	// OnManifest, when set, receives one row per file after a successful
	// receive — its path, size, and what fsend did with it — backing --manifest.
	OnManifest func(entries []ManifestEntry)

	// Sink, when non-nil, streams a ModeStream payload here instead of
	// writing under TargetDir.
	Sink io.Writer
}

// ClassifySummary is the breakdown surfaced to the accept prompt.
type ClassifySummary = classifySummary

// ManifestEntry is one row of the post-transfer --manifest record: a file and
// what fsend did with it.
type ManifestEntry struct {
	RelativePath string
	Size         uint64
	Status       string // new | identical | overwritten | kept | resumed
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
			_ = wire.WriteControl(s.Control, wire.TypeHelloAck, &wire.ReceiverHello{Accepts: false})
			drainControl(s)
			return fserrors.ErrIncompatibleVersion
		}
		return fmt.Errorf("recv: hello: %w", err)
	}
	if ft != wire.TypeHello {
		return fmt.Errorf("%w: expected HELLO, got %v", fserrors.ErrProtocolError, ft)
	}
	if hello.ProtocolVersion != wire.ProtocolVersion {
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
		return err
	}

	if opts.Sink != nil {
		return recvFilesToSink(ctx, s, hello, entries, opts)
	}
	plans, err := classify(entries, opts.TargetDir, opts.Checksum)
	if err != nil {
		declineTransfer(s, wire.ErrCodeListingInvalid, "invalid listing")
		return err
	}

	// --checksum: resolve same-size candidates by content hash before the
	// breakdown is shown or any decision is made.
	if opts.Checksum {
		if err := resolveVerify(s, plans); err != nil {
			return err
		}
	}

	summary := summarize(plans)
	if opts.Accept != nil && !opts.Accept(*hello, summary) {
		declineTransfer(s, wire.ErrCodeDeclined, "receiver declined")
		return fserrors.ErrReceiverDeclined
	}

	// Consent for differing/conflicting entries.
	confs := conflicts(plans)
	approveOverwrite := false
	switch {
	case len(confs) == 0:
	case opts.Overwrite:
		approveOverwrite = true
	case opts.ConfirmOverwrite != nil:
		approveOverwrite = opts.ConfirmOverwrite(confs)
	}

	decisions := make([]wire.Decision, len(plans))
	for i := range plans {
		decisions[i] = planToDecision(&plans[i], approveOverwrite, opts)
	}
	if err := sendDecisions(s.Control, decisions); err != nil {
		return fmt.Errorf("recv: decisions: %w", err)
	}

	// Materialize structural entries (dirs, symlinks, empty files) we're
	// creating; data files are opened lazily during the data phase.
	expected := 0
	byIndex := make(map[uint32]*entryPlan, len(plans))
	for i := range plans {
		p := &plans[i]
		byIndex[p.entry.Index] = p
		if decisions[i].Action == wire.DecisionSkip {
			continue
		}
		if p.entry.Type == wire.EntryFile && p.entry.Size > 0 {
			expected++
			continue
		}
		if err := materialize(s, p, approveOverwrite); err != nil {
			return err
		}
	}

	if err := receiveData(ctx, s, byIndex, decisions, plans, expected, opts); err != nil {
		return err
	}
	if err := finishRecv(s); err != nil {
		return err
	}
	if opts.OnManifest != nil {
		opts.OnManifest(manifestEntries(plans, decisions))
	}
	return nil
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

// recvFilesToSink streams a single-file transfer to opts.Sink. Multi-file and
// directory transfers can't be concatenated into one byte stream, so they are
// declined with a usage error.
func recvFilesToSink(ctx context.Context, s *Streams, hello *wire.SenderHello, entries []wire.ListingEntry, opts RecvOptions) error {
	if len(entries) != 1 || entries[0].Type != wire.EntryFile {
		declineTransfer(s, wire.ErrCodeDeclined, "multi-file transfer to stdout")
		return fmt.Errorf("%w: cannot stream a multi-file transfer to stdout; receive it with --out <dir>", fserrors.ErrUsage)
	}
	summary := ClassifySummary{
		Total: 1, NewItems: 1,
		BytesToRecv:  entries[0].Size,
		OfferedBytes: entries[0].Size,
		Files: []SummaryEntry{{
			RelativePath: entries[0].RelativePath, Size: entries[0].Size,
			Status: "new", Type: entries[0].Type,
		}},
	}
	if opts.Accept != nil && !opts.Accept(*hello, summary) {
		declineTransfer(s, wire.ErrCodeDeclined, "receiver declined")
		return fserrors.ErrReceiverDeclined
	}
	if err := sendDecisions(s.Control, []wire.Decision{{Index: 0, Action: wire.DecisionSend}}); err != nil {
		return fmt.Errorf("recv: decisions: %w", err)
	}
	if err := streamPayload(ctx, s, opts.Sink, func(n uint64) {
		if opts.ProgressFn != nil {
			opts.ProgressFn(0, n)
		}
	}); err != nil {
		return err
	}
	return finishRecv(s)
}

// streamPayload reads chunks for a single file/stream and writes their bytes
// to w, verifying both the per-chunk hash and the file's BLAKE3 root (on the
// EOF segment). Returns once the EOF segment lands. Used for sink and stream
// receives, where no partial/resume applies.
func streamPayload(ctx context.Context, s *Streams, w io.Writer, progress func(uint64)) error {
	verifier := blake3.New()
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
		plain, err := decodeChunkPayload(c, &dec)
		if err != nil {
			return err
		}
		if len(plain) > 0 {
			if _, err := w.Write(plain); err != nil {
				_ = s.Data.Close()
				declineTransfer(s, wire.ErrCodeWriteFailed, "write failed: "+err.Error())
				return classifyWriteErr("sink write", err)
			}
			_, _ = verifier.Write(plain)
			written += uint64(len(plain))
			if progress != nil {
				progress(written)
			}
		}
		for _, seg := range c.Segments {
			if seg.EOF {
				var got [32]byte
				copy(got[:], verifier.Sum(nil))
				if got != seg.RootHash {
					declineTransfer(s, wire.ErrCodeFileHashMismatch, "root hash mismatch")
					return fserrors.ErrHashMismatch
				}
				return nil
			}
		}
	}
}

// resolveVerify (--checksum) asks the sender for the BLAKE3 root of every
// same-size candidate, hashes the local copies, and rewrites each dispVerify
// to dispIdentical (match) or dispDiffers. Hashing failures fall to dispDiffers
// so an unreadable/changed file is re-sent rather than wrongly skipped.
func resolveVerify(s *Streams, plans []entryPlan) error {
	var idx []uint32
	for i := range plans {
		if plans[i].disp == dispVerify {
			idx = append(idx, plans[i].entry.Index)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	if err := wire.WriteControl(s.Control, wire.TypeVerifyRequest, idx); err != nil {
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
	hashes := make(map[uint32][32]byte, len(resp))
	for _, fh := range resp {
		hashes[fh.Index] = fh.Hash
	}
	for i := range plans {
		p := &plans[i]
		if p.disp != dispVerify {
			continue
		}
		senderHash, ok := hashes[p.entry.Index]
		if !ok {
			p.disp = dispDiffers
			continue
		}
		local, err := hashFileRoot(p.target)
		if err == nil && local == senderHash {
			p.disp = dispIdentical
			_ = os.Remove(p.target + partialSuffix)
		} else {
			p.disp = dispDiffers
		}
	}
	return nil
}

// receiveData consumes chunks until every expected file reaches EOF.
func receiveData(ctx context.Context, s *Streams, byIndex map[uint32]*entryPlan, decisions []wire.Decision, plans []entryPlan, expected int, opts RecvOptions) error {
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

	open := make(map[uint32]*recvFile)
	var dec *zstd.Decoder
	defer func() {
		if dec != nil {
			dec.Close()
		}
		for _, rf := range open {
			_ = rf.f.Close()
		}
	}()

	for expected > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := wire.ReadChunk(s.Data)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if reason := tryReadPeerError(s.Control); reason != nil {
					// A content-correctness failure means the on-disk prefix is
					// unreconcilable; discard partials so a re-run starts clean.
					// The peer never says which file, so drop every resume
					// partial plus any open one.
					if errors.Is(reason, fserrors.ErrPartialMismatch) || errors.Is(reason, fserrors.ErrHashMismatch) {
						discardOpenPartials(open)
						for i := range plans {
							if plans[i].disp == dispResume {
								_ = os.Remove(plans[i].target + partialSuffix)
							}
						}
					}
					return reason
				}
				return fmt.Errorf("recv: stream closed mid-file: %w", fserrors.ErrConnectFailed)
			}
			return fmt.Errorf("recv: chunk: %w", err)
		}
		plain, err := decodeChunkPayload(c, &dec)
		if err != nil {
			return err
		}
		off := 0
		for _, seg := range c.Segments {
			data := plain[off : off+int(seg.Length)]
			off += int(seg.Length)
			rf := open[seg.FileIndex]
			if rf == nil {
				if !allowed[seg.FileIndex] || done[seg.FileIndex] {
					declineTransfer(s, wire.ErrCodeListingInvalid, "chunk for a file not awaiting data")
					return fmt.Errorf("%w: chunk for file index %d not awaiting data", fserrors.ErrProtocolError, seg.FileIndex)
				}
				rf, err = openRecvFile(s, byIndex[seg.FileIndex], opts)
				if err != nil {
					return err
				}
				open[seg.FileIndex] = rf
			}
			if err := rf.write(s, data, &opts); err != nil {
				return err
			}
			if seg.EOF {
				if err := rf.finalize(s, seg.RootHash, opts); err != nil {
					return err
				}
				delete(open, seg.FileIndex)
				done[seg.FileIndex] = true
				expected--
			}
		}
	}
	return nil
}

// recvFile is one in-flight file's write state.
type recvFile struct {
	plan     *entryPlan
	f        *os.File
	partial  string
	verifier *blake3.Hasher
	written  uint64
	overwr   bool
}

func openRecvFile(s *Streams, p *entryPlan, opts RecvOptions) (*recvFile, error) {
	target := p.target
	partial := target + partialSuffix
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		declineTransfer(s, wire.ErrCodeWriteFailed, "mkdir parent")
		return nil, fmt.Errorf("%w: mkdir parent: %v", fserrors.ErrWriteFailed, err)
	}
	// Lstat-guard: a planted symlink at the sidecar must not redirect writes.
	if st, err := os.Lstat(partial); err == nil && !st.Mode().IsRegular() {
		declineTransfer(s, wire.ErrCodeProtocolError, "partial sidecar not a regular file")
		return nil, fmt.Errorf("%w: partial %s not a regular file", fserrors.ErrWriteFailed, partial)
	}

	rf := &recvFile{plan: p, partial: partial, verifier: blake3.New(), overwr: p.needsConsent()}
	resume := p.disp == dispResume

	flag := os.O_RDWR | os.O_CREATE
	if !resume {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(partial, flag, 0o600)
	if errors.Is(err, os.ErrPermission) && !resume {
		_ = os.Remove(partial)
		f, err = os.OpenFile(partial, flag, 0o600)
	}
	if err != nil {
		_ = s.Data.Close()
		declineTransfer(s, wire.ErrCodeWriteFailed, "open failed: "+err.Error())
		return nil, fmt.Errorf("%w: open partial: %v", fserrors.ErrWriteFailed, err)
	}
	rf.f = f

	if resume {
		off := int64(p.resumeOffset)
		if err := f.Truncate(off); err != nil {
			return rf, writeFail(s, "truncate", err)
		}
		if err := hashPrefixInto(rf.verifier, f, off); err != nil {
			return rf, fmt.Errorf("%w: verify prefix: %v", fserrors.ErrReadFailed, err)
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return rf, writeFail(s, "seek", err)
		}
		rf.written = p.resumeOffset
		if opts.OnResume != nil {
			opts.OnResume(p.entry.Index, p.resumeOffset, p.entry.Size)
		}
	}
	return rf, nil
}

func (rf *recvFile) write(s *Streams, data []byte, opts *RecvOptions) error {
	if len(data) == 0 {
		return nil
	}
	if rf.written+uint64(len(data)) > rf.plan.entry.Size {
		declineTransfer(s, wire.ErrCodeProtocolError, "received bytes exceed declared size")
		return fmt.Errorf("%w: received bytes exceed size %d", fserrors.ErrProtocolError, rf.plan.entry.Size)
	}
	if _, err := rf.f.Write(data); err != nil {
		_ = s.Data.Close()
		declineTransfer(s, wire.ErrCodeWriteFailed, "write failed: "+err.Error())
		return classifyWriteErr("write", err)
	}
	_, _ = rf.verifier.Write(data)
	rf.written += uint64(len(data))
	if opts.ProgressFn != nil {
		opts.ProgressFn(rf.plan.entry.Index, rf.written)
	}
	return nil
}

func (rf *recvFile) finalize(s *Streams, root [32]byte, opts RecvOptions) error {
	var got [32]byte
	copy(got[:], rf.verifier.Sum(nil))
	if got != root {
		_ = rf.f.Close()
		_ = os.Remove(rf.partial)
		declineTransfer(s, wire.ErrCodeFileHashMismatch, "root hash mismatch")
		return fserrors.ErrHashMismatch
	}
	// Flush data to stable storage before the rename so a crash can't leave a
	// size-correct, mtime-correct file with unpersisted (garbage) contents —
	// which a later skip-unchanged run would then trust forever.
	if err := rf.f.Sync(); err != nil {
		_ = rf.f.Close()
		return fmt.Errorf("%w: sync partial: %v", fserrors.ErrWriteFailed, err)
	}
	if err := rf.f.Close(); err != nil {
		return fmt.Errorf("%w: close partial: %v", fserrors.ErrWriteFailed, err)
	}
	target := rf.plan.target
	// Replacing a dir/symlink (an approved conflict) needs the slot cleared
	// before the rename.
	if rf.overwr {
		if st, err := os.Lstat(target); err == nil && (st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
			_ = os.RemoveAll(target)
		}
	}
	if err := os.Rename(rf.partial, target); err != nil {
		return fmt.Errorf("%w: finalize: %v", fserrors.ErrWriteFailed, err)
	}
	fsyncDir(filepath.Dir(target)) // make the rename itself durable
	_ = os.Chmod(target, os.FileMode(rf.plan.entry.Mode)&os.ModePerm)
	if rf.plan.entry.ModTimeSec > 0 {
		t := time.Unix(rf.plan.entry.ModTimeSec, 0)
		_ = os.Chtimes(target, t, t)
	}
	if opts.OnFileDone != nil {
		opts.OnFileDone(target)
	}
	return nil
}

// materialize creates a structural entry (dir / symlink / empty file).
func materialize(s *Streams, p *entryPlan, approveOverwrite bool) error {
	target := p.target
	if p.needsConsent() && approveOverwrite {
		_ = os.RemoveAll(target) // clear the slot (approved)
	}
	switch p.entry.Type {
	case wire.EntryDir:
		if err := os.MkdirAll(target, os.FileMode(p.entry.Mode)&os.ModePerm|0o700); err != nil {
			declineTransfer(s, wire.ErrCodeWriteFailed, "mkdir failed")
			return fmt.Errorf("%w: mkdir %s: %v", fserrors.ErrWriteFailed, target, err)
		}
		return nil
	case wire.EntrySymlink:
		// Defense in depth: classify already rejects peer symlinks. Never
		// create one from peer input — a planted link lets a later write
		// traverse out of the receive dir. Fail closed.
		declineTransfer(s, wire.ErrCodeProtocolError, "symlink entries are not accepted")
		return fmt.Errorf("%w: symlink entry %q", fserrors.ErrPathTraversal, p.entry.RelativePath)
	default: // empty regular file
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			declineTransfer(s, wire.ErrCodeWriteFailed, "mkdir parent")
			return fmt.Errorf("%w: mkdir parent: %v", fserrors.ErrWriteFailed, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(p.entry.Mode)&os.ModePerm)
		if err != nil {
			declineTransfer(s, wire.ErrCodeWriteFailed, "create empty: "+err.Error())
			return fmt.Errorf("%w: create %s: %v", fserrors.ErrWriteFailed, target, err)
		}
		_ = f.Close()
		if p.entry.ModTimeSec > 0 {
			t := time.Unix(p.entry.ModTimeSec, 0)
			_ = os.Chtimes(target, t, t)
		}
		return nil
	}
}

// planToDecision maps a plan to its wire decision and fires receiver-side
// notifications (skip / conflict-kept).
func planToDecision(p *entryPlan, approveOverwrite bool, opts RecvOptions) wire.Decision {
	d := wire.Decision{Index: p.entry.Index}
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
		d.PartialImohash = p.imohash
	case dispNew:
		d.Action = wire.DecisionSend
	case dispDiffers, dispConflict:
		if approveOverwrite {
			d.Action = wire.DecisionSend
		} else {
			d.Action = wire.DecisionSkip
			if opts.OnConflictKept != nil {
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
	var target string
	var f *os.File
	if w == nil {
		base := StreamFileName(hello.DisplayName)
		target = filepath.Join(opts.TargetDir, base)
		// An honest sender names streams with a random suffix, so an existing
		// target means a crafted DisplayName (or a freak collision) — refuse
		// to clobber it unless the user opted in with --overwrite.
		if !opts.Overwrite {
			if _, lerr := os.Lstat(target); lerr == nil {
				declineTransfer(s, wire.ErrCodeTargetExists, base)
				return fmt.Errorf("%w: %s (use --overwrite to replace it)", fserrors.ErrTargetExists, base)
			}
		}
		var err error
		f, err = os.OpenFile(target+partialSuffix, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			declineTransfer(s, wire.ErrCodeWriteFailed, "create: "+err.Error())
			return fmt.Errorf("%w: create stream file: %v", fserrors.ErrWriteFailed, err)
		}
		w = f
	}

	err := streamPayload(ctx, s, w, func(n uint64) {
		if opts.ProgressFn != nil {
			opts.ProgressFn(0, n)
		}
	})
	if f != nil {
		// Flush data before the rename (see finalize) so a crash can't commit
		// a correctly-named file with unpersisted contents.
		if err == nil {
			if serr := f.Sync(); serr != nil {
				err = fmt.Errorf("%w: sync stream file: %v", fserrors.ErrWriteFailed, serr)
			}
		}
		_ = f.Close()
		if err != nil {
			_ = os.Remove(target + partialSuffix)
		}
	}
	if err != nil {
		return err
	}
	if f != nil {
		if err := os.Rename(target+partialSuffix, target); err != nil {
			return fmt.Errorf("%w: finalize: %v", fserrors.ErrWriteFailed, err)
		}
		fsyncDir(filepath.Dir(target)) // make the rename itself durable
		if opts.OnFileDone != nil {
			opts.OnFileDone(target)
		}
	}
	return finishRecv(s)
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
			RelativePath: plans[i].entry.RelativePath,
			Size:         plans[i].entry.Size,
			Status:       manifestStatus(plans[i].disp, decisions[i].Action),
		})
	}
	return out
}

// manifestStatus names the outcome from the disposition and the decision the
// receiver sent (which already folded in the overwrite choice).
func manifestStatus(d disposition, a wire.DecisionAction) string {
	switch a {
	case wire.DecisionResume:
		return "resumed"
	case wire.DecisionSkip:
		if d == dispIdentical {
			return "identical"
		}
		return "kept" // differs/conflict left untouched (no consent)
	default: // DecisionSend
		if d == dispDiffers || d == dispConflict {
			return "overwritten"
		}
		return "new"
	}
}

func ackProceed(s *Streams, opts RecvOptions, accepts bool) {
	ack := &wire.ReceiverHello{
		Hostname: opts.Hostname, OS: opts.OS, ClientVersion: opts.ClientVersion, Accepts: accepts,
	}
	_ = wire.WriteControl(s.Control, wire.TypeHelloAck, ack)
}

func drainControl(s *Streams) {
	_ = s.Control.Close()
	_, _ = io.Copy(io.Discard, s.Control)
}

func discardOpenPartials(open map[uint32]*recvFile) {
	for _, rf := range open {
		_ = rf.f.Close()
		_ = os.Remove(rf.partial)
	}
}

func writeFail(s *Streams, op string, err error) error {
	_ = s.Data.Close()
	declineTransfer(s, wire.ErrCodeWriteFailed, op+" failed: "+err.Error())
	return fmt.Errorf("%w: %s: %v", fserrors.ErrWriteFailed, op, err)
}

// classifyWriteErr maps a filesystem write failure to the right catalog error.
func classifyWriteErr(op string, err error) error {
	base := fserrors.ErrWriteFailed
	if errors.Is(err, syscall.ENOSPC) {
		base = fserrors.ErrDiskFull
	}
	return fmt.Errorf("%w: %s: %v", base, op, err)
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
	case wire.ErrCodeCancelled:
		return fserrors.ErrPeerCancelled
	default:
		return fmt.Errorf("%w: peer reported %d: %s", fserrors.ErrProtocolError, ef.Code, ef.Message)
	}
}

// receiverPasswordHandshake answers the sender's --password challenge.
func receiverPasswordHandshake(s *Streams, opts RecvOptions) error {
	var ch wire.PasswordChallenge
	ft, err := wire.ReadControl(s.Control, &ch)
	if err != nil {
		return fmt.Errorf("recv: read password challenge: %w", err)
	}
	if ft != wire.TypePasswordChallenge {
		return fmt.Errorf("%w: expected PASSWORD_CHALLENGE, got %v", fserrors.ErrProtocolError, ft)
	}
	password := opts.Password
	if password == "" {
		if opts.PromptPass == nil {
			declineTransfer(s, wire.ErrCodePasswordRequired, "receiver has no password to offer")
			return fserrors.ErrPasswordRequired
		}
		password, err = opts.PromptPass()
		if err != nil {
			return fmt.Errorf("recv: read password: %w", err)
		}
	}
	mac := hmacPassword(password, ch.Nonce[:])
	var resp wire.PasswordResponse
	copy(resp.HMAC[:], mac)
	if err := wire.WriteControl(s.Control, wire.TypePasswordResponse, &resp); err != nil {
		return fmt.Errorf("recv: write password response: %w", err)
	}
	var ef wire.ErrorFrame
	ft, err = wire.ReadControl(s.Control, &ef)
	if err != nil {
		return fmt.Errorf("recv: read password verdict: %w", err)
	}
	switch ft {
	case wire.TypePasswordVerified:
		return nil
	case wire.TypeError:
		if ef.Code == wire.ErrCodeWrongPassword {
			return fserrors.ErrWrongPassword
		}
		return fmt.Errorf("%w: peer reported %d: %s", fserrors.ErrProtocolError, ef.Code, ef.Message)
	default:
		return fmt.Errorf("%w: expected PASSWORD_VERIFIED, got %v", fserrors.ErrProtocolError, ft)
	}
}
