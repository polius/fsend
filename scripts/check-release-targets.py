#!/usr/bin/env python3
"""Assert that the release matrix in scripts/install.sh matches .goreleaser.yml.

install.sh pre-checks os/arch pairs so users fail fast on unsupported
targets instead of a download 404. This script keeps that list from
drifting out of sync with what the release actually builds. Exits
non-zero on any mismatch.
"""

import re
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent


def expected_targets() -> set:
    cfg = yaml.safe_load((ROOT / ".goreleaser.yml").read_text())
    build = cfg["builds"][0]
    goarm = [str(a) for a in build.get("goarm", [])]
    ignore = {(i["goos"], i["goarch"]) for i in build.get("ignore", [])}
    targets = set()
    for goos in build["goos"]:
        for goarch in build["goarch"]:
            if (goos, goarch) in ignore:
                continue
            if goarch == "arm":
                # The archive name template renders arm as armv{{.Arm}}.
                targets.update(f"{goos}-armv{v}" for v in goarm)
            else:
                targets.add(f"{goos}-{goarch}")
    return targets


def declared_targets() -> set:
    text = (ROOT / "scripts" / "install.sh").read_text()
    block = re.search(
        r"# release-matrix:begin(.*?)# release-matrix:end", text, re.S
    ).group(1)
    targets = set()
    for line in block.splitlines():
        line = line.strip()
        if not line or line.startswith(("#", "case", "*)", "esac")):
            continue
        m = re.match(r"^((?:[a-z0-9-]+\|)*[a-z0-9-]+)\)", line)
        if m:
            targets.update(m.group(1).split("|"))
    return targets


def main() -> int:
    expected = expected_targets()
    declared = declared_targets()
    ok = True
    for missing in sorted(expected - declared):
        print(f"built by goreleaser but missing in install.sh: {missing}")
        ok = False
    for extra in sorted(declared - expected):
        print(f"listed in install.sh but not built by goreleaser: {extra}")
        ok = False
    if ok:
        print(f"release matrix in sync ({len(expected)} targets)")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
