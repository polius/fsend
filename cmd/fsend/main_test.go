package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizePassArgs(t *testing.T) {
	cases := []struct {
		name     string
		in, want []string
	}{
		{
			name: "bare flag at end keeps NoOptDefVal sentinel firing",
			in:   []string{"fsend", "report.pdf", "--pass"},
			want: []string{"fsend", "report.pdf", "--pass"},
		},
		{
			name: "bare flag followed by another flag stays bare",
			in:   []string{"fsend", "--pass", "--quiet", "report.pdf"},
			want: []string{"fsend", "--pass", "--quiet", "report.pdf"},
		},
		{
			name: "value glued onto the flag with =",
			in:   []string{"fsend", "--pass", "swordfish", "report.pdf"},
			want: []string{"fsend", "--pass=swordfish", "report.pdf"},
		},
		{
			name: "existing --pass=value form passes through untouched",
			in:   []string{"fsend", "--pass=swordfish", "report.pdf"},
			want: []string{"fsend", "--pass=swordfish", "report.pdf"},
		},
		{
			name: "trailing --pass value with no following positional",
			in:   []string{"fsend", "report.pdf", "--pass", "swordfish"},
			want: []string{"fsend", "report.pdf", "--pass=swordfish"},
		},
		{
			name: "unrelated flags pass through",
			in:   []string{"fsend", "report.pdf"},
			want: []string{"fsend", "report.pdf"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizePassArgs(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("normalizePassArgs(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeConnectArgs(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		// Bare flag at end: keep so NoOptDefVal fires.
		{[]string{"fsend", "--connect"}, []string{"fsend", "--connect"}},
		// Bare flag followed by another flag: still bare.
		{[]string{"fsend", "--connect", "--quiet"}, []string{"fsend", "--connect", "--quiet"}},
		// One value glued onto the flag.
		{[]string{"fsend", "--connect", "default"}, []string{"fsend", "--connect=default"}},
		// host:port + password get comma-joined for StringSliceVar.
		{[]string{"fsend", "--connect", "host:443", "secret"}, []string{"fsend", "--connect=host:443,secret"}},
		// Existing --connect=value form passes through untouched.
		{[]string{"fsend", "--connect=host:443"}, []string{"fsend", "--connect=host:443"}},
		// Unrelated flags pass through.
		{[]string{"fsend", "report.pdf"}, []string{"fsend", "report.pdf"}},
	}
	for _, c := range cases {
		got := normalizeConnectArgs(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("normalizeConnectArgs(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestReadLine(t *testing.T) {
	cases := map[string]string{
		"y\n":       "y",
		"Y\n":       "y",
		"  Yes  \n": "yes",
		"\n":        "",
		"":          "", // EOF → empty
		"\r\n":      "",
	}
	for in, want := range cases {
		if got := readLine(strings.NewReader(in)); got != want {
			t.Errorf("readLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractDetail_StripsSentinelPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"usage error: --send and --receive are mutually exclusive", "--send and --receive are mutually exclusive"},
		{"source not found: nonexistent.txt", "nonexistent.txt"},
		// No ": " boundary → empty (no detail to surface).
		{"plain error with no colon", ""},
	}
	for _, c := range cases {
		got := extractDetail(c.in)
		if got != c.want {
			t.Errorf("extractDetail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
