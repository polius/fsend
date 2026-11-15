package main

import (
	"context"
	"net"
	"os/exec"
	"time"
)

// socketfilterfw is the macOS application firewall's command-line tool.
const socketfilterfw = "/usr/libexec/ApplicationFirewall/socketfilterfw"

// runFirewallTool runs socketfilterfw with args. A var so tests can stand
// in for it.
var runFirewallTool = func(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, socketfilterfw, args...).Output() //nolint:gosec // G204: a fixed tool; args are flags and this binary's own path
	return string(out), err
}

// firewallCheck reports whether the macOS firewall lets other devices on
// the network reach fsend's local listener (macFirewallCheck judges what
// the tool printed). A blocked listener is the most common reason a
// same-network transfer goes through the server instead. answered says
// whether the listener self-test got through. ok is false when the
// firewall can't be queried.
func firewallCheck(_ net.IP, answered bool) (doctorCheck, bool) {
	state, err := runFirewallTool("--getglobalstate")
	if err != nil {
		return doctorCheck{}, false
	}
	if on, _ := macFirewallOn(state); !on {
		return macFirewallCheck(state, "", "", "", nil, answered), true
	}
	blockAll, _ := runFirewallTool("--getblockall")
	apps, err := runFirewallTool("--listapps")
	if err != nil && !macBlockAll(blockAll) {
		return doctorCheck{}, false
	}
	exes := exePaths()
	appBlocked := ""
	if len(exes) > 0 {
		appBlocked, _ = runFirewallTool("--getappblocked", exes[0])
	}
	return macFirewallCheck(state, blockAll, appBlocked, apps, exes, answered), true
}
