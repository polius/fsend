package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/iceconn"
	"github.com/polius/fsend/internal/landisc"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/retry"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/version"
	"github.com/polius/fsend/internal/wire"
)

// This file implements the parallel-pair sender. The high-level shape:
//
//   runSendParallel
//     ├── goroutine: pairOverLAN(pairCtx)        → lanSenderPairing
//     └── goroutine: pairOverInternet(pairCtx)   → internetSenderPairing
//
// The first goroutine to return a pairing wins; pairCtx is cancelled so
// the loser tears down cleanly. The transfer phase then runs on the
// winner. There is no LAN-only "budget" — same-LAN receivers always win
// because the receiver only contacts the pairing server after its
// 300 ms mDNS query misses (see receive.go). The internet path is
// always-on so cross-network receivers don't have to wait for any timer.

// lanSenderPairing holds everything the LAN transfer phase needs after
// the first peer dials in. listener stays open across retries so a
// mid-transfer drop can re-Accept on the same UDP socket; mDNS is
// closed exactly once at pair time.
type lanSenderPairing struct {
	listener *quicconn.Listener
	firstRes *quicconn.AcceptResult // already paired, used for attempt 0
	cleanup  func()                 // closes listener (mDNS already stopped)
}

// internetSenderPairing is the internet-side counterpart. The QUIC
// listener is bound to the established PacketConn (ICE-direct or relay);
// retries re-Accept on it. The cleanup func tears down the QUIC
// transport, the underlying PacketConn, and Deletes the server session.
type internetSenderPairing struct {
	quicListener *quic.Listener
	firstRes     *quicconn.AcceptResult
	code         string
	pathInfo     connpath.Info
	cleanup      func()

	// sigClient, sessionID, and roleToken let the post-transfer error
	// path probe the pairing server for a relay eviction reason. Without
	// this, a 100 MiB-cap-hit looks identical to a flaky network.
	sigClient *signaling.Client
	sessionID string
	roleToken string
}

// sendPairOutcome is what a pair goroutine reports back.
type sendPairOutcome struct {
	lan    *lanSenderPairing
	server *internetSenderPairing
	err    error
}

// pairOverLAN binds the deterministic LAN port for the code, announces
// it via mDNS, and blocks on Accept until the first receiver dials in.
// On success it stops the mDNS announcement (late receivers should fall
// through to the server) and returns the paired connection plus the
// listener for retry re-Accepts.
//
// Errors: setup failure (port conflict, no interface) is returned
// immediately. Context cancellation while waiting for Accept surfaces
// as ctx.Err() — the coordinator treats that as "loser, no-op".
func pairOverLAN(ctx context.Context, code string) (*lanSenderPairing, error) {
	port := landisc.PortForCode(code)
	ln, err := quicconn.ListenAddr(":"+strconv.Itoa(port), code)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", fserrors.ErrLANListenerFailed, err)
	}
	mdnsConn, err := landisc.Announce(code, landisc.PreferredLocalIP())
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("%w: mDNS announce: %v", fserrors.ErrLANListenerFailed, err)
	}
	var stopMDNSOnce sync.Once
	stopMDNS := func() { stopMDNSOnce.Do(func() { _ = mdnsConn.Close() }) }

	res, err := ln.Accept(ctx)
	if err != nil {
		stopMDNS()
		_ = ln.Close()
		return nil, err
	}
	stopMDNS()

	return &lanSenderPairing{
		listener: ln,
		firstRes: res,
		cleanup: func() {
			stopMDNS()
			_ = ln.Close()
		},
	}, nil
}

// waitMaxConsecFails is how many consecutive Wait failures the sender
// tolerates (sleeping 2s·n between attempts) before giving up on the
// internet path and deleting the session.
const waitMaxConsecFails = 6

// firstAcceptTimeout bounds the first QUIC accept after the receiver
// joins. The receiver's connect ladder (LAN dial → ICE → relay, with
// dial retries) gives up well inside this window, so expiry means the
// receiver is gone — without the bound the sender spins "Waiting for
// receiver" forever.
const firstAcceptTimeout = 60 * time.Second

// errPairedGone marks failures after a receiver claimed the code. The
// LAN race can't recover from these — a receiver that joined via the
// server already failed LAN discovery — so the coordinator aborts
// instead of waiting on a doomed LAN listener.
var errPairedGone = fmt.Errorf("%w: the receiver paired but the connection could not be established", fserrors.ErrConnectFailed)

