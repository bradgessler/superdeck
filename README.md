# Superdeck

Start Claude Code Remote Control servers on a Mac from your phone, under more than one Claude account.

Superdeck is a small web page served by one Go binary on your Mac. From it you pick a project and an account and press Start. The Mac runs Anthropic's own `claude remote-control` in that folder, and you carry on the conversation in the official Claude app. Superdeck is a launcher, not a chat client.

**Status: proof of concept.** It exists to answer the open questions below. It does not restart crashed servers, has no launchd installer and serves plain HTTP. Do not rely on it yet.

## What it does today

- Lists Git repositories under one projects folder, and can create a new one.
- Keeps one Claude config directory per account label (`CLAUDE_CONFIG_DIR`), so several accounts can be signed in side by side.
- Runs `claude auth login` and `claude remote-control` under a pseudo-terminal and shows that terminal in the browser, so you can answer Claude's own prompts (trust this folder, enable Remote Control, paste a sign-in code).
- Flags a server that is waiting on a trust decision, a consent prompt or a sign-in.

## Run it

```bash
go build -o superdeck .
```

```bash
./superdeck -root ~/Projects
```

Open `http://127.0.0.1:7433` and sign in with the token in `~/.superdeck/token`.

To reach it from a phone on your LAN or home VPN, listen on the Mac's LAN address:

```bash
./superdeck -addr 192.168.1.20:7433 -root ~/Projects
```

Then, in the page: add an account label, press **Sign in** and complete Anthropic's sign-in, pick a project and press **Start**.

## Open questions

These are what the proof of concept is for. "Observed" means seen on macOS with Claude Code 2.1.296.

| # | Question | Status |
|---|----------|--------|
| 1 | Can a Go process run `claude remote-control` under a pseudo-terminal and stream it to a browser? | **Partly.** The process starts and its output and exit code reach the browser. Only the signed-out path has been run. |
| 2 | Can an account be signed in from a phone, with no browser on the Mac? | **Looks possible.** `claude auth login` prints a URL whose redirect is an Anthropic-hosted page that shows a code, then waits at `Paste code here if prompted >`. The full round trip has not been done. |
| 3 | Do two accounts stay separate when they run at the same time? | **Open.** Anthropic documents one `CLAUDE_CONFIG_DIR` per account with its own Keychain entry. Observed: each directory gets its own `.claude.json`. Two signed-in accounts running together is untested. |
| 4 | With a pseudo-terminal attached, do the first-run prompts (trust, Remote Control consent, spawn mode) appear in the console instead of the process exiting? | **Open.** Needs a signed-in account. |
| 5 | Does each server show up in the Claude app under the right account, and only that account? | **Open.** |
| 6 | Does it all still work when started by launchd at login, including Keychain access? | **Open.** Other projects report stale Keychain entries under launchd. |
| 7 | What happens after a network outage longer than ten minutes, when the server exits by design? | **Open.** There is no automatic restart yet. |
| 8 | Is plain HTTP acceptable on a home VPN? | **Open.** The session cookie is not marked Secure. TLS is not built. |
| 9 | Can `--chrome` browser sessions run this way when the browser extension is signed in to a different account? | **Open.** |
| 10 | Is passing a sign-in code through this console within Anthropic's terms for third-party tools? | **Open.** Superdeck never reads or stores credentials; the code goes straight to the official binary's terminal. Read the [terms](https://code.claude.com/docs/en/legal-and-compliance) before relying on it. |

Known limits that are not going away:

- After a reboot with FileVault on, nothing runs until someone unlocks the Mac in person.
- On Team and Enterprise plans an owner has to turn Remote Control on before any of this works.

## Security model

Superdeck can start processes on your Mac, so treat it as a remote control for the machine.

- It only starts two fixed commands, `claude auth login` and `claude remote-control`, with arguments it builds itself. There is no endpoint that takes a command.
- Projects are confined to the `-root` folder after symlinks are resolved.
- Every request must carry an allowed `Host` header; every state-changing request must be same-origin; everything except the sign-in page needs the session cookie.
- Child processes get an explicit environment. The launcher's own `CLAUDE_*` and `ANTHROPIC_*` variables are not passed on.
- It never passes `--dangerously-skip-permissions` or any permission-mode override, and never edits Claude's config to pre-trust a folder.
- The console accepts keystrokes for the process it is showing. Anyone signed in to Superdeck can type into those Claude prompts.

Not yet built: TLS, sign-in rate limiting beyond one guess per second, log files and redaction, automatic restart, launchd install.

## Development

```bash
go test ./...
```

The browser terminal is [xterm.js](https://github.com/xtermjs/xterm.js) 5.5.0, vendored unmodified in `ui/vendor/xterm` under its MIT license.

## License

AGPL-3.0. If you change Superdeck and let other people use your version, including over a network, you must share your changes under the same license. See [LICENSE](LICENSE).
