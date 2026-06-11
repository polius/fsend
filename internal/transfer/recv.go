package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// classifyWriteErr maps a filesystem write failure to the right catalog
// error: a full disk gets E008 (ErrDiskFull) with its "free up space"
// hint, everything else falls back to E009 (ErrWriteFailed). op names the
// operation for the debug detail line.
func classifyWriteErr(op string, err error) error {
	base := fserrors.ErrWriteFailed
	if errors.Is(err, syscall.ENOSPC) {
		base = fserrors.ErrDiskFull
	}
	return fmt.Errorf("%w: %s: %v", base, op, err)
}

// RecvOptions configures one receive invocation.
type RecvOptions struct {
	Hostname      string
	OS            string
	ClientVersion string
	TargetDir     string // where files are written
	Overwrite     bool
	Accept        func(hello wire.SenderHello) bool           // prompt callback; nil → auto-accept (legacy --yes)
	Password      string                                      // pre-supplied (--pass or FSEND_PASS); used when sender requires a password
	PromptPass    func() (string, error)                      // fallback when sender requires a password and Password is empty
	ProgressFn    func(fileIndex uint32, bytesWritten uint64) // optional
	// OnResume fires once per file we elected to resume, before any new
	// bytes arrive. Lets the CLI announce the resume and count only the
	// tail as moved. offset is chunk-aligned; total is the file's size.
	OnResume func(fileIndex uint32, offset, total uint64)
	// ConfirmOverwrite is called when an incoming file would clobber an
	// existing one and Overwrite is false. true → accept this file as if
	// Overwrite were set; false or nil → E013 reject.
	ConfirmOverwrite func(relativePath string, existingSize int64, incomingSize uint64) bool
	// Sink, when non-nil, streams the payload to this writer instead of
	// writing files under TargetDir. Single-payload transfers only
	// (single file, text, piped stream); directory and multi-file
	// transfers are declined before the Accept prompt. Resume never
	// applies — emitted bytes can't be reconciled — so callers must not
	// retry a sink receive.
	Sink io.Writer
	// OnFileDone is called with each file's final path after it has been
	// verified and renamed into place. Not called in archive or Sink mode.
	OnFileDone func(path string)
}

