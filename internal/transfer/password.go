package transfer

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/pake"
	"github.com/polius/fsend/internal/wire"
)

// PasswordAttempts is how many password tries the sender allows per session
// before aborting the transfer. A typo at the receiver's no-echo prompt
// shouldn't burn the code.
const PasswordAttempts = 3

var (
	adPwSender   = []byte("fsend password sender")
	adPwReceiver = []byte("fsend password receiver")
)

// The --password gate is a second CPace run inside the authenticated
// session, with the password as the secret, the session binding as the
// channel id and a fresh nonce as the session id. Each attempt is one
// online guess for whoever is on the other end; nothing on the wire
// supports an offline dictionary attack, and the sender proves it knows
// the password too (mutual), so an impostor sender learns nothing.

// senderPasswordHandshake runs up to PasswordAttempts password PAKEs. Each
// mismatch below the cap answers with a WrongPassword error and a fresh
// challenge; the final mismatch closes the stream.
func senderPasswordHandshake(s *Streams, password string) error {
	for attempt := 1; ; attempt++ {
		var nonce [32]byte
		if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
			return fmt.Errorf("send: password nonce: %w", err)
		}
		st, err := pake.New(pake.Initiator, pake.Params{PRS: []byte(password), CI: s.Binding, SID: nonce[:], AD: adPwSender})
		if err != nil {
			return fmt.Errorf("send: password pake: %w", err)
		}
		ch := wire.PasswordChallenge{Nonce: nonce}
		copy(ch.Share[:], st.Share())
		if err := wire.WriteControl(s.Control, wire.TypePasswordChallenge, &ch); err != nil {
			// A receiver with a fixed password hangs up after its mismatch;
			// the honest error is still the password.
			if attempt > 1 {
				return fserrors.ErrWrongPassword
			}
			return fmt.Errorf("send: password challenge: %w", err)
		}
		ft, body, err := wire.ReadControlRaw(s.Control)
		if err != nil {
			if attempt > 1 {
				return fserrors.ErrWrongPassword
			}
			return fmt.Errorf("send: read password response: %w", err)
		}
		if ft == wire.TypeError {
			return peerError(body)
		}
		if ft != wire.TypePasswordResponse {
			return fmt.Errorf("%w: expected PASSWORD_RESPONSE, got %v", fserrors.ErrProtocolError, ft)
		}
		var resp wire.PasswordResponse
		if err := wire.Decode(body, &resp); err != nil {
			return fmt.Errorf("send: decode password response: %w", err)
		}
		isk, err := st.Finish(resp.Share[:], adPwReceiver)
		if err != nil {
			return fmt.Errorf("%w: password pake: %v", fserrors.ErrProtocolError, err)
		}
		want := passwordTag(isk, adPwReceiver, nonce[:], ch.Share[:], resp.Share[:])
		if subtle.ConstantTimeCompare(want, resp.Tag[:]) == 1 {
			var v wire.PasswordVerified
			copy(v.Tag[:], passwordTag(isk, adPwSender, nonce[:], ch.Share[:], resp.Share[:]))
			if err := wire.WriteControl(s.Control, wire.TypePasswordVerified, &v); err != nil {
				return fmt.Errorf("send: password verified: %w", err)
			}
			return nil
		}
		_ = wire.WriteControl(s.Control, wire.TypeError, &wire.ErrorFrame{
			Code: wire.ErrCodeWrongPassword, Message: "wrong password",
		})
		if attempt == PasswordAttempts {
			_ = s.Control.Close()
			_, _ = io.Copy(io.Discard, s.Control)
			return fserrors.ErrWrongPassword
		}
	}
}

// receiverPasswordHandshake answers the sender's password PAKEs. A fixed
// password (--password / FSEND_PASSWORD) gets one try — resending the same
// value can't succeed; a prompted one gets up to PasswordAttempts. The loop
// is bounded by the same cap the sender enforces, so a hostile sender can't
// hold the receiver in an endless prompt.
func receiverPasswordHandshake(s *Streams, opts RecvOptions) error {
	for attempt := 1; attempt <= PasswordAttempts; attempt++ {
		var ch wire.PasswordChallenge
		ft, err := wire.ReadControl(s.Control, &ch)
		if err != nil {
			if attempt > 1 {
				return fserrors.ErrWrongPassword
			}
			return fmt.Errorf("recv: read password challenge: %w", err)
		}
		if ft != wire.TypePasswordChallenge {
			return fmt.Errorf("%w: expected PASSWORD_CHALLENGE, got %v", fserrors.ErrProtocolError, ft)
		}
		password := opts.Password
		if password == "" {
			if opts.PromptPass == nil {
				declineTransfer(s, wire.ErrCodePasswordRequired, "receiver has no password to offer")
				return fserrors.ErrPasswordRequired
			}
			password, err = opts.PromptPass(attempt)
			if err != nil {
				return fmt.Errorf("recv: read password: %w", err)
			}
		}
		st, err := pake.New(pake.Responder, pake.Params{PRS: []byte(password), CI: s.Binding, SID: ch.Nonce[:], AD: adPwReceiver})
		if err != nil {
			return fmt.Errorf("recv: password pake: %w", err)
		}
		isk, err := st.Finish(ch.Share[:], adPwSender)
		if err != nil {
			return fmt.Errorf("%w: password pake: %v", fserrors.ErrProtocolError, err)
		}
		var resp wire.PasswordResponse
		copy(resp.Share[:], st.Share())
		copy(resp.Tag[:], passwordTag(isk, adPwReceiver, ch.Nonce[:], ch.Share[:], resp.Share[:]))
		if err := wire.WriteControl(s.Control, wire.TypePasswordResponse, &resp); err != nil {
			return fmt.Errorf("recv: write password response: %w", err)
		}
		ft, body, err := wire.ReadControlRaw(s.Control)
		if err != nil {
			return fmt.Errorf("recv: read password verdict: %w", err)
		}
		switch ft {
		case wire.TypePasswordVerified:
			var v wire.PasswordVerified
			if err := wire.Decode(body, &v); err != nil {
				return fmt.Errorf("recv: decode password verdict: %w", err)
			}
			want := passwordTag(isk, adPwSender, ch.Nonce[:], ch.Share[:], resp.Share[:])
			if subtle.ConstantTimeCompare(want, v.Tag[:]) != 1 {
				// The sender claimed success without knowing the password.
				return fserrors.ErrPeerAuthFailed
			}
			return nil
		case wire.TypeError:
			var ef wire.ErrorFrame
			_ = wire.Decode(body, &ef)
			if ef.Code != wire.ErrCodeWrongPassword {
				return mapPeerError(ef)
			}
			if opts.Password == "" && opts.PromptPass != nil {
				continue // loop back for the sender's next challenge
			}
			return fserrors.ErrWrongPassword
		default:
			return fmt.Errorf("%w: expected PASSWORD_VERIFIED, got %v", fserrors.ErrProtocolError, ft)
		}
	}
	return fserrors.ErrWrongPassword
}

// passwordTag is one side's key-confirmation tag for a password PAKE run.
func passwordTag(isk, who, nonce, senderShare, receiverShare []byte) []byte {
	m := hmac.New(sha256.New, isk)
	m.Write(who)
	m.Write(nonce)
	m.Write(senderShare)
	m.Write(receiverShare)
	return m.Sum(nil)
}
