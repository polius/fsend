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

	// sigClient and sessionID let the post-transfer error path probe
	// the pairing server for a relay eviction reason. Without this,
	// a 100 MiB-cap-hit looks identical to a flaky network.
	sigClient *signaling.Client
	sessionID string
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

// pairOverInternet runs the full pairing-server + ICE/relay handshake and
// returns once the receiver has paired and the QUIC SenderHandshake
// over the established data path is up.
//
// The function owns the server-side session lifecycle: it Creates with
// our suggested code, long-polls Wait until a receiver Joins, runs ICE
// (falling back to a server-side relay on failure), then runs the QUIC
// handshake. The cleanup func threaded onto the returned pairing
// guarantees the server session is Deleted and resources released on
// any teardown path.
func pairOverInternet(ctx context.Context, f *flags, code string, cfg *config.Config) (*internetSenderPairing, error) {
	client, serverAddr := signalingClient(cfg)
	created, err := client.Create(ctx, code)
	if err != nil {
		return nil, err
	}
	// From here on we own a server-side session and must Delete it on
	// every error path. We use a fresh ctx for Delete because the parent
	// ctx may already be cancelled (e.g., the LAN path won the race).
	deleteSession := func() { _ = client.Delete(context.Background(), created.SessionID) }

	// Long-poll indefinitely until the receiver pairs, the user
	// cancels, or the server reaps the session. The server's per-call
	// long-poll timeout returns nil periodically; we just re-issue.
	// There is no client-side deadline — the user controls the wait
	// duration by keeping the terminal open.
	//
	// If the server reaps the session out from under us (unpaired TTL
	// hit on the server side; ErrCodeNotFound from Wait), we surface a
	// dedicated "session expired" error instead of the receiver-side
	// E002 wording.
	var waitResp *server.WaitResponse
	for waitResp == nil {
		resp, err := client.Wait(ctx, created.Code)
		if err != nil {
			if errors.Is(err, fserrors.ErrCodeNotFound) {
				deleteSession()
				return nil, fserrors.ErrSessionExpired
			}
			deleteSession()
			return nil, err
		}
		waitResp = resp
	}

	// Establish the underlying data path: ICE-direct first, relay fallback.
	pc, pathInfo, err := establishInternetDataPath(ctx, f, client, created, waitResp, serverAddr)
	if err != nil {
		deleteSession()
		return nil, err
	}

	// Bring up QUIC on the established PacketConn and run the sender
	// handshake on the first peer. Retries re-Accept on the same listener.
	ln, res, teardown, err := senderQUICAccept(ctx, pc, created.Code)
	if err != nil {
		deleteSession()
		return nil, err
	}

	return &internetSenderPairing{
		quicListener: ln,
		firstRes:     res,
		code:         created.Code,
		pathInfo:     pathInfo,
		sigClient:    client,
		sessionID:    created.SessionID,
		cleanup: func() {
			teardown()
			deleteSession()
		},
	}, nil
}

// establishInternetDataPath wraps the ICE-then-relay ladder. On ICE
// success it returns the ICE-owning PacketConn; on failure it allocates
// a server-side relay and returns a relay PacketConn. The pathInfo
// reflects the choice for UX rendering.
//
// The debug --mode flag short-circuits the ladder:
//   - modeSTUN: only ICE; surface the ICE error if it fails (no relay fallback).
//   - modeTURN: skip ICE entirely; allocate the relay immediately.
func establishInternetDataPath(ctx context.Context, f *flags, client *signaling.Client, created *server.CreateSessionResponse, waitResp *server.WaitResponse, serverAddr string) (net.PacketConn, connpath.Info, error) {
	if f != nil && f.mode == modeTURN {
		return allocAndDialRelay(ctx, client, created.SessionID, created.RoleToken)
	}
	stunHost := stunHostFromServer(serverAddr)
	iceConn, icePath, iceErr := iceEstablish(ctx, client, created.SessionID, created.RoleToken, iceconn.Options{
		LocalUfrag:  created.IceCredentials.Ufrag,
		LocalPwd:    created.IceCredentials.Pwd,
		RemoteUfrag: waitResp.PeerIceCredentials.Ufrag,
		RemotePwd:   waitResp.PeerIceCredentials.Pwd,
		STUNHost:    stunHost,
	}, true /* controlling */)
	if iceErr == nil {
		return iceConn, icePath, nil
	}
	if f != nil && f.debug {
		fmt.Fprintln(os.Stderr, "DEBUG: ICE failed:", iceErr)
	}
	if f != nil && f.mode == modeSTUN {
		return nil, connpath.Info{}, fmt.Errorf("%w: ICE failed under --mode=stun: %v", fserrors.ErrConnectFailed, iceErr)
	}
	return allocAndDialRelay(ctx, client, created.SessionID, created.RoleToken)
}

