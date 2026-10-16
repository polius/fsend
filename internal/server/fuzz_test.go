package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FuzzDecodeJSON fuzzes the request-body decoder every POST handler shares.
// It must reject arbitrary input without panicking, and MaxBytesReader must
// cap the read even when the body lies about its size.
func FuzzDecodeJSON(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"slot":"abc","client_version":"1.2.3"}`))
	f.Add([]byte(`{"session_id":"x","candidates":["a","b"],"role_token":"t"}`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(``))
	f.Add([]byte(`[1,2,3]`))

	f.Fuzz(func(t *testing.T, body []byte) {
		var v CreateSessionRequest
		r := httptest.NewRequest(http.MethodPost, "/v1/session", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		// The error is expected for most inputs; we only assert no panic.
		_ = decodeJSON(rec, r, &v)
	})
}
