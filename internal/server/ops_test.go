package server

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Default (info) logs must not carry the client IP or the slot; debug may.
func TestAccessLog_PrivateByDefault(t *testing.T) {
	for _, tc := range []struct {
		level   slog.Level
		private bool
	}{{slog.LevelInfo, true}, {slog.LevelDebug, false}} {
		var buf bytes.Buffer
		s := New(Config{
			LongPollTimeout: 100 * time.Millisecond,
			Logger:          slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level})),
		})
		srv := httptest.NewServer(s.Handler())
		resp := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", nil)
		resp.Body.Close()
		srv.Close()

		out := buf.String()
		if !strings.Contains(out, "/v1/session/{slot}/join") {
			t.Errorf("level %v: route pattern missing from log:\n%s", tc.level, out)
		}
		leaks := strings.Contains(out, testSlot) || strings.Contains(out, "127.0.0.1")
		if leaks == tc.private {
			t.Errorf("level %v: leaks IP/slot = %v, want %v:\n%s", tc.level, leaks, !tc.private, out)
		}
	}
}
