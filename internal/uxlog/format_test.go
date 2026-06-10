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
		{1024, "1 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1 MB"},
		{int64(2.5 * 1024 * 1024), "2.5 MB"},
		{100 * 1024 * 1024, "100 MB"},
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
	if got := HumanRate(1024*1024, 50*time.Millisecond); got != "" {
		t.Errorf("sub-100ms should yield empty, got %q", got)
	}
	// Zero bytes → empty.
	if got := HumanRate(0, time.Second); got != "" {
		t.Errorf("zero bytes should yield empty, got %q", got)
	}
	// Sub-MiB is dominated by handshake noise → empty.
	if got := HumanRate(64*1024, time.Second); got != "" {
		t.Errorf("sub-MiB should yield empty, got %q", got)
	}
	// 1 MiB / 1s ≈ 1.0 MB/s.
	if got := HumanRate(1024*1024, time.Second); !strings.HasSuffix(got, "/s") {
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
