package main

import (
	"bufio"
	"strings"
	"testing"
)

// The word list must be big enough for the entropy the comment claims
// (512 words → 9 bits each), unique, and easy to type: lowercase a–z.
func TestPassphraseList(t *testing.T) {
	words := strings.Fields(passphraseList)
	if len(words) < 512 {
		t.Errorf("passphraseList has %d words, want at least 512", len(words))
	}
	seen := map[string]bool{}
	for _, w := range words {
		if seen[w] {
			t.Errorf("duplicate word %q", w)
		}
		seen[w] = true
		if strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") != "" || len(w) < 2 || len(w) > 8 {
			t.Errorf("word %q is not a short lowercase word", w)
		}
	}
}

func TestGeneratePassphrase(t *testing.T) {
	a, err := generatePassphrase(passphraseWords)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(a, "-")); n != passphraseWords {
		t.Errorf("%q has %d words, want %d", a, n, passphraseWords)
	}
	// Two draws colliding at 36 bits means a broken source (a fixed seed).
	b, _ := generatePassphrase(passphraseWords)
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
