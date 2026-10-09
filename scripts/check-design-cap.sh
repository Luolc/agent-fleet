#!/usr/bin/env bash
# Fails when docs/design.md grows past its line cap.
set -euo pipefail
cd "$(dirname "$0")/.."

limit=200
lines=$(wc -l < docs/design.md)
if (( lines > limit )); then
	echo "docs/design.md has $lines lines; the cap is $limit" >&2
	exit 1
fi
