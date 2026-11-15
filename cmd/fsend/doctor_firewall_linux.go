package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	mrand "math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/net/ipv4"
)

// fwTool runs a firewall tool and returns what it printed, stdout and
// stderr together: the tools say "not running" or "you must be root" on
// either. found is false when the tool isn't installed. The search covers
// the sbin directories, which an unprivileged user's PATH often lacks.
// The C locale keeps the messages the parsers look for in English.
func fwTool(name string, args ...string) (out string, found bool, err error) {
	path, lookErr := exec.LookPath(name)
	if lookErr != nil {
		for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
			if st, statErr := os.Stat(filepath.Join(dir, name)); statErr == nil && !st.IsDir() {
				path, lookErr = filepath.Join(dir, name), nil
				break
			}
		}
	}
	if lookErr != nil {
		return "", false, lookErr
	}
	// firewall-cmd is a Python program talking to a daemon: seconds, on a
	// busy machine.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // G204: fixed tools; the one variable argument is an interface, zone or table name the system reported
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, err := cmd.CombinedOutput()
	return string(b), true, err
}

// firewallCheck reports whether this machine's firewall lets devices on
// the network reach fsend's listener. It runs the inbound test, then asks
// in turn the firewalls a Linux system is likely to run: ufw (its files),
// firewalld (its daemon), then the kernel's own rules (nft and iptables),
// which only root can read. The judging is in doctor_firewall_ufw.go,
// _firewalld.go and _netfilter.go. ok is false when there is nothing to
// ask and nothing was seen.
func firewallCheck(ip net.IP, _ bool) (doctorCheck, bool) {
	lan := lanNetFor(ip)
	probe := udpInboundProbe(lan)

	if conf, err := os.ReadFile("/etc/ufw/ufw.conf"); err == nil && ufwEnabled(string(conf)) {
		defaults, _ := os.ReadFile("/etc/default/ufw")
		name := "user.rules"
		if lan.v6() {
			name = "user6.rules"
		}
		// /etc/ufw since ufw 0.35; /lib/ufw before.
		rules, err := os.ReadFile(filepath.Join("/etc/ufw", name))
		if err != nil && os.IsNotExist(err) {
			rules, err = os.ReadFile(filepath.Join("/lib/ufw", name))
		}
		return ufwCheck(ufwInputPolicy(string(defaults)), string(rules), err == nil, lan, probe), true
	}

	if out, found, _ := fwTool("firewall-cmd", "--state"); found {
		if running, authorized := firewalldStatus(out); running {
			zone := ""
			if lan.iface != "" {
				out, _, _ := fwTool("firewall-cmd", "--get-zone-of-interface="+lan.iface)
				zone = firewalldZoneName(out)
			}
			if zone == "" {
				out, _, _ := fwTool("firewall-cmd", "--get-default-zone")
				zone = firewalldZoneName(out)
			}
			list, readable := "", false
			if authorized {
				args := []string{"--list-all"}
				if zone != "" {
					args = []string{"--zone=" + zone, "--list-all"}
				}
				out, _, err := fwTool("firewall-cmd", args...)
				list, readable = out, err == nil
			}
			return firewalldCheck(zone, list, readable, lan, probe), true
		}
	}

	rules, unread := readNetfilter(lan.v6())
	switch {
	case unread == nil:
		return netfilterCheck(rules, lan, probe), true
	case errors.Is(unread, errNoNetfilterTool) && probe.result == probeNone:
		return doctorCheck{}, false
	case errors.Is(unread, errNoNetfilterTool):
		return netfilterUnreadable("there is no nft or iptables here to read the kernel's rules with", "", probe), true
	case os.Geteuid() == 0:
		return netfilterUnreadable("nftables/iptables rules could not be read", "", probe), true
	}
	return netfilterUnreadable("nftables/iptables rules need root to read", "`sudo fsend doctor` reads them. ", probe), true
}

var (
	errNoNetfilterTool = errors.New("neither nft nor iptables is installed")
	errNetfilterUnread = errors.New("the kernel's rules could not be read")
)

