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
// used to bind the PAKE run to this specific TLS session.
// Bumping this is a wire-protocol-major event.
const exporterLabel = "EXPORTER-fsend-channel-binding-v3"

const (
	exporterLen = 32
	tagLen      = 32 // HMAC-SHA256
	// pakeMsgLen is suite byte + suite version + CPace share.
	pakeMsgLen    = 2 + pake.ShareSize
	maxPakeMsgLen = 1024
	// suiteCPace tags the PAKE message so an older peer (protocol v2 sent
	// 'S' + a 32-byte SPAKE2 element) is recognised instead of failing auth.
	// The wire protocol version is negotiated later, in the authenticated
	// HELLO; suiteVersion changes only if the PAKE itself does.
	suiteCPace   = 'C'
	suiteVersion = 1
	suiteSPAKE2  = 'S'
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

// PAKE associated data: names the role, so role confusion changes the key.
var (
	adSender   = []byte("fsend sender")
	adReceiver = []byte("fsend receiver")
)

// authenticatePeer runs CPace over the control stream with the short code
// as the secret and the TLS exporter as the session id, then exchanges
// direction-separated key-confirmation tags.
//
// The exporter binding defeats a relay/MITM: an attacker terminating one
// TLS session with each side gets two different exporters, so the derived
// keys can never match. That holds as long as the attacker doesn't know
// the code, which the pairing server never sees (it gets only an argon2id
// slot, see internal/code.Slot). The PAKE is the only authentication
// layer: TLS certificates are ephemeral and unverified.
//
// The confirmation tags are direction-separated so a peer that never
// learned the code can't echo the honest tag back.
//
// Returns the session binding (a secret both peers share only after a
// successful run) for later in-session PAKEs such as --password. Any
// failure — wrong code, MITM, reflection, malformed input — returns
// fserrors.ErrPeerAuthFailed; a v2 peer returns ErrIncompatibleVersion.
func authenticatePeer(conn *quic.Conn, control io.ReadWriter, code string, r role) (binding []byte, err error) {
	// Defense in depth: no peer-controlled input may crash the process.
	defer func() {
		if p := recover(); p != nil {
			binding, err = nil, fmt.Errorf("%w: internal error: %v", fserrors.ErrPeerAuthFailed, p)
		}
	}()

	tlsState := conn.ConnectionState().TLS
	exporter, err := tlsState.ExportKeyingMaterial(exporterLabel, nil, exporterLen)
	if err != nil {
		return nil, fmt.Errorf("auth: tls exporter: %w", err)
	}

	myAD, peerAD, pr := adSender, adReceiver, pake.Initiator
	if r == roleReceiver {
		myAD, peerAD, pr = adReceiver, adSender, pake.Responder
	}
	st, err := pake.New(pr, pake.Params{PRS: []byte(code), CI: []byte("fsend"), SID: exporter, AD: myAD})
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}

	// Both sides write first, then read. QUIC bidi streams are full-duplex,
	// so this is deadlock-free regardless of which side is "sender".
	myMsg := append([]byte{suiteCPace, suiteVersion}, st.Share()...)
	if err := writeFramed(control, myMsg); err != nil {
		return nil, fmt.Errorf("auth: send pake: %w", err)
	}
	peerMsg, err := readFramed(control, maxPakeMsgLen)
	if err != nil {
		return nil, fmt.Errorf("auth: recv pake: %w", err)
	}
	peerShare, err := parsePakeMsg(peerMsg)
	if err != nil {
		return nil, err
	}
	key, err := st.Finish(peerShare, peerAD)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", fserrors.ErrPeerAuthFailed, err)
	}

	// Order the transcript by role so both peers hash the same bytes.
	senderMsg, receiverMsg := myMsg, peerMsg
	if r == roleReceiver {
		senderMsg, receiverMsg = peerMsg, myMsg
	}
	myTag := confirmTag(key, exporter, r, senderMsg, receiverMsg)
	wantTag := confirmTag(key, exporter, otherRole(r), senderMsg, receiverMsg)

	if _, err := control.Write(myTag); err != nil {
		return nil, fmt.Errorf("auth: send tag: %w", err)
	}
	var peerTag [tagLen]byte
	if _, err := io.ReadFull(control, peerTag[:]); err != nil {
		return nil, fmt.Errorf("auth: recv tag: %w", err)
	}
	if subtle.ConstantTimeCompare(wantTag, peerTag[:]) != 1 {
		return nil, fserrors.ErrPeerAuthFailed
	}
	return sessionBinding(key, exporter), nil
}

// parsePakeMsg validates the framed PAKE message and returns the share.
func parsePakeMsg(msg []byte) ([]byte, error) {
	switch {
	case len(msg) == 1+32 && msg[0] == suiteSPAKE2:
		return nil, fmt.Errorf("%w: peer speaks protocol v2", fserrors.ErrIncompatibleVersion)
	case len(msg) >= 2 && msg[0] == suiteCPace && msg[1] != suiteVersion:
		return nil, fmt.Errorf("%w: peer uses PAKE suite version %d", fserrors.ErrIncompatibleVersion, msg[1])
	case len(msg) != pakeMsgLen || msg[0] != suiteCPace:
		return nil, fmt.Errorf("%w: malformed pake message (%d bytes)", fserrors.ErrPeerAuthFailed, len(msg))
	}
	return msg[2:], nil
}

func otherRole(r role) role {
	if r == roleSender {
		return roleReceiver
	}
	return roleSender
}

// confirmTag binds the confirmation HMAC to the derived key, the TLS
// session (exporter), the direction, and the full PAKE transcript.
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

// sessionBinding derives a per-session secret from the PAKE key, distinct
// from the confirmation tags.
func sessionBinding(key, exporter []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("fsend-session-binding"))
	mac.Write(exporter)
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
		return nil, fmt.Errorf("%w: empty pake msg", fserrors.ErrPeerAuthFailed)
	}
	if n > max {
		return nil, fmt.Errorf("%w: pake msg too large: %d", fserrors.ErrPeerAuthFailed, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
