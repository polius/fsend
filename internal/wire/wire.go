// Package wire implements fsend's on-the-wire framing between peers.
//
// Two frame styles:
//   - Control frames (gob-encoded payloads) for session metadata, file
//     info, completion, errors. Used on the control stream.
//   - Data chunk frames (compact binary header) carry file bytes. Used on
//     the data stream. Each chunk has a BLAKE3 hash for end-to-end verification.
//
// Both styles share the same 1-byte version prefix so a peer running an
// incompatible version can be detected immediately.
package wire

// ProtocolVersion is the current wire protocol version byte.
//
// Bumping this is a major-version change for fsend. New optional frame
// types can be added without bumping by giving them unused type bytes.
//
// v2: listing/classify negotiation replaces the per-file lockstep; the
// directory tar is gone. No backward compatibility with v1 — peers detect
// the mismatch at HELLO and report a protocol error.
const ProtocolVersion = 0x02

// MaxControlFrameSize bounds gob payload size on control frames. One
// FILE_INFO at a time is the largest control frame in flight, so a
// modest limit is enough — and prevents a malicious peer from
// allocating unbounded memory.
const MaxControlFrameSize = 64 * 1024 // 64 KiB

// MaxChunkSize bounds the plaintext payload in a data CHUNK frame.
const MaxChunkSize = 1024 * 1024 // 1 MiB

// FrameType is the one-byte tag that identifies a frame on the control
// or data stream.
type FrameType uint8

// Control-stream frame types. Inline comments document the direction
// (sender → receiver or vice versa) and any preconditions.
const (
	TypeHello             FrameType = 0x01 // sender → receiver
	TypeHelloAck          FrameType = 0x02 // receiver → sender (carries accept/decline)
	TypePasswordChallenge FrameType = 0x03 // sender → receiver (only if HELLO.HasPassword)
	TypePasswordResponse  FrameType = 0x04 // receiver → sender
	TypeListingBatch      FrameType = 0x05 // sender → receiver: gob []ListingEntry (repeated)
	TypeListingEnd        FrameType = 0x06 // sender → receiver: ends the listing
	TypeClassifyBatch     FrameType = 0x07 // receiver → sender: gob []Decision (repeated)
	TypeClassifyEnd       FrameType = 0x08 // receiver → sender: ends classification
	TypePasswordVerified  FrameType = 0x09 // sender → receiver (positive ack of password)
	TypeTransferComplete  FrameType = 0x0A // sender → receiver
	TypeTransferAck       FrameType = 0x0B // receiver → sender
	TypeVerifyRequest     FrameType = 0x0C // receiver → sender: gob []uint32 (--checksum)
	TypeVerifyResponse    FrameType = 0x0D // sender → receiver: gob []FileHash
	TypeError             FrameType = 0xFE
)

// Data-stream frame types.
const (
	TypeChunk FrameType = 0x10
)

// TransferMode tells the receiver how to interpret the transfer.
type TransferMode uint8

// TransferMode values. ModeFiles runs the listing/classify negotiation;
// ModeStream is the stdin/--text exception: one unknown-length stream with
// no listing, no skip, no resume.
const (
	ModeFiles  TransferMode = 0
	ModeStream TransferMode = 1
)

// EntryType classifies one listing entry.
type EntryType uint8

// EntryType values.
const (
	EntryFile    EntryType = 0
	EntryDir     EntryType = 1
	EntrySymlink EntryType = 2
)

// DecisionAction is the receiver's per-entry verdict in the classify phase.
// The sender only learns what to do (send / skip / resume), never why.
type DecisionAction uint8

// DecisionAction values.
const (
	DecisionSend   DecisionAction = 0 // new, differing-and-overwrite, conflict-and-overwrite
	DecisionSkip   DecisionAction = 1 // identical, or kept (differ/conflict without consent)
	DecisionResume DecisionAction = 2 // valid partial; ResumeOffset/PartialImohash set
)

// ErrorCode is the catalog of error codes carried in TypeError frames.
type ErrorCode uint16

// ErrorCode values reported in TypeError frames. Only the codes a peer
// actually emits live here — transfer.mapPeerError is the source of
// truth for what gets mapped to a user-facing error.
// Numeric values are kept stable so a peer running an older fsend can
// still recognise them; new codes get fresh numbers.
const (
	ErrCodeWrongPassword    ErrorCode = 2
	ErrCodeFileHashMismatch ErrorCode = 8
	ErrCodeProtocolError    ErrorCode = 10
	// ErrCodePartialMismatch — receiver offered to resume from a prefix
	// whose imohash does not match the source's prefix at the same length.
	// Almost always means the source file changed since the previous
	// attempt; the receiver should discard its partial and retry.
	ErrCodePartialMismatch ErrorCode = 11
	// ErrCodeTargetExists — receiver refused to clobber an existing file
	// because the user did not pass --overwrite.
	ErrCodeTargetExists ErrorCode = 12
	// ErrCodeWriteFailed — receiver hit a local filesystem error (mkdir,
	// extract, …). Terminal for the sender: retrying won't fix the
	// receiver's disk.
	ErrCodeWriteFailed ErrorCode = 13
	// ErrCodePasswordRequired — sender demanded a password but the
	// receiver had none to offer (--quiet with no --pass / FSEND_PASS).
	// Distinct from ErrCodeWrongPassword: no attempt was ever made.
	ErrCodePasswordRequired ErrorCode = 14
	// ErrCodeCancelled — the peer cancelled deliberately (Ctrl-C).
	// Terminal: lets the survivor skip its retry budget instead of
	// misdiagnosing the teardown as a network drop.
	ErrCodeCancelled ErrorCode = 15
	// ErrCodeListingInvalid — the sender's listing was malformed (bad path,
	// duplicate, case-collision) or the data stream diverged from it.
	ErrCodeListingInvalid ErrorCode = 16
	// ErrCodeDeclined — the receiver declined at the accept prompt (after the
	// listing, so it can't ride on HELLO_ACK like the stream-mode decline).
	ErrCodeDeclined ErrorCode = 17
)

// Chunk-frame flag bits (frame-level).
const (
	FlagCompressed = 1 << 0 // payload is zstd-compressed
)

// Segment flag bits (per-file within a chunk).
const (
	SegFlagEOF = 1 << 0 // last segment of this file; RootHash is set
)
