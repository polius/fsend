package main

import (
	"strconv"
	"strings"
)

// Windows Defender Firewall. It filters by program: the first time a
// program listens, Windows asks whether to allow it and records the
// answer as rules for that program — allow rules, or block rules when the
// prompt is cancelled, which then silently drop every connection to it.
// Those rules, the per-profile switches (domain, private, public) and
// which profile the current network is on decide whether a device on the
// network can connect.
//
// All of it is read without administrator rights and without parsing
// localized text: the switches and the rules come from the registry (the
// local store and the Group Policy store, which wins), and the profile in
// use from `netsh advfirewall show currentprofile`, whose localized
// section title is identified by position against `show allprofiles`.

// Profile bits, as the firewall API numbers them.
const (
	winDomain  = 1
	winPrivate = 2
	winPublic  = 4
)

var winProfiles = []int{winDomain, winPrivate, winPublic}

func winProfileName(p int) string {
	switch p {
	case winDomain:
		return "Domain"
	case winPrivate:
		return "Private"
	}
	return "Public"
}

// winProfile is one profile's switches.
type winProfile struct {
	on           bool // the firewall is on for this profile
	blockAll     bool // "Block all incoming connections, including those in the list of allowed apps"
	allowInbound bool // inbound connections without a rule are allowed (the default is to block)
	quiet        bool // Windows doesn't ask when a new program listens
}

// winProfileFrom merges a profile's registry values: policy, the Group
// Policy key, wins over local, the key Windows Security writes. A value
// in neither has the Windows default: firewall on, nothing blocked
// outright, inbound blocked unless a rule allows it, prompts shown.
//
// Value names and data are those of [MS-GPFAS] 2.2.2 (EnableFirewall,
// DoNotAllowExceptions, DefaultInboundAction: 0 allows, 1 blocks,
// DisableNotifications).
func winProfileFrom(policy, local map[string]uint32) winProfile {
	get := func(name string, def uint32) uint32 {
		if v, ok := policy[name]; ok {
			return v
		}
		if v, ok := local[name]; ok {
			return v
		}
		return def
	}
	return winProfile{
		on:           get("EnableFirewall", 1) != 0,
		blockAll:     get("DoNotAllowExceptions", 0) != 0,
		allowInbound: get("DefaultInboundAction", 1) == 0,
		quiet:        get("DisableNotifications", 0) != 0,
	}
}

// netshSections returns the title of each settings section in `netsh
// advfirewall show …` output: a line followed by a line of dashes.
//
//	Private Profile Settings:
//	----------------------------------------------------------------------
//	State                                 ON
//	Firewall Policy                       BlockInbound,AllowOutbound
func netshSections(out string) []string {
	var titles []string
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	for i := 1; i < len(lines); i++ {
		dashes := strings.TrimSpace(lines[i])
		if len(dashes) >= 10 && strings.Trim(dashes, "-") == "" {
			if t := strings.TrimSpace(lines[i-1]); t != "" {
				titles = append(titles, t)
			}
		}
	}
	return titles
}

// winActiveProfiles reads which profiles are in use from `netsh
// advfirewall show currentprofile` (one section per active profile). The
// section titles are localized, so each is matched against the three of
// `show allprofiles`, which always come in the order domain, private,
// public; English titles are recognized without it. 0 when it can't
// tell.
func winActiveProfiles(current, all string) int {
	ref := netshSections(all)
	active := 0
	for _, title := range netshSections(current) {
		bit := 0
		if len(ref) == len(winProfiles) {
			for i, r := range ref {
				if r == title {
					bit = winProfiles[i]
				}
			}
		}
		if bit == 0 {
			switch l := strings.ToLower(title); {
			case strings.HasPrefix(l, "domain profile"):
				bit = winDomain
			case strings.HasPrefix(l, "private profile"):
				bit = winPrivate
			case strings.HasPrefix(l, "public profile"):
				bit = winPublic
			}
		}
		active |= bit
	}
	return active
}

// winRule is a firewall rule, as far as the listener is concerned.
type winRule struct {
	allow    bool
	profiles int   // bits; every profile when the rule names none
	proto    int   // 6, 17, or -1 for any
	ports    cover // how much of the listener's range its local ports span
	app      string
	narrowed bool // limited by something this file doesn't judge (remote addresses, interfaces)
}