// pairOverInternet runs the full pairing-server + ICE/relay handshake and
// returns once the receiver has paired and the QUIC SenderHandshake
// over the established data path is up.
//
// The function owns the server-side session lifecycle: it Creates a
// session keyed by our code's argon2id slot (the server never learns
// the code itself), long-polls Wait until a receiver Joins, runs ICE
// (falling back to a server-side relay on failure), then runs the QUIC
// handshake. The cleanup func threaded onto the returned pairing
// guarantees the server session is Deleted and resources released on
// any teardown path.
func pairOverInternet(ctx context.Context, f *flags, code string, cfg *config.Config) (*internetSenderPairing, error) {
	client, _ := signalingClient(cfg)
	created, err := client.Create(ctx, code)
	if err != nil {
		return nil, err
	}
	// From here on we own a server-side session and must Delete it on
	// every error path. We use a fresh ctx for Delete because the parent
	// ctx may already be cancelled (e.g., the LAN path won the race).
	deleteSession := func() { _ = client.Delete(context.Background(), created.SessionID, created.RoleToken) }

	waitResp, err := waitForReceiver(ctx, client, created.Code)
	if err != nil {
		deleteSession()
		return nil, err
	}

	// Establish the underlying data path: ICE-direct first, relay fallback.
	pc, pathInfo, err := establishInternetDataPath(ctx, f, client, created, waitResp)
	if err != nil {
		deleteSession()
		if ctx.Err() == nil {
			err = fmt.Errorf("%w: %v", errPairedGone, err)
		}
		return nil, err
	}

	// Bring up QUIC on the established PacketConn and run the sender
	// handshake on the first peer. Retries re-Accept on the same listener.
	acceptCtx, cancelAccept := context.WithTimeout(ctx, firstAcceptTimeout)
	ln, res, teardown, err := senderQUICAccept(acceptCtx, pc, created.Code)
	cancelAccept()
	if err != nil {
		deleteSession()
		if ctx.Err() == nil {
			err = fmt.Errorf("%w: %v", errPairedGone, err)
		}
		return nil, err
	}

	return &internetSenderPairing{
		quicListener: ln,
		firstRes:     res,
		code:         created.Code,
		pathInfo:     pathInfo,
		sigClient:    client,
		sessionID:    created.SessionID,
		roleToken:    created.RoleToken,
		cleanup: func() {
			teardown()
			deleteSession()
		},
	}, nil
}

// waitRetryStep scales the backoff between Wait retries (n·step for the
// n-th consecutive failure). Var so tests can shrink it.
var waitRetryStep = 2 * time.Second

// waitForReceiver long-polls Wait until the receiver pairs, the user
// cancels, or the server reaps the session. The server's per-call
// long-poll timeout returns nil periodically; we just re-issue. There
// is no client-side deadline — the user controls the wait duration by
// keeping the terminal open.
//
// If the server reaps the session (unpaired TTL; ErrCodeNotFound from
// Wait), we surface a dedicated "session expired" error instead of the
// receiver-side E002 wording.
//
// Anything else is retried with backoff: senders wait minutes, and a
// single dropped poll (wifi roam, laptop sleep/wake) must not abandon a
// session the server still holds. The give-up threshold counts
// consecutive failures, not elapsed time — a wall-clock deadline would
// expire during a long sleep and kill the code on wake.
func waitForReceiver(ctx context.Context, client *signaling.Client, code string) (*server.WaitResponse, error) {
	consecFails := 0
	for {
		resp, err := client.Wait(ctx, code)
		switch {
		case err == nil:
			consecFails = 0
			if resp != nil {
				return resp, nil
			}
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, fserrors.ErrCodeNotFound):
			return nil, fserrors.ErrSessionExpired
		default:
			consecFails++
			if consecFails >= waitMaxConsecFails {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(consecFails) * waitRetryStep):
			}
		}
	}
}

