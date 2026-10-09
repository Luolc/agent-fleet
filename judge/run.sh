#!/usr/bin/env bash
# Host side of the judge: the black-box end-to-end suite that decides whether
# a binary behaves as the fleet CLI must. It builds the image (cached: nothing
# in it depends on the binary), then runs inside.sh in a container with the
# binary mounted in. Needs docker.
#
# Usage: judge/run.sh <path/to/binary>
#
# Parameters, all environment variables with defaults for the Go binary:
#   JUDGE_NAME        the command name; the binary is mounted under it and
#                     its messages are expected to use it (default: fleet)
#   JUDGE_ENV_PREFIX  prefix of the identity variables (default: FLEET_,
#                     that is, JUDGE_NAME upper-cased plus an underscore)
#   JUDGE_LEDGER      file name of the ledger under ~/scratch/<repo>/
#                     (default: <JUDGE_NAME>.db)
#   JUDGE_IMAGE       use an image built elsewhere (CI) instead of building
#
# An earlier implementation of the same CLI runs under its own name:
#   JUDGE_NAME=<its name> judge/run.sh <path/to/its binary>
# The suite is the same; only these parameters differ.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
bin=${1:-}
[[ -n "$bin" ]] || { echo "usage: judge/run.sh <path/to/binary>" >&2; exit 2; }
[[ -x "$bin" ]] || { echo "no executable at $bin" >&2; exit 2; }
bin=$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")
name=${JUDGE_NAME:-fleet}
prefix=${JUDGE_ENV_PREFIX:-$(printf %s "$name" | tr '[:lower:]' '[:upper:]')_}
ledger=${JUDGE_LEDGER:-$name.db}
image=${JUDGE_IMAGE:-}
if [[ -z "$image" ]]; then
	# One tag per checkout, so worktrees running at the same time on one
	# machine do not overwrite each other's image.
	image=fleet-judge-$(printf %s "$here" | sha256sum | cut -c1-8)
	docker build -q -t "$image" "$here" >/dev/null
fi
docker run --rm \
	-e "JUDGE_NAME=$name" -e "JUDGE_ENV_PREFIX=$prefix" -e "JUDGE_LEDGER=$ledger" \
	-v "$bin:/home/agent/bin/$name:ro" -v "$here:/home/agent/judge:ro" \
	"$image" bash /home/agent/judge/inside.sh