// Recv executes the full receiver-side protocol over the supplied streams.
func Recv(ctx context.Context, s *Streams, opts RecvOptions) error {
	var hello wire.SenderHello
	ft, err := wire.ReadControl(s.Control, &hello)
	if err != nil {
		return fmt.Errorf("recv: hello: %w", err)
	}
	if ft != wire.TypeHello {
		return fmt.Errorf("%w: expected HELLO, got %v", fserrors.ErrProtocolError, ft)
	}
	if hello.ProtocolVersion != wire.ProtocolVersion {
		return fmt.Errorf("%w: peer speaks v%d, we speak v%d", fserrors.ErrProtocolError, hello.ProtocolVersion, wire.ProtocolVersion)
	}

	// Sink mode carries one byte stream; concatenating a directory tar or
	// several files into it would hand the user garbage. Decline before
	// the Accept prompt so the user isn't asked about a doomed transfer.
	if opts.Sink != nil &&
		(hello.TransferKind == wire.TransferDirectory || hello.TransferKind == wire.TransferMultiFile) {
		ack := &wire.ReceiverHello{
			Hostname: opts.Hostname, OS: opts.OS, ClientVersion: opts.ClientVersion,
		}
		_ = wire.WriteControl(s.Control, wire.TypeHelloAck, ack)
		_ = s.Control.Close()
		_, _ = io.Copy(io.Discard, s.Control)
		what := "directory"
		if hello.TransferKind == wire.TransferMultiFile {
			what = "multi-file"
		}
		return fmt.Errorf("%w: cannot stream a %s transfer to stdout; receive it with --out <dir>", fserrors.ErrUsage, what)
	}

	accept := true
	if opts.Accept != nil {
		accept = opts.Accept(hello)
	}
	ack := &wire.ReceiverHello{
		Hostname:      opts.Hostname,
		OS:            opts.OS,
		ClientVersion: opts.ClientVersion,
		Accepts:       accept,
	}
	if err := wire.WriteControl(s.Control, wire.TypeHelloAck, ack); err != nil {
		return fmt.Errorf("recv: hello-ack: %w", err)
	}
	if !accept {
		// Close our write side so the FIN flushes HELLO_ACK to the
		// sender — without this, an immediate transport close races the
		// buffered bytes and the sender hangs waiting for HELLO_ACK
		// that's already been QUIC-buffered but never delivered.
		_ = s.Control.Close()
		// Drain any in-flight sender writes so the sender's send-side
		// FIN can land cleanly. Best-effort; ignore errors.
		_, _ = io.Copy(io.Discard, s.Control)
		return fserrors.ErrReceiverDeclined
	}

	if hello.HasPassword {
		if err := receiverPasswordHandshake(s, opts); err != nil {
			return err
		}
	}

	// Archive mode: a TransferDirectory hello means the sender bundled
	// one or more directories (and any sibling files) into a single
	// deterministic tar. The wire protocol below is identical to the
	// single-file case — one FILE_INFO, one FILE_ACCEPT, one chunk
	// stream — but after the tar lands and verifies we extract it into
	// the target directory instead of renaming it into place.
	archiveMode := hello.TransferKind == wire.TransferDirectory

	// The user consented to the HELLO's totals at the accept prompt;
	// without enforcement they are display-only, and a malicious sender
	// could advertise "1 file · 5 B" and then stream FILE_INFO frames
	// until the disk fills. Every kind carries exactly one FILE_INFO
	// (directory transfers wrap everything in one tar) except multi-file,
	// which carries TotalFiles.
	maxFiles := uint64(1)
	if hello.TransferKind == wire.TransferMultiFile {
		maxFiles = uint64(hello.TotalFiles)
	}
	var filesSeen, bytesDeclared uint64

	// Consent integrity: when the HELLO advertised the complete name
	// list (multi-file with TotalFiles ≤ MaxHelloFileNames), the user
	// consented to exactly those names — enforce that nothing else
	// lands. Incomplete (capped) or absent lists can't be enforced.
	var advertised map[string]bool
	if hello.TransferKind == wire.TransferMultiFile &&
		len(hello.FileNames) > 0 && uint32(len(hello.FileNames)) == hello.TotalFiles {
		advertised = make(map[string]bool, len(hello.FileNames))
		for _, n := range hello.FileNames {
			advertised[n] = true
		}
	}

	// File loop.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ft, body, err := wire.ReadControlRaw(s.Control)
		if err != nil {
			return fmt.Errorf("recv: file-info: %w", err)
		}
		if ft == wire.TypeTransferComplete {
			break
		}
		if ft == wire.TypeError {
			// Sender aborted between files (e.g. Ctrl-C) — surface its
			// stated reason instead of a generic protocol abort.
			var ef wire.ErrorFrame
			_ = wire.Decode(body, &ef)
			return mapPeerError(ef)
		}
		if ft != wire.TypeFileInfo {
			return fmt.Errorf("%w: expected FILE_INFO, got %v", fserrors.ErrProtocolError, ft)
		}
		var info wire.FileInfo
		if err := wire.Decode(body, &info); err != nil {
			return fmt.Errorf("recv: file-info: %w", err)
		}
		// Streaming exempts a file from the declared-size cap and the
		// root-hash check. Only a stdin transfer legitimately needs that;
		// accepting it elsewhere would let a sender who advertised a
		// small size stream unbounded, unverified bytes.
		if info.Streaming && hello.TransferKind != wire.TransferStdin {
			declineTransfer(s, wire.ErrCodeProtocolError, "streaming file in non-stdin transfer")
			return fmt.Errorf("%w: streaming FILE_INFO in a non-stdin transfer", fserrors.ErrProtocolError)
		}
		filesSeen++
		if filesSeen > maxFiles {
			declineTransfer(s, wire.ErrCodeProtocolError, "more files than HELLO declared")
			return fmt.Errorf("%w: FILE_INFO count exceeds the %d declared in HELLO", fserrors.ErrProtocolError, maxFiles)
		}
		bytesDeclared += info.Size
		if bytesDeclared > hello.TotalBytes {
			declineTransfer(s, wire.ErrCodeProtocolError, "declared sizes exceed HELLO total")
			return fmt.Errorf("%w: declared sizes exceed the %d bytes in HELLO", fserrors.ErrProtocolError, hello.TotalBytes)
		}
		// Legitimate senders only ever put bare basenames in RelativePath
		// (Walk basenames, ArchiveName, synthetic stdin/text names); a
		// separator means a peer creating directory trees the user never
		// consented to.
		if strings.ContainsAny(info.RelativePath, `/\`) {
			declineTransfer(s, wire.ErrCodeProtocolError, "path separator in file name")
			return fmt.Errorf("%w: RelativePath %q contains a path separator", fserrors.ErrProtocolError, info.RelativePath)
		}
		if advertised != nil && !advertised[info.RelativePath] {
			declineTransfer(s, wire.ErrCodeProtocolError, "file not in advertised name list")
			return fmt.Errorf("%w: file %q was not among the advertised names", fserrors.ErrProtocolError, info.RelativePath)
		}
		if err := recvOneFile(ctx, s, &info, opts, archiveMode); err != nil {
			return err
		}
	}

	if err := wire.WriteControl(s.Control, wire.TypeTransferAck, nil); err != nil {
		return err
	}
	// Wait for the sender to acknowledge our ack by closing their Control
	// send side (FIN). This guarantees the ack bytes have left our buffers
	// and reached the peer before any caller-driven transport close runs.
	_, _ = io.Copy(io.Discard, s.Control)
	return nil
}

func recvOneFile(ctx context.Context, s *Streams, info *wire.FileInfo, opts RecvOptions, archiveMode bool) error {
	if opts.Sink != nil {
		return recvPayloadToSink(ctx, s, info, opts)
	}
	// In archive mode the sender's RelativePath is a fixed placeholder
	// (ArchiveName); we don't actually surface that name to the user.
	// We just need a stable, hidden temp location inside TargetDir for
	// the partial file. Extraction happens after the bytes land.
	var target string
	if archiveMode {
		target = filepath.Join(opts.TargetDir, archivePartialName)
	} else {
		// Sanitize the peer-supplied relative path before composing the target.
		relClean, err := SanitizeRelativePath(info.RelativePath)
		if err != nil {
			_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
				Code: wire.ErrCodeProtocolError, Message: "bad path",
			})
			return fmt.Errorf("%w: %v", fserrors.ErrPathTraversal, err)
		}
		target = filepath.Join(opts.TargetDir, relClean)
	}

	// Defensive: confirm target stays under TargetDir even after filesystem
	// resolution.
	if absTarget, err := filepath.Abs(target); err == nil {
		if absDir, err := filepath.Abs(opts.TargetDir); err == nil {
			if !pathIsUnder(absTarget, absDir) {
				return fserrors.ErrPathTraversal
			}
		}
	}

	// Handle directory entries: just MkdirAll and ACK.
	if info.IsDir {
		// Mask to perm bits: don't honor peer-set setuid/setgid/sticky
		// (parity with the archive path). Owner rwx is forced so a
		// read-only mode (0555) can't block writing the files inside.
		if err := os.MkdirAll(target, os.FileMode(info.Mode)&os.ModePerm|0o700); err != nil {
			declineTransfer(s, wire.ErrCodeWriteFailed, "mkdir failed")
			return fmt.Errorf("%w: mkdir %s: %v", fserrors.ErrWriteFailed, target, err)
		}
		decision := wire.FileAcceptDecision{Index: info.Index, Action: wire.ActionAcceptFull}
		return wire.WriteControl(s.Control, wire.TypeFileAccept, &decision)
	}

	// Handle symlinks: just Symlink and ACK; no data follows.
	if info.IsSymlink {
		// Reject symlinks whose resolved target escapes TargetDir.
		// Without this, a malicious sender can emit a symlink "evil →
		// /elsewhere" followed by a regular file "evil/foo.txt" — the
		// second write resolves through the symlink and lands at
		// /elsewhere/foo.txt, completely outside TargetDir. The lexical
		// pathIsUnder check on `target` above sees only the path string,
		// not the filesystem symlink.
		if symlinkEscapes(opts.TargetDir, info.RelativePath, info.SymlinkTarget) {
			_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
				Code: wire.ErrCodeProtocolError, Message: "symlink target escapes target dir",
			})
			return fserrors.ErrPathTraversal
		}
		// Parent must exist.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			declineTransfer(s, wire.ErrCodeWriteFailed, "mkdir failed")
			return fmt.Errorf("%w: mkdir parent: %v", fserrors.ErrWriteFailed, err)
		}
		// Symlink fails if target exists; removing it needs the same
		// overwrite consent as a regular file — silently deleting here
		// previously lost receiver data (and on Windows the replacement
		// symlink may then fail to be created at all).
		if st, err := os.Lstat(target); err == nil {
			confirmed := opts.Overwrite || (opts.ConfirmOverwrite != nil &&
				opts.ConfirmOverwrite(info.RelativePath, st.Size(), 0))
			if !confirmed {
				declineTransfer(s, wire.ErrCodeTargetExists, "target exists")
				return fserrors.ErrTargetExists
			}
			_ = os.Remove(target)
		}
		// Best-effort: if the platform doesn't support symlinks, skip.
		if err := os.Symlink(info.SymlinkTarget, target); err != nil {
			if runtime.GOOS == "windows" {
				// Common on Windows non-admin; not fatal.
			} else {
				return fmt.Errorf("%w: symlink: %v", fserrors.ErrWriteFailed, err)
			}
		}
		decision := wire.FileAcceptDecision{Index: info.Index, Action: wire.ActionAcceptFull}
		return wire.WriteControl(s.Control, wire.TypeFileAccept, &decision)
	}

	// Refuse to clobber an existing target unless the user opted in with
	// --overwrite (or confirms interactively). We check the real target,
	// not the .partial sidecar, so a previous interrupted run can resume.
	if !archiveMode && !opts.Overwrite {
		if st, err := os.Stat(target); err == nil && !st.IsDir() {
			confirmed := opts.ConfirmOverwrite != nil &&
				opts.ConfirmOverwrite(info.RelativePath, st.Size(), info.Size)
			if !confirmed {
				declineTransfer(s, wire.ErrCodeTargetExists, "target exists")
				return fserrors.ErrTargetExists
			}
		}
	}

	// Regular file: write through a `.fsend-partial` sidecar and rename
	// to the real target only after the BLAKE3 root hash passes. That
	// way an interrupted transfer leaves a self-describing artifact the
	// next attempt can resume from, and a successful target file is
	// always fully verified.
	partial := target + partialSuffix

	// Lstat the partial before OpenFile so a pre-planted symlink can't
	// redirect chunk writes outside TargetDir.
	if st, err := os.Lstat(partial); err == nil && !st.Mode().IsRegular() {
		_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
			Code: wire.ErrCodeProtocolError, Message: "partial sidecar not a regular file",
		})
		return fmt.Errorf("%w: partial %s is not a regular file", fserrors.ErrWriteFailed, partial)
	}

	// Resume detection: if a partial exists and lines up on a chunk
	// boundary below the source size, elect ActionResume so the sender
	// seeks past the bytes we already have.
	//
	// We pair the resume offer with an imohash fingerprint of our
	// partial. The sender uses it to verify the source's first
	// resumeOffset bytes match the bytes we already have. This replaces
	// the previous "read the entire prefix back through BLAKE3" step,
	// which was the single biggest source of slow-resumes on large files.
	//
	// Imohash is non-cryptographic but the threat model here is "did the
	// source change between attempts?" — a collision-finder would need to
	// craft a file whose first N bytes imohash-collide with a specific
	// target, which is meaningless without also matching every per-chunk
	// BLAKE3 (which is cryptographic). The expected case — same source,
	// same partial — collides with probability ~2⁻⁶⁴.
	action := wire.ActionAcceptFull
	resumeOffset := uint64(0)
	var partialImo [ImohashSize]byte
	if info.Resumable {
		if st, err := os.Stat(partial); err == nil && !st.IsDir() {
			existing := uint64(st.Size())
			aligned := (existing / wire.MaxChunkSize) * wire.MaxChunkSize
			switch {
			case aligned > 0 && aligned < info.Size:
				// Fingerprint just the chunk-aligned prefix we plan to
				// keep. If the partial has bytes past `aligned`, the
				// truncation below trims them — so the imohash must
				// reflect what's *kept*, not what's on disk right now.
				h, err := PrefixImohash(partial, int64(aligned))
				if err == nil {
					action = wire.ActionResume
					resumeOffset = aligned
					partialImo = h
				}
				// On imohash failure we silently fall through to a
				// full re-transfer rather than blocking the user.
			case existing >= info.Size:
				// Stale or oversized partial — discard.
				_ = os.Remove(partial)
			}
		}
	}

	// Parent must exist.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		declineTransfer(s, wire.ErrCodeWriteFailed, "mkdir failed")
		return fmt.Errorf("%w: mkdir parent: %v", fserrors.ErrWriteFailed, err)
	}

	decision := wire.FileAcceptDecision{
		Index:          info.Index,
		Action:         action,
		ResumeOffset:   resumeOffset,
		PartialImohash: partialImo,
	}
	if err := wire.WriteControl(s.Control, wire.TypeFileAccept, &decision); err != nil {
		return fmt.Errorf("recv: file-accept: %w", err)
	}
	if action == wire.ActionResume && opts.OnResume != nil {
		opts.OnResume(info.Index, resumeOffset, info.Size)
	}

	// Open the partial sidecar (not the target). On full re-download we
	// O_TRUNC; on resume we keep the existing prefix. The sidecar is
	// created owner-private — mirroring a read-only source mode (0444)
	// would make it unreopenable and permanently break resume — and the
	// sender's mode is applied at finalize.
	flag := os.O_RDWR | os.O_CREATE
	if action == wire.ActionAcceptFull {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(partial, flag, 0o600)
	if errors.Is(err, os.ErrPermission) {
		// Pre-existing partial with a read-only mode (written by an older
		// fsend). Clear it so transfers self-heal instead of failing on
		// every attempt; a promised resume prefix is gone, so only the
		// full-download path retries immediately.
		_ = os.Remove(partial)
		if action == wire.ActionAcceptFull {
			f, err = os.OpenFile(partial, flag, 0o600)
		}
	}
	if err != nil {
		// FILE_ACCEPT is already out, so the sender is streaming chunks:
		// abort the data stream and say why (same shape as a mid-loop
		// write failure), or the sender misreads this as a network drop.
		_ = s.Data.Close()
		declineTransfer(s, wire.ErrCodeWriteFailed, "open failed: "+err.Error())
		return fmt.Errorf("%w: open partial: %v", fserrors.ErrWriteFailed, err)
	}
	defer func() { _ = f.Close() }()

	// Drop any bytes past the chunk boundary so what's on disk lines up
	// with what the sender will start writing from resumeOffset.
	if action == wire.ActionResume {
		if err := f.Truncate(int64(resumeOffset)); err != nil {
			_ = s.Data.Close()
			declineTransfer(s, wire.ErrCodeWriteFailed, "truncate failed: "+err.Error())
			return fmt.Errorf("%w: truncate partial: %v", fserrors.ErrWriteFailed, err)
		}
		if _, err := f.Seek(int64(resumeOffset), io.SeekStart); err != nil {
			_ = s.Data.Close()
			declineTransfer(s, wire.ErrCodeWriteFailed, "seek failed: "+err.Error())
			return fmt.Errorf("%w: seek: %v", fserrors.ErrWriteFailed, err)
		}
	}

	// Accumulate a BLAKE3 root over the new bytes. On resume we
	// pre-hash the on-disk prefix into the same verifier so the final
	// root check covers the assembled file end-to-end. The imohash
	// check on the FILE_ACCEPT already gives the sender a fast way to
	// abort on a clearly-changed source; this is the cryptographic
	// safety net behind it.
	resumed := resumeOffset > 0
	verifier := blake3.New()
	if resumed {
		if err := hashPrefixInto(verifier, f, int64(resumeOffset)); err != nil {
			return fmt.Errorf("%w: verify prefix: %v", fserrors.ErrReadFailed, err)
		}
		if _, err := f.Seek(int64(resumeOffset), io.SeekStart); err != nil {
			return fmt.Errorf("%w: seek: %v", fserrors.ErrWriteFailed, err)
		}
	}

	bytesWritten := resumeOffset
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
		// Peek the control stream non-blockingly: the sender may have
		// posted an ErrorFrame (e.g. partial-imohash mismatch) before
		// the first chunk lands. We don't actually peek — we just check
		// once via a short data-stream read. If the data stream is
		// closed by EOF and control has an ERROR queued, surface the
		// real reason instead of "stream closed mid-file".
		c, err := wire.ReadChunk(s.Data)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if reason := tryReadPeerError(s.Control); reason != nil {
					// Discard the partial whenever the peer reports a
					// content-correctness failure — either the source
					// changed (ErrPartialMismatch) or the bytes we
					// already received don't match the source's hash
					// (ErrHashMismatch). In both cases the on-disk
					// prefix is unreconcilable; resuming from it would
					// just reproduce the same failure.
					if errors.Is(reason, fserrors.ErrPartialMismatch) ||
						errors.Is(reason, fserrors.ErrHashMismatch) {
						_ = f.Close()
						_ = os.Remove(partial)
					}
					return reason
				}
				return fmt.Errorf("recv: stream closed mid-file: %w", fserrors.ErrConnectFailed)
			}
			return fmt.Errorf("recv: chunk: %w", err)
		}
		if c.FileIndex != info.Index {
			return fmt.Errorf("%w: chunk file index %d, expected %d", fserrors.ErrProtocolError, c.FileIndex, info.Index)
		}

		plain, err := chunkPlain(c, &dec)
		if err != nil {
			return err
		}

		// Enforce the declared size: a misbehaving peer that never sets
		// FlagLastChunk could otherwise stream unbounded chunks to the
		// partial and exhaust disk (the root-hash check only runs after
		// the loop). Streaming items carry no size and are exempt.
		if !info.Streaming && bytesWritten+uint64(len(plain)) > info.Size {
			return fmt.Errorf("%w: received bytes exceed declared size %d", fserrors.ErrProtocolError, info.Size)
		}

		if len(plain) > 0 {
			if _, err := f.Write(plain); err != nil {
				// Abort the data stream first (Close cancels the QUIC
				// read side) so the sender's chunk writes fail fast
				// instead of blocking on flow control, then say why —
				// otherwise the sender misreads our exit as a network
				// drop and burns retries telling the user to check
				// their connection.
				_ = s.Data.Close()
				declineTransfer(s, wire.ErrCodeWriteFailed, "write failed: "+err.Error())
				return classifyWriteErr("write", err)
			}
			// blake3.Hasher.Write never returns an error — it just
			// appends bytes to an internal buffer. The errcheck nag is
			// satisfied by ignoring explicitly.
			_, _ = verifier.Write(plain)
			bytesWritten += uint64(len(plain))
			if opts.ProgressFn != nil {
				opts.ProgressFn(info.Index, bytesWritten)
			}
		}

		if c.Flags&wire.FlagLastChunk != 0 {
			break
		}
	}

	// Final root hash check. Skipped only for synthetic items
	// (stdin / --text): the sender can't pre-compute a digest over a
	// stream of unknown size, so Blake3Root is left zero and Resumable
	// is false. Per-chunk hashes already cover integrity there.
	var got [32]byte
	copy(got[:], verifier.Sum(nil))
	var zero [32]byte
	syntheticSkip := !info.Resumable && info.Blake3Root == zero
	if !syntheticSkip && got != info.Blake3Root {
		// Discard the partial — its contents are corrupt or the source
		// changed mid-flight. Next attempt starts fresh.
		_ = f.Close()
		_ = os.Remove(partial)
		// Notify peer so its TRANSFER_ACK read can fail fast instead of
		// blocking on QUIC idle timeout.
		declineTransfer(s, wire.ErrCodeFileHashMismatch, "root hash mismatch")
		return fserrors.ErrHashMismatch
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: close partial: %v", fserrors.ErrWriteFailed, err)
	}

	if archiveMode {
		// The partial *is* the tar. Extract it into TargetDir, then
		// remove the partial so a re-run starts clean. Extraction is
		// the last point at which the transfer can fail visibly; if it
		// does, we leave the .partial in place so the user (or a
		// retry) can re-extract without re-downloading.
		//
		// Pass Overwrite through so a directory transfer respects the
		// same flag as a single-file transfer: without --overwrite, the
		// pre-scan refuses if any file inside would clobber something
		// on disk. The partial stays put so the user can re-run with
		// --overwrite to finish without re-downloading.
		if err := ExtractArchive(partial, opts.TargetDir, opts.Overwrite); err != nil {
			if errors.Is(err, fserrors.ErrTargetExists) {
				declineTransfer(s, wire.ErrCodeTargetExists, "target exists")
				return err
			}
			declineTransfer(s, wire.ErrCodeWriteFailed, "extract failed")
			return fmt.Errorf("%w: extract archive: %v", fserrors.ErrWriteFailed, err)
		}
		_ = os.Remove(partial)
		return nil
	}

	// Promote partial → target. os.Rename replaces atomically on POSIX
	// and (since Go 1.5) on Windows too. The previous Remove+Rename pair
	// left a TOCTOU window where a racing writer could resurrect target
	// between the two calls and the verified bytes would be lost.
	if err := os.Rename(partial, target); err != nil {
		return fmt.Errorf("%w: finalize: %v", fserrors.ErrWriteFailed, err)
	}

	// Apply the sender's mode now that the file is complete (the sidecar
	// was created owner-private; see the OpenFile above).
	_ = os.Chmod(target, os.FileMode(info.Mode)&os.ModePerm)

	// Apply modtime.
	if info.ModTime > 0 {
		t := time.Unix(0, info.ModTime)
		_ = os.Chtimes(target, t, t)
	}
	if opts.OnFileDone != nil {
		opts.OnFileDone(target)
	}
	return nil
}

// chunkPlain decompresses (when flagged) and hash-verifies one chunk's
// payload. dec is created lazily on the first compressed chunk and
// reused; the caller owns its Close.
func chunkPlain(c *wire.Chunk, dec **zstd.Decoder) ([]byte, error) {
	plain := c.Payload
	if c.Flags&wire.FlagCompressed != 0 {
		if *dec == nil {
			// Cap per-decode memory so an authenticated-but-misbehaving
			// peer can't RAM-bomb us with a high-ratio chunk. A single
			// frame can hold at most MaxChunkSize of plaintext (the
			// sender's invariant); 2× gives headroom for zstd's own
			// scratch buffers without admitting GB-scale balloons that
			// the klauspost default (1 GiB) allows.
			d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(2*wire.MaxChunkSize))
			if err != nil {
				return nil, fmt.Errorf("recv: zstd reader: %w", err)
			}
			*dec = d
		}
		var err error
		plain, err = (*dec).DecodeAll(c.Payload, nil)
		if err != nil {
			return nil, fmt.Errorf("recv: zstd decode: %w", err)
		}
		// Belt-and-braces against a sender that crafts a frame the
		// decoder accepts but whose plaintext exceeds the wire bound.
		if len(plain) > wire.MaxChunkSize {
			return nil, fmt.Errorf("%w: decompressed chunk %d > limit %d", fserrors.ErrProtocolError, len(plain), wire.MaxChunkSize)
		}
	}
	// BLAKE3 of uncompressed payload must match the per-chunk hash.
	if blakeHash32(plain) != c.Blake3Hash {
		return nil, fserrors.ErrHashMismatch
	}
	return plain, nil
}

// recvPayloadToSink streams one file's bytes to opts.Sink. No partial,
// no resume, no rename: per-chunk hashes verify each piece before it is
// emitted, and the root hash is still checked at the end — but by then
// the bytes are already out, so a mismatch can only surface as an error
// exit, never as withheld output.
func recvPayloadToSink(ctx context.Context, s *Streams, info *wire.FileInfo, opts RecvOptions) error {
	if info.IsDir || info.IsSymlink {
		return fmt.Errorf("%w: non-file entry in a sink transfer", fserrors.ErrProtocolError)
	}
	decision := wire.FileAcceptDecision{Index: info.Index, Action: wire.ActionAcceptFull}
	if err := wire.WriteControl(s.Control, wire.TypeFileAccept, &decision); err != nil {
		return fmt.Errorf("recv: file-accept: %w", err)
	}

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
		if c.FileIndex != info.Index {
			return fmt.Errorf("%w: chunk file index %d, expected %d", fserrors.ErrProtocolError, c.FileIndex, info.Index)
		}
		plain, err := chunkPlain(c, &dec)
		if err != nil {
			return err
		}
		if !info.Streaming && written+uint64(len(plain)) > info.Size {
			return fmt.Errorf("%w: received bytes exceed declared size %d", fserrors.ErrProtocolError, info.Size)
		}
		if len(plain) > 0 {
			if _, err := opts.Sink.Write(plain); err != nil {
				// Same shape as the file path: abort data, then explain.
				_ = s.Data.Close()
				declineTransfer(s, wire.ErrCodeWriteFailed, "write failed: "+err.Error())
				return classifyWriteErr("sink write", err)
			}
			_, _ = verifier.Write(plain)
			written += uint64(len(plain))
			if opts.ProgressFn != nil {
				opts.ProgressFn(info.Index, written)
			}
		}
		if c.Flags&wire.FlagLastChunk != 0 {
			break
		}
	}

	// Same root-hash policy as the file path, including the synthetic
	// (stdin / --text) skip.
	var got, zero [32]byte
	copy(got[:], verifier.Sum(nil))
	syntheticSkip := !info.Resumable && info.Blake3Root == zero
	if !syntheticSkip && got != info.Blake3Root {
		declineTransfer(s, wire.ErrCodeFileHashMismatch, "root hash mismatch")
		return fserrors.ErrHashMismatch
	}
	return nil
}

// partialSuffix is appended to the destination filename while a transfer
// is in flight. Renamed away atomically once the BLAKE3 root hash passes.
const partialSuffix = ".fsend-partial"

// receiverPasswordHandshake mirrors senderPasswordHandshake: read the
// challenge, derive the password (from --pass / FSEND_PASS or the
// interactive prompt), send the HMAC response, and wait for the sender's
// verdict.
//
// One attempt per session — a wrong password drops the connection. The
// receiver must rerun fsend to try again, which forces a fresh code path
// and a fresh nonce.
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
			// --quiet with no --pass / FSEND_PASS: nothing to answer the
			// challenge with. Decline explicitly or the sender misreads
			// the teardown as a network drop and burns retries on it.
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

	// Sender now writes either PASSWORD_VERIFIED (proceed) or
	// ERROR{ErrCodeWrongPassword} (abort). Any decoded-payload error here
	// also means abort — we don't try to "recover."
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

// declineTransfer posts an ERROR frame and runs the symmetric shutdown:
// close our write side so the frame's FIN reaches the sender before the
// deferred QUIC close tears the connection down. Without the shutdown the
// sender races the connection-close error against the frame and surfaces
// a confusing "Application error 0x0" — and then retries pointlessly.
func declineTransfer(s *Streams, code wire.ErrorCode, msg string) {
	_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{Code: code, Message: msg})
	_ = s.Control.Close()
	_, _ = io.Copy(io.Discard, s.Control)
}

// tryReadPeerError reads at most one frame from the control stream and
// returns a mapped fserrors error if it's a TypeError frame, otherwise
// nil. Used when the data stream EOFs early and we want to surface the
// real reason the sender bailed (e.g. ErrCodePartialMismatch).
//
// Best-effort only: if the control stream is also closed or carries
// something other than an ErrorFrame, returns nil and lets the caller
// fall back to its generic "stream closed" message.
func tryReadPeerError(r io.Reader) error {
	var ef wire.ErrorFrame
	ft, err := wire.ReadControl(r, &ef)
	if err != nil {
		return nil
	}
	if ft != wire.TypeError {
		return nil
	}
	return mapPeerError(ef)
}

// mapPeerError translates a peer-reported ERROR frame into the
// user-visible sentinel the CLI surfaces. Single source of truth for
// both directions; all of these are terminal for the retry layer.
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
	case wire.ErrCodeWriteFailed:
		return fmt.Errorf("%w: receiver: %s", fserrors.ErrWriteFailed, ef.Message)
	case wire.ErrCodeCancelled:
		return fserrors.ErrPeerCancelled
	default:
		return fmt.Errorf("%w: peer reported %d: %s", fserrors.ErrProtocolError, ef.Code, ef.Message)
	}
}
