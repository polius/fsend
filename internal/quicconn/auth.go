package quicconn

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/pake"
)

// exporterLabel is the TLS 1.3 RFC 5705 keying-material exporter label
// used to bind the SPAKE2-derived secret to this specific TLS session.
// Bumping this is a wire-protocol-major event.
const exporterLabel = "EXPORTER-fsend-channel-binding-v1"

const (
	exporterLen   = 32
	tagLen        = 32 // HMAC-SHA256
	maxPakeMsgLen = 1024
)

// role identifies which end of the transfer is authenticating. It is
// folded into the confirmation tag so the two directions carry distinct
// values — see authenticatePeer.
type role uint8

const (
	roleSender role = iota
	roleReceiver
)

// Direction labels mixed into the confirmation HMAC. Distinct per
// direction so a tag one side sends can never be replayed back as the
// tag the other side expects.
const (
	dirSenderToReceiver = "fsend-confirm-sender"
	dirReceiverToSender = "fsend-confirm-receiver"
)

// authenticatePeer runs symmetric SPAKE2 over the control stream and
// verifies that both peers derived the same key AND observed the same
// TLS session. It mixes the TLS RFC 5705 exporter into an HMAC tag —
// the channel binding that defeats a relay/MITM: an attacker terminating
// one TLS session with each side gets two different exporters, so the
// tags can never match.
//
// The tag is also direction-separated (sender and receiver compute
// different tags) and bound to the SPAKE2 transcript. Direction
// separation closes a reflection attack: without it both sides send and
// accept the same tag, so a peer that never learned the code could read
// the honest tag off the stream and echo it back to authenticate.
//
// Must be called immediately after the QUIC handshake and the control
// stream are up, before any application data flows. On mismatch (wrong
// code, MITM, reflection, or wire tampering) returns
// fserrors.ErrPeerAuthFailed.
func authenticatePeer(conn *quic.Conn, control io.ReadWriter, code string, r role) error {
	p := pake.New(code)

	// Both sides write first, then read. QUIC bidi streams are full-duplex,
	// so this is deadlock-free regardless of which side is "sender".
	myMsg := p.Start()
	if err := writeFramed(control, myMsg); err != nil {
		return fmt.Errorf("auth: send pake: %w", err)
	}
	peerMsg, err := readFramed(control, maxPakeMsgLen)
	if err != nil {
		return fmt.Errorf("auth: recv pake: %w", err)
	}
	key, err := p.Finish(peerMsg)
	if err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrPeerAuthFailed, err)
	}

	tlsState := conn.ConnectionState().TLS
	exporter, err := tlsState.ExportKeyingMaterial(exporterLabel, nil, exporterLen)
	if err != nil {
		return fmt.Errorf("auth: tls exporter: %w", err)
	}

	// Order the transcript by role so both peers hash the same bytes.
	senderMsg, receiverMsg := myMsg, peerMsg
	if r == roleReceiver {
		senderMsg, receiverMsg = peerMsg, myMsg
	}
	myTag := confirmTag(key, exporter, r, senderMsg, receiverMsg)
	wantTag := confirmTag(key, exporter, otherRole(r), senderMsg, receiverMsg)

	if _, err := control.Write(myTag); err != nil {
		return fmt.Errorf("auth: send tag: %w", err)
	}
	var peerTag [tagLen]byte
	if _, err := io.ReadFull(control, peerTag[:]); err != nil {
		return fmt.Errorf("auth: recv tag: %w", err)
	}
	if subtle.ConstantTimeCompare(wantTag, peerTag[:]) != 1 {
		return fserrors.ErrPeerAuthFailed
	}
	return nil
}

func otherRole(r role) role {
	if r == roleSender {
		return roleReceiver
	}
	return roleSender
}

// confirmTag binds the confirmation HMAC to the derived key, the TLS
// session (exporter), the direction, and the full SPAKE2 transcript.
// Messages are length-prefixed so the concatenation is unambiguous.
func confirmTag(key, exporter []byte, r role, senderMsg, receiverMsg []byte) []byte {
	dir := dirSenderToReceiver
	if r == roleReceiver {
		dir = dirReceiverToSender
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(exporter)
	mac.Write([]byte(dir))
	writeLenPrefixed(mac, senderMsg)
	writeLenPrefixed(mac, receiverMsg)
	return mac.Sum(nil)
}

func writeLenPrefixed(w io.Writer, b []byte) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	_, _ = w.Write(hdr[:]) // hash.Hash.Write never errors
	_, _ = w.Write(b)
}

func writeFramed(w io.Writer, msg []byte) error {
	if len(msg) > maxPakeMsgLen {
		return fmt.Errorf("auth: pake msg too large: %d", len(msg))
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(msg)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}

func readFramed(r io.Reader, max int) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 {
		// A crafted peer can send a zero-length frame; the SPAKE2 Finish
		// indexes msg[0] and would panic. Reject as an auth failure.
		return nil, fmt.Errorf("auth: empty pake msg")
	}
	if n > max {
		return nil, fmt.Errorf("auth: pake msg too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
