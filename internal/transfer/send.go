package transfer

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// SendOptions configures one send invocation.
type SendOptions struct {
	Items         []SourceItem
	Hostname      string
	OS            string
	ClientVersion string
	TransferKind  wire.TransferKind
	// TotalFiles overrides the wire-level HELLO.TotalFiles. When zero,
	// len(Items) is used. Archive transfers set this to the number of
	// files packed into the tar so the receiver's prompt block shows
	// the real file count instead of "1" (the tar wrapper).
	TotalFiles uint32
	// DisplayName is a peer-facing label rendered in the receiver's
	// accept block: "report.pdf" for single-file, "myproject/" for a
	// directory, "3 items" for multi-file. Empty for stdin/text — the
	// receiver falls back to a kind-specific phrase.
	DisplayName string
	Password    string                                   // empty → no password challenge
	ProgressFn  func(fileIndex uint32, bytesSent uint64) // called periodically; may be nil

	// OnStreamingEOF fires exactly once per streaming item, immediately
	// after the EOF chunk has been written. The CLI uses this hook to
	// latch the progress bar's total to the real byte count (which is
	// only knowable at EOF for unknown-length streams). Nil-safe.
	OnStreamingEOF func(fileIndex uint32, finalBytes uint64)
}

// Send executes the full sender-side protocol over the supplied streams:
//  1. Write HELLO
//  2. Read HELLO_ACK (abort if receiver declined)
//  3. For each file: write FILE_INFO, read FILE_ACCEPT, stream chunks if accepted
//  4. Write TRANSFER_COMPLETE, read TRANSFER_ACK
func Send(ctx context.Context, s *Streams, opts SendOptions) error {
	totalFiles := opts.TotalFiles
	if totalFiles == 0 {
		totalFiles = uint32(len(opts.Items))
	}
	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		Hostname:        opts.Hostname,
		OS:              opts.OS,
		ClientVersion:   opts.ClientVersion,
		TransferKind:    opts.TransferKind,
		TotalFiles:      totalFiles,
		HasPassword:     opts.Password != "",
		CompressionHint: 1,
		DisplayName:     opts.DisplayName,
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
		if err := senderPasswordHandshake(s, opts.Password); err != nil {
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

func sendOneFile(ctx context.Context, s *Streams, it *SourceItem, opts SendOptions) error {
	if err := wire.WriteControl(s.Control, wire.TypeFileInfo, &it.Info); err != nil {
		return fmt.Errorf("send: file-info %d: %w", it.Info.Index, err)
	}

	ft, body, err := wire.ReadControlRaw(s.Control)
	if err != nil {
		return fmt.Errorf("send: file-accept: %w", err)
	}
	if ft == wire.TypeError {
		// Receiver declined for a specific reason. Translate the wire
		// code into a user-visible sentinel so the CLI surfaces it
		// (rather than a generic "protocol error").
		var ef wire.ErrorFrame
		_ = wire.Decode(body, &ef)
		switch ef.Code {
		case wire.ErrCodeTargetExists:
			return fserrors.ErrTargetExists
		default:
			return fmt.Errorf("%w: peer reported %d: %s", fserrors.ErrProtocolError, ef.Code, ef.Message)
		}
	}
	if ft != wire.TypeFileAccept {
		return fmt.Errorf("%w: expected FILE_ACCEPT, got %v", fserrors.ErrProtocolError, ft)
	}
	var decision wire.FileAcceptDecision
	if err := wire.Decode(body, &decision); err != nil {
		return fmt.Errorf("send: decode file-accept: %w", err)
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

	// On resume, verify the receiver's partial matches the bytes we're
	// about to skip. Imohash collisions of well-formed (non-adversarial)
	// inputs are ~2⁻⁶⁴ — if this check fails, the source has almost
	// certainly changed since the receiver wrote its partial. Aborting
	// with a clear error is better than silently producing a frankenfile.
	if decision.Action == wire.ActionResume && it.AbsPath != "" && decision.ResumeOffset > 0 {
		got, err := PrefixImohash(it.AbsPath, int64(decision.ResumeOffset))
		if err != nil {
			return fmt.Errorf("%w: imohash source prefix: %v", fserrors.ErrReadFailed, err)
		}
		if got != decision.PartialImohash {
			// Close data first so the receiver's chunk-read EOFs and
			// falls through to its peek-control path. Without this, an
			// unbuffered transport (e.g. the in-memory pipe used in
			// tests) deadlocks: the sender blocks writing the error
			// frame on control while the receiver blocks reading from
			// data. QUIC's per-stream buffers paper over the order in
			// production, but the protocol contract is cleaner with the
			// EOF signal sent explicitly.
			_ = s.Data.Close()
			_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
				Code:    wire.ErrCodePartialMismatch,
				Message: "receiver's partial does not match source",
			})
			return fserrors.ErrPartialMismatch
		}
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
	defer func() { _ = closeFn() }()

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

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return fmt.Errorf("send: zstd writer: %w", err)
	}
	defer func() { _ = enc.Close() }()

	// Streaming items (e.g. piped stdin): size is not known up front.
	// Read until EOF, mark the EOF chunk with FlagLastChunk. Resume is
	// disabled and Blake3Root is zero — verification is per-chunk only,
	// which matches the prior buffered-stdin behavior.
	if it.Info.Streaming {
		return sendStreamingChunks(ctx, s, &it.Info, src, buf, chunkIndex, enc, opts)
	}

	totalSize := it.Info.Size

	// Edge case: empty file — emit one zero-length FlagLastChunk frame so
	// the receiver still observes a "done" marker for this file.
	if totalSize == 0 {
		return writeChunk(s, &it.Info, chunkIndex, nil, true, enc)
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
		if err := writeChunk(s, &it.Info, chunkIndex, buf[:n], isLast, enc); err != nil {
			return err
		}
		chunkIndex++
		if opts.ProgressFn != nil {
			opts.ProgressFn(it.Info.Index, bytesSentInFile)
		}
	}

	return nil
}

// sendStreamingChunks drains src until EOF, emitting chunks of up to
// MaxChunkSize each. The final (possibly empty) chunk carries
// FlagLastChunk so the receiver knows the file is done.
//
// We treat io.EOF and io.ErrUnexpectedEOF identically: both mean "this
// is the tail of the stream." A short read of exactly MaxChunkSize that
// happens to align with EOF is rare in practice but handled correctly —
// we'll emit one extra zero-length last chunk, which the receiver's
// "len(plain) > 0" guard already tolerates.
func sendStreamingChunks(
	ctx context.Context,
	s *Streams,
	info *wire.FileInfo,
	src io.Reader,
	buf []byte,
	startChunkIndex uint32,
	enc *zstd.Encoder,
	opts SendOptions,
) error {
	chunkIndex := startChunkIndex
	var bytesSent uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := io.ReadFull(src, buf)
		isLast := readErr == io.EOF || readErr == io.ErrUnexpectedEOF
		if readErr != nil && !isLast {
			return fmt.Errorf("%w: read: %v", fserrors.ErrReadFailed, readErr)
		}
		bytesSent += uint64(n)
		if err := writeChunk(s, info, chunkIndex, buf[:n], isLast, enc); err != nil {
			return err
		}
		chunkIndex++
		if opts.ProgressFn != nil && n > 0 {
			opts.ProgressFn(info.Index, bytesSent)
		}
		if isLast {
			if opts.OnStreamingEOF != nil {
				opts.OnStreamingEOF(info.Index, bytesSent)
			}
			return nil
		}
	}
}

