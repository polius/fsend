# Operations

**Status:** Locked
**Date:** 2026-06-03
**Scope:** What `fs.alzina.dev` is (and isn't) as a public service.

---

## What `fs.alzina.dev` is and isn't

**Is:** The default rendezvous + relay server that every fsend CLI uses
unless the user explicitly overrides via `--connect`. Operated by Pol
Alzina. Free for public use.

**Is not:** A guaranteed service. It has no SLA. It may go down for
maintenance, abuse, or simply because the operator chose to. Users who
need guaranteed availability should self-host.

This must be stated plainly in the README and the `--connect default`
output, so users have realistic expectations and a clear path to self-host.
