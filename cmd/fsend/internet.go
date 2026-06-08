package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
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
	"github.com/polius/fsend/internal/wire"
)

// iceBudget caps the total time we'll spend trying to establish a direct
// path before falling back to the relay.
const iceBudget = 15 * time.Second

// signalingClient builds a client pointed at the configured server.
//
// HTTPS is the default for non-local addresses; HTTP for localhost so dev
// loops work without a cert. If the user has configured a per-server
// password (via `fsend --connect <host:port> <password>`), the client
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

// stunHostFromServer extracts the bare host from an effective-server
// string for use as the STUN server's hostname in ICE. Returns "" on
// loopback addresses (we don't want to STUN to localhost in tests).
func stunHostFromServer(serverAddr string) string {
	h, _, err := net.SplitHostPort(serverAddr)
	if err != nil {
		h = serverAddr
	}
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "" {
		return ""
	}
	return h
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
	client, serverAddr := signalingClient(cfg)
	defer connSpin.Stop()

	joined, err := joinWithRetry(ctx, client, c, f, connSpin)
	if err != nil {
		return err
	}

	// --- Try ICE direct path ---
	stunHost := stunHostFromServer(serverAddr)
	iceConn, icePath, iceErr := iceEstablish(ctx, client, joined.SessionID, joined.RoleToken, iceconn.Options{
		LocalUfrag:  joined.YourIceCredentials.Ufrag,
		LocalPwd:    joined.YourIceCredentials.Pwd,
		RemoteUfrag: joined.PeerIceCredentials.Ufrag,
		RemotePwd:   joined.PeerIceCredentials.Pwd,
		STUNHost:    stunHost,
	}, false /* controlled */)
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

	// --- Fall back to relay ---
	alloc, err := client.AllocateRelay(ctx, joined.SessionID, joined.RoleToken)
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	relayConn, err := dialRelay(alloc)
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	defer func() { _ = relayConn.Close() }()
	connSpin.Stop()
	return classifyRelayDrop(ctx, client, joined.SessionID,
		runReceiverQUICOver(ctx, f, relayConn, c, connpath.FromRelay(alloc.RelayAddr)))
}

// classifyRelayDrop probes the relay-status endpoint when a relay-path
// transfer ends in a transient-looking error. If the relay tells us
// the allocation was evicted for a known reason, we promote the error
// to the corresponding non-transient sentinel so the user sees what
// actually happened instead of a "retrying…" loop.
func classifyRelayDrop(ctx context.Context, client *signaling.Client, sessionID string, runErr error) error {
	if runErr == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := client.RelayStatus(probeCtx, sessionID)
	if err != nil || status == nil || status.State != "evicted" {
		return runErr
	}
	switch status.Reason {
	case relay.ReasonCapHit:
		return fserrors.ErrRelayCapHit
	case relay.ReasonIdle:
		return fserrors.ErrRelayIdleTimeout
	}
	return runErr
}

