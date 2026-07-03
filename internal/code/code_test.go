package code

import (
	"strings"
	"testing"
	"unicode"
)

func TestGenerate_FormatAndAlphabet(t *testing.T) {
	for i := 0; i < 1000; i++ {
		c, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if err := Validate(c); err != nil {
			t.Fatalf("Generate produced invalid code %q: %v", c, err)
		}
		// Confirm format xxx-xxxx-xxx.
		parts := strings.Split(c, "-")
		if len(parts) != 3 {
			t.Fatalf("expected 3 hyphen-separated parts, got %d in %q", len(parts), c)
		}
		if len(parts[0]) != 3 || len(parts[1]) != 4 || len(parts[2]) != 3 {
			t.Fatalf("expected 3-4-3 layout, got %d-%d-%d in %q", len(parts[0]), len(parts[1]), len(parts[2]), c)
		}
		// Confirm only allowed letters.
		for _, r := range strings.ReplaceAll(c, "-", "") {
			if !unicode.IsLower(r) {
				t.Fatalf("non-lowercase rune %q in %q", r, c)
			}
			if r == 'i' || r == 'l' || r == 'o' {
				t.Fatalf("forbidden letter %q in %q", r, c)
			}
		}
	}
}

func TestGenerate_Uniqueness(t *testing.T) {
	// Not a security test — just a quick sanity check that we're getting
	// actual randomness and not the same code every time.
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		c, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if seen[c] {
			t.Fatalf("duplicate code generated within 100 iterations: %q", c)
		}
		seen[c] = true
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		in   string
		want error
	}{
		{"abc-defg-hjk", nil},
		{"aaa-bbbb-ccc", nil},
		{"zzz-zzzz-zzz", nil},

		// Bad alphabet
		{"abc-iefg-hjk", ErrInvalid}, // contains 'i'
		{"abc-lefg-hjk", ErrInvalid}, // contains 'l'
		{"abc-oefg-hjk", ErrInvalid}, // contains 'o'
		{"abc-defg-h1k", ErrInvalid}, // digit
		{"abc-defg-h k", ErrInvalid}, // space
		{"ABC-DEFG-HJK", ErrInvalid}, // uppercase
		{"abc-def-hjkm", ErrInvalid}, // wrong layout

		// Bad shape
		{"", ErrInvalid},
		{"abc", ErrInvalid},
		{"abcdefghjk", ErrInvalid},
		{"abc-defghjk", ErrInvalid},
		{"abc--defg-hjk", ErrInvalid},
		{"-abc-defg-hjk", ErrInvalid},
		{"abc-defg-hjk-", ErrInvalid},
		{"abc-defg-hjkm-extra", ErrInvalid},

		// Looks code-like but isn't
		{"report.pdf", ErrInvalid},
		{"7-banana-staple-river", ErrInvalid}, // croc-style, intentionally rejected
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := Validate(tt.in)
			if got != tt.want {
				t.Errorf("Validate(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsCode(t *testing.T) {
	if !IsCode("abc-defg-hjk") {
		t.Error("IsCode should return true for valid code")
	}
	if IsCode("report.pdf") {
		t.Error("IsCode should return false for filename")
	}
}

func TestLooksLikeCode(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		// Plausible typos of a real code.
		{"abc-defg-jk", true},   // dropped last letter
		{"abc-defg-jkmm", true}, // doubled letter
		{"abcd-efg-jkm", true},  // hyphen in the wrong place
		{"abcdefg-jkm", true},   // dropped hyphen
		{"abc-def0-jkm", true},  // digit for letter (0 ↔ o confusion)
		{"Abc-Defg-Jkm", true},  // chat-app capitalization
		{"abc-defg-jkm", true},  // exact code shape counts too
		{"abc-defg", true},      // whole group dropped (verbal relay)
		{"my-file", true},       // in-window false positive — costs only a hint line

		// Things that are clearly not codes.
		{"report.pdf", false},                  // extension
		{"a-b", false},                         // too short even for a dropped group
		{"my-favorite-very-long-notes", false}, // too long
		{"abc defg jkm", false},                // spaces, not hyphens
		{"abc-defg-", false},                   // trailing hyphen
		{"-abc-defg", false},                   // leading hyphen
		{"dir/abc-defg-jkm", false},            // path separator
		{"", false},
	}
	for _, tt := range tests {
		if got := LooksLikeCode(tt.in); got != tt.want {
			t.Errorf("LooksLikeCode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestAlphabet_NoConfusables(t *testing.T) {
	// Defensive: confirms the alphabet constant matches the regex.
	for _, r := range Alphabet {
		if r == 'i' || r == 'l' || r == 'o' {
			t.Fatalf("Alphabet should not contain %q", r)
		}
		if !Pattern.MatchString(strings.Repeat(string(r), 3) + "-" +
			strings.Repeat(string(r), 4) + "-" + strings.Repeat(string(r), 3)) {
			t.Fatalf("Pattern should accept letter %q from Alphabet", r)
		}
	}
}
