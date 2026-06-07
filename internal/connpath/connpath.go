// Package connpath classifies how the QUIC data path between two fsend
// peers was actually established and renders that classification for the
// user-facing UX layer.
//
// The classification is tri-state:
//
//   - Local       — peers are on the same LAN. Either mDNS discovery
//     short-circuited the pairing-server path entirely, or ICE
//     selected a host↔host candidate pair (no NAT crossed).
//   - DirectSTUN  — ICE hole-punched through one or both NATs. At least
//     one side of the selected pair is a server-reflexive
//     (srflx) or peer-reflexive (prflx) candidate.
//   - Relay       — ICE failed; QUIC is tunneled through the pairing
//     server's UDP relay.
//
// The classifier does not import pion/ice. Call sites that already hold
// pion candidate types pass them in as plain strings ("host", "srflx",
// "prflx", "relay") — keeping this package usable from tests and from any
// future non-pion transport.
package connpath

import "fmt"

// Kind is the three-way classification of an established data path.
type Kind int

const (
	// KindUnknown is the zero value; never displayed.
	KindUnknown Kind = iota
	// KindLocal — same broadcast domain. No NAT, no relay.
	KindLocal
	// KindDirectSTUN — direct P2P, but hole-punched through NAT.
	KindDirectSTUN
	// KindRelay — relayed via the pairing server's UDP listener.
	KindRelay
)

// Info bundles everything the UX layer needs to render the path line.
//
// LocalCand / RemoteCand are the lowercase ICE candidate type names
// ("host", "srflx", "prflx", "relay") and are only populated when the
// path was established via ICE (KindLocal-via-ICE or KindDirectSTUN).
// For KindLocal-via-mDNS they are empty; for KindRelay they are empty.
//
// RelayAddr is the host:port of the relay server and is only populated
// when Kind == KindRelay.
type Info struct {
	Kind       Kind
	LocalCand  string
	RemoteCand string
	RelayAddr  string
}

// FromLAN builds an Info for the mDNS-discovered LAN path.
func FromLAN() Info {
	return Info{Kind: KindLocal}
}

// FromRelay builds an Info for the relay-fallback path. The address is
// shown in the user-facing string so operators can confirm which relay
// they ended up on.
func FromRelay(relayAddr string) Info {
	return Info{Kind: KindRelay, RelayAddr: relayAddr}
}

// FromICE classifies an ICE selected candidate pair into Local vs
// DirectSTUN vs Relay.
//
// Rules:
//   - relay anywhere     → KindRelay (a TURN-style allocation; not our
//     server-side relay path, but kept distinct from
//     a "direct" claim).
//   - both host          → KindLocal (peers reached each other on
//     interface addresses; no NAT was crossed).
//   - anything else      → KindDirectSTUN (at least one srflx/prflx,
//     meaning a NAT was punched through).
//
// Unknown / empty inputs are conservatively classified as KindDirectSTUN
// rather than KindLocal — we only claim "Local" when we're sure.
func FromICE(localType, remoteType string) Info {
	info := Info{LocalCand: localType, RemoteCand: remoteType}
	switch {
	case localType == "relay" || remoteType == "relay":
		info.Kind = KindRelay
	case localType == "host" && remoteType == "host":
		info.Kind = KindLocal
	default:
		info.Kind = KindDirectSTUN
	}
	return info
}

// Short returns the headline label shown on the main status line, e.g.
// "direct (local)". Matches the spec's user-visible vocabulary.
func (i Info) Short() string {
	switch i.Kind {
	case KindLocal:
		return "direct (local)"
	case KindDirectSTUN:
		return "direct (STUN)"
	case KindRelay:
		return "relay (TURN)"
	default:
		return "unknown"
	}
}

// Tag returns the compact path label used in summary lines and inline
// chips, e.g. "Direct on LAN", "Direct via STUN", "Relayed via X". This
// is the short form — Headline() retains the verbose explainer form for
// situations where the long line is desired.
func (i Info) Tag() string {
	switch i.Kind {
	case KindLocal:
		return "Direct on LAN"
	case KindDirectSTUN:
		return "Direct via STUN"
	case KindRelay:
		if i.RelayAddr != "" {
			return "Relayed via " + i.RelayAddr
		}
		return "Relayed"
	default:
		return "unknown"
	}
}

// Glyph returns the UX glyph for this path: ✓ for direct paths, ⚠ for
// relay (relay is not a failure, but it is slower and worth flagging).
// Callers should pair with the ASCII fallback via uxlog.marker.
func (i Info) Glyph() (utf8, ascii string) {
	if i.Kind == KindRelay {
		return "⚠", "[!]"
	}
	return "✓", "[OK]"
}

// Headline is the single line the CLI prints right after the data path
// is established, e.g.
//
//	✓ Direct on LAN
//	✓ Direct via STUN
//	⚠ Relayed via fsend.alzina.dev
//
// Identical to Tag() today — the old verbose form ("— same LAN, no NAT
// crossed") was dropped because it read as jargon. Kept as a separate
// method so callers that want the headline form survive any future
// divergence.
func (i Info) Headline() string {
	return i.Tag()
}

// Detail returns the verbose ICE candidate trace for --debug output, e.g.
// "host → srflx". Empty when no candidate types are known (LAN/relay
// short-circuits).
func (i Info) Detail() string {
	if i.LocalCand == "" && i.RemoteCand == "" {
		return ""
	}
	return fmt.Sprintf("%s → %s", i.LocalCand, i.RemoteCand)
}
