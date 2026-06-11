package code

import (
	"strings"
	"testing"
)

func TestSlot_ShapeAndDeterminism(t *testing.T) {
	s1 := Slot("abc-defg-jkm")
	s2 := Slot("abc-defg-jkm")
	if s1 != s2 {
		t.Fatalf("Slot is not deterministic: %q vs %q", s1, s2)
	}
	if err := ValidateSlot(s1); err != nil {
		t.Fatalf("Slot output %q fails its own validation: %v", s1, err)
	}
	if len(s1) != SlotLen {
		t.Fatalf("len(slot) = %d, want %d", len(s1), SlotLen)
	}
	// The memoization must not leak a stale value across codes.
	other := Slot("zzz-zzzz-zzz")
	if other == s1 {
		t.Fatal("two different codes produced the same slot")
	}
	if again := Slot("abc-defg-jkm"); again != s1 {
		t.Fatalf("re-derivation after a different code changed the slot: %q vs %q", again, s1)
	}
}

// The slot must not contain the code (trivially true for hex output,
// but this is the load-bearing privacy property — keep it pinned).
func TestSlot_DoesNotEmbedCode(t *testing.T) {
	const c = "abc-defg-jkm"
	s := Slot(c)
	if strings.Contains(s, c) || strings.Contains(s, strings.ReplaceAll(c, "-", "")) {
		t.Fatalf("slot %q embeds the code", s)
	}
}

func TestValidateSlot(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"0123456789abcdef0123456789abcdef", true},
		{"", false},
		{"abc-defg-jkm", false}, // a raw code is not a slot
		{"0123456789abcdef0123456789abcde", false},   // too short
		{"0123456789abcdef0123456789abcdef0", false}, // too long
		{"0123456789ABCDEF0123456789ABCDEF", false},  // uppercase
		{"0123456789abcdeg0123456789abcdef", false},  // non-hex
	}
	for _, c := range cases {
		err := ValidateSlot(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ValidateSlot(%q) = %v, want ok=%v", c.in, err, c.ok)
		}
	}
}
