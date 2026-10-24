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
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	codepkg "github.com/polius/fsend/internal/code"
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
//   runSendParallel                              (one sendSession)
//     └── runSendOnce, per round                 (one sendRound)
//           ├── goroutine: pairOverLAN(pairCtx)      → lanSenderPairing
//           └── goroutine: pairOverInternet(pairCtx) → internetSenderPairing
//
// The first goroutine to return a pairing wins; pairCtx is cancelled so
// the loser tears down in the background. The transfer phase then runs
// on the winner. Same-LAN receivers normally win because the receiver
// gives its mDNS query a head start before contacting the server (see
// receive.go). The internet path is always-on so cross-network receivers
// don't have to wait for any timer.
//
// The first round pairs on the code. Once a receiver authenticates, the
// session switches to a resume key derived from the PAKE binding (see
// pairKey): later rounds rendezvous and authenticate on it, and the
// code's server slot is tombstoned, so the code itself is spent.

// sendSession is state that spans re-pair rounds.
type sendSession struct {
	mu      sync.Mutex
	key     pairKey
	resumed bool

	authFails atomic.Int32
}

// maxAuthFailures caps failed PAKE handshakes per session. Online guessing
// of a 45-bit code is hopeless anyway; the cap makes it impossible and
// tells the sender someone is trying.
const maxAuthFailures = 10

func (s *sendSession) current() (pairKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key, s.resumed
}

// paired switches the session to the resume key on its first successful
// PAKE. Later pairings keep the key, so the receiver needs only one.
func (s *sendSession) paired(binding []byte) pairKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.resumed {
		s.key, s.resumed = resumeKey(binding), true
	}
	return s.key
}

// authFailed counts a failed handshake and reports whether the cap is hit.
func (s *sendSession) authFailed(err error) error {
	if errors.Is(err, fserrors.ErrPeerAuthFailed) && s.authFails.Add(1) >= maxAuthFailures {
		return fmt.Errorf("%w: %v", fserrors.ErrTooManyPairingFailures, err)
	}
	return nil
}

// acceptCounting accepts the next authenticated connection, absorbing
// failed handshakes (impostors, a wrong code) into the session's cap.
func (s *sendSession) acceptCounting(ctx context.Context, ln *quicconn.Listener) (*quicconn.AcceptResult, error) {
	for {
		res, err := ln.Accept(ctx)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if capErr := s.authFailed(err); capErr != nil {
			return nil, capErr
		}
		if !errors.Is(err, quicconn.ErrHandshakeFailed) {
			return nil, err // the listener itself failed
		}
	}
}

// sendRound is one pairing round.
type sendRound struct {
	key    pairKey
	resume bool        // key is a resume key, not the code
	paired atomic.Bool // set once this round picked a winner
}

// consumeCode reports whether this round spent the code: the server slot
// is then tombstoned rather than deleted, so a late second receiver hears
// "already claimed". A resume slot is only deleted — the next round
// re-creates it.
func (r *sendRound) consumeCode() bool { return r.paired.Load() && !r.resume }

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
// transport, the underlying PacketConn, and ends the server session.
type internetSenderPairing struct {
	listener *quicconn.Listener
	firstRes *quicconn.AcceptResult
	pathInfo connpath.Info
	cleanup  func()

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

// listener returns the winner's QUIC listener.
func (o sendPairOutcome) listener() *quicconn.Listener {
	if o.lan != nil {
		return o.lan.listener
	}
	return o.server.listener
}

func (o sendPairOutcome) firstRes() *quicconn.AcceptResult {
	if o.lan != nil {
		return o.lan.firstRes
	}
	return o.server.firstRes
}

// pairOverLAN binds the deterministic LAN port for the round's key,
// announces it via mDNS, and blocks until the first receiver
// authenticates. On success it stops the mDNS announcement (late
// receivers should fall through to the server) and returns the paired
// connection plus the listener for retry re-Accepts.
//
// Errors: setup failure (port conflict, no interface) is returned
// immediately. Context cancellation while waiting surfaces as ctx.Err()
// — the coordinator treats that as "loser, no-op".
func pairOverLAN(ctx context.Context, sess *sendSession, key pairKey) (*lanSenderPairing, error) {
	epoch := codepkg.Epoch(time.Now())
	port := landisc.PortForCode(key.rendezvous, epoch)
	ln, err := listenLANWithRetry(ctx, port, key.secret)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", fserrors.ErrLANListenerFailed, err)
	}
	// No pinned address: each query is answered with the address of the
	// interface it arrived on (see landisc.Announce).
	mdnsConn, err := landisc.Announce(key.rendezvous, epoch, nil)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("%w: mDNS announce: %v", fserrors.ErrLANListenerFailed, err)
	}
	var stopMDNSOnce sync.Once
	// Bounded: a synchronous Close could wedge, hanging pairOverLAN and the
	// coordinator draining lanCh. See landisc.StopAnnounce.
	stopMDNS := func() { stopMDNSOnce.Do(func() { landisc.StopAnnounce(mdnsConn) }) }

	// Keep accepting until the real receiver completes the handshake or the
	// pairing window closes. A malicious LAN host can reach the deterministic
	// port and complete TLS but fail the PAKE (it lacks the code);
	// surrendering the LAN fast path to the first such impostor would be a
	// trivial, repeatable denial of the same-LAN shortcut. Failures count
	// toward the session's cap instead.
	res, err := sess.acceptCounting(ctx, ln)
	stopMDNS()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &lanSenderPairing{
		listener: ln,
		firstRes: res,
		cleanup: func() {
			_ = ln.Close()
			// Closing the listener leaves an accepted conn open (quic-go),
			// so close the paired one too — else the loser in a
			// two-receiver race hangs. Idempotent with the winning
			// transfer's own close.
			res.Close()
		},
	}, nil
}

