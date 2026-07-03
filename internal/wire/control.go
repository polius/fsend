package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
)

// Control-frame header layout:
//
//   +--------+--------+--------+--------+--------+--------+
//   | ver(1) |   type(1)       |   length(uint32 BE)     |
//   +--------+--------+--------+--------+--------+--------+
//   |                payload (length bytes)              |
//   +----------------------------------------------------+
//
// payload is the gob encoding of one of the structs in payloads.go.

const controlHeaderSize = 6 // 1 ver + 1 type + 4 length

// ErrUnsupportedVersion is returned when a frame's version byte doesn't
// match the version we speak.
var ErrUnsupportedVersion = errors.New("wire: unsupported protocol version")

// ErrFrameTooLarge is returned when a frame's declared length exceeds
// MaxControlFrameSize.
var ErrFrameTooLarge = errors.New("wire: frame too large")

// WriteControl serializes payload as gob, wraps it in a control-frame
// header of the given type, and writes the result to w.
//
// payload must be one of the types from payloads.go. Passing the wrong
// type for a frame type is a programming error caught by the receiver.
func WriteControl(w io.Writer, ft FrameType, payload any) error {
	var buf bytes.Buffer
	if payload != nil {
		if err := gob.NewEncoder(&buf).Encode(payload); err != nil {
			return fmt.Errorf("wire: encoding %T: %w", payload, err)
		}
	}
	body := buf.Bytes()
	if len(body) > MaxControlFrameSize {
		return fmt.Errorf("%w: control frame %d bytes exceeds limit %d", ErrFrameTooLarge, len(body), MaxControlFrameSize)
	}

	header := [controlHeaderSize]byte{ProtocolVersion, byte(ft)}
	binary.BigEndian.PutUint32(header[2:6], uint32(len(body)))

	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("wire: writing header: %w", err)
	}
	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			return fmt.Errorf("wire: writing body: %w", err)
		}
	}
	return nil
}

// ReadControl reads one control frame from r and gob-decodes the payload
// into the given pointer.
//
// If payloadPtr is nil, the body is discarded (useful for empty frames
// like TransferComplete / TransferAck / Abort).
//
// The returned FrameType is always populated, even on error after the
// header has been read — callers can distinguish "wrong frame type" from
// "bad version."
func ReadControl(r io.Reader, payloadPtr any) (FrameType, error) {
	ft, body, err := ReadControlRaw(r)
	if err != nil {
		return ft, err
	}
	if len(body) == 0 || payloadPtr == nil {
		return ft, nil
	}
	if err := gob.NewDecoder(bytes.NewReader(body)).Decode(payloadPtr); err != nil {
		return ft, fmt.Errorf("wire: decoding %T: %w", payloadPtr, err)
	}
	return ft, nil
}

// ReadControlRaw reads one control frame from r and returns its type
// and the still-gob-encoded body. Callers that need to dispatch on
// FrameType before decoding (e.g. a negotiate loop that may receive a
// data frame or an ERROR) use this to avoid a wasted decode against the
// wrong target type.
func ReadControlRaw(r io.Reader) (FrameType, []byte, error) {
	var header [controlHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err // surface EOF / connection-closed unwrapped
	}
	ver := header[0]
	ft := FrameType(header[1])
	length := binary.BigEndian.Uint32(header[2:6])

	if ver != ProtocolVersion {
		return ft, nil, fmt.Errorf("%w: peer sent version %d, we speak %d", ErrUnsupportedVersion, ver, ProtocolVersion)
	}
	if length > MaxControlFrameSize {
		return ft, nil, fmt.Errorf("%w: declared %d bytes exceeds limit %d", ErrFrameTooLarge, length, MaxControlFrameSize)
	}
	if length == 0 {
		return ft, nil, nil
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return ft, nil, fmt.Errorf("wire: reading body: %w", err)
	}
	return ft, body, nil
}

// Decode parses a raw control body into the supplied pointer. Used in
// conjunction with ReadControlRaw when the caller already knows the
// frame type and wants to decode the body into the matching struct.
func Decode(body []byte, payloadPtr any) error {
	if len(body) == 0 || payloadPtr == nil {
		return nil
	}
	if err := gob.NewDecoder(bytes.NewReader(body)).Decode(payloadPtr); err != nil {
		return fmt.Errorf("wire: decoding %T: %w", payloadPtr, err)
	}
	return nil
}
