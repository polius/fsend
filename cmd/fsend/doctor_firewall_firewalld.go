package main

import (
	"regexp"
	"strings"
)

// firewalld (Fedora, RHEL, openSUSE). firewall-cmd asks the daemon over
// D-Bus; whether an unprivileged user may read the rules is polkit's
// call: a desktop session usually may, an ssh session gets
// "Authorization failed". The zone names are readable either way, so a
// refused read still names the zone to look at.

// firewalldStatus reads `firewall-cmd --state`: "running", "not running",
// or — for a daemon that is up but won't answer this user —
// "Authorization failed.".
func firewalldStatus(out string) (running, authorized bool) {
	out = strings.ToLower(out)
	switch {
	case strings.Contains(out, "not running"):
		return false, false
	case strings.Contains(out, "authorization failed"):
		return true, false
	case strings.HasPrefix(strings.TrimSpace(out), "running"):
		return true, true
	}
	return false, false
}

// firewalldZoneName reads a zone from `firewall-cmd
// --get-zone-of-interface=…` or `--get-default-zone`: one word, or
// "no zone" for an interface bound to none (it then uses the default).
func firewalldZoneName(out string) string {
	out = strings.TrimSpace(out)
	if out == "" || strings.ContainsAny(out, " \t\n:") {
		return ""
	}
	return out
}

var (
	richPort   = regexp.MustCompile(`\bport port="([0-9-]+)" protocol="(tcp|udp)"`)
	richSource = regexp.MustCompile(`\bsource (NOT )?address="([^"]+)"`)
	richOther  = regexp.MustCompile(`\b(service|protocol|destination|source-port|forward-port|icmp-block|icmp-type|masquerade|source (?:NOT )?(?:mac|ipset))\b`)
	richQuoted = regexp.MustCompile(`"[^"]*"`)
	richAction = regexp.MustCompile(`\b(accept|reject|drop)\b`)
)

// firewalldPorts judges the listener's ports from `firewall-cmd
// --zone=<zone> --list-all`:
//
//	public (default, active)
//	  target: default
//	  services: dhcpv6-client mdns ssh
//	  ports: 50000-50999/udp 8080/tcp
//	  rich rules:
//		rule family="ipv4" source address="192.168.1.0/24" port port="50000-50999" protocol="tcp" accept
//
// A zone's target decides what its lists don't mention: ACCEPT lets
// everything in; default, DROP and %%REJECT%% let nothing else in. Rich
// rules that reject or drop are applied before anything is allowed.
// Services are not expanded: none of the stock ones covers fsend's range.
// rejected says that a rich rule is what stops (part of) the ports:
// opening them then means removing it.
func firewalldPorts(listAll string, lan lanNet) (udp, tcp fwAnswer, rejected bool) {
	var target string
	var open, richAllow, richBlock [2]cover // udp, tcp
	inRich := false
	for _, line := range strings.Split(listAll, "\n") {
		trimmed := strings.TrimSpace(line)
		if inRich {
			if !strings.HasPrefix(trimmed, "rule ") {
				continue
			}
			rule := trimmed
			who := coverAll
			if m := richSource.FindStringSubmatch(rule); m != nil {
				who = lan.srcCover(parseNet(m[2]))
				if m[1] != "" { // NOT: everyone but that source
					who = map[cover]cover{coverAll: coverNone, coverSome: coverSome, coverNone: coverAll}[who]
				}
			}
			if fam := lan.v6(); (fam && strings.Contains(rule, `family="ipv4"`)) || (!fam && strings.Contains(rule, `family="ipv6"`)) {
				continue
			}
			// The action, outside any quoted value (a log prefix may say
			// "drop"). A rule without one only logs or marks.
			act := richAction.FindStringSubmatch(richQuoted.ReplaceAllString(rule, `""`))
			if act == nil || who == coverNone {
				continue
			}
			var c [2]cover
			switch pm := richPort.FindStringSubmatch(rule); {
			case pm != nil:
				lo, hi, ok := parsePorts(pm[1], "-")
				if !ok {
					continue
				}
				i := 0
				if pm[2] == "tcp" {
					i = 1
				}
				c[i] = min(who, portCover(lo, hi))
			case richOther.MatchString(rule):
				continue // about something other than these ports
			default:
				c = [2]cover{who, who} // everything from that source
			}
			for i := range c {
				if act[1] == "accept" {
					richAllow[i] = max(richAllow[i], c[i])
				} else {
					richBlock[i] = max(richBlock[i], c[i])
				}
			}
			continue
		}
		key, val, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		switch val = strings.TrimSpace(val); key {
		case "target":
			target = val
		case "ports":
			for _, p := range strings.Fields(val) {
				spec, proto, _ := strings.Cut(p, "/")
				lo, hi, ok := parsePorts(spec, "-")
				if !ok {
					continue
				}
				switch proto {
				case "udp":
					open[0] = max(open[0], portCover(lo, hi))
				case "tcp":
					open[1] = max(open[1], portCover(lo, hi))
				}
			}
		case "rich rules":
			inRich = true
		}
	}
	answer := func(i int) fwAnswer {
		allowed := max(open[i], richAllow[i])
		if target == "ACCEPT" {
			allowed = coverAll
		}
		switch {
		case richBlock[i] == coverAll:
			return fwBlocked
		case richBlock[i] == coverSome && allowed != coverNone:
			return fwUnknown
		case allowed == coverAll:
			return fwOpen
		case allowed == coverSome:
			return fwUnknown
		}
		return fwBlocked
	}
	return answer(0), answer(1), max(richBlock[0], richBlock[1]) != coverNone
}

// firewalldFix opens the listener's ports in zone, now and for good.
func firewalldFix(zone string) []string {
	z := ""
	if zone != "" {
		z = " --zone=" + zone
	}
	return []string{
		"sudo firewall-cmd" + z + " --add-port=50000-50999/udp --add-port=50000-50999/tcp",
		"sudo firewall-cmd --runtime-to-permanent",
	}
}

// firewalldCheck is the row for a running firewalld. zone is the zone of
// the LAN interface ("" when unknown); listAll its `--list-all`, and
// readable is false when the daemon refused to show it. probe is what the
// inbound test saw.
func firewalldCheck(zone, listAll string, readable bool, lan lanNet, probe udpProbe) doctorCheck {
	name := "firewalld"
	if zone != "" {
		name = `firewalld (zone "` + zone + `")`
	}
	fix := firewalldFix(zone)
	if !readable {
		return unreadCheck(name, name+" is running; it won't show its rules without authorization",
			"it won't show its rules, or whether TCP gets through, without authorization", probe).with(
			"`sudo fsend doctor` reads them. fsend listens on UDP and TCP " + lanPorts + "; to open them:").run(fix...)
	}
	udp, tcp, rejected := firewalldPorts(listAll, lan)
	if rejected {
		// Rich rules that reject apply before any port is opened.
		z := ""
		if zone != "" {
			z = " --zone=" + zone
		}
		return portsCheck(name, udp, tcp, probe, detailLine{text: "a rich rule rejects them: `sudo firewall-cmd" + z + " --list-rich-rules` lists them, and --remove-rich-rule='<rule>' removes one"})
	}
	return portsCheck(name, udp, tcp, probe, noteRow("", "").with("to open them:").run(fix...).detail...)
}
