---
name: superdeck-go
description: Go conventions, process supervision rules and testing practice for Superdeck. Read before changing any .go file.
---

# Go in Superdeck

## Commands are built, never passed in

Superdeck's safety is that it can only run a short list of commands. Keep it that way.

- Every `exec.Command` takes an argument list built in Go from validated values. Never a shell, never a string from a request.
- The only commands are `claude remote-control`, `claude auth login`, `claude auth status`, `claude update`, `claude --version` and `git init`. Adding one is a security decision: say so and update the README.
- Validate before use: account names and folder names against the patterns in `paths.go`; folders through `resolveProject`, which resolves symlinks and checks the result is inside an allowed folder.
- The browser cannot type into a process. It can answer yes or no, press one of the named keys in `instances.go`, or submit a sign-in code matching `codeRe`. Do not widen this without being asked.
- Child processes get the explicit environment from `childEnv`. Never inherit `os.Environ()`.
- Anything a process prints is untrusted. It reaches a page only through `html.EscapeString` or `html/template`.

## Supervision

- One `Run` per process. Each lives in its own tmux session on a private tmux server (`-S <state>/tmux.sock`), not as a child of Superdeck, so it survives Superdeck restarting. Claude needs a terminal; plain pipes do not work.
- Superdeck never stops instances when it exits. On start, `adopt` reads the `runs` table and takes over whatever tmux is still holding; `restore` launches remembered instances that are not running.
- tmux is given the command as separate arguments after `--`, which it executes directly. Never build a command string for it. The process is started through `env -i` with exactly the environment from `childEnv`.
- State is read by polling tmux (`display-message` for dead/exit status, `capture-pane` for text). `remain-on-exit` keeps a finished pane long enough to read its exit status and output; when a pane dies tmux scrolls the top line away, so read with scrollback.
- A question is only a question while it is the last line on the screen, and its kind is read from that line alone: an answered question can still be visible above.
- The terminal is wide (200 columns) on purpose, so Claude never breaks a link or path across lines. The page wraps text.
- Restart rules live in `serverExited`. An instance that dies on its own is restarted with backoff. One that a person stopped, that failed within `minLife`, or that is waiting on sign-in or trust is not, because that would loop. Keep both halves tested.
- Launched instances are remembered in the `instances` table. Entries go through the same validation as a fresh launch.

## Data and migrations

- Superdeck's own data is one SQLite file, `superdeck.db`, through `store.go`. Claude's data stays in each account's config directory; never store credentials.
- **Never edit a migration that has shipped.** Change the schema by adding the next numbered file to `migrations/` with `-- +goose Up` and `-- +goose Down` sections. It is applied automatically at startup.
- A migration must be safe on real data: this is open source and other people's databases will run it unattended. Add columns with defaults; do not drop data without a copy.
- Startup order is fixed: open and migrate, then listen, then relaunch instances. A failed migration must stop startup.
- An account's `id` is its config directory name and never changes. Its `name` is for people.

## Conventions

- Standard library first. Current dependencies: `modernc.org/sqlite`, `pressly/goose`, and the `tmux` binary at run time.
- `gofmt`, `go vet`, and `go test -race ./...` clean before committing.
- Errors shown to a person are lower-case sentences without a full stop; `fail()` in `web.go` formats them.
- Comments explain why, especially for anything that looks odd.
- Timeouts and limits on anything that reads from the network or a process.

## Testing

- `fakeClaude` in `superdeck_test.go` is a shell script that stands in for the CLI. It is driven by marker files in the account's config directory. Tests need no Claude account, but they do need tmux, and each test gets its own tmux server.
- Test the refusal as well as the success: bad names, paths outside the allowed folders, keys not on the list.
- Tuning values that tests shorten (`minLife`, `restartBase`, `loginTimeout`, `bootRetry`) are package variables.
- When real Claude behaves differently from the stand-in, fix the stand-in first, then the code.
