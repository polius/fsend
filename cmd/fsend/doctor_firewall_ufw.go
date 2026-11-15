package main

import "strings"

// ufw (Ubuntu, Debian, Mint). `ufw status` needs root, but ufw keeps its
// state in files: /etc/ufw/ufw.conf (whether it is on) and
// /etc/default/ufw (the default policy) are world-readable; the rules,
// /etc/ufw/user.rules and user6.rules, are root-only. So without root
// doctor can say ufw is on and what it does by default, says plainly that
// the rules are out of reach, and reports what the inbound test saw
// (udpProbe).

// ufwFix opens the listener's ports.
const ufwFix = "sudo ufw allow 50000:50999/udp && sudo ufw allow 50000:50999/tcp"

// shellVar returns the value of NAME=value in a shell-style config file,
// unquoted, or "" when it isn't set.
func shellVar(text, name string) string {
	val := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, name+"="); ok {
			val = strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return val
}

// ufwEnabled reads /etc/ufw/ufw.conf.
func ufwEnabled(conf string) bool {
	return strings.EqualFold(shellVar(conf, "ENABLED"), "yes")
}

// ufwInputPolicy reads the default policy for incoming traffic from
// /etc/default/ufw: ACCEPT, DROP or REJECT. ufw ships with DROP.
func ufwInputPolicy(defaults string) string {
	if p := strings.ToUpper(shellVar(defaults, "DEFAULT_INPUT_POLICY")); p != "" {
		return p
	}
	return "DROP"
}

// ufwPorts judges the listener's ports from a user.rules file. ufw writes
// each rule's meaning as a comment above the iptables lines it generates:
//
//	### tuple ### allow udp 50000:50999 0.0.0.0/0 any 192.168.1.0/24 in
//
// (action, protocol, destination port, destination, source port, source,
// direction; rules made from an application profile carry two more fields
// before the direction). Rules apply in order and the first match wins;
// what none matches gets the default policy. denied says that a deny or
// reject rule is what stops (part of) the ports: allowing them again
// means deleting it, since a rule added later comes after it.
func ufwPorts(rules, policy string, lan lanNet) (udp, tcp fwAnswer, denied bool) {
	type state struct {
		decided              bool
		open                 bool
		someAllow, someBlock bool // an earlier rule covers part of the traffic
	}
	var st [2]state // udp, tcp
	for _, line := range strings.Split(rules, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "### tuple ### ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if n := len(f); n > 0 && strings.HasPrefix(f[n-1], "comment=") {
			f = f[:n-1]
		}
		if len(f) != 7 && len(f) != 9 {
			continue
		}
		action, _, _ := strings.Cut(f[0], "_") // allow_log, deny_log-all
		proto, dport, dst, sport, src, dir := f[1], f[2], f[3], f[4], f[5], f[len(f)-1]

		// Direction: "in", or "in_eth0" for one interface. Outgoing and
		// routed ("in_eth0!out_eth1") rules don't concern the listener.
		c := coverAll
		switch iface, onIface := strings.CutPrefix(dir, "in_"); {
		case dir == "in":
		case !onIface || strings.Contains(dir, "!"):
			continue
		case lan.iface == "":
			c = coverSome
		case iface != lan.iface:
			continue
		}
		if dport != "any" {
			pc, ok := portsCover(dport, ":")
			if !ok {
				pc = coverSome
			}
			c = min(c, pc)
		}
		if sport != "any" {
			c = min(c, coverSome) // a receiver's source port is arbitrary
		}
		if dst != "0.0.0.0/0" && dst != "::/0" {
			if n := parseNet(dst); n == nil || lan.ip == nil || !n.Contains(lan.ip) {
				continue // a rule for another of this machine's addresses
			}
		}
		if src != "0.0.0.0/0" && src != "::/0" {
			c = min(c, lan.srcCover(parseNet(src)))
		}
		if c == coverNone {
			continue
		}
		allow := action == "allow" || action == "limit"
		if !allow && action != "deny" && action != "reject" {
			continue
		}
		for i, p := range []string{"udp", "tcp"} {
			if proto != "any" && proto != p {
				continue
			}
			s := &st[i]
			switch {
			case s.decided:
			case c == coverAll:
				s.decided, s.open = true, allow
				denied = denied || !allow
			case allow:
				s.someAllow = true
			default:
				s.someBlock = true
				denied = true
			}
		}
	}
	answer := func(s state) fwAnswer {
		if !s.decided {
			s.open = policy == "ACCEPT"
		}
		switch {
		case s.open && s.someBlock, !s.open && s.someAllow:
			return fwUnknown
		case s.open:
			return fwOpen
		}
		return fwBlocked
	}
	return answer(st[0]), answer(st[1]), denied
}

// ufwCheck is the row for an active ufw. rules is the user.rules file of
// the listener's address family; readable is false when it could not be
// read (it is root-only). probe is what the inbound test saw.
func ufwCheck(policy, rules string, readable bool, lan lanNet, probe udpProbe) doctorCheck {
	if !readable {
		byDefault := "blocks incoming connections by default"
		if policy == "ACCEPT" {
			byDefault = "allows incoming connections by default"
		}
		return unreadCheck("ufw", "ufw is on and "+byDefault+"; its rules can't be read without root",
			"its rules, and whether TCP gets through, can't be read without root", probe).with(
			"`sudo ufw status` lists them. fsend listens on UDP and TCP " + lanPorts + "; to open them:").run(ufwFix)
	}
	udp, tcp, denied := ufwPorts(rules, policy, lan)
	if denied {
		// An allow added now would come after the rule that denies.
		return portsCheck("ufw", udp, tcp, probe, detailLine{text: "a ufw rule denies them: `sudo ufw status numbered` lists the rules, `sudo ufw delete <number>` removes one"})
	}
	return portsCheck("ufw", udp, tcp, probe, detailLine{text: "to open them:"}, detailLine{text: ufwFix, cmd: true})
}
