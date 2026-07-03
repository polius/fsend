package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/iceconn"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/retry"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/version"
)

// iceBudget caps the total time we'll spend trying to establish a direct
// path before falling back to the relay. With srflx candidates (STUN via
// the relay socket), a viable direct path completes within ~2s worldwide:
// gather, one signaling poll (200 ms), and connectivity checks are all
// RTT-scale. This bounds the *unviable* cases (symmetric NAT, filtered
// UDP) — paths that need the relay regardless — so generosity here only
// delays the inevitable fallback.
const iceBudget = 5 * time.Second

// signalingClient builds a client pointed at the configured server.
//
// HTTPS is the default for non-local addresses; HTTP for localhost so dev
// loops work without a cert. If the user has configured a per-server
// password (via `fsend --connect <host:port>,<password>`), the client
// carries it through; the server matches against FSEND_SERVER_PASSWORD.
func signalingClient(cfg *config.Config) (*signaling.Client, string) {
	addr := cfg.EffectiveServer()
	baseURL := addr
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		if isLocalAddr(addr) {
			baseURL = "http://" + addr
		} else {
			baseURL = "https://" + addr
		}
	}
	return signaling.New(baseURL, version.Version).WithPassword(cfg.ServerPassword), addr
}

func isLocalAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	return h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// joinRetryBudget caps how long the receiver waits for the sender to
// register the code on the pairing server. With the sender's short
// LAN-only window (~5s) plus a Create round-trip, the sender should
// always be registered within a couple of seconds of starting. But the
// human is in the loop — the receiver may have typed the code while
// the sender is still in its LAN window, so we give Join a budget to
// outlast that race instead of failing instantly with E002.
//
// Var (not const) so tests can shrink it.
var joinRetryBudget = 15 * time.Second

// runReceiveOverInternet performs the cross-internet receive flow.
//
// Mirror of runSendOverInternet: try ICE direct first (receiver =
// controlled), fall back to relay on failure.
//
// connSpin, when non-nil, is the "Connecting" spinner the caller
// started. We keep it animating through Join + ICE/relay setup and stop
// it just before printPath — the first user-visible status line. This
// replaces what used to be a sequence of brief spinner flashes that read
// as glitchy. The deferred Stop covers error returns; Stop is idempotent
// (sync.Once), so explicit stops before printPath don't double-close.
func runReceiveOverInternet(ctx context.Context, f *flags, c string, cfg *config.Config, connSpin *uxlog.Spinner) error {
	client, _ := signalingClient(cfg)
	defer connSpin.Stop()

	joined, err := joinWithRetry(ctx, client, c, f, connSpin)
	if err != nil {
		return err
	}

	// STUN address rides on the join response; no relay allocation up front.
	stunAddr := stunAddrForICE(ctx, client, joined.RelayAddr, joined.SessionID, joined.RoleToken)

	// --- Try ICE direct path ---
	iceConn, icePath, iceErr := iceEstablish(ctx, client, joined.SessionID, joined.RoleToken, iceconn.Options{
		LocalUfrag:  joined.YourIceCredentials.Ufrag,
		LocalPwd:    joined.YourIceCredentials.Pwd,
		RemoteUfrag: joined.PeerIceCredentials.Ufrag,
		RemotePwd:   joined.PeerIceCredentials.Pwd,
		STUNAddr:    stunAddr,
	}, false /* controlled */, f.debug)
	if iceErr == nil {
		defer func() { _ = iceConn.Close() }()
		// Stop the spinner; the receive UX is owned by runReceiverQUICOver
		// from here (no standalone path line — the prompt block carries
		// path info as a chip and the summary names it again).
		connSpin.Stop()
		return runReceiverQUICOver(ctx, f, iceConn, c, icePath)
	}
	if f.debug {
		fmt.Fprintln(os.Stderr, "DEBUG: ICE failed:", iceErr)
	}

	// --- Fall back to relay: allocate now that we know we need it ---
	if joined.RelayForwardingDisabled {
		return fmt.Errorf("%w: direct connection failed and this server has relay forwarding disabled", fserrors.ErrConnectFailed)
	}
	alloc, allocErr := client.AllocateRelay(ctx, joined.SessionID, joined.RoleToken)
	if allocErr != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, allocErr)
	}
	if alloc.BudgetExhausted {
		return fserrors.ErrRelayBudgetExhausted
	}
	if alloc.ForwardingDisabled {
		return fmt.Errorf("%w: direct connection failed and this server has relay forwarding disabled", fserrors.ErrConnectFailed)
	}
	relayConn, err := dialRelay(alloc)
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	defer func() { _ = relayConn.Close() }()
	connSpin.Stop()
	return classifyRelayDrop(ctx, client, joined.SessionID, joined.RoleToken,
		runReceiverQUICOver(ctx, f, relayConn, c, connpath.FromRelay(alloc.RelayAddr)))
}

