package conv

import (
	"math"
	"testing"
)

func TestInt64(t *testing.T) {
	for in, want := range map[uint64]int64{0: 0, 42: 42, math.MaxInt64: math.MaxInt64, math.MaxInt64 + 1: math.MaxInt64, math.MaxUint64: math.MaxInt64} {
		if got := Int64(in); got != want {
			t.Errorf("Int64(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestUint64(t *testing.T) {
	for in, want := range map[int64]uint64{0: 0, 42: 42, math.MaxInt64: math.MaxInt64, -1: 0, math.MinInt64: 0} {
		if got := Uint64(in); got != want {
			t.Errorf("Uint64(%d) = %d, want %d", in, got, want)
		}
	}
}
