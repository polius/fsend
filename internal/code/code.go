// Package code handles the short pairing codes users share out of band.
//
// Format: xxx-xxxx-xxx (10 lowercase letters from a 23-letter alphabet,
// excluding i, l, o to avoid confusion with 1 and 0).
//
// Entropy: log2(23^10) ≈ 45 bits — comfortably more than enough for a
// PAKE-protected code, where the only feasible attack is online guessing
// against a rate-limited pairing server.
package code

import (
	"crypto/rand"
	"errors"
	"math/big"
	"regexp"
	"strings"
)

// Alphabet is the set of letters used in fsend codes.
//
// We exclude i, l, o because they look like 1, 1, 0 when read off a screen,
// dictated over a phone, or written on paper. Filesync uses the full 26-letter
// alphabet because it shows a QR code alongside; fsend is CLI-first, so users
// will actually type these — visual disambiguation matters more.
const Alphabet = "abcdefghjkmnpqrstuvwxyz"

// Length is the total number of letters in a code (excluding the hyphens).
const Length = 10

// Layout describes the hyphen positions: 3-4-3.
var Layout = [3]int{3, 4, 3}

// Pattern is the regex used to detect whether a CLI argument is a code.
//
// It matches the exact same alphabet and shape we generate. Strict matching
// is important: a permissive regex would conflict with real filenames.
var Pattern = regexp.MustCompile(`^[a-hjkmnp-z]{3}-[a-hjkmnp-z]{4}-[a-hjkmnp-z]{3}$`)

// ErrInvalid is returned when a string is not a syntactically valid code.
var ErrInvalid = errors.New("invalid code format")

// Generate returns a fresh random code in canonical format.
//
// Uses crypto/rand with rejection sampling (via crypto/rand.Int) so the
// distribution over the alphabet is uniform with no modulo bias.
func Generate() (string, error) {
	letters := make([]byte, Length)
	max := big.NewInt(int64(len(Alphabet)))
	for i := range letters {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		letters[i] = Alphabet[n.Int64()]
	}
	return format(letters), nil
}

// Validate returns nil if s is a syntactically valid code, ErrInvalid otherwise.
func Validate(s string) error {
	if !Pattern.MatchString(s) {
		return ErrInvalid
	}
	return nil
}

// IsCode reports whether s looks like a code (suitable for CLI dispatch
// where we decide whether an argument is a code or a path).
func IsCode(s string) bool {
	return Pattern.MatchString(s)
}

// looksLikeCodePattern is deliberately looser than Pattern: any-case
// alphanumeric groups joined by hyphens. Digits are included because the
// alphabet excludes i/l/o precisely for their 1/1/0 lookalikes — typing
// the digit is the expected transcription mistake.
var looksLikeCodePattern = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)+$`)

// LooksLikeCode reports whether s is shaped enough like a code that a
// mistyped code is the plausible explanation: hyphen-joined groups with
// roughly the right number of characters, even if the strict Pattern
// rejects it (wrong group sizes, dropped/extra letter, digit for letter).
// Used to attach a "was this a receive code?" hint to errors — false
// positives only cost a harmless extra hint line, so close is good enough.
func LooksLikeCode(s string) bool {
	if !looksLikeCodePattern.MatchString(s) {
		return false
	}
	n := len(s) - strings.Count(s, "-")
	return n >= Length-2 && n <= Length+2
}

// format takes a 10-byte buffer of code letters and inserts hyphens.
func format(letters []byte) string {
	var b strings.Builder
	b.Grow(Length + len(Layout) - 1)
	pos := 0
	for i, group := range Layout {
		if i > 0 {
			b.WriteByte('-')
		}
		b.Write(letters[pos : pos+group])
		pos += group
	}
	return b.String()
}
