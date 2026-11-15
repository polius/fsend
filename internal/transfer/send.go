package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/polius/fsend/internal/conv"
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
	OnSkip func(index uint32, kept bool)
	// OnRefused fires per entry the receiver could not create (a name its
	// system doesn't allow), with the receiver's reason: before the data
	// for a name refused up front, after the last byte for one the
	// receiver's filesystem refused only when the file was written.
	// Without it an entry refused up front is reported through OnSkip as
	// kept.
	OnRefused func(index uint32, reason string)
	// OnRefusedMore fires once, with how many more files the receiver
	// refused at write time than its bounded report lists.
	OnRefusedMore  func(n int)
	OnStreamingEOF func(index uint32, finalBytes uint64)
	// OnPeer fires when the receiver answers the HELLO: its self-reported
	// device name (peer-supplied — sanitize before display) and whether it
	// accepted the transfer.
	OnPeer func(hostname string, accepted bool)
	// OnPasswordOK fires once the receiver has proved it holds the
	// transfer password (password sends only).
	OnPasswordOK func()
	// OnAccepted fires when the receiver has actually agreed to the
	// transfer. For file sends the HELLO reply only lets the listing
	// through — the receiver decides after seeing it — so this fires once
	// its per-file decisions arrive. Stream receivers ask before replying,
	// so for them it fires with the accepting HELLO reply.
	OnAccepted func()
	// OnSourceSkipped fires for a file that could not be read; the receiver
	// is told to drop it and the transfer carries on with the rest.
	OnSourceSkipped func(index uint32, err error)
	// OnSourceChanged fires for a file whose size or mtime changed while it
	// was being read; the copy that arrives may be inconsistent.
	OnSourceChanged func(index uint32)

	// Conn, when set, is done once the connection under the streams is
	// gone, with the close as its cause (quicconn's Conn.Context). Hashing
	// a source can take minutes during which nothing is written, so a drop
	// would go unnoticed; with Conn it ends the hashing at once and Send
	// returns the connection's error.
	Conn context.Context

	// advertiseMax overrides the newest version the HELLO offers — tests
	// stand in for a newer build with it. 0 means wire.ProtocolVersion.
	advertiseMax uint8
	// withoutCaps drops capability bits from the HELLO — tests stand in for
	// an older build with it.
	withoutCaps wire.Capability
}

func (o SendOptions) maxVersion() uint8 {
	if o.advertiseMax != 0 {
		return o.advertiseMax
	}
	return wire.ProtocolVersion
}

// Send executes the full sender-side protocol over the supplied streams.
func Send(ctx context.Context, s *Streams, opts SendOptions) error {
	var negotiated uint8
	err := send(ctx, s, opts, &negotiated)
	switch {
	case errors.Is(err, context.Canceled):
		notifyPeer(s, negotiated, wire.ErrCodeCancelled, "sender cancelled")
	case errors.Is(err, fserrors.ErrReadFailed):
		// A local source failure otherwise surfaces on the receiver as a bare
		// stream close — a "network" error it would burn retries on. Post the
		// reason, without the sender's local paths.
		notifyPeer(s, negotiated, wire.ErrCodeReadFailed, "the sender could not read its source")
	}
	return err
}

