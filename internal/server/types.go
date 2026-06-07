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
// Code is optional. When the client supplies one (and it matches the
// canonical code format and isn't currently in use), the server adopts
// it instead of generating a fresh one. The client-suggested-code path
// exists so the sender can register on the pairing server with the
// same code it already announced on LAN — eliminating the "different
// code on LAN vs internet" race that otherwise leaves receivers with
// an E002 from the server. On empty/invalid/taken code, the server
// silently falls back to generation; the actual code is always echoed
// back in CreateSessionResponse.Code.
type CreateSessionRequest struct {
	ClientVersion string `json:"client_version"`
	Code          string `json:"code,omitempty"`
}

// CreateSessionResponse is the success body of POST /v1/session.
//
// RoleToken is an opaque bearer credential the sender includes on
// subsequent /candidates calls so the server can route candidate batches
// to the right side without relying on source IP. Without this, two
// peers behind the same NAT can't disambiguate.
type CreateSessionResponse struct {
	SessionID        string   `json:"session_id"`
	Code             string   `json:"code"`
	YourObservedAddr string   `json:"your_observed_addr"`
	IceCredentials   IceCreds `json:"ice_credentials"`
	TTLSeconds       int      `json:"ttl_seconds"`
	ServerVersion    string   `json:"server_version"`
	RoleToken        string   `json:"role_token"`
}

// JoinSessionRequest is the body of POST /v1/session/<code>/join.
type JoinSessionRequest struct {
	ClientVersion string `json:"client_version"`
}

// JoinSessionResponse is the success body of POST /v1/session/<code>/join.
type JoinSessionResponse struct {
	SessionID          string   `json:"session_id"`
	YourObservedAddr   string   `json:"your_observed_addr"`
	PeerObservedAddr   string   `json:"peer_observed_addr"`
	PeerIceCredentials IceCreds `json:"peer_ice_credentials"`
	YourIceCredentials IceCreds `json:"your_ice_credentials"`
	RoleToken          string   `json:"role_token"`
}

// WaitRequest is the body of POST /v1/session/<code>/wait.
type WaitRequest struct {
	Since string `json:"since,omitempty"` // optional resume marker
}

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
	Ended      bool     `json:"ended"`
}

// RelayAllocateRequest is the body of POST /v1/relay/allocate.
type RelayAllocateRequest struct {
	SessionID string `json:"session_id"`
}

// RelayAllocateResponse is the body returned on relay allocation.
type RelayAllocateResponse struct {
	RelayAddr    string `json:"relay_addr"`
	SessionToken string `json:"session_token"` // Crockford-base32 encoded 16 bytes
	TTLSeconds   int    `json:"ttl_seconds"`
}

// HealthResponse is the body of GET /v1/health.
type HealthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
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
type RelayStatusResponse struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
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
type session struct {
	ID                 string
	Code               string
	SenderAddr         string
	SenderICE          IceCreds
	SenderToken        string
	ReceiverAddr       string
	ReceiverICE        IceCreds
	ReceiverToken      string
	State              string // "waiting" | "paired" | "complete"
	CreatedAt          time.Time
	PairedAt           time.Time
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