// establishInternetDataPath wraps the ICE-then-relay ladder. On ICE
// success it returns the ICE-owning PacketConn; on failure it falls back
// to a relay PacketConn. The pathInfo reflects the choice for UX
// rendering.
//
// The relay is allocated *before* ICE runs: its UDP address doubles as
// the STUN server for srflx gathering — without that, two NATed peers
// only exchange private addresses and ICE is doomed to time out. The
// allocation is idempotent and idle-evicted server-side, so it costs
// nothing when ICE wins. If allocation fails (e.g. relay not enabled),
// ICE still runs with host candidates only, and its failure surfaces
// the allocation error — the same outcome the old post-ICE allocation
// produced.
//
// The debug --mode flag short-circuits the ladder:
//   - modeDirect: only ICE; surface the ICE error if it fails (no relay fallback).
//   - modeRelay:  skip ICE entirely; relay only.
func establishInternetDataPath(ctx context.Context, f *flags, client *signaling.Client, created *signaling.CreateResult, waitResp *server.WaitResponse) (net.PacketConn, connpath.Info, error) {
	if f != nil && f.mode == modeRelay {
		return allocAndDialRelay(ctx, client, created.SessionID, created.RoleToken)
	}
	alloc, allocErr := client.AllocateRelay(ctx, created.SessionID, created.RoleToken)
	stunAddr := ""
	if allocErr == nil {
		stunAddr = alloc.RelayAddr
	}
	iceConn, icePath, iceErr := iceEstablish(ctx, client, created.SessionID, created.RoleToken, iceconn.Options{
		LocalUfrag:  created.IceCredentials.Ufrag,
		LocalPwd:    created.IceCredentials.Pwd,
		RemoteUfrag: waitResp.PeerIceCredentials.Ufrag,
		RemotePwd:   waitResp.PeerIceCredentials.Pwd,
		STUNAddr:    stunAddr,
	}, true /* controlling */, f != nil && f.debug)
	if iceErr == nil {
		return iceConn, icePath, nil
	}
	if f != nil && f.debug {
		fmt.Fprintln(os.Stderr, "DEBUG: ICE failed:", iceErr)
	}
	if f != nil && f.mode == modeDirect {
		return nil, connpath.Info{}, fmt.Errorf("%w: ICE failed under --mode=direct: %v", fserrors.ErrConnectFailed, iceErr)
	}
	if allocErr != nil {
		return nil, connpath.Info{}, fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, allocErr)
	}
	rc, err := dialRelay(alloc)
	if err != nil {
		return nil, connpath.Info{}, fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	return rc, connpath.FromRelay(alloc.RelayAddr), nil
}

