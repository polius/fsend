package transfer

import "testing"

// windowsPathHazard is the pure predicate behind the Windows-receiver gate in
// SanitizeRelativePath. Tested directly (not through SanitizeRelativePath) so
// the Windows-only logic is exercised on every OS, including the darwin/linux
// dev and CI runners where runtime.GOOS != "windows".
func TestWindowsPathHazard(t *testing.T) {
	unsafe := []string{
		"notes:secret",   // alternate data stream
		"file.txt:$DATA", // ADS variant
		"CON", "con",     // reserved, any case
		"NUL.txt",      // reserved + extension still resolves to the device
		"COM1", "LPT9", // reserved serial/printer devices
		"aux.c",   // reserved stem with a legit-looking extension
		"report.", // trailing dot (stripped by Windows → collision)
		"report ", // trailing space (stripped by Windows)
	}
	for _, c := range unsafe {
		if windowsPathHazard(c) == "" {
			t.Errorf("windowsPathHazard(%q) = safe, want a hazard", c)
		}
	}

	safe := []string{
		"report.pdf", "my-notes.txt", "föö", "a_b.c",
		"CONtext.md", // only a prefix of a reserved name — fine
		"COM10",      // COM1-9 only; COM10 isn't reserved
		"scores.csv",
	}
	for _, c := range safe {
		if r := windowsPathHazard(c); r != "" {
			t.Errorf("windowsPathHazard(%q) = %q, want safe", c, r)
		}
	}
}

// The Windows hazards must be rejected only when the receiver is Windows —
// the same names are legitimate on Unix and must pass there. Driven with the
// OS injected so both branches run on any host.
func TestSanitizeRelativePath_WindowsGate(t *testing.T) {
	windowsUnsafe := []string{"notes:secret", "CON", "nul.txt", "COM1", "a/aux.c/b", "report.", "report "}
	for _, p := range windowsUnsafe {
		if _, err := sanitizeRelativePath(p, "windows"); err == nil {
			t.Errorf("sanitizeRelativePath(%q, windows) accepted, want rejected", p)
		}
		if _, err := sanitizeRelativePath(p, "linux"); err != nil {
			t.Errorf("sanitizeRelativePath(%q, linux) rejected (%v), want accepted", p, err)
		}
	}

	// Traversal/absolute rejections are OS-independent — still blocked on Unix.
	for _, p := range []string{"../escape", "/abs", `C:\x`} {
		if _, err := sanitizeRelativePath(p, "linux"); err == nil {
			t.Errorf("sanitizeRelativePath(%q, linux) accepted, want rejected", p)
		}
	}
}