// send runs the protocol; *negotiated receives the session's version once
// the hellos are exchanged, so Send's closing notice can carry it too.
func send(ctx context.Context, s *Streams, opts SendOptions, negotiated *uint8) error {
	// Bind the streams to ctx so a Ctrl-C parked in a blocked QUIC write
	// unblocks promptly instead of waiting out the idle timeout. notifyPeer
	// (called by the outer Send with the raw, unwrapped streams) is unaffected.
	s, stop := bindCtx(ctx, s)
	defer stop()

	hello := &wire.SenderHello{
		MinVersion:    wire.MinProtocolVersion,
		MaxVersion:    opts.maxVersion(),
		Caps:          wire.CapsSupported &^ opts.withoutCaps,
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
	v, ok := wire.Negotiate(ack.MinVersion, ack.MaxVersion)
	if !ok {
		return fserrors.ErrIncompatibleVersion
	}
	setVersion(s, v)
	*negotiated = v
	if opts.OnPeer != nil {
		opts.OnPeer(ack.Hostname, ack.Accepts)
	}
	if !ack.Accepts {
		return fserrors.ErrReceiverDeclined
	}
	if opts.Mode == wire.ModeStream && opts.OnAccepted != nil {
		opts.OnAccepted()
	}

	if opts.Password != "" {
		if err := senderPasswordHandshake(s, opts.Password); err != nil {
			return err
		}
		if opts.OnPasswordOK != nil {
			opts.OnPasswordOK()
		}
	}

	if opts.Mode == wire.ModeStream {
		return sendStream(ctx, s, opts)
	}
	return sendFiles(ctx, s, opts, ack.Caps)
}

// sendFiles runs the listing → classify → data flow.
func sendFiles(ctx context.Context, s *Streams, opts SendOptions, peerCaps wire.Capability) error {
	if len(opts.Sources) > maxListingEntries {
		return fmt.Errorf("%w: %d entries, the limit is %d", fserrors.ErrTooManyEntries, len(opts.Sources), maxListingEntries)
	}
	if err := sendListing(s.Control, opts.Sources, peerCaps.Has(wire.CapCompressedListing)); err != nil {
		return fmt.Errorf("send: listing: %w", err)
	}
	// work bounds the hashing of sources: it ends with ctx, with the
	// connection, and when this attempt returns, so no hasher outlives the
	// attempt it worked for.
	work, stopWork := workContext(ctx, opts.Conn)
	defer stopWork()
	decisions, verified, err := senderNegotiate(work, s, opts.Sources)
	if err != nil {
		return err
	}
	if opts.OnAccepted != nil {
		opts.OnAccepted()
	}

	// Verify every resume prefix in the background while earlier files
	// stream, so a big resume doesn't stall the pipe when its turn comes.
	prefixes, hashed := startPrefixHashing(work, opts.Sources, decisions)
	// "No hasher outlives the attempt" includes the moment it returns: the
	// hasher is told to stop and then waited for (it stops within one
	// block), so nothing of this attempt is still reading files, or the
	// cache, once its caller has moved on.
	defer func() {
		stopWork()
		<-hashed
	}()

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
			switch {
			case !ok || src.Entry.Type == wire.EntryDir:
			case d.Reason != "" && opts.OnRefused != nil:
				opts.OnRefused(src.Entry.Index, d.Reason)
			case opts.OnSkip != nil:
				opts.OnSkip(src.Entry.Index, d.Kept)
			}
			continue
		}
		// Structural entries and empty files carry no data; the receiver
		// materializes them from the listing.
		if src.Entry.Type != wire.EntryFile || src.Entry.Size == 0 {
			continue
		}
		// A receiver comparing by content (--checksum) asks for the roots
		// of delivered files again on a retry: remember every file's then.
		if err := sendOneFile(ctx, work, s, packer, src, d, prefixes[src.Entry.Index], verified, opts); err != nil {
			return err
		}
	}
	if err := packer.flush(); err != nil {
		return sendChunkErr(s, err)
	}
	if err := packer.Close(); err != nil {
		return sendChunkErr(s, err)
	}

	result, err := finishSend(s)
	if err != nil {
		return err
	}
	reportWriteRefusals(result, decisions, opts)
	return nil
}

// finishSend posts TRANSFER_COMPLETE and reads the receiver's ack. The ack
// is empty unless the receiver has write-time refusals to report (which it
// does only to a sender that advertised CapWriteRefusals); the result is
// then non-nil.
func finishSend(s *Streams) (*wire.TransferResult, error) {
	if err := wire.WriteControl(s.Control, wire.TypeTransferComplete, nil); err != nil {
		return nil, fmt.Errorf("send: complete: %w", err)
	}
	ft, body, err := wire.ReadControlRaw(s.Control)
	if err != nil {
		return nil, fmt.Errorf("send: read complete-ack: %w", err)
	}
	if ft == wire.TypeError {
		return nil, peerError(body)
	}
	if ft != wire.TypeTransferAck {
		return nil, fmt.Errorf("%w: expected TRANSFER_ACK, got %v", fserrors.ErrProtocolError, ft)
	}
	var result *wire.TransferResult
	if len(body) > 0 {
		result = new(wire.TransferResult)
		if err := wire.Decode(body, result); err != nil {
			return nil, fmt.Errorf("%w: transfer result: %v", fserrors.ErrProtocolError, err)
		}
	}
	_ = s.Control.Close()
	return result, nil
}

