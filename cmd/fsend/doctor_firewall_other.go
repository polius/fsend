//go:build !darwin && !linux && !windows

package main

import "net"

// firewallCheck has no row on the systems whose firewall doctor doesn't
// know (the BSDs' pf and ipfw).
func firewallCheck(net.IP, bool) (doctorCheck, bool) { return doctorCheck{}, false }
