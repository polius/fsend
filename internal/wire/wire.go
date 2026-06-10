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
const ProtocolVersion = 0x01

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
	TypeHelloAck          FrameType = 0x02 // receiver → sender
	TypePasswordChallenge FrameType = 0x03 // sender → receiver (only if HELLO.HasPassword)
	TypePasswordResponse  FrameType = 0x04 // receiver → sender
	TypeFileInfo          FrameType = 0x05 // sender → receiver (per file)
	TypeFileAccept        FrameType = 0x06 // receiver → sender (per file)
	TypeTransferComplete  FrameType = 0x07 // sender → receiver
	TypeTransferAck       FrameType = 0x08 // receiver → sender
	TypePasswordVerified  FrameType = 0x09 // sender → receiver (positive ack of password)
	TypeError             FrameType = 0xFE
)

// Data-stream frame types.
const (
	TypeChunk FrameType = 0x10
)

// TransferKind tells the receiver how to interpret the upcoming payload.
type TransferKind uint8

// TransferKind values. TransferDirectory is the only kind that triggers
// the tar-wrapping path on the receiver; all others stream into one
// destination file (or stdout) each.
const (
	TransferSingleFile TransferKind = 1
	TransferMultiFile  TransferKind = 2
	TransferDirectory  TransferKind = 3
	TransferStdin      TransferKind = 4
	TransferText       TransferKind = 5
)

// FileAcceptAction is how the receiver responds to a FILE_INFO.
type FileAcceptAction uint8

// FileAcceptAction values. ActionResume carries a chunk-aligned offset;
// every other action ignores the offset.
const (
	ActionAcceptFull FileAcceptAction = 0
	ActionResume     FileAcceptAction = 1
	ActionSkip       FileAcceptAction = 2
	ActionAbortAll   FileAcceptAction = 3
)

// ErrorCode is the catalog of error codes carried in TypeError frames.
type ErrorCode uint16

// ErrorCode values reported in TypeError frames. Only the codes a peer
// actually emits live here — the receiver's switch in tryReadPeerError
// is the source of truth for what gets mapped to a user-facing error.
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
)

// Chunk-frame flag bits.
const (
	FlagCompressed = 1 << 0 // payload is zstd-compressed
	FlagLastChunk  = 1 << 1 // last chunk of this file
)