// reportWriteRefusals hands the receiver's write-time refusals to the
// caller. The report is peer input: an entry counts only if it is a file
// this sender was asked to send, and only once, and the unlisted remainder
// is capped by the files sent, so a receiver can't invent or inflate
// refusals. Folders are not part of it (the receiver leaves them out, and
// one named anyway is dropped here), as for a name refused up front: every
// file under a refused folder is in the report itself.
func reportWriteRefusals(result *wire.TransferResult, decisions map[uint32]wire.Decision, opts SendOptions) {
	if result == nil {
		return
	}
	sent := make(map[uint32]bool)
	for i := range opts.Sources {
		e := &opts.Sources[i].Entry
		if d, ok := decisions[e.Index]; ok && d.Action != wire.DecisionSkip && e.Type == wire.EntryFile {
			sent[e.Index] = true
		}
	}
	listed := result.Entries[:min(len(result.Entries), wire.MaxWriteRefusals)]
	for _, e := range listed {
		if !sent[e.Index] {
			continue
		}
		delete(sent, e.Index) // what is left are the files not yet reported
		if opts.OnRefused != nil {
			opts.OnRefused(e.Index, e.Reason)
		}
	}
	more := min(conv.Int64(uint64(result.Refused))-int64(len(listed)), int64(len(sent)))
	if more > 0 && opts.OnRefusedMore != nil {
		opts.OnRefusedMore(int(more))
	}
}

// maxVerifyRounds caps how many VERIFY_REQUESTs the sender will answer in one
// negotiation: one per maxVerifyIDsPerFrame entries, so a hostile receiver
// can't replay the request to make the sender re-hash forever.
const maxVerifyRounds = maxListingEntries/maxVerifyIDsPerFrame + 1

// workContext returns the context local work for one connection runs under:
// it ends with ctx, and with conn (when the caller named one), whose close
// then becomes its cause.
func workContext(ctx, conn context.Context) (context.Context, func()) {
	work, cancel := context.WithCancelCause(ctx)
	if conn == nil {
		return work, func() { cancel(nil) }
	}
	stop := context.AfterFunc(conn, func() {
		// The connection's own error, so the caller classifies the drop as
		// it would a failed write (a retry, a deliberate close); never a
		// bare cancellation, which Send reports as the user's Ctrl-C.
		cause := context.Cause(conn)
		if cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
			cancel(fmt.Errorf("%w: connection lost", fserrors.ErrConnectFailed))
			return
		}
		cancel(fmt.Errorf("send: connection lost: %w", cause))
	})
	return work, func() { stop(); cancel(nil) }
}

// workEnded is the error for hashing cut short because work ended: the
// reason the receiver posted before it went, if it did (a decline, a full
// disk), else why work ended — the connection's error, or ctx's.
func workEnded(s *Streams, work context.Context) error {
	cause := context.Cause(work)
	// With the connection gone or ctx cancelled the read returns at once;
	// the bound is for a caller whose Conn ended while its streams live on.
	posted := make(chan error, 1)
	go func() { posted <- tryReadPeerError(s.Control) }()
	select {
	case reason := <-posted:
		if reason != nil {
			return reason
		}
	case <-time.After(time.Second):
	}
	return cause
}

