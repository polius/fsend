package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// SendOptions configures one send invocation.
type SendOptions struct {
	Items          []SourceItem
	Hostname       string
	OS             string
	ClientVersion  string
	TransferKind   wire.TransferKind
	Password       string // empty = no password challenge
	SessionKey     []byte // 32-byte TLS exporter-derived key, for HMAC over Password
	Compress       bool   // if true, attempt zstd; per-chunk auto-decision
	ProgressFn     func(fileIndex uint32, bytesSent uint64) // called periodically; may be nil
}

// Send executes the full sender-side protocol over the supplied streams.
//
// Order of operations matches docs/decisions/wire-protocol.md:
//  1. Write HELLO
//  2. Read HELLO_ACK (abort if receiver declined)
//  3. (if HasPassword) write PASSWORD_CHALLENGE, read PASSWORD_RESPONSE, verify
//  4. For each file: write FILE_INFO, read FILE_ACCEPT, stream chunks if accepted
//  5. Write TRANSFER_COMPLETE, read TRANSFER_ACK
func Send(ctx context.Context, s *Streams, opts SendOptions) error {
	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		Hostname:        opts.Hostname,
		OS:              opts.OS,
		ClientVersion:   opts.ClientVersion,
		TransferKind:    opts.TransferKind,
		TotalFiles:      uint32(len(opts.Items)),
		HasPassword:     opts.Password != "",
		CompressionHint: 0,
	}
	if opts.Compress {
		hello.CompressionHint = 1
	}
	for _, it := range opts.Items {
		hello.TotalBytes += it.Info.Size
	}

	if err := wire.WriteControl(s.Control, wire.TypeHello, hello); err != nil {
		return fmt.Errorf("send: hello: %w", err)
	}

	var ack wire.ReceiverHello
	ft, err := wire.ReadControl(s.Control, &ack)
	if err != nil {
		return fmt.Errorf("send: read hello-ack: %w", err)
	}
	if ft != wire.TypeHelloAck {
		return fmt.Errorf("%w: expected HELLO_ACK, got %v", fserrors.ErrProtocolError, ft)
	}
	if !ack.Accepts {
		return fserrors.ErrReceiverDeclined
	}

	if opts.Password != "" {
		if err := sendPasswordChallenge(s, opts.Password, opts.SessionKey); err != nil {
			return err
		}
	}

	for _, it := range opts.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sendOneFile(ctx, s, &it, opts); err != nil {
			return err
		}
	}

	if err := wire.WriteControl(s.Control, wire.TypeTransferComplete, nil); err != nil {
		return fmt.Errorf("send: complete: %w", err)
	}
	ft, err = wire.ReadControl(s.Control, nil)
	if err != nil {
		return fmt.Errorf("send: read complete-ack: %w", err)
	}
	if ft != wire.TypeTransferAck {
		return fmt.Errorf("%w: expected TRANSFER_ACK, got %v", fserrors.ErrProtocolError, ft)
	}
	// Signal graceful shutdown: close our send side of Control. The receiver
	// is blocked reading-until-EOF on Control as its final step, so seeing
	// our FIN unblocks it and lets it return cleanly. This ordering
	// guarantees both sides finish before any transport close runs.
	_ = s.Control.Close()
	return nil
}

func sendPasswordChallenge(s *Streams, password string, sessionKey []byte) error {
	want := computePasswordHMAC(password, sessionKey)
	if err := wire.WriteControl(s.Control, wire.TypePasswordChallenge, want); err != nil {
		return fmt.Errorf("send: password-challenge: %w", err)
	}
	var resp [32]byte
	ft, err := wire.ReadControl(s.Control, &resp)
	if err != nil {
		return fmt.Errorf("send: password-response: %w", err)
	}
	if ft != wire.TypePasswordResponse {
		return fmt.Errorf("%w: expected PASSWORD_RESPONSE, got %v", fserrors.ErrProtocolError, ft)
	}
	if !constantTimeEqual32(want, resp) {
		// Inform peer, then bubble up.
		_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
			Code: wire.ErrCodeWrongPassword, Message: "wrong password",
		})
		return fserrors.ErrWrongPassword
	}
	return nil
}

