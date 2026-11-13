//go:build !darwin

package main

// firewallCheck has no portable way to ask the OS firewall about fsend
// outside macOS, so there is no row.
func firewallCheck() (doctorCheck, bool) { return doctorCheck{}, false }
