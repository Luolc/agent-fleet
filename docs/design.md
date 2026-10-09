# fleet design

## What this is and is not

A CLI that runs and coordinates coding agents in herdr; not an agent itself, not a service, not a Go library.

## Parts and how they connect

Only `cmd/fleet` exists so far; parts are added here as they are ported.

## Invariants

None yet. Each invariant links to the test that guards it, or is marked untested.

## Interfaces

None yet; this section will link to the CLI reference, `--json` output, and hooks.

## Known issues and next steps

Next: port the commands from an earlier tool one by one.

## Decision log

