//go:build race

package transfer

// raceEnabled: the race detector is on. sync.Pool then drops a share of
// what is put back, so allocation counts say nothing about the code.
const raceEnabled = true
