#!/usr/bin/env bash
# Runs a copy of release-guard.sh against a throwaway cmd/fleet/main.go, one
# arm per outcome the guard must tell apart.
set -euo pipefail

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/scripts" "$tmp/cmd/fleet"
cp "$(dirname "$0")/release-guard.sh" "$tmp/scripts/"

fails=0
arm() {
	local want=$1 name=$2 tag=$3 line=$4 rc=0
	printf 'package main\n\n%s\n' "$line" >"$tmp/cmd/fleet/main.go"
	"$tmp/scripts/release-guard.sh" "$tag" >/dev/null 2>&1 || rc=$?
	if [[ "$rc" != "$want" ]]; then
		echo "FAIL $name: exit $rc, want $want" >&2
		fails=$((fails + 1))
	fi
}

arm 0 "tag matches" v0.1.0 'const version = "0.1.0"'
arm 1 "tag differs" v0.1.1 'const version = "0.1.0"'
arm 1 "tag without v" 0.1.0 'const version = "0.1.0"'
arm 4 "version unreadable" v0.1.0 'var version = "0.1.0"'

if ((fails > 0)); then
	exit 1
fi
echo "release-guard: all arms pass"
