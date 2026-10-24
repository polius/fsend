package transfer

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"syscall"
	"time"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// recvFilesToSink streams a ModeFiles transfer to opts.Sink: a single file as
// its raw bytes, anything else (a folder, several files) as a tar archive,
// so `fsend --out - <code> | tar x` works.
func recvFilesToSink(ctx context.Context, s *Streams, hello *wire.SenderHello, entries []wire.ListingEntry, opts RecvOptions) error {
	single := len(entries) == 1 && entries[0].Type == wire.EntryFile
	names := make([]string, len(entries))
	seen := make(map[string]struct{}, len(entries))
	summary := ClassifySummary{Total: len(entries)}
	for i, e := range entries {
		if e.Type == wire.EntrySymlink {
			declineTransfer(s, wire.ErrCodeListingInvalid, "symlink entries are not accepted")
			return fmt.Errorf("%w: peer sent a symlink (%q)", fserrors.ErrPathTraversal, e.RelativePath)
		}
		rel, err := SanitizeRelativePath(e.RelativePath)
		if err != nil {
			declineTransfer(s, wire.ErrCodeListingInvalid, "invalid path")
			return fmt.Errorf("%w: %v", fserrors.ErrPathTraversal, err)
		}
		names[i] = filepath.ToSlash(rel)
		if _, dup := seen[names[i]]; dup {
			declineTransfer(s, wire.ErrCodeListingInvalid, "duplicate path")
			return fmt.Errorf("%w: duplicate path %q", fserrors.ErrProtocolError, e.RelativePath)
		}
		seen[names[i]] = struct{}{}
		summary.NewItems++
		summary.BytesToRecv += e.Size
		summary.OfferedBytes += e.Size
		if e.Type != wire.EntryDir {
			summary.Files = append(summary.Files, SummaryEntry{Index: e.Index, RelativePath: e.RelativePath,
				Size: e.Size, Status: "new", Type: e.Type})
		}
	}
	if opts.Accept != nil && !opts.Accept(*hello, summary) {
		declineTransfer(s, wire.ErrCodeDeclined, "receiver declined")
		return fserrors.ErrReceiverDeclined
	}
	decisions := make([]wire.Decision, len(entries))
	for i, e := range entries {
		decisions[i] = wire.Decision{Index: e.Index, Action: wire.DecisionSend}
	}
	if err := sendDecisions(s.Control, decisions); err != nil {
		return fmt.Errorf("recv: decisions: %w", err)
	}
	if single {
		// A zero-byte file carries no data phase (the sender skips it), so
		// waiting on streamPayload would hang until the idle timeout.
		if entries[0].Size > 0 {
			if err := streamPayload(ctx, s, opts.Sink, func(n uint64) {
				if opts.ProgressFn != nil {
					opts.ProgressFn(0, n)
				}
			}); err != nil {
				return err
			}
		}
		return finishRecv(s)
	}
	if err := streamTar(ctx, s, entries, names, opts); err != nil {
		return err
	}
	return finishRecv(s)
}