// waitMaxConsecFails is how many consecutive Wait failures the sender
// tolerates (sleeping 2s·n between attempts) before declaring the server
// down.
const waitMaxConsecFails = 6

// firstAcceptTimeout bounds the first QUIC accept after the receiver
// joins. The receiver's connect ladder (LAN dial → ICE → relay, with
// dial retries) gives up well inside this window, so expiry means the
// receiver is gone — without the bound the sender spins "Waiting for
// receiver" forever.
const firstAcceptTimeout = 60 * time.Second

// sessionDeleteTimeout bounds best-effort session cleanup, so an
// unreachable server can't stall teardown up to the client's full
// request timeout.
const sessionDeleteTimeout = 3 * time.Second

// pairTTL bounds one pairing round, matching the server's unpaired TTL:
// past it the code (or resume key) stops being announced on the LAN and
// re-created on the server. Var so tests can shrink it.
var pairTTL = time.Hour

// serverRetryMax caps the backoff between attempts to reach a down
// server. Var so tests can shrink it.
var serverRetryMax = 30 * time.Second

// errPairedGone marks failures after a receiver claimed the code. The
// LAN race can't recover from these — a receiver that joined via the
// server already failed LAN discovery — so the coordinator aborts
// instead of waiting on a doomed LAN listener.
var errPairedGone = fmt.Errorf("%w: the receiver paired but the connection could not be established", fserrors.ErrConnectFailed)

// pairOverInternet runs the full pairing-server + ICE/relay handshake and
// returns once the receiver has paired and the QUIC SenderHandshake
// over the established data path is up.
//
// It owns the server-side session lifecycle: it Creates a session keyed
// by the rendezvous's argon2id slot (the server never learns the code
// itself), long-polls Wait until a receiver Joins, runs ICE (falling back
// to a server-side relay on failure), then runs the QUIC handshake. The
// cleanup func threaded onto the returned pairing ends the server session
// and releases resources on any teardown path.
//
// onDown, when non-nil, makes the path persistent: an unreachable server
// is reported once through it and retried with capped backoff, and a
// session the server lost (restart, TTL) is re-created. With nil, the
// first such failure is returned (the internet path is the only one).
func pairOverInternet(ctx context.Context, f *flags, sess *sendSession, round *sendRound, cfg *config.Config, onDown func(error)) (*internetSenderPairing, error) {
	client, _, err := signalingClient(cfg)
	if err != nil {
		return nil, err
	}
	for {
		created, err := createWithRetry(ctx, client, round.key.rendezvous, onDown)
		if err != nil {
			return nil, err
		}
		// End the session on every exit. Fresh ctx (the parent is often
		// already cancelled, e.g. the LAN path won) but bounded.
		endSession := func() {
			ctx, cancel := context.WithTimeout(context.Background(), sessionDeleteTimeout)
			defer cancel()
			if round.consumeCode() {
				_ = client.Consume(ctx, created.SessionID, created.RoleToken)
				return
			}
			_ = client.Delete(ctx, created.SessionID, created.RoleToken)
		}

		waitResp, err := waitForReceiver(ctx, client, created.Code, onDown)
		if errors.Is(err, fserrors.ErrSessionExpired) && onDown != nil && ctx.Err() == nil {
			// The server lost the session (restart, deploy, TTL) while we
			// still hold the code: the slot is client-derived, so simply
			// re-create it.
			continue
		}
		if err != nil {
			endSession()
			return nil, err
		}

		pc, pathInfo, err := establishInternetDataPath(ctx, f, client, created, waitResp)
		if err != nil {
			// A spent daily budget is terminal and self-explanatory. Surface
			// it as-is (not the generic errPairedGone), and leave the session
			// for the receiver to reach the relay and learn the same reason.
			if errors.Is(err, fserrors.ErrRelayBudgetExhausted) {
				return nil, err
			}
			endSession()
			if ctx.Err() == nil {
				err = fmt.Errorf("%w: %v", errPairedGone, err)
			}
			return nil, err
		}

		// Bring up QUIC on the established PacketConn and authenticate the
		// first peer. Retries re-Accept on the same listener.
		acceptCtx, cancelAccept := context.WithTimeout(ctx, firstAcceptTimeout)
		ln, res, teardown, err := senderQUICAccept(acceptCtx, sess, pc, round.key.secret)
		cancelAccept()
		if err != nil {
			endSession()
			if ctx.Err() == nil && !errors.Is(err, fserrors.ErrTooManyPairingFailures) {
				err = fmt.Errorf("%w: %v", errPairedGone, err)
			}
			return nil, err
		}

		return &internetSenderPairing{
			listener:  ln,
			firstRes:  res,
			pathInfo:  pathInfo,
			sigClient: client,
			sessionID: created.SessionID,
			roleToken: created.RoleToken,
			cleanup: func() {
				teardown()
				endSession()
			},
		}, nil
	}
}