// readNetfilter reads the kernel's firewall rules: nft's ruleset, and
// with iptables the tables nft doesn't show — those of the legacy
// backend, which the kernel lists in /proc/net/ip_tables_names — or, on a
// machine without nft, the filter table. Nothing is read that isn't
// loaded already: listing a legacy table that doesn't exist would load
// its kernel module. The error says why something on the inbound path
// could not be read.
func readNetfilter(v6 bool) (nfRules, error) {
	ipt, proc := "iptables", "/proc/net/ip_tables_names"
	if v6 {
		ipt, proc = "ip6tables", "/proc/net/ip6_tables_names"
	}
	rules := nfRules{iptables: map[string]string{}}

	nft, haveNft, err := fwTool("nft", "list", "ruleset")
	if haveNft && err != nil {
		return rules, errNetfilterUnread
	}
	rules.nft, rules.noNft = nft, !haveNft

	// Which backend the plain iptables command talks to, and the command
	// that reads the legacy one.
	version, haveIpt, _ := fwTool(ipt, "-V")
	legacyTool := ipt + "-legacy"
	if _, found, _ := fwTool(legacyTool, "-V"); !found {
		legacyTool = ""
		if haveIpt && !iptablesIsNft(version) {
			legacyTool = ipt
		}
	}
	if !haveNft && !haveIpt && legacyTool == "" {
		return rules, errNoNetfilterTool
	}
	if !haveNft && haveIpt && iptablesIsNft(version) {
		out, _, err := fwTool(ipt, "-S")
		if err != nil {
			return rules, errNetfilterUnread
		}
		rules.iptables["filter"] = out
	}

	names, err := os.ReadFile(proc)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return rules, nil // the legacy backend isn't loaded: it has no rules
	case haveNft:
		return rules, nil // root in a sandbox that hides /proc/net: go by nft
	default:
		return rules, errNetfilterUnread
	}
	for _, table := range legacyTablesToRead(string(names)) {
		if legacyTool == "" {
			return rules, errNetfilterUnread
		}
		out, _, err := fwTool(legacyTool, "-t", table, "-S")
		if err != nil {
			return rules, errNetfilterUnread
		}
		rules.iptables[table] = out
	}
	return rules, nil
}

// mdnsGroup is the multicast group of the inbound test: the mDNS one,
// which this machine is in already when anything on it does discovery.
var mdnsGroup = net.IPv4(224, 0, 0, 251)

// udpInboundProbe runs the inbound test (see probeResult) through the
// LAN interface: one datagram to a port of the listener's range, and one
// to the mDNS port for comparison — a firewall that filters by port lets
// mDNS in (ufw, firewalld's home and workstation zones) or drops both. A
// datagram that arrives got through the firewall; one that doesn't, while
// the mDNS one does, was dropped for its port. Anything else shows
// nothing: no multicast on the interface, or a firewall that drops both.
//
// Both go out on the network, like the mDNS check's queries: a few bytes,
// to a group address, with a TTL of 1.
func udpInboundProbe(lan lanNet) udpProbe {
	if lan.ip == nil || lan.v6() || lan.iface == "" {
		return udpProbe{}
	}
	ifc, err := net.InterfaceByName(lan.iface)
	if err != nil || ifc.Flags&net.FlagMulticast == 0 {
		return udpProbe{}
	}
	var test *net.UDPConn
	port := 0
	for range 8 { // a port another socket holds can't be bound: try another
		port = lanPortFirst + mrand.IntN(lanPortLast-lanPortFirst+1)
		if test, err = net.ListenMulticastUDP("udp4", ifc, &net.UDPAddr{IP: mdnsGroup, Port: port}); err == nil {
			break
		}
	}
	if err != nil {
		return udpProbe{}
	}
	defer func() { _ = test.Close() }()
	control, err := net.ListenMulticastUDP("udp4", ifc, &net.UDPAddr{IP: mdnsGroup, Port: 5353})
	if err == nil {
		defer func() { _ = control.Close() }()
	}

	out, err := net.ListenUDP("udp4", &net.UDPAddr{IP: lan.ip})
	if err != nil {
		return udpProbe{}
	}
	defer func() { _ = out.Close() }()
	pc := ipv4.NewPacketConn(out)
	if pc.SetMulticastInterface(ifc) != nil || pc.SetMulticastLoopback(true) != nil || pc.SetMulticastTTL(1) != nil {
		return udpProbe{}
	}
	// To the test port, random bytes; to the mDNS port, a DNS query with
	// no questions, which every responder ignores.
	token := make([]byte, 16)
	query := make([]byte, 12)
	if _, err := rand.Read(token); err != nil {
		return udpProbe{}
	}
	copy(query, token[:2])
	if _, err := out.WriteToUDP(token, &net.UDPAddr{IP: mdnsGroup, Port: port}); err != nil {
		return udpProbe{}
	}
	if control != nil {
		_, _ = out.WriteToUDP(query, &net.UDPAddr{IP: mdnsGroup, Port: 5353})
	}

	from, _ := out.LocalAddr().(*net.UDPAddr)
	// The kernel loops a multicast datagram back at once; the wait is for
	// a busy machine.
	arrived := func(c *net.UDPConn, payload []byte) bool {
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		buf := make([]byte, 64)
		for {
			n, src, err := c.ReadFromUDP(buf)
			if err != nil {
				return false
			}
			if from != nil && src.Port == from.Port && src.IP.Equal(lan.ip) && bytes.Equal(buf[:n], payload) {
				return true
			}
		}
	}
	switch {
	case arrived(test, token):
		return udpProbe{result: probeThrough, port: port}
	case control != nil && arrived(control, query):
		return udpProbe{result: probeDropped, port: port}
	}
	return udpProbe{}
}