// senderNegotiate reads the receiver's classification, answering any
// TypeVerifyRequest (--checksum) by hashing just the requested files before
// the decision vector arrives. verified reports that there was one. The
// hashing ends with work.
func senderNegotiate(work context.Context, s *Streams, sources []Source) (decisions map[uint32]wire.Decision, verified bool, err error) {
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
			return nil, false, fmt.Errorf("send: negotiate: %w", err)
		}
		switch ft {
		case wire.TypeVerifyRequest:
			verifyRounds++
			if verifyRounds > maxVerifyRounds {
				return nil, false, fmt.Errorf("%w: too many verify requests", fserrors.ErrProtocolError)
			}
			var idx []uint32
			if err := wire.Decode(body, &idx); err != nil {
				return nil, false, fmt.Errorf("send: verify request: %w", err)
			}
			resp := verifyHashes(work, idx, byIndex, hashed)
			if work.Err() != nil {
				// Cut short: the answer would pass unhashed files off as
				// unreadable. Report why the work ended instead.
				return nil, false, workEnded(s, work)
			}
			if err := wire.WriteControl(s.Control, wire.TypeVerifyResponse, resp); err != nil {
				return nil, false, fmt.Errorf("send: verify response: %w", err)
			}
		case wire.TypeClassifyBatch:
			var batch []wire.Decision
			if err := wire.Decode(body, &batch); err != nil {
				return nil, false, fmt.Errorf("send: decisions decode: %w", err)
			}
			for _, d := range batch {
				out[d.Index] = d
			}
			// A well-behaved receiver returns at most one decision per entry we
			// sent. More means a malformed or hostile peer trying to grow the
			// map unbounded — the listing path is capped, so cap this too.
			if len(out) > len(sources) {
				return nil, false, fmt.Errorf("%w: more decisions than entries sent", fserrors.ErrProtocolError)
			}
		case wire.TypeClassifyEnd:
			return out, verifyRounds > 0, nil
		case wire.TypeError:
			var ef wire.ErrorFrame
			_ = wire.Decode(body, &ef)
			return nil, false, mapPeerError(ef)
		default:
			return nil, false, fmt.Errorf("%w: expected classification, got %v", fserrors.ErrProtocolError, ft)
		}
	}
}

// verifyHashes hashes the requested files a few at a time. A file that
// can't be read is left out, so the receiver treats it as differing and the
// data phase reports it per file. hashed caches roots so a replayed request
// can't make a hostile peer re-hash the same files indefinitely. Hashing
// stops when ctx ends; the caller then discards the answer.
func verifyHashes(ctx context.Context, idx []uint32, byIndex map[uint32]string, hashed map[uint32][32]byte) []wire.FileHash {
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
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i uint32, path string) {
			defer func() { <-sem; wg.Done() }()
			if h, err := hashCache.fileRoot(ctx, path, nil); err == nil {
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

// prefixResult is the sender's block hashes for a resume prefix, with the
// state the source was in and the moment its hashing began.
type prefixResult struct {
	blocks [][32]byte
	id     fileID
	began  time.Time
	err    error
}

// startPrefixHashing hashes each resume prefix of the source in the
// background, in send order. Each result arrives on its own channel. It
// stops when work ends (the attempt is over, or its connection is): the
// remaining results then carry work's cause. done closes when the hashing
// goroutine has returned.
func startPrefixHashing(work context.Context, sources []Source, decisions map[uint32]wire.Decision) (results map[uint32]chan prefixResult, done <-chan struct{}) {
	out := make(map[uint32]chan prefixResult)
	finished := make(chan struct{})
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
		defer close(finished)
		for _, src := range jobs {
			ch := out[src.Entry.Index]
			if work.Err() != nil {
				ch <- prefixResult{err: context.Cause(work)}
				continue
			}
			began := time.Now()
			blocks, id, err := hashCache.hashPrefix(work, src.AbsPath, conv.Int64(decisions[src.Entry.Index].ResumeOffset), nil)
			ch <- prefixResult{blocks: blocks, id: id, began: began, err: err}
		}
	}()
	return out, finished
}

// sendOneFile streams one regular file (resume-aware) through the packer,
// hashing it once, in parallel blocks, for the root on its EOF segment. A
// file that can't be read is skipped (the receiver is told) rather than
// aborting the whole transfer.
//
// work is the attempt's hashing context (sendFiles). remember asks for the
// file's block hashes to be kept once it is sent; those of a file cut off
// part-way always are, so a retry's prefix check needn't read it again.
func sendOneFile(ctx, work context.Context, s *Streams, packer *chunkPacker, src *Source, d wire.Decision, prefix chan prefixResult, remember bool, opts SendOptions) error {
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
	keep := sourceBlocks{path: src.AbsPath, began: time.Now()}
	if canResume(src.Entry.Size) || remember {
		if fi, err := f.Stat(); err == nil && conv.Uint64(fi.Size()) == src.Entry.Size {
			keep.id = identify(fi)
		}
	}
	sent, done := uint64(0), false
	defer func() {
		// Sent whole, or cut off part-way with a retry to come: either way
		// these hashes are the ones it would compute again.
		if done && !remember || !done && sent == 0 {
			h.discard()
			return
		}
		keep.save(f, h.blocks())
	}()
	offset := uint64(0)
	if prefix != nil {
		var pr prefixResult
		select {
		case pr = <-prefix:
		case <-work.Done():
			return workEnded(s, work)
		}
		if pr.err != nil {
			if work.Err() != nil {
				return workEnded(s, work) // cut short, not unreadable
			}
			return skip(pr.err)
		}
		// Resume only when the receiver's prefix is byte-for-byte ours (by
		// its tree root). Otherwise the source changed or the partial is
		// stale: send the file from 0 and tell the receiver to start over.
		if root := foldRoot(pr.blocks, d.ResumeOffset); string(d.PrefixRoot) == string(root[:]) {
			h.seed(pr.blocks)
			keep.seeded(pr)
			offset = d.ResumeOffset
			if _, err := f.Seek(conv.Int64(offset), io.SeekStart); err != nil {
				return skip(err)
			}
			if opts.OnResume != nil {
				opts.OnResume(idx, offset, src.Entry.Size)
			}
		} else {
			packer.markRestart(idx)
		}
	}

	sent = offset
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
				rerr = fmt.Errorf("shrank during send (%d of %d bytes)", sent+conv.Uint64(int64(n)), src.Entry.Size)
			}
			return skip(rerr)
		}
		if err := packer.appendBytes(idx, b); err != nil {
			putBlock(b)
			return sendChunkErr(s, err)
		}
		h.addBlock(b) // hashed in parallel; the buffer returns to the pool
		sent += conv.Uint64(int64(n))
		if opts.ProgressFn != nil {
			opts.ProgressFn(idx, sent)
		}
	}
	changed := false
	if st, err := f.Stat(); err == nil && (st.Size() != conv.Int64(src.Entry.Size) || st.ModTime().Unix() != src.Entry.ModTimeSec) {
		changed = true
		if opts.OnSourceChanged != nil {
			opts.OnSourceChanged(idx)
		}
	}
	if err := packer.endFile(idx, h.Sum(), changed); err != nil {
		return sendChunkErr(s, err)
	}
	done = true
	return nil
}

