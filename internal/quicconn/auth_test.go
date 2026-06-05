package quicconn

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
)

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
		_, recvErr = Dial(ctx, ln.LocalAddr().String(), "xyz-pqrs-tuv")
	}()

	wg.Wait()

	if !errors.Is(sendErr, fserrors.ErrPeerAuthFailed) {
		t.Errorf("sender: got %v, want ErrPeerAuthFailed", sendErr)
	}
	if !errors.Is(recvErr, fserrors.ErrPeerAuthFailed) {
		t.Errorf("receiver: got %v, want ErrPeerAuthFailed", recvErr)
	}
}