// createWithRetry creates the session. With onDown set, transient
// failures (unreachable, rate-limited, 5xx, an intercepted TLS session)
// are reported once and retried with capped backoff: a sender started
// right after wake, behind a captive portal, or during a server deploy
// must still become reachable across networks. Auth and retirement
// errors need the user, so they return at once.
func createWithRetry(ctx context.Context, client *signaling.Client, rendezvous string, onDown func(error)) (*signaling.CreateResult, error) {
	wait := min(time.Second, serverRetryMax)
	for {
		created, err := client.Create(ctx, rendezvous)
		if err == nil {
			return created, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if onDown == nil || errors.Is(err, fserrors.ErrServerAuthRequired) ||
			errors.Is(err, fserrors.ErrServerRetired) || errors.Is(err, fserrors.ErrCodeAlreadyClaimed) {
			return nil, err
		}
		onDown(err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, serverRetryMax)
	}
}

// listenLANWithRetry binds the deterministic LAN port, retrying briefly:
// on a re-pair round the previous listener's socket is only released once
// its dead QUIC connection finishes draining, so an immediate re-bind can
// lose that race and needlessly surrender the LAN path to the server.
func listenLANWithRetry(ctx context.Context, port int, secret string) (*quicconn.Listener, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		ln, err := quicconn.ListenAddr(":"+strconv.Itoa(port), secret)
		if err == nil || time.Now().After(deadline) {
			return ln, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// waitRetryStep scales the backoff between Wait retries (n·step for the
// n-th consecutive failure). Var so tests can shrink it.
var waitRetryStep = 2 * time.Second

// waitForReceiver long-polls Wait until the receiver pairs, the user
// cancels, or the server reaps the session. The server's per-call
// long-poll timeout returns nil periodically; we just re-issue. There
// is no client-side deadline here — the round's pairTTL bounds it.
//
// If the server reaps the session (ErrCodeNotFound from Wait), we
// surface ErrSessionExpired; a persistent caller re-creates it.
//
// Anything else is retried with backoff: senders wait minutes, and a
// single dropped poll (wifi roam, laptop sleep/wake) must not abandon a
// session the server still holds. The threshold counts consecutive
// failures, not elapsed time — a wall-clock deadline would expire during
// a long sleep and kill the code on wake. Past it, a nil onDown gives up;
// otherwise onDown hears about it and polling continues at capped backoff.
func waitForReceiver(ctx context.Context, client *signaling.Client, code string, onDown func(error)) (*server.WaitResponse, error) {
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
				if onDown == nil {
					return nil, err
				}
				onDown(err)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(min(time.Duration(consecFails)*waitRetryStep, serverRetryMax)):
			}
		}
	}
}

// establishInternetDataPath wraps the ICE-then-relay ladder. On ICE
// success it returns the ICE-owning PacketConn; on failure it falls back
// to a relay PacketConn. The pathInfo reflects the choice for UX
// rendering. The sender is the ICE controlling side, so it decides the
// path and publishes the decision for the receiver (see iceEstablish).
//
// ICE gathers srflx candidates against the STUN address the server put in
// the create response (the relay socket also answers STUN). The relay slot
// itself is allocated only on ICE failure, when we actually need to relay —
// so a direct connection never reserves one. Old servers that don't
// advertise the address fall back to allocating up front for STUN (see
// stunAddrForICE).
//
// The debug --mode flag short-circuits the ladder:
//   - modeDirect: only ICE; surface the ICE error if it fails (no relay fallback).
//   - modeRelay:  skip ICE entirely; relay only.
func establishInternetDataPath(ctx context.Context, f *flags, client *signaling.Client, created *signaling.CreateResult, waitResp *server.WaitResponse) (net.PacketConn, connpath.Info, error) {
	if f != nil && f.mode == modeRelay {
		publishPathChoice(ctx, client, created.SessionID, created.RoleToken, pathChoiceRelay)
		return allocAndDialRelay(ctx, client, created.SessionID, created.RoleToken)
	}
	// The STUN address rides on the create response, so ICE runs without a
	// relay allocation. We allocate only if ICE fails and we must relay.
	stunAddr := stunAddrForICE(ctx, client, created.RelayAddr, created.SessionID, created.RoleToken)
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
	if created.RelayForwardingDisabled {
		return nil, connpath.Info{}, fmt.Errorf("%w: direct connection failed and this server has relay forwarding disabled", fserrors.ErrConnectFailed)
	}
	return allocAndDialRelay(ctx, client, created.SessionID, created.RoleToken)
}

