package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	Accept        func(hello wire.SenderHello) bool           // prompt callback; nil → auto-accept (legacy --yes)
	Password      string                                      // pre-supplied (--pass or FSEND_PASS); used when sender requires a password
	PromptPass    func() (string, error)                      // fallback when sender requires a password and Password is empty
	ProgressFn    func(fileIndex uint32, bytesWritten uint64) // optional
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

	// File loop.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var info wire.FileInfo
		ft, err := wire.ReadControl(s.Control, &info)
		if err != nil {
			return fmt.Errorf("recv: file-info: %w", err)
		}
		if ft == wire.TypeTransferComplete {
			break
		}
		if ft == wire.TypeAbort {
			return fserrors.ErrConnectFailed
		}
		if ft == wire.TypeError {
			// Re-read with payload — we already consumed the header.
			// Simplification: treat any TypeError as protocol abort.
			return fmt.Errorf("%w: peer sent ERROR", fserrors.ErrProtocolError)
		}
		if ft != wire.TypeFileInfo {
			return fmt.Errorf("%w: expected FILE_INFO, got %v", fserrors.ErrProtocolError, ft)
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
		if err := os.MkdirAll(target, os.FileMode(info.Mode)); err != nil {
			return fmt.Errorf("%w: mkdir %s: %v", fserrors.ErrWriteFailed, target, err)
		}
		decision := wire.FileAcceptDecision{Index: info.Index, Action: wire.ActionAcceptFull}
		return wire.WriteControl(s.Control, wire.TypeFileAccept, &decision)
	}

	// Handle symlinks: just Symlink and ACK; no data follows.
	if info.IsSymlink {
		// Parent must exist.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("%w: mkdir parent: %v", fserrors.ErrWriteFailed, err)
		}
		// Best-effort: if the platform doesn't support symlinks, log and skip.
		_ = os.Remove(target) // Symlink fails if target exists
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
	// --overwrite. We check the real target (not the .partial sidecar) so
	// a previous interrupted run can still be resumed.
	if !archiveMode && !opts.Overwrite {
		if st, err := os.Stat(target); err == nil && !st.IsDir() {
			_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
				Code: wire.ErrCodeTargetExists, Message: "target exists",
			})
			return fserrors.ErrTargetExists
		}
	}

	// Regular file: write through a `.fsend-partial` sidecar and rename
	// to the real target only after the BLAKE3 root hash passes. That
	// way an interrupted transfer leaves a self-describing artifact the
	// next attempt can resume from, and a successful target file is
	// always fully verified.
	partial := target + partialSuffix

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

	// Open the partial sidecar (not the target). On full re-download we
	// O_TRUNC; on resume we keep the existing prefix.
	flag := os.O_RDWR | os.O_CREATE
	if action == wire.ActionAcceptFull {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(partial, flag, os.FileMode(info.Mode))
	if err != nil {
		return fmt.Errorf("%w: open partial: %v", fserrors.ErrWriteFailed, err)
	}
	defer f.Close()

	// Drop any bytes past the chunk boundary so what's on disk lines up
	// with what the sender will start writing from resumeOffset.
	if action == wire.ActionResume {
		if err := f.Truncate(int64(resumeOffset)); err != nil {
			return fmt.Errorf("%w: truncate partial: %v", fserrors.ErrWriteFailed, err)
		}
		if _, err := f.Seek(int64(resumeOffset), io.SeekStart); err != nil {
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
	var verifier *blake3.Hasher
	verifier = blake3.New()
	if resumed {
		if err := hashPrefixInto(verifier, f, int64(resumeOffset)); err != nil {
			return fmt.Errorf("%w: verify prefix: %v", fserrors.ErrReadFailed, err)
		}
		if _, err := f.Seek(int64(resumeOffset), io.SeekStart); err != nil {
			return fmt.Errorf("%w: seek: %v", fserrors.ErrWriteFailed, err)
		}
	}

	var bytesWritten uint64 = resumeOffset
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
					return reason
				}
				return fmt.Errorf("recv: stream closed mid-file: %w", fserrors.ErrConnectFailed)
			}
			return fmt.Errorf("recv: chunk: %w", err)
		}
		if c.FileIndex != info.Index {
			return fmt.Errorf("%w: chunk file index %d, expected %d", fserrors.ErrProtocolError, c.FileIndex, info.Index)
		}

		plain := c.Payload
		if c.Flags&wire.FlagCompressed != 0 {
			if dec == nil {
				dec, err = zstd.NewReader(nil)
				if err != nil {
					return fmt.Errorf("recv: zstd reader: %w", err)
				}
			}
			plain, err = dec.DecodeAll(c.Payload, nil)
			if err != nil {
				return fmt.Errorf("recv: zstd decode: %w", err)
			}
		}

		// BLAKE3 of uncompressed payload must match the per-chunk hash.
		if hashEq := blakeHash32(plain); hashEq != c.Blake3Hash {
			return fserrors.ErrHashMismatch
		}

		if len(plain) > 0 {
			if _, err := f.Write(plain); err != nil {
				return fmt.Errorf("%w: write: %v", fserrors.ErrWriteFailed, err)
			}
			if verifier != nil {
				verifier.Write(plain)
			}
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
	if verifier != nil {
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
			_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
				Code: wire.ErrCodeFileHashMismatch, Message: "root hash mismatch",
			})
			return fserrors.ErrHashMismatch
		}
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
		if err := ExtractArchive(partial, opts.TargetDir); err != nil {
			return fmt.Errorf("%w: extract archive: %v", fserrors.ErrWriteFailed, err)
		}
		_ = os.Remove(partial)
		return nil
	}

	// Promote partial → target. Close first so Windows doesn't refuse
	// the rename, and remove any existing target so POSIX and Windows
	// behave identically.
	_ = os.Remove(target)
	if err := os.Rename(partial, target); err != nil {
		return fmt.Errorf("%w: finalize: %v", fserrors.ErrWriteFailed, err)
	}

	// Apply modtime.
	if info.ModTime > 0 {
		t := time.Unix(0, info.ModTime)
		_ = os.Chtimes(target, t, t)
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
			return fserrors.ErrWrongPassword
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
	switch ef.Code {
	case wire.ErrCodePartialMismatch:
		return fserrors.ErrPartialMismatch
	case wire.ErrCodeFileHashMismatch, wire.ErrCodeChunkHashMismatch:
		return fserrors.ErrHashMismatch
	default:
		return fmt.Errorf("%w: peer reported %d: %s", fserrors.ErrProtocolError, ef.Code, ef.Message)
	}
}
