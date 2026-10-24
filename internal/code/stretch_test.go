package code

import (
	"bytes"
	"testing"
)

// The slot and the mDNS name come from one stretch but must stay
// unlinkable: different labels give unrelated bytes.
func TestStretch_LabelsSeparate(t *testing.T) {
	a := Stretch("abc-defg-jkm", 20000, "fsend-slot-v3", 16)
	b := Stretch("abc-defg-jkm", 20000, "fsend-landisc-v3", 16)
	if bytes.Equal(a, b) {
		t.Fatal("two labels produced the same bytes")
	}
	if again := Stretch("abc-defg-jkm", 20000, "fsend-slot-v3", 16); !bytes.Equal(a, again) {
		t.Fatal("Stretch is not deterministic")
	}
}

// The memo is bounded and never returns a stale value after eviction.
func TestStretch_CacheBounded(t *testing.T) {
	first := Stretch("abc-defg-jkm", 1, "x", 8)
	for e := int64(2); e < stretchCacheMax+3; e++ {
		Stretch("abc-defg-jkm", e, "x", 8)
	}
	if len(stretchCache) > stretchCacheMax {
		t.Fatalf("cache grew to %d entries", len(stretchCache))
	}
	if again := Stretch("abc-defg-jkm", 1, "x", 8); !bytes.Equal(first, again) {
		t.Fatal("re-derivation after eviction changed the value")
	}
}
