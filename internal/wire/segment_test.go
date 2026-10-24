package wire

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestChunk_SegmentFlagsRoundtrip(t *testing.T) {
	original := &Chunk{
		Payload: []byte("abcdef"),
		Segments: []Segment{
			{FileIndex: 1, Skip: true},
			{FileIndex: 2, Length: 3, Restart: true},
			{FileIndex: 2, Length: 3, EOF: true, Changed: true},
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
	if !reflect.DeepEqual(got.Segments, original.Segments) {
		t.Errorf("flags lost:\ngot:  %+v\nwant: %+v", got.Segments, original.Segments)
	}
}

// Empty data segments and contradictory flags are refused at parse time, so
// a peer can't open files or spin the receiver with zero-length frames.
func TestChunk_RejectMalformedSegments(t *testing.T) {
	for name, seg := range map[string]Segment{
		"empty":         {FileIndex: 1},
		"skip with eof": {FileIndex: 1, Skip: true, EOF: true},
		"skip with len": {FileIndex: 1, Skip: true, Length: 1},
		"changed alone": {FileIndex: 1, Length: 1, Changed: true},
		"empty restart": {FileIndex: 1, Restart: true, EOF: true},
	} {
		var buf bytes.Buffer
		c := &Chunk{Segments: []Segment{seg}, Payload: make([]byte, seg.Length)}
		if err := WriteChunk(&buf, c); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		if _, err := ReadChunk(&buf); !errors.Is(err, ErrMalformedChunk) {
			t.Errorf("%s: err = %v, want ErrMalformedChunk", name, err)
		}
	}
}
