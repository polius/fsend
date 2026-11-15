package main

import (
	"net"
	"strconv"
	"strings"
)

// The firewall row of `fsend doctor` answers one question: can a device
// on this network connect to this machine's fsend listener — UDP for
// QUIC, and TCP for the TCP route receivers try first — and if not, what
// to change. Each system's firewall is read by a thin wrapper behind a
// build tag (doctor_firewall_<os>.go); what it read is judged by the pure
// functions in the files without a tag, which every platform's tests run
// against captured output.
//
// A firewall whose rules can't be read (no root, no authorization) gets a
// note that says so, never a guess.

// The listener's port range: a sender binds one port of it, derived from
// the code (landisc.PortForCode), for both UDP and TCP.
const (
	lanPortFirst = 50000
	lanPortLast  = 50999
	lanPorts     = "50000-50999"
)

// fwBlockedMeans is the consequence of a blocked listener, in the words
// every firewall row uses.
const fwBlockedMeans = " — devices on this network can't connect to this one directly, so transfers to them go through the server"

// fwAnswer is a firewall's verdict on one protocol of the listener.
type fwAnswer int

const (
	fwUnknown fwAnswer = iota // the rules don't settle it
	fwOpen
	fwBlocked
)

func (a fwAnswer) String() string {
	switch a {
	case fwOpen:
		return "open"
	case fwBlocked:
		return "blocked"
	}
	return "unclear"
}

// cover says how much of something a rule applies to: all of the
// listener's ports or only some, every device on the network or only
// some.
type cover int

const (
	coverNone cover = iota
	coverSome
	coverAll
)

// portCover relates the port range lo-hi to the listener's.
func portCover(lo, hi int) cover {
	switch {
	case lo <= lanPortFirst && hi >= lanPortLast:
		return coverAll
	case hi < lanPortFirst || lo > lanPortLast:
		return coverNone
	}
	return coverSome
}

// parsePorts reads one port or a range written with sep ("50000",
// "50000:50999", "50000-50999").
func parsePorts(s, sep string) (lo, hi int, ok bool) {
	a, b, isRange := strings.Cut(s, sep)
	lo, err := strconv.Atoi(strings.TrimSpace(a))
	if err != nil || lo < 0 || lo > 65535 {
		return 0, 0, false
	}
	if !isRange {
		return lo, lo, true
	}
	hi, err = strconv.Atoi(strings.TrimSpace(b))
	if err != nil || hi < lo || hi > 65535 {
		return 0, 0, false
	}
	return lo, hi, true
}

// portsCover relates a comma-separated list of ports and ranges to the
// listener's range. ok is false when an entry isn't numeric (a service
// name).
func portsCover(list, rangeSep string) (c cover, ok bool) {
	for _, e := range strings.Split(list, ",") {
		lo, hi, good := parsePorts(e, rangeSep)
		if !good {
			return coverNone, false
		}
		c = max(c, portCover(lo, hi))
	}
	return c, true
}

// lanNet is the network the listener is reached through: this machine's
// LAN address, with the interface and subnet it belongs to when known.
type lanNet struct {
	ip     net.IP
	iface  string     // "" when unknown
	subnet *net.IPNet // nil when unknown
}

// lanNetFor finds the interface and subnet of ip.
func lanNetFor(ip net.IP) lanNet {
	l := lanNet{ip: ip}
	ifaces, err := net.Interfaces()
	if err != nil {
		return l
	}
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
				l.iface = ifaces[i].Name
				l.subnet = &net.IPNet{IP: ipnet.IP.Mask(ipnet.Mask), Mask: ipnet.Mask}
				return l
			}
		}
	}
	return l
}

// v6 reports whether the listener is reached over IPv6.
func (l lanNet) v6() bool { return l.ip != nil && l.ip.To4() == nil }

// srcCover relates a rule's source network to the devices on this
// network: all of them when it contains the whole subnet, some when it
// lies inside the subnet, none otherwise.
func (l lanNet) srcCover(n *net.IPNet) cover {
	if n == nil || l.ip == nil {
		return coverSome
	}
	if l.subnet == nil {
		if n.Contains(l.ip) {
			return coverAll
		}
		return coverNone
	}
	nOnes, _ := n.Mask.Size()
	sOnes, _ := l.subnet.Mask.Size()
	switch {
	case n.Contains(l.subnet.IP) && nOnes <= sOnes:
		return coverAll
	case l.subnet.Contains(n.IP):
		return coverSome
	}
	return coverNone
}

