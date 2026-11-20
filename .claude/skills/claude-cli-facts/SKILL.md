---
name: claude-cli-facts
description: What has actually been observed about the Claude Code CLI's behaviour when Superdeck drives it: sign-in, Keychain, trust prompts, Remote Control output. Read before changing how Superdeck launches or interprets Claude, and add to it when you learn something.
---

# Observed Claude CLI behaviour

Seen on macOS with Claude Code 2.1.296 to 2.1.329. These are observations, not guarantees; re-check after a CLI update. Mark anything you have not seen yourself as unverified.

## Accounts and sign-in

- `CLAUDE_CONFIG_DIR` gives an account its own config, including its own `.claude.json`.
- On macOS the login is stored in the Keychain under a service name ending in the first 8 hex characters of the SHA-256 of the config directory's **path string**. Moving or renaming the directory orphans the login. This is why account directories never move and renaming only changes a display name.
- Keychain access works for a process started by a launchd LaunchAgent in the logged-in session.
- A sandboxed shell may not be able to read the Keychain and will report the account as signed out even when it is not. Trust what the running Superdeck reports.
- `claude auth status` prints JSON with `loggedIn`, `email`, `orgName` and exits non-zero when signed out.
- `claude auth login --claudeai` prints `If the browser didn't open, visit: <URL>` then waits at `Paste code here if prompted >`. The URL redirects to an Anthropic-hosted page that shows a code; no localhost callback is needed. Pasting the code and Enter completes the sign-in. It also tries to open a browser on the Mac.
- Anthropic's sign-in page reuses whatever Claude login the browser already has. Two Superdeck accounts ended up signed in as the same Claude user this way. A private window avoids it.
- A subscription token from `claude setup-token` cannot start Remote Control (documented, not tested here).

## Remote Control

- `claude remote-control` needs a terminal. It runs correctly inside a detached tmux session started by a launchd agent, including Keychain access.
- With one attached, an untrusted folder produces a prompt ending `Trust <path>? [y/N]` rather than exiting. Without a terminal it exits with `Workspace not trusted` (documented).
- Once trusted, the trust is remembered in that account's config and later launches go straight through.
- When connected it prints a block containing `Connected`, `Capacity: 1/4`, and `Continue coding in the Claude mobile app or https://claude.ai/code?environment=env_...`. Superdeck turns that link into the "Open in Claude" button. The environment id stayed the same across restarts.
- Signed out, it prints `Error: You must be logged in to use Remote Control.` and exits 1.
- `--spawn worktree` needs a Git repository. For a folder that is not one, Superdeck uses `--spawn same-dir`.
- Pass `--spawn` explicitly; without it a first run may ask interactively (reported by other projects, unverified here).
- Documented, not yet seen here: it exits by design after roughly ten minutes without network; Team and Enterprise plans need an owner to enable Remote Control.

## Other

- The CLI updates itself in the background. It went from 2.1.296 to 2.1.329 during one evening without Superdeck asking. A relaunched instance runs the new version.
- `claude update` checks for and installs an update. Superdeck runs it only when asked. Not yet run from Superdeck against the real CLI.
- An instance runs with its account's config directory, not `~/.claude`. Skills, MCP servers, settings and instructions kept in `~/.claude` are therefore **not** available to instances. Project-level `.claude` folders and `CLAUDE.md` in the folder still are.
