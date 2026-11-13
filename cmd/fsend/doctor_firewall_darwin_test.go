package main

import (
	"errors"
	"strings"
	"testing"
)

func TestFirewallCheck(t *testing.T) {
	defer func(old func(...string) (string, error)) { runFirewallTool = old }(runFirewallTool)
	for _, tc := range []struct {
		name    string
		answers map[string]string
		ok      bool
		want    string
	}{
		{"off", map[string]string{"--getglobalstate": "Firewall is disabled. (State = 0)"}, true, "is off"},
		{"block all", map[string]string{"--getglobalstate": "Firewall is enabled. (State = 1)", "--getblockall": "Firewall has block all state set to enabled."}, false, "blocks all incoming"},
		{"fsend blocked", map[string]string{"--getglobalstate": "Firewall is enabled. (State = 1)", "--getblockall": "Firewall has block all state set to disabled.",
			"--getappblocked": "Incoming connection to /x/fsend is blocked."}, false, "blocks incoming connections to fsend"},
		{"fsend allowed", map[string]string{"--getglobalstate": "Firewall is enabled. (State = 1)", "--getblockall": "Firewall has block all state set to disabled.",
			"--getappblocked": "Incoming connection to /x/fsend is permitted."}, true, "allows"},
	} {
		runFirewallTool = func(args ...string) (string, error) {
			if out, ok := tc.answers[args[0]]; ok {
				return out, nil
			}
			return "", errors.New("unexpected " + args[0])
		}
		c, shown := firewallCheck()
		if !shown || c.ok != tc.ok || !strings.Contains(c.msg, tc.want) {
			t.Errorf("%s: got %+v (shown %v), want ok=%v containing %q", tc.name, c, shown, tc.ok, tc.want)
		}
	}
	runFirewallTool = func(...string) (string, error) { return "", errors.New("no tool") }
	if _, shown := firewallCheck(); shown {
		t.Error("a row without a firewall tool")
	}
}
