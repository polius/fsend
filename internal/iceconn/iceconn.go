// Package iceconn wraps pion/ice/v4 into a small API the rest of fsend
// can use to establish a direct UDP path between two peers, then hand the
// resulting socket to quic-go's Transport.
//
// Critical wiring note: pion's *ice.Agent produces an *ice.Conn after
// Dial/Accept that is stream-shaped (net.Conn). quic-go's Transport
// expects a net.PacketConn. Because each ice.Conn.Write produces
// exactly one UDP datagram on the wire (and each Read returns exactly
// one), we can adapt the two with a thin wrapper that injects a
// synthetic peer address — the same trick internal/relay.Conn uses on
// the relay path.
//
// The PacketConn handed to quic.Transport must NOT be the agent's
// gathered candidate socket directly — it must be the post-Dial/Accept
// *ice.Conn so the data flows over the selected candidate pair (with the
// punched NAT mapping). Any other wiring wastes the hole-punch.
//
// Roles:
//
//   - The "controlling" agent uses Dial (sender, by fsend convention).
//   - The "controlled" agent uses Accept (receiver).
//
// Either ordering works as long as exactly one side is controlling; the
// roles are independent of the QUIC server/client roles layered on top.
package iceconn

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v3"
)

// Options bundle the inputs callers must supply.
//
// LocalUfrag/LocalPwd are the ICE credentials this side has already
// shared with the peer via the signaling channel. RemoteUfrag/RemotePwd
// likewise come from the peer's signaling response. Both pairs are short
// random strings; see internal/server.newIceCreds.
//
// STUNHost is the pairing server's host (no port — Port is hard-coded
// to 443 to match the server's UDP listener). Empty disables STUN-based
// server-reflexive gathering, which limits us to host candidates only —
// useful in tests, not in production.
type Options struct {
	LocalUfrag  string
	LocalPwd    string
	RemoteUfrag string
	RemotePwd   string

	// STUNHost is the pairing server's hostname. Used for STUN-style
	// reflection (server-reflexive candidates). Empty disables srflx
	// gathering.
	STUNHost string
	// STUNPort is the pairing server's UDP port. Defaults to 443.
	STUNPort int
}

// Agent is fsend's ICE coordinator.
//
// Lifecycle:
//
//	a, _ := iceconn.New(opts)
//	defer a.Close()
//	// goroutine: pump local candidates out via signaling
//	for c := range a.LocalCandidates() { signaling.PushCandidates(c) }
//	// goroutine: pump remote candidates in from signaling
//	for c := range signaling.PullCandidates() { a.AddRemoteCandidate(c) }
//	conn, _ := a.Dial(ctx)   // or a.Accept(ctx)
//	// hand conn to a quic.Transport
type Agent struct {
	inner *ice.Agent
	opts  Options

	// localCh receives the string-marshalled form of each gathered local
	// candidate. It closes when gathering completes (pion signals this
	// by invoking OnCandidate with nil).
	localCh   chan string
	closeOnce sync.Once
}

// defaultTimeouts returns fsend's pion ICE agent timings. Kept as a
// function so callers can't mutate the package's defaults.
func defaultTimeouts() (disconnected, failed, keepalive time.Duration) {
	return 5 * time.Second, 15 * time.Second, 2 * time.Second
}

