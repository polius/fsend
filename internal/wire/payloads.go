package wire

// Payload structs for the gob-encoded control frames.
//
// All time-bearing fields use int64 (seconds or nanos as noted) to keep gob
// output deterministic across timezone settings. Strings are UTF-8 and
// bounded by MaxControlFrameSize at the frame layer.

// SenderHello is the first control frame the sender emits, immediately
// after the TLS handshake completes.
type SenderHello struct {
	MinVersion    uint8        // oldest wire protocol the sender speaks
	MaxVersion    uint8        // newest wire protocol the sender speaks
	Caps          Capability   // optional features the sender supports
	Hostname      string       // sender's hostname or --name override
	OS            string       // "linux" | "darwin" | "windows" | ...
	ClientVersion string       // fsend semver
	HasPassword   bool         // true → expect PASSWORD_CHALLENGE after HELLO_ACK
	Mode          TransferMode // ModeFiles (listing) | ModeStream (stdin/text)

	// Stream-mode fields (ignored for ModeFiles).
	IsText      bool   // true → --text payload (receiver prints it)
	DisplayName string // peer-facing label for the accept prompt / stream naming
}

// ReceiverHello is the receiver's response to SenderHello. Carries the
// receiver's identity and the user's accept/reject decision.
type ReceiverHello struct {
	MinVersion    uint8 // zero only when the receiver couldn't parse HELLO
	MaxVersion    uint8
	Caps          Capability
	Hostname      string
	OS            string
	ClientVersion string
	Accepts       bool // false → user rejected at prompt; sender should ABORT
}

// PasswordChallenge opens one --password attempt: a fresh nonce (the CPace
// session id) and the sender's CPace share over the password. The run is
// bound to the session, so it can't be replayed or relayed elsewhere.
type PasswordChallenge struct {
	Nonce [32]byte
	Share [32]byte
}

// PasswordResponse is the receiver's CPace share plus its key-confirmation
// tag. A wrong password yields a tag the sender can't verify, and neither
// side learns anything an offline dictionary attack could use.
type PasswordResponse struct {
	Share [32]byte
	Tag   [32]byte
}

// PasswordVerified carries the sender's confirmation tag, proving to the
// receiver that the sender knows the password too.
type PasswordVerified struct {
	Tag [32]byte
}

// ListingEntry describes one filesystem entry the sender offers. Emitted in
// walk order, batched into TypeListingBatch frames before any data flows.
//
// ModTimeSec is unix seconds (not nanos): mtime resolution varies by
// filesystem (FAT/network FS truncate sub-second), and the receiver sets
// this value on finalize, so second-granularity is what makes the
// size+mtime skip check survive a round-trip.
type ListingEntry struct {
	Index         uint32
	RelativePath  string // forward-slash; no leading slash, drive, UNC, or .. segments
	Size          uint64 // bytes (0 for dirs/symlinks/empty files)
	ModTimeSec    int64  // unix seconds
	Mode          uint32 // unix perm bits
	Type          EntryType
	SymlinkTarget string // EntrySymlink only
}

// Decision is the receiver's per-entry verdict, batched into
// TypeClassifyBatch frames. ResumeOffset/PartialImohash are meaningful only
// when Action == DecisionResume.
type Decision struct {
	Index          uint32
	Action         DecisionAction
	ResumeOffset   uint64
	PartialImohash [16]byte
	// Kept marks a DecisionSkip of a differing/conflicting entry the
	// receiver kept, as opposed to an identical one. Gob keeps this
	// compatible across versions: old peers ignore the field, and a zero
	// value from an old receiver reads as "unknown" — the sender then
	// falls back to the neutral "skipped" wording.
	Kept bool
}

// FileHash carries one file's BLAKE3 root, sent by the sender in a
// TypeVerifyResponse when the receiver requests content verification
// (--checksum) for files it already holds at a matching size.
type FileHash struct {
	Index uint32
	Hash  [32]byte
}

// ErrorFrame carries a numbered error from one peer to the other.
type ErrorFrame struct {
	Code    ErrorCode
	Message string // human-readable
}