// classifyRelayDrop probes the relay-status endpoint when a relay-path
// transfer ends in a transient-looking error. If the relay tells us
// the allocation was evicted for a known reason, we promote the error
// to the corresponding non-transient sentinel so the user sees what
// actually happened instead of a "retrying…" loop.
func classifyRelayDrop(ctx context.Context, client *signaling.Client, sessionID, roleToken string, runErr error) error {
	if runErr == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := client.RelayStatus(probeCtx, sessionID, roleToken)
	if err != nil || status == nil || status.State != "evicted" {
		return runErr
	}
	switch status.Reason {
	case relay.ReasonCapHit:
		if status.LimitBytes > 0 {
			return fmt.Errorf("%w: Server limit: %s on the wire (after compression)",
				fserrors.ErrRelayCapHit, uxlog.HumanBytes(int64(status.LimitBytes)))
		}
		return fserrors.ErrRelayCapHit
	case relay.ReasonBudgetHit:
		return fserrors.ErrRelayBudgetExhausted
	case relay.ReasonIdle:
		return fserrors.ErrRelayIdleTimeout
	}
	return runErr
}

// joinWithRetry calls Join, retrying on ErrCodeNotFound and
// ErrCodeAlreadyClaimed. Not-found covers the receiver winning the race
// against the sender's Create (slow human, laggy network) — without
// retries that surfaced as an instant, misleading E002. Already-claimed
// covers a stale session whose receiver died mid-transfer: the sender
// Deletes and re-Creates it when it re-enters pairing, so within the
// budget the slot flips from claimed to joinable.
//
// Other errors propagate immediately: server-unreachable,
// ctx-cancelled, etc., are not transient and the caller should hear
// about them on the first try.
//
// Every join — including a 404 — counts against the server's per-IP
// new-session budget (that's what stops code-space probing), so the
// schedule is deliberately sparse: ~7 joins across the budget, not a
// tight poll. And if the server starts throttling us mid-loop after
// retryable answers, the honest error is the last retryable one (E002/
// E003), not E017 — a mistyped code must not surface as "too many
// attempts".
func joinWithRetry(ctx context.Context, client *signaling.Client, code string, f *flags, existing *uxlog.Spinner) (*server.JoinSessionResponse, error) {
	// The retry window can run the full budget (~15 s); a spinner stuck on
	// the caller's "Connecting" for that long reads as a network problem.
	// Say what we're actually doing — holding for the sender's Create.
	const waitMsg = "Waiting for the sender to publish this code"

	deadline := time.Now().Add(joinRetryBudget)
	delay := 500 * time.Millisecond
	var lastRetryable error
	var spin *uxlog.Spinner
	retitled := false
	// Close over the variable so a spinner started inside the loop is
	// still Stopped on return — a plain `defer spin.Stop()` would only
	// stop the nil pointer captured at defer-time.
	defer func() { spin.Stop() }()
	for {
		joined, err := client.Join(ctx, code)
		if err == nil {
			if retitled {
				// The caller's spinner keeps running through ICE/relay setup;
				// hand it back with its original label.
				existing.SetMessage("Connecting")
			}
			return joined, nil
		}
		if lastRetryable != nil && errors.Is(err, fserrors.ErrRateLimited) {
			return nil, lastRetryable
		}
		retryable := errors.Is(err, fserrors.ErrCodeNotFound) || errors.Is(err, fserrors.ErrCodeAlreadyClaimed)
		if !retryable || time.Now().After(deadline) {
			return nil, err
		}
		lastRetryable = err
		// Animate a single line for the duration of the wait so the user
		// knows we're holding for the sender rather than stuck. A caller-
		// owned spinner is retitled in place — starting a second one would
		// fight it on stderr.
		if !retitled && !f.quiet {
			if existing != nil {
				existing.SetMessage(waitMsg)
			} else if spin == nil {
				spin = uxlog.StartSpinner(waitMsg)
			}
			retitled = existing != nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 4*time.Second {
			delay *= 2
		}
	}
}

// iceEstablish runs the full ICE handshake: starts an agent, pumps local
// candidates out via signaling, pulls remote candidates in via signaling,
// and drives Dial/Accept under a single iceBudget timeout.
//
// Returns a net.PacketConn ready for quic.Transport, plus a classified
// connpath.Info derived from the selected candidate pair, on success. On
// failure, the agent is closed and the caller falls back to relay.
//
// controlling=true → sender role (calls Dial).
// controlling=false → receiver role (calls Accept).
//
// debug enables candidate-level stderr logging — the data needed to
// diagnose "why did this pair of networks relay?" reports.
func iceEstablish(parent context.Context, sig *signaling.Client, sessionID, roleToken string, opts iceconn.Options, controlling bool, debug bool) (net.PacketConn, connpath.Info, error) {
	debugf := func(format string, args ...any) {
		if debug {
			fmt.Fprintf(os.Stderr, "DEBUG: "+format+"\n", args...)
		}
	}
	ctx, cancel := context.WithTimeout(parent, iceBudget)
	defer cancel()

	agent, err := iceconn.New(opts)
	if err != nil {
		return nil, connpath.Info{}, fmt.Errorf("agent: %w", err)
	}

	// Pump local candidates out and remote candidates in. The signaling
	// server's push/pull endpoints route candidates between peers; see
	// internal/server.{pushCandidates,pullCandidates}.
	pumpCtx, pumpCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Push each gathered local candidate one-shot. The select on
		// pumpCtx is load-bearing: if ICE times out before pion finishes
		// gathering (no STUN host on loopback, no candidates beyond the
		// initial host set), agent.LocalCandidates never closes — a bare
		// `for range` would block here, wg.Wait() would hang waiting for
		// it, and the deferred agent.Close() (which closes the channel)
		// would never run. Closing the loop on pumpCtx breaks that cycle.
		for {
			select {
			case <-pumpCtx.Done():
				return
			case cstr, ok := <-agent.LocalCandidates():
				if !ok {
					return
				}
				debugf("ICE local candidate: %s", cstr)
				if err := sig.PushCandidates(pumpCtx, sessionID, roleToken, []string{cstr}); err != nil {
					// Best-effort: pion's ICE will keep going with whatever
					// candidates have already crossed.
					debugf("ICE push candidate failed: %v", err)
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		since := 0
		// Poll every 200ms; the server's GET is 204 when there's nothing
		// new, so this is cheap.
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-pumpCtx.Done():
				return
			case <-t.C:
			}
			resp, err := sig.PullCandidates(pumpCtx, sessionID, roleToken, since)
			if err != nil || resp == nil {
				continue
			}
			for _, cstr := range resp.Candidates {
				debugf("ICE remote candidate: %s", cstr)
				_ = agent.AddRemoteCandidate(cstr)
			}
			since = resp.NextSince
		}
	}()

	// Drive Dial or Accept under the ICE budget.
	var (
		conn    net.PacketConn
		dialErr error
	)
	if controlling {
		conn, dialErr = agent.Dial(ctx)
	} else {
		conn, dialErr = agent.Accept(ctx)
	}

	pumpCancel()
	wg.Wait()

	if dialErr != nil {
		_ = agent.Close()
		return nil, connpath.Info{}, dialErr
	}
	// Classify the path from the selected candidate pair. If pion didn't
	// surface a pair (shouldn't happen post Dial/Accept, but guard anyway),
	// treat ICE as failed and let the caller fall through to relay — we
	// won't fabricate a "direct" badge we can't verify.
	localCT, remoteCT, ok := agent.SelectedPair()
	if !ok {
		_ = agent.Close()
		return nil, connpath.Info{}, fmt.Errorf("ice: no selected candidate pair")
	}
	return &iceOwningConn{PacketConn: conn, agent: agent}, connpath.FromICE(localCT, remoteCT), nil
}

// iceOwningConn keeps the ICE Agent alive for the lifetime of the
// net.PacketConn (so the agent's gather sockets aren't reaped while QUIC
// is still using them) and closes the agent when the conn is closed.
type iceOwningConn struct {
	net.PacketConn
	agent *iceconn.Agent
}

func (c *iceOwningConn) Close() error {
	err := c.PacketConn.Close()
	if aerr := c.agent.Close(); err == nil {
		err = aerr
	}
	return err
}

// dialRelay wires up a relay.Conn for QUIC. Mirrors the original
// internet.go relay path; factored out so both sender and receiver code
// can share it.
func dialRelay(alloc *server.RelayAllocateResponse) (net.PacketConn, error) {
	myUDP, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("local UDP: %w", err)
	}
	relayUDPAddr, err := net.ResolveUDPAddr("udp", alloc.RelayAddr)
	if err != nil {
		_ = myUDP.Close()
		return nil, fmt.Errorf("resolving relay: %w", err)
	}
	tok, err := relay.ParseToken(alloc.SessionToken)
	if err != nil {
		_ = myUDP.Close()
		return nil, fmt.Errorf("parsing session token: %w", err)
	}
	rc := relay.NewClient(myUDP, relayUDPAddr, tok)
	if _, err := rc.WriteTo([]byte{0}, nil); err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("relay bootstrap: %w", err)
	}
	// Resend the bootstrap until the relay forwards us a datagram: a single
	// lost one would strand the passive sender (it only waits to be dialed).
	rc.KeepBootstrapping(500*time.Millisecond, 20*time.Second)
	return rc, nil
}

