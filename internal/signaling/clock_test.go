package signaling

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/server"
)

// skewed returns a client whose clock runs off from the real one (and so
// from the test server's Date header) by off.
func skewed(url string, off time.Duration) *Client {
	c := New(url, "test")
	c.clock = func() time.Time { return time.Now().Add(off) }
	return c
}

// Slots are salted with the UTC day. A device whose clock is more than a
// day off used to derive another slot and get "no sender" (E002); the
// server's Date header now gives both sides the same day.
func TestClient_WrongClockStillPairs(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct{ sender, receiver time.Duration }{
		"receiver behind": {0, -30 * time.Hour},
		"receiver ahead":  {0, 50 * time.Hour},
		"sender behind":   {-30 * time.Hour, 0},
		"both wrong":      {-30 * time.Hour, 75 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			url, _ := setupServer(t)
			created, err := skewed(url, tc.sender).Create(ctx, "")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			joined, err := skewed(url, tc.receiver).Join(ctx, created.Code)
			if err != nil {
				t.Fatalf("Join: %v", err)
			}
			if joined.SessionID != created.SessionID {
				t.Fatalf("joined session %q, created %q", joined.SessionID, created.SessionID)
			}
		})
	}
}

// When no sender is there, a device with a wrong clock hears about its
// clock as well; one within the tolerance does not.
func TestClient_NotFoundNamesWrongClock(t *testing.T) {
	url, _ := setupServer(t)
	ctx := context.Background()

	_, err := skewed(url, -30*time.Hour).Join(ctx, "abc-defg-hjk")
	var skewErr *ClockSkewError
	if !errors.Is(err, fserrors.ErrCodeNotFound) || !errors.As(err, &skewErr) {
		t.Fatalf("got %v, want ErrCodeNotFound carrying a ClockSkewError", err)
	}
	if d := skewErr.Detail(); !strings.Contains(d, "30 h behind") {
		t.Errorf("detail %q does not say the clock is 30 h behind", d)
	}

	_, err = skewed(url, 2*time.Minute).Join(ctx, "abc-defg-hjk")
	if !errors.Is(err, fserrors.ErrCodeNotFound) || errors.As(err, &skewErr) {
		t.Fatalf("a 2-minute skew: got %v, want a plain ErrCodeNotFound", err)
	}
}

func TestClockSkewDetail(t *testing.T) {
	for skew, want := range map[time.Duration]string{
		7 * time.Minute:                 "7 min behind",
		-(3*time.Hour + 12*time.Minute): "3 h 12 min ahead of",
		26 * time.Hour:                  "26 h behind",
		-73 * time.Hour:                 "3 days ahead of",
	} {
		if got := ClockSkewDetail(skew); !strings.Contains(got, want) {
			t.Errorf("ClockSkewDetail(%v) = %q, want it to contain %q", skew, got, want)
		}
	}
	if _, ok := ServerSkew(http.Header{}, time.Now()); ok {
		t.Error("ServerSkew reported a skew without a Date header")
	}
}

// The server pairs the receiver but the response never arrives: the join
// is retried with the same nonce and gets the pairing, not "already
// claimed".
func TestClient_JoinSurvivesLostResponse(t *testing.T) {
	s := server.New(server.Config{ServerVersion: "0.0.0-test", UnpairedTTL: 2 * time.Second, LongPollTimeout: 500 * time.Millisecond})
	inner := s.Handler()
	var dropped atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/join") && dropped.CompareAndSwap(false, true) {
			inner.ServeHTTP(httptest.NewRecorder(), r) // the server pairs...
			conn, _, err := w.(http.Hijacker).Hijack() // ...and the answer is lost
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()

	ctx := context.Background()
	created, err := New(ts.URL, "test").Create(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	joined, err := New(ts.URL, "test").Join(ctx, created.Code)
	if err != nil {
		t.Fatalf("Join after a lost response: %v", err)
	}
	if !dropped.Load() || joined.SessionID != created.SessionID {
		t.Fatalf("dropped=%v, joined %q, created %q", dropped.Load(), joined.SessionID, created.SessionID)
	}
}
