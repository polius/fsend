#!/usr/bin/env bash
# Runs unit and e2e tests with coverage and prints per-package and total
# coverage. With --gate, fails if a security-critical package drops below
# its floor (FLOORS below; CI runs this).
#
# Unit tests use Go's classic -coverprofile output. The e2e suite builds
# fsend with -cover (see test/e2e/harness_test.go) and writes its
# profile to e2e_coverage.out on shutdown. Both are "set" mode by
# default and can be concatenated — the second mode header is stripped.
#
# Outputs (gitignored, *.out):
#   unit_coverage.out      — unit-test coverage profile
#   e2e_coverage.out       — e2e-binary coverage profile
#   merged_coverage.out    — both, ready for `go tool cover`

set -euo pipefail

cd "$(dirname "$0")/.."

# Floors sit a few points under the measured merged coverage, so timing-
# dependent e2e paths don't flake the gate. Raise them as coverage grows.
FLOORS="internal/transfer=77 internal/wire=85 internal/quicconn=80 internal/pake=88"

gate=0
case "${1:-}" in
    --gate) gate=1 ;;
    "") ;;
    *) echo "usage: $0 [--gate]" >&2; exit 2 ;;
esac

echo "==> unit tests" >&2
# Exclude the e2e package from unit-test instrumentation — it runs the
# binary as a subprocess and has no unit-testable statements of its own.
unit_pkgs=$(go list ./... | grep -v '/test/e2e$' | paste -sd, -)
# shellcheck disable=SC2046  # package list is intentionally word-split
go test -count=1 -coverpkg="$unit_pkgs" -coverprofile=unit_coverage.out \
    $(go list ./... | grep -v '/test/e2e$') >/dev/null

echo "==> e2e tests" >&2
# -count=1 disables the test cache. Cached test results skip TestMain,
# which is where the harness converts covdata to e2e_coverage.out — so
# without this, a second invocation produces no e2e profile.
go test -count=1 -timeout 600s -cover -coverpkg=./... ./test/e2e/ >/dev/null

echo "==> merge" >&2
{
    cat unit_coverage.out
    tail -n +2 e2e_coverage.out
} > merged_coverage.out

# Per-package statement coverage. A block counts once even when both
# profiles list it, and is covered if either run hit it.
module=$(go list -m)
LC_ALL=C awk -v mod="$module/" '
    NR > 1 {
        split($1, loc, ":"); pkg = loc[1]; sub(/\/[^\/]*$/, "", pkg); sub("^" mod, "", pkg)
        stmts[$1] = $2; owner[$1] = pkg
        if ($3 > 0) hit[$1] = 1
    }
    END {
        for (b in stmts) { total[owner[b]] += stmts[b]; if (b in hit) cov[owner[b]] += stmts[b] }
        for (p in total) printf "%-24s %5.1f%%\n", p, 100 * cov[p] / total[p]
    }' merged_coverage.out | sort > coverage_by_package.out
cat coverage_by_package.out
go tool cover -func=merged_coverage.out | tail -1

[ "$gate" = 1 ] || exit 0
status=0
for floor in $FLOORS; do
    pkg=${floor%=*} min=${floor#*=}
    got=$(awk -v p="$pkg" '$1 == p { sub(/%/, "", $2); print $2 }' coverage_by_package.out)
    if [ -z "$got" ] || LC_ALL=C awk -v g="$got" -v m="$min" 'BEGIN { exit !(g < m) }'; then
        echo "coverage gate: $pkg at ${got:-?}% is below its ${min}% floor" >&2
        status=1
    fi
done
exit "$status"
