package main

import (
	"context"
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
	"github.com/polius/fsend/internal/version"
	"github.com/polius/fsend/internal/wire"
)

// iceBudget caps the total time we'll spend trying to establish a direct
// path before falling back to the relay. Matches the budget in
// PROJECT_SPEC.md "Timeouts" → "ICE connectivity checks".
const iceBudget = 15 * time.Second

// signalingClient builds a client pointed at the configured server.
//
// HTTPS is the default for non-local addresses; HTTP for localhost so dev
// loops work without a cert.
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
	return signaling.New(baseURL, version.Version), addr
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

// runSendOverInternet performs the cross-internet send flow when LAN
// discovery has failed (or been disabled).
//
// Strategy ladder per PROJECT_SPEC.md:
//   1. signaling Create + wait for peer
//   2. try ICE direct (sender = controlling)
//   3. on ICE failure, AllocateRelay + run QUIC over the relay
//
// In all three cases the QUIC wire protocol is the same — only the
// underlying net.PacketConn differs.
func runSendOverInternet(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, label string, cfg *config.Config) error {
	client, serverAddr := signalingClient(cfg)

	created, err := client.Create(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Delete(context.Background(), created.SessionID) }()

	printSendArtifact(f, created.Code, items, kind, totalFiles, label)

	// Long-poll until the receiver pairs.
	deadline := time.Now().Add(time.Duration(created.TTLSeconds) * time.Second)
	var waitResp *server.WaitResponse
	for time.Now().Before(deadline) && waitResp == nil {
		resp, err := client.Wait(ctx, created.Code)
		if err != nil {
			return err
		}
		waitResp = resp
	}
	if waitResp == nil {
		return fmt.Errorf("%w: receiver did not arrive within %ds", fserrors.ErrPromptTimeout, created.TTLSeconds)
	}

	// --- Try ICE direct path ---
	stunHost := stunHostFromServer(serverAddr)
	iceConn, icePath, iceErr := iceEstablish(ctx, client, created.SessionID, iceconn.Options{
		LocalUfrag:  created.IceCredentials.Ufrag,
		LocalPwd:    created.IceCredentials.Pwd,
		RemoteUfrag: waitResp.PeerIceCredentials.Ufrag,
		RemotePwd:   waitResp.PeerIceCredentials.Pwd,
		STUNHost:    stunHost,
	}, true /* controlling */)
	if iceErr == nil {
		defer iceConn.Close()
		printPath(f, icePath)
		return runSenderQUICOver(ctx, f, items, kind, totalFiles, iceConn, created.Code)
	}
	if f.debug {
		fmt.Fprintln(os.Stderr, "DEBUG: ICE failed:", iceErr)
	}

	// --- Fall back to relay ---
	alloc, err := client.AllocateRelay(ctx, created.SessionID)
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	relayConn, err := dialRelay(alloc)
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	defer relayConn.Close()
	printPath(f, connpath.FromRelay(alloc.RelayAddr))
	return runSenderQUICOver(ctx, f, items, kind, totalFiles, relayConn, created.Code)
}

