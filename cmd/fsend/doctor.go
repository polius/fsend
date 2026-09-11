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

	// mDNS: prove the full announce→query roundtrip works from this host —
	// the whole LAN path depends on multicast sockets opening and answers
	// coming back. A miss means LAN discovery will fall through to the
	// pairing server (or fail, if the server is also unreachable).
	ann, err := landisc.Announce(doctorCode, ip)
	if err != nil {
		say(false, "mDNS", "cannot open multicast sockets (%v) — LAN discovery unavailable", err)
		return
	}
	defer landisc.StopAnnounce(ann)

	// Let the announcer settle before asking: a query fired the instant
	// the group join completes is racy (observed on macOS/Wi-Fi). The
	// query itself is retried because multicast frames drop capriciously
	// on Wi-Fi.
	time.Sleep(250 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	qStart := time.Now()
	var res *landisc.QueryResult
	for attempt := 0; attempt < 3 && res == nil; attempt++ {
		r, err := landisc.Query(ctx, doctorCode, 1500*time.Millisecond)
		if err == nil {
			res = r
		}
	}
	if res == nil {
		say(false, "mDNS", "no answer after 3 queries — multicast may be blocked; LAN discovery falls back to the pairing server")
		return
	}
	say(true, "mDNS", "answered by %s in %s — LAN discovery works", res.IP, time.Since(qStart).Round(time.Millisecond))
}

const doctorHelpTemplate = `fsend doctor — check what a transfer depends on

Runs four read-only checks and reports each as ✓ (working) or ⚠
(degraded, with the consequence):

  config    which pairing server is configured (never prints the password)
  server    HTTPS reachability of that server's /health
  network   the local LAN address fsend would announce
  mDNS      a real announce→query roundtrip on this machine

Always exits 0. Every degraded check has a graceful fallback:
without a server, same-LAN transfers still work; without mDNS,
transfers use the pairing server.
`
