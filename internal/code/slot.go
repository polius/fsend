package code

import (
	"encoding/hex"
	"errors"
	"regexp"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Slot returns the pairing-server lookup key for a code: 16 bytes of
// argon2id, hex-encoded (32 lowercase hex chars).
//
// The server keys sessions on the slot, never the code: the code is the
// PAKE secret, and a server that learned it could run the SPAKE2
// handshake with each peer and MITM the transfer. The code has only
// ~45 bits of entropy, so a fast hash would let anyone holding a slot
// recover the code by offline brute force. argon2id makes each guess
// cost ~64 MiB of memory and tens of milliseconds (same parameters as
// internal/landisc, which solves the identical problem for the mDNS
// name), putting the 2^44-average search out of practical reach.
//
// The salt is a fixed domain label — the peers share no prior state, so
// a per-session salt is impossible; the memory-hardness carries the
// defense. Distinct from the landisc salt so the two derived values
// can't be cross-correlated. Memoized because the sender re-derives on
// every /wait long-poll.
func Slot(code string) string {
	slotMu.Lock()
	defer slotMu.Unlock()
	if code == slotCode {
		return slotVal
	}
	v := hex.EncodeToString(argon2.IDKey([]byte(code), []byte("fsend-slot-v1"), 2, 64*1024, 4, 16))
	slotCode, slotVal = code, v
	return v
}

var (
	slotMu   sync.Mutex
	slotCode string
	slotVal  string
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
