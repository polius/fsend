package main

import "testing"

// TestValidateRelayAddr covers the B5 SSRF-ish guard on the server-chosen
// relay address: the same host as the signaling server is always trusted (the
// normal single-box deploy, whose server host may itself be loopback/LAN),
// but a redirect to any other non-public host is refused.
func TestValidateRelayAddr(t *testing.T) {
	cases := []struct {
		name       string
		relayAddr  string
		serverHost string
		wantErr    bool
	}{
		// Trusted: relay host equals the signaling host, whatever it is.
		{"same_public", "fs.example.com:443", "fs.example.com", false},
		{"same_loopback", "127.0.0.1:443", "127.0.0.1", false},
		{"same_lan", "192.168.1.10:443", "192.168.1.10", false},
		{"same_host_case_insensitive", "FS.Example.com:443", "fs.example.com", false},

		// Different public host: allowed.
		{"different_public_ip", "203.0.113.7:443", "fs.example.com", false},
		{"different_public_name_mismatch", "relay.example.com:443", "fs.example.com", true},

		// Different internal hosts: refused.
		{"metadata", "169.254.169.254:443", "fs.example.com", true},
		{"loopback_redirect", "127.0.0.1:443", "fs.example.com", true},
		{"rfc1918", "10.0.0.5:443", "fs.example.com", true},
		{"ipv6_ula", "[fd00::1]:443", "fs.example.com", true},
		{"unspecified", "0.0.0.0:443", "fs.example.com", true},
		{"multicast", "224.0.0.1:443", "fs.example.com", true},
		{"private_with_empty_server", "10.0.0.5:443", "", true},

		// Malformed.
		{"no_port", "fs.example.com", "fs.example.com", true},
		{"empty_host", ":443", "fs.example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRelayAddr(tc.relayAddr, tc.serverHost)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateRelayAddr(%q, %q) err = %v, wantErr = %v",
					tc.relayAddr, tc.serverHost, err, tc.wantErr)
			}
		})
	}
}
