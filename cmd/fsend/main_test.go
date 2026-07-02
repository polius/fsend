package main

import (
	"reflect"
	"strings"
	"testing"
)

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
		// Second positional is NOT glued: it stays a positional so the
		// dispatcher can flag `--connect host:port file.pdf` as a usage
		// error instead of silently saving "file.pdf" as the password.
		// For host+password use the explicit comma form below.
		{[]string{"fsend", "--connect", "host:443", "secret"}, []string{"fsend", "--connect=host:443", "secret"}},
		{[]string{"fsend", "--connect", "host:443", "/tmp/file.pdf"}, []string{"fsend", "--connect=host:443", "/tmp/file.pdf"}},
		// Comma form passes through untouched — pflag splits it.
		{[]string{"fsend", "--connect=host:443,secret"}, []string{"fsend", "--connect=host:443,secret"}},
		// Existing --connect=value form passes through untouched.
		{[]string{"fsend", "--connect=host:443"}, []string{"fsend", "--connect=host:443"}},
		// Unrelated flags pass through.
		{[]string{"fsend", "report.pdf"}, []string{"fsend", "report.pdf"}},
		// Stops at -- so a file named --connect survives as a positional.
		{[]string{"fsend", "--", "--connect", "f"}, []string{"fsend", "--", "--connect", "f"}},
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
		"":          "", // EOF → empty (and eof=true, covered in helpers_test)
		"\r\n":      "",
	}
	for in, want := range cases {
		if got, _ := readLine(strings.NewReader(in)); got != want {
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