// allocAndDialRelay performs the relay allocation + dial steps. Shared
// between the default ICE-failure fallback and the debug --mode=relay
// short-circuit so both produce identical errors and pathInfo.
func allocAndDialRelay(ctx context.Context, client *signaling.Client, sessionID, roleToken string) (net.PacketConn, connpath.Info, error) {
	alloc, err := client.AllocateRelay(ctx, sessionID, roleToken)
	if err != nil {
		return nil, connpath.Info{}, fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	rc, err := dialRelay(alloc)
	if err != nil {
		return nil, connpath.Info{}, fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	return rc, connpath.FromRelay(alloc.RelayAddr), nil
}

// senderQUICAccept brings up a quic.Transport on the established
// PacketConn, accepts the first inbound QUIC connection, runs the
// SPAKE2 sender handshake, and returns the listener (for retries), the
// first AcceptResult, and a teardown closure.
func senderQUICAccept(ctx context.Context, pc net.PacketConn, code string) (*quic.Listener, *quicconn.AcceptResult, func(), error) {
	tlsCfg, err := quicconn.SenderTLSConfig()
	if err != nil {
		_ = pc.Close()
		return nil, nil, nil, err
	}
	tr := &quic.Transport{Conn: pc}
	ln, err := tr.Listen(tlsCfg, quicconn.QuicConfig())
	if err != nil {
		_ = tr.Close()
		_ = pc.Close()
		return nil, nil, nil, fmt.Errorf("QUIC listen: %w", err)
	}
	qc, err := ln.Accept(ctx)
	if err != nil {
		_ = ln.Close()
		_ = tr.Close()
		_ = pc.Close()
		return nil, nil, nil, fmt.Errorf("QUIC accept: %w", err)
	}
	res, err := quicconn.SenderHandshake(ctx, qc, code)
	if err != nil {
		_ = ln.Close()
		_ = tr.Close()
		_ = pc.Close()
		return nil, nil, nil, err
	}
	teardown := func() {
		_ = ln.Close()
		_ = tr.Close()
		_ = pc.Close()
	}
	return ln, res, teardown, nil
}

// runSendParallel is the top-level coordinator. It runs the two pair
// paths concurrently, picks the first that succeeds, cancels the loser,
// and runs the transfer on the winner.
//
// Failure handling is deliberately asymmetric:
//   - LAN-only failure (e.g. port conflict): keep waiting for the server
//     to pair; surface an ℹ line so the user knows internet is the only
//     remaining path.
//   - Server-only failure (unreachable): keep waiting for LAN; surface a
//     ⚠ line so the user knows only same-LAN receivers can connect now.
//   - Both fail: return the most informative error (E001 if the server
//     was unreachable, otherwise the LAN error).
func runSendParallel(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, label, code string, cfg *config.Config, waitSpin *uxlog.Spinner) error {
	pairCtx, cancelPair := context.WithCancel(ctx)
	defer cancelPair()
	// Belt-and-braces: if we exit through an unusual path (panic-recover,
	// ctx cancel before any branch), make sure the spinner goroutine isn't
	// left scribbling on the screen. waitSpin gets reassigned on notice
	// swaps, so the defer must close over the variable, not its value at
	// defer-time.
	defer func() { waitSpin.Stop() }()

	lanCh := make(chan sendPairOutcome, 1)
	serverCh := make(chan sendPairOutcome, 1)

	// --mode collapses the LAN+internet race to a single path. The
	// disabled side is marked done upfront and its goroutine never starts,
	// so the coordinator only ever picks a winner from the enabled side.
	lanDisabled := f.mode == modeDirect || f.mode == modeRelay
	serverDisabled := f.mode == modeLocal

	if !lanDisabled {
		go func() {
			p, err := pairOverLAN(pairCtx, code)
			lanCh <- sendPairOutcome{lan: p, err: err}
		}()
	}
	if !serverDisabled {
		go func() {
			p, err := pairOverInternet(pairCtx, f, code, cfg)
			serverCh <- sendPairOutcome{server: p, err: err}
		}()
	}

	var (
		lanDone, serverDone bool
		lanErr, serverErr   error
		serverDownNoticed   bool
		lanDownNoticed      bool
		winner              sendPairOutcome
		winnerPicked        bool
	)
	lanDone = lanDisabled
	serverDone = serverDisabled

	for !winnerPicked && (!lanDone || !serverDone) {
		select {
		case <-ctx.Done():
			waitSpin.Stop()
			cancelPair()
			drainBoth(lanCh, serverCh, lanDone, serverDone)
			return ctx.Err()

		case res := <-lanCh:
			lanDone = true
			if res.err == nil {
				winner = res
				winnerPicked = true
				continue
			}
			lanErr = res.err
			if errors.Is(lanErr, context.Canceled) {
				continue
			}
			// Only the notice when there's another path left (not under
			// --mode=local). Same-LAN receivers can still pair through
			// the server (host↔host ICE), so this describes the loss of
			// the discovery shortcut, not a reachability verdict.
			if !lanDownNoticed && !f.quiet && !serverDisabled {
				lanDownNoticed = true
				// Pause the spinner so the notice prints on a clean
				// line, then restart it for the remaining (server)
				// path. The next pair outcome will either flip
				// winnerPicked (and Stop fires via deferred cleanup)
				// or trigger another notice.
				waitSpin.Stop()
				fmt.Fprintln(os.Stderr, uxlog.Info(), "Local-network discovery unavailable — connecting via the server instead.")
				waitSpin = startWaitSpinner(f, "Waiting for receiver via server")
			}

		case res := <-serverCh:
			serverDone = true
			if res.err == nil {
				winner = res
				winnerPicked = true
				continue
			}
			serverErr = res.err
			if errors.Is(serverErr, context.Canceled) {
				continue
			}
			// A receiver claimed the code and then the connection died.
			// That receiver already failed LAN discovery, so waiting on
			// the LAN listener can never pair — abort the race.
			if errors.Is(serverErr, errPairedGone) {
				waitSpin.Stop()
				cancelPair()
				drainBoth(lanCh, serverCh, lanDone, serverDone)
				return serverErr
			}
			// Any server-path failure (unreachable, rate-limited, bad
			// password, 5xx) leaves the code working on the local network
			// only — say so, or the sender waits forever on a code that
			// cross-network receivers can't redeem. Skipped when the user
			// explicitly forced the internet path with --mode=direct/relay:
			// LAN is disabled, so the notice would be misleading.
			if !serverDownNoticed && !f.quiet && !lanDisabled {
				serverDownNoticed = true
				waitSpin.Stop()
				fmt.Fprintln(os.Stderr, uxlog.Warn(), "Server unavailable — only receivers on your local network can connect.")
				if entry, known := fserrors.Lookup(serverErr); known {
					fmt.Fprintf(os.Stderr, "  [%s] %s\n", entry.Code, entry.Message)
				}
				waitSpin = startWaitSpinner(f, "Waiting for receiver on local network")
			}
		}
	}

	if !winnerPicked {
		waitSpin.Stop()
		return pickFinalSendError(lanErr, serverErr)
	}

	cancelPair()
	drainLoser(lanCh, serverCh, winner, lanDone, serverDone)
	// Stop the wait spinner now so the connected line and progress bar
	// land on a clean line. The deferred Stop above is idempotent.
	waitSpin.Stop()

	pathInfo := connpath.FromLAN()
	if winner.lan == nil {
		pathInfo = winner.server.pathInfo
	}
	printPath(f, pathInfo)
	// The receiver may now sit at its accept prompt for a while; without
	// this line the sender stares at a dead terminal wondering whether
	// the code even arrived. The path rides along so both sides see the
	// route before any bytes flow. ✓ even for relay: the glyph reports
	// the connect succeeding, the chip carries the route — same neutral
	// treatment as the receiver chip and the summary tag.
	if !f.quiet {
		fmt.Fprintf(os.Stderr, "%s Receiver connected (%s) — waiting for them to accept.\n",
			uxlog.Check(), pathInfo.Chip())
	}

	if winner.lan != nil {
		return runSenderTransferOverLAN(ctx, f, items, kind, totalFiles, label, winner.lan)
	}
	return runSenderTransferOverInternet(ctx, f, items, kind, totalFiles, label, winner.server)
}

// startWaitSpinner returns a quiet-aware spinner. In --quiet mode we
// emit no animation and return nil; Stop is nil-safe so call sites stay
// free of branches.
func startWaitSpinner(f *flags, msg string) *uxlog.Spinner {
	if f.quiet {
		return nil
	}
	return uxlog.StartSpinner(msg)
}

// runSenderTransferOverLAN runs the transfer protocol on a paired LAN
// connection. The first attempt uses the AcceptResult captured at pair
// time; transient mid-transfer errors trigger a retry that re-Accepts
// on the same listener (the receiver's own retry loop re-Dials).
func runSenderTransferOverLAN(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, displayName string, pair *lanSenderPairing) error {
	defer pair.cleanup()
	return runSenderTransferLoop(ctx, f, items, kind, totalFiles, displayName, connpath.FromLAN(), pair.firstRes, func(ctx context.Context) (*quicconn.AcceptResult, error) {
		acceptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		res, err := pair.listener.Accept(acceptCtx)
		if err != nil {
			return nil, fmt.Errorf("QUIC accept: %w", err)
		}
		return res, nil
	})
}

// runSenderTransferOverInternet is the internet-path counterpart. Same
// retry shape as LAN: first attempt uses the captured AcceptResult,
// subsequent attempts re-Accept on the same QUIC listener (the
// underlying PacketConn is preserved across attempts).
func runSenderTransferOverInternet(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, displayName string, pair *internetSenderPairing) error {
	defer pair.cleanup()
	err := runSenderTransferLoop(ctx, f, items, kind, totalFiles, displayName, pair.pathInfo, pair.firstRes, func(ctx context.Context) (*quicconn.AcceptResult, error) {
		// Bounded like the LAN re-accept: if the receiver exited
		// terminally, an unbounded Accept would hang "retrying…" until
		// Ctrl-C instead of exhausting the retry budget.
		acceptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		qc, err := pair.quicListener.Accept(acceptCtx)
		if err != nil {
			return nil, fmt.Errorf("QUIC accept: %w", err)
		}
		return quicconn.SenderHandshake(ctx, qc, pair.code)
	})
	if pair.pathInfo.Kind == connpath.KindRelay {
		return classifyRelayDrop(ctx, pair.sigClient, pair.sessionID, pair.roleToken, err)
	}
	return err
}

// runSenderTransferLoop is the shared inner loop. It runs transfer.Send
// under retry.WithBackoff; the first attempt uses firstRes, subsequent
// attempts call reaccept to get a fresh paired connection. Wrapping
// both paths in this helper keeps the two transfer entry points purely
// declarative.
func runSenderTransferLoop(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, displayName string, pathInfo connpath.Info, firstRes *quicconn.AcceptResult, reaccept func(context.Context) (*quicconn.AcceptResult, error)) error {
	closeProg, progressFn, onResume, stats, onStreamingEOF := newSenderProgress(f, items)
	defer closeProg()

	// Time from the first byte, not from here — the receiver may sit at
	// its accept prompt for a while, and counting that wait makes the
	// summary read like a glacial transfer.
	start := time.Now()
	var firstByte time.Time
	progress := func(fi uint32, b uint64) {
		if firstByte.IsZero() {
			firstByte = time.Now()
		}
		progressFn(fi, b)
	}
	current := firstRes
	opts := retry.Options{OnRetry: retryNoticeFor(f)}
	if hasConsumableReader(items) {
		// A partially-consumed stdin/--text reader would resend from an
		// arbitrary offset and still pass per-chunk hashes; fail instead.
		opts.Attempts = 1
	}
	err := retry.WithBackoff(ctx, opts, nil,
		func(attempt int) error {
			if current == nil {
				res, err := reaccept(ctx)
				if err != nil {
					return err
				}
				current = res
			}
			res := current
			current = nil
			defer res.Close()
			return transfer.Send(ctx, &res.Streams, transfer.SendOptions{
				Items:          items,
				Hostname:       hostnameOrDefault(f.hostname),
				OS:             runtime.GOOS,
				ClientVersion:  version.Version,
				TransferKind:   kind,
				TotalFiles:     totalFiles,
				DisplayName:    displayName,
				Password:       f.passArg,
				ProgressFn:     progress,
				OnResume:       onResume,
				OnStreamingEOF: onStreamingEOF,
			})
		})
	if err != nil {
		return err
	}
	// moved counts the bytes this run pushed on the wire; skipped is the
	// resumed prefix the receiver already had. The summary shows the
	// full size but bases the rate on moved alone. printSendSummary
	// no-ops under --quiet, so the outer guard isn't needed.
	moved, skipped := stats()
	total := moved + skipped
	if total == 0 {
		total, moved = totalBytes(items), totalBytes(items)
	}
	// Flush the bar first so its terminal frame lands above the summary
	// (the deferred call is then a no-op).
	closeProg()
	elapsed := time.Since(start)
	if !firstByte.IsZero() {
		elapsed = time.Since(firstByte)
	}
	printSendSummary(f, total, moved, elapsed, pathInfo)
	return nil
}

// isServerDown reports whether an error is the "pairing server is
// unreachable" flavor — used to decide whether to surface the
// "only same-LAN receivers can connect" warning vs treating it as a
// transfer-side failure.
func isServerDown(err error) bool {
	return errors.Is(err, fserrors.ErrServerUnreachable)
}

// pickFinalSendError chooses which error to surface when both pair
// paths failed. Server-unreachable (E001) wins the tiebreaker because
// it carries the most actionable hint ("use --connect <other-host>")
// and is by far the most common cause of total failure (a real LAN bind
// failure is rare; a server outage isn't). If only one side errored,
// we return that one.
func pickFinalSendError(lanErr, serverErr error) error {
	if isServerDown(serverErr) {
		return serverErr
	}
	if lanErr != nil {
		return lanErr
	}
	return serverErr
}

// drainBoth collects any not-yet-consumed outcomes from the pair
// channels after the coordinator gives up (ctx cancelled). Releases
// resources for any pairings that landed late. lanDone / serverDone
// reflect whether the main loop has already drained that channel.
func drainBoth(lanCh, serverCh <-chan sendPairOutcome, lanDone, serverDone bool) {
	if !lanDone {
		res := <-lanCh
		if res.lan != nil {
			res.lan.cleanup()
		}
	}
	if !serverDone {
		res := <-serverCh
		if res.server != nil {
			res.server.cleanup()
		}
	}
}

// drainLoser collects the loser channel's outcome (the goroutine is
// already shutting down because pairCtx was cancelled) and releases any
// resources it may have set up before noticing.
//
// The lanDone / serverDone flags reflect whether the coordinator's main
// loop has already consumed the loser channel's outcome (e.g. the
// server reported "unreachable" before LAN paired). If so, the channel
// is empty and we must not read from it — otherwise we deadlock waiting
// for an outcome that's never coming.
func drainLoser(lanCh, serverCh <-chan sendPairOutcome, winner sendPairOutcome, lanDone, serverDone bool) {
	if winner.lan != nil {
		if serverDone {
			return // server's outcome already consumed in the main loop
		}
		res := <-serverCh
		if res.server != nil {
			res.server.cleanup()
		}
		return
	}
	if lanDone {
		return
	}
	res := <-lanCh
	if res.lan != nil {
		res.lan.cleanup()
	}
}