// runReceiverQUICOver runs the receiver's QUIC + transfer flow over an
// already-established net.PacketConn.
//
// Symmetric retry: a transient error tears down the current QUIC
// session, sleeps, and re-Dials on the same PacketConn. The receiver's
// .fsend-partial sidecar plus its imohash fingerprint let the next
// attempt resume mid-file — the sender verifies the prefix, seeks past
// it, and streams the remainder.
func runReceiverQUICOver(ctx context.Context, f *flags, pc net.PacketConn, code string, pathInfo connpath.Info) error {
	tr := quicconn.NewTransport(pc)
	defer func() { _ = tr.Close() }()

	outDir, sink, err := resolveOutDir(f)
	if err != nil {
		return err
	}
	ui := newReceiverUI(ctx, f, outDir, sink, pathInfo)
	defer ui.close()

	start := time.Now()
	// Sink mode gets one attempt: emitted bytes can't be reconciled, so
	// a retry would duplicate output.
	opts := retry.Options{OnRetry: retryNoticeFor(f)}
	if sink {
		opts.Attempts = 1
	}
	if err := retry.WithBackoff(ctx, opts, nil,
		func(attempt int) error {
			return runReceiverOneAttempt(ctx, tr, f, ui, code)
		}); err != nil {
		// A Ctrl-C at an interactive prompt cancels ctx but surfaces as a
		// decline/target-exists error; report it as a cancellation (E026).
		if ctx.Err() != nil {
			printCancelKeptHint(f, ui)
			return ctx.Err()
		}
		return err
	}
	return finishReceive(f, ui, time.Since(start))
}

