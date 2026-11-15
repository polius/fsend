//go:build !(darwin || freebsd || openbsd || netbsd || dragonfly || linux)

package uxlog

// queryBackground has no terminal query here: the background stays unknown
// and output uses the terminal's own ANSI colours (FSEND_THEME and
// COLORFGBG still pick a palette).
//
// Windows is in this group on purpose, although Windows Terminal answers
// OSC 11 since 1.22. The Unix query is safe because of one property: the
// terminal answers the OSC 11 query and the DA1 request that follows it in
// order, so the DA1 reply proves nothing more is coming and no byte of an
// answer is left for the shell to read as typing. On Windows that property
// depends on what sits between the program and the terminal. With a ConPTY
// that answers DA1 itself and forwards the colour query (an older Windows,
// VS Code's terminal, an SSH session), the colour answer can arrive after
// the DA1 reply, and would then be typed into the next prompt. Whether it
// does differs by Windows build and terminal, and can only be established
// on those terminals — not from a Unix pty, and not in CI, whose Windows
// runners have no console. Until it has been, no query: the wrong answer
// here is a slightly different orange, the wrong query is garbage in the
// user's prompt.
func queryBackground() theme { return themeUnknown }
