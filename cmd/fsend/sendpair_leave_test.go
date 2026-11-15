package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
)

// joinUntil joins code, retrying while the server has no session for it
// or the session is still the previous receiver's.
func joinUntil(ctx context.Context, t *testing.T, c *signaling.Client, code string) *server.JoinSessionResponse {
	t.Helper()
	for {
		joined, err := c.Join(ctx, code)
		if err == nil {
			return joined
		}
		if ctx.Err() != nil {
			t.Fatalf("join: %v", err)
		}
		if !errors.Is(err, fserrors.ErrCodeNotFound) && !errors.Is(err, fserrors.ErrCodeAlreadyClaimed) {
			t.Fatalf("join: %v", err)
		}
		// Every join counts against the server's per-address budget.
		time.Sleep(250 * time.Millisecond)
	}
}

// A receiver that joins and gives up before authenticating leaves the
// session, and the sender opens the code again within seconds: a second
// receiver pairs, with the timings the command ships. Before, the code
// stayed claimed for the sender's whole first-accept timeout (a minute),
// and the second receiver was told it belonged to someone else.
func TestPairOverInternet_ReceiverThatLeavesFreesTheCode(t *testing.T) {
	for _, stage := range []string{"right after the join", "with its data path up"} {
		t.Run(stage, func(t *testing.T) {
			_, baseURL := newSignalingTestServer(t)
			const code = "abc-defg-jkm"
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var vanished int32
			type result struct {
				p   *internetSenderPairing
				err error
			}
			done := make(chan result, 1)
			go func() {
				p, err := pairOverInternet(ctx, &flags{quiet: true}, &sendRound{key: codeKey(code)},
					&config.Config{Server: baseURL}, func(error) {}, func() { atomic.AddInt32(&vanished, 1) })
				done <- result{p, err}
			}()

			first := signaling.New(baseURL, "test")
			joined := joinUntil(ctx, t, first, code)
			if stage == "with its data path up" {
				link, err := linkViaInternet(ctx, &flags{quiet: true}, first, joined)
				if err != nil {
					t.Fatalf("first receiver's link: %v", err)
				}
				defer link.close()
			}
			leaveSession(first, joined)
			left := time.Now()

			second := signaling.New(baseURL, "test")
			joined2 := joinUntil(ctx, t, second, code)
			reopened := time.Since(left)
			if joined2.SessionID == joined.SessionID {
				t.Fatal("the second receiver joined the session the first one left")
			}
			// Watch interval plus grace is 3 s; the rest is ICE giving up
			// on the first receiver when it left before its path was up.
			if reopened > 12*time.Second {
				t.Errorf("the code took %v to open again", reopened)
			}
			t.Logf("code open again %v after the first receiver left", reopened.Round(10*time.Millisecond))

			link2, err := linkViaInternet(ctx, &flags{quiet: true}, second, joined2)
			if err != nil {
				t.Fatalf("second receiver's link: %v", err)
			}
			defer link2.close()
			res, err := link2.dial(ctx, code)
			if err != nil {
				t.Fatalf("second receiver's handshake: %v", err)
			}
			defer res.Close()
			select {
			case r := <-done:
				if r.err != nil {
					t.Fatalf("the sender did not pair with the second receiver: %v", r.err)
				}
				r.p.cleanup()
			case <-ctx.Done():
				t.Fatal("the sender never paired with the second receiver")
			}
			// A receiver that leaves right after its join can be gone
			// before the sender's next poll has seen it join. The sender
			// then finds its session missing while it still waits for a
			// receiver, registers the code again as it does after a server
			// restart, and never announces a receiver that vanished: the
			// code opens just the same (and sooner). With the data path up
			// the sender has seen the join, so it must say so.
			got := atomic.LoadInt32(&vanished)
			if stage == "with its data path up" && got != 1 {
				t.Errorf("the sender went back to waiting %d times, want 1", got)
			}
			if got > 1 {
				t.Errorf("the sender went back to waiting %d times, want at most 1", got)
			}
		})
	}
}

