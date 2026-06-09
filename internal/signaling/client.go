// Package signaling is the client-side counterpart to internal/server.
//
// It speaks the same JSON HTTP API and exposes a tiny Go API to the rest
// of fsend so the CLI's send and receive flows don't need to know about
// URL paths, JSON shapes, or long-poll mechanics.
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

// Create requests a new session. The returned response carries the code
// the sender must share out-of-band.
//
// suggestedCode lets the caller propose the code they've already shown
// the user (e.g., the LAN-phase code). When non-empty and free on the
// server, the server adopts it. Empty / invalid / taken values fall
// back to server-side generation. Callers should always read the actual
// code from the response, not assume it equals their suggestion.
func (c *Client) Create(ctx context.Context, suggestedCode string) (*server.CreateSessionResponse, error) {
	var out server.CreateSessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/session",
		server.CreateSessionRequest{ClientVersion: c.version, Code: suggestedCode}, &out, nil)
	return &out, err
}

// Join attempts to pair as a receiver, returning the sender's reflected
// address and ICE credentials.
func (c *Client) Join(ctx context.Context, code string) (*server.JoinSessionResponse, error) {
	var out server.JoinSessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/session/"+code+"/join",
		server.JoinSessionRequest{ClientVersion: c.version}, &out, nil)
	return &out, err
}

// Wait long-polls until a receiver pairs with this session.
//
// Returns (nil, nil) on timeout (HTTP 204) — caller should retry until
// the session TTL expires.
func (c *Client) Wait(ctx context.Context, code string) (*server.WaitResponse, error) {
	var out server.WaitResponse
	err := c.do(ctx, http.MethodPost, "/v1/session/"+code+"/wait",
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

// Delete cleans up a session after a successful handshake.
func (c *Client) Delete(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/session/"+sessionID, nil, nil, nil)
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