// runReceiverOneAttempt is one Dial → handshake → transfer iteration.
// Mirror of runSenderOneAttempt; returns nil on success or an error for
// the retry layer to classify.
func runReceiverOneAttempt(ctx context.Context, tr *quic.Transport, f *flags, ui *receiverUI, code string) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	defer dialCancel()
	// The PacketConn ignores the dial address (both relay.Conn and our
	// iceOwningConn route via fixed underlying paths). quic-go uses the
	// address as a per-peer routing tag — a stable synthetic value
	// suffices. See internal/relay.peerSyntheticAddr.
	dialAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 1}
	qc, err := tr.Dial(dialCtx, dialAddr, quicconn.ReceiverTLSConfig(), quicconn.QuicConfig())
	if err != nil {
		return fmt.Errorf("QUIC dial: %w", err)
	}
	res, err := quicconn.ReceiverHandshake(ctx, qc, code)
	if err != nil {
		return err
	}
	defer res.Close()

	return transfer.Recv(ctx, &res.Streams, ui.recvOptions(hostnameOrDefault(f.hostname)))
}

// retryNoticeFor returns an OnRetry callback that prints the standard
// "retrying" notice to stderr. Returns nil when --quiet is set so we
// don't break pipeline-mode output.
//
// The default rendering keeps the technical cause out of the user's
// face — most "idle timeout"/"connection reset" strings are noise to
// anyone not debugging. Pass --debug to surface the underlying error
// in parentheses for bug reports.
func retryNoticeFor(f *flags) func(attempt int, wait time.Duration, lastErr error) {
	if f.quiet {
		return nil
	}
	// uxlog.Println coordinates with a live progress bar so the notice
	// prints above it instead of colliding with the in-place redraw.
	return func(attempt int, wait time.Duration, lastErr error) {
		// The raw jittered duration ("618.167744ms") is debugging noise
		// on a user-facing line.
		wait = wait.Round(100 * time.Millisecond)
		if f.debug {
			uxlog.Println(fmt.Sprintf("  %s Connection interrupted (%s) — retrying in %s (attempt %d/%d)",
				uxlog.Retry(), shortErr(lastErr), wait, attempt, retry.DefaultAttempts))
			return
		}
		uxlog.Println(fmt.Sprintf("  %s Connection interrupted — retrying in %s (attempt %d/%d)",
			uxlog.Retry(), wait, attempt, retry.DefaultAttempts))
	}
}

// shortErr produces a one-line summary of an error for the retry
// notice. Long QUIC error strings break the layout; we truncate.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 60
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}

func hostnameOrDefault(s string) string {
	if s != "" {
		return s
	}
	h, _ := os.Hostname()
	return h
}

// printPath renders the --debug path trace: the standalone headline
// plus the selected ICE candidate types. The user-facing path display
// lives inline on the "Receiver connected" line (sender) and the
// "Incoming from" line (receiver), so non-debug runs print nothing
// extra here — a standalone headline would repeat the inline chip.
func printPath(f *flags, info connpath.Info) {
	if f.quiet || !f.debug {
		return
	}
	fmt.Fprintln(os.Stderr, uxlog.Check(), info.Headline())
	if d := info.Detail(); d != "" {
		fmt.Fprintln(os.Stderr, "    ICE candidate pair:", d)
	}
}
