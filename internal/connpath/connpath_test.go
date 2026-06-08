package connpath

import "testing"

func TestFromICE_Classification(t *testing.T) {
	cases := []struct {
		name   string
		local  string
		remote string
		want   Kind
	}{
		// Spec rule: host ↔ host means peers reached each other on
		// interface addresses, so we claim Local.
		{"host_host_is_local", "host", "host", KindLocal},

		// Anything involving srflx / prflx means a NAT was crossed —
		// classified as DirectNAT (direct peer-to-peer over the internet).
		{"srflx_srflx_is_direct_nat", "srflx", "srflx", KindDirectNAT},
		{"srflx_host_is_direct_nat", "srflx", "host", KindDirectNAT},
		{"host_srflx_is_direct_nat", "host", "srflx", KindDirectNAT},
		{"prflx_host_is_direct_nat", "prflx", "host", KindDirectNAT},
		{"host_prflx_is_direct_nat", "host", "prflx", KindDirectNAT},
		{"prflx_prflx_is_direct_nat", "prflx", "prflx", KindDirectNAT},
		{"srflx_prflx_is_direct_nat", "srflx", "prflx", KindDirectNAT},

		// Relay anywhere means a relay candidate was selected — surface
		// it distinctly even though the controlling side may have a
		// direct candidate.
		{"relay_host_is_relay", "relay", "host", KindRelay},
		{"host_relay_is_relay", "host", "relay", KindRelay},
		{"relay_relay_is_relay", "relay", "relay", KindRelay},
		{"relay_srflx_is_relay", "relay", "srflx", KindRelay},

		// Unknown / empty inputs fall through to DirectNAT as the
		// conservative choice (don't overclaim "local").
		{"empty_pair_is_direct_nat", "", "", KindDirectNAT},
		{"unknown_value_is_direct_nat", "wat", "host", KindDirectNAT},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FromICE(c.local, c.remote)
			if got.Kind != c.want {
				t.Errorf("Kind = %v, want %v", got.Kind, c.want)
			}
			if got.LocalCand != c.local || got.RemoteCand != c.remote {
				t.Errorf("candidate types not preserved: got (%q,%q), want (%q,%q)",
					got.LocalCand, got.RemoteCand, c.local, c.remote)
			}
		})
	}
}

func TestFromLAN(t *testing.T) {
	got := FromLAN()
	if got.Kind != KindLocal {
		t.Errorf("Kind = %v, want %v", got.Kind, KindLocal)
	}
	if got.LocalCand != "" || got.RemoteCand != "" {
		t.Errorf("LAN path should not carry candidate types: got (%q,%q)", got.LocalCand, got.RemoteCand)
	}
	if got.Detail() != "" {
		t.Errorf("Detail() = %q, want empty for LAN path", got.Detail())
	}
}

func TestFromRelay(t *testing.T) {
	got := FromRelay("fsend.alzina.dev:443")
	if got.Kind != KindRelay {
		t.Errorf("Kind = %v, want %v", got.Kind, KindRelay)
	}
	if got.RelayAddr != "fsend.alzina.dev:443" {
		t.Errorf("RelayAddr = %q, want fsend.alzina.dev:443", got.RelayAddr)
	}
	// Headline collapses to the compact Tag() form; the address still
	// appears so operators can see which relay.
	wantHead := "Relayed via fsend.alzina.dev:443"
	if got.Headline() != wantHead {
		t.Errorf("Headline() = %q, want %q", got.Headline(), wantHead)
	}
	if got.Tag() != wantHead {
		t.Errorf("Tag() = %q, want %q", got.Tag(), wantHead)
	}
}

func TestTag_CompactForms(t *testing.T) {
	if got := (Info{Kind: KindLocal}).Tag(); got != "Direct on local network" {
		t.Errorf("Local Tag() = %q, want %q", got, "Direct on local network")
	}
	if got := (Info{Kind: KindDirectNAT}).Tag(); got != "Direct over the internet" {
		t.Errorf("DirectNAT Tag() = %q, want %q", got, "Direct over the internet")
	}
	if got := (Info{Kind: KindRelay}).Tag(); got != "Relayed" {
		t.Errorf("Relay (no addr) Tag() = %q, want %q", got, "Relayed")
	}
}

func TestGlyph(t *testing.T) {
	// Direct paths get a check; relay gets a warning glyph (per spec:
	// relay is honest disclosure, not an error, but it warrants a flag).
	if utf8, _ := (Info{Kind: KindLocal}).Glyph(); utf8 != "✓" {
		t.Errorf("Local glyph = %q, want ✓", utf8)
	}
	if utf8, _ := (Info{Kind: KindDirectNAT}).Glyph(); utf8 != "✓" {
		t.Errorf("DirectNAT glyph = %q, want ✓", utf8)
	}
	if utf8, ascii := (Info{Kind: KindRelay}).Glyph(); utf8 != "⚠" || ascii != "[!]" {
		t.Errorf("Relay glyph = (%q,%q), want (⚠, [!])", utf8, ascii)
	}
}

func TestDetail(t *testing.T) {
	if d := FromICE("srflx", "host").Detail(); d != "srflx → host" {
		t.Errorf("Detail() = %q, want %q", d, "srflx → host")
	}
	if d := FromLAN().Detail(); d != "" {
		t.Errorf("LAN Detail() = %q, want empty", d)
	}
	if d := FromRelay("x:1").Detail(); d != "" {
		t.Errorf("Relay Detail() = %q, want empty", d)
	}
}
