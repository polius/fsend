package iceconn

import (
	"net"
	"testing"
)

func TestHostInterface(t *testing.T) {
	for name, want := range map[string]bool{
		"en0": true, "eth0": true, "wlan0": true, "lo0": true,
		"utun3": false, "tailscale0": false, "wg0": false, "docker0": false,
		"br-1a2b": false, "veth12": false, "vmnet8": false, "awdl0": false,
	} {
		if got := hostInterface(name); got != want {
			t.Errorf("hostInterface(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRemoteIP(t *testing.T) {
	for addr, want := range map[string]bool{
		"203.0.113.7":     true,
		"192.168.1.20":    true, // same-LAN peers pair over private addresses
		"10.0.0.5":        true,
		"2001:db8::1":     true,
		"127.0.0.1":       false,
		"::1":             false,
		"0.0.0.0":         false,
		"169.254.169.254": false, // cloud metadata
		"fe80::1":         false,
		"fd00:ec2::254":   false,
		"224.0.0.251":     false,
	} {
		if got := remoteIP(net.ParseIP(addr)); got != want {
			t.Errorf("remoteIP(%s) = %v, want %v", addr, got, want)
		}
	}
}

// A hostile pairing server can't aim our connectivity checks at this
// host, a metadata service, or a name we'd have to resolve.
func TestAddRemoteCandidate_RefusesUnsafeAddresses(t *testing.T) {
	a, err := New(Options{LocalUfrag: "Luuuu", LocalPwd: "lppppppppppppppppppppppp", RemoteUfrag: "Ruuuu", RemotePwd: "rppppppppppppppppppppppp"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, cand := range []string{
		"candidate:1 1 udp 2130706431 127.0.0.1 50000 typ host",
		"candidate:1 1 udp 2130706431 169.254.169.254 80 typ host",
		"candidate:1 1 udp 2130706431 fsend-peer.local 50000 typ host",
	} {
		if err := a.AddRemoteCandidate(cand); err == nil {
			t.Errorf("AddRemoteCandidate(%q) = %v, want a refusal", cand, err)
		}
	}
	if err := a.AddRemoteCandidate("candidate:1 1 udp 2130706431 192.168.1.20 50000 typ host"); err != nil {
		t.Errorf("private LAN candidate refused: %v", err)
	}
}
