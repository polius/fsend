package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/quick"
)

func TestControl_HelloRoundtrip(t *testing.T) {
	want := SenderHello{
		MinVersion: MinProtocolVersion, MaxVersion: ProtocolVersion,
		Hostname:      "Pol's MacBook",
		OS:            "darwin",
		ClientVersion: "0.1.0",
		Mode:          ModeFiles,
		DisplayName:   "my-project/",
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
	buf := bytes.NewBuffer([]byte{0xFF /* bad ver */, byte(TypeHello), 0, 0, 0, 0})
	_, err := ReadControl(buf, nil)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("expected ErrUnsupportedVersion, got %v", err)
	}
}

func TestControl_RejectOversizedFrame(t *testing.T) {
	buf := bytes.NewBuffer([]byte{ProtocolVersion, byte(TypeHello), 0xFF, 0xFF, 0xFF, 0xFF})
	_, err := ReadControl(buf, nil)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestControl_TruncatedReadReturnsEOF(t *testing.T) {
	_, err := ReadControl(bytes.NewReader(nil), nil)
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestControl_ListingBatchRoundtrip(t *testing.T) {
	want := []ListingEntry{
		{Index: 0, RelativePath: "项目/子目录/文件 名.txt", Size: 1234567, ModTimeSec: 1717414800, Mode: 0o644, Type: EntryFile},
		{Index: 1, RelativePath: "sub", Type: EntryDir, Mode: 0o755, ModTimeSec: 1717414800},
		{Index: 2, RelativePath: "link", Type: EntrySymlink, SymlinkTarget: "../target"},
	}
	var buf bytes.Buffer
	if err := WriteControl(&buf, TypeListingBatch, want); err != nil {
		t.Fatal(err)
	}
	var got []ListingEntry
	ft, err := ReadControl(&buf, &got)
	if err != nil {
		t.Fatal(err)
	}
	if ft != TypeListingBatch {
		t.Errorf("frame type = %v, want TypeListingBatch", ft)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listing roundtrip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestControl_DecisionBatchRoundtrip(t *testing.T) {
	root := make([]byte, 32)
	for i := range root {
		root[i] = byte(i)
	}
	want := []Decision{
		{Index: 0, Action: DecisionSend},
		{Index: 1, Action: DecisionSkip},
		{Index: 2, Action: DecisionResume, ResumeOffset: 1 << 20, PrefixRoot: root},
	}
	var buf bytes.Buffer
	if err := WriteControl(&buf, TypeClassifyBatch, want); err != nil {
		t.Fatal(err)
	}
	var got []Decision
	if _, err := ReadControl(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decision roundtrip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestChunk_SingleSegmentRoundtrip(t *testing.T) {
	var root [32]byte
	for i := range root {
		root[i] = byte(i * 3)
	}
	original := &Chunk{
		Compressed: true,
		Payload:    []byte("hello world, this is a chunk payload"),
		Segments: []Segment{
			{FileIndex: 7, Length: 36, EOF: true, RootHash: root},
		},
	}
	var buf bytes.Buffer
	if err := WriteChunk(&buf, original); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	got, err := ReadChunk(&buf)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if got.Compressed != original.Compressed {
		t.Error("chunk header mismatch")
	}
	if !reflect.DeepEqual(got.Segments, original.Segments) {
		t.Errorf("segments mismatch:\ngot:  %+v\nwant: %+v", got.Segments, original.Segments)
	}
	if !bytes.Equal(got.Payload, original.Payload) {
		t.Errorf("payload mismatch: got %q", got.Payload)
	}
}

func TestChunk_MultiSegmentPacked(t *testing.T) {
	// Three small whole files packed into one uncompressed chunk.
	a, b, c := []byte("aaa"), []byte("bb"), []byte("cccc")
	payload := append(append(append([]byte{}, a...), b...), c...)
	var r1, r2, r3 [32]byte
	r1[0], r2[0], r3[0] = 1, 2, 3
	original := &Chunk{
		Payload: payload,
		Segments: []Segment{
			{FileIndex: 0, Length: uint32(len(a)), EOF: true, RootHash: r1},
			{FileIndex: 1, Length: uint32(len(b)), EOF: true, RootHash: r2},
			{FileIndex: 2, Length: uint32(len(c)), EOF: true, RootHash: r3},
		},
	}
	var buf bytes.Buffer
	if err := WriteChunk(&buf, original); err != nil {
		t.Fatal(err)
	}
	got, err := ReadChunk(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Segments, original.Segments) || !bytes.Equal(got.Payload, payload) {
		t.Error("packed multi-segment chunk corrupted")
	}
}

func TestChunk_LargeFileMidStreamSegment(t *testing.T) {
	// A non-EOF segment carries no root hash.
	original := &Chunk{
		Payload:  bytes.Repeat([]byte{0xAB}, 1024),
		Segments: []Segment{{FileIndex: 3, Length: 1024, EOF: false}},
	}
	var buf bytes.Buffer
	if err := WriteChunk(&buf, original); err != nil {
		t.Fatal(err)
	}
	got, err := ReadChunk(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Segments[0].EOF || got.Segments[0].RootHash != [32]byte{} {
		t.Error("non-EOF segment must not carry a root hash")
	}
}

func TestChunk_MaxSize(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, MaxChunkSize)
	original := &Chunk{Payload: payload, Segments: []Segment{{FileIndex: 0, Length: MaxChunkSize, EOF: true}}}
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
	original := &Chunk{Payload: make([]byte, MaxChunkSize+1), Segments: []Segment{{Length: MaxChunkSize + 1}}}
	var buf bytes.Buffer
	if err := WriteChunk(&buf, original); !errors.Is(err, ErrChunkTooLarge) {
		t.Errorf("WriteChunk should reject oversized, got %v", err)
	}
}

func TestChunk_RejectZeroSegments(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteChunk(&buf, &Chunk{Payload: []byte("x")}); !errors.Is(err, ErrMalformedChunk) {
		t.Errorf("expected ErrMalformedChunk for zero segments, got %v", err)
	}
}

func TestChunk_RejectSegmentSumMismatch(t *testing.T) {
	// Uncompressed payload whose segment lengths don't sum to it.
	var buf bytes.Buffer
	if err := WriteChunk(&buf, &Chunk{Payload: []byte("abcd"), Segments: []Segment{{Length: 2}}}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if _, err := ReadChunk(&buf); !errors.Is(err, ErrMalformedChunk) {
		t.Errorf("expected ErrMalformedChunk on sum mismatch, got %v", err)
	}
}

func TestChunk_RejectWrongTypeOnDataStream(t *testing.T) {
	hdr := make([]byte, chunkFixedHeader)
	hdr[0] = 0x99 // bogus type
	_, err := ReadChunk(bytes.NewReader(hdr))
	if !errors.Is(err, ErrWrongFrameType) {
		t.Errorf("expected ErrWrongFrameType, got %v", err)
	}
}

// A hostile header declaring thousands of segments must not translate into a
// matching allocation before any segment bytes arrive.
func TestChunk_HeaderOnlyNoPreallocation(t *testing.T) {
	hdr := make([]byte, chunkFixedHeader)
	hdr[0] = byte(TypeChunk)
	binary.BigEndian.PutUint16(hdr[6:8], 0xFFFF)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := ReadChunk(bytes.NewReader(hdr))
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("header-only frame should fail, not succeed")
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 1<<20 {
		t.Errorf("header-only frame allocated %d bytes (want ≤ 1 MiB)", grown)
	}
}

// Property test: any single-segment payload survives a round-trip.
func TestChunk_PropertyRoundtrip(t *testing.T) {
	prop := func(compressed bool, fileIdx uint32, payload []byte) bool {
		if len(payload) > MaxChunkSize {
			return true
		}
		var root [32]byte
		seg := Segment{FileIndex: fileIdx, Length: uint32(len(payload)), EOF: true, RootHash: root}
		// Compressed payloads need not match the segment sum, so only assert
		// the sum invariant for the uncompressed case the codec enforces.
		if compressed && len(payload) == 0 {
			return true
		}
		c := &Chunk{Compressed: compressed, Segments: []Segment{seg}, Payload: payload}
		var buf bytes.Buffer
		if err := WriteChunk(&buf, c); err != nil {
			return false
		}
		got, err := ReadChunk(&buf)
		if err != nil {
			return false
		}
		return got.Compressed == c.Compressed &&
			reflect.DeepEqual(got.Segments, c.Segments) && bytes.Equal(got.Payload, c.Payload)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
}

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

// Decision.Reason is additive: a peer built before it decodes a decision
// carrying one, and a decision from such a peer reads as Reason "".
func TestDecisionReasonIsAdditive(t *testing.T) {
	type oldDecision struct {
		Index        uint32
		Action       DecisionAction
		ResumeOffset uint64
		PrefixRoot   []byte
		Kept         bool
	}
	var buf bytes.Buffer
	if err := WriteControl(&buf, TypeClassifyBatch, []Decision{{Index: 3, Action: DecisionSkip, Kept: true, Reason: "reserved device name"}}); err != nil {
		t.Fatal(err)
	}
	var old []oldDecision
	if _, err := ReadControl(bytes.NewReader(buf.Bytes()), &old); err != nil || len(old) != 1 || old[0].Index != 3 || !old[0].Kept {
		t.Fatalf("old peer decode = %+v, %v", old, err)
	}
	buf.Reset()
	if err := WriteControl(&buf, TypeClassifyBatch, []oldDecision{{Index: 4, Action: DecisionSkip, Kept: true}}); err != nil {
		t.Fatal(err)
	}
	var cur []Decision
	if _, err := ReadControl(bytes.NewReader(buf.Bytes()), &cur); err != nil || len(cur) != 1 || cur[0].Reason != "" {
		t.Fatalf("new peer decode = %+v, %v", cur, err)
	}
	if !CapsSupported.Has(CapSkipReasons) {
		t.Error("this build must advertise CapSkipReasons")
	}
}

type versioned struct {
	bytes.Buffer
	v uint8
}

func (w *versioned) WireVersion() uint8 { return w.v }

// The stamping rule, independent of today's constants: HELLO and HELLO_ACK
// always carry MinProtocolVersion (a v3 reader rejects any other), later
// frames carry what the session negotiated — not the build's own newest
// version — and a stream with nothing negotiated falls back to the minimum.
func TestStampFollowsNegotiation(t *testing.T) {
	neg := &versioned{v: MinProtocolVersion + 1}
	for _, c := range []struct {
		w    io.Writer
		ft   FrameType
		want byte
	}{
		{neg, TypeHello, MinProtocolVersion},
		{neg, TypeHelloAck, MinProtocolVersion},
		{neg, TypeListingBatch, MinProtocolVersion + 1},
		{neg, TypeError, MinProtocolVersion + 1},
		{&bytes.Buffer{}, TypeError, MinProtocolVersion},
	} {
		if got := Stamp(c.w, c.ft); got != c.want {
			t.Errorf("Stamp(%T, %v) = %d, want %d", c.w, c.ft, got, c.want)
		}
	}
	// What a newer build must avoid: a frame stamped past a peer's newest
	// version is unreadable to it.
	newer := &versioned{v: ProtocolVersion + 1}
	if err := WriteControl(newer, TypeListingEnd, nil); err != nil {
		t.Fatal(err)
	}
	if newer.Bytes()[0] != ProtocolVersion+1 {
		t.Fatalf("WriteControl ignored the negotiated version: %x", newer.Bytes())
	}
	if _, _, err := ReadControlRaw(&newer.Buffer); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("a frame from a newer version must be refused, got %v", err)
	}
}

// Once a session has a version, a frame stamped with another one is
// refused: a peer can't switch versions mid-session. The hellos and an
// early ERROR at the minimum are the exceptions Stamp allows for.
func TestReadControl_RequiresSessionVersion(t *testing.T) {
	frame := func(ft FrameType) []byte {
		var b bytes.Buffer
		if err := WriteControl(&b, ft, nil); err != nil { // stamped MinProtocolVersion
			t.Fatal(err)
		}
		return b.Bytes()
	}
	session := uint8(MinProtocolVersion + 1) // a session two newer builds negotiated
	for _, c := range []struct {
		ft FrameType
		ok bool
	}{
		{TypeListingEnd, false},
		{TypeTransferComplete, false},
		{TypeHello, true},
		{TypeHelloAck, true},
		{TypeError, true},
	} {
		r := &versioned{v: session}
		r.Write(frame(c.ft))
		_, _, err := ReadControlRaw(r)
		if c.ok && err != nil {
			t.Errorf("%v stamped %d in a version %d session: %v, want it read", c.ft, MinProtocolVersion, session, err)
		}
		if !c.ok && !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("%v stamped %d in a version %d session: err = %v, want ErrUnsupportedVersion", c.ft, MinProtocolVersion, session, err)
		}
	}
	// Without a session version (before the hellos) the minimum is fine.
	if _, _, err := ReadControlRaw(bytes.NewReader(frame(TypeListingEnd))); err != nil {
		t.Errorf("an unversioned stream refused a minimum-version frame: %v", err)
	}
}
