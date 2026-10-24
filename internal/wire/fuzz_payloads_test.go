package wire

import (
	"bytes"
	"testing"
)

// FuzzDecodePayloads feeds arbitrary bodies to the gob decode of every
// control payload a peer can send. Decoding may fail; it must not panic,
// hang, or allocate without bound (the frame layer caps bodies at 64 KiB).
func FuzzDecodePayloads(f *testing.F) {
	root := bytes.Repeat([]byte{0xFF}, 32)
	for _, p := range []any{
		&SenderHello{MinVersion: MinProtocolVersion, MaxVersion: ProtocolVersion, Hostname: "h", DisplayName: "d"},
		&ReceiverHello{MinVersion: MinProtocolVersion, MaxVersion: ProtocolVersion, Accepts: true},
		&PasswordChallenge{}, &PasswordResponse{}, &PasswordVerified{},
		[]ListingEntry{{Index: 0, RelativePath: "a/b", Size: 9, Type: EntryFile}, {Index: 1, RelativePath: "a", Type: EntryDir}},
		[]Decision{{Index: 0, Action: DecisionResume, ResumeOffset: 1 << 20, PrefixRoot: root}, {Index: 1, Kept: true}},
		[]uint32{0, 1, 2},
		[]FileHash{{Index: 3}},
		&ErrorFrame{Code: ErrCodeDiskFull, Message: "needs 1 GB"},
	} {
		var buf bytes.Buffer
		if err := WriteControl(&buf, TypeError, p); err != nil {
			f.Fatal(err)
		}
		f.Add(buf.Bytes()[controlHeaderSize:])
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, body []byte) {
		targets := []any{
			new(SenderHello), new(ReceiverHello), new(PasswordChallenge), new(PasswordResponse),
			new(PasswordVerified), new([]ListingEntry), new([]Decision), new([]uint32),
			new([]FileHash), new(ErrorFrame),
		}
		for _, v := range targets {
			_ = Decode(body, v)
		}
	})
}