// parseNet reads an address or a CIDR.
func parseNet(s string) *net.IPNet {
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}
}

// The inbound test (Linux, doctor_firewall_linux.go): doctor sends itself
// a datagram addressed to a multicast group, out through the LAN
// interface, on a UDP port of the listener's range. The kernel hands the
// copy for this machine back through the firewall's inbound rules, as if
// it had come from the network (a connection to the machine's own address
// never touches them), so whether it arrives shows what the firewall does
// with UDP to that port — without root, and whatever the rules say. It
// says nothing about TCP, and a rule that tells multicast from unicast
// traffic, or one device's address from another's, can treat a real
// connection differently.
type probeResult int

const (
	probeNone    probeResult = iota // not run, or it showed nothing either way
	probeThrough                    // the datagram arrived
	probeDropped                    // it did not, and one to the mDNS port, sent the same way, did
)

// udpProbe is what the inbound test found for one UDP port.
type udpProbe struct {
	result probeResult
	port   int
}

// seen words what the test saw, with who did it as the subject.
func (p udpProbe) seen(who string) string {
	if p.result == probeDropped {
		return who + " dropped a test datagram to UDP port " + strconv.Itoa(p.port) +
			" on its way in, so devices on this network won't reach fsend over UDP"
	}
	return who + " let a test datagram to UDP port " + strconv.Itoa(p.port) +
		" in, so devices on this network should reach fsend over UDP"
}

// unreadCheck is the row for a firewall whose rules can't be read. who
// names it; unread is what is known without them ("ufw is on …; its rules
// can't be read without root") and the whole message when the inbound
// test showed nothing; hidden says what stays out of reach when it did.
// A dropped datagram is a finding (!); one that got through leaves TCP
// open, so the row stays a note.
func unreadCheck(who, unread, hidden string, probe udpProbe, detail ...detailLine) doctorCheck {
	switch probe.result {
	case probeDropped:
		return warnRow("firewall", probe.seen(who)+"; "+hidden).add(detail...)
	case probeThrough:
		return noteRow("firewall", probe.seen(who)+"; "+hidden).add(detail...)
	}
	return noteRow("firewall", unread).add(detail...)
}

// portsCheck words the row for a firewall that filters by port, from what
// its rules say of each protocol. fix is how to open the ports, printed
// under a row that needs it. When the inbound test contradicts the rules
// on UDP, the rules doctor read are not the whole story (another table,
// a firewall that isn't loaded, a rule misread), so neither answer is
// reported as settled.
func portsCheck(name string, udp, tcp fwAnswer, probe udpProbe, fix ...detailLine) doctorCheck {
	if (udp == fwOpen && probe.result == probeDropped) || (udp == fwBlocked && probe.result == probeThrough) {
		return noteRow("firewall", "the rules of "+name+" read as "+udp.String()+" for UDP "+lanPorts+", but "+
			probe.seen("the firewall")+" — the rules doctor can read don't settle whether fsend's ports get through").add(fix...)
	}
	switch {
	case udp == fwOpen && tcp == fwOpen:
		return passRow("firewall", name+" lets devices on this network connect to fsend (UDP and TCP "+lanPorts+" are open)")
	case udp == fwBlocked && tcp == fwBlocked:
		return warnRow("firewall", name+" blocks fsend's ports (UDP and TCP "+lanPorts+")"+fwBlockedMeans).add(fix...)
	case udp == fwOpen && tcp == fwBlocked:
		return warnRow("firewall", name+" opens UDP "+lanPorts+" but blocks TCP — same-network transfers to this machine take the slower QUIC route").add(fix...)
	case udp == fwBlocked && tcp == fwOpen:
		return warnRow("firewall", name+" opens TCP "+lanPorts+" but blocks UDP — fine with a current fsend on the other side, which tries TCP first; an older one goes through the server").add(fix...)
	}
	return noteRow("firewall", name+" filters incoming connections, and its rules don't settle whether fsend's ports get through (UDP "+
		lanPorts+" "+udp.String()+", TCP "+tcp.String()+")").add(fix...)
}
