package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/iceconn"
	"github.com/polius/fsend/internal/landisc"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/retry"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
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

// pathDecisionWait is how long past iceBudget the receiver waits for the
// sender's path decision before trusting its own ICE outcome.
const pathDecisionWait = 5 * time.Second

// The sender (ICE controlling side) publishes which path it chose as a
// pseudo-candidate on the signaling channel, so both sides agree even when
// ICE completes right at the budget edge on one clock and not the other.
const (
	pathChoicePrefix = "fsend-path "
	pathChoiceICE    = pathChoicePrefix + "ice"
	pathChoiceRelay  = pathChoicePrefix + "relay"
)

// signalingClient builds a client pointed at the configured server.
//
// HTTPS is the default for non-local addresses; HTTP for localhost so dev
// loops work without a cert. If the user has configured a per-server
// password (via `fsend --connect <host:port>,<password>` or the
// FSEND_SERVER_PASSWORD env), the client carries it through; the server
// matches against FSEND_SERVER_PASSWORD.
func signalingClient(cfg *config.Config) (*signaling.Client, string, error) {
	addr := cfg.EffectiveServer()
	pw, err := effectiveServerPassword(cfg)
	if err != nil {
		return nil, "", err
	}
	return signaling.New(serverBaseURL(addr), version.Version).WithPassword(pw), addr, nil
}

// effectiveServerPassword prefers the --connect value (kept in the config
// file); FSEND_SERVER_PASSWORD is the non-argv fallback for scripts, the
// client-side mirror of the server's env of the same name, with
// FSEND_SERVER_PASSWORD_FILE as the mounted-file alternative.
func effectiveServerPassword(cfg *config.Config) (string, error) {
	if cfg.ServerPassword != "" {
		return cfg.ServerPassword, nil
	}
	return passwordFromEnv("FSEND_SERVER_PASSWORD")
}

// serverBaseURL maps a configured server address to an http(s) base URL:
// HTTPS for non-local addresses, HTTP for localhost so dev loops work
// without a cert. A user-supplied URL is preserved as-is.
func serverBaseURL(addr string) string {
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		if isLocalAddr(addr) {
			return "http://" + addr
		}
		return "https://" + addr
	}
	return addr
}

func isLocalAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	return h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// joinRetryBudget caps how long the receiver waits for the sender to
// register a fresh code on the pairing server. The sender Creates its
// session the moment it starts, so the code is normally registered long
// before a human can type it; the budget only covers a receiver beating a
// slow Create. A mistyped code — the common case — must fail fast.
//
// Var (not const) so tests can shrink it.
var joinRetryBudget = 6 * time.Second

// resumeJoinBudget is the wait when re-pairing on a resume key: the sender
// may still be detecting the drop (QUIC idle timeout plus its re-accept
// retries) before it re-creates the session.
var resumeJoinBudget = 90 * time.Second

// joinOpts tunes joinRace.
type joinOpts struct {
	budget time.Duration
	// retryClaimed keeps retrying 409: a resume slot may still hold the
	// session of the connection that just dropped until the sender
	// recycles it. A fresh code's 409 is final (E003).
	retryClaimed bool
	// lan, when set, races an in-flight mDNS query: an answer before the
	// join succeeds wins.
	lan <-chan lanResult
}

