// Package server implements the pairing + relay-allocation HTTP
// signaling endpoints served by `fsend server`. All endpoints are
// under /v1/...
package server

import (
	"time"

	"github.com/polius/fsend/internal/relay"
)

// CreateSessionRequest is the body of POST /v1/session.
//
// Slot is required: the argon2id-stretched lookup key the client derives
// from its locally-generated code (internal/code.Slot). The raw code
// never reaches the server — it is the PAKE secret, and a server that
// knew it could MITM the transfer. The server only matches the two
// peers that derived the same slot.
type CreateSessionRequest struct {
	ClientVersion string `json:"client_version"`
	Slot          string `json:"slot"`
}

// CreateSessionResponse is the success body of POST /v1/session.
//
// There is no code (or slot) field: the client generated the code and
// owns it; the server has nothing to add.
//
// RoleToken is an opaque bearer credential the sender includes on
// subsequent /candidates calls so the server can route candidate batches
// to the right side without relying on source IP. Without this, two
// peers behind the same NAT can't disambiguate.
type CreateSessionResponse struct {
	SessionID        string   `json:"session_id"`
	YourObservedAddr string   `json:"your_observed_addr"`
	IceCredentials   IceCreds `json:"ice_credentials"`
	TTLSeconds       int      `json:"ttl_seconds"`
	ServerVersion    string   `json:"server_version"`
	RoleToken        string   `json:"role_token"`
	// RelayAddr is the server-wide STUN/relay address, so the client can
	// gather srflx candidates without allocating a slot. Empty if no relay.
	RelayAddr string `json:"relay_addr,omitempty"`
	// RelayForwardingDisabled: relay answers STUN but won't carry data, so a
	// client whose ICE fails fails fast instead of a doomed alloc. omitempty:
	// absent/false both mean "try the relay" (matches old servers).
	RelayForwardingDisabled bool `json:"relay_forwarding_disabled,omitempty"`
}

// JoinSessionRequest is the body of POST /v1/session/<slot>/join.
type JoinSessionRequest struct {
	ClientVersion string `json:"client_version"`
}

// JoinSessionResponse is the success body of POST /v1/session/<slot>/join.
type JoinSessionResponse struct {
	SessionID          string   `json:"session_id"`
	YourObservedAddr   string   `json:"your_observed_addr"`
	PeerObservedAddr   string   `json:"peer_observed_addr"`
	PeerIceCredentials IceCreds `json:"peer_ice_credentials"`
	YourIceCredentials IceCreds `json:"your_ice_credentials"`
	RoleToken          string   `json:"role_token"`
	// RelayAddr / RelayForwardingDisabled: see CreateSessionResponse.
	RelayAddr               string `json:"relay_addr,omitempty"`
	RelayForwardingDisabled bool   `json:"relay_forwarding_disabled,omitempty"`
}

// WaitRequest is the body of POST /v1/session/<slot>/wait. It carries no
// fields today — the slot in the path is the only input — but is kept as
// a named type so the wire shape has somewhere to grow.
type WaitRequest struct{}

// WaitResponse is the body the sender sees once a receiver pairs.
type WaitResponse struct {
	PeerObservedAddr   string   `json:"peer_observed_addr"`
	PeerIceCredentials IceCreds `json:"peer_ice_credentials"`
}

// CandidatesPushRequest is the body of POST /v1/session/<id>/candidates.
type CandidatesPushRequest struct {
	Candidates []string `json:"candidates"`
}

// CandidatesPullResponse is the body of GET /v1/session/<id>/candidates.
type CandidatesPullResponse struct {
	Candidates []string `json:"candidates"`
	NextSince  int      `json:"next_since"`
}

// RelayAllocateRequest is the body of POST /v1/relay/allocate.
type RelayAllocateRequest struct {
	SessionID string `json:"session_id"`
}

// RelayAllocateResponse is the body returned on relay allocation.
//
// ForwardingDisabled is set when the server does pairing + STUN only and
// won't carry data. RelayAddr is still returned (it's the STUN server);
// the client uses it for srflx gathering but skips the relay fallback.
// omitempty so older servers round-trip as false (forwarding capable).
//
// BudgetExhausted is set when the relay's daily byte budget is already
// spent. Like ForwardingDisabled it lets the client fail fast with a
// specific reason instead of dialing a slot that can't forward.
type RelayAllocateResponse struct {
	RelayAddr          string `json:"relay_addr"`
	SessionToken       string `json:"session_token"` // Crockford-base32 encoded 16 bytes
	TTLSeconds         int    `json:"ttl_seconds"`
	ForwardingDisabled bool   `json:"forwarding_disabled,omitempty"`
	BudgetExhausted    bool   `json:"budget_exhausted,omitempty"`
}

// HealthResponse is the body of GET /health. Deliberately minimal: a
// pure liveness signal (plus the 200/503 status code) with no version or
// uptime, so an unauthenticated caller learns nothing about the build.
// Version and uptime live on the gated /metrics for operators who need them.
type HealthResponse struct {
	Status string `json:"status"`
}

// RelayStatusResponse is the body of GET /v1/relay/status.
//
// State is one of:
//
//	active   – allocation is live, forwarding traffic
//	evicted  – allocation was torn down (see Reason)
//	unknown  – no allocation for this session_id
//
// Reason is set only when State == "evicted"; valid values are
// relay.ReasonCapHit and relay.ReasonIdle.
//
// LimitBytes is populated when Reason == ReasonCapHit. It is omitempty
// so older servers that don't set it round-trip as zero and the CLI
// falls back to the generic message instead of saying "0 MiB".
type RelayStatusResponse struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	LimitBytes uint64 `json:"limit_bytes,omitempty"`
}

// ErrorResponse is the body of any 4xx/5xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// IceCreds is a tiny ICE-credentials triple used during candidate exchange.
type IceCreds struct {
	Ufrag string `json:"ufrag"`
	Pwd   string `json:"pwd"`
}

// internal: a session lives in the server's in-memory table.
//
// SenderAddr / ReceiverAddr are the raw observed IPs returned to the
// peer in JoinSessionResponse.PeerObservedAddr — kept precise for the
// human-facing "Peer at <addr>" line. SenderRateKey / ReceiverRateKey
// are the rate-limit identities (raw v4, /64 for v6) — they're what
// indexes ipCounts and ipBucket, and they're stored separately so the
// decrement on session teardown lands on the same map key the
// increment used.
type session struct {
	ID              string
	Slot            string // argon2id stretch of the code — the server never holds the code itself
	SenderAddr      string
	SenderRateKey   string
	SenderICE       IceCreds
	SenderToken     string
	ReceiverAddr    string
	ReceiverRateKey string
	ReceiverICE     IceCreds
	ReceiverToken   string
	State           string // "waiting" | "paired" | "complete"
	CreatedAt       time.Time
	PairedAt        time.Time
	// LastSeen is touched by the sender's /wait long-poll (recurs every
	// LongPollTimeout while the client is alive). Sessions whose client
	// stopped polling are reclaimed after AbandonedTTL instead of
	// holding a per-IP slot for the full UnpairedTTL.
	LastSeen           time.Time
	SenderCandidates   []string
	ReceiverCandidates []string
	// relayToken is the shared relay session token, allocated lazily on
	// the first AllocateRelay call and reused for any subsequent calls
	// in the same session — both peers MUST end up with the same token
	// for the relay's source-addr de-mux to pair them.
	relayToken    relay.Token
	relayTokenSet bool
	waiters       chan struct{}
}
