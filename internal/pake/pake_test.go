package pake

import (
	"bytes"
	"testing"
)

func TestSpake2_MatchingCodesAgree(t *testing.T) {
	a := New("abc-defg-hjk")
	b := New("abc-defg-hjk")

	am, bm := a.Start(), b.Start()

	aKey, err := a.Finish(bm)
	if err != nil {
		t.Fatalf("a.Finish: %v", err)
	}
	bKey, err := b.Finish(am)
	if err != nil {
		t.Fatalf("b.Finish: %v", err)
	}

	if !bytes.Equal(aKey, bKey) {
		t.Fatalf("keys differ\na: %x\nb: %x", aKey, bKey)
	}
	if len(aKey) != KeySize {
		t.Fatalf("key length %d, want %d", len(aKey), KeySize)
	}
}

// Wrong-code peers must derive different keys. SPAKE2 Finish does not
// fail on wrong password by itself — the divergence is what the
// channel-binding step in quicconn turns into a hard reject.
func TestSpake2_WrongCodesDiverge(t *testing.T) {
	a := New("abc-defg-hjk")
	b := New("xyz-pqrs-tuv")

	am, bm := a.Start(), b.Start()

	aKey, errA := a.Finish(bm)
	bKey, errB := b.Finish(am)

	if errA == nil && errB == nil && bytes.Equal(aKey, bKey) {
		t.Fatal("wrong-code peers derived identical keys")
	}
}