// New constructs a configured ICE agent and starts candidate gathering.
//
// Returns once the agent is alive and gathering is in flight; the caller
// must then immediately start draining LocalCandidates so pion's
// OnCandidate callback doesn't back up.
func New(opts Options) (*Agent, error) {
	if opts.LocalUfrag == "" || opts.LocalPwd == "" {
		return nil, fmt.Errorf("iceconn: LocalUfrag and LocalPwd are required")
	}
	if opts.STUNPort == 0 {
		opts.STUNPort = 443
	}

	disc, fail, ka := defaultTimeouts()

	agentOpts := []ice.AgentOption{
		ice.WithNetworkTypes([]ice.NetworkType{
			ice.NetworkTypeUDP4,
			ice.NetworkTypeUDP6,
		}),
		ice.WithCandidateTypes([]ice.CandidateType{
			ice.CandidateTypeHost,
			ice.CandidateTypeServerReflexive,
		}),
		ice.WithLocalCredentials(opts.LocalUfrag, opts.LocalPwd),
		ice.WithDisconnectedTimeout(disc),
		ice.WithFailedTimeout(fail),
		ice.WithKeepaliveInterval(ka),
	}
	if opts.STUNHost != "" {
		agentOpts = append(agentOpts, ice.WithUrls([]*stun.URI{
			{
				Scheme: stun.SchemeTypeSTUN,
				Host:   opts.STUNHost,
				Port:   opts.STUNPort,
				Proto:  stun.ProtoTypeUDP,
			},
		}))
	}

	inner, err := ice.NewAgentWithOptions(agentOpts...)
	if err != nil {
		return nil, fmt.Errorf("iceconn: new agent: %w", err)
	}

	a := &Agent{
		inner:   inner,
		opts:    opts,
		localCh: make(chan string, 16),
	}

	// OnCandidate fires for each gathered local candidate. A nil candidate
	// means gathering finished. We translate both into a single channel
	// stream — close on nil.
	if err := inner.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			a.closeLocal()
			return
		}
		// Non-blocking send with a generous buffer; if the caller hasn't
		// drained, we drop rather than stall pion's internal goroutine.
		select {
		case a.localCh <- c.Marshal():
		default:
		}
	}); err != nil {
		_ = inner.Close()
		return nil, fmt.Errorf("iceconn: register OnCandidate: %w", err)
	}

	if err := inner.GatherCandidates(); err != nil {
		_ = inner.Close()
		return nil, fmt.Errorf("iceconn: gather: %w", err)
	}

	return a, nil
}

// LocalCandidates returns the channel of string-marshalled local
// candidates. Callers should push each to the signaling channel.
//
// The channel closes when gathering finishes.
func (a *Agent) LocalCandidates() <-chan string { return a.localCh }

// AddRemoteCandidate parses and feeds a candidate received from the peer
// via signaling.
//
// Malformed candidates are returned as errors rather than silently
// dropped — pion's pairing only succeeds when both sides see a workable
// candidate, so a peer that emitted garbage will fail the connection
// check anyway. We surface the error so it shows up in --debug logs.
func (a *Agent) AddRemoteCandidate(raw string) error {
	if raw == "" {
		return nil
	}
	c, err := ice.UnmarshalCandidate(raw)
	if err != nil {
		return fmt.Errorf("iceconn: parse remote candidate %q: %w", raw, err)
	}
	if err := a.inner.AddRemoteCandidate(c); err != nil {
		return fmt.Errorf("iceconn: add remote candidate: %w", err)
	}
	return nil
}

// Dial drives the ICE connection check as the controlling agent.
// Returns a net.PacketConn ready to be handed to quic.Transport.
//
// Blocks until either a candidate pair is selected or ctx expires.
func (a *Agent) Dial(ctx context.Context) (net.PacketConn, error) {
	c, err := a.inner.Dial(ctx, a.opts.RemoteUfrag, a.opts.RemotePwd)
	if err != nil {
		return nil, fmt.Errorf("iceconn: dial: %w", err)
	}
	return wrapAsPacket(c), nil
}

// Accept drives the ICE connection check as the controlled agent.
// Returns a net.PacketConn ready to be handed to quic.Transport.
func (a *Agent) Accept(ctx context.Context) (net.PacketConn, error) {
	c, err := a.inner.Accept(ctx, a.opts.RemoteUfrag, a.opts.RemotePwd)
	if err != nil {
		return nil, fmt.Errorf("iceconn: accept: %w", err)
	}
	return wrapAsPacket(c), nil
}

// Close releases the agent's resources. Safe to call multiple times.
func (a *Agent) Close() error {
	a.closeLocal()
	return a.inner.Close()
}

// SelectedPair returns the ICE candidate types of the pair pion selected
// for data flow. Only meaningful after Dial or Accept has returned
// successfully — before that, ok is false.
//
// The strings come from pion's own CandidateType.String() and are stable:
// "host", "srflx", "prflx", or "relay". Callers (e.g. internal/connpath)
// can rely on these exact values.
//
// Returning strings rather than pion types keeps the rest of the codebase
// free of a transitive pion/ice dependency for what is otherwise pure
// display logic.
func (a *Agent) SelectedPair() (localType, remoteType string, ok bool) {
	pair, err := a.inner.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return "", "", false
	}
	return pair.Local.Type().String(), pair.Remote.Type().String(), true
}

func (a *Agent) closeLocal() {
	a.closeOnce.Do(func() { close(a.localCh) })
}
