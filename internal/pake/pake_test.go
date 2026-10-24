package pake

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Vectors from draft-irtf-cfrg-cpace-21, appendix B.1.
var (
	vecPRS = []byte("Password")
	vecCI  = []byte("\x0bA_initiator\x0bB_responder")
	vecSID = "7e4b4791d6a8ef019b936c79fb7f2c57"
	vecYa  = "21b4f4bd9e64ed355c3eb676a28ebedaf6d8f17bdc365995b319097153044080"
	vecYb  = "848b0779ff415f0af4ea14df9dd1d3c29ac41d836c7808896c4eba19c51ac40a"
)

func TestVectorsLEB128AndLvCat(t *testing.T) {
	if got := hex.EncodeToString(prependLen(nil)); got != "00" {
		t.Fatalf("prepend_len(\"\") = %s", got)
	}
	r128 := make([]byte, 128)
	for i := range r128 {
		r128[i] = byte(i)
	}
	if got := prependLen(r128); got[0] != 0x80 || got[1] != 0x01 || len(got) != 130 {
		t.Fatalf("prepend_len(range(128)) header = %x", got[:2])
	}
	got := hex.EncodeToString(lvCat([]byte("1234"), []byte("5"), nil, []byte("678")))
	if got != "043132333401350003363738" {
		t.Fatalf("lv_cat = %s", got)
	}
	got = hex.EncodeToString(transcriptIR([]byte("123"), []byte("PartyA"), []byte("234"), []byte("PartyB")))
	if got != "03313233065061727479410332333406506172747942" {
		t.Fatalf("transcript_ir = %s", got)
	}
}

func TestVectorsGenerator(t *testing.T) {
	sid := unhex(t, vecSID)
	gs := generatorString(vecPRS, vecCI, sid)
	if len(gs) != 170 {
		t.Fatalf("generator string length %d, want 170", len(gs))
	}
	g := calculateGenerator(vecPRS, vecCI, sid)
	want := "d04bf6d41f6a289632a2e929fa29bebd51092512a7829fdde7d314b62f05a73f"
	if hex.EncodeToString(g) != want {
		t.Fatalf("generator = %x\nwant        %s", g, want)
	}
}

func TestVectorsFullRun(t *testing.T) {
	sid := unhex(t, vecSID)
	a, err := newWithScalar(Initiator, Params{PRS: vecPRS, CI: vecCI, SID: sid, AD: []byte("ADa")}, unhex(t, vecYa))
	if err != nil {
		t.Fatal(err)
	}
	b, err := newWithScalar(Responder, Params{PRS: vecPRS, CI: vecCI, SID: sid, AD: []byte("ADb")}, unhex(t, vecYb))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(a.Share()); got != "1d13c89278cdadd826f6d8d7f887701430f8380ddc17611cdd6dc989ce0c9f32" {
		t.Fatalf("Ya = %s", got)
	}
	if got := hex.EncodeToString(b.Share()); got != "248cccf6d5cdc3646f0ad593f9e6cef4e69d4945f8372e623512ecea32185623" {
		t.Fatalf("Yb = %s", got)
	}
	iskA, err := a.Finish(b.Share(), []byte("ADb"))
	if err != nil {
		t.Fatal(err)
	}
	iskB, err := b.Finish(a.Share(), []byte("ADa"))
	if err != nil {
		t.Fatal(err)
	}
	want := "6e19b875f7a561d6b3ca3dbb9ef42ac55de3e717881018204b8922b4d5e53bb2" +
		"aa82c300bea7b65d2b671da71922ddf6472301b79bc270adfa8bf413285f2263"
	if hex.EncodeToString(iskA) != want || hex.EncodeToString(iskB) != want {
		t.Fatalf("ISK mismatch\nA:    %x\nB:    %x\nwant: %s", iskA, iskB, want)
	}
}

