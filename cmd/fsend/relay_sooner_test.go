package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
)

// On a network where no direct path is possible and UDP to the server is
// blocked (so neither side has a server-reflexive candidate), both sides
// reach the relay's TCP fallback in about 2 s. It used to take the whole
// ICE budget, then the UDP probe, then the TCP dial: about 7 s.
func TestInternetPath_RelaySoonerWithoutDirectPath(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	blackhole, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rs := relay.NewServer(udp, relay.ServerConfig{Logger: logger})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rs.Run(ctx); close(done) }()
	// The advertised relay (and STUN) port is a black hole: UDP blocked.
	inner := server.New(server.Config{Logger: logger}).WithRelay(rs, blackhole.LocalAddr().(*net.UDPAddr).Port).Handler()
	// ICE candidates never cross: no direct path. The sender's path choice
	// still does.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/candidates") {
			var req server.CandidatesPushRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			var keep []string
			for _, c := range req.Candidates {
				if strings.HasPrefix(c, pathChoicePrefix) {
					keep = append(keep, c)
				}
			}
			b, _ := json.Marshal(server.CandidatesPushRequest{Candidates: keep})
			r.Body = io.NopCloser(bytes.NewReader(b))
			r.ContentLength = int64(len(b))
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.Close()
		cancel()
		_ = udp.Close()
		_ = blackhole.Close()
		<-done
	})

	sc, rc := signaling.New(ts.URL, "test"), signaling.New(ts.URL, "test")
	created, err := sc.Create(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	joined, err := rc.Join(ctx, created.Code)
	if err != nil {
		t.Fatal(err)
	}
	waited, err := sc.Wait(ctx, created.Code)
	if err != nil || waited == nil {
		t.Fatalf("Wait: %v, %v", waited, err)
	}

	start := time.Now()
	var wg sync.WaitGroup
	var senderPath, receiverPath connpath.Info
	var senderErr, receiverErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		var pc net.PacketConn
		pc, senderPath, senderErr = establishInternetDataPath(ctx, &flags{}, sc, created, waited)
		if pc != nil {
			_ = pc.Close()
		}
	}()
	go func() {
		defer wg.Done()
		var link *recvLink
		link, receiverErr = linkViaInternet(ctx, &flags{}, rc, joined)
		if link != nil {
			receiverPath = link.path
			link.close()
		}
	}()
	wg.Wait()
	elapsed := time.Since(start)
	if senderErr != nil || receiverErr != nil {
		t.Fatalf("sender: %v; receiver: %v", senderErr, receiverErr)
	}
	for side, p := range map[string]connpath.Info{"sender": senderPath, "receiver": receiverPath} {
		if p.Kind != connpath.KindRelay || !strings.HasSuffix(p.RelayAddr, "over TCP") {
			t.Errorf("%s path = %+v, want the relay's TCP fallback", side, p)
		}
	}
	if elapsed > 4*time.Second {
		t.Errorf("reaching the relay took %v, want about 2 s (the ICE budget alone is %v)", elapsed, iceBudget)
	}
}

// A re-dial that fails after a drop goes straight to finding the sender
// again instead of spending more dials (each up to 15 s) on a dead route.
func TestReceiveOver_FailedRedialRediscovers(t *testing.T) {
	timeout := &net.OpError{Op: "read", Err: errTimeoutForTest{}}
	for _, tc := range []struct {
		canRediscover bool
		wantDials     int32
	}{{true, 1}, {false, 2}} {
		var dials atomic.Int32
		link := &recvLink{dial: func(context.Context, string) (*quicconn.AcceptResult, error) {
			dials.Add(1)
			return nil, timeout
		}}
		rs := &recvSession{key: codeKey("abc-defg-hjk"), resumed: true}
		f := &flags{quiet: true}
		ui := newReceiverUI(context.Background(), f, t.TempDir(), false, connpath.Info{})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := receiveOver(ctx, f, rs, ui, false, link, tc.canRediscover)
		cancel()
		if !errors.Is(err, fserrors.ErrTransientFailure) {
			t.Errorf("canRediscover=%v: got %v, want ErrTransientFailure (which re-discovers)", tc.canRediscover, err)
		}
		if got := dials.Load(); tc.canRediscover && got != tc.wantDials || !tc.canRediscover && got < tc.wantDials {
			t.Errorf("canRediscover=%v: %d dials, want %d", tc.canRediscover, got, tc.wantDials)
		}
	}
}

type errTimeoutForTest struct{}

func (errTimeoutForTest) Error() string   { return "i/o timeout" }
func (errTimeoutForTest) Timeout() bool   { return true }
func (errTimeoutForTest) Temporary() bool { return true }
