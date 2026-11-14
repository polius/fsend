// Package conv converts between the integer types byte counts travel in.
// Sizes and offsets are uint64 on the wire and int64 in the os and io APIs;
// these conversions saturate instead of wrapping, so a value out of range
// can't turn negative (or huge) on the way.
package conv

import "math"

// Int64 returns u as an int64, saturating at math.MaxInt64.
func Int64(u uint64) int64 {
	if u > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(u)
}

// Uint64 returns i as a uint64, or 0 for a negative i. OS file sizes are
// never negative; the floor only keeps the conversion total.
func Uint64(i int64) uint64 {
	if i < 0 {
		return 0
	}
	return uint64(i)
}