// runReceiveOverInternet performs the cross-internet receive flow.
//
// Mirror of runSendOverInternet: try ICE direct first (receiver =
// controlled), fall back to relay on failure.
func runReceiveOverInternet(ctx context.Context, f *flags, c string, cfg *config.Config) error {
	client, serverAddr := signalingClient(cfg)

	joined, err := client.Join(ctx, c)
	if err != nil {
		return err
	}

	// --- Try ICE direct path ---
	stunHost := stunHostFromServer(serverAddr)
	iceConn, icePath, iceErr := iceEstablish(ctx, client, joined.SessionID, iceconn.Options{
		LocalUfrag:  joined.YourIceCredentials.Ufrag,
		LocalPwd:    joined.YourIceCredentials.Pwd,
		RemoteUfrag: joined.PeerIceCredentials.Ufrag,
		RemotePwd:   joined.PeerIceCredentials.Pwd,
		STUNHost:    stunHost,
	}, false /* controlled */)
	if iceErr == nil {
		defer iceConn.Close()
		printPath(f, icePath)
		return runReceiverQUICOver(ctx, f, iceConn, c)
	}
	if f.debug {
		fmt.Fprintln(os.Stderr, "DEBUG: ICE failed:", iceErr)
	}

	// --- Fall back to relay ---
	alloc, err := client.AllocateRelay(ctx, joined.SessionID)
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	relayConn, err := dialRelay(alloc)
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrConnectFailed, err)
	}
	defer relayConn.Close()
	printPath(f, connpath.FromRelay(alloc.RelayAddr))
	return runReceiverQUICOver(ctx, f, relayConn, c)
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
func iceEstablish(parent context.Context, sig *signaling.Client, sessionID string, opts iceconn.Options, controlling bool) (net.PacketConn, connpath.Info, error) {
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
			if err := sig.PushCandidates(pumpCtx, sessionID, []string{cstr}); err != nil {
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
			resp, err := sig.PullCandidates(pumpCtx, sessionID, since)
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
	// On success, ownership of the agent passes to the returned conn —
	// closing the conn (which the caller does) tears down the ice.Conn
	// but the agent itself still owns the gathered sockets. We close the
	// agent when the conn is closed by hooking a wrapper around it.
	//
	// Classify the path from the selected candidate pair so the caller
	// can render it. If pion didn't surface a pair (shouldn't happen post
	// Dial/Accept, but guard anyway), fall back to DirectSTUN — strictly
	// safer than overclaiming "local".
	localCT, remoteCT, ok := agent.SelectedPair()
	var info connpath.Info
	if ok {
		info = connpath.FromICE(localCT, remoteCT)
	} else {
		info = connpath.Info{Kind: connpath.KindDirectSTUN}
	}
	return &iceOwningConn{PacketConn: conn, agent: agent}, info, nil
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

// runSenderQUICOver runs the sender's QUIC + transfer flow over an
// already-established net.PacketConn (ICE or relay, doesn't matter
// past this point).
//
// Wrapped in a retry loop: a transient QUIC/transfer error tears down
// the current QUIC session, sleeps, and re-Accepts on the same
// underlying PacketConn. The receiver's imohash + chunk-aligned partial
// pick up where the previous attempt left off, so the user sees a brief
// "retrying" line and the progress bar resumes — not a fresh download.
func runSenderQUICOver(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, pc net.PacketConn, code string) error {
	tlsCfg, err := quicconn.SenderTLSConfig()
	if err != nil {
		return err
	}
	tr := &quic.Transport{Conn: pc}
	defer tr.Close()
	ln, err := tr.Listen(tlsCfg, quicconn.QuicConfig())
	if err != nil {
		return fmt.Errorf("QUIC listen: %w", err)
	}
	defer ln.Close()

	closeProg, progressFn := newSenderProgress(f, items)
	defer closeProg()

	err = retry.WithBackoff(ctx, retry.Options{OnRetry: retryNoticeFor(f)}, nil,
		func(attempt int) error {
			return runSenderOneAttempt(ctx, ln, items, kind, totalFiles, f, progressFn, code)
		})
	if err != nil {
		return err
	}

	if !f.quiet {
		fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Transfer complete")
	}
	return nil
}

// runSenderOneAttempt is one Accept → handshake → transfer iteration. A
// success returns nil; transient errors are returned for the retry layer
// to classify.
func runSenderOneAttempt(ctx context.Context, ln *quic.Listener, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, f *flags, progressFn func(uint32, uint64), code string) error {
	qc, err := ln.Accept(ctx)
	if err != nil {
		return fmt.Errorf("QUIC accept: %w", err)
	}
	res, err := quicconn.SenderHandshake(ctx, qc, code)
	if err != nil {
		return err
	}
	defer res.Close()

	return transfer.Send(ctx, &res.Streams, transfer.SendOptions{
		Items:         items,
		Hostname:      hostnameOrDefault(f.hostname),
		OS:            runtime.GOOS,
		ClientVersion: version.Version,
		TransferKind:  kind,
		TotalFiles:    totalFiles,
		Compress:      !f.noCompress,
		Password:      f.passArg,
		ProgressFn:    progressFn,
	})
}

// runReceiverQUICOver runs the receiver's QUIC + transfer flow over an
// already-established net.PacketConn.
//
// Symmetric retry: a transient error tears down the current QUIC
// session, sleeps, and re-Dials on the same PacketConn. The receiver's
// .fsend-partial sidecar plus its imohash fingerprint let the next
// attempt resume mid-file — the sender verifies the prefix, seeks past
// it, and streams the remainder.
func runReceiverQUICOver(ctx context.Context, f *flags, pc net.PacketConn, code string) error {
	tr := &quic.Transport{Conn: pc}
	defer tr.Close()

	outDir := f.outDir
	if outDir == "" {
		var err error
		outDir, err = os.Getwd()
		if err != nil {
			return err
		}
	}

	closeProg, accept, progressFn := newReceiverProgress(f)
	defer closeProg()

	if err := retry.WithBackoff(ctx, retry.Options{OnRetry: retryNoticeFor(f)}, nil,
		func(attempt int) error {
			return runReceiverOneAttempt(ctx, tr, outDir, f, accept, progressFn, code)
		}); err != nil {
		return err
	}
	if !f.quiet {
		fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Transfer complete")
	}
	return nil
}

// runReceiverOneAttempt is one Dial → handshake → transfer iteration.
// Mirror of runSenderOneAttempt; returns nil on success or an error for
// the retry layer to classify.
func runReceiverOneAttempt(ctx context.Context, tr *quic.Transport, outDir string, f *flags, accept func(wire.SenderHello) bool, progressFn func(uint32, uint64), code string) error {
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
		Hostname:      hostnameOrDefault(f.hostname),
		OS:            runtime.GOOS,
		ClientVersion: version.Version,
		TargetDir:     outDir,
		Overwrite:     f.overwrite,
		Accept:        accept,
		Password:      f.passArg,
		PromptPass:    receiverPasswordPrompt(f),
		ProgressFn:    progressFn,
	})
}

// retryNoticeFor returns an OnRetry callback that prints the standard
// "retrying" notice to stderr. Returns nil when --quiet is set so we
// don't break pipeline-mode output.
func retryNoticeFor(f *flags) func(attempt int, wait time.Duration, lastErr error) {
	if f.quiet {
		return nil
	}
	return func(attempt int, wait time.Duration, lastErr error) {
		fmt.Fprintf(os.Stderr, "  %s Connection interrupted (%v) — retrying in %s (attempt %d)\n",
			marker("⟳", "[~]"), shortErr(lastErr), wait, attempt)
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

// printPath renders the connection-path status line per PROJECT_SPEC.md
// "Always show the data path". Tri-state:
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
	utf8Glyph, asciiGlyph := info.Glyph()
	fmt.Fprintln(os.Stderr, marker(utf8Glyph, asciiGlyph), info.Headline())
	if f.debug {
		if d := info.Detail(); d != "" {
			fmt.Fprintln(os.Stderr, "    ICE candidate pair:", d)
		}
	}
}

