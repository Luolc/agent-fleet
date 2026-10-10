#!/usr/bin/env bash
# Fails unless the judge's shards in CI run every suite once: the words of
# the matrix in .github/workflows/ci.yml are the suites of `all` in
# judge/inside.sh. A suite missing from the matrix would never run in CI.
set -euo pipefail
cd "$(dirname "$0")/.."

words() { tr ' ,' '\n\n' | sed '/^$/d' | sort; }
suites=$(sed -n 's/^all="\(.*\)"$/\1/p' judge/inside.sh | words)
shards=$(sed -n 's/^ *suites: \[\(.*\)\]$/\1/p' .github/workflows/ci.yml | words)
if [[ -z "$suites" || "$suites" != "$shards" ]]; then
	echo "the judge's shards in .github/workflows/ci.yml run [${shards//$'\n'/ }];" \
		"judge/inside.sh has the suites [${suites//$'\n'/ }], each to run once" >&2
	exit 1
fi