// joinWithRetry calls Join, retrying only on ErrCodeNotFound. The
// receiver almost always wins the race against the sender's Create
// (LAN window is ~5s, server RTT is <200ms), but a slow human or a
// laggy network can still produce a few hundred milliseconds where the
// server hasn't seen the code yet. Without retries that surfaced as
// an instant, misleading E002 — telling the user the code expired when
// it just hasn't been registered yet.
//
// Other errors propagate immediately: code-already-claimed,
// server-unreachable, ctx-cancelled, etc., are not transient and the
// caller should hear about them on the first try.
func joinWithRetry(ctx context.Context, client *signaling.Client, code string, f *flags, existing *uxlog.Spinner) (*server.JoinSessionResponse, error) {
	deadline := time.Now().Add(joinRetryBudget)
	delay := 200 * time.Millisecond
	var spin *uxlog.Spinner
	// Close over the variable so a spinner started inside the loop is
	// still Stopped on return — a plain `defer spin.Stop()` would only
	// stop the nil pointer captured at defer-time.
	defer func() { spin.Stop() }()
	for {
		joined, err := client.Join(ctx, code)
		if err == nil {
			return joined, nil
		}
		if !errors.Is(err, fserrors.ErrCodeNotFound) || time.Now().After(deadline) {
			return nil, err
		}
		// Animate a single line for the duration of the wait so the
		// user knows we're holding for the sender rather than stuck.
		// If the caller already gave us a running spinner, leave it
		// alone — two spinners would fight each other on stderr.
		if spin == nil && existing == nil && !f.quiet {
			spin = uxlog.StartSpinner("Waiting for sender to register code")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < time.Second {
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
func iceEstablish(parent context.Context, sig *signaling.Client, sessionID, roleToken string, opts iceconn.Options, controlling bool) (net.PacketConn, connpath.Info, error) {
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
		// Push each gathered local candidate one-shot. We use the parent
		// ctx for the HTTP call so it can outlive pumpCtx briefly when
		// pumpCtx is cancelled mid-push (we still want the candidate to
		// land if possible).
		for cstr := range agent.LocalCandidates() {
			if pumpCtx.Err() != nil {
				return
			}
			if err := sig.PushCandidates(pumpCtx, sessionID, roleToken, []string{cstr}); err != nil {
				// Best-effort: pion's ICE will keep going with whatever
				// candidates have already crossed; surface in --debug only.
				_ = err
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
	tr := &quic.Transport{Conn: pc}
	defer func() { _ = tr.Close() }()

	outDir := f.outDir
	if outDir == "" {
		var err error
		outDir, err = os.Getwd()
		if err != nil {
			return err
		}
	}

	closeProg, accept, confirmOverwrite, progressFn, recvBytes := newReceiverProgress(f, outDir, pathInfo)
	defer closeProg()

	start := time.Now()
	if err := retry.WithBackoff(ctx, retry.Options{OnRetry: retryNoticeFor(f)}, nil,
		func(attempt int) error {
			return runReceiverOneAttempt(ctx, tr, outDir, f, accept, confirmOverwrite, progressFn, code)
		}); err != nil {
		return err
	}
	printRecvSummary(f, displayPath(outDir), recvBytes(), time.Since(start), pathInfo)
	return nil
}

// runReceiverOneAttempt is one Dial → handshake → transfer iteration.
// Mirror of runSenderOneAttempt; returns nil on success or an error for
// the retry layer to classify.
func runReceiverOneAttempt(ctx context.Context, tr *quic.Transport, outDir string, f *flags, accept func(wire.SenderHello) bool, confirmOverwrite func(string, int64, uint64) bool, progressFn func(uint32, uint64), code string) error {
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

	return transfer.Recv(ctx, &res.Streams, transfer.RecvOptions{
		Hostname:         hostnameOrDefault(f.hostname),
		OS:               runtime.GOOS,
		ClientVersion:    version.Version,
		TargetDir:        outDir,
		Overwrite:        f.overwrite,
		Accept:           accept,
		Password:         f.passArg,
		PromptPass:       receiverPasswordPrompt(f),
		ConfirmOverwrite: confirmOverwrite,
		ProgressFn:       progressFn,
	})
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
	return func(attempt int, wait time.Duration, lastErr error) {
		if f.debug {
			fmt.Fprintf(os.Stderr, "  %s Connection interrupted (%s) — retrying in %s (attempt %d/%d)\n",
				uxlog.Retry(), shortErr(lastErr), wait, attempt, retry.DefaultAttempts)
			return
		}
		fmt.Fprintf(os.Stderr, "  %s Connection interrupted — retrying in %s (attempt %d/%d)\n",
			uxlog.Retry(), wait, attempt, retry.DefaultAttempts)
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

// printPath renders the connection-path status line. Tri-state:
//
//	✓ direct (local) — same LAN, no NAT crossed
//	✓ direct (STUN) — NAT hole-punched
//	⚠ relay (TURN) via <relay-host> — NAT hole-punch failed
//
// In --debug mode, a second line shows the selected ICE candidate types
// ("host → srflx" etc.) — useful for diagnosing why a peer ended up on a
// particular path. --quiet suppresses both lines.
func printPath(f *flags, info connpath.Info) {
	if f.quiet {
		return
	}
	var prefix string
	if info.Kind == connpath.KindRelay {
		prefix = uxlog.Warn()
	} else {
		prefix = uxlog.Check()
	}
	fmt.Fprintln(os.Stderr, prefix, info.Headline())
	if f.debug {
		if d := info.Detail(); d != "" {
			fmt.Fprintln(os.Stderr, "    ICE candidate pair:", d)
		}
	}
}
