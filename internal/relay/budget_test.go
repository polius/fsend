package relay

import (
	"testing"
	"time"
)

// day1 and day2 are noon on two consecutive UTC days, so Truncate(24h)
// maps them to different windows.
var (
	day1 = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	day2 = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
)

func TestDayBudget_ZeroMeansUnlimited(t *testing.T) {
	b := &dayBudget{limit: 0}
	for i := 0; i < 100; i++ {
		if b.charge(day1, 1<<30) {
			t.Fatalf("charge dropped under a 0 (unlimited) budget")
		}
	}
}

func TestDayBudget_ChargeUpToLimit(t *testing.T) {
	b := &dayBudget{limit: 100}
	// The datagram that reaches the limit is counted and allowed; the next
	// one is dropped.
	if b.charge(day1, 100) {
		t.Fatal("charge dropped the datagram that reaches the limit")
	}
	if !b.charge(day1, 1) {
		t.Fatal("charge allowed a datagram past the spent budget")
	}
}

func TestDayBudget_RollsAtUTCMidnight(t *testing.T) {
	b := &dayBudget{limit: 100}
	b.charge(day1, 100)
	if !b.charge(day1, 1) {
		t.Fatal("budget should be spent on day1")
	}
	// A new UTC day resets the window even with no charge in between.
	if got := b.usedToday(day2); got != 0 {
		t.Fatalf("usedToday(day2) = %d, want 0 after rollover", got)
	}
	if b.charge(day2, 50) {
		t.Fatal("fresh day should accept a charge under the limit")
	}
}

func TestDayBudget_UsedToday(t *testing.T) {
	b := &dayBudget{limit: 1000}
	b.charge(day1, 300)
	b.charge(day1, 200)
	if got := b.usedToday(day1); got != 500 {
		t.Fatalf("usedToday = %d, want 500", got)
	}
}
