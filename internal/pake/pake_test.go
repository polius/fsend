package pake

import (
	"bytes"
	"testing"
)

func TestSpake2_SuccessfulHandshake(t *testing.T) {
	const code = "abc-defg-hjk"
	const sid = "01HG7P3M9XKN"

	alice := New(code, sid)
	bob := New(code, sid)

	aMsg, err := alice.Start()
	if err != nil {
		t.Fatalf("alice.Start: %v", err)
	}
	bMsg, err := bob.Start()
	if err != nil {
		t.Fatalf("bob.Start: %v", err)
	}

	aKey, err := alice.Finish(bMsg)
	if err != nil {
		t.Fatalf("alice.Finish: %v", err)
	}
	bKey, err := bob.Finish(aMsg)
	if err != nil {
		t.Fatalf("bob.Finish: %v", err)
	}

	if !bytes.Equal(aKey, bKey) {
		t.Errorf("derived keys differ:\nalice: %x\nbob:   %x", aKey, bKey)
	}
	if len(aKey) != KeySize {
		t.Errorf("derived key length %d, expected %d", len(aKey), KeySize)
	}
}

func TestSpake2_WrongCodeYieldsDifferentKeys(t *testing.T) {
	const sid = "01HG7P3M9XKN"

	alice := New("abc-defg-hjk", sid)
	bob := New("xyz-pqrs-tuv", sid)

	aMsg, _ := alice.Start()
	bMsg, _ := bob.Start()

	aKey, errA := alice.Finish(bMsg)
	bKey, errB := bob.Finish(aMsg)

	// Both Finishes may succeed (SPAKE2 only fails on bad encoding, not on
	// wrong-password — the verifier is the derived key disagreement),
	// but the keys MUST differ. That divergence is what the channel-binding
	// step turns into a TLS handshake failure.
	if errA != nil && errB != nil {
		// At least one side failed, fine.
		return
	}
	if errA == nil && errB == nil && bytes.Equal(aKey, bKey) {
		t.Error("wrong codes produced matching keys — PAKE is broken")
	}
}

func TestSpake2_DifferentSessionIDsYieldDifferentKeys(t *testing.T) {
	const code = "abc-defg-hjk"

	alice := New(code, "session-one")
	bob := New(code, "session-two")

	aMsg, _ := alice.Start()
	bMsg, _ := bob.Start()

	aKey, _ := alice.Finish(bMsg)
	bKey, _ := bob.Finish(aMsg)

	if bytes.Equal(aKey, bKey) {
		t.Error("different session IDs produced matching keys — replay protection broken")
	}
}

func TestSpake2_StartTwiceFails(t *testing.T) {
	p := New("abc-defg-hjk", "sid")
	if _, err := p.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Start(); err == nil {
		t.Error("expected error on second Start")
	}
}

func TestSpake2_FinishBeforeStartFails(t *testing.T) {
	p := New("abc-defg-hjk", "sid")
	if _, err := p.Finish([]byte("not a real message")); err == nil {
		t.Error("expected error on Finish before Start")
	}
}

func TestSpake2_FinishTwiceFails(t *testing.T) {
	alice := New("abc-defg-hjk", "sid")
	bob := New("abc-defg-hjk", "sid")
	_, _ = alice.Start()
	bMsg, _ := bob.Start()
	if _, err := alice.Finish(bMsg); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Finish(bMsg); err == nil {
		t.Error("expected error on second Finish")
	}
}
