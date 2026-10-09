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
  explicit directory is kept only because the tests use it. Text that
  mentioned `init` drops the mention.

## Names

- The command is `fleet`; the identity variables are `FLEET_AGENT`,
  `FLEET_ROLE`, `FLEET_PARENT`, `FLEET_REPO`, `FLEET_JOB`; the ledger file
  is `fleet.db` (its directory is in design.md); watch's override is
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
the same file. Two modules that export one name get distinct Go names:
`status`'s `herdr_agents` (the map `status` and `watch` read) is
`HerdrAgents`, `spawn`'s (the raw array `close` reads) is `HerdrAgentList`.
Module-private items (`plan`, `reach_input_box`) are unexported; a private
name that would collide with a command's exported name carries the command
name (`spawn`'s `plan` is `planSpawn`). Go-only scaffolding that no source
module has (the flag helpers) lives in `internal/cliargs`.

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
  (clap `global = true`), a flag given twice refused, a required flag (a
  non-`Option` `#[arg(long)]`, `spawn --task-file`) missing refused with
  clap's "required arguments were not provided" list, and the implicit
  `help [COMMAND]` subcommand (the named command's help; an unknown name is
  an unrecognized subcommand). Help (`-h`, `--help`) prints to stdout and
  exits 0; every other parse error exits 1 (the source maps clap's exit 2 to
  1) and prints clap's frame to stderr: `error: <message>`, the usage line,
  `For more information, try '--help'.`. The message is clap's for what
  `cliargs` detects (a missing positional, an unexpected argument, an
  unknown subcommand, a repeated flag) and `flag`'s own for what `flag`
  detects (an unknown flag, a bad value); the words differ there. The help
  text is clap's `about` and `long_about` verbatim, kept as constants next
  to the command (`SendAbout`, `SendLongAbout`); the layout (usage line,
  Arguments, Options) is assembled in `cmd/fleet` and approximates clap's,
  it is not byte-identical.
- serde to `encoding/json`: field order is declaration order with `json`
  tags in snake_case; `Option` fields are pointers so they serialize as
  `null`; a `Vec` serializes as `[]` when empty, so slices are initialized,
  never nil. Pretty output is a `json.Encoder` with `SetIndent("", "  ")` and
  `SetEscapeHTML(false)` (serde does not escape `<`, `>`, `&`). Untyped
  `serde_json::Value` lookups never decode into a struct (struct decoding
  matches keys case-insensitively): `herdr.Lookup` reads the exact key with
  numbers kept as `json.Number`, and array elements are type-asserted one
  at a time, so a missing key, a wrong type and a stray element are skipped
  exactly where the source skips them. A `Value` printed with `{}` is
  `herdr.Display`: compact, object keys sorted, as serde's Display.
- rusqlite to `modernc.org/sqlite` through `database/sql`: one `*sql.DB` with
  `SetMaxOpenConns(1)`, so the pragmas and the transaction state belong to one
  connection as with one rusqlite connection. The DSN carries the source's
  settings: `_pragma=busy_timeout(5000)` (5 s), `_pragma=journal_mode(WAL)`,
  and `_txlock=immediate`, so every `BeginTx` is `BEGIN IMMEDIATE` (the source
  opens its only transaction with `TransactionBehavior::Immediate`).
  `?1`-style parameters are kept; SQLite binds them by index. NULL-able
  columns scan into `sql.Null*`. `.optional()` on a query is `sql.ErrNoRows`
  read as `None`; the count `execute` returns is `RowsAffected`, its error
  `exit.Database` as any other.
- `std::process::Command` to `os/exec`: `exec.CommandContext` with a 60 s
  deadline per herdr call, `WaitDelay` 5 s, `Setpgid`, and an explicit `Env`
  built from an allow-list of the caller's environment (`PATH`, `HOME`,
  `USER`, `LOGNAME`, `TMPDIR`, `TERM`, `LANG`, `LC_*`, `XDG_*`, `HERDR_*`).
  stdout and stderr are captured separately. When the call returns, for any
  reason, the process group is killed: nothing herdr starts for a call may
  outlive it. A call that hits the deadline is exit 5 and the message names
  only the operation (`herdr agent prompt timed out ...`), never an argument,
  which may be a message body. A `WaitDelay` that expires after herdr itself
  exited is not an error: what herdr printed is the reply.
