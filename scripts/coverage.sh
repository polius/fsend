#!/usr/bin/env bash
# Runs unit and e2e tests with coverage and prints the merged total.
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

echo "==> unit tests"
# Exclude the e2e package from unit-test instrumentation — it runs the
# binary as a subprocess and has no unit-testable statements of its own.
unit_pkgs=$(go list ./... | grep -v '/test/e2e$' | paste -sd, -)
go test -coverpkg="$unit_pkgs" -coverprofile=unit_coverage.out \
    $(go list ./... | grep -v '/test/e2e$') >/dev/null

echo "==> e2e tests"
go test -cover -coverpkg=./... ./test/e2e/ >/dev/null

echo "==> merge"
{
    cat unit_coverage.out
    tail -n +2 e2e_coverage.out
} > merged_coverage.out

go tool cover -func=merged_coverage.out | tail -1
