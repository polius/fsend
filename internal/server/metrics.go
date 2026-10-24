package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/polius/fsend/internal/relay"
)

// MetricsResponse is the body of GET /metrics.json. Everything is an
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

// MetricsHandler serves GET /metrics (Prometheus text format) and
// GET /metrics.json. It is meant for a separate, non-public listener
// (FSEND_METRICS_ADDR): the snapshot names the running build.
func (s *Server) MetricsHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /metrics", s.metricsProm)
	m.HandleFunc("GET /metrics.json", s.metricsJSON)
	return m
}

func (s *Server) snapshot() MetricsResponse {
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
	return resp
}

func (s *Server) metricsJSON(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot())
}

// metricsProm renders the snapshot in the Prometheus text exposition format
// (version 0.0.4), which OpenMetrics scrapers also accept.
func (s *Server) metricsProm(w http.ResponseWriter, _ *http.Request) {
	m := s.snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	p := promWriter{w: w}
	p.metric("fsend_build_info", "gauge", "Build running; the value is always 1.",
		`{version="`+promLabel.Replace(m.Version)+`"}`, 1)
	p.metric("fsend_uptime_seconds", "gauge", "Seconds since the process started.", "", m.UptimeSeconds)
	p.metric("fsend_sessions_active", "gauge", "Pairing sessions live now.", "", m.SessionsActive)
	p.metric("fsend_sessions_created_total", "counter", "Sessions a sender opened.", "", m.SessionsCreatedTotal)
	p.metric("fsend_sessions_paired_total", "counter", "Sessions a receiver joined.", "", m.SessionsPairedTotal)
	p.header("fsend_sessions_rejected_total", "counter", "Requests turned away, by reason.")
	p.sample("fsend_sessions_rejected_total", `{reason="rate_limit"}`, m.SessionsRejectedTotal.RateLimit)
	p.sample("fsend_sessions_rejected_total", `{reason="concurrency_limit"}`, m.SessionsRejectedTotal.ConcurrencyLimit)
	p.sample("fsend_sessions_rejected_total", `{reason="unauthorized"}`, m.SessionsRejectedTotal.Unauthorized)
	if r := m.Relay; r != nil {
		p.metric("fsend_relay_forwarding", "gauge", "1 if the relay carries data, 0 if STUN-only.", "", b2i(r.Forwarding))
		p.metric("fsend_relay_healthy", "gauge", "1 while the relay read loop is alive.", "", b2i(r.Healthy))
		p.metric("fsend_relay_transfers_active", "gauge", "Relay transfers moving bytes now.", "", r.TransfersActive)
		p.metric("fsend_relay_transfers_total", "counter", "Relay transfers that forwarded data.", "", r.TransfersTotal)
		p.metric("fsend_relay_transfers_capped_total", "counter", "Transfers cut off by the per-session byte cap.", "", r.TransfersCappedTotal)
		p.metric("fsend_relay_forwarded_bytes_total", "counter", "Bytes relayed.", "", r.BytesForwardedTotal)
		p.metric("fsend_relay_peak_transfer_bytes", "gauge", "Largest single relayed transfer.", "", r.PeakTransferBytes)
		p.metric("fsend_relay_budget_bytes_today", "gauge", "Bytes relayed in the current UTC day.", "", r.BudgetBytesToday)
	}
}

type promWriter struct{ w io.Writer }

func (p promWriter) header(name, typ, help string) {
	_, _ = fmt.Fprintf(p.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (p promWriter) sample(name, labels string, v any) {
	_, _ = fmt.Fprintf(p.w, "%s%s %v\n", name, labels, v)
}

func (p promWriter) metric(name, typ, help, labels string, v any) {
	p.header(name, typ, help)
	p.sample(name, labels, v)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// promLabel escapes a label value per the exposition format.
var promLabel = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
