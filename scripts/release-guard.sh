#!/usr/bin/env bash
# Usage: release-guard.sh <tag>
# Passes (exit 0) only when the tag is v plus the version in
# cmd/fleet/main.go of the checkout. Exits 1 when they differ and 4 when the
# version cannot be read.
set -euo pipefail
cd "$(dirname "$0")/.."

tag=$1
version=$(sed -n 's/^const version = "\(.*\)"$/\1/p' cmd/fleet/main.go)
if [[ -z "$version" ]]; then
	echo "release-guard: no const version in cmd/fleet/main.go" >&2
	exit 4
fi
if [[ "$tag" != "v$version" ]]; then
	echo "release-guard: tag $tag is not v$version" >&2
	exit 1
fi
echo "release-guard: tag $tag is v$version"
