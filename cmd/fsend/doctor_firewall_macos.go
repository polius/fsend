package main

import (
	"regexp"
	"strings"
)

// The macOS application firewall, as its command-line tool reports it
// (/usr/libexec/ApplicationFirewall/socketfilterfw; reading needs no
// root). It filters by application, not by port, so one answer covers UDP
// and TCP.
//
// Two things about it, both seen on macOS 27 with the firewall's own log
// (subsystem com.apple.ALF.ApplicationFirewall) open:
//
//   - `--getappblocked <path>` answers "permitted" for anything the
//     firewall has no rule against, including an app it has never seen
//     and will ask about, and a path that doesn't exist. Only its
//     "blocked" is information; the app list says who is allowed.
//   - The firewall knows an app by its code-signing identifier, not its
//     path, and Go's linker signs every binary as "a.out": a fsend at a
//     path the list doesn't hold is often allowed already. What settles
//     it is the listener self-test, because this firewall screens a
//     connection from the machine to its own LAN address like any
//     incoming one: it holds those to an app it doesn't know until the
//     user (or policy) has answered.

// macFirewallOn reads `--getglobalstate`:
//
//	Firewall is enabled. (State = 1)
//	Firewall is disabled. (State = 0)
//
// State 2 is "block all incoming connections" on the systems that report
// it there.
func macFirewallOn(out string) (on, blockAll bool) {
	switch {
	case strings.Contains(out, "State = 0"):
		return false, false
	case strings.Contains(out, "State = 2"):
		return true, true
	case strings.Contains(out, "State = 1"):
		return true, false
	}
	return strings.Contains(strings.ToLower(out), "enabled"), false
}

// macBlockAll reads `--getblockall`:
//
//	Firewall has block all state set to disabled.
//
// (older systems: "Block all DISABLED!").
func macBlockAll(out string) bool {
	return strings.Contains(strings.ToLower(out), "enabled")
}

var macAppLine = regexp.MustCompile(`^\s*\d+\s*:\s*(.*\S)\s*$`)

// macFirewallApps reads `--listapps` into each listed path's setting,
// true for "Allow incoming connections":
//
//	Total number of apps = 2
//	1 : /opt/homebrew/Caskroom/fsend/1.12.0/fsend
//	             (Allow incoming connections)
//	2 : /Applications/Example.app
//	             (Block incoming connections)
func macFirewallApps(out string) map[string]bool {
	apps := map[string]bool{}
	path := ""
	for _, line := range strings.Split(out, "\n") {
		if m := macAppLine.FindStringSubmatch(line); m != nil {
			path = m[1]
			continue
		}
		if path == "" {
			continue
		}
		switch l := strings.ToLower(line); {
		case strings.Contains(l, "allow incoming"):
			apps[path] = true
			path = ""
		case strings.Contains(l, "block incoming"):
			apps[path] = false
			path = ""
		}
	}
	return apps
}

// macFirewallWhere is where the setting lives.
const macFirewallWhere = "System Settings → Network → Firewall → Options"

// macAppBlocked reads `--getappblocked <path>`:
//
//	Incoming connection to /opt/homebrew/bin/fsend is permitted.
//
// and "… is blocked." for an app the firewall refuses.
func macAppBlocked(out string) bool {
	return strings.Contains(strings.ToLower(out), "is blocked")
}

// macFirewallCheck is the row for the macOS firewall. appBlocked is what
// `--getappblocked` said of this fsend; exes are the paths it may be
// listed under (the one it was started as, and the one that resolves
// to); answered says whether the listener self-test got through.
func macFirewallCheck(globalState, blockAll, appBlocked, listApps string, exes []string, answered bool) doctorCheck {
	const allows = "macOS firewall allows incoming connections to fsend"
	on, all := macFirewallOn(globalState)
	if !on {
		return passRow("firewall", "macOS firewall is off")
	}
	if all || macBlockAll(blockAll) {
		return warnRow("firewall", "macOS firewall blocks all incoming connections"+fwBlockedMeans).with(
			"turn off “Block all incoming connections” in " + macFirewallWhere)
	}
	blocked := warnRow("firewall", "macOS firewall blocks incoming connections to fsend"+fwBlockedMeans).with(
		"allow fsend in " + macFirewallWhere)
	apps := macFirewallApps(listApps)
	for _, exe := range exes {
		allowed, listed := apps[exe]
		switch {
		case !listed:
		case allowed:
			return passRow("firewall", allows)
		default:
			return blocked
		}
	}
	switch {
	case macAppBlocked(appBlocked):
		return blocked
	case answered:
		return passRow("firewall", allows).with(
			"it has no entry for this fsend's path, but let the listener test through, which it screens like any incoming connection")
	}
	return noteRow("firewall", "macOS firewall is on and has no entry for this fsend yet").with(
		"macOS asks whether fsend may accept incoming connections when it first listens: choose Allow, then run fsend doctor again. Or add fsend in " + macFirewallWhere)
}
