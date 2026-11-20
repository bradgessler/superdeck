# Superdeck

Start Claude Code Remote Control instances on a Mac from your phone, under more than one Claude account.

Superdeck is a small web page served by one Go binary on your Mac. You open it, see what is running, and launch a new instance by picking an account and a folder. The Mac runs Anthropic's own `claude remote-control` in that folder, and you carry on the conversation in the official Claude app. Superdeck is a launcher, not a chat client.

**Status: proof of concept.** It has been exercised against a stand-in for the Claude CLI and against the real CLI while signed out. It has not yet been run with a signed-in Claude account. It has no sign-in of its own and serves plain HTTP.

## What it does

- **Instances.** The home screen lists what is running. Open one to see its terminal output, stop it or relaunch it.
- **New instance.** Pick an account and a Git repository under your projects folder, or create a new folder. Sessions started from the Claude app each get their own Git worktree.
- **Accounts.** Each account label has its own Claude config directory (`CLAUDE_CONFIG_DIR`), so several accounts can be signed in side by side. Signing in runs Anthropic's `claude auth login`; you finish it on Anthropic's page and paste the code back.
- **Prompts.** When Claude asks something on first run (trust this folder, enable Remote Control), the instance is flagged and you answer with on-screen keys.
- **Staying up.** An instance that dies on its own is restarted with a growing delay. Instances you launched are remembered and brought back when Superdeck itself restarts.
- **Updates.** The home screen shows the installed Claude Code version and a "Check for updates" link that runs `claude update`. Nothing updates on its own. An instance still running an older version says so and offers Relaunch.

## Run it

```bash
go build -o superdeck .
```

```bash
./superdeck -root ~/Projects
```

Open `http://127.0.0.1:7433`.

To reach it from a phone on your LAN or home VPN, listen on the Mac's LAN address. There is no sign-in, so anything that can reach that address can use it; only do this on a network you trust.

```bash
./superdeck -addr 192.168.1.20:7433 -root ~/Projects
```

### Start at login

This prints a launchd agent for the settings you pass. It does not install anything.

```bash
./superdeck -addr 192.168.1.20:7433 -root ~/Projects -launch-agent > ~/Library/LaunchAgents/com.github.bradgessler.superdeck.plist
```

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.github.bradgessler.superdeck.plist
```

To remove it:

```bash
launchctl bootout gui/$(id -u)/com.github.bradgessler.superdeck
```

## Security model

Superdeck can start processes on your Mac. It has no sign-in. The model is: it can only do a short, fixed list of things, and you keep it reachable only from a private network.

The worst someone who reaches the page should be able to do is launch Claude instances in your project folders, start a sign-in, and stop or relaunch what is running.

- **Fixed commands.** It runs only `claude remote-control`, `claude auth login`, `claude auth status`, `claude update`, `claude --version` and `git init`, with arguments it builds itself and passes as a list, never through a shell.
- **No typing into Claude.** The page cannot send text to a process. It can press one of nine named keys (Enter, y, n, 1, 2, 3, Up, Down, Esc). The one exception is a sign-in, which accepts a single code made of letters, digits and `_.~#+/=-`.
- **Validated names.** Account names, new folder names, spawn mode and capacity are checked against strict patterns before they reach a command line.
- **Confined folders.** A project must sit inside the `-root` folder after symlinks are resolved.
- **No cross-site driving.** Every request must carry an expected `Host` header and every state-changing request must be same-origin, so a web page elsewhere cannot drive Superdeck through your browser.
- **Clean environment.** Child processes get an explicit environment. The launcher's own `CLAUDE_*` and `ANTHROPIC_*` variables are not passed on.
- **No shortcuts around Claude's safeguards.** It never passes `--dangerously-skip-permissions` or a permission-mode override, and never edits Claude's config to pre-trust a folder. It never reads or stores Claude credentials.

Not built: sign-in, TLS, log files.

## Open questions

"Observed" means seen on macOS with Claude Code 2.1.296.

| # | Question | Status |
|---|----------|--------|
| 1 | Can a Go process run `claude remote-control` under a pseudo-terminal and show it in a browser? | **Partly.** Observed for the signed-out path. The signed-in path has only been run against a stand-in. |
| 2 | Can an account be signed in from a phone, with no browser on the Mac? | **Looks possible.** Observed: `claude auth login` prints a URL that leads to an Anthropic page showing a code, then waits at `Paste code here if prompted >`. The full round trip has not been done. |
| 3 | Do two accounts stay separate when they run at the same time? | **Open.** Anthropic documents one `CLAUDE_CONFIG_DIR` per account with its own Keychain entry. Observed: each directory gets its own `.claude.json`. |
| 4 | With a pseudo-terminal attached, do the first-run prompts appear instead of the process exiting? And are the nine keys enough to answer them? | **Open.** |
| 5 | Does each instance show up in the Claude app under the right account, and only that account? | **Open.** |
| 6 | Does it all work when launchd starts it at login, including Keychain access? | **Open.** Other projects report stale Keychain entries under launchd. |
| 7 | After a network outage longer than ten minutes, when the server exits by design, does the automatic restart bring it back cleanly? | **Open.** Restart is built and tested against a stand-in. |
| 8 | Does `claude update` behave when run from here, and does a relaunch pick up the new version? | **Open.** |
| 9 | Can `--chrome` browser sessions run this way when the browser extension is signed in to a different account? | **Open.** Not built. |
| 10 | Is passing a sign-in code through this page within Anthropic's terms for third-party tools? | **Open.** Superdeck hands the code straight to the official binary and stores nothing. Read the [terms](https://code.claude.com/docs/en/legal-and-compliance) before relying on it. |

Known limits that are not going away:

- After a reboot with FileVault on, nothing runs until someone unlocks the Mac in person.
- On Team and Enterprise plans an owner has to turn Remote Control on before any of this works.

## Development

```bash
go test ./...
```

The browser terminal is [xterm.js](https://github.com/xtermjs/xterm.js) 5.5.0, vendored unmodified in `ui/vendor/xterm` under its MIT license.

## License

AGPL-3.0. If you change Superdeck and let other people use your version, including over a network, you must share your changes under the same license. See [LICENSE](LICENSE).