- Function size: `?` becomes an `if err != nil` branch each, so a long
  source function can exceed the linter's complexity cap (gocognit 20).
  Such a function is split at the source's own seams (the checks before
  anything is created, a closure, a polling loop) into unexported helpers
  in the same file; the order of operations and every message are
  unchanged. `spawn`'s `run` is `spawnChecks`, `spawnCreate` and `Spawn`;
  `close`'s is `closeChecks` and `closeWorktree` (the checks and the
  worktree lookup), `removeWorktree` (the worktree and branch) and `Close`.
- `Result::ok()`, which discards the error, discards the Go error the same
  way (`close` reading the worktree's branch).
- Closures: a function that takes `impl FnMut` arguments
  (`reach_input_box`) takes `func` values; the test's scripted closures are
  the same funcs.
- `std::process::Command` for `git`: `exec.Command` as the source runs it,
  with the full environment and no deadline; stdout and stderr captured
  separately. Only herdr calls get the bounded runner above. A raw
  `Command::new("herdr")` outside `Herdr` (`spawn`'s `pane_text`) goes
  through `herdr.Run`, the exported form of the bounded runner, so it has
  the same deadline, environment and process-group kill.
- Paths and the machine: `fs::canonicalize` is `filepath.EvalSymlinks` then
  `filepath.Abs`; `Path::exists` is `os.Stat` without an error (false on any
  error, as the source); `Path::starts_with` compares components, so it is
  `pathStartsWith` in `close`, not `strings.HasPrefix` (`/w/item-10` does not
  start with `/w/item-1`); `thread::available_parallelism` is
  `runtime.NumCPU` (marked `TODO(port)`: the source also honors a cgroup
  CPU quota). An `f64` printed with `{}` is `strconv.FormatFloat(x, 'f',
  -1, 64)`: no exponent, no trailing zeros, `2` for `2.0`.
- Native error text: where the source embeds a Rust error's `Display` (an
  `io::Error`, a rusqlite error, a serde error, an `ExitStatus`), the Go form
  embeds Go's error text in the same position and keeps the words the source
  puts around it verbatim, odd ones included (`cannot read <path>: `,
  `database: `, and the no-JSON message's `(exit exit status: N)`, where the
  source prints the status's own Display after the word `exit`). Only the
  wrapped text differs; it is not listed per message.
- Strings: `trim()` is `strings.TrimSpace`, `trim_start()` is
  `strings.TrimLeftFunc(s, unicode.IsSpace)`, `chars().count()` is
  `utf8.RuneCountInString`, `{:<width$}` is `%-*s` (both pad by runes).
  Rust's `lines()` ends a line at `\n` or `\r\n`; a Go port of it trims the
  `\r`. `String::from_utf8_lossy` is `string(bytes)` (see Deviations).
- Collections and hashing: a `BTreeSet<String>` that is only built and
  compared is a sorted slice without duplicates (`slices.Sort`, then
  `slices.Compact`; `slices.Equal`, `slices.Contains`). A hand-written
  FNV-1a 64 is `hash/fnv`'s `New64a`, printed with `%016x`, so a screen
  hash the earlier implementation wrote to the ledger reads as unchanged.
- Printing: `println!` writes to `os.Stdout`, `eprintln!` to `os.Stderr`,
  directly, as the source does. Process-level tests re-run the test binary
  (`TestMain` with `FLEET_TEST_RUN_MAIN=1`) against a fake `herdr` script on
  `PATH`, with an explicit environment and `HOME` under `t.TempDir()`. The
  fake is generated per case with its reply and file paths baked in: the
  environment allow-list above hides any test variable from it.

## Deviations

- Every herdr call has a deadline, and the process group is killed when the
  call returns (above); the source waits forever and leaves descendants.
- Invalid UTF-8 passes through everywhere: a file or stdin body (the source
  refuses the file with exit 1 and fails on stdin with exit 5), herdr's
  stdout and stderr and the screen (the source replaces the bad bytes with
  U+FFFD).
- `encoding/json` escapes U+2028 and U+2029 in strings; serde writes them
  literally.
- Help and usage-error layout approximates clap's, and `flag`'s own
  messages are kept where `flag` detects the error (above).
- Native error text is Go's where the source embeds Rust's (above).
