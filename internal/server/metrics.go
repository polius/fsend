package server

import (
	"net/http"
	"time"

	"github.com/polius/fsend/internal/relay"
)

// MetricsResponse is the body of GET /metrics. Everything is an
// aggregate count or gauge — no per-IP, per-session, or per-code data, so
// it can't leak what the server doesn't store. Relay is omitted when no
// relay is wired.
type MetricsResponse struct {
	Version               string         `json:"version"`
	UptimeSeconds         int64          `json:"uptime_seconds"`
	SessionsActive        int            `json:"sessions_active"`
	SessionsCreatedTotal  uint64         `json:"sessions_created_total"`
	SessionsPairedTotal   uint64         `json:"sessions_paired_total"`
	SessionsRejectedTotal RejectedCounts `json:"sessions_rejected_total"`
	Relay                 *relay.Metrics `json:"relay,omitempty"`
}

// RejectedCounts breaks down turned-away sessions by reason. RateLimit and
// ConcurrencyLimit are both per-IP caps — named by what they cap (inflow
// rate vs standing count), not by "IP", since both key on the source IP.
type RejectedCounts struct {
	RateLimit        uint64 `json:"rate_limit"`
	ConcurrencyLimit uint64 `json:"concurrency_limit"`
	Unauthorized     uint64 `json:"unauthorized"`
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	active := len(s.byID)
	s.mu.Unlock()

	resp := MetricsResponse{
		Version:              s.cfg.ServerVersion,
		UptimeSeconds:        int64(time.Since(s.started).Seconds()),
		SessionsActive:       active,
		SessionsCreatedTotal: s.met.sessionsCreated.Load(),
		SessionsPairedTotal:  s.met.sessionsPaired.Load(),
		SessionsRejectedTotal: RejectedCounts{
			RateLimit:        s.met.rejRate.Load(),
			ConcurrencyLimit: s.met.rejConcurrency.Load(),
			Unauthorized:     s.met.rejAuth.Load(),
		},
	}
	if s.relayAllocator != nil {
		m := s.relayAllocator.Metrics()
		resp.Relay = &m
	}
	writeJSON(w, http.StatusOK, resp)
}
