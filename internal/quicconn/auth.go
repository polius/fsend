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

// authenticatePeer runs symmetric SPAKE2 over the control stream and
// verifies that both peers derived the same key AND observed the same
// TLS session. The latter half — mixing the TLS RFC 5705 exporter into
// an HMAC tag — is the channel binding that makes a relay/MITM attempt
// fail: an attacker terminating one TLS session with each side ends up
// with two different exporters, so the tags can never match.
//
// Must be called immediately after the QUIC handshake and the control
// stream are up, before any application data flows. On mismatch (wrong
// code, MITM, or wire tampering) returns fserrors.ErrPeerAuthFailed.
func authenticatePeer(conn *quic.Conn, control io.ReadWriter, code string) error {
	p := pake.New(code)

	// Both sides write first, then read. QUIC bidi streams are full-duplex,
	// so this is deadlock-free regardless of which side is "sender".
	if err := writeFramed(control, p.Start()); err != nil {
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
	mac := hmac.New(sha256.New, key)
	mac.Write(exporter)
	myTag := mac.Sum(nil)

	if _, err := control.Write(myTag); err != nil {
		return fmt.Errorf("auth: send tag: %w", err)
	}
	var peerTag [tagLen]byte
	if _, err := io.ReadFull(control, peerTag[:]); err != nil {
		return fmt.Errorf("auth: recv tag: %w", err)
	}
	if subtle.ConstantTimeCompare(myTag, peerTag[:]) != 1 {
		return fserrors.ErrPeerAuthFailed
	}
	return nil
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
	if n > max {
		return nil, fmt.Errorf("auth: pake msg too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
