package main

import (
	"strings"
	"testing"
)

// passwordAlphabet mirrors the alphabet inside generateRandomPassword.
// Duplicated rather than exported so the production constant stays
// scoped to the function that needs it.
const passwordAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"

func TestGenerateRandomPassword_LengthAndAlphabet(t *testing.T) {
	pw, err := generateRandomPassword(16)
	if err != nil {
		t.Fatalf("generateRandomPassword: %v", err)
	}
	if len(pw) != 16 {
		t.Errorf("len = %d, want 16", len(pw))
	}
	for _, r := range pw {
		if !strings.ContainsRune(passwordAlphabet, r) {
			t.Errorf("character %q not in safe alphabet (%s)", r, passwordAlphabet)
		}
	}
}

// Distinctness: two consecutive draws should not collide. With a
// 53-letter alphabet at length 16 the collision probability is
// effectively zero, so a single collision is a real bug (e.g. a fixed
// seed slipped in).
func TestGenerateRandomPassword_DistinctDraws(t *testing.T) {
	a, err := generateRandomPassword(16)
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateRandomPassword(16)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two random draws collided: %q == %q", a, b)
	}
}
