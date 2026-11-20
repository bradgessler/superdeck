---
name: superdeck-browser-code
description: Rules for anything that runs in the browser in Superdeck. Read before adding a script tag, a JavaScript file, an hx- attribute or a front-end dependency.
---

# Browser code in Superdeck

The owner wants the least possible running in the browser. A previous version was a JavaScript single-page application talking to a JSON API. It broke in front of them (a null-state crash, hash routes) and was thrown away. Browser code is treated as a failure surface to keep small.

## Rules

1. **The server renders everything.** A screen is a Go template. State lives on the server.
2. **Every screen works as plain links and forms.** Navigation uses real URLs, never hash routes. A form posts and the server redirects.
3. **htmx is the only script**, vendored as one file in `ui/static`. Use it for exactly two things: refreshing a fragment that shows changing status, and filtering a list as you type.
4. **No React, no Node, no bundler, no build step for the front end, no npm packages.** Do not add a terminal emulator or any other library to the browser. Terminal emulation happens on the server (`proc.go`).
5. **No inline scripts and no `hx-on`.** The Content-Security-Policy is `default-src 'self'` with inline styles allowed (the terminal's colour spans need them), and htmx is configured with `allowEval: false`.
6. **Prefer CSS to script** for presentation: `:has()`, `:focus-within`, `:valid` cover revealing a button, moving emphasis, and styling a chosen radio.
7. **If a pushed connection ever replaces polling**, keep the pages the same: the server still sends HTML fragments.

## Patterns already in use

- A screen with buttons or an input stays still. It polls `/instances/{id}/watch?sig=...` with `hx-swap="none"`, sending the signature of what it is showing. The server answers `204` and nothing until the signature no longer matches, then sends `HX-Refresh` or `HX-Redirect`. Never swap a region that holds a control someone may be about to tap.
- A list of links that changes (the instances list): `hx-get` the fragment with `hx-trigger="every 3s"` and `hx-swap="outerHTML"`.
- The terminal: `hx-get` the screen into a `<pre>` every second; the server sends `HX-Refresh` once the process has ended so polling stops.
- Type-to-filter: `hx-get` on the input with `hx-trigger="input changed delay:120ms"`, `hx-include` the checked radio so the selection survives. The filter is its own `GET` form, so it works without htmx and Enter in it can never submit something else. Controls that belong to another form use the `form="..."` attribute.
- A form that should work with or without htmx: give it both `action` and `hx-post`. Handlers use `to()` in `web.go`, which sends `HX-Redirect` or a `303`.
- Confirmation: `hx-confirm` on the form.

## Before adding anything

Ask whether the server can do it. If it can, do it there.
