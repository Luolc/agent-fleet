#!/usr/bin/env bash
# Runs every check CI runs, except the race test and the leak scan.
# FLEET_CHECK_CORES=N limits Go to N cores for local runs on a shared machine.
set -euo pipefail
cd "$(dirname "$0")/.."

export GOTOOLCHAIN=local
if [[ -n "${FLEET_CHECK_CORES:-}" ]]; then
	export GOMAXPROCS="$FLEET_CHECK_CORES" GOFLAGS="-p=$FLEET_CHECK_CORES"
fi

step() { printf '==> %s\n' "$*"; }

step gofmt
unformatted=$(gofmt -l .)
if [[ -n "$unformatted" ]]; then
	printf 'gofmt needed:\n%s\n' "$unformatted" >&2
	exit 1
fi

step go vet
go vet ./...

step golangci-lint
go tool -modfile=tools/golangci-lint.mod golangci-lint run ./...

step go test
go test ./...

step govulncheck
go tool -modfile=tools/govulncheck.mod govulncheck ./...

step go build
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /dev/null ./cmd/fleet

step docs/design.md line cap
scripts/check-design-cap.sh

step ok