// streamTar writes the listing as a tar archive. Headers for folders and
// empty files are emitted in listing order around the data files, whose
// bytes are copied through as they arrive and verified per file.
func streamTar(ctx context.Context, s *Streams, entries []wire.ListingEntry, names []string, opts RecvOptions) error {
	tw := tar.NewWriter(opts.Sink)
	sinkErr := func(err error) error {
		_ = s.Data.Close()
		code := wire.ErrCodeWriteFailed
		if errors.Is(err, syscall.ENOSPC) {
			code = wire.ErrCodeDiskFull
		}
		declineTransfer(s, code, peerSafe("write", "", err))
		return classifyWriteErr("sink write", err)
	}
	next := 0 // first entry whose header isn't written yet
	header := func(i int) error {
		e := entries[i]
		h := &tar.Header{Name: names[i], Mode: int64(e.Mode & 0o7777), ModTime: time.Unix(e.ModTimeSec, 0),
			Typeflag: tar.TypeReg, Size: int64(e.Size), Format: tar.FormatPAX}
		if e.Type == wire.EntryDir {
			h.Typeflag, h.Name, h.Size = tar.TypeDir, names[i]+"/", 0
		}
		return tw.WriteHeader(h)
	}
	// structuralUpTo writes headers for folders and empty files before i;
	// data files are written when their bytes arrive (or skipped).
	structuralUpTo := func(i int) error {
		for ; next < i; next++ {
			if e := entries[next]; e.Type == wire.EntryDir || e.Size == 0 {
				if err := header(next); err != nil {
					return sinkErr(err)
				}
			}
		}
		return nil
	}

	pending := make(map[uint32]int) // data files still expected → position
	for i, e := range entries {
		if e.Type == wire.EntryFile && e.Size > 0 {
			pending[e.Index] = i
		}
	}
	in := make(chan inChunk, 4)
	stop := make(chan struct{})
	defer close(stop)
	if len(pending) > 0 {
		go readChunks(s.Data, in, stop)
	}
	cur := -1 // position of the file being written
	var h *fileHasher
	var written uint64
	defer func() {
		if h != nil {
			h.discard()
		}
	}()
	for len(pending) > 0 {
		var c inChunk
		select {
		case c = <-in:
		case <-ctx.Done():
			return ctx.Err()
		}
		if c.err != nil {
			if errors.Is(c.err, io.EOF) {
				if reason := tryReadPeerError(s.Control); reason != nil {
					return reason
				}
				return fmt.Errorf("recv: stream closed mid-file: %w", fserrors.ErrConnectFailed)
			}
			return fmt.Errorf("recv: chunk: %w", c.err)
		}
		err := func() error {
			defer c.release()
			off := 0
			for _, seg := range c.segs {
				data := c.plain[off : off+int(seg.Length)]
				off += int(seg.Length)
				pos, ok := pending[seg.FileIndex]
				if !ok || seg.Restart || (cur >= 0 && pos != cur) || (cur < 0 && pos < next) {
					declineTransfer(s, wire.ErrCodeListingInvalid, "chunk for a file not awaiting data")
					return fmt.Errorf("%w: chunk for file index %d not awaiting data", fserrors.ErrProtocolError, seg.FileIndex)
				}
				if seg.Skip {
					if cur >= 0 {
						// Bytes of it are already in the archive; it can't be
						// taken back, so the archive would be wrong.
						return fmt.Errorf("%w: %s became unreadable mid-file; the archive is incomplete", fserrors.ErrReadFailed, names[pos])
					}
					delete(pending, seg.FileIndex)
					if opts.OnEntrySkipped != nil {
						opts.OnEntrySkipped(names[pos], "the sender could not read it")
					}
					continue
				}
				if cur < 0 {
					if err := structuralUpTo(pos); err != nil {
						return err
					}
					if err := header(pos); err != nil {
						return sinkErr(err)
					}
					next = pos + 1
					cur, h, written = pos, newFileHasher(), 0
				}
				if written+uint64(len(data)) > entries[pos].Size {
					declineTransfer(s, wire.ErrCodeProtocolError, "received bytes exceed declared size")
					return fmt.Errorf("%w: received bytes exceed size", fserrors.ErrProtocolError)
				}
				if _, err := tw.Write(data); err != nil {
					return sinkErr(err)
				}
				_, _ = h.Write(data)
				written += uint64(len(data))
				if opts.ProgressFn != nil {
					opts.ProgressFn(seg.FileIndex, written)
				}
				if seg.EOF {
					if written != entries[pos].Size || h.Sum() != seg.RootHash {
						h = nil
						declineTransfer(s, wire.ErrCodeFileHashMismatch, "root hash mismatch")
						return fserrors.ErrHashMismatch
					}
					h = nil
					delete(pending, seg.FileIndex)
					cur = -1
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	if err := structuralUpTo(len(entries)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return sinkErr(err)
	}
	return nil
}
