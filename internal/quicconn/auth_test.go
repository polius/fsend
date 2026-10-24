package quicconn

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/pake"
)

// A zero-length framed PAKE message must be rejected, not fed into the
// PAKE.
func TestReadFramed_RejectsEmptyFrame(t *testing.T) {
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], 0)
	_, err := readFramed(bytes.NewReader(hdr[:]), maxPakeMsgLen)
	if err == nil {
		t.Fatal("expected error on zero-length frame, got nil")
	}
}

// Wrong codes on the two peers must cause SenderHandshake /
// ReceiverHandshake to fail with ErrPeerAuthFailed, before any
// application data flows.
func TestHandshake_WrongCodeRejected(t *testing.T) {
	ln, err := ListenAddr("127.0.0.1:0", "abc-defg-hjk")
	if err != nil {
		t.Fatalf("ListenAddr: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, sendErr = ln.Accept(ctx)
	}()

	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		_, recvErr = Dial(ctx, ln.ln.Addr().String(), "xyz-pqrs-tuv")
	}()

	wg.Wait()

	if !errors.Is(sendErr, fserrors.ErrPeerAuthFailed) {
		t.Errorf("sender: got %v, want ErrPeerAuthFailed", sendErr)
	}
	if !errors.Is(recvErr, fserrors.ErrPeerAuthFailed) {
		t.Errorf("receiver: got %v, want ErrPeerAuthFailed", recvErr)
	}
}

// Malformed PAKE messages from an unauthenticated peer must fail auth (or
// report a version mismatch) and never panic. 'S' + 32 zero bytes crashed
// the old SPAKE2 library.
func TestParsePakeMsg(t *testing.T) {
	share := make([]byte, 32)
	cases := []struct {
		name string
		msg  []byte
		want error
	}{
		{"v2 spake2", append([]byte{'S'}, share...), fserrors.ErrIncompatibleVersion},
		{"future version", append([]byte{'C', 9}, share...), fserrors.ErrIncompatibleVersion},
		{"short", []byte{'C', 1, 1}, fserrors.ErrPeerAuthFailed},
		{"long", append([]byte{'C', 1, 0}, share...), fserrors.ErrPeerAuthFailed},
		{"bad suite", append([]byte{'X', 1}, share...), fserrors.ErrPeerAuthFailed},
	}
	for _, c := range cases {
		if _, err := parsePakeMsg(c.msg); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if got, err := parsePakeMsg(append([]byte{'C', 1}, share...)); err != nil || len(got) != 32 {
		t.Errorf("valid message rejected: %v", err)
	}
}

// FuzzParsePakeMsg feeds arbitrary peer bytes through the parser and the
// CPace finish, the full unauthenticated input surface.
func FuzzParsePakeMsg(f *testing.F) {
	f.Add(append([]byte{'S'}, make([]byte, 32)...))
	f.Add(append([]byte{'C', 1}, make([]byte, 32)...))
	f.Fuzz(func(t *testing.T, msg []byte) {
		share, err := parsePakeMsg(msg)
		if err != nil {
			return
		}
		st, err := pake.New(pake.Initiator, pake.Params{PRS: []byte("abc-defg-hjk")})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = st.Finish(share, adReceiver)
	})
}
