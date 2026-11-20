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
// mDNS name share one stretch, so a sender pays for a single 64 MiB
// derivation rather than two concurrent ones (a ~140 MiB peak that could
// OOM a small device). Stretches run one at a time and are memoized:
// the sender re-derives on every /wait long-poll.
func Stretch(code string, epoch int64, label string, n int) []byte {
	key, err := hkdf.Key(sha256.New, stretchKey(code, epoch), nil, label, n)
	if err != nil {
		panic("code: hkdf: " + err.Error()) // only for n > 255*32
	}
	return key
}

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
	k := argon2.IDKey([]byte(code), EpochSalt("fsend-stretch-v3", epoch), 2, 64*1024, 4, 32)
	// Argon2's 64 MiB of blocks is garbage now, but still counted live by
	// the GC pacer until the next cycle: left alone, it sets the next GC
	// target near 128 MiB and the transfer's heap grows into it (139 MiB
	// peak RSS on the sender). Collect and hand the pages back now, once
	// per code (the result is memoised), so the transfer starts small.
	debug.FreeOSMemory()
	if len(stretchCache) >= stretchCacheMax {
		clear(stretchCache)
	}
	stretchCache[id] = k
	return k
}
