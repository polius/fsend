package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// socketfilterfw is the macOS application firewall's command-line tool.
const socketfilterfw = "/usr/libexec/ApplicationFirewall/socketfilterfw"

// runFirewallTool runs socketfilterfw with args. A var so tests can stand
// in for it.
var runFirewallTool = func(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, socketfilterfw, args...).Output() //nolint:gosec // G204: fixed tool; args are flags and this binary's own path
	return string(out), err
}

// firewallCheck reports whether the macOS firewall lets other devices on
// the network reach fsend's local listener. A blocked listener is the most
// common reason a same-network transfer goes through the server instead.
// ok is false when the firewall can't be queried.
func firewallCheck() (doctorCheck, bool) {
	state, err := runFirewallTool("--getglobalstate")
	if err != nil {
		return doctorCheck{}, false
	}
	if !strings.Contains(state, "enabled") {
		return doctorCheck{true, "firewall", "macOS firewall is off"}, true
	}
	const blocked = " — devices on this network can't connect to this one directly, so transfers to them go through the server"
	if all, err := runFirewallTool("--getblockall"); err == nil && strings.Contains(all, "enabled") {
		return doctorCheck{false, "firewall", "macOS firewall blocks all incoming connections" + blocked}, true
	}
	exe, err := os.Executable()
	if err != nil {
		return doctorCheck{}, false
	}
	app, err := runFirewallTool("--getappblocked", exe)
	if err != nil {
		return doctorCheck{}, false
	}
	if strings.Contains(app, "blocked") {
		return doctorCheck{false, "firewall", "macOS firewall blocks incoming connections to fsend" + blocked +
			" (allow fsend in System Settings → Network → Firewall → Options)"}, true
	}
	return doctorCheck{true, "firewall", "macOS firewall allows incoming connections to fsend"}, true
}
