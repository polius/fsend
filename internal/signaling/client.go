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

// DefaultRequestTimeout caps individual HTTP requests; longer-polling
// endpoints set their own timeouts via context.
const DefaultRequestTimeout = 30 * time.Second

// Client talks to an fsend-server signaling endpoint.
type Client struct {
	baseURL string
	hc      *http.Client
	version string
}

// New builds a client for the given http(s) base URL.
//
// `https://fs.alzina.dev` and `http://localhost:8080` are both valid. The
// scheme is preserved as-is — callers control TLS via the URL.
func New(baseURL, clientVersion string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: DefaultRequestTimeout},
		version: clientVersion,
	}
}

// Create requests a new session. The returned response carries the code
// the sender must share out-of-band.
func (c *Client) Create(ctx context.Context) (*server.CreateSessionResponse, error) {
	var out server.CreateSessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/session", server.CreateSessionRequest{ClientVersion: c.version}, &out)
	return &out, err
}

// Join attempts to pair as a receiver, returning the sender's reflected
// address and ICE credentials.
//
// Errors are mapped to fserrors sentinels so the CLI can render the
// user-facing message correctly.
func (c *Client) Join(ctx context.Context, code string) (*server.JoinSessionResponse, error) {
	var out server.JoinSessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/session/"+code+"/join", server.JoinSessionRequest{ClientVersion: c.version}, &out)
	return &out, err
}

// Wait long-polls until a receiver pairs with this session.
//
// Returns nil + nil on timeout (HTTP 204) — caller should retry until the
// session TTL expires.
func (c *Client) Wait(ctx context.Context, code string) (*server.WaitResponse, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/session/"+code+"/wait", server.WaitRequest{})
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, mapNetworkErr(err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var w server.WaitResponse
		if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
			return nil, fmt.Errorf("decode wait response: %w", err)
		}
		return &w, nil
	case http.StatusNoContent:
		return nil, nil
	case http.StatusNotFound:
		return nil, fserrors.ErrCodeNotFound
	default:
		return nil, fmt.Errorf("wait: unexpected status %d", resp.StatusCode)
	}
}

// PushCandidates uploads a batch of ICE candidates for this peer.
func (c *Client) PushCandidates(ctx context.Context, sessionID string, candidates []string) error {
	return c.do(ctx, http.MethodPost, "/v1/session/"+sessionID+"/candidates",
		server.CandidatesPushRequest{Candidates: candidates}, nil)
}

// PullCandidates fetches candidates the peer has pushed since the `since`
// index. Returns nil + nil if nothing new is available yet.
func (c *Client) PullCandidates(ctx context.Context, sessionID string, since int) (*server.CandidatesPullResponse, error) {
	url := fmt.Sprintf("/v1/session/%s/candidates?since=%d", sessionID, since)
	req, err := c.newRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, mapNetworkErr(err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var out server.CandidatesPullResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, err
		}
		return &out, nil
	case http.StatusNoContent:
		return nil, nil
	case http.StatusNotFound:
		return nil, fserrors.ErrCodeNotFound
	default:
		return nil, fmt.Errorf("pull candidates: status %d", resp.StatusCode)
	}
}

// Delete cleans up a session after a successful handshake.
//
// Best-effort: errors are logged at the call site, not surfaced.
func (c *Client) Delete(ctx context.Context, sessionID string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/session/"+sessionID, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return mapNetworkErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("delete: status %d", resp.StatusCode)
	}
	return nil
}

// Health pings /v1/health and returns the parsed response.
func (c *Client) Health(ctx context.Context) (*server.HealthResponse, error) {
	var out server.HealthResponse
	err := c.do(ctx, http.MethodGet, "/v1/health", nil, &out)
	return &out, err
}

// do is the common path for "send JSON, expect JSON" round-trips.
//
// Out may be nil; if set, the response body is decoded into it.
// Maps 404 to fserrors.ErrCodeNotFound, 409 to ErrCodeAlreadyClaimed,
// 410 to ErrServerRetired, 429 to ErrRateLimited.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	req, err := c.newRequest(ctx, method, path, in)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return mapNetworkErr(err)
	}
	defer resp.Body.Close()

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

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
		rdr = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "fsend/"+c.version)
	return req, nil
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
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("signaling: status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
}

func mapNetworkErr(err error) error {
	if err == nil {
		return nil
	}
	// Surface as ErrServerUnreachable for any network-level failure so
	// the CLI renders the right message.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w: %v", fserrors.ErrServerUnreachable, err)
}
