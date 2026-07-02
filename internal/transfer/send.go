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
	"time"

	"github.com/zeebo/blake3"
	"golang.org/x/crypto/argon2"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// SendOptions configures one send invocation.
type SendOptions struct {
	Hostname      string
	OS            string
	ClientVersion string
	Mode          wire.TransferMode

	// ModeFiles inputs.
	Sources []Source

	// ModeStream inputs (stdin / --text).
	Stream      io.Reader
	IsText      bool
	DisplayName string

	Password string

	ProgressFn     func(index uint32, bytesSent uint64)
	OnResume       func(index uint32, offset, total uint64)
	OnSkip         func(index uint32)
	OnStreamingEOF func(index uint32, finalBytes uint64)
}

// Send executes the full sender-side protocol over the supplied streams.
func Send(ctx context.Context, s *Streams, opts SendOptions) error {
	err := send(ctx, s, opts)
	switch {
	case errors.Is(err, context.Canceled):
		notifyPeer(s, wire.ErrCodeCancelled, "sender cancelled")
	case errors.Is(err, fserrors.ErrReadFailed):
		// A local source failure (unreadable, vanished, shrunk) otherwise
		// surfaces on the receiver as a bare stream close — a "network"
		// error it would burn retries on. Post the real reason instead.
		notifyPeer(s, wire.ErrCodeReadFailed, err.Error())
	}
	return err
}

func send(ctx context.Context, s *Streams, opts SendOptions) error {
	// Bind the streams to ctx so a Ctrl-C parked in a blocked QUIC write
	// unblocks promptly instead of waiting out the idle timeout. notifyPeer
	// (called by the outer Send with the raw, unwrapped streams) is unaffected.
	s, stop := bindCtx(ctx, s)
	defer stop()

	hello := &wire.SenderHello{
		ProtocolVersion: wire.ProtocolVersion,
		Hostname:        opts.Hostname,
		OS:              opts.OS,
		ClientVersion:   opts.ClientVersion,
		HasPassword:     opts.Password != "",
		Mode:            opts.Mode,
		IsText:          opts.IsText,
		DisplayName:     opts.DisplayName,
	}
	if err := wire.WriteControl(s.Control, wire.TypeHello, hello); err != nil {
		return fmt.Errorf("send: hello: %w", err)
	}

	var ack wire.ReceiverHello
	ft, err := wire.ReadControl(s.Control, &ack)
	if err != nil {
		if errors.Is(err, wire.ErrUnsupportedVersion) {
			return fserrors.ErrIncompatibleVersion
		}
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

	if opts.Mode == wire.ModeStream {
		return sendStream(ctx, s, opts)
	}
	return sendFiles(ctx, s, opts)
}

// sendFiles runs the listing → classify → data flow.
func sendFiles(ctx context.Context, s *Streams, opts SendOptions) error {
	if err := sendListing(s.Control, opts.Sources); err != nil {
		return fmt.Errorf("send: listing: %w", err)
	}
	decisions, err := senderNegotiate(s, opts.Sources)
	if err != nil {
		return err
	}

	packer, err := newChunkPacker(s.Data)
	if err != nil {
		return err
	}
	defer packer.Close()

	for i := range opts.Sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		src := &opts.Sources[i]
		d, ok := decisions[src.Entry.Index]
		if !ok || d.Action == wire.DecisionSkip {
			// Mirror the receiver (recv.go): directories aren't user-facing
			// "files", so a skipped dir mustn't inflate the unchanged count.
			if ok && opts.OnSkip != nil && src.Entry.Type != wire.EntryDir {
				opts.OnSkip(src.Entry.Index)
			}
			continue
		}
		// Structural entries and empty files carry no data; the receiver
		// materializes them from the listing.
		if src.Entry.Type != wire.EntryFile || src.Entry.Size == 0 {
			continue
		}
		if err := sendOneFile(ctx, s, packer, src, d, opts); err != nil {
			return err
		}
	}
	if err := packer.flush(); err != nil {
		return err
	}

	if err := wire.WriteControl(s.Control, wire.TypeTransferComplete, nil); err != nil {
		return fmt.Errorf("send: complete: %w", err)
	}
	ft, body, err := wire.ReadControlRaw(s.Control)
	if err != nil {
		return fmt.Errorf("send: read complete-ack: %w", err)
	}
	if ft == wire.TypeError {
		return peerError(body)
	}
	if ft != wire.TypeTransferAck {
		return fmt.Errorf("%w: expected TRANSFER_ACK, got %v", fserrors.ErrProtocolError, ft)
	}
	_ = s.Control.Close()
	return nil
}

