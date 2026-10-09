# Porting rulebook

How the earlier implementation (Rust) is ported to this repo, one batch at a
time. Every batch follows this page; a batch that needs a new rule adds it
here in the same PR. The page is deleted when the port is complete.

## Posture

- One-to-one: same module structure, same behavior, same messages. No
  improvements, no cleanups, no new checks. Defects of the source are
  reproduced and marked in a comment `BUG(port): <what, how to reproduce>`.
- When a construct has no obvious Go form, take the most conservative
  translation and mark it `TODO(port): <question>`.
- A deliberate deviation exists only if a line in this page says so. The
  list of deviations is the "Deviations" section below; nothing else.
- The source is read, never edited, and never named: this page and the code
  say "the earlier implementation". No path or repository name of the source
  appears anywhere in this repo.
- `init` is not ported. What only `init` used is dropped: the `dataset`
  table's reader and writer (the table itself stays, so the schema and its
  version marker are identical), and the ledger-path helper that takes an
  explicit home directory is kept only because the tests use it. Text that
  mentioned `init` drops the mention.

## Names

- The command is `fleet`; the identity variables are `FLEET_AGENT`,
  `FLEET_ROLE`, `FLEET_PARENT`, `FLEET_REPO`, `FLEET_JOB`; the ledger is
  `~/scratch/<repo name>/fleet.db`; watch's override is
  `FLEET_WATCH_STALE_SECS`. Every message, help text and error that named the
  earlier tool or its variables uses these names instead; nothing else in the
  wording changes.
- The version string is the `version` constant in `cmd/fleet`; it is not
  taken from the source.

## Dependencies

Standard library plus `modernc.org/sqlite` (pure Go, so `CGO_ENABLED=0`
builds stay static). Nothing else; a batch that seems to need another
dependency stops and asks.

## File mapping

| Source | Here |
|---|---|
| `main.rs`, `cli.rs` | `cmd/fleet/main.go` |
| `exit.rs` | `internal/exit/exit.go` |
| `db.rs` | `internal/db/db.go` |
| `identity.rs` | `internal/identity/identity.go` |
| `herdr.rs` | `internal/herdr/herdr.go` |
| `cmd/mod.rs` | `internal/cmd/doc.go` (the package comment) |
| `cmd/<name>.rs` | `internal/cmd/<name>.go`, all in package `cmd` |
| `cmd/<name>.rs` `#[cfg(test)]` | `internal/cmd/<name>_test.go` |
| `tests/cli.rs` | `cmd/fleet/main_test.go` |
| `tests/<name>.rs` | `cmd/fleet/<name>_test.go` |

One Rust module is one Go file. The command modules share one package, so
each one's `Args` and `run` carry the command name: `SendArgs`, `Send`,
`StatusArgs`, `Status`. Items a command module exported for another command
(`with_header`, `deliver`, `live_rows`, ...) are exported Go identifiers in
the same file. Go-only scaffolding that no source module has (the flag
helpers) lives in `internal/cliargs`.

Doc comments are ported with the code: `//!` becomes the package or file
comment, `///` the item's doc comment.

## Constructs

- Errors: `Result<T, Failure>` is `(T, error)` where the error is always a
  `*exit.Failure{Code, Message}`. `Exit` is `exit.Code` with the same six
  constants and values. `?` on a rusqlite error is `exit.Database(err)`, on an
  `io::Error` it is `exit.IO(err)`; both are exit 5 with the same message
  prefix as the source. A command returns `(exit.Code, error)`; `main` prints
  `fleet: <message>` to stderr and exits with the failure's code. An error
  that is not a `*exit.Failure` cannot occur; `main` treats one as exit 5.
- `Option<T>` is a pointer, or `sql.Null*` for a ledger column; an
  `Option<String>` flag is `cliargs.OptString`, which records whether the
  flag was given, because `--job ""` and no `--job` differ in the source.
- Enums without data are `type X int` with constants and `String()`; enums
  with data (`Reply`, `PromptOutcome`) are a struct with a kind field and
  the data fields the variants carry.
- clap to `flag`: `main` dispatches on `os.Args[1]`; each command has its own
  `flag.FlagSet`. clap behaviors that `flag` lacks are reproduced in
  `internal/cliargs`: flags after positionals (`send <to> --file x`), `--`
  ending the flags, `--session` accepted both before and after the command
  (clap `global = true`). Help (`-h`, `--help`) prints to stdout and exits 0;
  every other parse error prints clap's wording (`error: ...`, the usage line,
  `For more information, try '--help'.`) to stderr and exits 1, as the source
  maps clap's exit 2 to 1. clap's implicit `help` subcommand exists too. The
  help text is clap's `about` and `long_about` verbatim, kept as constants
  next to the command (`SendAbout`, `SendLongAbout`); the layout (usage
  line, Arguments, Options) is assembled in `cmd/fleet` and approximates
  clap's, it is not byte-identical.
- serde to `encoding/json`: field order is declaration order with `json`
  tags in snake_case; `Option` fields are pointers so they serialize as
  `null`; a `Vec` serializes as `[]` when empty, so slices are initialized,
  never nil. Pretty output is a `json.Encoder` with `SetIndent("", "  ")` and
  `SetEscapeHTML(false)` (serde does not escape `<`, `>`, `&`). Untyped
  `serde_json::Value` lookups decode with `UseNumber` and type-assert, so a
  missing key and a wrong type are skipped exactly where the source skips
  them.
- rusqlite to `modernc.org/sqlite` through `database/sql`: one `*sql.DB` with
  `SetMaxOpenConns(1)`, so the pragmas and the transaction state belong to one
  connection as with one rusqlite connection. The DSN carries the source's
  settings: `_pragma=busy_timeout(5000)` (5 s), `_pragma=journal_mode(WAL)`,
  and `_txlock=immediate`, so every `BeginTx` is `BEGIN IMMEDIATE` (the source
  opens its only transaction with `TransactionBehavior::Immediate`).
  `?1`-style parameters are kept; SQLite binds them by index. NULL-able
  columns scan into `sql.Null*`.
- `std::process::Command` to `os/exec`: `exec.CommandContext` with a 60 s
  deadline per herdr call, `WaitDelay` 5 s, `Setpgid` and a `Cancel` that
  kills the process group, and an explicit `Env` built from an allow-list of
  the caller's environment (`PATH`, `HOME`, `USER`, `LOGNAME`, `TMPDIR`,
  `TERM`, `LANG`, `LC_*`, `XDG_*`, `HERDR_*`). stdout and stderr are captured
  separately. A call that hits the deadline is exit 5.
- Strings: `trim()` is `strings.TrimSpace`, `trim_start()` is
  `strings.TrimLeftFunc(s, unicode.IsSpace)`, `chars().count()` is
  `utf8.RuneCountInString`, `{:<width$}` is `%-*s` (both pad by runes).
  `String::from_utf8_lossy` is `string(bytes)`: Go strings carry the bytes
  through unchanged.
- Printing: `println!` writes to `os.Stdout`, `eprintln!` to `os.Stderr`,
  directly, as the source does. Process-level tests re-run the test binary
  (`TestMain` with `FLEET_TEST_RUN_MAIN=1`) against a fake `herdr` script on
  `PATH`, with an explicit environment and `HOME` under `t.TempDir()`. The
  fake is generated per case with its reply and file paths baked in: the
  environment allow-list above hides any test variable from it.

## Deviations

- Every herdr call has a deadline (above); the source waits forever.
- A file or stdin body that is not valid UTF-8 is passed through; the source
  refuses the file (exit 1) or fails on stdin (exit 5).
- Help and usage-error layout approximates clap's (above).
