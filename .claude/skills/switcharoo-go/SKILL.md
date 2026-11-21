---
name: switcharoo-go
description: Go conventions, process supervision rules and testing practice for Switcharoo. Read before changing any .go file.
---

# Go in Switcharoo

## Commands are built, never passed in

Switcharoo's safety is that it can only run a short list of commands. Keep it that way.

- Every `exec.Command` takes an argument list built in Go from validated values. Never a shell, never a string from a request.
- The only commands are `claude remote-control`, `claude auth login`, `claude auth status`, `claude auth logout`, `claude update`, `claude --version`, `git init` and `git commit` (one empty first commit in a folder it has just made). Adding one is a security decision: say so and update the README.
- Validate before use: account names and folder names against the patterns in `paths.go`; folders through `resolveProject`, which resolves symlinks and checks the result is inside an allowed folder, and `offered`, which checks it is one Switcharoo lists.
- The browser cannot type into a process. It can answer yes or no, press one of the named keys in `instances.go`, or submit a sign-in code matching `codeRe`. Do not widen this without being asked.
- Child processes get the explicit environment from `childEnv`. Never inherit `os.Environ()`.
- Anything a process prints is untrusted. It reaches a page only through `html.EscapeString` or `html/template`.

## Supervision

- One `Run` per process. Each is kept alive by a **holder** (`hold.go`): this same binary run as `switcharoo hold`, in its own session, so it survives Switcharoo restarting. The holder owns the pseudo-terminal (`creack/pty`); Claude needs one, plain pipes do not work.
- **The holder must stay dumb and must never stall.** It keeps raw output and answers a socket. It does not parse output. No write to a client happens while its lock is held, and every client has a deadline or a bounded queue; a slow viewer is dropped, never waited for.
- **Holders outlive Switcharoo across upgrades.** The socket protocol may only grow: add verbs and reply fields, never change the meaning of existing ones. Bump `holdProtocol` when you add something a client might depend on.
- A signal the holder ignores is inherited by Claude. The holder catches and drops hang-ups instead, so Claude still dies if its holder does.
- **Never signal a process whose holder is alive.** `Run.unreachable` ends a run only on an exit record, or when the holder process itself is gone (checked by process id *and* command line, since ids are reused). A timeout proves nothing, and neither does a refused connection: a frozen holder with a full queue refuses too. An unreachable-but-alive holder is shown as "not responding" and left alone indefinitely. Killing a healthy Claude because a holder was slow is the worst thing Switcharoo can do.
- A process whose holder really has died is an orphan: it is ended (TERM, then KILL) and the run is not over until it is gone, so a replacement never starts beside it.
- Switcharoo never stops instances when it exits. On start, `adopt` reads the `runs` table and takes over whatever is still held; a holder with no record is stopped; `restore` launches remembered instances that are not running. If the record cannot be read, nothing is launched.
- A run that Switcharoo cannot record must not be left running (`onStart` returning an error cancels the launch).
- **An instance a person launched is only forgotten by a person.** `serverExited` restarts one that died with capped backoff for as long as it takes; there is no giving up. The exceptions: a signed-out account is not retried but is kept, and `bringBack` starts it when the account signs in; a folder the person declined to trust is forgotten; and an instance that has never run for `minLife` (not `proven`) is dropped after its first failure. A queued restart re-checks everything when it comes due.
- `restore` and `bringBack` keep retrying a remembered instance that cannot start yet.
- Everything that starts or stops instances in a folder holds `folderLock` for that folder, and acts on `current(run)`: a page or tab may be holding a run that has since been replaced.
- Move and relaunch check the target can start before stopping anything (`canStart`). A stop that does not complete is taken back (`unstop`) so the run stays supervised.
- `Run.read` is serialised. Two readers would each replay the same output into the emulator.
- `Manager.Start` reserves the run under the lock and does the slow part without it. Do not hold `Manager.mu` across anything that waits.
- The terminal emulator (`hinshun/vt10x`) is third-party code parsing arbitrary output, so every call into it is wrapped in `recover`. A panic degrades one process to plain text.
- The terminal is wide (200 columns) on purpose, so Claude never breaks a link or path across lines. The page wraps text.
- A question is only a question while it is the last line on the screen, and its kind is read from that line alone: an answered question can still be visible above.
- Launched instances are remembered in the `instances` table. Entries go through the same validation as a fresh launch, including that the folder is one Switcharoo lists.

- **Only a clear no counts as dead.** A check that could not be made (`ps` or `pgrep` would not run, a database read failed) is never read as "the holder is gone" or "this instance never worked". Each such check fails towards leaving the process and its record alone (`holderAlive`, `someoneHolds`, `Store.Proven`).
- **A page may be stale.** Stop, Move, Relaunch and answers act on the instance as it is now (`current`, which also finds a fresh launch of the same account and folder), and an answer is typed only if the page showed the question Claude is asking now.

- **Being reachable comes first.** The owner uses this from far away; a Switcharoo that will not start or will not answer cannot be fixed from a phone. Startup must not die over one bad record (`adoptOne`), a stuck Switcharoo restarts itself (`watchdog`, `/health`), and the Host check must not refuse the owner because the Mac's address changed (`hostOK`). Restarting Switcharoo is always safe; treat it as the remedy of first resort and keep it that way.
- **A process id read from the database is only a hint.** After a reboot it may belong to something else. Check what the process is (`holderAlive`, `commandHas`) before believing it, and never signal on the id alone.

## Data and migrations

- Switcharoo's own data is one SQLite file, `switcharoo.db`, through `store.go`. Claude's data stays in each account's config directory; never store credentials.
- **Never edit a migration that has shipped.** Change the schema by adding the next numbered file to `migrations/` with `-- +goose Up` and `-- +goose Down` sections. It is applied automatically at startup.
- A migration must be safe on real data: this is open source and other people's databases will run it unattended. Add columns with defaults; do not drop data without a copy.
- Startup order is fixed: open and migrate, then listen, then relaunch instances. A failed migration must stop startup.
- An account's `id` is its config directory name and never changes. Its `name` is for people.

## Conventions

- Standard library first. Current dependencies: `creack/pty`, `hinshun/vt10x`, `modernc.org/sqlite`, `pressly/goose`, `golang.org/x/term`. No external programs beyond `claude` and `git`.
- `gofmt`, `go vet`, and `go test -race ./...` clean before committing.
- Errors shown to a person are lower-case sentences without a full stop; `fail()` in `web.go` formats them.
- Comments explain why, especially for anything that looks odd.
- Timeouts and limits on anything that reads from the network or a process.

## Testing

- `fakeClaude` in `switcharoo_test.go` is a shell script that stands in for the CLI. It is driven by marker files in the account's config directory. Tests need no Claude account. The test binary doubles as the holder (see `TestMain`).
- After a full run, no `switcharoo.test hold` process and no stand-in process may be left behind. Check with `pgrep`; a leak is a bug.
- Every failure found in review has a test that reproduces it. Keep doing that.
- Test the refusal as well as the success: bad names, paths outside the allowed folders, keys not on the list.
- Tuning values that tests shorten (`minLife`, `restartBase`, `loginTimeout`, `bootRetry`) are package variables.
- When real Claude behaves differently from the stand-in, fix the stand-in first, then the code.
