package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The firewall rows are judged by pure functions fed what each system's
// tools print, so these tests run on every platform. The samples live in
// testdata/doctor:
//
//   - linux/: all captured from real systems, in Docker. ufw 0.36.2,
//     nftables 1.0.9 and iptables 1.8.10 (nf_tables, and legacy for the
//     iptables-legacy files) on Ubuntu 24.04; firewalld 2.3.2 on Fedora
//     42; nft-docker.txt is what Docker 27.5.1 set up for a container
//     publishing TCP 8080 and UDP 50123 (iptables 1.8.11, nftables 1.1.1,
//     Alpine). The rules in the others were made with the tools
//     themselves (ufw allow …, firewall-cmd --add-port …, nft -f,
//     iptables -A); the rulesets fed to nft -f for the "arch", "debian"
//     and "wiki" files were typed from those distributions' stock
//     /etc/nftables.conf and the nftables wiki, then listed back by nft.
//   - macos/listapps.txt: the format of a real `socketfilterfw --listapps`
//     (macOS 27), with the paths replaced. listapps.blocked.txt is written
//     after it: no machine at hand had a blocked app.
//   - windows/: written from Microsoft's documentation of `netsh
//     advfirewall show` and of the registry rule grammar ([MS-GPFAS]
//     2.2.2), not captured: no Windows machine was at hand.

// detailText is what a row prints under itself, one line per entry.
func detailText(c doctorCheck) string {
	var lines []string
	for _, d := range c.detail {
		lines = append(lines, d.text)
	}
	return strings.Join(lines, "\n")
}

