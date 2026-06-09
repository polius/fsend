package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/uxlog"
)

// warnIfConfigCorrupted prints the E016 warning to stderr when
// config.Load reported a corrupt file. config.Load still returns a usable
// zero-value Config in that case, so the caller proceeds on defaults —
// this just makes the silent fallback visible. No-op when quiet or when
// err is nil/not a corruption error.
func warnIfConfigCorrupted(err error, quiet bool) {
	if err == nil || quiet {
		return
	}
	entry, ok := fserrors.Lookup(err)
	if !ok {
		return
	}
	fmt.Fprintf(os.Stderr, "%s [%s] %s\n", uxlog.Warn(), entry.Code, entry.Message)
	if entry.Action != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", entry.Action)
	}
}

// loadConfig loads the persisted config, surfacing the E016 corruption
// warning (unless quiet). Used by the send/receive flows, which need the
// configured server but should not fail just because the file is invalid.
func loadConfig(quiet bool) *config.Config {
	cfg, err := config.Load()
	warnIfConfigCorrupted(err, quiet)
	return cfg
}

// runConnect implements `fsend --connect ...`:
//
//	fsend --connect                       → print current config + default
//	fsend --connect default               → revert to compiled-in default
//	fsend --connect <host[:port]>         → set custom server (port optional)
//	fsend --connect <host[:port]>,<pw>    → set custom server + password
func runConnect(f *flags) error {
	// A set/revert overwrites the file, so corruption there is moot. But
	// the show path (no args) must warn — otherwise a corrupt config that
	// silently reverts to the default reads as "you're on the default".
	cfg, loadErr := config.Load()

	// Strip the bare-flag sentinel cobra synthesised when the user
	// typed `fsend --connect` with no value. After filtering, an empty
	// slice means "show current server".
	args := make([]string, 0, len(f.connectArgsRaw))
	for _, a := range f.connectArgsRaw {
		if a == connectShowSentinel {
			continue
		}
		args = append(args, a)
	}

	if len(args) == 0 {
		warnIfConfigCorrupted(loadErr, false)
		printCurrentServer(cfg)
		return nil
	}

	switch args[0] {
	case "default":
		// `default` reverts to the compiled-in server; it takes no
		// password. Reject a stray second element rather than silently
		// dropping it (the user would think they set a password).
		if len(args) > 1 {
			return fmt.Errorf("%w: --connect default takes no password", fserrors.ErrUsage)
		}
		if cfg.IsDefault() {
			fmt.Fprintln(os.Stderr, uxlog.Info(), "Already on the default server:", config.DefaultServer)
			return nil
		}
		cfg.Server = ""
		cfg.ServerPassword = ""
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Fprintln(os.Stderr, uxlog.Check(), "Reverted to default server:", config.DefaultServer)
		return nil
	default:
		if len(args) > 2 {
			return fmt.Errorf("%w: --connect takes at most: <host[:port]> [password]", fserrors.ErrUsage)
		}
		server, err := normalizeServer(args[0])
		if err != nil {
			return fmt.Errorf("%w: %v", fserrors.ErrUsage, err)
		}
		password := ""
		if len(args) >= 2 {
			password = args[1]
		}
		cfg.Server = server
		cfg.ServerPassword = password
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		msg := "Server set to " + server
		if password != "" {
			msg += " (password set)"
		}
		fmt.Fprintln(os.Stderr, uxlog.Check(), msg)
		return nil
	}
}

// normalizeServer validates a user-supplied server address and returns
// the canonical "<host>:<port>" form.
//
// Accepted shapes:
//   - "host:port"  → returned as-is (after port-range check)
//   - "host"       → port filled in implicitly: 443 for DNS hostnames,
//     80 for IP literals and "localhost"
//   - "http(s)://host[:port][/]" → scheme stripped, then treated as above
//
// The default mirrors web convention — domain names ride HTTPS on 443,
// while bare IPs / loopback ride HTTP on 80 (IPs rarely carry trusted
// TLS certs). Users on a non-default port still type it explicitly.
func normalizeServer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("server address is empty")
	}
	// Without scheme stripping, `--connect http://host:8080` (the form
	// the self-hosting LAN-only docs used to show) lands as the corrupt
	// `[http://host:8080]:443` and breaks every transfer with E001.
	s = strings.TrimSuffix(s, "/")
	if rest, ok := strings.CutPrefix(s, "http://"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "https://"); ok {
		s = rest
	}
	if s == "" {
		return "", fmt.Errorf("server address is empty")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		// No port supplied — treat the whole input as the host.
		// Strip optional IPv6 brackets so net.JoinHostPort re-adds them.
		host = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
		port = defaultPortForHost(host)
	} else {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return "", fmt.Errorf("port must be 1-65535, got %q", port)
		}
	}
	if strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("host part is empty in %q", s)
	}
	return net.JoinHostPort(host, port), nil
}

// defaultPortForHost returns the implicit port for an address where the
// user did not supply one. Loopback and IP literals default to HTTP/80;
// anything else (treated as a DNS name) defaults to HTTPS/443.
func defaultPortForHost(host string) string {
	if host == "localhost" || net.ParseIP(host) != nil {
		return "80"
	}
	return "443"
}

func printCurrentServer(cfg *config.Config) {
	fmt.Fprintln(os.Stderr)
	if cfg.IsDefault() {
		fmt.Fprintln(os.Stderr, "  Current server:", config.DefaultServer, "(default)")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  Set a custom server:  fsend --connect <host[:port]>[,<password>]")
	} else {
		tag := "custom"
		if cfg.ServerPassword != "" {
			tag = "custom, password set"
		}
		fmt.Fprintf(os.Stderr, "  Current server: %s  (%s)\n", cfg.Server, tag)
		fmt.Fprintln(os.Stderr, "  Default server:", config.DefaultServer)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  Revert to the default:  fsend --connect default")
		fmt.Fprintln(os.Stderr, "  Set a new server:       fsend --connect <host[:port]>[,<password>]")
	}
	fmt.Fprintln(os.Stderr)
}