// lanResult is a finished mDNS query.
type lanResult struct {
	q   *landisc.QueryResult
	err error
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

// joinWithRetry joins a fresh code: joinRace with the default budget and
// no LAN race.
func joinWithRetry(ctx context.Context, client *signaling.Client, code string, f *flags, existing *uxlog.Spinner) (*server.JoinSessionResponse, error) {
	joined, _, err := joinRace(ctx, client, code, f, existing, joinOpts{budget: joinRetryBudget})
	return joined, err
}

// joinRace calls Join, retrying ErrCodeNotFound (and, with
// opts.retryClaimed, ErrCodeAlreadyClaimed) within opts.budget.
// Not-found covers the receiver winning the race against the sender's
// Create (or, on a resume key, the sender still detecting the drop).
//
// Other errors propagate immediately: server-unreachable, ctx-cancelled,
// etc., are not transient and the caller should hear about them on the
// first try — unless an mDNS query is still running, which then decides.
// A LAN answer arriving first returns (nil, hit, nil).
//
// Every join — including a 404 — counts against the server's per-IP
// new-session budget (that's what stops code-space probing), so the
// schedule is deliberately sparse rather than a tight poll. And if the
// server starts throttling us mid-loop after retryable answers, the honest
// error is the last retryable one (E002/E003), not E017 — a mistyped code
// must not surface as "too many attempts".
func joinRace(ctx context.Context, client *signaling.Client, rendezvous string, f *flags, existing *uxlog.Spinner, opts joinOpts) (*server.JoinSessionResponse, *landisc.QueryResult, error) {
	// Say what we're actually doing — waiting for a sender that isn't on
	// this code (yet) — rather than a reassuring "Connecting".
	waitMsg := "No sender on this code yet — waiting"
	if opts.retryClaimed {
		waitMsg = "Waiting for the sender to reconnect"
	}

	deadline := time.Now().Add(opts.budget)
	delay := 500 * time.Millisecond
	var lastRetryable error
	var spin *uxlog.Spinner
	retitled := false
	// Close over the variable so a spinner started inside the loop is
	// still Stopped on return — a plain `defer spin.Stop()` would only
	// stop the nil pointer captured at defer-time.
	defer func() { spin.Stop() }()
	lan := opts.lan
	// lanHit drains a finished query without blocking; a miss disarms it.
	lanHit := func() *landisc.QueryResult {
		select {
		case r := <-lan:
			lan = nil
			if r.err == nil {
				return r.q
			}
		default:
		}
		return nil
	}
	for {
		if q := lanHit(); q != nil {
			return nil, q, nil
		}
		joined, err := client.Join(ctx, rendezvous)
		if err == nil {
			if retitled {
				// The caller's spinner keeps running through ICE/relay setup;
				// hand it back with its original label.
				existing.SetMessage("Connecting")
			}
			return joined, nil, nil
		}
		throttled := lastRetryable != nil && errors.Is(err, fserrors.ErrRateLimited)
		if throttled {
			err = lastRetryable
		}
		retryable := !throttled && (errors.Is(err, fserrors.ErrCodeNotFound) ||
			(opts.retryClaimed && errors.Is(err, fserrors.ErrCodeAlreadyClaimed)))
		if !retryable || time.Now().After(deadline) {
			// The server can't help; a LAN query still in flight might.
			if lan != nil && ctx.Err() == nil {
				select {
				case r := <-lan:
					if r.err == nil {
						return nil, r.q, nil
					}
				case <-ctx.Done():
				}
			}
			return nil, nil, err
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
				spin = uxlog.StartSpinner(waitMsg, uxlog.HintCancel)
			}
			retitled = existing != nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case r := <-lan:
			lan = nil
			if r.err == nil {
				return nil, r.q, nil
			}
		case <-time.After(delay):
		}
		if delay < 4*time.Second {
			delay *= 2
		}
	}
}

// publishPathChoice tells the receiver which path the sender committed to.
// Best effort: without it the receiver falls back to its own ICE outcome.
func publishPathChoice(ctx context.Context, sig *signaling.Client, sessionID, roleToken, choice string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = sig.PushCandidates(ctx, sessionID, roleToken, []string{choice})
}

