package relay

import (
	"sync"
	"time"
)

// dayBudget caps the bytes the relay forwards per UTC day — the
// Denial-of-Wallet backstop that bounds egress spend no matter how the
// per-IP and per-session limits are set. A limit of 0 means unlimited.
//
// The window rolls at UTC midnight. charge() and exhausted() both roll, so
// the counter resets on a new day even when no datagrams are flowing.
type dayBudget struct {
	limit uint64 // bytes per day; 0 = unlimited

	mu   sync.Mutex
	day  int64  // UTC-midnight unix seconds of the current window
	used uint64 // bytes forwarded so far this window
}

// roll resets the window when now lands on a different UTC day. Caller holds mu.
func (b *dayBudget) roll(now time.Time) {
	if d := now.UTC().Truncate(24 * time.Hour).Unix(); d != b.day {
		b.day = d
		b.used = 0
	}
}

// charge records n forwarded bytes and returns true once the day's budget
// is exceeded, meaning the caller should drop the datagram rather than
// forward it. Mirrors the per-session cap: the datagram that crosses the
// limit is counted but not forwarded.
func (b *dayBudget) charge(now time.Time, n uint64) bool {
	if b.limit == 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll(now)
	b.used += n
	return b.used > b.limit
}

// exhausted reports whether the day's budget is spent, without charging.
// Allocate uses it to refuse a new transfer up front — deterministically,
// before the lossy relay pairing — so both peers fail fast with a clear
// reason instead of racing the in-flight breaker.
func (b *dayBudget) exhausted(now time.Time) bool {
	if b.limit == 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll(now)
	return b.used >= b.limit
}

// usedToday returns bytes forwarded in the current window, for /metrics.
func (b *dayBudget) usedToday(now time.Time) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll(now)
	return b.used
}
