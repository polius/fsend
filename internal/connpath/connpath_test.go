package connpath

import "testing"

func TestFromICE_Classification(t *testing.T) {
	cases := []struct {
		name      string
		local     string
		remote    string
		want      Kind
		wantShort string
	}{
		// Spec rule: host ↔ host means peers reached each other on
		// interface addresses, so we claim Local.
		{"host_host_is_local", "host", "host", KindLocal, "direct (local)"},

		// Anything involving srflx / prflx means a NAT was crossed.
		{"srflx_srflx_is_stun", "srflx", "srflx", KindDirectSTUN, "direct (STUN)"},
		{"srflx_host_is_stun", "srflx", "host", KindDirectSTUN, "direct (STUN)"},
		{"host_srflx_is_stun", "host", "srflx", KindDirectSTUN, "direct (STUN)"},
		{"prflx_host_is_stun", "prflx", "host", KindDirectSTUN, "direct (STUN)"},
		{"host_prflx_is_stun", "host", "prflx", KindDirectSTUN, "direct (STUN)"},
		{"prflx_prflx_is_stun", "prflx", "prflx", KindDirectSTUN, "direct (STUN)"},
		{"srflx_prflx_is_stun", "srflx", "prflx", KindDirectSTUN, "direct (STUN)"},

		// Relay anywhere means a relay candidate was selected — surface
		// it distinctly even though the controlling side may have a
		// direct candidate.
		{"relay_host_is_relay", "relay", "host", KindRelay, "relay (TURN)"},
		{"host_relay_is_relay", "host", "relay", KindRelay, "relay (TURN)"},
		{"relay_relay_is_relay", "relay", "relay", KindRelay, "relay (TURN)"},
		{"relay_srflx_is_relay", "relay", "srflx", KindRelay, "relay (TURN)"},

		// Unknown / empty inputs fall through to DirectSTUN as the
		// conservative choice (don't overclaim "local").
		{"empty_pair_is_stun", "", "", KindDirectSTUN, "direct (STUN)"},
		{"unknown_value_is_stun", "wat", "host", KindDirectSTUN, "direct (STUN)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FromICE(c.local, c.remote)
			if got.Kind != c.want {
				t.Errorf("Kind = %v, want %v", got.Kind, c.want)
			}
			if got.Short() != c.wantShort {
				t.Errorf("Short() = %q, want %q", got.Short(), c.wantShort)
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
	if got.Short() != "direct (local)" {
		t.Errorf("Short() = %q, want %q", got.Short(), "direct (local)")
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
	if got.Short() != "relay (TURN)" {
		t.Errorf("Short() = %q", got.Short())
	}
	// Headline includes the address so operators can see which relay.
	wantHead := "relay (TURN) via fsend.alzina.dev:443 — NAT hole-punch failed"
	if got.Headline() != wantHead {
		t.Errorf("Headline() = %q, want %q", got.Headline(), wantHead)
	}
}

func TestGlyph(t *testing.T) {
	// Direct paths get a check; relay gets a warning glyph (per spec:
	// relay is honest disclosure, not an error, but it warrants a flag).
	if utf8, _ := (Info{Kind: KindLocal}).Glyph(); utf8 != "✓" {
		t.Errorf("Local glyph = %q, want ✓", utf8)
	}
	if utf8, _ := (Info{Kind: KindDirectSTUN}).Glyph(); utf8 != "✓" {
		t.Errorf("DirectSTUN glyph = %q, want ✓", utf8)
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
