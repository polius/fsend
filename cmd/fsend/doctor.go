package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/landisc"
	"github.com/polius/fsend/internal/uxlog"
)

// doctorCmd implements `fsend doctor`: a read-only diagnostic covering the
// pieces a transfer depends on — config, pairing server, local address,
// mDNS roundtrip. It changes no state and always exits 0: every condition
// it reports degrades gracefully (LAN-only without a server, internet-only
// without mDNS), so its job is to explain the degradation, not gate on it.
func doctorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check config, server reachability, and LAN discovery",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("%w: doctor takes no positional arguments (got %q)", fserrors.ErrUsage, args[0])
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			runDoctor()
			return nil
		},
	}
	dht := boldHelpHeaders(doctorHelpTemplate)
	c.SetHelpTemplate(dht)
	c.SetUsageTemplate(dht)
	return c
}

// doctorCode drives the self-announce/query roundtrip. It deliberately
// falls outside the share-code alphabet ('o' is never generated), so the
// derived mDNS name can't collide with a real transfer's.
const doctorCode = "doctor-check"

func runDoctor() {
	say := func(ok bool, label, format string, args ...any) {
		glyph := uxlog.Warn()
		if ok {
			glyph = uxlog.Check()
		}
		fmt.Fprintf(os.Stderr, "%s %-8s %s\n", glyph, label, fmt.Sprintf(format, args...))
	}

	server := config.DefaultServer
	cfg, err := config.Load()
	switch {
	case err != nil:
		say(false, "config", "unreadable, using defaults (%v)", err)
	case cfg.IsDefault():
		say(true, "config", "server %s (default)", server)
	default:
		server = cfg.EffectiveServer()
		detail := "server " + server + " (custom)"
		if cfg.ServerPassword != "" {
			detail += ", password set"
		}
		say(true, "config", "%s", detail)
	}

	// Server reachability: /health is the one endpoint that never requires
	// the self-hosted password, so it works against any server.
	start := time.Now()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(serverBaseURL(server) + "/health")
	switch {
	case err != nil:
		say(false, "server", "%s unreachable (%v) — cross-network transfers will fail, LAN transfers unaffected", server, err)
	case resp.StatusCode != http.StatusOK:
		say(false, "server", "%s answered HTTP %d — cross-network transfers may fail, LAN transfers unaffected", server, resp.StatusCode)
	default:
		say(true, "server", "reachable in %s", time.Since(start).Round(time.Millisecond))
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	ip := landisc.PreferredLocalIP()
	if ip.IsLoopback() {
		say(false, "network", "no non-loopback address found — only same-machine transfers will work")
	} else {
		say(true, "network", "LAN address %s", ip)
	}

	// mDNS: prove the full announce→query roundtrip works from this host.
	// The check is two-tier so each outcome is deterministic and honest:
	//
	//   1. machinery — loopback multicast, which the OS never filters.
	//      A miss here means fsend's mDNS itself is broken on this host.
	//   2. LAN — the physical interface. macOS 15+ (Local Network
	//      Privacy) and some Wi-Fi APs silently drop or stall multicast
	//      there; when that happens, discovering *other* devices falls
	//      back to the pairing server (transfers still complete, and
	//      same-device transfers keep working via loopback).
	ann, err := landisc.Announce(doctorCode, ip)
	if err != nil {
		say(false, "mDNS", "cannot open multicast sockets (%v) — LAN discovery unavailable", err)
		return
	}
	defer landisc.StopAnnounce(ann)

	// Let the announcer settle before asking: a query fired the instant
	// the group join completes is racy (observed on macOS/Wi-Fi).
	time.Sleep(250 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	machStart := time.Now()
	if _, err := landisc.QueryLoopback(ctx, doctorCode, 500*time.Millisecond); err != nil {
		say(false, "mDNS", "roundtrip failed even on loopback (%v) — LAN discovery unavailable", err)
		return
	}
	say(true, "mDNS", "roundtrip ok in %s", time.Since(machStart).Round(time.Millisecond))

	// LAN tier: a query that only leaves via the physical interface. If
	// the OS or the AP blocks multicast there, this is the honest signal.
	lanStart := time.Now()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if res, err := landisc.QueryInterface(ctx2, doctorCode, 500*time.Millisecond, ip); err != nil {
		say(false, "mDNS", "no multicast answer via %s (%s) — discovering other devices falls back to the pairing server; check Local Network permission / Wi-Fi", ip, time.Since(lanStart).Round(time.Millisecond))
	} else {
		say(true, "mDNS", "LAN answer from %s in %s — discovering other devices on this network works", res.IP, time.Since(lanStart).Round(time.Millisecond))
	}
}

const doctorHelpTemplate = `fsend doctor — check what a transfer depends on

Runs four read-only checks and reports each as ✓ (working) or ⚠
(degraded, with the consequence):

  config    which pairing server is configured (never prints the password)
  server    HTTPS reachability of that server's /health
  network   the local LAN address fsend would announce
  mDNS      a real announce→query roundtrip on this machine, in two
            tiers: loopback (fsend's mDNS machinery) and the physical
            LAN interface (discovering other devices)

Always exits 0. Every degraded check has a graceful fallback:
without a server, same-LAN transfers still work; without mDNS,
transfers use the pairing server.
`
