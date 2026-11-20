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

- One `Run` per process, attached to a pseudo-terminal (`creack/pty`). Claude needs one; plain pipes do not work.
- The terminal is wide (200 columns) on purpose, so Claude never breaks a link or path across lines. The page wraps text.
- Restart rules live in `serverExited`. An instance that dies on its own is restarted with backoff. One that a person stopped, that failed within `minLife`, or that is waiting on sign-in or trust is not, because that would loop. Keep both halves tested.
- Third-party parsing of process output (the terminal emulator) is wrapped in `recover`. A panic there degrades one process to plain text; it must never take Superdeck down, because every instance dies with it.
- Launched instances are remembered in `instances.json` and relaunched by `restore`. Entries go through the same validation as a fresh launch.

## Conventions

- Standard library first. Current dependencies: `creack/pty`, `hinshun/vt10x`.
- `gofmt`, `go vet`, and `go test -race ./...` clean before committing.
- Errors shown to a person are lower-case sentences without a full stop; `fail()` in `web.go` formats them.
- Comments explain why, especially for anything that looks odd.
- Timeouts and limits on anything that reads from the network or a process.

## Testing

- `fakeClaude` in `superdeck_test.go` is a shell script that stands in for the CLI. It is driven by marker files in the account's config directory. Tests need no Claude account.
- Test the refusal as well as the success: bad names, paths outside the allowed folders, keys not on the list.
- Tuning values that tests shorten (`minLife`, `restartBase`, `loginTimeout`, `bootRetry`) are package variables.
- When real Claude behaves differently from the stand-in, fix the stand-in first, then the code.
