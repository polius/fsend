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
	TargetDir     string  // where files are written
	Overwrite     bool
	Accept        func(hello wire.SenderHello) bool      // prompt callback; nil → auto-accept (legacy --yes)
	PromptPasswd  func() (string, error)                  // prompt callback for --pass; nil → no password support
	SessionKey    []byte                                   // 32-byte TLS exporter-derived key
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
		return fserrors.ErrReceiverDeclined
	}

	if hello.HasPassword {
		if err := recvPasswordChallenge(s, opts); err != nil {
			return err
		}
	}

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
		if err := recvOneFile(ctx, s, &info, opts); err != nil {
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

func recvPasswordChallenge(s *Streams, opts RecvOptions) error {
	var got [32]byte
	ft, err := wire.ReadControl(s.Control, &got)
	if err != nil {
		return fmt.Errorf("recv: password-challenge: %w", err)
	}
	if ft != wire.TypePasswordChallenge {
		return fmt.Errorf("%w: expected PASSWORD_CHALLENGE, got %v", fserrors.ErrProtocolError, ft)
	}

	if opts.PromptPasswd == nil {
		return fserrors.ErrWrongPassword
	}
	pwd, err := opts.PromptPasswd()
	if err != nil {
		return err
	}
	mine := computePasswordHMAC(pwd, opts.SessionKey)
	if err := wire.WriteControl(s.Control, wire.TypePasswordResponse, &mine); err != nil {
		return fmt.Errorf("recv: password-response: %w", err)
	}
	if !constantTimeEqual32(got, mine) {
		// Sender will see the mismatch and abort; we still need to consume
		// any in-flight ERROR or just bail.
		return fserrors.ErrWrongPassword
	}
	return nil
}

func recvOneFile(ctx context.Context, s *Streams, info *wire.FileInfo, opts RecvOptions) error {
	// Sanitize the relative path before composing the target.
	relClean, err := SanitizeRelativePath(info.RelativePath)
	if err != nil {
		// Inform peer + abort.
		_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
			Code: wire.ErrCodeProtocolError, Message: "bad path",
		})
		return fmt.Errorf("%w: %v", fserrors.ErrPathTraversal, err)
	}
	target := filepath.Join(opts.TargetDir, relClean)

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

	// Regular file: decide action.
	action := wire.ActionAcceptFull
	resumeOffset := uint64(0)
	if existing, err := os.Stat(target); err == nil && !existing.IsDir() {
		if !opts.Overwrite {
			// In v1 single-receiver flow we accept-full anyway (sender
			// won't have generated this filename if there was a prior
			// successful receive — the partial sidecar handles in-flight
			// resume). A future version can prompt here.
			// For now: if a non-resume partial exists, just accept-full
			// and overwrite the contents.
		}
		_ = existing
	}

	// Parent must exist.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("%w: mkdir parent: %v", fserrors.ErrWriteFailed, err)
	}

	decision := wire.FileAcceptDecision{
		Index:        info.Index,
		Action:       action,
		ResumeOffset: resumeOffset,
	}
	if err := wire.WriteControl(s.Control, wire.TypeFileAccept, &decision); err != nil {
		return fmt.Errorf("recv: file-accept: %w", err)
	}

	// Open target for write.
	flag := os.O_RDWR | os.O_CREATE
	if action == wire.ActionAcceptFull {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(target, flag, os.FileMode(info.Mode))
	if err != nil {
		return fmt.Errorf("%w: open target: %v", fserrors.ErrWriteFailed, err)
	}
	defer f.Close()

	if resumeOffset > 0 {
		if _, err := f.Seek(int64(resumeOffset), io.SeekStart); err != nil {
			return fmt.Errorf("%w: seek: %v", fserrors.ErrWriteFailed, err)
		}
	}

	// Receive chunks.
	verifier := blake3.New()
	// If resuming, fast-forward the verifier over already-written bytes.
	if resumeOffset > 0 {
		// Re-read what's on disk into the verifier so the final root check works.
		_, _ = f.Seek(0, io.SeekStart)
		if _, err := io.CopyN(verifier, f, int64(resumeOffset)); err != nil {
			return fmt.Errorf("%w: re-hash existing: %v", fserrors.ErrReadFailed, err)
		}
		// Re-seek for writes.
		if _, err := f.Seek(int64(resumeOffset), io.SeekStart); err != nil {
			return fmt.Errorf("%w: re-seek: %v", fserrors.ErrWriteFailed, err)
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
		c, err := wire.ReadChunk(s.Data)
		if err != nil {
			if errors.Is(err, io.EOF) {
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
			verifier.Write(plain)
			bytesWritten += uint64(len(plain))
			if opts.ProgressFn != nil {
				opts.ProgressFn(info.Index, bytesWritten)
			}
		}

		if c.Flags&wire.FlagLastChunk != 0 {
			break
		}
	}

	// Final root hash check.
	var got [32]byte
	copy(got[:], verifier.Sum(nil))
	if got != info.Blake3Root {
		// Delete corrupted file.
		_ = os.Remove(target)
		return fserrors.ErrHashMismatch
	}

	// Apply modtime.
	if info.ModTime > 0 {
		t := time.Unix(0, info.ModTime)
		_ = os.Chtimes(target, t, t)
	}
	return nil
}
