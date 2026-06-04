package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/polius/fsend/internal/config"
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

	args := f.connectArgsRaw

	// No args: print current + default.
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
		fmt.Fprintln(os.Stderr, "✓ Reverted to default server:", config.DefaultServer)
		return nil
	default:
		// args[0] is host:port, args[1] (optional) is password.
		host := args[0]
		password := ""
		if len(args) >= 2 {
			password = args[1]
		}
		if len(args) > 2 {
			return errors.New("--connect takes at most: <host:port> [password]")
		}
		cfg.Server = host
		cfg.ServerPassword = password
		now := time.Now().UTC()
		cfg.FirstRunAt = &now
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Fprintln(os.Stderr, "✓ Server set to:", host)
		return nil
	}
}

func printCurrentServer(cfg *config.Config) {
	fmt.Fprintln(os.Stderr)
	if cfg.IsDefault() {
		fmt.Fprintln(os.Stderr, "  Current server:", config.DefaultServer, "(default)")
	} else {
		fmt.Fprintln(os.Stderr, "  Current server:", cfg.Server, "(custom)")
		fmt.Fprintln(os.Stderr, "  Default server:", config.DefaultServer)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  To revert to the default:  fsend --connect default")
		fmt.Fprintln(os.Stderr, "  To set a new server:       fsend --connect <host:port> [password]")
	}
	fmt.Fprintln(os.Stderr)
}