// stunAddrForICE returns the STUN address for srflx gathering: the one the
// server advertised, else (old servers that don't) an allocation's address.
// On error ICE runs host-only, as when no relay exists.
func stunAddrForICE(ctx context.Context, client *signaling.Client, advertised, sessionID, roleToken string) string {
	if advertised != "" {
		return advertised
	}
	if alloc, err := client.AllocateRelay(ctx, sessionID, roleToken); err == nil {
		return alloc.RelayAddr
	}
	return ""
}

// allocAndDialRelay performs the relay allocation + dial steps. Shared
// between the default ICE-failure fallback and the debug --mode=relay
// short-circuit so both produce identical errors and pathInfo.
func allocAndDialRelay(ctx context.Context, client *signaling.Client, sessionID, roleToken string) (net.PacketConn, connpath.Info, error) {
	alloc, err := client.AllocateRelay(ctx, sessionID, roleToken)
	if err != nil {
		return nil, connpath.Info{}, fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	if alloc.BudgetExhausted {
		return nil, connpath.Info{}, fserrors.ErrRelayBudgetExhausted
	}
	if alloc.ForwardingDisabled {
		return nil, connpath.Info{}, fmt.Errorf("%w: this server has relay forwarding disabled", fserrors.ErrConnectFailed)
	}
	return dialRelay(ctx, client, alloc, sessionID, roleToken)
}

// senderQUICAccept brings up a quic.Transport on the established
// PacketConn, authenticates the first inbound QUIC connection, and
// returns the listener (for retries), the first AcceptResult, and a
// teardown closure.
func senderQUICAccept(ctx context.Context, sess *sendSession, pc net.PacketConn, secret string) (*quicconn.Listener, *quicconn.AcceptResult, func(), error) {
	tlsCfg, err := quicconn.SenderTLSConfig()
	if err != nil {
		_ = pc.Close()
		return nil, nil, nil, err
	}
	tr := quicconn.NewTransport(pc)
	qln, err := tr.Listen(tlsCfg, quicconn.QuicConfig())
	if err != nil {
		_ = tr.Close()
		_ = pc.Close()
		return nil, nil, nil, fmt.Errorf("QUIC listen: %w", err)
	}
	ln := quicconn.NewListener(qln, secret)
	teardown := func() {
		_ = ln.Close()
		_ = tr.Close()
		_ = pc.Close()
	}
	// One attempt: a peer that joined this session and failed the PAKE is
	// not going to start passing it.
	res, err := ln.Accept(ctx)
	if err != nil {
		if capErr := sess.authFailed(err); capErr != nil {
			err = capErr
		}
		teardown()
		return nil, nil, nil, fmt.Errorf("QUIC accept: %w", err)
	}
	return ln, res, teardown, nil
}

// runSendParallel pairs and transfers, returning to "Waiting for
// receiver" when the receiver vanishes mid-transfer. After the first
// pairing, rounds rendezvous on the resume key, so only the original
// receiver device can come back — a rerun `fsend <code>` there finds the
// sender and resumes from its .fsend-partial.
//
// Stdin/--text transfers can't replay their reader, so they fail as
// before instead of re-pairing.
func runSendParallel(ctx context.Context, f *flags, plan *sendPlan, code string, cfg *config.Config, waitSpin *uxlog.Spinner) error {
	sess := &sendSession{key: codeKey(code)}
	transferDropped := false
	pwMisses := 0
	for {
		err := runSendOnce(ctx, f, plan, sess, cfg, waitSpin)
		// A receiver who ran out of password tries (or had none) can rerun
		// with the right one: keep the code alive instead of ending the
		// session. No data was read yet, so even a stdin stream can wait.
		// Bounded, so a leaked code can't buy unlimited password guesses.
		if ctx.Err() == nil && pwMisses < maxPasswordRounds-1 &&
			(errors.Is(err, fserrors.ErrWrongPassword) || errors.Is(err, fserrors.ErrPasswordRequired)) {
			pwMisses++
			if !f.quiet {
				fmt.Fprintf(os.Stderr, "%s %s — still waiting; they can rerun %s with the right password.\n",
					uxlog.Warn(), passwordMissNotice(err), uxlog.Code("fsend "+code))
			}
			waitSpin = startWaitSpinner(f, "Waiting for receiver")
			continue
		}
		if err == nil || ctx.Err() != nil || plan.consumable() ||
			(!errors.Is(err, fserrors.ErrTransientFailure) && !isReceiverClose(err)) {
			// A re-pair round failing with "server unreachable" after a
			// transfer already dropped is a mid-transfer connection loss,
			// not a pairing failure: E001's copy suggests switching servers
			// and omits that the receiver's partial is preserved. E020's
			// resume framing is what actually happened.
			if transferDropped && errors.Is(err, fserrors.ErrServerUnreachable) {
				return fmt.Errorf("%w: %v", fserrors.ErrTransientFailure, err)
			}
			return err
		}
		transferDropped = true
		if !f.quiet {
			// A deliberate close (Ctrl-C, clean exit) may genuinely re-run
			// and reconnect. A death (kill, crash, network gone) surfaces
			// as exhausted retries; the receiver may still recover on its
			// own (network change) or be rerun on the same device. The code
			// box has long scrolled away, so the line repeats the code.
			fmt.Fprintf(os.Stderr, "%s %s\n", uxlog.Info(), dropNotice(isReceiverClose(err), code))
		}
		waitSpin = startWaitSpinner(f, "Waiting for receiver")
	}
}

// isReceiverClose reports whether err is the receiver deliberately
// closing its QUIC connection (Ctrl-C, clean process exit). That
// receiver will never re-dial the current pairing, so the re-accept
// grace would be a dead wait. Network drops and kills surface as idle
// timeouts instead and keep the grace.
//
// A remote stream cancel is the same close seen earlier: the receiver's
// teardown cancels its data-stream read (STOP_SENDING) just before the
// connection close, and an in-flight chunk write can surface the stream
// error before the connection-level one.
func isReceiverClose(err error) bool {
	var appErr *quic.ApplicationError
	if errors.As(err, &appErr) && appErr.Remote {
		return true
	}
	var streamErr *quic.StreamError
	return errors.As(err, &streamErr) && streamErr.Remote
}

// runSendOnce runs one pair-then-transfer round. It races the two pair
// paths, picks the first that succeeds, cancels the loser, and runs the
// transfer on the winner.
//
// Failure handling is deliberately asymmetric:
//   - LAN-only failure (e.g. port conflict): keep waiting for the server
//     to pair; surface an ℹ line so the user knows internet is the only
//     remaining path.
//   - Server down: keep waiting for LAN while the server path retries in
//     the background; surface a ⚠ line so the user knows only same-LAN
//     receivers can connect for now.
//   - Both fail: return the most informative error (E001 if the server
//     was unreachable, otherwise the LAN error).
//   - Too many failed PAKEs, or a receiver that paired and vanished:
//     abort the round.
func runSendOnce(ctx context.Context, f *flags, plan *sendPlan, sess *sendSession, cfg *config.Config, waitSpin *uxlog.Spinner) error {
	pairCtx, cancelPair := context.WithCancel(ctx)
	defer cancelPair()
	// Belt-and-braces: if we exit through an unusual path (panic-recover,
	// ctx cancel before any branch), make sure the spinner goroutine isn't
	// left scribbling on the screen. waitSpin gets reassigned on notice
	// swaps, so the defer must close over the variable, not its value at
	// defer-time.
	defer func() { waitSpin.Stop() }()

	key, resumed := sess.current()
	round := &sendRound{key: key, resume: resumed}

	lanCh := make(chan sendPairOutcome, 1)
	serverCh := make(chan sendPairOutcome, 1)
	serverDownCh := make(chan error, 1)

	// --mode collapses the LAN+internet race to a single path. The
	// disabled side is marked done upfront and its goroutine never starts,
	// so the coordinator only ever picks a winner from the enabled side.
	lanDisabled := f.mode == modeDirect || f.mode == modeRelay
	serverDisabled := f.mode == modeLocal

	if !lanDisabled {
		go func() {
			p, err := pairOverLAN(pairCtx, sess, key)
			lanCh <- sendPairOutcome{lan: p, err: err}
		}()
	}
	if !serverDisabled {
		// The server path only persists while LAN can still pair.
		var onDown func(error)
		if !lanDisabled {
			onDown = func(err error) {
				select {
				case serverDownCh <- err:
				default:
				}
			}
		}
		go func() {
			p, err := pairOverInternet(pairCtx, f, sess, round, cfg, onDown)
			serverCh <- sendPairOutcome{server: p, err: err}
		}()
	}

	ttl := time.NewTimer(pairTTL)
	defer ttl.Stop()

	var (
		lanDone, serverDone bool
		lanErr, serverErr   error
		serverDownErr       error
		lanDownNoticed      bool
		winner              sendPairOutcome
		winnerPicked        bool
	)
	lanDone = lanDisabled
	serverDone = serverDisabled

	// abort ends the round with err, releasing whatever the paths set up.
	abort := func(err error) error {
		waitSpin.Stop()
		cancelPair()
		drainBoth(lanCh, serverCh, lanDone, serverDone)
		return err
	}
	// noticeServerDown prints the LAN-only warning once. The notice names
	// the cause when the sender can act on it (password, rate limit).
	// Skipped under --mode=direct/relay: LAN is disabled, so the notice
	// would be misleading. No E-coded line here: if the LAN path also
	// fails, the final renderError prints the catalog entry.
	noticeServerDown := func(err error) {
		if f.debug {
			fmt.Fprintln(os.Stderr, "DEBUG: server pairing failed:", err)
		}
		if serverDownErr != nil || f.quiet || lanDisabled {
			serverDownErr = err
			return
		}
		serverDownErr = err
		waitSpin.Stop()
		fmt.Fprintln(os.Stderr, uxlog.Warn(), serverPairNotice(err))
		waitSpin = startWaitSpinner(f, "Waiting for receiver on local network")
	}

	for !winnerPicked && (!lanDone || !serverDone) {
		select {
		case <-ctx.Done():
			return abort(ctx.Err())

		case <-ttl.C:
			// Stop announcing a code nobody redeemed within the server's TTL.
			return abort(fserrors.ErrSessionExpired)

		case err := <-serverDownCh:
			noticeServerDown(err)
			// The server path keeps retrying, but if LAN already failed
			// nothing can pair until it recovers: report now.
			if lanDone && lanErr != nil && !errors.Is(lanErr, context.Canceled) {
				return abort(pickFinalSendError(lanErr, err))
			}

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
			if errors.Is(lanErr, fserrors.ErrTooManyPairingFailures) {
				return abort(lanErr)
			}
			if serverDownErr != nil {
				return abort(pickFinalSendError(lanErr, serverDownErr))
			}
			// Only the notice when there's another path left (not under
			// --mode=local). Same-LAN receivers can still pair through
			// the server (host↔host ICE), so this describes the loss of
			// the discovery shortcut, not a reachability verdict.
			if !lanDownNoticed && !f.quiet && !serverDisabled {
				lanDownNoticed = true
				// Pause the spinner so the notice prints on a clean
				// line, then restart it for the remaining (server) path.
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
			if errors.Is(serverErr, errPairedGone) || errors.Is(serverErr, fserrors.ErrTooManyPairingFailures) {
				return abort(serverErr)
			}
			// Any other server-path failure (auth, retired) leaves the code
			// working on the local network only — say so, or the sender
			// waits forever on a code cross-network receivers can't redeem.
			noticeServerDown(serverErr)
		}
	}

	if !winnerPicked {
		waitSpin.Stop()
		return pickFinalSendError(lanErr, serverErr)
	}

	// Switch to the resume key before any byte moves, and before the loser
	// tears down: the loser's server session is then consumed (tombstoned)
	// rather than deleted, and in-transfer re-dials must use the new secret.
	round.paired.Store(true)
	newKey := sess.paired(winner.firstRes().Streams.Binding)
	winner.listener().SetSecret(newKey.secret)

	cancelPair()
	// Tear the loser down in the background — its server cleanup can take
	// seconds — but finish it before returning so a short transfer can't
	// exit with the tombstone unwritten.
	var loserDone sync.WaitGroup
	loserDone.Add(1)
	go func() {
		defer loserDone.Done()
		drainLoser(lanCh, serverCh, winner, lanDone, serverDone)
	}()
	defer loserDone.Wait()
	// Stop the wait spinner now so the connected line and progress bar
	// land on a clean line. The deferred Stop above is idempotent.
	waitSpin.Stop()

	pathInfo := connpath.FromLAN()
	if winner.lan == nil {
		pathInfo = winner.server.pathInfo
	}
	printPath(f, pathInfo)
	// Without this line the sender stares at a dead terminal wondering
	// whether the code even arrived. The path rides along as a dim chip —
	// same shape as the receiver's "Incoming from <peer>  ·  <chip>" — so
	// both sides see the route before any bytes flow. ✓ even for relay:
	// the glyph reports the connect succeeding, the chip carries the
	// route. The accept wait itself is a spinner, started in
	// runSenderTransferLoop.
	if !f.quiet {
		fmt.Fprintf(os.Stderr, "%s Receiver connected%s\n",
			uxlog.Check(), "  ·  "+pathInfo.Chip())
	}

	if winner.lan != nil {
		return runSenderTransferOverLAN(ctx, f, plan, sess, winner.lan)
	}
	return runSenderTransferOverInternet(ctx, f, plan, sess, winner.server)
}

// startWaitSpinner returns a quiet-aware spinner. In --quiet mode we
// emit no animation and return nil; Stop is nil-safe so call sites stay
// free of branches.
func startWaitSpinner(f *flags, msg string) *uxlog.Spinner {
	if f.quiet {
		return nil
	}
	// Every wait advertises its escape hatch: a sender staring at a dead
	// terminal should know Ctrl-C is the way out.
	return uxlog.StartSpinner(msg, uxlog.HintCancel)
}

// reacceptTimeout bounds a mid-transfer re-Accept: if the receiver exited
// terminally, an unbounded Accept would hang "retrying…" until Ctrl-C
// instead of exhausting the retry budget.
const reacceptTimeout = 15 * time.Second

// runSenderTransferOverLAN runs the transfer protocol on a paired LAN
// connection. The first attempt uses the AcceptResult captured at pair
// time; transient mid-transfer errors trigger a retry that re-Accepts
// on the same listener (the receiver's own retry loop re-Dials).
func runSenderTransferOverLAN(ctx context.Context, f *flags, plan *sendPlan, sess *sendSession, pair *lanSenderPairing) error {
	defer pair.cleanup()
	return runSenderTransferLoop(ctx, f, plan, connpath.FromLAN(), pair.firstRes, func(ctx context.Context) (*quicconn.AcceptResult, error) {
		acceptCtx, cancel := context.WithTimeout(ctx, reacceptTimeout)
		defer cancel()
		res, err := sess.acceptCounting(acceptCtx, pair.listener)
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
func runSenderTransferOverInternet(ctx context.Context, f *flags, plan *sendPlan, sess *sendSession, pair *internetSenderPairing) error {
	defer pair.cleanup()
	err := runSenderTransferLoop(ctx, f, plan, pair.pathInfo, pair.firstRes, func(ctx context.Context) (*quicconn.AcceptResult, error) {
		acceptCtx, cancel := context.WithTimeout(ctx, reacceptTimeout)
		defer cancel()
		res, err := sess.acceptCounting(acceptCtx, pair.listener)
		if err != nil {
			return nil, fmt.Errorf("QUIC accept: %w", err)
		}
		return res, nil
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
func runSenderTransferLoop(ctx context.Context, f *flags, plan *sendPlan, pathInfo connpath.Info, firstRes *quicconn.AcceptResult, reaccept func(context.Context) (*quicconn.AcceptResult, error)) error {
	closeProg, progressFn, onResume, onSkip, stats, onStreamingEOF, resetCounts := newSenderProgress(f, plan, pathInfo)
	defer closeProg()

	// The receiver may sit at its accept prompt for a while; the spinner
	// owns that window. The first event off the wire — a byte, a resume
	// notice, a skip, a retry — stops it so the progress bar (or notice
	// line) lands on a clean row. Stop is idempotent and nil-safe (--quiet).
	spin := startWaitSpinner(f, "Waiting for them to accept")
	defer func() { spin.Stop() }() // spin is swapped once the receiver answers
	peer := func(host string, accepted bool) {
		spin.Stop()
		onPeer(f, host, accepted)
		if accepted && f.passArg != "" {
			spin = startWaitSpinner(f, "Waiting for "+sendState.peerName()+" to enter the password")
		}
	}

	// Time from the first byte, not from here — the receiver may sit at
	// its accept prompt for a while, and counting that wait makes the
	// summary read like a glacial transfer.
	start := time.Now()
	var firstByte time.Time
	progress := func(fi uint32, b uint64) {
		if firstByte.IsZero() {
			firstByte = time.Now()
			spin.Stop()
		}
		progressFn(fi, b)
	}
	resume := func(fi uint32, offset, total uint64) {
		spin.Stop()
		onResume(fi, offset, total)
	}
	// An all-skipped transfer (receiver had everything) emits no bytes, so
	// the skip is the only event that stops the accept spinner.
	skip := func(fi uint32, kept bool) {
		spin.Stop()
		onSkip(fi, kept)
	}
	unreadable, onUnreadable, onChanged := sourceIssueHooks(f, plan)
	current := firstRes
	opts := retry.Options{OnRetry: retryNoticeFor(f)}
	if notice := opts.OnRetry; notice != nil {
		opts.OnRetry = func(attempt int, wait time.Duration, lastErr error) {
			spin.Stop()
			notice(attempt, wait, lastErr)
		}
	}
	classify := retry.IsTransient
	if plan.consumable() {
		// A partially-consumed stdin/--text reader would resend from an
		// arbitrary offset and still pass per-chunk hashes; fail instead.
		opts.Attempts = 1
	} else {
		// A deliberate receiver close means no re-dial is coming: bail
		// past the re-accept grace so runSendParallel re-pairs now. The
		// raw error never reaches the user — the re-pair loop always
		// catches it (re-pairable items only).
		classify = func(err error) bool { return !isReceiverClose(err) && retry.IsTransient(err) }
	}
	err := retry.WithBackoff(ctx, opts, classify,
		func(attempt int) error {
			if attempt > 1 {
				resetCounts()
			}
			if current == nil {
				res, err := reaccept(ctx)
				if err != nil {
					return err
				}
				current = res
			}
			res := current
			current = nil
			// A Ctrl-C closes with the cancel code, so the receiver reports
			// E032 at once instead of retrying a sender that is gone.
			defer func() {
				if ctx.Err() != nil {
					res.Cancel()
					return
				}
				res.Close()
			}()
			return transfer.Send(ctx, &res.Streams, transfer.SendOptions{
				Hostname:        hostnameOrDefault(f.hostname),
				OS:              runtime.GOOS,
				ClientVersion:   version.Version,
				Mode:            plan.mode,
				Sources:         plan.sources,
				Stream:          plan.stream,
				IsText:          plan.isText,
				DisplayName:     plan.displayName,
				Password:        f.passArg,
				ProgressFn:      progress,
				OnResume:        resume,
				OnSkip:          skip,
				OnStreamingEOF:  onStreamingEOF,
				OnPeer:          peer,
				OnSourceSkipped: onUnreadable,
				OnSourceChanged: onChanged,
			})
		})
	if err != nil {
		return err
	}
	// Flush the bar first so its terminal frame lands above the summary
	// (the deferred call is then a no-op).
	closeProg()
	elapsed := time.Since(start)
	if !firstByte.IsZero() {
		elapsed = time.Since(firstByte)
	}
	// The summary shows the offered total (agreeing with the pre-transfer
	// headline) with moved as the "(X sent)" clause, so any partial send —
	// down to a full skip ("0 B sent") — reconciles instead of silently
	// dropping to the sent bytes. The skipped-file count explains the gap and
	// reconciles with the receiver's breakdown. printSend* no-op under --quiet.
	s := stats()
	printSendSummary(f, sendSubject(plan), int64(plan.totalBytes), s, elapsed, pathInfo)
	ev := jsonDoneEvent{Ok: true, Role: "sender",
		BytesMoved: ptr64(s.moved), DurationMS: msPtr(elapsed), Route: jsonRoute(pathInfo.Kind)}
	// A piped stdin stream has no known size; per the JSON contract an omitted
	// bytes_total means "not known", so don't emit a bogus 0.
	if plan.mode != wire.ModeStream || plan.isText {
		ev.BytesTotal = ptr64(int64(plan.totalBytes))
	}
	if plan.mode == wire.ModeFiles {
		ev.FilesSent = ptrInt(plan.totalFiles - s.skippedFiles)
		ev.FilesSkipped = ptrInt(s.skippedFiles - s.keptFiles)
		ev.FilesKept = ptrInt(s.keptFiles)
	}
	// Unreadable files were skipped; everything else arrived. Say which, and
	// exit E050 so scripts notice.
	var retErr error
	if n := len(unreadable()); n > 0 {
		printSkippedList(f, "could not be read and", unreadable())
		retErr = fmt.Errorf("%w: %d unreadable on the sender", fserrors.ErrFilesSkipped, n)
		entry, _ := fserrors.Lookup(retErr)
		ev.Ok, ev.Error, ev.Exit = false, entry.Code, entry.Exit
	}
	jsonEmitDone(ev)
	return retErr
}

// isServerDown reports whether an error is the "pairing server is
// unreachable" flavor — used to decide whether to surface the
// "only same-LAN receivers can connect" warning vs treating it as a
// transfer-side failure.
func isServerDown(err error) bool {
	return errors.Is(err, fserrors.ErrServerUnreachable)
}

// serverPairNotice renders the mid-wait warning for a failed server
// path. 401 and 429 are the sender's own problem to fix — name them;
// everything else (unreachable, 5xx) stays the generic "unavailable".
func serverPairNotice(err error) string {
	const lanOnly = "only receivers on your local network can connect."
	switch {
	case errors.Is(err, fserrors.ErrServerAuthRequired):
		return "The server requires a password (fsend --connect <host:port>,<password>) — " + lanOnly
	case errors.Is(err, fserrors.ErrRateLimited):
		return "The server rate-limited this device; retrying — until then " + lanOnly
	case errors.Is(err, fserrors.ErrServerCertUntrusted):
		return "The server's certificate isn't trusted (a Wi-Fi login page?); retrying — until then " + lanOnly
	default:
		return "Server unavailable; retrying — until then " + lanOnly
	}
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
