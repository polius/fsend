package main

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
)

// The wrapper asks socketfilterfw the questions macFirewallCheck needs and
// looks this binary up in the app list (the judging itself is covered on
// every platform by TestMacFirewall).
func TestFirewallCheck(t *testing.T) {
	defer func(old func(...string) (string, error)) { runFirewallTool = old }(runFirewallTool)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const (
		on  = "Firewall is enabled. (State = 1)\n"
		all = "Firewall has block all state set to disabled.\n"
	)
	listed := func(setting string) string {
		return "Total number of apps = 1 \n1 : " + exe + " \n             (" + setting + " incoming connections)\n"
	}
	const none = "Total number of apps = 0 \n"
	permitted := "Incoming connection to " + exe + " is permitted.\n"
	for _, tc := range []struct {
		name     string
		answers  map[string]string
		answered bool
		ok, note bool
		want     string
	}{
		{"off", map[string]string{"--getglobalstate": "Firewall is disabled. (State = 0)\n"}, false, true, false, "is off"},
		{"block all", map[string]string{"--getglobalstate": on, "--getblockall": "Firewall has block all state set to enabled.\n"}, false, false, false, "blocks all incoming"},
		{"fsend blocked", map[string]string{"--getglobalstate": on, "--getblockall": all, "--listapps": listed("Block"), "--getappblocked": permitted}, true, false, false, "blocks incoming connections to fsend"},
		{"fsend allowed", map[string]string{"--getglobalstate": on, "--getblockall": all, "--listapps": listed("Allow"), "--getappblocked": permitted}, false, true, false, "allows"},
		{"not listed, listener answered", map[string]string{"--getglobalstate": on, "--getblockall": all, "--listapps": none, "--getappblocked": permitted}, true, true, false, "allows"},
		{"not listed, no answer", map[string]string{"--getglobalstate": on, "--getblockall": all, "--listapps": none, "--getappblocked": permitted}, false, false, true, "no entry for this fsend"},
		{"not listed, the tool says blocked", map[string]string{"--getglobalstate": on, "--getblockall": all, "--listapps": none,
			"--getappblocked": "Incoming connection to " + exe + " is blocked.\n"}, true, false, false, "blocks incoming connections to fsend"},
	} {
		runFirewallTool = func(args ...string) (string, error) {
			if args[0] == "--getappblocked" && (len(args) != 2 || args[1] != exe) {
				return "", errors.New("--getappblocked wants this binary's path")
			}
			if out, ok := tc.answers[args[0]]; ok {
				return out, nil
			}
			return "", errors.New("unexpected " + args[0])
		}
		c, shown := firewallCheck(net.IPv4(192, 168, 1, 20), tc.answered)
		if !shown || c.ok != tc.ok || c.note != tc.note || !strings.Contains(c.msg, tc.want) {
			t.Errorf("%s: got %+v (shown %v), want ok=%v note=%v containing %q", tc.name, c, shown, tc.ok, tc.note, tc.want)
		}
	}
	// A check that cannot run has no row.
	runFirewallTool = func(...string) (string, error) { return "", errors.New("no tool") }
	if _, shown := firewallCheck(nil, true); shown {
		t.Error("a row without a firewall tool")
	}
	runFirewallTool = func(args ...string) (string, error) {
		if args[0] == "--listapps" {
			return "", errors.New("no list")
		}
		return map[string]string{"--getglobalstate": on, "--getblockall": all}[args[0]], nil
	}
	if _, shown := firewallCheck(nil, true); shown {
		t.Error("a row without the app list")
	}
}