// senderNegotiate reads the receiver's classification, answering any
// TypeVerifyRequest (--checksum) by hashing just the requested files before
// the decision vector arrives.
func senderNegotiate(s *Streams, sources []Source) (map[uint32]wire.Decision, error) {
	byIndex := make(map[uint32]string, len(sources))
	for _, src := range sources {
		byIndex[src.Entry.Index] = src.AbsPath
	}
	out := make(map[uint32]wire.Decision)
	hashed := make(map[uint32][32]byte) // hash each file at most once per session
	for {
		ft, body, err := wire.ReadControlRaw(s.Control)
		if err != nil {
			return nil, fmt.Errorf("send: negotiate: %w", err)
		}
		switch ft {
		case wire.TypeVerifyRequest:
			var idx []uint32
			if err := wire.Decode(body, &idx); err != nil {
				return nil, fmt.Errorf("send: verify request: %w", err)
			}
			resp := make([]wire.FileHash, 0, len(idx))
			for _, i := range idx {
				p, ok := byIndex[i]
				if !ok || p == "" {
					continue
				}
				// Cache so a replayed VERIFY_REQUEST can't make a hostile peer
				// re-hash the same files indefinitely. Bounded by len(sources).
				h, done := hashed[i]
				if !done {
					if h, err = hashFileRoot(p); err != nil {
						return nil, fmt.Errorf("%w: hash %s: %v", fserrors.ErrReadFailed, p, err)
					}
					hashed[i] = h
				}
				resp = append(resp, wire.FileHash{Index: i, Hash: h})
			}
			if err := wire.WriteControl(s.Control, wire.TypeVerifyResponse, resp); err != nil {
				return nil, fmt.Errorf("send: verify response: %w", err)
			}
		case wire.TypeClassifyBatch:
			var batch []wire.Decision
			if err := wire.Decode(body, &batch); err != nil {
				return nil, fmt.Errorf("send: decisions decode: %w", err)
			}
			for _, d := range batch {
				out[d.Index] = d
			}
			// A well-behaved receiver returns at most one decision per entry we
			// sent. More means a malformed or hostile peer trying to grow the
			// map unbounded — the listing path is capped, so cap this too.
			if len(out) > len(sources) {
				return nil, fmt.Errorf("%w: more decisions than entries sent", fserrors.ErrProtocolError)
			}
		case wire.TypeClassifyEnd:
			return out, nil
		case wire.TypeError:
			var ef wire.ErrorFrame
			_ = wire.Decode(body, &ef)
			return nil, mapPeerError(ef)
		default:
			return nil, fmt.Errorf("%w: expected classification, got %v", fserrors.ErrProtocolError, ft)
		}
	}
}