// iceEstablish runs the full ICE handshake: starts an agent, pumps local
// candidates out via signaling, pulls remote candidates in via signaling,
// and drives Dial/Accept under a single iceBudget timeout.
//
// Returns a net.PacketConn ready for quic.Transport, plus a classified
// connpath.Info derived from the selected candidate pair, on success. On
// failure, the agent is closed and the caller falls back to relay.
//
// controlling=true → sender role: calls Dial, then publishes its path
// choice. controlling=false → receiver role: calls Accept, and follows the
// sender's published choice, so the two never split between ICE and the
// relay when ICE completes right at the budget edge.
//
// debug enables candidate-level stderr logging — the data needed to
// diagnose "why did this pair of networks relay?" reports.
func iceEstablish(parent context.Context, sig *signaling.Client, sessionID, roleToken string, opts iceconn.Options, controlling bool, debug bool) (net.PacketConn, connpath.Info, error) {
	debugf := func(format string, args ...any) {
		if debug {
			fmt.Fprintf(os.Stderr, "DEBUG: "+format+"\n", args...)
		}
	}
	budget := iceBudget
	if !controlling {
		budget += pathDecisionWait
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	agent, err := iceconn.New(opts)
	if err != nil {
		if controlling {
			publishPathChoice(parent, sig, sessionID, roleToken, pathChoiceRelay)
		}
		return nil, connpath.Info{}, fmt.Errorf("agent: %w", err)
	}

	// Pump local candidates out and remote candidates in. The signaling
	// server's push/pull endpoints route candidates between peers; see
	// internal/server.{pushCandidates,pullCandidates}.
	pumpCtx, pumpCancel := context.WithCancel(ctx)
	defer pumpCancel()
	var wg sync.WaitGroup
	decision := make(chan string, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		// The select on pumpCtx is load-bearing: if ICE times out before
		// pion finishes gathering, agent.LocalCandidates never closes — a
		// bare `for range` would block here, wg.Wait() would hang waiting
		// for it, and the deferred agent.Close() (which closes the channel)
		// would never run.
		for {
			var batch []string
			select {
			case <-pumpCtx.Done():
				return
			case cstr, ok := <-agent.LocalCandidates():
				if !ok {
					return
				}
				batch = append(batch, cstr)
			}
			// Batch whatever else is already gathered into one request, so
			// a host with many interfaces doesn't queue its srflx candidate
			// behind a POST per host candidate.
		drain:
			for {
				select {
				case cstr, ok := <-agent.LocalCandidates():
					if !ok {
						break drain
					}
					batch = append(batch, cstr)
				default:
					break drain
				}
			}
			sort.SliceStable(batch, func(i, j int) bool {
				return strings.Contains(batch[i], " typ srflx") && !strings.Contains(batch[j], " typ srflx")
			})
			for _, c := range batch {
				debugf("ICE local candidate: %s", c)
			}
			if err := sig.PushCandidates(pumpCtx, sessionID, roleToken, batch); err != nil {
				// Best-effort: pion's ICE will keep going with whatever
				// candidates have already crossed.
				debugf("ICE push candidates failed: %v", err)
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
				if strings.HasPrefix(cstr, pathChoicePrefix) {
					select {
					case decision <- cstr:
					default:
					}
					continue
				}
				debugf("ICE remote candidate: %s", cstr)
				if err := agent.AddRemoteCandidate(cstr); err != nil {
					debugf("ICE remote candidate rejected: %v", err)
				}
			}
			since = resp.NextSince
		}
	}()

	var (
		conn    net.PacketConn
		dialErr error
	)
	if controlling {
		dctx, dcancel := context.WithTimeout(ctx, iceBudget)
		conn, dialErr = agent.Dial(dctx)
		dcancel()
		choice := pathChoiceICE
		if dialErr != nil {
			choice = pathChoiceRelay
		}
		publishPathChoice(parent, sig, sessionID, roleToken, choice)
	} else {
		conn, dialErr = acceptFollowingSender(ctx, agent, decision)
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

// errSenderChoseRelay is the receiver dropping ICE on the sender's word.
var errSenderChoseRelay = errors.New("ice: sender chose the relay")

// acceptFollowingSender runs the controlled side's Accept and reconciles
// it with the sender's published choice. ctx bounds the whole wait
// (iceBudget + pathDecisionWait). No choice by then: trust our own result.
func acceptFollowingSender(ctx context.Context, agent *iceconn.Agent, decision <-chan string) (net.PacketConn, error) {
	type result struct {
		conn net.PacketConn
		err  error
	}
	acceptCtx, cancelAccept := context.WithCancel(ctx)
	defer cancelAccept()
	accepted := make(chan result, 1)
	go func() {
		c, err := agent.Accept(acceptCtx)
		accepted <- result{c, err}
	}()

	var got *result
	choice := ""
	for {
		select {
		case r := <-accepted:
			got = &r
			accepted = nil
		case choice = <-decision:
			decision = nil
		case <-ctx.Done():
			if got == nil {
				cancelAccept()
				r := <-accepted
				got = &r
			}
			return got.conn, got.err
		}
		switch {
		case choice == pathChoiceRelay:
			if got == nil {
				cancelAccept()
				r := <-accepted
				got = &r
			}
			return nil, errSenderChoseRelay
		case got != nil && (choice == pathChoiceICE || got.err != nil):
			return got.conn, got.err
		}
	}
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
//
// serverHost is the signaling server's host (signaling.Client.ServerHost);
// the relay address is server-chosen, so validateRelayAddr screens it before
// we dial (SSRF-ish defense against a hostile/misconfigured server).
func dialRelay(alloc *server.RelayAllocateResponse, serverHost string) (net.PacketConn, error) {
	if err := validateRelayAddr(alloc.RelayAddr, serverHost); err != nil {
		return nil, err
	}
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

// validateRelayAddr refuses a relay address the signaling server chose
// unless it is the same host we trusted enough to speak signaling to, or a
// public address. relayAddrFor normally pairs the client's Host with the
// relay's UDP port, so the trusted case is exact-host equality — even for a
// single-box deploy whose server host is loopback or a LAN IP. Anything else
// is a redirect we didn't ask for: a hostile/misconfigured server could
// otherwise aim our QUIC dialer at loopback, RFC1918, link-local, multicast,
// or a cloud-metadata address.
func validateRelayAddr(relayAddr, serverHost string) error {
	host, port, err := net.SplitHostPort(relayAddr)
	if err != nil {
		return fmt.Errorf("relay address %q: %w", relayAddr, err)
	}
	if host == "" || port == "" {
		return fmt.Errorf("relay address %q: missing host or port", relayAddr)
	}
	if serverHost != "" && strings.EqualFold(host, serverHost) {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A name pointing somewhere other than our server. Resolving it
		// would invite DNS-rebinding, and the supported relay layout never
		// advertises a different host, so refuse.
		return fmt.Errorf("relay address %q: host is not the signaling server %q", relayAddr, serverHost)
	}
	if unsafeRelayIP(ip) {
		return fmt.Errorf("relay address %q: refusing non-public relay host", relayAddr)
	}
	return nil
}

// unsafeRelayIP reports whether ip is anything other than a globally
// routable address — the set a relay must never redirect a client to.
func unsafeRelayIP(ip net.IP) bool {
	return ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
}

// linkViaInternet turns a successful join into a recvLink: ICE direct
// first (receiver = controlled, following the sender's choice), relay on
// failure. The link dials QUIC over the established PacketConn.
func linkViaInternet(ctx context.Context, f *flags, client *signaling.Client, joined *server.JoinSessionResponse) (*recvLink, error) {
	// STUN address rides on the join response; no relay allocation up front.
	stunAddr := stunAddrForICE(ctx, client, joined.RelayAddr, joined.SessionID, joined.RoleToken)
	pc, path, iceErr := iceEstablish(ctx, client, joined.SessionID, joined.RoleToken, iceconn.Options{
		LocalUfrag:  joined.YourIceCredentials.Ufrag,
		LocalPwd:    joined.YourIceCredentials.Pwd,
		RemoteUfrag: joined.PeerIceCredentials.Ufrag,
		RemotePwd:   joined.PeerIceCredentials.Pwd,
		STUNAddr:    stunAddr,
	}, false /* controlled */, f.debug)
	var probe *relayProbe
	if iceErr != nil {
		if f.debug {
			fmt.Fprintln(os.Stderr, "DEBUG: ICE failed:", iceErr)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Fall back to relay: allocate now that we know we need it.
		if joined.RelayForwardingDisabled {
			return nil, fmt.Errorf("%w: direct connection failed and this server has relay forwarding disabled", fserrors.ErrConnectFailed)
		}
		var err error
		pc, path, err = allocAndDialRelay(ctx, client, joined.SessionID, joined.RoleToken)
		if err != nil {
			return nil, err
		}
		probe = &relayProbe{client: client, sessionID: joined.SessionID, roleToken: joined.RoleToken}
	}
	tr := quicconn.NewTransport(pc)
	return &recvLink{
		path:  path,
		relay: probe,
		dial: func(ctx context.Context, secret string) (*quicconn.AcceptResult, error) {
			return dialOverTransport(ctx, tr, secret)
		},
		close: func() {
			_ = tr.Close()
			_ = pc.Close()
		},
	}, nil
}

// dialOverTransport is one QUIC dial plus the receiver PAKE over an
// established ICE or relay PacketConn.
func dialOverTransport(ctx context.Context, tr *quic.Transport, secret string) (*quicconn.AcceptResult, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// The PacketConn ignores the dial address (both relay.Conn and our
	// iceOwningConn route via fixed underlying paths). quic-go uses the
	// address as a per-peer routing tag — the shared synthetic value
	// suffices. See relay.SyntheticPeer.
	qc, err := tr.Dial(dialCtx, relay.SyntheticPeer, quicconn.ReceiverTLSConfig(), quicconn.QuicConfig())
	if err != nil {
		return nil, fmt.Errorf("QUIC dial: %w", err)
	}
	// ReceiverHandshake bounds itself by HandshakeTimeout: a stalled peer
	// or a hostile relay can't hang the receiver.
	return quicconn.ReceiverHandshake(ctx, qc, secret)
}

// relayProbe identifies a relay allocation for classifyRelayDrop.
type relayProbe struct {
	client    *signaling.Client
	sessionID string
	roleToken string
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
