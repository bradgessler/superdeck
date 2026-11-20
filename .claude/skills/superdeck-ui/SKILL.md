---
name: superdeck-ui
description: How Superdeck's screens should be organised and styled. Read before changing any template in ui/templates or ui/static/app.css, or adding a screen.
---

# Superdeck UI

The owner's standard is high fit and finish with less on the screen. Earlier versions were rejected for cramming things onto one page, boxing everything, scattering buttons, and showing a raw terminal. Do not repeat those.

## Organisation

- **One job per screen.** A list screen lists. Adding, renaming or configuring something gets its own screen, reached by a button or by tapping the row.
- **Open to status.** The home screen is the list of running instances and nothing else of substance.
- **Rows are the tap target.** A list row is a plain link to that thing's own screen. No per-row buttons, no arrows or chevrons.
- **No inline "type a name, press Add" forms on list screens.**
- **Edit in place on the thing's own screen** where it is natural: an account's name is an input styled as the title, with Save appearing when it has focus.
- **No raw terminal on a status screen.** Say what is happening in a sentence and offer the buttons that matter. The terminal lives on its own screen behind a "Terminal" action.
- **A question from Claude is shown as a question** with the answers as buttons. For a question Superdeck does not recognise, show Claude's own words with Yes and No.

## Layout

- **One column.** Title, a status line, one or two lines of detail, then actions.
- **Actions are full-width buttons stacked in a column** (`.actions`). Do not mix button widths or float buttons beside text.
- **One emphasised thing at a time.** Only the next step is amber. If the next step changes (for example once a code has been typed), move the emphasis with CSS rather than showing two amber buttons.
- **Flat rows with hairlines.** No cards, panels, boxes or shadows. No divider-plus-expander constructions.
- **Every row in a list has the same shape.** If one row has a grey second line, all of them do, starting at the same left edge. Never put grey text inline after a name; it staggers.
- **Destructive actions are quiet:** a neutral button with red text, or plain grey text for Cancel. Not a red block.
- **Must look right on a phone and on a desktop.** Check both.

## Words

- Buttons say what happens: "Open in Claude", "Finish signing in", "Stop instance".
- Sentence case. No jargon where a plain word exists. "Folder", not "project root".
- Errors say what went wrong and what to do, in one sentence.

## Stability

- **Never redraw under someone who is typing or reading.** A screen with an input must stay still. Use a silent watcher (`/instances/{id}/watch`) that tells the browser to move on only when something actually changed.
- No animation on page load.

## Style

Dark, warm charcoal with a single amber accent. System rounded sans for text, system monospace for small labels and paths. The colour and spacing tokens are at the top of `app.css`; use them rather than new values.

Never mark a selected or active thing with a coloured bar down its left edge. Use a lit fill, a tick, or weight.
