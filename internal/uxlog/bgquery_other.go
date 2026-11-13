//go:build !(darwin || freebsd || openbsd || netbsd || dragonfly || linux)

package uxlog

// queryBackground has no portable terminal query here: the background stays
// unknown and output uses the terminal's own ANSI colours.
func queryBackground() theme { return themeUnknown }
