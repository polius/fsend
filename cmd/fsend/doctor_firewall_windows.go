package main

import (
	"context"
	"net"
	"os/exec"
	"time"

	"golang.org/x/sys/windows/registry"
)

// Where Windows Defender Firewall keeps its settings ([MS-GPFAS] 2.2):
// the local store, written by Windows Security and netsh, and the Group
// Policy store, which overrides it. Both are readable without
// administrator rights.
const (
	winLocalStore  = `SYSTEM\CurrentControlSet\Services\SharedAccess\Parameters\FirewallPolicy`
	winPolicyStore = `SOFTWARE\Policies\Microsoft\WindowsFirewall`
)

// regDwords reads a profile key's switches. ok is false when the key
// can't be opened.
func regDwords(path string) (vals map[string]uint32, ok bool) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if err != nil {
		return nil, false
	}
	defer func() { _ = k.Close() }()
	vals = map[string]uint32{}
	for _, name := range []string{"EnableFirewall", "DoNotAllowExceptions", "DefaultInboundAction", "DisableNotifications"} {
		if v, _, err := k.GetIntegerValue(name); err == nil {
			vals[name] = uint32(v) //nolint:gosec // G115: a REG_DWORD
		}
	}
	return vals, true
}

// regRules reads the rule strings under a FirewallRules key, with the
// environment variables in their program paths expanded.
func regRules(path string) []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer func() { _ = k.Close() }()
	names, err := k.ReadValueNames(-1)
	if err != nil {
		return nil
	}
	var rules []string
	for _, name := range names {
		s, _, err := k.GetStringValue(name)
		if err != nil {
			continue
		}
		if expanded, err := registry.ExpandString(s); err == nil {
			s = expanded
		}
		rules = append(rules, s)
	}
	return rules
}

// netsh runs `netsh advfirewall show <what>`.
func netsh(what string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "netsh", "advfirewall", "show", what).Output() //nolint:gosec // G204: a fixed tool with fixed arguments
	if err != nil {
		return ""
	}
	return string(out)
}

// overlay returns base with over's values on top.
func overlay(base, over map[string]uint32) map[string]uint32 {
	out := map[string]uint32{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// firewallCheck reports whether Windows Defender Firewall lets devices on
// the network reach fsend's listener (winFirewallCheck judges what was
// read). ok is false when the firewall's settings can't be read.
func firewallCheck(net.IP, bool) (doctorCheck, bool) {
	fw := winFirewall{profiles: map[int]winProfile{}}
	// The private profile is "StandardProfile" in the local store. In the
	// policy store a StandardProfile key is the pre-Vista policy, which
	// applies to private and public networks alike.
	legacy, _ := regDwords(winPolicyStore + `\StandardProfile`)
	for _, p := range []struct {
		bit           int
		local, policy string
	}{
		{winDomain, `\DomainProfile`, `\DomainProfile`},
		{winPrivate, `\StandardProfile`, `\PrivateProfile`},
		{winPublic, `\PublicProfile`, `\PublicProfile`},
	} {
		local, ok := regDwords(winLocalStore + p.local)
		if !ok {
			return doctorCheck{}, false
		}
		policy, _ := regDwords(winPolicyStore + p.policy)
		if p.bit != winDomain {
			policy = overlay(legacy, policy)
		}
		fw.profiles[p.bit] = winProfileFrom(policy, local)
	}
	fw.rules = append(regRules(winLocalStore+`\FirewallRules`), regRules(winPolicyStore+`\FirewallRules`)...)
	fw.active = winActiveProfiles(netsh("currentprofile"), netsh("allprofiles"))
	return winFirewallCheck(fw, exePaths()), true
}