// sendOneFile streams one regular file (resume-aware) through the packer,
// computing its BLAKE3 root inline and emitting it on the EOF segment.
func sendOneFile(ctx context.Context, s *Streams, packer *chunkPacker, src *Source, d wire.Decision, opts SendOptions) error {
	f, err := os.Open(src.AbsPath)
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrReadFailed, err)
	}
	defer func() { _ = f.Close() }()

	root := blake3.New()
	offset := uint64(0)
	if d.Action == wire.DecisionResume && d.ResumeOffset > 0 {
		// Verify the receiver's partial matches our source before skipping
		// those bytes — a mismatch means the source changed.
		got, err := PrefixImohash(src.AbsPath, int64(d.ResumeOffset))
		if err != nil {
			return fmt.Errorf("%w: imohash source prefix: %v", fserrors.ErrReadFailed, err)
		}
		if got != d.PartialImohash {
			_ = s.Data.Close()
			declineTransfer(s, wire.ErrCodePartialMismatch, "receiver's partial does not match source")
			return fserrors.ErrPartialMismatch
		}
		// Hash the prefix into the root (not sent) so the trailer covers the
		// whole file, then seek to the resume point.
		if err := hashPrefixInto(root, f, int64(d.ResumeOffset)); err != nil {
			return fmt.Errorf("%w: hash prefix: %v", fserrors.ErrReadFailed, err)
		}
		if _, err := f.Seek(int64(d.ResumeOffset), io.SeekStart); err != nil {
			return fmt.Errorf("%w: seek: %v", fserrors.ErrReadFailed, err)
		}
		offset = d.ResumeOffset
		if opts.OnResume != nil {
			opts.OnResume(src.Entry.Index, offset, src.Entry.Size)
		}
	}

	sent := offset
	rbuf := make([]byte, 256*1024)
	for sent < src.Entry.Size {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Cap the read to the bytes we declared in the listing. Without this,
		// a file that grew since the walk would overshoot its declared size
		// and the receiver would reject the whole transfer. We send a clean
		// snapshot of the first Size bytes instead.
		toRead := uint64(len(rbuf))
		if remaining := src.Entry.Size - sent; remaining < toRead {
			toRead = remaining
		}
		n, rerr := f.Read(rbuf[:toRead])
		if n > 0 {
			_, _ = root.Write(rbuf[:n])
			if err := packer.appendBytes(src.Entry.Index, rbuf[:n]); err != nil {
				return sendChunkErr(s, err)
			}
			sent += uint64(n)
			if opts.ProgressFn != nil {
				opts.ProgressFn(src.Entry.Index, sent)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("%w: read %s: %v", fserrors.ErrReadFailed, src.AbsPath, rerr)
		}
	}
	var r [32]byte
	copy(r[:], root.Sum(nil))
	return packer.endFile(src.Entry.Index, r)
}

// sendChunkErr surfaces a receiver-posted reason (e.g. write failure) instead
// of the bare stream error, which would misclassify as a network drop.
func sendChunkErr(s *Streams, err error) error {
	if reason := tryReadPeerError(s.Control); reason != nil {
		return reason
	}
	return err
}

// sendStream streams a single unknown-length reader (stdin/--text). Per-chunk
// hashes verify integrity; a BLAKE3 root is emitted on the final segment.
func sendStream(ctx context.Context, s *Streams, opts SendOptions) error {
	packer, err := newChunkPacker(s.Data)
	if err != nil {
		return err
	}
	defer packer.Close()

	root := blake3.New()
	var sent uint64
	rbuf := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := opts.Stream.Read(rbuf)
		if n > 0 {
			_, _ = root.Write(rbuf[:n])
			if err := packer.appendBytes(0, rbuf[:n]); err != nil {
				return sendChunkErr(s, err)
			}
			sent += uint64(n)
			if opts.ProgressFn != nil {
				opts.ProgressFn(0, sent)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("%w: read stream: %v", fserrors.ErrReadFailed, rerr)
		}
	}
	var r [32]byte
	copy(r[:], root.Sum(nil))
	// endFile/flush are where a short stream's write actually hits the wire,
	// so a receiver decline (e.g. target exists) surfaces here — check for
	// its posted reason like appendBytes does.
	if err := packer.endFile(0, r); err != nil {
		return sendChunkErr(s, err)
	}
	if err := packer.flush(); err != nil {
		return sendChunkErr(s, err)
	}
	if opts.OnStreamingEOF != nil {
		opts.OnStreamingEOF(0, sent)
	}

	if err := wire.WriteControl(s.Control, wire.TypeTransferComplete, nil); err != nil {
		return fmt.Errorf("send: complete: %w", err)
	}
	ft, body, err := wire.ReadControlRaw(s.Control)
	if err != nil {
		return fmt.Errorf("send: read complete-ack: %w", err)
	}
	if ft == wire.TypeError {
		return peerError(body)
	}
	if ft != wire.TypeTransferAck {
		return fmt.Errorf("%w: expected TRANSFER_ACK, got %v", fserrors.ErrProtocolError, ft)
	}
	_ = s.Control.Close()
	return nil
}

