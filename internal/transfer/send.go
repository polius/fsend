package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

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

	ProgressFn func(index uint32, bytesSent uint64)
	OnResume   func(index uint32, offset, total uint64)
	// OnSkip fires per receiver-declined entry. kept is true when the
	// receiver reported keeping a differing copy; false also means
	// "unknown" (old receivers don't send the flag).
	OnSkip         func(index uint32, kept bool)
	OnStreamingEOF func(index uint32, finalBytes uint64)
	// OnSourceSkipped fires for a file that could not be read; the receiver
	// is told to drop it and the transfer carries on with the rest.
	OnSourceSkipped func(index uint32, err error)
	// OnSourceChanged fires for a file whose size or mtime changed while it
	// was being read; the copy that arrives may be inconsistent.
	OnSourceChanged func(index uint32)
}

// Send executes the full sender-side protocol over the supplied streams.
func Send(ctx context.Context, s *Streams, opts SendOptions) error {
	err := send(ctx, s, opts)
	switch {
	case errors.Is(err, context.Canceled):
		notifyPeer(s, wire.ErrCodeCancelled, "sender cancelled")
	case errors.Is(err, fserrors.ErrReadFailed):
		// A local source failure otherwise surfaces on the receiver as a bare
		// stream close — a "network" error it would burn retries on. Post the
		// reason, without the sender's local paths.
		notifyPeer(s, wire.ErrCodeReadFailed, "the sender could not read its source")
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
		MinVersion:    wire.MinProtocolVersion,
		MaxVersion:    wire.ProtocolVersion,
		Caps:          wire.CapsSupported,
		Hostname:      opts.Hostname,
		OS:            opts.OS,
		ClientVersion: opts.ClientVersion,
		HasPassword:   opts.Password != "",
		Mode:          opts.Mode,
		IsText:        opts.IsText,
		DisplayName:   opts.DisplayName,
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
	if _, ok := wire.Negotiate(ack.MinVersion, ack.MaxVersion); !ok {
		return fserrors.ErrIncompatibleVersion
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
	if len(opts.Sources) > maxListingEntries {
		return fmt.Errorf("%w: %d entries, the limit is %d", fserrors.ErrTooManyEntries, len(opts.Sources), maxListingEntries)
	}
	if err := sendListing(s.Control, opts.Sources); err != nil {
		return fmt.Errorf("send: listing: %w", err)
	}
	decisions, err := senderNegotiate(s, opts.Sources)
	if err != nil {
		return err
	}

	// Verify every resume prefix in the background while earlier files
	// stream, so a big resume doesn't stall the pipe when its turn comes.
	prefixes := startPrefixHashing(ctx, opts.Sources, decisions)

	packer, err := newChunkPacker(s.Data)
	if err != nil {
		return err
	}
	defer func() { _ = packer.Close() }()

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
				opts.OnSkip(src.Entry.Index, d.Kept)
			}
			continue
		}
		// Structural entries and empty files carry no data; the receiver
		// materializes them from the listing.
		if src.Entry.Type != wire.EntryFile || src.Entry.Size == 0 {
			continue
		}
		if err := sendOneFile(ctx, s, packer, src, d, prefixes[src.Entry.Index], opts); err != nil {
			return err
		}
	}
	if err := packer.flush(); err != nil {
		return sendChunkErr(s, err)
	}
	if err := packer.Close(); err != nil {
		return sendChunkErr(s, err)
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

// maxVerifyRounds caps how many VERIFY_REQUESTs the sender will answer in one
// negotiation: one per maxVerifyIDsPerFrame entries, so a hostile receiver
// can't replay the request to make the sender re-hash forever.
const maxVerifyRounds = maxListingEntries/maxVerifyIDsPerFrame + 1

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
	verifyRounds := 0
	for {
		ft, body, err := wire.ReadControlRaw(s.Control)
		if err != nil {
			return nil, fmt.Errorf("send: negotiate: %w", err)
		}
		switch ft {
		case wire.TypeVerifyRequest:
			verifyRounds++
			if verifyRounds > maxVerifyRounds {
				return nil, fmt.Errorf("%w: too many verify requests", fserrors.ErrProtocolError)
			}
			var idx []uint32
			if err := wire.Decode(body, &idx); err != nil {
				return nil, fmt.Errorf("send: verify request: %w", err)
			}
			if err := wire.WriteControl(s.Control, wire.TypeVerifyResponse, verifyHashes(idx, byIndex, hashed)); err != nil {
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

// verifyHashes hashes the requested files a few at a time. A file that
// can't be read is left out, so the receiver treats it as differing and the
// data phase reports it per file. hashed caches roots so a replayed request
// can't make a hostile peer re-hash the same files indefinitely.
func verifyHashes(idx []uint32, byIndex map[uint32]string, hashed map[uint32][32]byte) []wire.FileHash {
	var todo []uint32
	for _, i := range idx {
		if _, done := hashed[i]; !done && byIndex[i] != "" {
			hashed[i] = [32]byte{} // reserve: a duplicate index hashes once
			todo = append(todo, i)
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, verifyWorkers)
	for _, i := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(i uint32, path string) {
			defer func() { <-sem; wg.Done() }()
			if h, err := hashFileRoot(path, nil); err == nil {
				mu.Lock()
				hashed[i] = h
				mu.Unlock()
			}
		}(i, byIndex[i])
	}
	wg.Wait()
	resp := make([]wire.FileHash, 0, len(idx))
	for _, i := range idx {
		if h, ok := hashed[i]; ok && h != ([32]byte{}) {
			resp = append(resp, wire.FileHash{Index: i, Hash: h})
		}
	}
	return resp
}

// prefixResult is the sender's block hashes for a resume prefix.
type prefixResult struct {
	blocks [][32]byte
	err    error
}

// startPrefixHashing hashes each resume prefix of the source in the
// background, in send order. Each result arrives on its own channel.
func startPrefixHashing(ctx context.Context, sources []Source, decisions map[uint32]wire.Decision) map[uint32]chan prefixResult {
	out := make(map[uint32]chan prefixResult)
	var jobs []*Source
	for i := range sources {
		src := &sources[i]
		if d, ok := decisions[src.Entry.Index]; ok && d.Action == wire.DecisionResume && d.ResumeOffset > 0 &&
			d.ResumeOffset < src.Entry.Size && d.ResumeOffset%hashBlockSize == 0 && src.Entry.Type == wire.EntryFile {
			out[src.Entry.Index] = make(chan prefixResult, 1)
			jobs = append(jobs, src)
		}
	}
	go func() {
		for _, src := range jobs {
			ch := out[src.Entry.Index]
			if ctx.Err() != nil {
				ch <- prefixResult{err: ctx.Err()}
				continue
			}
			f, err := os.Open(src.AbsPath)
			if err != nil {
				ch <- prefixResult{err: err}
				continue
			}
			blocks, err := hashBlocks(f, int64(decisions[src.Entry.Index].ResumeOffset), nil)
			_ = f.Close()
			ch <- prefixResult{blocks: blocks, err: err}
		}
	}()
	return out
}

// sendOneFile streams one regular file (resume-aware) through the packer,
// hashing it once, in parallel blocks, for the root on its EOF segment. A
// file that can't be read is skipped (the receiver is told) rather than
// aborting the whole transfer.
func sendOneFile(ctx context.Context, s *Streams, packer *chunkPacker, src *Source, d wire.Decision, prefix chan prefixResult, opts SendOptions) error {
	idx := src.Entry.Index
	skip := func(cause error) error {
		if opts.OnSourceSkipped != nil {
			opts.OnSourceSkipped(idx, cause)
		}
		if err := packer.skipFile(idx); err != nil {
			return sendChunkErr(s, err)
		}
		return nil
	}

	f, err := os.Open(src.AbsPath)
	if err != nil {
		return skip(err)
	}
	defer func() { _ = f.Close() }()

	h := newFileHasher()
	defer h.discard()
	offset := uint64(0)
	if prefix != nil {
		pr := <-prefix
		if pr.err != nil {
			if errors.Is(pr.err, context.Canceled) {
				return pr.err
			}
			return skip(pr.err)
		}
		// Resume only when the receiver's prefix is byte-for-byte ours (by
		// its tree root). Otherwise the source changed or the partial is
		// stale: send the file from 0 and tell the receiver to start over.
		if root := foldRoot(pr.blocks, d.ResumeOffset); string(d.PrefixRoot) == string(root[:]) {
			h.seed(pr.blocks)
			offset = d.ResumeOffset
			if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
				return skip(err)
			}
			if opts.OnResume != nil {
				opts.OnResume(idx, offset, src.Entry.Size)
			}
		} else {
			packer.markRestart(idx)
		}
	}

	sent := offset
	for sent < src.Entry.Size {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Read whole blocks, capped at the size declared in the listing: a
		// file that grew since the walk sends a clean snapshot of that size.
		b := getBlock()[:min(uint64(hashBlockSize), src.Entry.Size-sent)]
		n, rerr := io.ReadFull(f, b)
		if rerr != nil {
			putBlock(b)
			if errors.Is(rerr, io.ErrUnexpectedEOF) || errors.Is(rerr, io.EOF) {
				// Shrank since the walk: what we'd send would be a truncated
				// file that hashes clean. Drop it instead.
				rerr = fmt.Errorf("shrank during send (%d of %d bytes)", sent+uint64(n), src.Entry.Size)
			}
			return skip(rerr)
		}
		if err := packer.appendBytes(idx, b); err != nil {
			putBlock(b)
			return sendChunkErr(s, err)
		}
		h.addBlock(b) // hashed in parallel; the buffer returns to the pool
		sent += uint64(n)
		if opts.ProgressFn != nil {
			opts.ProgressFn(idx, sent)
		}
	}
	changed := false
	if st, err := f.Stat(); err == nil && (st.Size() != int64(src.Entry.Size) || st.ModTime().Unix() != src.Entry.ModTimeSec) {
		changed = true
		if opts.OnSourceChanged != nil {
			opts.OnSourceChanged(idx)
		}
	}
	if err := packer.endFile(idx, h.Sum(), changed); err != nil {
		return sendChunkErr(s, err)
	}
	return nil
}

// sendChunkErr surfaces a receiver-posted reason (e.g. write failure) instead
// of the bare stream error, which would misclassify as a network drop.
func sendChunkErr(s *Streams, err error) error {
	if reason := tryReadPeerError(s.Control); reason != nil {
		return reason
	}
	return err
}

// sendStream streams a single unknown-length reader (stdin/--text). A tree
// root is emitted on the final segment.
func sendStream(ctx context.Context, s *Streams, opts SendOptions) error {
	packer, err := newChunkPacker(s.Data)
	if err != nil {
		return err
	}
	defer func() { _ = packer.Close() }()

	root := newFileHasher()
	defer root.discard()
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
	// endFile/flush are where a short stream's write actually hits the wire,
	// so a receiver decline (e.g. target exists) surfaces here — check for
	// its posted reason like appendBytes does.
	if err := packer.endFile(0, root.Sum(), false); err != nil {
		return sendChunkErr(s, err)
	}
	if err := packer.flush(); err != nil {
		return sendChunkErr(s, err)
	}
	if err := packer.Close(); err != nil {
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