func writeChunk(s *Streams, info *wire.FileInfo, chunkIndex uint32, plain []byte, isLast bool, enc *zstd.Encoder) error {
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
	if len(plain) > 0 {
		compressed := enc.EncodeAll(plain, nil)
		// Adopt compressed only if it saves ≥10% — applied per-chunk so a
		// single file that mixes compressible and incompressible regions
		// still benefits where it can.
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
	// blake3.Hasher.Write never returns an error.
	_, _ = h.Write(b)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// readerToSeeker wraps an io.Reader to satisfy io.ReadSeeker. Seek is only
// valid to offset 0; any other call panics — this is intentional, callers
// that need real seek must pass a real *os.File.
type readerSeeker struct{ r io.Reader }

func readerToSeeker(r io.Reader) io.ReadSeeker      { return &readerSeeker{r} }
func (rs *readerSeeker) Read(p []byte) (int, error) { return rs.r.Read(p) }
func (rs *readerSeeker) Seek(offset int64, whence int) (int64, error) {
	if offset != 0 || whence != io.SeekStart {
		return 0, errors.New("transfer: cannot seek a non-seekable reader")
	}
	return 0, nil
}

// senderPasswordHandshake runs the challenge/response exchange that gates
// the transfer when --pass is set. Wire flow:
//
//	sender → receiver  PASSWORD_CHALLENGE{nonce}
//	receiver → sender  PASSWORD_RESPONSE{HMAC-SHA256(password, nonce)}
//	sender → receiver  PASSWORD_VERIFIED (on match)
//	                or ERROR{ErrCodeWrongPassword} (on mismatch)
//
// The constant-time compare is non-negotiable: a timing leak here would
// trivially recover the password over a few thousand attempts.
func senderPasswordHandshake(s *Streams, password string) error {
	var nonce [32]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return fmt.Errorf("send: password nonce: %w", err)
	}
	if err := wire.WriteControl(s.Control, wire.TypePasswordChallenge, &wire.PasswordChallenge{Nonce: nonce}); err != nil {
		return fmt.Errorf("send: password challenge: %w", err)
	}
	var resp wire.PasswordResponse
	ft, err := wire.ReadControl(s.Control, &resp)
	if err != nil {
		return fmt.Errorf("send: read password response: %w", err)
	}
	if ft != wire.TypePasswordResponse {
		return fmt.Errorf("%w: expected PASSWORD_RESPONSE, got %v", fserrors.ErrProtocolError, ft)
	}
	expected := hmacPassword(password, nonce[:])
	if subtle.ConstantTimeCompare(expected, resp.HMAC[:]) != 1 {
		_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
			Code:    wire.ErrCodeWrongPassword,
			Message: "wrong password",
		})
		// Symmetric shutdown: close our write side so the ERROR frame's
		// FIN reaches the receiver before the deferred Conn.Close tears
		// the QUIC connection down. Without this, the receiver races
		// the connection-close error against the frame and surfaces a
		// confusing "Application error 0x0 (remote)" instead of the
		// real "wrong password."
		_ = s.Control.Close()
		_, _ = io.Copy(io.Discard, s.Control)
		return fserrors.ErrWrongPassword
	}
	if err := wire.WriteControl(s.Control, wire.TypePasswordVerified, nil); err != nil {
		return fmt.Errorf("send: password verified: %w", err)
	}
	return nil
}

// hmacPassword is the shared HMAC-SHA256(password, nonce) computation
// used by both sides. Exposed at package scope so recv.go can reuse it
// without duplicating the construction.
func hmacPassword(password string, nonce []byte) []byte {
	m := hmac.New(sha256.New, []byte(password))
	m.Write(nonce)
	return m.Sum(nil)
}