// parseWinRule reads a rule as the registry stores it ([MS-GPFAS]
// 2.2.2.14, the "firewall rule grammar"):
//
//	v2.31|Action=Allow|Active=TRUE|Dir=In|Protocol=17|Profile=Private|App=C:\Users\me\fsend.exe|Name=fsend.exe|Desc=fsend.exe|Defer=User|
//
// ok is false for a rule that can't affect the listener: inactive,
// outbound, for a service or a packaged app, another protocol, or one
// that needs an authenticated (IPsec) connection.
func parseWinRule(s string) (r winRule, ok bool) {
	fields := strings.Split(s, "|")
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "v") {
		return r, false
	}
	r.proto, r.ports = -1, coverAll
	action, dir, active := "", "", false
	havePorts := false
	for _, f := range fields[1:] {
		key, val, found := strings.Cut(f, "=")
		if !found {
			continue
		}
		switch key {
		case "Action":
			action = val
		case "Dir":
			dir = val
		case "Active":
			active = strings.EqualFold(val, "TRUE")
		case "Profile":
			switch val {
			case "Domain":
				r.profiles |= winDomain
			case "Private":
				r.profiles |= winPrivate
			case "Public":
				r.profiles |= winPublic
			}
		case "Protocol":
			n, err := strconv.Atoi(val)
			if err != nil {
				return r, false
			}
			r.proto = n
		case "LPort", "LPort2_10":
			if !havePorts {
				havePorts, r.ports = true, coverNone
			}
			if lo, hi, good := parsePorts(val, "-"); good {
				r.ports = max(r.ports, portCover(lo, hi))
			}
		case "LPort2_20":
			if !havePorts {
				havePorts, r.ports = true, coverNone
			}
		case "App":
			r.app = val
		case "Svc", "AppPkgId", "LUOwn", "LUAuth", "LUAuth2_24", "RMauth", "RUAuth":
			return r, false
		case "Security", "Security2_9", "Security2":
			if !strings.EqualFold(val, "NotRequired") {
				return r, false
			}
		case "RA4", "RA6", "RA42", "RA62":
			// A rule for the local subnet still lets the network's
			// devices in; any other remote address narrows it.
			if !strings.EqualFold(val, "LocalSubnet") {
				r.narrowed = true
			}
		case "LA4", "LA6", "IF", "IFType", "NNm", "TTK", "TTK2_22", "TTK2_27", "TTK2_28", "ICMP4", "ICMP6", "Platform", "Platform2",
			"RPort", "RPort2_10", "SecurityRealmId":
			// Remote ports too: the port a receiver connects from is arbitrary.
			r.narrowed = true
		}
	}
	if r.profiles == 0 {
		r.profiles = winDomain | winPrivate | winPublic
	}
	r.allow = strings.EqualFold(action, "Allow")
	if !active || !strings.EqualFold(dir, "In") || (!r.allow && !strings.EqualFold(action, "Block")) {
		return r, false
	}
	if r.proto != -1 && r.proto != 6 && r.proto != 17 {
		return r, false
	}
	return r, r.ports != coverNone
}

