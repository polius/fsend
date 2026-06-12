// Package connpath classifies how the QUIC data path between two fsend
// peers was actually established and renders that classification for the
// user-facing UX layer.
//
// The classification is tri-state:
//
//   - Local      — peers are on the same local network. Either mDNS
//     short-circuited the pairing-server path entirely, or ICE
//     selected a host↔host candidate pair (no NAT crossed).
//   - DirectNAT  — direct peer-to-peer over the internet. ICE
//     hole-punched through one or both NATs; at least one
//     side of the selected pair is a server-reflexive
//     (srflx) or peer-reflexive (prflx) candidate.
//   - Relay      — ICE failed; QUIC is tunneled through the pairing
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
	// KindLocal — same local network. No NAT crossed, no relay.
	KindLocal
	// KindDirectNAT — direct peer-to-peer over the internet, with
	// hole-punching through one or both NATs. The user-facing label
	// avoids the term "STUN" because the connection involves no
	// third-party STUN server; the STUN protocol is used only between
	// the two clients during ICE connectivity checks.
	KindDirectNAT
	// KindRelay — relayed via the pairing server's UDP listener.
	KindRelay
)

// Info bundles everything the UX layer needs to render the path line.
//
// LocalCand / RemoteCand are the lowercase ICE candidate type names
// ("host", "srflx", "prflx", "relay") and are only populated when the
// path was established via ICE (KindLocal-via-ICE or KindDirectNAT).
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

// FromLAN builds an Info for the mDNS-discovered same-network path.
// The function name is historical (it predates "local network" as the
// UX wording); the path it describes is the local-network short-circuit.
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
// DirectNAT vs Relay.
//
// Rules:
//   - relay anywhere → KindRelay (a relay-style allocation; kept
//     distinct from a "direct" claim).
//   - both host      → KindLocal (peers reached each other on
//     interface addresses; no NAT was crossed).
//   - anything else  → KindDirectNAT (at least one srflx/prflx,
//     meaning a NAT was punched through).
//
// Unknown / empty inputs are conservatively classified as KindDirectNAT
// rather than KindLocal — we only claim "Local" when we're sure.
func FromICE(localType, remoteType string) Info {
	info := Info{LocalCand: localType, RemoteCand: remoteType}
	switch {
	case localType == "relay" || remoteType == "relay":
		info.Kind = KindRelay
	case localType == "host" && remoteType == "host":
		info.Kind = KindLocal
	default:
		info.Kind = KindDirectNAT
	}
	return info
}

// Tag returns the compact path label used in summary lines and inline
// chips, e.g. "Direct on local network", "Direct over the internet",
// "Relayed via <host>".
//
// The labels deliberately avoid "LAN", "STUN", and "TURN":
//   - "local network" is everyday vocabulary; "LAN" sounds technical.
//   - "Direct over the internet" tells the user *where* the bytes went
//     while "Direct" still signals "no server in the middle." The old
//     "Direct via STUN" read like a third-party intermediary (it isn't).
//   - "Relayed via <addr>" already names what's in the middle without
//     leaking the "TURN" protocol name (the relay is custom, not TURN).
func (i Info) Tag() string {
	switch i.Kind {
	case KindLocal:
		return "Direct on local network"
	case KindDirectNAT:
		return "Direct over the internet"
	case KindRelay:
		if i.RelayAddr != "" {
			return "Relayed via " + i.RelayAddr
		}
		return "Relayed"
	default:
		return "unknown"
	}
}

// Headline is the standalone path line the CLI prints under --debug
// right after the data path is established, e.g.
//
//	✓ Direct over the internet
//
// Identical to Tag() today — the old verbose form ("— same LAN, no NAT
// crossed") was dropped because it read as jargon. Kept as a separate
// method so callers that want the headline form survive any future
// divergence.
func (i Info) Headline() string {
	return i.Tag()
}

// Chip returns the lowercase mid-line form shown when the connection is
// established: "Receiver connected (local network)" on the sender,
// "Incoming from <peer> · direct over the internet" on the receiver.
// Tag remains the standalone capitalized form for summary lines.
func (i Info) Chip() string {
	switch i.Kind {
	case KindLocal:
		return "local network"
	case KindDirectNAT:
		return "direct over the internet"
	case KindRelay:
		if i.RelayAddr != "" {
			return "relayed via " + i.RelayAddr
		}
		return "relayed"
	default:
		return "unknown"
	}
}

// Detail returns the verbose ICE candidate trace for --debug output, e.g.
// "host → srflx". Empty when no candidate types are known (LAN/relay
// short-circuits). Kept in pion's vocabulary — this is a deliberately
// technical surface for diagnosing connectivity; users only see it with
// --debug, and Googleable terms beat humanized labels here.
func (i Info) Detail() string {
	if i.LocalCand == "" && i.RemoteCand == "" {
		return ""
	}
	return fmt.Sprintf("%s → %s", i.LocalCand, i.RemoteCand)
}
