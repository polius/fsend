package code

import (
	"crypto/hkdf"
	"crypto/sha256"
	"runtime/debug"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Stretch returns n bytes derived from the argon2id stretch of code at
// epoch, domain-separated by label with HKDF. The pairing slot and the
// mDNS name share one stretch, so a sender pays for one derivation rather
// than two concurrent ones. Stretches run one at a time and are memoized:
// the sender re-derives on every /wait long-poll.
func Stretch(code string, epoch int64, label string, n int) []byte {
	key, err := hkdf.Key(sha256.New, stretchKey(code, epoch), nil, label, n)
	if err != nil {
		panic("code: hkdf: " + err.Error()) // only for n > 255*32
	}
	return key
}

// The argon2id cost: 8 MiB and 5 passes, a little above OWASP's 7 MiB
// and 5 passes (one of its settings equivalent to 19 MiB and 2; OWASP
// uses one lane, fsend four, with the same memory and work in total).
// Up to 1.17 it was 64 MiB and 2 passes, a 64 MiB peak on both sides,
// more than the whole transfer needs. A guess now takes about a third of
// the time (6.7 ms against 22 ms, measured on an M-series Mac), and a GPU
// fits 8 times as many at once, but a code still takes about 2^44
// guesses on average, far beyond what fits in the hour a code lives.
// Changing either value changes every slot and mDNS name, so peers on
// different settings never find each other: bump the salt label with
// them.
const (
	stretchMemKiB = 8 * 1024
	stretchPasses = 5
)

type stretchID struct {
	code  string
	epoch int64
}

var (
	stretchMu    sync.Mutex
	stretchCache = map[stretchID][]byte{}
)

// stretchCacheMax bounds the memo: a receiver near midnight needs two
// epochs, and a re-pair adds a resume rendezvous.
const stretchCacheMax = 8

func stretchKey(code string, epoch int64) []byte {
	stretchMu.Lock()
	defer stretchMu.Unlock()
	id := stretchID{code, epoch}
	if k, ok := stretchCache[id]; ok {
		return k
	}
	k := argon2.IDKey([]byte(code), EpochSalt("fsend-stretch-v4", epoch), stretchPasses, stretchMemKiB, 4, 32)
	// Argon2's blocks are garbage now, but still counted live by the GC
	// pacer until the next cycle, which would let the transfer's heap grow
	// into them. Collect and hand the pages back now, once per code (the
	// result is memoised), so the transfer starts small.
	debug.FreeOSMemory()
	if len(stretchCache) >= stretchCacheMax {
		clear(stretchCache)
	}
	stretchCache[id] = k
	return k
}
