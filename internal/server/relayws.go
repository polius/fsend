package server

import (
	"net/http"

	"github.com/polius/fsend/internal/relay"
)

// relayWSServer is the relay's TCP fallback (internal/relay.Server). It is
// optional on the allocator, so test fakes and relays without it still fit.
type relayWSServer interface {
	ServeWS(http.ResponseWriter, *http.Request, relay.Token)
	ProbeWS(http.ResponseWriter, *http.Request)
}

// relayTCPFallback returns the relay's WebSocket side, or nil when there is
// none to offer (no relay, no WebSocket support, or forwarding disabled).
func (s *Server) relayTCPFallback() relayWSServer {
	if s.relayAllocator == nil || !s.relayAllocator.Forwarding() {
		return nil
	}
	ws, _ := s.relayAllocator.(relayWSServer)
	return ws
}

// relayWebSocket is GET /v1/relay/ws: the relay over a WebSocket, for
// clients whose network blocks UDP. With a session_id it is authenticated
// like allocateRelay and carries that session's relay traffic; without one
// it is a reachability probe for `fsend doctor`, which also learns the
// relay's UDP address from the RelayAddrHeader.
func (s *Server) relayWebSocket(w http.ResponseWriter, r *http.Request) {
	ws := s.relayTCPFallback()
	if ws == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "relay TCP fallback not available on this server")
		return
	}
	id := r.URL.Query().Get("session_id")
	if id == "" {
		w.Header().Set(RelayAddrHeader, s.relayAddrFor(r))
		ws.ProbeWS(w, r)
		return
	}
	s.mu.Lock()
	sess, ok := s.byID[id]
	if !ok || !sess.relayTokenSet {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "relay not allocated for this session")
		return
	}
	if _, authed := sess.sideForToken(bearerToken(r)); !authed {
		s.mu.Unlock()
		writeJSONError(w, http.StatusUnauthorized, "missing or invalid role token")
		return
	}
	tok := sess.relayToken
	s.mu.Unlock()
	ws.ServeWS(w, r, tok)
}
