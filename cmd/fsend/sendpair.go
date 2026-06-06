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
// because the receiver only contacts the rendezvous server after its
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
	// the rendezvous server for a relay eviction reason. Without this,
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
		return nil, fmt.Errorf("LAN listener: %w", err)
	}
	mdnsConn, err := landisc.Announce(code, landisc.PreferredLocalIP(), port)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("mDNS announce: %w", err)
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

// pairOverInternet runs the full rendezvous + ICE/relay handshake and
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

	// Long-poll until the receiver pairs or the session TTL expires.
	deadline := time.Now().Add(time.Duration(created.TTLSeconds) * time.Second)
	var waitResp *server.WaitResponse
	for time.Now().Before(deadline) && waitResp == nil {
		resp, err := client.Wait(ctx, created.Code)
		if err != nil {
			deleteSession()
			return nil, err
		}
		waitResp = resp
	}
	if waitResp == nil {
		deleteSession()
		return nil, fmt.Errorf("%w: receiver did not arrive within %ds", fserrors.ErrPromptTimeout, created.TTLSeconds)
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
func establishInternetDataPath(ctx context.Context, f *flags, client *signaling.Client, created *server.CreateSessionResponse, waitResp *server.WaitResponse, serverAddr string) (net.PacketConn, connpath.Info, error) {
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
	alloc, err := client.AllocateRelay(ctx, created.SessionID)
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
func runSendParallel(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, label, code string, cfg *config.Config) error {
	pairCtx, cancelPair := context.WithCancel(ctx)
	defer cancelPair()

	lanCh := make(chan sendPairOutcome, 1)
	serverCh := make(chan sendPairOutcome, 1)

	go func() {
		p, err := pairOverLAN(pairCtx, code)
		lanCh <- sendPairOutcome{lan: p, err: err}
	}()
	go func() {
		p, err := pairOverInternet(pairCtx, f, code, cfg)
		serverCh <- sendPairOutcome{server: p, err: err}
	}()

	var (
		lanDone, serverDone bool
		lanErr, serverErr   error
		serverDownNoticed   bool
		lanDownNoticed      bool
		winner              sendPairOutcome
		winnerPicked        bool
	)

	for !winnerPicked && (!lanDone || !serverDone) {
		select {
		case <-ctx.Done():
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
			if !lanDownNoticed && !f.quiet {
				lanDownNoticed = true
				fmt.Fprintln(os.Stderr, marker("ℹ", "[i]"), "Local network unavailable — receiver must use a different network.")
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
			if !serverDownNoticed && !f.quiet && isServerDown(serverErr) {
				serverDownNoticed = true
				fmt.Fprintln(os.Stderr, marker("⚠", "[!]"), "Rendezvous server unreachable — only same-LAN receivers can connect.")
			}
		}
	}

	if !winnerPicked {
		return pickFinalSendError(lanErr, serverErr)
	}

	cancelPair()
	drainLoser(lanCh, serverCh, winner, lanDone, serverDone)

	if winner.lan != nil {
		printPath(f, connpath.FromLAN())
		return runSenderTransferOverLAN(ctx, f, items, kind, totalFiles, winner.lan)
	}
	printPath(f, winner.server.pathInfo)
	return runSenderTransferOverInternet(ctx, f, items, kind, totalFiles, winner.server)
}

// runSenderTransferOverLAN runs the transfer protocol on a paired LAN
// connection. The first attempt uses the AcceptResult captured at pair
// time; transient mid-transfer errors trigger a retry that re-Accepts
// on the same listener (the receiver's own retry loop re-Dials).
func runSenderTransferOverLAN(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, pair *lanSenderPairing) error {
	defer pair.cleanup()
	return runSenderTransferLoop(ctx, f, items, kind, totalFiles, pair.firstRes, func(ctx context.Context) (*quicconn.AcceptResult, error) {
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
func runSenderTransferOverInternet(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, pair *internetSenderPairing) error {
	defer pair.cleanup()
	err := runSenderTransferLoop(ctx, f, items, kind, totalFiles, pair.firstRes, func(ctx context.Context) (*quicconn.AcceptResult, error) {
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
func runSenderTransferLoop(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, firstRes *quicconn.AcceptResult, reaccept func(context.Context) (*quicconn.AcceptResult, error)) error {
	closeProg, progressFn := newSenderProgress(f, items)
	defer closeProg()

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
				Items:         items,
				Hostname:      hostnameOrDefault(f.hostname),
				OS:            runtime.GOOS,
				ClientVersion: version.Version,
				TransferKind:  kind,
				TotalFiles:    totalFiles,
				Password:      f.passArg,
				ProgressFn:    progressFn,
			})
		})
	if err != nil {
		return err
	}
	if !f.quiet {
		fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Transfer complete")
	}
	return nil
}

// isServerDown reports whether an error is the "rendezvous server is
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
