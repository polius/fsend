package uxlog

import (
	"testing"
	"time"
)

// TestRateWindow_ReflectsRecentThroughput is the point of the window: after
// a fast stretch followed by a slow one, the reported rate must be the
// recent (slow) throughput, not the lifetime mean.
func TestRateWindow_ReflectsRecentThroughput(t *testing.T) {
	w := &rateWindow{}
	t0 := time.Now()
	// 5 s at 10 MB/s, then 10 s at 1 MB/s (sampled every 100 ms, like the
	// bar's refresh rate). Lifetime mean would be 4 MB/s.
	cum := int64(0)
	for ts := time.Duration(0); ts <= 15*time.Second; ts += 100 * time.Millisecond {
		if ts <= 5*time.Second {
			cum += 1000 * 1000 // 1 MB per 100 ms
		} else {
			cum += 100 * 1000 // 0.1 MB per 100 ms
		}
		w.observe(t0.Add(ts), cum)
	}
	got := w.rate()
	if got < 0.9*1000*1000 || got > 1.1*1000*1000 {
		t.Errorf("windowed rate = %.0f B/s, want ~1 MB/s (recent), not the ~4 MB/s lifetime mean", got)
	}
}

// TestRateWindow_StallDecaysToZero: when bytes stop moving, the rate must
// drop to 0 once the window no longer spans any progress — the chip then
// hides instead of freezing on a stale figure.
func TestRateWindow_StallDecaysToZero(t *testing.T) {
	w := &rateWindow{}
	t0 := time.Now()
	w.observe(t0, 0)
	w.observe(t0.Add(time.Second), 5*1000*1000)
	// Stalled: same cumulative count for longer than the window.
	got := w.observe(t0.Add(1*time.Second+rateWindowSpan+time.Second), 5*1000*1000)
	if got != 0 {
		t.Errorf("stalled window rate = %.0f, want 0", got)
	}
}

// TestRateWindow_Stalled: the stall marker trips only after stallThreshold
// with no byte advance, never before the first sample, and recovers as
// soon as bytes move again.
func TestRateWindow_Stalled(t *testing.T) {
	w := &rateWindow{}
	t0 := time.Now()
	if w.stalled(t0) {
		t.Error("stalled before any sample")
	}
	w.observe(t0, 1000)
	if w.stalled(t0.Add(stallThreshold - time.Second)) {
		t.Error("stalled before the threshold elapsed")
	}
	// Frames keep arriving with the same cumulative count (a real stall):
	// they must not reset the clock.
	w.observe(t0.Add(2*time.Second), 1000)
	if !w.stalled(t0.Add(stallThreshold + time.Second)) {
		t.Error("not stalled after threshold with no byte advance")
	}
	// Bytes move again: recovered.
	w.observe(t0.Add(stallThreshold+2*time.Second), 2000)
	if w.stalled(t0.Add(stallThreshold + 2*time.Second)) {
		t.Error("still stalled after bytes advanced")
	}
}

// TestRateWindow_NoDivideByZero: empty and degenerate windows must return
// 0, never divide by a zero span.
func TestRateWindow_NoDivideByZero(t *testing.T) {
	w := &rateWindow{}
	if got := w.rate(); got != 0 {
		t.Errorf("empty window rate = %.0f, want 0", got)
	}
	now := time.Now()
	if got := w.observe(now, 1000); got != 0 {
		t.Errorf("single-sample rate = %.0f, want 0", got)
	}
	// Two samples at the same instant: zero span.
	if got := w.observe(now, 2000); got != 0 {
		t.Errorf("zero-span rate = %.0f, want 0", got)
	}
}

// TestRateWindow_PrunesOldSamples: samples older than the window are
// dropped (one kept as the baseline), so memory stays bounded and the
// baseline tracks the window's leading edge.
func TestRateWindow_PrunesOldSamples(t *testing.T) {
	w := &rateWindow{}
	t0 := time.Now()
	for i := range 100 {
		w.observe(t0.Add(time.Duration(i)*100*time.Millisecond), int64(i)*1000)
	}
	w.mu.Lock()
	n := len(w.samples)
	w.mu.Unlock()
	// 5 s window at 100 ms sampling ≈ 51 samples + 1 baseline.
	if n > 60 {
		t.Errorf("window kept %d samples, want ≤ 60 (pruned)", n)
	}
}