// winPath normalizes a program path for comparison: Windows paths are
// case-insensitive and may be written with either slash.
func winPath(p string) string {
	return strings.ToLower(strings.ReplaceAll(strings.Trim(strings.TrimSpace(p), `"`), "/", `\`))
}

// winVerdict is what one profile does with a connection to the listener,
// and why.
type winVerdict struct {
	answer fwAnswer
	why    string // "off", "blockall", "blockrule", "allowrule", "norule", "default"
}

// winFirewall is everything read from one machine.
type winFirewall struct {
	profiles map[int]winProfile
	active   int      // the profiles in use; 0 when unknown
	rules    []string // rule strings, with environment variables expanded
}

// judge says what profile p does with the listener's proto (6 or 17) for
// a program at one of exes. A block rule beats an allow rule; without
// either, the profile's default applies.
func (fw winFirewall) judge(p, proto int, exes []string) winVerdict {
	prof, ok := fw.profiles[p]
	if !ok {
		prof = winProfileFrom(nil, nil)
	}
	switch {
	case !prof.on:
		return winVerdict{fwOpen, "off"}
	case prof.blockAll:
		return winVerdict{fwBlocked, "blockall"}
	}
	allow, block, unsure := false, false, false
	for _, s := range fw.rules {
		r, ok := parseWinRule(s)
		if !ok || r.profiles&p == 0 || (r.proto != -1 && r.proto != proto) {
			continue
		}
		if r.app != "" {
			mine := false
			for _, exe := range exes {
				mine = mine || winPath(r.app) == winPath(exe)
			}
			if !mine {
				continue
			}
		}
		switch {
		case r.narrowed || r.ports != coverAll:
			unsure = true
		case r.allow:
			allow = true
		default:
			block = true
		}
	}
	switch {
	case block:
		return winVerdict{fwBlocked, "blockrule"}
	case unsure:
		return winVerdict{fwUnknown, "rules"}
	case allow:
		return winVerdict{fwOpen, "allowrule"}
	case prof.allowInbound:
		return winVerdict{fwOpen, "default"}
	}
	return winVerdict{fwBlocked, "norule"}
}

// winFirewallCheck is the row for Windows Defender Firewall. exes are the
// paths this fsend may be known under.
func winFirewallCheck(fw winFirewall, exes []string) doctorCheck {
	exe := ""
	if len(exes) > 0 {
		exe = exes[0]
	}
	addRule := `netsh advfirewall firewall add rule name="fsend" dir=in action=allow program="` + exe + `" enable=yes`
	type both struct{ udp, tcp winVerdict }
	verdict := func(p int) both { return both{fw.judge(p, 17, exes), fw.judge(p, 6, exes)} }

	// The profiles to answer for: those in use, or all three when netsh
	// didn't say.
	var consider []int
	for _, p := range winProfiles {
		if fw.active == 0 || fw.active&p != 0 {
			consider = append(consider, p)
		}
	}
	first := verdict(consider[0])
	same := true
	for _, p := range consider[1:] {
		same = same && verdict(p) == first
	}
	if !same {
		// The answer depends on the network type and doctor doesn't know
		// (or the machine is on two kinds at once): say each.
		var parts []string
		for _, p := range consider {
			v := verdict(p)
			state := "blocked"
			switch {
			case v.udp.answer == fwOpen && v.tcp.answer == fwOpen:
				state = "allowed"
			case v.udp.answer == fwUnknown || v.tcp.answer == fwUnknown:
				state = "unclear"
			case v.udp.answer == fwOpen || v.tcp.answer == fwOpen:
				state = "partly allowed"
			}
			parts = append(parts, winProfileName(p)+" networks: "+state)
		}
		return noteRow("firewall", "Windows Firewall treats fsend differently by network type — "+strings.Join(parts, "; ")).with(
			"Settings → Network & internet shows which type this network is. To allow fsend on every type, in an administrator terminal:").run(addRule)
	}

	where := ""
	if fw.active != 0 && len(consider) == 1 {
		where = " (" + winProfileName(consider[0]) + " network)"
	}
	v := first
	switch {
	case v.udp.why == "off":
		return passRow("firewall", "Windows Firewall is off"+where)
	case v.udp.why == "blockall":
		return warnRow("firewall", "Windows Firewall blocks all incoming connections"+where+fwBlockedMeans).with(
			"turn off “Blocks all incoming connections, including those in the list of allowed apps” in Windows Security → Firewall & network protection")
	case v.udp.answer == fwOpen && v.tcp.answer == fwOpen:
		return passRow("firewall", "Windows Firewall allows incoming connections to fsend"+where)
	case v.udp.why == "blockrule" || v.tcp.why == "blockrule":
		return warnRow("firewall", "Windows Firewall has a rule blocking fsend"+where+fwBlockedMeans).with(
			"Windows adds such a rule when its “allow access?” prompt is cancelled. Tick fsend in Windows Security → Firewall & network protection → Allow an app through firewall, or delete the rule in wf.msc → Inbound Rules.")
	case v.udp.answer == fwBlocked && v.tcp.answer == fwBlocked:
		// No rule at all. Is fsend allowed on another network type?
		elsewhere := ""
		for _, p := range winProfiles {
			if o := verdict(p); o.udp.why == "allowrule" && o.tcp.why == "allowrule" {
				elsewhere = winProfileName(p)
			}
		}
		if elsewhere != "" {
			return warnRow("firewall", "Windows Firewall allows fsend on "+elsewhere+" networks only, and this one isn't"+where+fwBlockedMeans).with(
				"set this network to " + elsewhere + " in Settings → Network & internet, or allow fsend on every network type, in an administrator terminal:").run(addRule)
		}
		ask := "Windows asks to allow fsend the first time it listens: choose Allow."
		if p, ok := fw.profiles[consider[0]]; ok && p.quiet {
			ask = "Windows is set not to ask about new programs on this network type."
		}
		return warnRow("firewall", "Windows Firewall has no rule allowing fsend"+where+", so it blocks incoming connections to it"+fwBlockedMeans).with(
			ask + " Or, in an administrator terminal:").run(addRule)
	case v.udp.answer == fwOpen && v.tcp.answer == fwBlocked:
		return warnRow("firewall", "Windows Firewall allows UDP to fsend but not TCP"+where+" — same-network transfers to this machine take the slower QUIC route").with(
			"to allow both, in an administrator terminal:").run(addRule)
	case v.udp.answer == fwBlocked && v.tcp.answer == fwOpen:
		return warnRow("firewall", "Windows Firewall allows TCP to fsend but not UDP"+where+" — fine with a current fsend on the other side, which tries TCP first; an older one goes through the server").with(
			"to allow both, in an administrator terminal:").run(addRule)
	}
	return noteRow("firewall", "Windows Firewall has rules for fsend"+where+" that don't settle whether devices on this network get through").with(
		"they are limited to some addresses, interfaces or ports; see wf.msc → Inbound Rules. To allow fsend outright, in an administrator terminal:").run(addRule)
}
