package wire

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/quick"
)

func TestControl_HelloRoundtrip(t *testing.T) {
	want := SenderHello{
		ProtocolVersion: ProtocolVersion,
		Hostname:        "Pol's MacBook",
		OS:              "darwin",
		ClientVersion:   "0.1.0",
		TransferKind:    TransferSingleFile,
		TotalFiles:      1,
		TotalBytes:      4404019,
	}
	var buf bytes.Buffer
	if err := WriteControl(&buf, TypeHello, &want); err != nil {
		t.Fatalf("WriteControl: %v", err)
	}

	var got SenderHello
	ft, err := ReadControl(&buf, &got)
	if err != nil {
		t.Fatalf("ReadControl: %v", err)
	}
	if ft != TypeHello {
		t.Errorf("frame type = %v, want TypeHello", ft)
	}
	if got != want {
		t.Errorf("roundtrip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestControl_EmptyPayload(t *testing.T) {
	// TransferComplete carries no payload.
	var buf bytes.Buffer
	if err := WriteControl(&buf, TypeTransferComplete, nil); err != nil {
		t.Fatalf("WriteControl: %v", err)
	}
	ft, err := ReadControl(&buf, nil)
	if err != nil {
		t.Fatalf("ReadControl: %v", err)
	}
	if ft != TypeTransferComplete {
		t.Errorf("frame type = %v, want TypeTransferComplete", ft)
	}
}

func TestControl_VersionMismatchReportedClearly(t *testing.T) {
	// Synthesize a frame with a bogus version byte.
	buf := bytes.NewBuffer([]byte{0xFF /* bad ver */, byte(TypeHello), 0, 0, 0, 0})
	_, err := ReadControl(buf, nil)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("expected ErrUnsupportedVersion, got %v", err)
	}
}

func TestControl_RejectOversizedFrame(t *testing.T) {
	// Synthesize a header claiming a frame larger than the limit.
	buf := bytes.NewBuffer([]byte{ProtocolVersion, byte(TypeHello), 0xFF, 0xFF, 0xFF, 0xFF})
	_, err := ReadControl(buf, nil)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestControl_TruncatedReadReturnsEOF(t *testing.T) {
	// Empty input → EOF (callers use this to detect peer-closed stream).
	_, err := ReadControl(bytes.NewReader(nil), nil)
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestControl_FileInfoWithBigPaths(t *testing.T) {
	// Stress: a long Unicode path with edge characters.
	want := FileInfo{
		Index:        42,
		RelativePath: "项目/子目录/文件 名.txt",
		Size:         1234567,
		Mode:         0o644,
		ModTime:      1717414800000000000,
		IsDir:        false,
		Resumable:    true,
	}
	for i := range want.Blake3Root {
		want.Blake3Root[i] = byte(i)
	}
	var buf bytes.Buffer
	if err := WriteControl(&buf, TypeFileInfo, &want); err != nil {
		t.Fatal(err)
	}
	var got FileInfo
	if _, err := ReadControl(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("FileInfo roundtrip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestControl_FileInfoStreaming exercises the additive Streaming field.
// On the streaming path Size and Blake3Root are zero; Resumable is false.
// The round-trip must preserve the flag so an older receiver (Streaming
// not present in its decoded struct) still observes the expected zero
// default for the unrelated size-driven fields.
func TestControl_FileInfoStreaming(t *testing.T) {
	want := FileInfo{
		Index:        0,
		RelativePath: "fsend-stdin-abc12345",
		Mode:         0o644,
		ModTime:      1717414800000000000,
		Streaming:    true,
	}
	var buf bytes.Buffer
	if err := WriteControl(&buf, TypeFileInfo, &want); err != nil {
		t.Fatal(err)
	}
	var got FileInfo
	if _, err := ReadControl(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Streaming FileInfo roundtrip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
	if got.Size != 0 || got.Blake3Root != [32]byte{} || got.Resumable {
		t.Errorf("streaming preconditions violated on decode: %+v", got)
	}
}

func TestChunk_Roundtrip(t *testing.T) {
	original := &Chunk{
		Flags:      FlagCompressed | FlagLastChunk,
		FileIndex:  7,
		ChunkIndex: 42,
		Payload:    []byte("hello world, this is a chunk payload"),
	}
	for i := range original.Blake3Hash {
		original.Blake3Hash[i] = byte(i * 3)
	}

	var buf bytes.Buffer
	if err := WriteChunk(&buf, original); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	got, err := ReadChunk(&buf)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if got.Flags != original.Flags {
		t.Errorf("Flags: got %x, want %x", got.Flags, original.Flags)
	}
	if got.FileIndex != original.FileIndex {
		t.Errorf("FileIndex: got %d, want %d", got.FileIndex, original.FileIndex)
	}
	if got.ChunkIndex != original.ChunkIndex {
		t.Errorf("ChunkIndex: got %d, want %d", got.ChunkIndex, original.ChunkIndex)
	}
	if got.Blake3Hash != original.Blake3Hash {
		t.Error("Blake3Hash mismatch")
	}
	if !bytes.Equal(got.Payload, original.Payload) {
		t.Errorf("Payload mismatch: got %q, want %q", got.Payload, original.Payload)
	}
}

func TestChunk_EmptyPayload(t *testing.T) {
	original := &Chunk{
		Flags:      FlagLastChunk,
		FileIndex:  0,
		ChunkIndex: 0,
	}
	var buf bytes.Buffer
	if err := WriteChunk(&buf, original); err != nil {
		t.Fatal(err)
	}
	got, err := ReadChunk(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Payload) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(got.Payload))
	}
}

func TestChunk_MaxSize(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, MaxChunkSize)
	original := &Chunk{Payload: payload}
	var buf bytes.Buffer
	if err := WriteChunk(&buf, original); err != nil {
		t.Fatal(err)
	}
	got, err := ReadChunk(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Error("max-size chunk corrupted on roundtrip")
	}
}

func TestChunk_TooLarge(t *testing.T) {
	original := &Chunk{Payload: make([]byte, MaxChunkSize+1)}
	var buf bytes.Buffer
	if err := WriteChunk(&buf, original); !errors.Is(err, ErrChunkTooLarge) {
		t.Errorf("WriteChunk should reject oversized, got %v", err)
	}
}

func TestChunk_RejectWrongTypeOnDataStream(t *testing.T) {
	// Forge a frame whose first byte isn't TypeChunk.
	hdr := []byte{
		0x99, 0x00, // bogus type, no flags
		0x00, 0x00, 0x00, 0x00, // length 0
		0x00, 0x00, 0x00, 0x00, // file index
		0x00, 0x00, 0x00, 0x00, // chunk index
	}
	// Pad to chunkHeaderSize so ReadFull succeeds.
	hdr = append(hdr, bytes.Repeat([]byte{0}, chunkHeaderSize-len(hdr))...)
	_, err := ReadChunk(bytes.NewReader(hdr))
	if !errors.Is(err, ErrWrongFrameType) {
		t.Errorf("expected ErrWrongFrameType, got %v", err)
	}
}

// Property test: any payload bytes survive a round-trip.
func TestChunk_PropertyRoundtrip(t *testing.T) {
	prop := func(flags uint8, fileIdx, chunkIdx uint32, payload []byte) bool {
		if len(payload) > MaxChunkSize {
			return true // not a valid input; skip
		}
		var hash [32]byte
		for i := range hash {
			hash[i] = byte(i)
		}
		c := &Chunk{
			Flags:      flags,
			FileIndex:  fileIdx,
			ChunkIndex: chunkIdx,
			Blake3Hash: hash,
			Payload:    payload,
		}
		var buf bytes.Buffer
		if err := WriteChunk(&buf, c); err != nil {
			return false
		}
		got, err := ReadChunk(&buf)
		if err != nil {
			return false
		}
		return got.Flags == c.Flags &&
			got.FileIndex == c.FileIndex &&
			got.ChunkIndex == c.ChunkIndex &&
			got.Blake3Hash == c.Blake3Hash &&
			bytes.Equal(got.Payload, c.Payload)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
}

// Property test: control frames with random ErrorFrame payloads round-trip.
func TestControl_ErrorFramePropertyRoundtrip(t *testing.T) {
	prop := func(code uint16, msgLen int) bool {
		if msgLen < 0 {
			msgLen = -msgLen
		}
		if msgLen > 4096 {
			msgLen = msgLen % 4096
		}
		msg := strings.Repeat("x", msgLen)
		want := ErrorFrame{Code: ErrorCode(code), Message: msg}
		var buf bytes.Buffer
		if err := WriteControl(&buf, TypeError, &want); err != nil {
			return false
		}
		var got ErrorFrame
		if _, err := ReadControl(&buf, &got); err != nil {
			return false
		}
		return got == want
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}
