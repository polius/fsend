// Package signaling is the client-side counterpart to internal/server.
//
// It speaks the same JSON HTTP API and exposes a tiny Go API to the rest
// of fsend so the CLI's send and receive flows don't need to know about
// URL paths, JSON shapes, or long-poll mechanics.
//
// The share code never leaves the client: it is the PAKE secret, and a
// pairing server that learned it could MITM the transfer. Every server
// round-trip identifies the session by the code's argon2id slot
// (internal/code.Slot) — in URLs, bodies, everywhere.
package signaling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/server"
)

// DefaultRequestTimeout is the http.Client ceiling applied to every
// request. Must stay comfortably above the server's LongPollTimeout
// (25s) so /wait long-polls complete via a server-side 204 rather than
// a client-side transport timeout that wastes the round-trip.
const DefaultRequestTimeout = 40 * time.Second

// Client talks to a fsend signaling endpoint.
type Client struct {
	baseURL  string
	hc       *http.Client
	version  string
	password string // optional: self-hosted server's shared secret
}

// New builds a client for the given http(s) base URL.
//
// `https://fsend.alzina.dev` and `http://localhost:8080` are both valid. The
// scheme is preserved as-is — callers control TLS via the URL.
func New(baseURL, clientVersion string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: DefaultRequestTimeout},
		version: clientVersion,
	}
}

// WithPassword attaches a shared-secret server password that the client
// presents in the X-Fsend-Auth header on every request. Empty disables
// the header. Returns c so the call composes with New.
func (c *Client) WithPassword(pw string) *Client {
	c.password = pw
	return c
}

// CreateResult is Create's return value: the server's session metadata
// plus the client-owned code. The server never returns (or sees) the
// code — only its argon2id slot — so it lives here, not in
// server.CreateSessionResponse.
type CreateResult struct {
	server.CreateSessionResponse
	Code string
}

// createAttempts bounds how many fresh codes Create tries when the
// server reports the slot taken (409). With ~2^45 codes an honest
// collision is effectively impossible, so one retry round-trip is
// already generous insurance.
const createAttempts = 3

// Create registers a new session, keyed server-side by the argon2id
// slot of the code — the raw code is never sent.
//
// codeStr is the code the caller has already shown the user (e.g., the
// LAN-phase code); a slot collision then surfaces as
// fserrors.ErrCodeAlreadyClaimed, because silently switching to a code
// the user never saw would strand the receiver. When codeStr is empty,
// Create generates the code itself and owns it — a 409 just means
// regenerate and retry. Callers read the final code from the result.
func (c *Client) Create(ctx context.Context, codeStr string) (*CreateResult, error) {
	generated := codeStr == ""
	for attempt := 1; ; attempt++ {
		if generated {
			var err error
			codeStr, err = code.Generate()
			if err != nil {
				return nil, fmt.Errorf("generating code: %w", err)
			}
		}
		var out server.CreateSessionResponse
		err := c.do(ctx, http.MethodPost, "/v1/session",
			server.CreateSessionRequest{ClientVersion: c.version, Slot: code.Slot(codeStr)}, &out, nil)
		if err == nil {
			return &CreateResult{CreateSessionResponse: out, Code: codeStr}, nil
		}
		if generated && errors.Is(err, fserrors.ErrCodeAlreadyClaimed) && attempt < createAttempts {
			continue
		}
		return nil, err
	}
}

// Join attempts to pair as a receiver, returning the sender's reflected
// address and ICE credentials. The session is addressed by the code's
// slot; the code itself stays on this machine.
func (c *Client) Join(ctx context.Context, codeStr string) (*server.JoinSessionResponse, error) {
	var out server.JoinSessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/session/"+code.Slot(codeStr)+"/join",
		server.JoinSessionRequest{ClientVersion: c.version}, &out, nil)
	return &out, err
}