// Appendix B.1.10: u0..u5 and u7 must abort; the others must produce the
// listed points (exercising bit-255 clearing and non-canonical inputs).
func TestVectorsLowOrder(t *testing.T) {
	s := unhex(t, "af46e36bf0527c9d3b16154b82465edd62144c0ac1fc5a18506a2244ba449aff")
	cases := []struct{ u, q string }{
		{"0000000000000000000000000000000000000000000000000000000000000000", ""},
		{"0100000000000000000000000000000000000000000000000000000000000000", ""},
		{"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", ""},
		{"e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800", ""},
		{"5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157", ""},
		{"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", ""},
		{"daffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "d8e2c776bbacd510d09fd9278b7edcd25fc5ae9adfba3b6e040e8d3b71b21806"},
		{"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", ""},
		{"dbffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "c85c655ebe8be44ba9c0ffde69f2fe10194458d137f09bbff725ce58803cdb38"},
		{"d9ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "db64dafa9b8fdd136914e61461935fe92aa372cb056314e1231bc4ec12417456"},
		{"cdeb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b880", "e062dcd5376d58297be2618c7498f55baa07d7e03184e8aada20bca28888bf7a"},
		{"4c9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f11d7", "993c6ad11c4c29da9a56f7691fd0ff8d732e49de6250b6c2e80003ff4629a175"},
	}
	for i, c := range cases {
		st, err := newWithScalar(Initiator, Params{PRS: vecPRS}, s)
		if err != nil {
			t.Fatal(err)
		}
		q, err := scalarMult(st.priv, unhex(t, c.u))
		if c.q == "" {
			if err == nil {
				t.Errorf("u%x: accepted low-order point (q=%x)", i, q)
			}
			if _, ferr := st.Finish(unhex(t, c.u), nil); ferr != ErrInvalidShare {
				t.Errorf("u%x: Finish err = %v, want ErrInvalidShare", i, ferr)
			}
			continue
		}
		if err != nil || hex.EncodeToString(q) != c.q {
			t.Errorf("u%x: q = %x, err %v; want %s", i, q, err, c.q)
		}
	}
}

func run(t *testing.T, prsA, prsB string) (ka, kb []byte) {
	t.Helper()
	sid := []byte("session-1")
	a, err := New(Initiator, Params{PRS: []byte(prsA), SID: sid, AD: []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Responder, Params{PRS: []byte(prsB), SID: sid, AD: []byte("b")})
	if err != nil {
		t.Fatal(err)
	}
	ka, errA := a.Finish(b.Share(), []byte("b"))
	kb, errB := b.Finish(a.Share(), []byte("a"))
	if errA != nil || errB != nil {
		t.Fatalf("finish: %v / %v", errA, errB)
	}
	return ka, kb
}

func TestMatchingSecretsAgree(t *testing.T) {
	ka, kb := run(t, "abc-defg-hjk", "abc-defg-hjk")
	if !bytes.Equal(ka, kb) || len(ka) != KeySize {
		t.Fatalf("keys differ\na: %x\nb: %x", ka, kb)
	}
}

func TestWrongSecretsDiverge(t *testing.T) {
	ka, kb := run(t, "abc-defg-hjk", "abc-defg-hjm")
	if bytes.Equal(ka, kb) {
		t.Fatal("wrong-secret peers derived identical keys")
	}
}

func TestMalformedSharesRejected(t *testing.T) {
	for _, share := range [][]byte{nil, {}, make([]byte, 31), make([]byte, 33), bytes.Repeat([]byte{0xff}, 64)} {
		st, err := New(Initiator, Params{PRS: []byte("x")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Finish(share, nil); err != ErrInvalidShare {
			t.Errorf("share len %d: err = %v, want ErrInvalidShare", len(share), err)
		}
	}
}

func TestFinishOnce(t *testing.T) {
	a, _ := New(Initiator, Params{PRS: []byte("x")})
	b, _ := New(Responder, Params{PRS: []byte("x")})
	if _, err := a.Finish(b.Share(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Finish(b.Share(), nil); err == nil {
		t.Fatal("second Finish succeeded")
	}
}

func FuzzFinish(f *testing.F) {
	f.Add(make([]byte, 32), []byte("ad"))
	f.Add(bytes.Repeat([]byte{0xff}, 32), []byte(nil))
	f.Fuzz(func(t *testing.T, share, ad []byte) {
		st, err := New(Initiator, Params{PRS: []byte("abc-defg-hjk")})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = st.Finish(share, ad) // must never panic
	})
}