// allocAndDialRelay performs the relay allocation + dial steps. Shared
// between the default ICE-failure fallback and the debug --mode=turn
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
	lanDisabled := f.mode == modeSTUN || f.mode == modeTURN
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
			// Don't print the "must use a different network" notice when the
			// user explicitly forced the LAN path with --mode=local:
			// there is no other path, so the bare error is the whole story.
			if !lanDownNoticed && !f.quiet && !serverDisabled {
				lanDownNoticed = true
				// Pause the spinner so the notice prints on a clean
				// line, then restart it for the remaining (server)
				// path. The next pair outcome will either flip
				// winnerPicked (and Stop fires via deferred cleanup)
				// or trigger another notice.
				waitSpin.Stop()
				fmt.Fprintln(os.Stderr, uxlog.Info(), "Local network unavailable — receiver must use a different network.")
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
			// Don't print the "only same-LAN receivers can connect" notice
			// when the user explicitly forced the internet path with
			// --mode=stun/turn: LAN is disabled, so the notice would
			// be misleading.
			if !serverDownNoticed && !f.quiet && !lanDisabled && isServerDown(serverErr) {
				serverDownNoticed = true
				waitSpin.Stop()
				fmt.Fprintln(os.Stderr, uxlog.Warn(), "Server unreachable — only same-LAN receivers can connect.")
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
	// Stop the wait spinner now so the path headline and progress bar
	// land on a clean line. The deferred Stop above is idempotent.
	waitSpin.Stop()

	if winner.lan != nil {
		printPath(f, connpath.FromLAN())
		return runSenderTransferOverLAN(ctx, f, items, kind, totalFiles, label, winner.lan)
	}
	printPath(f, winner.server.pathInfo)
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
		qc, err := pair.quicListener.Accept(ctx)
		if err != nil {
			return nil, fmt.Errorf("QUIC accept: %w", err)
		}
		return quicconn.SenderHandshake(ctx, qc, pair.code)
	})
	if pair.pathInfo.Kind == connpath.KindRelay {
		return classifyRelayDrop(ctx, pair.sigClient, pair.sessionID, err)
	}
	return err
}

// runSenderTransferLoop is the shared inner loop. It runs transfer.Send
// under retry.WithBackoff; the first attempt uses firstRes, subsequent
// attempts call reaccept to get a fresh paired connection. Wrapping
// both paths in this helper keeps the two transfer entry points purely
// declarative.
func runSenderTransferLoop(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, displayName string, pathInfo connpath.Info, firstRes *quicconn.AcceptResult, reaccept func(context.Context) (*quicconn.AcceptResult, error)) error {
	closeProg, progressFn, sentBytes, onStreamingEOF := newSenderProgress(f, items)
	defer closeProg()

	start := time.Now()
	current := firstRes
	err := retry.WithBackoff(ctx, retry.Options{OnRetry: retryNoticeFor(f)}, nil,
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
				ProgressFn:     progressFn,
				OnStreamingEOF: onStreamingEOF,
			})
		})
	if err != nil {
		return err
	}
	// Use the actual bytes counter so resumed transfers reflect what
	// moved on the wire — not the full source size. printSendSummary
	// no-ops under --quiet, so the outer guard isn't needed.
	bytes := sentBytes()
	if bytes == 0 {
		bytes = totalBytes(items)
	}
	printSendSummary(f, bytes, time.Since(start), pathInfo)
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
