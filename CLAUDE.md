# Superdeck

Superdeck is a small web application, one Go binary, that launches Anthropic's `claude remote-control` on a Mac under one of several Claude accounts. You pick an account and a folder; the conversation itself happens in the official Claude app. Superdeck is a launcher and supervisor. It is not a chat client and must not become one.

Read the README for what it does and its security model. This file is how to work on it.

## Principles

These came from the person who owns this project. Treat them as requirements.

1. **Render on the server.** Go templates produce every screen. The browser runs as little as possible. See the `superdeck-browser-code` skill before adding any script.
2. **One job per screen, and less on it.** See the `superdeck-ui` skill before touching templates or CSS.
3. **Drive Claude on the server; show purpose-built controls.** Do not show a raw terminal where a button or a sentence will do. The terminal has its own screen for when it is needed.
4. **Restrict what can be run, not who can reach it.** There is no sign-in. Superdeck is meant to be reachable only on a private network. Its safety comes from running a short fixed list of commands with arguments it builds itself. Do not add authentication unless asked. Do add validation.
5. **Use Anthropic's binary as it is.** Never pass `--dangerously-skip-permissions` or a permission-mode override, never edit Claude's config to pre-trust a folder, never read, copy or store Claude credentials. Sign-in happens on Anthropic's page.
6. **Survive failure.** Anything that can crash should be supervised or contained. See "What happens when things go wrong" in the README and keep it true.
7. **Few dependencies, well-known ones.** Prefer the standard library, then a battle-tested package.

## Vocabulary

- **Instance:** one running `claude remote-control` for an account in a folder.
- **Account:** a Claude login with its own config directory. Its display name can change; its directory never moves, because Claude keys the Keychain login to the directory path.
- **Folder:** where an instance runs. A Git repository, or a folder that holds repositories.

## Layout

| File | What is in it |
|---|---|
| `server.go` | `main`, flags, the request guard, account status |
| `web.go` | routes, page handlers, view models |
| `instances.go` | accounts, launching, restart rules, sign-in, the allowed keys |
| `proc.go` | one supervised process: pseudo-terminal, terminal emulator, prompt detection |
| `paths.go` | allowed folders, folder listing, path confinement |
| `store.go` | the SQLite store and migration runner |
| `migrations/` | numbered SQL schema changes, compiled into the binary |
| `persist.go` | remembered instances, import of pre-database files, the launchd agent |
| `ui/templates` | every screen; `layout.html` holds the shared pieces |
| `ui/static` | `app.css` and the vendored `htmx.min.js` |

## Working on it

```bash
go test -race ./...
```

```bash
go build -o superdeck . && ./superdeck ~/Projects
```

- Tests use a shell-script stand-in for the Claude CLI (`fakeClaude` in `superdeck_test.go`). Extend it when you learn something new about how the real CLI behaves, and record the fact in the `claude-cli-facts` skill.
- Every screen must render with zero accounts and zero folders. `TestEveryPageRendersEvenWhenEmpty` guards this; add new pages to it.
- Look at a changed screen in a browser at phone width and desktop width before calling it done.
- **Restarting Superdeck restarts every instance** and interrupts the sessions they host. Before restarting a copy someone is using, check for a sign-in in progress (`pgrep -f 'claude auth login'`) and say that you are about to restart.
- Never trust a folder or accept a consent prompt on the owner's behalf while testing against real Claude. Stop at the prompt.
- Do not put real email addresses, host names or home-directory paths in the repository. Use the stand-in for screenshots.

## Skills

- `superdeck-ui`: how screens should look and be organised.
- `superdeck-browser-code`: the rules for anything that runs in the browser.
- `superdeck-go`: Go conventions, process supervision, and testing.
- `claude-cli-facts`: what has been observed about the Claude CLI's behaviour.