// sourceBlocks decides whether the block hashes computed while sending a
// source may go into hashCache: only for a file that was the listed size
// when opened, is still in that state afterwards, and had been left alone
// for a while before any of its bytes were read (fileID.settled).
type sourceBlocks struct {
	path  string
	id    fileID    // the source when it was opened; zero: don't keep
	began time.Time // when the first of the hashed bytes was read
}

// seeded notes that the hashes start with a prefix hashed earlier (pr): it
// must have been read from the file in this same state.
func (k *sourceBlocks) seeded(pr prefixResult) {
	if !pr.id.same(k.id) {
		k.id = fileID{}
		return
	}
	k.began = pr.began
}

func (k *sourceBlocks) save(f *os.File, blocks [][32]byte) {
	if k.id.fi == nil || !k.id.settled(k.began) {
		return
	}
	if after, err := f.Stat(); err == nil && identify(after).same(k.id) {
		hashCache.put(k.path, k.id, blocks)
	}
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
			sent += conv.Uint64(int64(n))
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

	// A stream has no entries for the receiver to refuse one by one.
	_, err = finishSend(s)
	return err
}

// notifyPeer posts a terminal ERROR so the peer can tell the real cause
// (cancel, unreadable source, …) from a network drop. Data closes first so
// a receiver blocked mid-chunk unblocks and reads the reason. Bounded so a
// wedged peer can't hold the sender hostage.
// v is the negotiated version (0 before the hellos), stamped on the notice
// like every other post-hello frame.
func notifyPeer(s *Streams, v uint8, code wire.ErrorCode, msg string) {
	// A cancel set a past write deadline on the control stream; give the
	// notice its own short window. (Set it on the raw stream: the versioned
	// wrapper below doesn't expose deadlines.)
	setWriteDeadline(s.Control, time.Now().Add(time.Second))
	if v != 0 {
		s = &Streams{Control: versionedConn{ReadWriteCloser: s.Control, v: v}, Data: s.Data}
	}
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