// notifyPeer posts a terminal ERROR so the peer can tell the real cause
// (cancel, unreadable source, …) from a network drop. Data closes first so
// a receiver blocked mid-chunk unblocks and reads the reason. Bounded so a
// wedged peer can't hold the sender hostage.
func notifyPeer(s *Streams, code wire.ErrorCode, msg string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Data.Close()
		declineTransfer(s, code, msg)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// peerError translates a receiver-reported ERROR frame body into a sentinel.
func peerError(body []byte) error {
	var ef wire.ErrorFrame
	_ = wire.Decode(body, &ef)
	return mapPeerError(ef)
}

// PasswordAttempts is how many password tries the sender allows per session
// before aborting the transfer. A typo at the receiver's no-echo prompt
// shouldn't burn the one-shot code.
const PasswordAttempts = 3

// senderPasswordHandshake runs the challenge/response that gates --password.
// Each mismatch below the attempt cap answers with a WrongPassword error and
// a fresh challenge; the final mismatch closes the stream as before.
func senderPasswordHandshake(s *Streams, password string) error {
	for attempt := 1; ; attempt++ {
		var nonce [32]byte
		if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
			return fmt.Errorf("send: password nonce: %w", err)
		}
		if err := wire.WriteControl(s.Control, wire.TypePasswordChallenge, &wire.PasswordChallenge{Nonce: nonce}); err != nil {
			// A receiver that can't retry (fixed password, old fsend) hangs
			// up after its mismatch; the honest error is still the password.
			if attempt > 1 {
				return fserrors.ErrWrongPassword
			}
			return fmt.Errorf("send: password challenge: %w", err)
		}
		ft, body, err := wire.ReadControlRaw(s.Control)
		if err != nil {
			if attempt > 1 {
				return fserrors.ErrWrongPassword
			}
			return fmt.Errorf("send: read password response: %w", err)
		}
		if ft == wire.TypeError {
			return peerError(body)
		}
		if ft != wire.TypePasswordResponse {
			return fmt.Errorf("%w: expected PASSWORD_RESPONSE, got %v", fserrors.ErrProtocolError, ft)
		}
		var resp wire.PasswordResponse
		if err := wire.Decode(body, &resp); err != nil {
			return fmt.Errorf("send: decode password response: %w", err)
		}
		expected := hmacPassword(password, nonce[:])
		if subtle.ConstantTimeCompare(expected, resp.HMAC[:]) == 1 {
			if err := wire.WriteControl(s.Control, wire.TypePasswordVerified, nil); err != nil {
				return fmt.Errorf("send: password verified: %w", err)
			}
			return nil
		}
		_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
			Code: wire.ErrCodeWrongPassword, Message: "wrong password",
		})
		if attempt == PasswordAttempts {
			_ = s.Control.Close()
			_, _ = io.Copy(io.Discard, s.Control)
			return fserrors.ErrWrongPassword
		}
	}
}

// hmacPassword is the shared response tag; argon2id stretches the password
// (salted by the session nonce) before keying the HMAC.
func hmacPassword(password string, nonce []byte) []byte {
	key := argon2.IDKey([]byte(password), nonce, 2, 64*1024, 4, 32)
	m := hmac.New(sha256.New, key)
	m.Write(nonce)
	return m.Sum(nil)
}
