package wire

// Payload structs for the gob-encoded control frames.
//
// All time-bearing fields use int64 (seconds or nanos as noted) to keep gob
// output deterministic across timezone settings. Strings are UTF-8 and
// bounded by MaxControlFrameSize at the frame layer.

// SenderHello is the first control frame the sender emits, immediately
// after the TLS handshake completes.
type SenderHello struct {
	ProtocolVersion uint8        // must equal ProtocolVersion
	Hostname        string       // sender's hostname or --name override
	OS              string       // "linux" | "darwin" | "windows" | ...
	ClientVersion   string       // fsend semver
	HasPassword     bool         // true → expect PASSWORD_CHALLENGE after HELLO_ACK
	Mode            TransferMode // ModeFiles (listing) | ModeStream (stdin/text)

	// Stream-mode fields (ignored for ModeFiles).
	IsText      bool   // true → --text payload (receiver prints it)
	DisplayName string // peer-facing label for the accept prompt / stream naming
}

// ReceiverHello is the receiver's response to SenderHello. Carries the
// receiver's identity and the user's accept/reject decision.
type ReceiverHello struct {
	Hostname      string
	OS            string
	ClientVersion string
	Accepts       bool // false → user rejected at prompt; sender should ABORT
}

// PasswordChallenge is sent by the sender right after a positive
// HELLO_ACK when SenderHello.HasPassword is true. The nonce is fresh per
// session so a captured response cannot be replayed.
type PasswordChallenge struct {
	Nonce [32]byte
}

// PasswordResponse carries HMAC(argon2id(password, nonce), nonce) computed
// by the receiver. The sender verifies it in constant time.
type PasswordResponse struct {
	HMAC [32]byte
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
