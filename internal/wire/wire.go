// Package wire implements fsend's on-the-wire framing between peers.
//
// See docs/decisions/wire-protocol.md for the full specification.
//
// Two frame styles:
//   - Control frames (gob-encoded payloads) for session metadata, password
//     challenge, file info, completion, errors. Used on the control stream.
//   - Data chunk frames (compact binary header) carry file bytes. Used on
//     the data stream. Each chunk has a BLAKE3 hash for end-to-end verification.
//
// Both styles share the same 1-byte version prefix so a peer running an
// incompatible version can be detected immediately.
package wire

// ProtocolVersion is the current wire protocol version byte.
//
// Bumping this is a major-version change for fsend (see PROJECT_SPEC.md
// "Versioning + compatibility promise"). New optional frame types can be
// added without bumping by giving them unused type bytes.
const ProtocolVersion = 0x01

// MaxControlFrameSize bounds gob payload size on control frames. We pick a
// generous limit because the FILE_INFO batch for large directories can be
// significant (each entry is ~150 bytes; 100k files = ~15 MB), but cap it
// to prevent a malicious peer from allocating unbounded memory.
const MaxControlFrameSize = 64 * 1024 * 1024 // 64 MiB

// MaxChunkSize bounds the plaintext payload in a data CHUNK frame.
// 1 MiB is the chunk size locked in docs/decisions/wire-protocol.md.
const MaxChunkSize = 1024 * 1024 // 1 MiB

// Frame type bytes.
type FrameType uint8

const (
	// Control stream frame types (per docs/decisions/wire-protocol.md).
	TypeHello             FrameType = 0x01 // sender → receiver
	TypeHelloAck          FrameType = 0x02 // receiver → sender
	TypePasswordChallenge FrameType = 0x03 // sender → receiver
	TypePasswordResponse  FrameType = 0x04 // receiver → sender
	TypeFileInfo          FrameType = 0x05 // sender → receiver (per file)
	TypeFileAccept        FrameType = 0x06 // receiver → sender (per file)
	TypeTransferComplete  FrameType = 0x07 // sender → receiver
	TypeTransferAck       FrameType = 0x08 // receiver → sender
	TypeError             FrameType = 0xFE
	TypeAbort             FrameType = 0xFF

	// Data stream frame types.
	TypeChunk FrameType = 0x10

	// Receiver-control stream frame types.
	TypeProgressAck   FrameType = 0x20
	TypeResumeRequest FrameType = 0x21
)

// TransferKind tells the receiver how to interpret the upcoming payload.
type TransferKind uint8

const (
	TransferSingleFile TransferKind = 1
	TransferMultiFile  TransferKind = 2
	TransferDirectory  TransferKind = 3
	TransferStdin      TransferKind = 4
	TransferText       TransferKind = 5
)

// FileAcceptAction is how the receiver responds to a FILE_INFO.
type FileAcceptAction uint8

const (
	ActionAcceptFull FileAcceptAction = 0
	ActionResume     FileAcceptAction = 1
	ActionSkip       FileAcceptAction = 2
	ActionAbortAll   FileAcceptAction = 3
)

// ErrorCode is the catalog of error codes carried in TypeError frames.
// See docs/decisions/wire-protocol.md "Error catalog".
type ErrorCode uint16

const (
	ErrCodeUnsupportedVersion ErrorCode = 1
	ErrCodeWrongPassword      ErrorCode = 2
	ErrCodeReceiverRejected   ErrorCode = 3
	ErrCodeDiskFull           ErrorCode = 4
	ErrCodeWriteFailed        ErrorCode = 5
	ErrCodeReadFailed         ErrorCode = 6
	ErrCodeChunkHashMismatch  ErrorCode = 7
	ErrCodeFileHashMismatch   ErrorCode = 8
	ErrCodeTimeout            ErrorCode = 9
	ErrCodeProtocolError      ErrorCode = 10
)

// Chunk-frame flag bits.
const (
	FlagCompressed = 1 << 0 // payload is zstd-compressed
	FlagLastChunk  = 1 << 1 // last chunk of this file
)
