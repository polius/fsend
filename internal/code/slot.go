package code

import (
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// EpochGrace is how close to a UTC day boundary a receiver also tries the
// adjacent day's epoch. It matches the session TTL: a code created just
// before midnight is still valid for that long afterwards.
const EpochGrace = time.Hour

// Epoch is the salt epoch for t: the UTC day number. Salting the slot and
// the mDNS name with it means a precomputed code→slot table expires daily
// instead of working forever.
func Epoch(t time.Time) int64 { return t.UTC().Unix() / 86400 }

// EpochsFor lists the epochs a receiver should try at t, current first.
// Near a day boundary the sender may have derived with the neighbouring
// day (a code made just before midnight, or a few minutes of clock skew).
func EpochsFor(t time.Time) []int64 {
	e := Epoch(t)
	secs := t.UTC().Unix() % 86400
	switch {
	case secs < int64(EpochGrace/time.Second):
		return []int64{e, e - 1}
	case secs >= 86400-int64(EpochGrace/time.Second):
		return []int64{e, e + 1}
	}
	return []int64{e}
}

// EpochSalt returns the argon2id salt for a domain label and epoch.
func EpochSalt(label string, epoch int64) []byte {
	return []byte(label + "/" + strconv.FormatInt(epoch, 10))
}

// Slot returns the pairing-server lookup key for a code at the current
// epoch: 16 bytes of argon2id, hex-encoded (32 lowercase hex chars).
func Slot(code string) string { return SlotAt(code, Epoch(time.Now())) }

// SlotAt is Slot for an explicit epoch.
//
// The server keys sessions on the slot, never the code: the code is the
// PAKE secret, and a server that learned it could run the PAKE
// handshake with each peer and MITM the transfer. The code has only
// ~45 bits of entropy, so a fast hash would let anyone holding a slot
// recover the code by offline brute force. argon2id makes each guess
// cost ~64 MiB of memory and tens of milliseconds (same parameters as
// internal/landisc, which solves the identical problem for the mDNS
// name), putting the 2^44-average search out of practical reach.
//
// The peers share no prior state, so a per-session salt is impossible.
// The salt is a domain label plus the UTC day (see Epoch): the
// memory-hardness makes one day's table cost tens of thousands of
// CPU-years, and a new day needs a new table. Distinct from the landisc
// salt so the two derived values can't be cross-correlated. Memoized
// because the sender re-derives on every /wait long-poll.
func SlotAt(code string, epoch int64) string {
	slotMu.Lock()
	defer slotMu.Unlock()
	if code == slotCode && epoch == slotEpoch {
		return slotVal
	}
	v := hex.EncodeToString(argon2.IDKey([]byte(code), EpochSalt("fsend-slot-v3", epoch), 2, 64*1024, 4, 16))
	slotCode, slotEpoch, slotVal = code, epoch, v
	return v
}

var (
	slotMu    sync.Mutex
	slotCode  string
	slotEpoch int64
	slotVal   string
)

// SlotLen is the length of an encoded slot.
const SlotLen = 32

// slotPattern matches exactly what Slot produces. The server validates
// inbound slots against it so the session table can't be polluted with
// arbitrary strings.
var slotPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ErrInvalidSlot is returned when a string is not a well-formed slot.
var ErrInvalidSlot = errors.New("invalid slot format")

// ValidateSlot returns nil if s is shaped like a slot, ErrInvalidSlot
// otherwise.
func ValidateSlot(s string) error {
	if !slotPattern.MatchString(s) {
		return ErrInvalidSlot
	}
	return nil
}