func fwSample(t *testing.T, name string) string {
	t.Helper()
	if name == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join("testdata", "doctor", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may hold the samples with CRLF line ends (git's
	// autocrlf). The tools that wrote them end lines with LF, and that is
	// what the parsers are tested on, whatever the checkout did.
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// lanAt is a machine at cidr on iface.
func lanAt(cidr, iface string) lanNet {
	ip, n, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return lanNet{ip: ip, iface: iface, subnet: n}
}

// nfDump is the kernel's rules as one dump: `nft list ruleset`, or the
// filter table of `iptables -S` on a machine without nft.
func nfDump(dump string, nft bool) nfRules {
	if nft {
		return nfRules{nft: dump}
	}
	return nfRules{noNft: true, iptables: map[string]string{"filter": dump}}
}

// Three places the same rules can be read from.
var (
	lanHome  = lanAt("192.168.1.20/24", "eth0") // the subnet the sample rules name
	lanOther = lanAt("10.0.0.5/24", "eth0")
	lanWifi  = lanAt("10.0.0.5/24", "wlan0")
	lanV6    = lanAt("2001:db8::5/64", "eth0")
)

func TestFirewallPortHelpers(t *testing.T) {
	for _, tc := range []struct {
		s, sep string
		lo, hi int
		ok     bool
		cover  cover
	}{
		{"50000:50999", ":", 50000, 50999, true, coverAll},
		{"1025-65535", "-", 1025, 65535, true, coverAll},
		{"50100", ":", 50100, 50100, true, coverSome},
		{"50500-60000", "-", 50500, 60000, true, coverSome},
		{"22", "-", 22, 22, true, coverNone},
		{"60000:61000", ":", 60000, 61000, true, coverNone},
		{"ssh", "-", 0, 0, false, coverNone},
		{"99999", "-", 0, 0, false, coverNone},
		{"9-3", "-", 0, 0, false, coverNone},
	} {
		lo, hi, ok := parsePorts(tc.s, tc.sep)
		if lo != tc.lo || hi != tc.hi || ok != tc.ok {
			t.Errorf("parsePorts(%q) = %d-%d %v, want %d-%d %v", tc.s, lo, hi, ok, tc.lo, tc.hi, tc.ok)
		}
		if ok && portCover(lo, hi) != tc.cover {
			t.Errorf("portCover(%q) = %v, want %v", tc.s, portCover(lo, hi), tc.cover)
		}
	}
	if c, ok := portsCover("80,443,50500", ":"); !ok || c != coverSome {
		t.Errorf("a list with one port of the range: %v %v", c, ok)
	}
	if c, ok := portsCover("80,40000:60000", ":"); !ok || c != coverAll {
		t.Errorf("a list with a range that spans it: %v %v", c, ok)
	}

	for _, tc := range []struct {
		lan  lanNet
		src  string
		want cover
	}{
		{lanHome, "192.168.1.0/24", coverAll},
		{lanHome, "192.168.0.0/16", coverAll},
		{lanHome, "192.168.1.128/25", coverSome},
		{lanHome, "192.168.1.77", coverSome},
		{lanHome, "10.0.0.0/8", coverNone},
		{lanV6, "2001:db8::/32", coverAll},
		{lanV6, "192.168.1.0/24", coverNone},
		{lanNet{ip: net.ParseIP("192.168.1.20")}, "192.168.1.0/24", coverAll}, // subnet unknown
		{lanNet{ip: net.ParseIP("192.168.1.20")}, "10.0.0.0/8", coverNone},
	} {
		if got := tc.lan.srcCover(parseNet(tc.src)); got != tc.want {
			t.Errorf("%v from %s: cover %v, want %v", tc.lan.ip, tc.src, got, tc.want)
		}
	}

	// The loopback address belongs to an interface with a subnet on every
	// system, which is all lanNetFor needs to find.
	if l := lanNetFor(net.IPv4(127, 0, 0, 1)); l.iface == "" || l.subnet == nil || !l.subnet.Contains(l.ip) {
		t.Errorf("lanNetFor(127.0.0.1) = %+v", l)
	}
	if l := lanNetFor(net.ParseIP("203.0.113.250")); l.iface != "" || l.subnet != nil {
		t.Errorf("an address this machine doesn't have: %+v", l)
	}
}

func TestPortsCheckRows(t *testing.T) {
	const fix = "sudo open-the-ports"
	for _, tc := range []struct {
		udp, tcp fwAnswer
		ok, note bool
		want     string
		fixed    bool
	}{
		{fwOpen, fwOpen, true, false, "lets devices on this network connect to fsend (UDP and TCP 50000-50999 are open)", false},
		{fwBlocked, fwBlocked, false, false, "blocks fsend's ports (UDP and TCP 50000-50999) — devices on this network can't connect to this one directly, so transfers to them go through the server", true},
		{fwOpen, fwBlocked, false, false, "opens UDP 50000-50999 but blocks TCP — same-network transfers to this machine take the slower QUIC route", true},
		{fwBlocked, fwOpen, false, false, "opens TCP 50000-50999 but blocks UDP", true},
		{fwOpen, fwUnknown, false, true, "don't settle whether fsend's ports get through (UDP 50000-50999 open, TCP unclear)", true},
		{fwUnknown, fwBlocked, false, true, "(UDP 50000-50999 unclear, TCP blocked)", true},
	} {
		c := portsCheck("ufw", tc.udp, tc.tcp, udpProbe{}, detailLine{text: fix, cmd: true})
		if c.ok != tc.ok || c.note != tc.note || c.label != "firewall" || !strings.HasPrefix(c.msg, "ufw ") || !strings.Contains(c.msg, tc.want) {
			t.Errorf("udp %v tcp %v: %+v", tc.udp, tc.tcp, c)
		}
		if got := detailText(c) == fix; got != tc.fixed {
			t.Errorf("udp %v tcp %v: detail %q", tc.udp, tc.tcp, detailText(c))
		}
	}
}

// The inbound test's part in the rows: what it saw is reported where the
// rules can't be read, and where it contradicts the rules that were read
// neither is reported as settled.
func TestFirewallRowsWithProbe(t *testing.T) {
	through, dropped := udpProbe{probeThrough, 50321}, udpProbe{probeDropped, 50321}
	fix := detailLine{text: "sudo open-the-ports", cmd: true}

	// The rules and the test agree: the row is the rules' row.
	for _, tc := range []struct {
		udp, tcp fwAnswer
		probe    udpProbe
	}{
		{fwOpen, fwOpen, through}, {fwBlocked, fwBlocked, dropped}, {fwOpen, fwBlocked, through},
		{fwBlocked, fwOpen, dropped}, {fwUnknown, fwOpen, dropped}, {fwUnknown, fwBlocked, through},
	} {
		with, without := portsCheck("ufw", tc.udp, tc.tcp, tc.probe, fix), portsCheck("ufw", tc.udp, tc.tcp, udpProbe{}, fix)
		if with.msg != without.msg || with.ok != without.ok || with.note != without.note {
			t.Errorf("udp %v tcp %v, test %v: %+v, want %+v", tc.udp, tc.tcp, tc.probe.result, with, without)
		}
	}
	// They disagree: a note, which says both and keeps the fix.
	c := portsCheck("ufw", fwOpen, fwOpen, dropped, fix)
	if !c.note || c.ok || c.msg != "the rules of ufw read as open for UDP 50000-50999, but the firewall dropped a test datagram to UDP port 50321 on its way in, "+
		"so devices on this network won't reach fsend over UDP — the rules doctor can read don't settle whether fsend's ports get through" ||
		detailText(c) != fix.text {
		t.Errorf("rules open, datagram dropped: %+v", c)
	}
	c = portsCheck(`firewalld (zone "public")`, fwBlocked, fwBlocked, through, fix)
	if !c.note || c.ok || !strings.HasPrefix(c.msg, `the rules of firewalld (zone "public") read as blocked for UDP 50000-50999, but the firewall let a test datagram to UDP port 50321 in, `+
		"so devices on this network should reach fsend over UDP — ") {
		t.Errorf("rules closed, datagram through: %+v", c)
	}

	// Rules that can't be read: the test's finding leads, a drop as a
	// problem, a pass as a note (TCP is still unknown).
	for _, tc := range []struct {
		name     string
		row      doctorCheck
		ok, note bool
		want     string
	}{
		{"ufw, nothing seen", ufwCheck("DROP", "", false, lanHome, udpProbe{}), false, true,
			"ufw is on and blocks incoming connections by default; its rules can't be read without root"},
		{"ufw, dropped", ufwCheck("DROP", "", false, lanHome, dropped), false, false,
			"ufw dropped a test datagram to UDP port 50321 on its way in, so devices on this network won't reach fsend over UDP; its rules, and whether TCP gets through, can't be read without root"},
		{"ufw, through", ufwCheck("DROP", "", false, lanHome, through), false, true,
			"ufw let a test datagram to UDP port 50321 in, so devices on this network should reach fsend over UDP; its rules, and whether TCP gets through, can't be read without root"},
		{"firewalld, dropped", firewalldCheck("public", "", false, lanHome, dropped), false, false,
			`firewalld (zone "public") dropped a test datagram to UDP port 50321 on its way in, so devices on this network won't reach fsend over UDP; it won't show its rules, or whether TCP gets through, without authorization`},
		{"firewalld, through", firewalldCheck("public", "", false, lanHome, through), false, true,
			`firewalld (zone "public") let a test datagram to UDP port 50321 in, so devices`},
		{"kernel rules, nothing seen", netfilterUnreadable("nftables/iptables rules need root to read", "`sudo fsend doctor` reads them. ", udpProbe{}), false, true,
			"can't tell: ufw and firewalld aren't active here, and nftables/iptables rules need root to read"},
		{"kernel rules, dropped", netfilterUnreadable("nftables/iptables rules need root to read", "`sudo fsend doctor` reads them. ", dropped), false, false,
			"a firewall on this machine dropped a test datagram to UDP port 50321 on its way in, so devices on this network won't reach fsend over UDP; ufw and firewalld aren't active here, and nftables/iptables rules need root to read"},
		{"kernel rules, through", netfilterUnreadable("nftables/iptables rules need root to read", "`sudo fsend doctor` reads them. ", through), false, true,
			"a test datagram to UDP port 50321 came in unhindered, so devices on this network should reach fsend over UDP; whether TCP is filtered can't be told: ufw and firewalld aren't active here, and nftables/iptables rules need root to read"},
		{"no tool to read with", netfilterUnreadable("there is no nft or iptables here to read the kernel's rules with", "", through), false, true,
			"whether TCP is filtered can't be told: ufw and firewalld aren't active here, and there is no nft or iptables here to read the kernel's rules with"},
	} {
		if tc.row.ok != tc.ok || tc.row.note != tc.note || tc.row.label != "firewall" || !strings.Contains(tc.row.msg, tc.want) {
			t.Errorf("%s: got ok=%v note=%v %q", tc.name, tc.row.ok, tc.row.note, tc.row.msg)
		}
		if detailText(tc.row) == "" {
			t.Errorf("%s: no word on what to do", tc.name)
		}
	}
	// The rules read as empty, but something dropped the datagram: not
	// "no firewall".
	if c := netfilterCheck(nfDump("", true), lanHome, dropped); c.ok || !c.note || !strings.Contains(c.msg, "the firewall dropped a test datagram") {
		t.Errorf("empty ruleset, datagram dropped: %+v", c)
	}
	if c := netfilterCheck(nfDump("", true), lanHome, through); !c.ok || !strings.Contains(c.msg, "no firewall rules filter incoming connections") {
		t.Errorf("empty ruleset, datagram through: %+v", c)
	}
}

// ---- ufw ---------------------------------------------------------------

func TestUfw(t *testing.T) {
	if !ufwEnabled(fwSample(t, "linux/ufw.conf.enabled")) || ufwEnabled(fwSample(t, "linux/ufw.conf.disabled")) || ufwEnabled("") {
		t.Error("ENABLED in ufw.conf")
	}
	for file, want := range map[string]string{"linux/default-ufw.deny": "DROP", "linux/default-ufw.allow": "ACCEPT", "linux/default-ufw.reject": "REJECT", "": "DROP"} {
		if got := ufwInputPolicy(fwSample(t, file)); got != want {
			t.Errorf("%s: policy %q, want %q", file, got, want)
		}
	}
	for _, tc := range []struct {
		rules, policy string
		lan           lanNet
		udp, tcp      fwAnswer
	}{
		{"linux/ufw-user.rules.empty", "DROP", lanHome, fwBlocked, fwBlocked},
		{"linux/ufw-user.rules.empty", "REJECT", lanHome, fwBlocked, fwBlocked},
		{"linux/ufw-user.rules.empty", "ACCEPT", lanHome, fwOpen, fwOpen},
		// ufw allow 50000:50999/udp; ufw allow 50000:50999/tcp
		{"linux/ufw-user.rules.range", "DROP", lanHome, fwOpen, fwOpen},
		{"linux/ufw-user6.rules.range", "DROP", lanV6, fwOpen, fwOpen},
		// With a comment, and with logging (allow_log).
		{"linux/ufw-user.rules.app-comment-log", "DROP", lanOther, fwOpen, fwOpen},
		// The mixed file: UDP for 192.168.1.0/24 only, everything from that
		// subnet, TCP on eth0 only, single ports of the range denied,
		// rejected and allowed.
		{"linux/ufw-user.rules.mixed", "DROP", lanHome, fwOpen, fwOpen},
		{"linux/ufw-user.rules.mixed", "DROP", lanOther, fwBlocked, fwOpen},
		// On another interface only TCP 50500 of the range is open: unclear.
		{"linux/ufw-user.rules.mixed", "DROP", lanWifi, fwBlocked, fwUnknown},
		// Allowed by default, but UDP 50100 is denied and TCP 50200 rejected.
		{"linux/ufw-user.rules.mixed", "ACCEPT", lanWifi, fwUnknown, fwUnknown},
		// The IPv6 file has no subnet rules: UDP only loses port 50100.
		{"linux/ufw-user6.rules.mixed", "DROP", lanV6, fwBlocked, fwOpen},
	} {
		udp, tcp, _ := ufwPorts(fwSample(t, tc.rules), tc.policy, tc.lan)
		if udp != tc.udp || tcp != tc.tcp {
			t.Errorf("%s, %s by default, at %v on %s: udp %v tcp %v, want %v %v",
				tc.rules, tc.policy, tc.lan.ip, tc.lan.iface, udp, tcp, tc.udp, tc.tcp)
		}
	}

	// Without root the rules are out of reach, and the row says so.
	c := ufwCheck("DROP", "", false, lanHome, udpProbe{})
	if !c.note || c.ok || !strings.Contains(c.msg, "ufw is on and blocks incoming connections by default; its rules can't be read without root") ||
		!strings.Contains(detailText(c), "sudo ufw allow 50000:50999/udp && sudo ufw allow 50000:50999/tcp") {
		t.Errorf("unreadable rules: %+v", c)
	}
	if c := ufwCheck("ACCEPT", "", false, lanHome, udpProbe{}); !c.note || !strings.Contains(c.msg, "allows incoming connections by default") {
		t.Errorf("unreadable rules, allowed by default: %+v", c)
	}
	if c := ufwCheck("DROP", fwSample(t, "linux/ufw-user.rules.empty"), true, lanHome, udpProbe{}); c.ok || c.note ||
		!strings.Contains(c.msg, "ufw blocks fsend's ports") || !strings.Contains(detailText(c), "sudo ufw allow 50000:50999/udp") {
		t.Errorf("no rule: %+v", c)
	}
	if c := ufwCheck("DROP", fwSample(t, "linux/ufw-user.rules.range"), true, lanHome, udpProbe{}); !c.ok || len(c.detail) != 0 {
		t.Errorf("ports open: %+v", c)
	}
	// What a rule denies, another allow doesn't bring back (it would come
	// after): the row says to delete the rule instead. Here ufw allows by
	// default and the mixed file denies UDP 50100 and rejects TCP 50200.
	if c := ufwCheck("ACCEPT", fwSample(t, "linux/ufw-user.rules.mixed"), true, lanWifi, udpProbe{}); !c.note ||
		!strings.Contains(detailText(c), "sudo ufw delete <number>") || strings.Contains(detailText(c), "ufw allow") {
		t.Errorf("denied by a rule: %+v", c)
	}
}

// ---- firewalld ---------------------------------------------------------

func TestFirewalld(t *testing.T) {
	// What `firewall-cmd --state` printed on Fedora 42: as root, with the
	// daemon stopped, as a user polkit refuses, and with no D-Bus at all.
	for out, want := range map[string][2]bool{
		"running\n":     {true, true},
		"not running\n": {false, false},
		"Authorization failed.\n    Make sure polkit agent is running or run the application as superuser.\n":     {true, false},
		"Error: DBUS_ERROR: Failed to connect to socket /run/dbus/system_bus_socket: No such file or directory\n": {false, false},
		"": {false, false},
	} {
		if running, authorized := firewalldStatus(out); running != want[0] || authorized != want[1] {
			t.Errorf("state %q: running %v authorized %v", out, running, authorized)
		}
	}
	for out, want := range map[string]string{"public\n": "public", "FedoraWorkstation\n": "FedoraWorkstation", "no zone\n": "", "": "",
		"Authorization failed.\n    Make sure polkit agent is running or run the application as superuser.\n": ""} {
		if got := firewalldZoneName(out); got != want {
			t.Errorf("zone %q = %q, want %q", out, got, want)
		}
	}

	for _, tc := range []struct {
		file     string
		lan      lanNet
		udp, tcp fwAnswer
	}{
		{"linux/firewalld-list-all.public.txt", lanHome, fwBlocked, fwBlocked},
		{"linux/firewalld-list-all.home.txt", lanHome, fwBlocked, fwBlocked},
		{"linux/firewalld-list-all.drop.txt", lanHome, fwBlocked, fwBlocked},
		{"linux/firewalld-list-all.trusted.txt", lanHome, fwOpen, fwOpen},                                  // target: ACCEPT
		{"linux/firewalld-list-all.FedoraWorkstation.txt", lanHome, fwOpen, fwOpen},                        // 1025-65535 open
		{"linux/firewalld-list-all.fsend-ports.txt", lanHome, fwOpen, fwOpen},                              // --add-port=50000-50999/udp, /tcp
		{"linux/firewalld-list-all.udp-only-rich.txt", lanHome, fwOpen, fwOpen},                            // TCP by a rich rule for 192.168.1.0/24
		{"linux/firewalld-list-all.udp-only-rich.txt", lanOther, fwOpen, fwBlocked},                        // … which isn't this network
		{"linux/firewalld-list-all.udp-only-rich.txt", lanV6, fwOpen, fwBlocked},                           // … nor IPv6
		{"linux/firewalld-list-all.udp-only-rich.txt", lanAt("192.168.0.9/16", "eth0"), fwOpen, fwUnknown}, // part of this network
	} {
		udp, tcp, _ := firewalldPorts(fwSample(t, tc.file), tc.lan)
		if udp != tc.udp || tcp != tc.tcp {
			t.Errorf("%s at %v: udp %v tcp %v, want %v %v", tc.file, tc.lan.ip, udp, tcp, tc.udp, tc.tcp)
		}
	}

	// Rich rules that reject come before anything is allowed.
	const zone = "public\n  target: default\n  ports: 50000-50999/udp 50000-50999/tcp\n  rich rules: \n"
	for rule, want := range map[string][2]fwAnswer{
		`rule family="ipv4" port port="50000-50999" protocol="udp" reject`:                                                        {fwBlocked, fwOpen},
		`rule family="ipv4" source address="192.168.1.0/24" drop`:                                                                 {fwBlocked, fwBlocked},
		`rule family="ipv4" source address="192.168.1.77" reject`:                                                                 {fwUnknown, fwUnknown},
		`rule family="ipv4" source NOT address="192.168.1.0/24" drop`:                                                             {fwOpen, fwOpen},
		`rule family="ipv6" drop`:                                                                                                 {fwOpen, fwOpen},
		`rule family="ipv4" service name="ssh" log prefix="drop " level="info" limit value="1/m" accept`:                          {fwOpen, fwOpen},
		`rule family="ipv4" source address="192.168.1.0/24" port port="50100" protocol="tcp" reject type="icmp-port-unreachable"`: {fwOpen, fwUnknown},
	} {
		udp, tcp, _ := firewalldPorts(zone+"\t"+rule+"\n", lanHome)
		if udp != want[0] || tcp != want[1] {
			t.Errorf("%s: udp %v tcp %v, want %v", rule, udp, tcp, want)
		}
	}
	// What a rich rule rejects, opening the port doesn't bring back: the
	// row says to remove the rule instead.
	c := firewalldCheck("public", zone+"\t"+`rule family="ipv4" port port="50000-50999" protocol="udp" reject`+"\n", true, lanHome, udpProbe{})
	if c.ok || c.note || !strings.Contains(c.msg, "opens TCP 50000-50999 but blocks UDP") ||
		!strings.Contains(detailText(c), "sudo firewall-cmd --zone=public --list-rich-rules") || strings.Contains(detailText(c), "--add-port") {
		t.Errorf("rejected by a rich rule: %+v", c)
	}

	c = firewalldCheck("public", "", false, lanHome, udpProbe{})
	detail := detailText(c)
	if !c.note || !strings.Contains(c.msg, `firewalld (zone "public") is running; it won't show its rules without authorization`) ||
		!strings.Contains(detail, "sudo firewall-cmd --zone=public --add-port=50000-50999/udp --add-port=50000-50999/tcp") ||
		!strings.Contains(detail, "sudo firewall-cmd --runtime-to-permanent") {
		t.Errorf("unreadable: %+v", c)
	}
	if c := firewalldCheck("public", fwSample(t, "linux/firewalld-list-all.public.txt"), true, lanHome, udpProbe{}); c.ok || c.note ||
		!strings.Contains(c.msg, `firewalld (zone "public") blocks fsend's ports`) || !strings.Contains(detailText(c), "--zone=public --add-port=50000-50999/udp") {
		t.Errorf("public zone: %+v", c)
	}
	if c := firewalldCheck("FedoraWorkstation", fwSample(t, "linux/firewalld-list-all.FedoraWorkstation.txt"), true, lanHome, udpProbe{}); !c.ok || len(c.detail) != 0 {
		t.Errorf("FedoraWorkstation zone: %+v", c)
	}
}

// ---- nftables and iptables --------------------------------------------

func TestNetfilter(t *testing.T) {
	for _, tc := range []struct {
		file     string
		nft      bool
		lan      lanNet
		udp, tcp fwAnswer
		filtered bool
	}{
		// Nothing loaded, Debian's stock ruleset (no policy, no rules), and
		// what is left after `ufw disable` or `iptables -F`.
		{"", true, lanHome, fwOpen, fwOpen, false},
		{"linux/nft-debian-default.txt", true, lanHome, fwOpen, fwOpen, false},
		{"linux/nft-iptables-accept-all.txt", true, lanHome, fwOpen, fwOpen, false},
		{"linux/nft-ufw-disabled.txt", true, lanHome, fwOpen, fwOpen, true},
		// Arch's stock ruleset: drop by default, ssh only. Then the same
		// after the two commands the row suggests.
		{"linux/nft-arch-default.txt", true, lanHome, fwBlocked, fwBlocked, true},
		{"linux/nft-arch-default.txt", true, lanV6, fwBlocked, fwBlocked, true},
		{"linux/nft-arch-fsend-open.txt", true, lanHome, fwOpen, fwOpen, true},
		{"linux/nft-arch-fsend-open.txt", true, lanV6, fwOpen, fwOpen, true},
		// Verdict maps, a jump per family, and a named set (not read: a
		// TCP rule on it might open the ports). UDP is open for
		// 172.16.0.0/12 only.
		{"linux/nft-wiki-vmap.txt", true, lanAt("172.17.0.3/16", "eth0"), fwOpen, fwUnknown, true},
		{"linux/nft-wiki-vmap.txt", true, lanHome, fwBlocked, fwUnknown, true},
		// ufw's own ruleset, loaded from the "mixed" user.rules: the same
		// answers TestUfw gets from the file.
		{"linux/nft-ufw-enabled.txt", true, lanHome, fwOpen, fwOpen, true},
		{"linux/nft-ufw-enabled.txt", true, lanOther, fwBlocked, fwOpen, true},
		{"linux/nft-ufw-enabled.txt", true, lanWifi, fwBlocked, fwUnknown, true},
		{"linux/nft-ufw-enabled.txt", true, lanV6, fwBlocked, fwOpen, true},
		// firewalld's ruleset for its public zone with 50000-50999/udp
		// open and a rich rule opening TCP to 192.168.1.0/24: the same
		// answers TestFirewalld gets from --list-all.
		{"linux/nft-firewalld.txt", true, lanHome, fwOpen, fwOpen, true},
		{"linux/nft-firewalld.txt", true, lanOther, fwOpen, fwBlocked, true},
		{"linux/nft-firewalld.txt", true, lanV6, fwOpen, fwBlocked, true},
		// Hand-written iptables rules, read through nft and through
		// iptables -S.
		{"linux/nft-iptables-hand-udp-only.txt", true, lanHome, fwOpen, fwBlocked, true},
		{"linux/iptables-S-hand-udp-only.txt", false, lanHome, fwOpen, fwBlocked, true},
		{"linux/iptables-S-hand-open.txt", false, lanHome, fwOpen, fwOpen, true},
		{"linux/iptables-S-accept-all.txt", false, lanHome, fwOpen, fwOpen, false},
		{"linux/iptables-S-ufw-enabled.txt", false, lanHome, fwOpen, fwOpen, true},
		{"linux/iptables-S-ufw-enabled.txt", false, lanOther, fwBlocked, fwOpen, true},
		{"linux/iptables-S-ufw-enabled.txt", false, lanWifi, fwBlocked, fwUnknown, true},
		{"linux/ip6tables-S-ufw-enabled.txt", false, lanV6, fwBlocked, fwOpen, true},
		// What Docker sets up: nothing on the inbound path but its nat
		// chain, which sends the ports containers publish elsewhere. One
		// here publishes UDP 50123, a port of the listener's range.
		{"linux/nft-docker.txt", true, lanHome, fwUnknown, fwOpen, true},
		{"linux/nft-docker.txt", true, lanV6, fwOpen, fwOpen, false},
		// A netdev table drops on ingress, ahead of an inet table that
		// accepts: its rules aren't followed, so nothing is settled.
		{"linux/nft-netdev-ingress.txt", true, lanHome, fwUnknown, fwUnknown, true},
	} {
		udp, tcp, filtered, _ := netfilterPorts(nfDump(fwSample(t, tc.file), tc.nft), tc.lan)
		if udp != tc.udp || tcp != tc.tcp || filtered != tc.filtered {
			t.Errorf("%q at %v on %s: udp %v tcp %v filtered %v, want %v %v %v",
				tc.file, tc.lan.ip, tc.lan.iface, udp, tcp, filtered, tc.udp, tc.tcp, tc.filtered)
		}
	}

	// The fix names the chain that stops the connection, in that chain's
	// own tool. nft-arch-fsend-open.txt and iptables-S-hand-open.txt are
	// what these commands left behind when they were run.
	detail := func(file string, nft bool) string {
		return detailText(netfilterCheck(nfDump(fwSample(t, file), nft), lanHome, udpProbe{}))
	}
	if d := detail("linux/nft-arch-default.txt", true); !strings.Contains(d, "sudo nft insert rule inet filter input udp dport 50000-50999 accept") ||
		!strings.Contains(d, "sudo nft insert rule inet filter input tcp dport 50000-50999 accept") {
		t.Errorf("nft fix: %q", d)
	}
	if d := detail("linux/nft-iptables-hand-udp-only.txt", true); !strings.Contains(d, "sudo iptables -I INPUT -p tcp --dport 50000:50999 -j ACCEPT") {
		t.Errorf("a table iptables manages, read through nft: %q", d)
	}
	if d := detail("linux/iptables-S-hand-udp-only.txt", false); !strings.Contains(d, "sudo iptables -I INPUT -p tcp --dport 50000:50999 -j ACCEPT") {
		t.Errorf("iptables fix: %q", d)
	}
	// The legacy iptables backend, which nft doesn't show: an empty nft
	// ruleset says nothing about it. Here its filter table drops by
	// default and accepts fsend's UDP, then its raw table drops that too.
	legacy := nfRules{nft: "", iptables: map[string]string{"filter": fwSample(t, "linux/iptables-legacy-S-udp-only.txt")}}
	if udp, tcp, filtered, fix := netfilterPorts(legacy, lanHome); udp != fwOpen || tcp != fwBlocked || !filtered ||
		len(fix) != 3 || fix[2].text != "sudo iptables -I INPUT -p tcp --dport 50000:50999 -j ACCEPT" {
		t.Errorf("legacy filter table: udp %v tcp %v filtered %v fix %v", udp, tcp, filtered, fix)
	}
	if c := netfilterCheck(legacy, lanHome, udpProbe{}); c.ok || c.note || !strings.HasPrefix(c.msg, "nftables opens UDP 50000-50999 but blocks TCP") {
		t.Errorf("legacy filter table: %+v", c)
	}
	legacy.iptables["raw"] = fwSample(t, "linux/iptables-legacy-raw-S-drop-udp.txt")
	if udp, tcp, _, fix := netfilterPorts(legacy, lanHome); udp != fwBlocked || tcp != fwBlocked || len(fix) != 0 {
		t.Errorf("legacy raw and filter tables: udp %v tcp %v fix %v", udp, tcp, fix) // no one-line fix for a raw table
	}
	if got := legacyTablesToRead(fwSample(t, "linux/proc-ip_tables_names.raw-filter.txt")); len(got) != 2 || got[0] != "raw" || got[1] != "filter" {
		t.Errorf("legacy tables to read: %v", got)
	}
	if got := legacyTablesToRead("nat\n"); len(got) != 0 {
		t.Errorf("a nat table alone is nothing to read: %v", got)
	}
	for version, want := range map[string]bool{"iptables v1.8.10 (nf_tables)\n": true, "iptables v1.8.10 (legacy)\n": false, "iptables v1.6.1\n": false} {
		if iptablesIsNft(version) != want {
			t.Errorf("%q: nft variant %v", version, !want)
		}
	}
	if c := netfilterCheck(nfRules{noNft: true}, lanHome, udpProbe{}); !c.ok || !strings.Contains(c.msg, "(iptables has none on the inbound path)") {
		t.Errorf("no nft, no legacy tables: %+v", c)
	}

	if c := netfilterCheck(nfDump("", true), lanHome, udpProbe{}); !c.ok || !strings.Contains(c.msg, "no firewall rules filter incoming connections") {
		t.Errorf("empty ruleset: %+v", c)
	}
	if c := netfilterCheck(nfDump(fwSample(t, "linux/nft-arch-default.txt"), true), lanHome, udpProbe{}); c.ok || c.note || !strings.Contains(c.msg, "nftables blocks fsend's ports") {
		t.Errorf("arch default: %+v", c)
	}
	if c := netfilterUnreadable("nftables/iptables rules need root to read", "`sudo fsend doctor` reads them. ", udpProbe{}); !c.note || c.ok ||
		c.msg != "can't tell: ufw and firewalld aren't active here, and nftables/iptables rules need root to read" ||
		detailText(c) != "`sudo fsend doctor` reads them. fsend listens on UDP and TCP 50000-50999." {
		t.Errorf("unreadable: %+v", c)
	}
}

// Rules the samples don't hold, one at a time, in a chain that drops by
// default: the rule decides.
func TestNftRules(t *testing.T) {
	for rule, want := range map[string][2]fwAnswer{
		`udp dport 50000-50999 accept`:                                 {fwOpen, fwBlocked},
		`meta l4proto { tcp, udp } th dport 50000-50999 accept`:        {fwOpen, fwOpen},
		`meta l4proto { tcp, udp } th dport 1024-65535 counter accept`: {fwOpen, fwOpen},
		`iifname "eth0" accept`:                                        {fwOpen, fwOpen},
		`iifname "eth*" accept`:                                        {fwOpen, fwOpen},
		`iifname != "lo" accept`:                                       {fwOpen, fwOpen},
		`iifname { "wlan0", "eth1" } accept`:                           {fwBlocked, fwBlocked},
		`iifname vmap { "lo" : accept, "eth0" : jump lan }`:            {fwOpen, fwBlocked},
		`ip saddr 192.168.1.0/24 accept`:                               {fwOpen, fwOpen},
		`ip saddr 192.168.1.77 accept`:                                 {fwUnknown, fwUnknown}, // one device of the network
		`ip saddr @trusted accept`:                                     {fwUnknown, fwUnknown}, // a named set
		`ip saddr != 192.168.1.0/24 accept`:                            {fwBlocked, fwBlocked},
		`ip6 saddr fe80::/10 accept`:                                   {fwBlocked, fwBlocked},
		`ip daddr 192.168.1.20 tcp dport 50000-50999 accept`:           {fwBlocked, fwOpen},
		`tcp dport 50000-50500 accept`:                                 {fwBlocked, fwUnknown}, // half the range
		`tcp dport != 22 accept`:                                       {fwBlocked, fwOpen},
		`tcp dport vmap { 22 : accept, 50000-50999 : jump lan }`:       {fwBlocked, fwBlocked},
		`ct state new accept`:                                          {fwOpen, fwOpen},
		`ct state established,related accept`:                          {fwBlocked, fwBlocked},
		`meta pkttype host accept`:                                     {fwOpen, fwOpen},
		`fib daddr type local accept`:                                  {fwOpen, fwOpen},
		`meta nfproto ipv4 accept`:                                     {fwOpen, fwOpen},
		`meta nfproto ipv6 accept`:                                     {fwBlocked, fwBlocked},
		`tcp flags & (fin | syn | rst | ack) == syn accept`:            {fwBlocked, fwUnknown}, // not modelled
		`meta mark 0x1 accept`:                                         {fwUnknown, fwUnknown}, // not modelled
		`limit rate over 10/second drop`:                               {fwBlocked, fwBlocked},
		`log prefix "in: " level info`:                                 {fwBlocked, fwBlocked},
		`udp dport 50000-50999 queue`:                                  {fwUnknown, fwBlocked},
		`goto lan`:                                                     {fwOpen, fwBlocked},
		`jump missing`:                                                 {fwUnknown, fwUnknown},
		// The value of an expression that isn't modelled can be a word
		// this file reads ("ip", "tcp", "ct"): the verdict after it counts.
		`ether type ip accept`:                             {fwUnknown, fwUnknown},
		`ether type ip drop`:                               {fwBlocked, fwBlocked},
		`meta l4proto tcp accept`:                          {fwBlocked, fwOpen},
		`ip6 nexthdr tcp accept`:                           {fwBlocked, fwBlocked},
		`tcp option maxseg size set rt mtu`:                {fwBlocked, fwBlocked}, // no verdict
		`rt type 0 accept`:                                 {fwBlocked, fwBlocked}, // a routing header
		`udp dport 50000-50999 redirect to :5000`:          {fwUnknown, fwBlocked},
		`udp dport 50123 counter xt target "DNAT"`:         {fwUnknown, fwBlocked}, // as nft prints iptables' DNAT
		`counter xt target "LOG"`:                          {fwBlocked, fwBlocked},
		`ip saddr 192.168.1.0/24 xt match "recent" accept`: {fwUnknown, fwUnknown},
	} {
		ruleset := "table inet filter {\n\tchain lan {\n\t\tudp dport 50000-50999 accept\n\t}\n\n" +
			"\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t\t" + rule + "\n\t}\n}\n"
		udp, tcp, _, _ := netfilterPorts(nfDump(ruleset, true), lanHome)
		if udp != want[0] || tcp != want[1] {
			t.Errorf("%s: udp %v tcp %v, want %v", rule, udp, tcp, want)
		}
	}

	// A drop the certain rules reach is final even after a rule that
	// might have accepted; an accept after a rule that might have
	// dropped is not.
	open := "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t\tmeta mark 0x1 drop\n\t}\n}\n"
	if udp, tcp, _, _ := netfilterPorts(nfDump(open, true), lanHome); udp != fwUnknown || tcp != fwUnknown {
		t.Errorf("a drop that might apply, then accept: udp %v tcp %v", udp, tcp)
	}
	// Two base chains: the connection must pass both. Chains hooked
	// elsewhere (forward, output, postrouting) don't see it.
	two := "table inet a {\n\tchain in {\n\t\ttype filter hook input priority filter; policy accept;\n\t}\n}\n" +
		"table ip b {\n\tchain pre {\n\t\ttype filter hook prerouting priority raw; policy accept;\n\t\tudp dport 50000-50999 drop\n\t}\n}\n" +
		"table ip c {\n\tchain fwd {\n\t\ttype filter hook forward priority filter; policy drop;\n\t}\n\n" +
		"\tchain post {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n\t\ttcp dport 50000-50999 drop\n\t}\n}\n"
	if udp, tcp, _, _ := netfilterPorts(nfDump(two, true), lanHome); udp != fwBlocked || tcp != fwOpen {
		t.Errorf("two base chains: udp %v tcp %v", udp, tcp)
	}
	// A nat chain before routing is on the path too: it can drop.
	nat := "table ip nat {\n\tchain PREROUTING {\n\t\ttype nat hook prerouting priority dstnat; policy accept;\n\t\ttcp dport 50000-50999 drop\n\t}\n}\n"
	if udp, tcp, _, _ := netfilterPorts(nfDump(nat, true), lanHome); udp != fwOpen || tcp != fwBlocked {
		t.Errorf("a nat chain that drops: udp %v tcp %v", udp, tcp)
	}
	// A bridge or netdev table that can't drop changes nothing.
	harmless := "table bridge b {\n\tchain in {\n\t\ttype filter hook input priority filter; policy accept;\n\t\tcounter\n\t}\n}\n" + two
	if udp, tcp, _, _ := netfilterPorts(nfDump(harmless, true), lanHome); udp != fwBlocked || tcp != fwOpen {
		t.Errorf("a bridge table that only counts: udp %v tcp %v", udp, tcp)
	}
	if udp, _, _, _ := netfilterPorts(nfDump(two, true), lanV6); udp != fwOpen {
		t.Errorf("an ip table doesn't see IPv6: udp %v", udp)
	}
}

func TestIptablesRules(t *testing.T) {
	for rule, want := range map[string][2]fwAnswer{
		`-p udp -m udp --dport 50000:50999 -j ACCEPT`:                                            {fwOpen, fwBlocked},
		`-p tcp -m multiport --dports 22,50000:50999 -m comment --comment "fsend lan" -j ACCEPT`: {fwBlocked, fwOpen},
		`-s 192.168.1.0/24 -j ACCEPT`:                                                            {fwOpen, fwOpen},
		`! -s 192.168.1.0/24 -j ACCEPT`:                                                          {fwBlocked, fwBlocked},
		`-i eth+ -j ACCEPT`:                                                                      {fwOpen, fwOpen},
		`! -i lo -m conntrack --ctstate NEW -j ACCEPT`:                                           {fwOpen, fwOpen},
		`-m state --state RELATED,ESTABLISHED -j ACCEPT`:                                         {fwBlocked, fwBlocked},
		`-m addrtype --dst-type LOCAL -j ACCEPT`:                                                 {fwOpen, fwOpen},
		`-p tcp -m tcp --dport 50000:50999 --syn -j ACCEPT`:                                      {fwBlocked, fwOpen},
		`-p tcp -m tcp --dport 50000:50999 -m recent --update --seconds 30 --name x -j ACCEPT`:   {fwBlocked, fwUnknown},
		`-p all -j LOG --log-prefix "in: "`:                                                      {fwBlocked, fwBlocked},
		`-j lan`:                                                                                 {fwOpen, fwBlocked},
		`-g lan`:                                                                                 {fwOpen, fwBlocked},
		`-p udp -m udp --dport 50123 -j DNAT --to-destination 172.18.0.2:5000`:                   {fwUnknown, fwBlocked},
	} {
		dump := "-P INPUT DROP\n-P FORWARD ACCEPT\n-P OUTPUT ACCEPT\n-N lan\n-A INPUT " + rule + "\n-A lan -p udp -m udp --dport 50000:50999 -j ACCEPT\n"
		udp, tcp, _, _ := netfilterPorts(nfDump(dump, false), lanHome)
		if udp != want[0] || tcp != want[1] {
			t.Errorf("%s: udp %v tcp %v, want %v", rule, udp, tcp, want)
		}
	}
}

// ---- macOS ---------------------------------------------------------------

func TestMacFirewall(t *testing.T) {
	const (
		on       = "Firewall is enabled. (State = 1)\n"
		off      = "Firewall is disabled. (State = 0)\n"
		allOff   = "Firewall has block all state set to disabled.\n"
		allOn    = "Firewall has block all state set to enabled.\n"
		homebrew = "/opt/homebrew/Caskroom/fsend/1.12.0/fsend"
	)
	apps := fwSample(t, "macos/listapps.txt")
	if got := macFirewallApps(apps); len(got) != 4 || !got[homebrew] || !got["/Applications/Example Browser.app/Contents/Frameworks/Example Helper (Plugin).app"] {
		t.Errorf("app list: %v", got)
	}
	if got := macFirewallApps(fwSample(t, "macos/listapps.blocked.txt")); len(got) != 2 || got["/Users/me/Downloads/fsend"] || !got["/Users/me/.nvm/versions/node/v22.21.1/bin/node"] {
		t.Errorf("app list with a blocked app: %v", got)
	}
	// The layout older systems print.
	old := "ALF: total number of apps = 2 \n\n1 :  /Applications/Example.app \n\t ( Allow incoming connections ) \n\n2 :  /usr/local/bin/fsend \n\t ( Block incoming connections ) \n"
	if got := macFirewallApps(old); len(got) != 2 || !got["/Applications/Example.app"] || got["/usr/local/bin/fsend"] {
		t.Errorf("old app list: %v", got)
	}

	const (
		permitted = "Incoming connection to /tmp/build/fsend is permitted.\n"
		refused   = "Incoming connection to /tmp/build/fsend is blocked.\n"
	)
	for _, tc := range []struct {
		name                  string
		state, blockAll, apps string
		appBlocked            string
		exes                  []string
		answered              bool
		ok, note              bool
		want, detail          string
	}{
		{"off", off, "", "", "", nil, false, true, false, "macOS firewall is off", ""},
		{"block all", on, allOn, apps, permitted, []string{homebrew}, false, false, false,
			"blocks all incoming connections — devices on this network can't connect", "Block all incoming connections"},
		{"block all, as state 2", "Firewall is enabled. (State = 2)\n", allOff, apps, permitted, []string{homebrew}, true, false, false,
			"blocks all incoming connections", ""},
		{"allowed", on, allOff, apps, permitted, []string{homebrew}, true, true, false, "allows incoming connections to fsend", ""},
		// Started through a link: the list holds what it resolves to.
		{"allowed, via a link", on, allOff, apps, permitted, []string{"/opt/homebrew/bin/fsend", homebrew}, true, true, false,
			"allows incoming connections to fsend", ""},
		{"blocked", on, allOff, fwSample(t, "macos/listapps.blocked.txt"), refused, []string{"/Users/me/Downloads/fsend"}, false, false, false,
			"blocks incoming connections to fsend — devices", "allow fsend in System Settings"},
		// Not in the list by path. The firewall knows apps by their code
		// signature, so the listener self-test decides: it got through …
		{"not listed, listener answered", on, allOff, apps, permitted, []string{"/tmp/build/fsend"}, true, true, false,
			"allows incoming connections to fsend", "let the listener test through"},
		// … or it did not, and macOS is asking (or about to).
		{"not listed, no answer", on, allOff, apps, permitted, []string{"/tmp/build/fsend"}, false, false, true,
			"is on and has no entry for this fsend yet", "choose Allow, then run fsend doctor again"},
		{"not listed, empty list", on, allOff, "Total number of apps = 0 \n", permitted, []string{homebrew}, false, false, true,
			"has no entry for this fsend yet", ""},
		// The tool's "blocked" counts even without a list entry.
		{"not listed, blocked by signature", on, allOff, apps, refused, []string{"/tmp/build/fsend"}, false, false, false,
			"blocks incoming connections to fsend", ""},
	} {
		c := macFirewallCheck(tc.state, tc.blockAll, tc.appBlocked, tc.apps, tc.exes, tc.answered)
		if c.ok != tc.ok || c.note != tc.note || c.label != "firewall" || !strings.Contains(c.msg, tc.want) {
			t.Errorf("%s: got %+v, want ok=%v note=%v containing %q", tc.name, c, tc.ok, tc.note, tc.want)
		}
		detail := detailText(c)
		if !strings.Contains(detail, tc.detail) {
			t.Errorf("%s: detail %q, want it to contain %q", tc.name, detail, tc.detail)
		}
		if !c.ok && detail == "" {
			t.Errorf("%s: no word on what to do", tc.name)
		}
	}
}

// ---- Windows -------------------------------------------------------------

func TestWinActiveProfiles(t *testing.T) {
	all := fwSample(t, "windows/netsh-allprofiles.txt")
	if got := netshSections(all); len(got) != 3 || got[0] != "Domain Profile Settings:" || got[2] != "Public Profile Settings:" {
		t.Fatalf("sections of allprofiles: %q", got)
	}
	for _, tc := range []struct {
		current, all string
		want         int
	}{
		{"windows/netsh-currentprofile.private.txt", "windows/netsh-allprofiles.txt", winPrivate},
		{"windows/netsh-currentprofile.public.txt", "windows/netsh-allprofiles.txt", winPublic},
		{"windows/netsh-currentprofile.private-public.txt", "windows/netsh-allprofiles.txt", winPrivate | winPublic},
		// English titles are known even when allprofiles failed.
		{"windows/netsh-currentprofile.public.txt", "", winPublic},
		// Translated titles are placed by their position in allprofiles …
		{"windows/netsh-currentprofile.de-public.txt", "windows/netsh-allprofiles.de.txt", winPublic},
		// … and are unknown without it.
		{"windows/netsh-currentprofile.de-public.txt", "", 0},
		{"", "windows/netsh-allprofiles.txt", 0},
	} {
		if got := winActiveProfiles(fwSample(t, tc.current), fwSample(t, tc.all)); got != tc.want {
			t.Errorf("%s with %q: profiles %d, want %d", tc.current, tc.all, got, tc.want)
		}
	}
}

func TestWinProfileFrom(t *testing.T) {
	if p := winProfileFrom(nil, nil); !p.on || p.blockAll || p.allowInbound || p.quiet {
		t.Errorf("defaults: %+v", p)
	}
	local := map[string]uint32{"EnableFirewall": 0, "DoNotAllowExceptions": 1, "DefaultInboundAction": 0, "DisableNotifications": 1}
	if p := winProfileFrom(nil, local); p.on || !p.blockAll || !p.allowInbound || !p.quiet {
		t.Errorf("local values: %+v", p)
	}
	// Group Policy turns the firewall back on; what it leaves alone stays.
	if p := winProfileFrom(map[string]uint32{"EnableFirewall": 1, "DefaultInboundAction": 1}, local); !p.on || !p.blockAll || p.allowInbound {
		t.Errorf("policy over local: %+v", p)
	}
}

// The rules Windows writes when its prompt is answered, for a program at
// exe: one for TCP and one for UDP.
func winPromptRules(action, profile, exe string) []string {
	return []string{
		`v2.31|Action=` + action + `|Active=TRUE|Dir=In|Protocol=6|Profile=` + profile + `|App=` + exe + `|Name=fsend.exe|Desc=fsend.exe|Defer=User|`,
		`v2.31|Action=` + action + `|Active=TRUE|Dir=In|Protocol=17|Profile=` + profile + `|App=` + exe + `|Name=fsend.exe|Desc=fsend.exe|Defer=User|`,
	}
}

func TestParseWinRule(t *testing.T) {
	const exe = `C:\Users\me\AppData\Local\fsend\fsend.exe`
	r, ok := parseWinRule(winPromptRules("Allow", "Private", exe)[1])
	if !ok || !r.allow || r.profiles != winPrivate || r.proto != 17 || r.ports != coverAll || r.app != exe || r.narrowed {
		t.Errorf("the prompt's UDP rule: %+v %v", r, ok)
	}
	for rule, want := range map[string]struct {
		ok       bool
		profiles int
		ports    cover
		narrowed bool
	}{
		// No Profile= means every profile; no Protocol= means any.
		`v2.10|Action=Block|Active=TRUE|Dir=In|App=` + exe + `|Name=x|`:                                                 {true, 7, coverAll, false},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=6|Profile=Domain|Profile=Private|LPort2_10=50000-50999|Name=x|`: {true, winDomain | winPrivate, coverAll, false},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=17|LPort=50123|Name=x|`:                                         {true, 7, coverSome, false},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=6|LPort=80|LPort=443|Name=x|`:                                   {false, 7, coverNone, false},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=6|RA4=LocalSubnet|RA6=LocalSubnet|App=` + exe + `|Name=x|`:      {true, 7, coverAll, false},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=6|RA4=10.0.0.0/255.0.0.0|App=` + exe + `|Name=x|`:               {true, 7, coverAll, true},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=6|IFType=Wireless|App=` + exe + `|Name=x|`:                      {true, 7, coverAll, true},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=17|RPort=5353|App=` + exe + `|Name=x|`:                          {true, 7, coverAll, true},
		// Not about the listener: inactive, no Active= (false by the
		// grammar), outbound, a service's, a packaged app's, ICMP, IPsec.
		`v2.30|Action=Allow|Active=FALSE|Dir=In|Protocol=6|App=` + exe + `|Name=x|`:                                                {false, 7, coverAll, false},
		`v2.30|Action=Allow|Dir=In|Protocol=6|App=` + exe + `|Name=x|`:                                                             {false, 7, coverAll, false},
		`v2.30|Action=Allow|Active=TRUE|Dir=Out|Protocol=6|App=` + exe + `|Name=x|`:                                                {false, 7, coverAll, false},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=17|LPort=5353|App=C:\Windows\system32\svchost.exe|Svc=dnscache|Name=mDNS|`: {false, 0, coverNone, false},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=1|ICMP4=8:*|Name=ping|`:                                                    {false, 7, coverAll, true},
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=6|Security=Authenticate|App=` + exe + `|Name=x|`:                           {false, 0, coverAll, false},
		`not a rule`: {false, 0, 0, false},
	} {
		r, ok := parseWinRule(rule)
		if ok != want.ok {
			t.Errorf("%s: ok %v, want %v", rule, ok, want.ok)
			continue
		}
		if ok && (r.profiles != want.profiles || r.ports != want.ports || r.narrowed != want.narrowed) {
			t.Errorf("%s: %+v, want %+v", rule, r, want)
		}
	}
}

func TestWinFirewallCheck(t *testing.T) {
	const exe = `C:\Users\me\AppData\Local\fsend\fsend.exe`
	on := winProfileFrom(nil, nil)
	allOn := map[int]winProfile{winDomain: on, winPrivate: on, winPublic: on}
	with := func(p int, change func(*winProfile)) map[int]winProfile {
		m := map[int]winProfile{winDomain: on, winPrivate: on, winPublic: on}
		prof := m[p]
		change(&prof)
		m[p] = prof
		return m
	}
	rules := func(sets ...[]string) []string {
		var out []string
		for _, s := range sets {
			out = append(out, s...)
		}
		return out
	}
	// A rule for another program, and Windows' own mDNS rule.
	others := []string{
		`v2.31|Action=Block|Active=TRUE|Dir=In|Protocol=6|Profile=Public|App=C:\Program Files\Other\other.exe|Name=other|`,
		`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=17|LPort=5353|App=C:\Windows\system32\svchost.exe|Svc=dnscache|Name=mDNS|`,
	}
	for _, tc := range []struct {
		name     string
		fw       winFirewall
		ok, note bool
		want     string
		detail   string
	}{
		{"off everywhere", winFirewall{profiles: map[int]winProfile{winDomain: {}, winPrivate: {}, winPublic: {}}}, true, false, "Windows Firewall is off", ""},
		{"off on this network", winFirewall{profiles: with(winPrivate, func(p *winProfile) { p.on = false }), active: winPrivate}, true, false,
			"Windows Firewall is off (Private network)", ""},
		{"no rule yet", winFirewall{profiles: allOn, active: winPrivate, rules: others}, false, false,
			"Windows Firewall has no rule allowing fsend (Private network), so it blocks incoming connections to it — devices on this network can't connect",
			"Windows asks to allow fsend the first time it listens: choose Allow."},
		{"no rule, prompts off", winFirewall{profiles: with(winPublic, func(p *winProfile) { p.quiet = true }), active: winPublic}, false, false,
			"has no rule allowing fsend (Public network)", "Windows is set not to ask about new programs"},
		{"allowed by the prompt", winFirewall{profiles: allOn, active: winPrivate, rules: rules(others, winPromptRules("Allow", "Private", exe))}, true, false,
			"Windows Firewall allows incoming connections to fsend (Private network)", ""},
		// Paths compare without regard to case or slash.
		{"allowed, path written differently", winFirewall{profiles: allOn, active: winPrivate,
			rules: winPromptRules("Allow", "Private", `c:/users/ME/appdata/local/fsend/FSEND.EXE`)}, true, false, "allows incoming connections to fsend", ""},
		{"allowed on private, network is public", winFirewall{profiles: allOn, active: winPublic, rules: winPromptRules("Allow", "Private", exe)}, false, false,
			"Windows Firewall allows fsend on Private networks only, and this one isn't (Public network)", "set this network to Private"},
		{"prompt cancelled", winFirewall{profiles: allOn, active: winPublic, rules: winPromptRules("Block", "Public", exe)}, false, false,
			"Windows Firewall has a rule blocking fsend (Public network) — devices", "Allow an app through firewall"},
		// A block rule wins over an allow rule.
		{"blocked and allowed", winFirewall{profiles: allOn, active: winPublic,
			rules: rules(winPromptRules("Allow", "Public", exe), winPromptRules("Block", "Public", exe))}, false, false, "has a rule blocking fsend", ""},
		{"block all", winFirewall{profiles: with(winPublic, func(p *winProfile) { p.blockAll = true }), active: winPublic,
			rules: winPromptRules("Allow", "Public", exe)}, false, false, "Windows Firewall blocks all incoming connections (Public network)", "Windows Security"},
		{"inbound allowed by default", winFirewall{profiles: with(winPrivate, func(p *winProfile) { p.allowInbound = true }), active: winPrivate}, true, false,
			"allows incoming connections to fsend", ""},
		{"only UDP allowed", winFirewall{profiles: allOn, active: winPrivate, rules: winPromptRules("Allow", "Private", exe)[1:]}, false, false,
			"allows UDP to fsend but not TCP (Private network)", ""},
		{"only TCP allowed", winFirewall{profiles: allOn, active: winPrivate, rules: winPromptRules("Allow", "Private", exe)[:1]}, false, false,
			"allows TCP to fsend but not UDP (Private network)", ""},
		{"a port rule for any program", winFirewall{profiles: allOn, active: winPrivate, rules: []string{
			`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=6|LPort2_10=50000-50999|Name=fsend tcp|`,
			`v2.30|Action=Allow|Active=TRUE|Dir=In|Protocol=17|LPort2_10=50000-50999|Name=fsend udp|`}}, true, false, "allows incoming connections to fsend", ""},
		{"rules limited to some addresses", winFirewall{profiles: allOn, active: winPrivate, rules: []string{
			`v2.30|Action=Allow|Active=TRUE|Dir=In|RA4=10.0.0.0/255.0.0.0|App=` + exe + `|Name=x|`}}, false, true, "don't settle whether devices on this network get through", "wf.msc"},
		// The network type is unknown (netsh said nothing usable).
		{"unknown type, same answer everywhere", winFirewall{profiles: allOn, rules: winPromptRules("Allow", "Private", exe)[:0]}, false, false,
			"Windows Firewall has no rule allowing fsend, so it blocks", ""},
		{"unknown type, allowed on private only", winFirewall{profiles: allOn, rules: winPromptRules("Allow", "Private", exe)}, false, true,
			"treats fsend differently by network type — Domain networks: blocked; Private networks: allowed; Public networks: blocked", "Settings → Network & internet"},
		// On two kinds of network at once, with different answers.
		{"two types in use", winFirewall{profiles: allOn, active: winPrivate | winPublic, rules: winPromptRules("Allow", "Private", exe)}, false, true,
			"Private networks: allowed; Public networks: blocked", ""},
	} {
		c := winFirewallCheck(tc.fw, []string{exe})
		if c.ok != tc.ok || c.note != tc.note || c.label != "firewall" || !strings.Contains(c.msg, tc.want) {
			t.Errorf("%s: got ok=%v note=%v %q, want ok=%v note=%v containing %q", tc.name, c.ok, c.note, c.msg, tc.ok, tc.note, tc.want)
		}
		detail := detailText(c)
		if !strings.Contains(detail, tc.detail) {
			t.Errorf("%s: detail %q, want it to contain %q", tc.name, detail, tc.detail)
		}
		// Every row that isn't a pass says what to do, and every command
		// names this fsend.
		if !c.ok && detail == "" {
			t.Errorf("%s: no word on what to do", tc.name)
		}
		if strings.Contains(detail, "netsh advfirewall") && !strings.Contains(detail, `add rule name="fsend" dir=in action=allow program="`+exe+`" enable=yes`) {
			t.Errorf("%s: the command doesn't name this fsend: %q", tc.name, detail)
		}
	}
}

// ---- the grid --------------------------------------------------------------

// Tests run with stderr not on a terminal, so rows take their plain form:
// an ASCII marker, no escape sequences, and detail lines under the
// message column of their own row.
func TestPrintDoctorRow_PlainGrid(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var b bytes.Buffer
	printDoctorRow(&b, passRow("listener", "answered").with("first", "second"))
	printDoctorRow(&b, warnRow("firewall", "blocked — a long sentence stays on one line when nothing is there to wrap it for").with("to open them:").run("sudo  fix   --it"))
	printDoctorRow(&b, noteRow("firewall", "can't tell").with("ask root"))
	printDoctorRow(&b, passRow("NAT", "none"))
	want := "" +
		"[OK] listener answered\n" +
		"              first\n" +
		"              second\n" +
		"[!] firewall blocked — a long sentence stays on one line when nothing is there to wrap it for\n" +
		"             to open them:\n" +
		"             sudo  fix   --it\n" + // a command is printed as written
		"[i] firewall can't tell\n" +
		"             ask root\n" +
		"[OK] NAT      none\n"
	if b.String() != want {
		t.Errorf("rows:\n%s\nwant:\n%s", b.String(), want)
	}
	if strings.Contains(b.String(), "\x1b") {
		t.Error("an escape sequence in plain output")
	}
}

// A note is not a check: it isn't counted and can't fail the run.
func TestDoctorRows_Kinds(t *testing.T) {
	if c := passRow("a", "b"); !c.ok || c.note {
		t.Errorf("pass: %+v", c)
	}
	if c := warnRow("a", "b"); c.ok || c.note {
		t.Errorf("warn: %+v", c)
	}
	if c := noteRow("a", "b"); c.ok || !c.note {
		t.Errorf("note: %+v", c)
	}
	base := warnRow("a", "b").with("one")
	if more := base.run("two"); len(base.detail) != 1 || len(more.detail) != 2 || !more.detail[1].cmd || more.detail[0].cmd {
		t.Errorf("adding detail must not change the row it was called on: %v %v", base.detail, more.detail)
	}
}
