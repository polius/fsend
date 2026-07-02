package uxlog

import (
	"strings"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{999, "999 B"},
		{1000, "1 KB"},
		{1500, "1.5 KB"},
		// Decimal units, matching Finder/Explorer — and a value that only
		// displays round drops the decimal like an exact one: 1 KiB reads
		// "1 KB", not a lone "1.0 KB" next to 1000 B's "1 KB".
		{1024, "1 KB"},
		{1000 * 1000, "1 MB"},
		{2_500_000, "2.5 MB"},
		{100_000_000, "100 MB"},
		{1_500_000_000, "1.5 GB"},
	} {
		if got := HumanBytes(tc.in); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "500ms"},
		{1500 * time.Millisecond, "1.5s"},
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m30s"},
		{3 * time.Hour, "3h00m00s"},
	} {
		if got := HumanDuration(tc.in); got != tc.want {
			t.Errorf("HumanDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanRate(t *testing.T) {
	// Below 100ms is too noisy → empty.
	if got := HumanRate(1000*1000, 50*time.Millisecond); got != "" {
		t.Errorf("sub-100ms should yield empty, got %q", got)
	}
	// Zero bytes → empty.
	if got := HumanRate(0, time.Second); got != "" {
		t.Errorf("zero bytes should yield empty, got %q", got)
	}
	// Sub-MB is dominated by handshake noise → empty.
	if got := HumanRate(64*1000, time.Second); got != "" {
		t.Errorf("sub-MB should yield empty, got %q", got)
	}
	// 1 MB / 1s = 1 MB/s.
	if got := HumanRate(1000*1000, time.Second); !strings.HasSuffix(got, "/s") {
		t.Errorf("got %q, want trailing /s", got)
	}
}

// HumanDuration regression: hour-tier output used to render as "90m02s"
// at exactly 1h30m02s. Lock the correct format in.
func TestHumanDuration_HourTier(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{50 * time.Millisecond, "50ms"},
		{1500 * time.Millisecond, "1.5s"},
		{15 * time.Second, "15s"},
		{90 * time.Second, "1m30s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{time.Hour + 30*time.Minute + 2*time.Second, "1h30m02s"},
	}
	for _, c := range cases {
		if got := HumanDuration(c.d); got != c.want {
			t.Errorf("HumanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// A value that only *displays* round must drop the decimal: 300006 B is
// "300 KB", matching an exact 300000 B — not a lone "300.0 KB".
func TestHumanBytes_DisplayRoundDropsDecimal(t *testing.T) {
	if got := HumanBytes(300006); got != "300 KB" {
		t.Errorf("HumanBytes(300006) = %q, want \"300 KB\"", got)
	}
	if got := HumanBytes(2_400_000); got != "2.4 MB" {
		t.Errorf("HumanBytes(2400000) = %q, want \"2.4 MB\"", got)
	}
}
