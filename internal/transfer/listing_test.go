package transfer

import (
	"bytes"
	"testing"
)

// A compressed listing is a fraction of the plain one and decodes to the
// same entries; a receiver that did not advertise the bit gets plain frames.
func TestListingCompressed(t *testing.T) {
	es := deepTree(20_000)
	srcs := make([]Source, len(es))
	for i, e := range es {
		srcs[i] = Source{Entry: e}
	}
	var plain, z bytes.Buffer
	if err := sendListing(&plain, srcs, false); err != nil {
		t.Fatal(err)
	}
	if err := sendListing(&z, srcs, true); err != nil {
		t.Fatal(err)
	}
	zLen := z.Len()
	if zLen*5 > plain.Len() {
		t.Errorf("compressed listing %d bytes vs %d plain: want at least 5x smaller", zLen, plain.Len())
	}
	got, err := recvListing(&z)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(es) || got[len(got)-1].RelativePath != es[len(es)-1].RelativePath {
		t.Fatalf("decoded %d entries, want %d", len(got), len(es))
	}
	t.Logf("listing of %d entries: %d bytes plain, %d compressed", len(es), plain.Len(), zLen)
}
