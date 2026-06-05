// Package server implements the rendezvous + relay-allocation HTTP
// signaling endpoints for fsend-server.
//
// API surface and JSON shapes follow docs/decisions/signaling-protocol.md.
// All endpoints are under /v1/...
package server

import (
	"time"

	"github.com/polius/fsend/internal/relay"
)

// CreateSessionRequest is the body of POST /v1/session.
type CreateSessionRequest struct {
	ClientVersion string `json:"client_version"`
}

// CreateSessionResponse is the success body of POST /v1/session.
type CreateSessionResponse struct {
	SessionID        string      `json:"session_id"`
	Code             string      `json:"code"`
	YourObservedAddr string      `json:"your_observed_addr"`
	IceCredentials   IceCreds    `json:"ice_credentials"`
	TTLSeconds       int         `json:"ttl_seconds"`
	ServerVersion    string      `json:"server_version"`
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
	ReceiverAddr       string
	ReceiverICE        IceCreds
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