func sendOneFile(ctx context.Context, s *Streams, it *SourceItem, opts SendOptions) error {
	if err := wire.WriteControl(s.Control, wire.TypeFileInfo, &it.Info); err != nil {
		return fmt.Errorf("send: file-info %d: %w", it.Info.Index, err)
	}

	var decision wire.FileAcceptDecision
	ft, err := wire.ReadControl(s.Control, &decision)
	if err != nil {
		return fmt.Errorf("send: file-accept: %w", err)
	}
	if ft != wire.TypeFileAccept {
		return fmt.Errorf("%w: expected FILE_ACCEPT, got %v", fserrors.ErrProtocolError, ft)
	}

	switch decision.Action {
	case wire.ActionAbortAll:
		return fserrors.ErrReceiverDeclined
	case wire.ActionSkip:
		return nil
	case wire.ActionAcceptFull, wire.ActionResume:
		// proceed below
	default:
		return fmt.Errorf("%w: unknown FileAccept action %d", fserrors.ErrProtocolError, decision.Action)
	}

	// Directory or symlink: no data to stream.
	if it.Info.IsDir || it.Info.IsSymlink {
		return nil
	}

	// Open source.
	var src io.ReadSeeker
	var closeFn func() error
	if it.Reader != nil {
		// Synthetic (stdin/text) — not resumable, no seek; wrap in a fake seeker.
		// In v1 we only support stdin/text without resume, so seek would be
		// called only with offset 0.
		src = readerToSeeker(it.Reader)
		closeFn = func() error { return nil }
	} else {
		f, err := os.Open(it.AbsPath)
		if err != nil {
			return fmt.Errorf("%w: %v", fserrors.ErrReadFailed, err)
		}
		src = f
		closeFn = f.Close
	}
	defer closeFn()

	// Seek to resume offset if requested.
	if decision.Action == wire.ActionResume && decision.ResumeOffset > 0 {
		if _, err := src.Seek(int64(decision.ResumeOffset), io.SeekStart); err != nil {
			return fmt.Errorf("%w: seek: %v", fserrors.ErrReadFailed, err)
		}
	}

	startChunk := uint32(decision.ResumeOffset / wire.MaxChunkSize)
	bytesSentInFile := decision.ResumeOffset

	// Streaming loop. We know it.Info.Size up front, so we can deterministically
	// mark the last chunk rather than relying on EOF behavior of io.ReadFull.
	// (io.ReadFull on a buffer-sized read returns nil error even when EOF is
	// next, so EOF-based detection misses last chunk on aligned file sizes.)
	buf := make([]byte, wire.MaxChunkSize)
	chunkIndex := startChunk

	var enc *zstd.Encoder
	if opts.Compress {
		var err error
		enc, err = zstd.NewWriter(nil)
		if err != nil {
			return fmt.Errorf("send: zstd writer: %w", err)
		}
		defer enc.Close()
	}

	totalSize := it.Info.Size

	// Edge case: empty file — emit one zero-length FlagLastChunk frame so
	// the receiver still observes a "done" marker for this file.
	if totalSize == 0 {
		return writeChunk(s, &it.Info, chunkIndex, nil, true, enc, opts.Compress)
	}

	for bytesSentInFile < totalSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := totalSize - bytesSentInFile
		toRead := uint64(len(buf))
		if remaining < toRead {
			toRead = remaining
		}

		n, readErr := io.ReadFull(src, buf[:toRead])
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return fmt.Errorf("%w: read: %v", fserrors.ErrReadFailed, readErr)
		}
		if uint64(n) != toRead {
			return fmt.Errorf("%w: short read: expected %d, got %d", fserrors.ErrReadFailed, toRead, n)
		}

		bytesSentInFile += uint64(n)
		isLast := bytesSentInFile >= totalSize
		if err := writeChunk(s, &it.Info, chunkIndex, buf[:n], isLast, enc, opts.Compress); err != nil {
			return err
		}
		chunkIndex++
		if opts.ProgressFn != nil {
			opts.ProgressFn(it.Info.Index, bytesSentInFile)
		}
	}

	return nil
}

func writeChunk(s *Streams, info *wire.FileInfo, chunkIndex uint32, plain []byte, isLast bool, enc *zstd.Encoder, attemptCompress bool) error {
	c := &wire.Chunk{
		FileIndex:  info.Index,
		ChunkIndex: chunkIndex,
	}
	if isLast {
		c.Flags |= wire.FlagLastChunk
	}
	// BLAKE3 of *uncompressed* payload.
	c.Blake3Hash = blakeHash32(plain)

	payload := plain
	if attemptCompress && enc != nil && len(plain) > 0 {
		compressed := enc.EncodeAll(plain, nil)
		// Adopt compressed only if it saves ≥10% — matches the spec's
		// "first-chunk peek, ≥10%" rule applied per-chunk for simplicity.
		// Per-chunk costs slightly more CPU than a one-time decision but
		// handles the case where a single file mixes compressible and
		// incompressible regions.
		if len(compressed)+len(compressed)/10 < len(plain) {
			payload = compressed
			c.Flags |= wire.FlagCompressed
		}
	}
	c.Payload = payload
	return wire.WriteChunk(s.Data, c)
}

func blakeHash32(b []byte) [32]byte {
	h := blake3.New()
	h.Write(b)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// readerToSeeker wraps an io.Reader to satisfy io.ReadSeeker. Seek is only
// valid to offset 0; any other call panics — this is intentional, callers
// that need real seek must pass a real *os.File.
type readerSeeker struct{ r io.Reader }

func readerToSeeker(r io.Reader) io.ReadSeeker { return &readerSeeker{r} }
func (rs *readerSeeker) Read(p []byte) (int, error) { return rs.r.Read(p) }
func (rs *readerSeeker) Seek(offset int64, whence int) (int64, error) {
	if offset != 0 || whence != io.SeekStart {
		return 0, errors.New("transfer: cannot seek a non-seekable reader")
	}
	return 0, nil
}

// constantTimeEqual32 compares two 32-byte arrays in constant time.
func constantTimeEqual32(a, b [32]byte) bool {
	var diff byte
	for i := 0; i < 32; i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// computePasswordHMAC binds the password to the TLS exporter key.
//
// HMAC-SHA256(password, sessionKey) per docs/decisions/implementation-defaults.md.
// The result never crosses the wire in the clear — it's transmitted inside
// the TLS-encrypted control stream.
func computePasswordHMAC(password string, sessionKey []byte) [32]byte {
	// Imported lazily here to keep the build trim if the rest of the
	// package wants to compile without crypto/hmac for some reason.
	// (No real reason; just keeping the test surface small.)
	h := hmacSHA256(sessionKey, []byte(password))
	var out [32]byte
	copy(out[:], h)
	return out
}

// Ensure we don't grow OS-specific paths in the test surface.
var _ = runtime.GOOS
