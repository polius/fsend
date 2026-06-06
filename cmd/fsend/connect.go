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

// runConnect implements `fsend --connect ...` per PROJECT_SPEC.md
// "Server configuration":
//
//   fsend --connect                  → print current config + default
//   fsend --connect default          → revert to compiled-in default
//   fsend --connect <host:port>      → set custom server
//   fsend --connect <host:port> <pw> → set custom server + password
func runConnect(f *flags) error {
	cfg, _ := config.Load() // ignore corruption error — we're about to overwrite anyway

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
		printCurrentServer(cfg)
		return nil
	}

	switch args[0] {
	case "default":
		cfg.Server = ""
		cfg.ServerPassword = ""
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Fprintln(os.Stderr, uxlog.Check(), "Reverted to default server:", config.DefaultServer)
		return nil
	default:
		if len(args) > 2 {
			return fmt.Errorf("%w: --connect takes at most: <host:port> [password]", fserrors.ErrUsage)
		}
		host := args[0]
		if err := validateHostPort(host); err != nil {
			return fmt.Errorf("%w: %v", fserrors.ErrUsage, err)
		}
		password := ""
		if len(args) >= 2 {
			password = args[1]
		}
		cfg.Server = host
		cfg.ServerPassword = password
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		msg := "Server set to: " + host
		if password != "" {
			msg += "  (password set)"
		}
		fmt.Fprintln(os.Stderr, uxlog.Check(), msg)
		return nil
	}
}

// validateHostPort enforces "<host>:<port>" with a numeric port in range.
// Hostnames are not resolved (we don't want a DNS round-trip here);
// validating the shape catches "fsend --connect foo" before it persists
// junk into the config and bewilders the next transfer with E001.
func validateHostPort(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("expected <host>:<port>, got %q", s)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("host part is empty in %q", s)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("port must be 1-65535, got %q", port)
	}
	return nil
}

func printCurrentServer(cfg *config.Config) {
	fmt.Fprintln(os.Stderr)
	if cfg.IsDefault() {
		fmt.Fprintln(os.Stderr, "  Current server:", config.DefaultServer, "(default)")
	} else {
		extra := ""
		if cfg.ServerPassword != "" {
			extra = "  (password set)"
		}
		fmt.Fprintln(os.Stderr, "  Current server:", cfg.Server, "(custom)"+extra)
		fmt.Fprintln(os.Stderr, "  Default server:", config.DefaultServer)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  To revert to the default:  fsend --connect default")
		fmt.Fprintln(os.Stderr, "  To set a new server:       fsend --connect <host:port> [password]")
	}
	fmt.Fprintln(os.Stderr)
}
