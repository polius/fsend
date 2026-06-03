package wire

// Payload structs for the gob-encoded control frames.
//
// All time-bearing fields use int64 unix-nanoseconds to keep gob output
// deterministic across timezone settings. Strings are UTF-8 and bounded by
// MaxControlFrameSize at the frame layer.

// SenderHello is the first control frame the sender emits, immediately
// after the TLS+PAKE handshake completes.
type SenderHello struct {
	ProtocolVersion uint8        // must equal ProtocolVersion
	Hostname        string       // sender's hostname or --name override
	OS              string       // "linux" | "darwin" | "windows" | ...
	ClientVersion   string       // fsend semver
	TransferKind    TransferKind // single-file, multi-file, directory, stdin, text
	TotalFiles      uint32       // 1 for single, N for multi/dir, 0 if unknown
	TotalBytes      uint64       // sum of file sizes (0 if unknown, e.g. stdin)
	HasPassword     bool         // true → expect PASSWORD_CHALLENGE next
	CompressionHint uint8        // 0=none, 1=zstd-auto-per-chunk
}

// ReceiverHello is the receiver's response to SenderHello. Carries the
// receiver's identity and the user's accept/reject decision.
type ReceiverHello struct {
	Hostname      string
	OS            string
	ClientVersion string
	Accepts       bool // false → user rejected at prompt; sender should ABORT
}

// FileInfo describes one file (or directory entry, or symlink) in the
// transfer. The sender emits these in walk order before any chunks flow.
type FileInfo struct {
	Index         uint32 // 0-based within the transfer
	RelativePath  string // forward-slash, no leading slash, no .. segments
	Size          uint64 // bytes (0 for empty files, dirs, symlinks)
	Mode          uint32 // unix mode bits; mapped to closest equivalent on Windows
	ModTime       int64  // unix nanoseconds
	IsDir         bool   // true → no data, just create the directory
	IsSymlink     bool   // true → SymlinkTarget set, no data
	SymlinkTarget string // only if IsSymlink
	Blake3Root    [32]byte // BLAKE3 hash of the full plaintext file
	Resumable     bool   // false for stdin/text transfers
}

// FileAcceptDecision is the receiver's per-file response to a FILE_INFO.
type FileAcceptDecision struct {
	Index        uint32 // matches the FileInfo this responds to
	Action       FileAcceptAction
	ResumeOffset uint64 // only when Action == ActionResume; chunk-boundary aligned
}

// ErrorFrame carries a numbered error from one peer to the other.
type ErrorFrame struct {
	Code    ErrorCode
	Message string // human-readable
}

// ProgressAck is a receiver→sender heartbeat for live progress display.
// Carried on the receiver-control stream (not the main control stream).
type ProgressAck struct {
	FileIndex  uint32
	BytesAcked uint64
}

// ResumeRequest is a v2 facility for mid-transfer "skip ahead." v1
// reserves the type byte but doesn't use it.
type ResumeRequest struct {
	FileIndex  uint32
	FromOffset uint64
}
