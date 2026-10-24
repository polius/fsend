package relay

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// startWS serves the relay's WebSocket endpoints for one allocation.
func startWS(t *testing.T, srv *Server, tok Token) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) { srv.ServeWS(w, r, tok) })
	mux.HandleFunc("/probe", srv.ProbeWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func dialWSClient(t *testing.T, url string, tok Token) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := DialWS(ctx, url, nil, tok, PeerIDFor(t.Name()+url))
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestWS_PairsWithUDP: a TCP-fallback peer and a UDP peer share one
// allocation, in both directions — including a datagram over 1500 bytes.
func TestWS_PairsWithUDP(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{})
	tok, _ := srv.Allocate()
	ts := startWS(t, srv, tok)

	udp := registered(t, relayAddr, tok)
	ws := dialWSClient(t, ts.URL+"/ws", tok)

	if _, err := udp.WriteTo([]byte("udp→ws"), nil); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, ws, []byte("udp→ws"))
	big := bytes.Repeat([]byte{'w'}, 1600)
	if _, err := ws.WriteTo(big, nil); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, udp, big)
	if got := srv.Metrics().TCPFallbackTotal; got != 1 {
		t.Errorf("tcp_fallback_total = %d, want 1", got)
	}
}

func TestWS_BothSides(t *testing.T) {
	srv, _ := startRelay(t, ServerConfig{})
	tok, _ := srv.Allocate()
	ts := startWS(t, srv, tok)
	a := dialWSClient(t, ts.URL+"/ws?a", tok)
	b := dialWSClient(t, ts.URL+"/ws?b", tok)
	_, _ = a.WriteTo([]byte("ping"), nil)
	expectPayload(t, b, []byte("ping"))
	_, _ = b.WriteTo([]byte("pong"), nil)
	expectPayload(t, a, []byte("pong"))
}

func TestWS_RefusesUnknownAllocation(t *testing.T) {
	srv, _ := startRelay(t, ServerConfig{})
	ts := startWS(t, srv, Token{0xFF}) // never allocated
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := DialWS(ctx, ts.URL+"/ws", nil, Token{0xFF}, PeerID{})
	var se *HTTPStatusError
	if !errors.As(err, &se) || se.Code != http.StatusGone {
		t.Fatalf("DialWS on an unknown allocation = %v, want HTTP 410", err)
	}
}

func TestProbeWS(t *testing.T) {
	srv, _ := startRelay(t, ServerConfig{})
	ts := startWS(t, srv, Token{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := ProbeWS(ctx, ts.URL+"/probe", nil); err != nil {
		t.Fatalf("ProbeWS: %v", err)
	}
	_, err := ProbeWS(ctx, ts.URL+"/nope", nil)
	var se *HTTPStatusError
	if !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("ProbeWS on a server without the endpoint = %v, want HTTP 404", err)
	}
}

// A read deadline set while ReadFrom blocks must wake it — quic-go stops
// its read loop that way on Transport.Close.
func TestWSPacketConn_DeadlineWakesReader(t *testing.T) {
	srv, _ := startRelay(t, ServerConfig{})
	tok, _ := srv.Allocate()
	ts := startWS(t, srv, tok)
	c := dialWSClient(t, ts.URL+"/ws", tok)
	errc := make(chan error, 1)
	go func() {
		_, _, err := c.ReadFrom(make([]byte, MaxDatagram))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = c.SetReadDeadline(time.Now())
	select {
	case err := <-errc:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("ReadFrom = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetReadDeadline did not wake a blocked ReadFrom")
	}
}
