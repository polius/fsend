package main

import (
	"bufio"
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

// The sender prompt must take typed input verbatim past the line
// terminator: every other password path (hidden read, --password=VALUE,
// FSEND_PASSWORD) preserves leading/trailing spaces, so trimming here
// made a pasted "secret " silently diverge into a wrong-password loop.
func TestPromptPasswordWithSuggestion_VerbatimInput(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "hunter2\n", "hunter2"},
		{"trailing_space_kept", "hunter2 \n", "hunter2 "},
		{"leading_space_kept", " hunter2\n", " hunter2"},
		{"crlf_trimmed", "hunter2\r\n", "hunter2"},
		{"spaces_only_is_a_password", "  \n", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pw string
			var err error
			captureStderr(t, func() {
				pw, err = promptPasswordWithSuggestion(bufio.NewReader(strings.NewReader(tc.in)))
			})
			if err != nil {
				t.Fatalf("promptPasswordWithSuggestion(%q): %v", tc.in, err)
			}
			if pw != tc.want {
				t.Errorf("password = %q, want %q", pw, tc.want)
			}
		})
	}
}

// Bare <enter> still accepts the printed suggestion.
func TestPromptPasswordWithSuggestion_EnterAcceptsSuggestion(t *testing.T) {
	var pw string
	var err error
	stderr := captureStderr(t, func() {
		pw, err = promptPasswordWithSuggestion(bufio.NewReader(strings.NewReader("\n")))
	})
	if err != nil {
		t.Fatal(err)
	}
	if pw == "" || !strings.Contains(stderr, pw) {
		t.Errorf("bare Enter must return the suggestion shown on stderr; got %q", pw)
	}
}