// Wait long-polls until a receiver pairs with this session. Addressed
// by slot, like Join (code.Slot memoizes, so the repeated polls don't
// re-pay the argon2id stretch).
//
// Returns (nil, nil) on timeout (HTTP 204) — caller should retry until
// the session TTL expires.
func (c *Client) Wait(ctx context.Context, codeStr string) (*server.WaitResponse, error) {
	var out server.WaitResponse
	err := c.do(ctx, http.MethodPost, "/v1/session/"+code.Slot(codeStr)+"/wait",
		server.WaitRequest{}, &out, allowNoContent())
	if errors.Is(err, errNoContent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PushCandidates uploads a batch of ICE candidates for this peer.
// roleToken is the bearer credential returned from Create/Join; it
// tells the server which side this caller is, independent of source IP.
func (c *Client) PushCandidates(ctx context.Context, sessionID, roleToken string, candidates []string) error {
	return c.do(ctx, http.MethodPost, "/v1/session/"+sessionID+"/candidates",
		server.CandidatesPushRequest{Candidates: candidates}, nil, withAuth(roleToken))
}

// PullCandidates fetches candidates the peer has pushed since the `since`
// index. Returns (nil, nil) if nothing new is available yet.
func (c *Client) PullCandidates(ctx context.Context, sessionID, roleToken string, since int) (*server.CandidatesPullResponse, error) {
	var out server.CandidatesPullResponse
	path := fmt.Sprintf("/v1/session/%s/candidates?since=%d", sessionID, since)
	err := c.do(ctx, http.MethodGet, path, nil, &out, withAuth(roleToken).withNoContent())
	if errors.Is(err, errNoContent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete cleans up a session after a successful handshake. roleToken
// authenticates the caller as a party to the session — the server gates
// the delete on it so a third party who learns a session ID can't tear
// down someone else's session.
func (c *Client) Delete(ctx context.Context, sessionID, roleToken string) error {
	return c.do(ctx, http.MethodDelete, "/v1/session/"+sessionID, nil, nil, withAuth(roleToken))
}

// AllocateRelay asks the server for a relay token for this session.
//
// roleToken is the bearer credential the caller obtained from Create or
// Join. The server uses it to confirm the caller is actually one of the
// session's peers before minting a relay token.
func (c *Client) AllocateRelay(ctx context.Context, sessionID, roleToken string) (*server.RelayAllocateResponse, error) {
	var out server.RelayAllocateResponse
	err := c.do(ctx, http.MethodPost, "/v1/relay/allocate",
		server.RelayAllocateRequest{SessionID: sessionID}, &out, withAuth(roleToken))
	return &out, err
}

// RelayStatus probes the pairing server for the current state of a
// relay allocation. Used after a relay-path drop to surface the real
// reason (e.g. cap_hit) instead of looping on retry forever. roleToken
// authenticates the caller as a party to the session.
//
// Network errors are treated as "unknown" — we don't want a probe
// failure to mask the underlying transfer failure the caller is
// already handling.
func (c *Client) RelayStatus(ctx context.Context, sessionID, roleToken string) (*server.RelayStatusResponse, error) {
	var out server.RelayStatusResponse
	path := "/v1/relay/status?session_id=" + sessionID
	if err := c.do(ctx, http.MethodGet, path, nil, &out, withAuth(roleToken)); err != nil {
		return nil, err
	}
	return &out, nil
}

// errNoContent is the sentinel `do` returns when the server replied 204
// AND the caller opted into noContentOK behavior. Callers translate it
// to a typed nil-response.
var errNoContent = errors.New("signaling: no content")

// doOption mutates the request behavior. Used to surface optional
// 204-handling and bearer-auth without growing the do() signature.
type doOption struct {
	noContentOK bool
	bearerToken string
}

func allowNoContent() *doOption { return &doOption{noContentOK: true} }

func withAuth(token string) *doOption { return &doOption{bearerToken: token} }

func (o *doOption) withNoContent() *doOption {
	if o == nil {
		return allowNoContent()
	}
	o.noContentOK = true
	return o
}

// do is the common path for "send JSON, expect JSON" round-trips.
//
// in/out may be nil. When out is set, the response body is decoded into
// it. Status-code mapping:
//
//   - 2xx with body → decode into out (if non-nil)
//   - 204           → returns errNoContent if opt.noContentOK, else nil
//   - 404           → fserrors.ErrCodeNotFound
//   - 409           → fserrors.ErrCodeAlreadyClaimed
//   - 410           → fserrors.ErrServerRetired
//   - 429           → fserrors.ErrRateLimited
//   - other 4xx/5xx → opaque error with body excerpt
func (c *Client) do(ctx context.Context, method, path string, in, out any, opt *doOption) error {
	var rdr io.Reader
	if in != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(in); err != nil {
			return err
		}
		rdr = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "fsend/"+c.version)
	if opt != nil && opt.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+opt.bearerToken)
	}
	if c.password != "" {
		req.Header.Set("X-Fsend-Auth", c.password)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return mapNetworkErr(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNoContent {
		if opt != nil && opt.noContentOK {
			return errNoContent
		}
		return nil
	}
	if err := statusToFsendErr(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func statusToFsendErr(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fserrors.ErrCodeNotFound
	case http.StatusConflict:
		return fserrors.ErrCodeAlreadyClaimed
	case http.StatusGone:
		return fserrors.ErrServerRetired
	case http.StatusTooManyRequests:
		return fserrors.ErrRateLimited
	case http.StatusUnauthorized:
		return fserrors.ErrServerAuthRequired
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("signaling: status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
}

func mapNetworkErr(err error) error {
	if err == nil {
		return nil
	}
	// User-initiated cancel (Ctrl-C) must surface as-is so the CLI can
	// distinguish "user aborted" from "server didn't respond".
	if errors.Is(err, context.Canceled) {
		return err
	}
	// A DeadlineExceeded here means the http.Client's 30s Timeout fired
	// (no signaling call uses a caller-supplied deadline), which is the
	// firewall-drops-our-packets flavor of "server unreachable". Map it
	// to the same sentinel as connection-refused / DNS-failure so the
	// catalog renders E001 instead of E099.
	return fmt.Errorf("%w: %v", fserrors.ErrServerUnreachable, err)
}