// A session that disappears for another reason (a server restart) does
// not cost a receiver whose connection is already on its way: the sender
// keeps accepting for the grace period.
func TestPairOverInternet_SessionGoneGraceLetsTheReceiverFinish(t *testing.T) {
	// A grace far longer than a handshake takes, also on a loaded CI
	// runner: the test ends as soon as the receiver is in, not when the
	// grace does.
	origEvery, origGrace := sessionWatchEvery, sessionGoneGrace
	sessionWatchEvery, sessionGoneGrace = 20*time.Millisecond, 45*time.Second
	t.Cleanup(func() { sessionWatchEvery, sessionGoneGrace = origEvery, origGrace })

	_, baseURL := newSignalingTestServer(t)
	// polls counts the sender's watch polls (their since is past any
	// candidate list); told counts those the server answered with "no
	// such session".
	var creates, polls, told int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/session" {
			atomic.AddInt32(&creates, 1)
		}
		watch := r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/candidates") && strings.Contains(r.URL.RawQuery, "since=1073741824")
		if watch {
			atomic.AddInt32(&polls, 1)
		}
		rec := &statusWriter{ResponseWriter: w}
		proxyTo(t, baseURL, rec, r)
		if watch && rec.status == http.StatusNotFound {
			atomic.AddInt32(&told, 1)
		}
	}))
	defer ts.Close()

	const code = "abc-defg-jkm"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The sender's side stays up until the receiver has its answer: the
	// sender returns once its own confirmation is queued, and tearing the
	// path down at that moment can drop it before it is on the wire.
	type paired struct {
		p   *internetSenderPairing
		err error
	}
	done := make(chan paired, 1)
	go func() {
		p, err := pairOverInternet(ctx, &flags{quiet: true}, &sendRound{key: codeKey(code)},
			&config.Config{Server: ts.URL}, func(error) {}, nil)
		done <- paired{p, err}
	}()
	var sender *paired
	defer func() {
		if sender == nil {
			r := <-done
			sender = &r
		}
		if sender.err == nil {
			sender.p.cleanup()
		}
	}()

	rc := signaling.New(ts.URL, "test")
	joined := joinUntil(ctx, t, rc, code)
	link, err := linkViaInternet(ctx, &flags{quiet: true}, rc, joined)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	defer link.close()
	// The watch only runs while the sender waits for the first connection:
	// its first poll says the sender's side of the path is up too.
	for atomic.LoadInt32(&polls) < 1 && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	// "The server restarted": the session is gone, the receiver is not.
	if err := rc.Delete(ctx, joined.SessionID, joined.RoleToken); err != nil {
		t.Fatal(err)
	}
	// Wait until the sender has been told. Not "one more poll": the watch
	// stops polling at the first 404, so a poll that lands between the
	// Delete and a count taken here would be the last one there is.
	for atomic.LoadInt32(&told) < 1 && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	res, err := link.dial(ctx, code)
	if err != nil {
		// What the sender was doing says which side dropped the ball.
		state := "still waiting"
		select {
		case r := <-done:
			sender = &r
			state = fmt.Sprintf("returned %v", r.err)
		default:
		}
		t.Fatalf("handshake inside the grace period: %v (sender %s; %d session(s) created, %d watch polls, %d answered 404)",
			err, state, atomic.LoadInt32(&creates), atomic.LoadInt32(&polls), atomic.LoadInt32(&told))
	}
	defer res.Close()
	r := <-done
	sender = &r
	if r.err != nil {
		t.Fatalf("the sender gave up on a receiver that was still coming: %v", r.err)
	}
	if got := atomic.LoadInt32(&creates); got != 1 {
		t.Errorf("Create called %d times, want 1", got)
	}
}

// Only a clear "no such session" ends the wait: a server that cannot be
// reached, or answers with an error, says nothing about the receiver.
func TestWatchSessionGone_IgnoresAnUnreachableServer(t *testing.T) {
	origEvery, origGrace := sessionWatchEvery, sessionGoneGrace
	sessionWatchEvery, sessionGoneGrace = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { sessionWatchEvery, sessionGoneGrace = origEvery, origGrace })

	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan struct{})
	go watchSessionGone(ctx, signaling.New(ts.URL, "test"), &signaling.CreateResult{SessionID: "s", RoleToken: "t"},
		func() { close(cancelled) })

	for hits.Load() < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-cancelled:
		t.Fatal("a failing server ended the wait for the receiver")
	default:
	}
	status.Store(http.StatusNotFound)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("a deleted session did not end the wait")
	}
}

// statusWriter remembers the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
