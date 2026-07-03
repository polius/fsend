package uxlog

import (
	"sync"
	"time"
)

// rateWindowSpan is how much recent history the rate/ETA reflect. Long
// enough to smooth per-frame jitter, short enough that a mid-transfer
// speed change shows within seconds.
const rateWindowSpan = 5 * time.Second

// rateWindow measures throughput over the last rateWindowSpan of samples
// so the bar's rate and ETA track current conditions instead of the
// lifetime mean, which a slow handshake or a speed change would skew for
// the rest of the transfer. Fed once per refresh frame by the rate
// decorator; the ETA decorator reads it.
type rateWindow struct {
	mu      sync.Mutex
	samples []rateSample
}

type rateSample struct {
	t   time.Time
	cum int64 // cumulative bytes at t
}

// observe records the cumulative byte count at now and returns the
// windowed rate in bytes/sec (see rate).
func (w *rateWindow) observe(now time.Time, cum int64) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples = append(w.samples, rateSample{now, cum})
	// Drop samples older than the window, but keep the newest of them as
	// the baseline so the measured span stays ~window wide instead of
	// shrinking toward the sampling interval.
	cutoff := now.Add(-rateWindowSpan)
	i := 0
	for i < len(w.samples)-1 && w.samples[i+1].t.Before(cutoff) {
		i++
	}
	w.samples = w.samples[i:]
	return w.rateLocked()
}

// rate returns the current windowed rate in bytes/sec, or 0 when the
// window is too short to divide by meaningfully.
func (w *rateWindow) rate() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rateLocked()
}

func (w *rateWindow) rateLocked() float64 {
	if len(w.samples) < 2 {
		return 0
	}
	first, last := w.samples[0], w.samples[len(w.samples)-1]
	span := last.t.Sub(first.t)
	// A few frames of history minimum — a near-zero span would just
	// amplify per-chunk jitter (and 0 would divide by zero).
	if span < 300*time.Millisecond || last.cum <= first.cum {
		return 0
	}
	return float64(last.cum-first.cum) / span.Seconds()
}
