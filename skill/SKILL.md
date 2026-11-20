---
name: superdeck
description: Move this work to another Claude account on this Mac, or pick up work that was handed over from another account. Use when the user asks to move, switch or change accounts, says this account is out of usage or rate limited, or asks to continue, resume or pick up where another account or an earlier session left off.
---

# Superdeck

This session was started by Superdeck, a launcher on this Mac that runs Claude under one of several Claude accounts. Each account has its own usage. Superdeck can move the work in this folder to another account. You talk to it with one command, whose path is in `$SUPERDECK`.

If `$SUPERDECK` is empty, this session was started before Superdeck could be reached from inside it. Say so, and ask the user to relaunch this instance from the Superdeck page.

## Seeing the accounts

```bash
"$SUPERDECK" accounts
```

Lists the accounts, which one this session is running under, and which are signed in. Only a signed-in account can take the work. Use the names exactly as listed.

## Moving to another account

Moving stops this instance and starts it again under the other account. **This conversation ends when that happens**, and it cannot be carried across: it belongs to this account. What carries the work across is the state of the files and the notes you leave. So, in this order:

1. If the user did not say which account, run `"$SUPERDECK" accounts` and ask.
2. Finish or park what you are doing. In a Git repository, commit work in progress on the current branch (a commit marked WIP is fine). Do not push unless the user asked.
3. Tell the user, in one or two sentences, that you are moving the work now, that this conversation will end, and that they should start a new session for this folder in the Claude app and ask it to pick up where this left off. If the other account has not worked in this folder before, mention that Superdeck's page will show a one-time question about trusting the folder, which they need to answer there first. Say this before the command: you will not get to say anything after it.
4. Run the move as your last action, with handoff notes on standard input:

```bash
"$SUPERDECK" move ACCOUNT <<'NOTES'
What the user asked for, in their words where it matters.
What is done, and what is left, in order.
The branch and the last commit, and anything not committed.
Decisions made and why, things tried that did not work, and anything the
user said about how they want it done.
The exact next step.
NOTES
```

Write the notes for someone with none of this conversation's context: they are all the next session will have. Be specific; name files, commands and branches.

Superdeck checks the move can work before stopping anything. If it refuses (the account is signed out, or already running this folder), nothing has changed: tell the user what it said.

## Picking up handed-over work

When the user asks you to continue or pick up where things left off, or you start in a folder and it is unclear what was going on:

```bash
"$SUPERDECK" handoff
```

It prints the notes the last session left for this folder, when it left them, and the directory it was working in. That directory may be a different Git worktree of this repository from yours, holding the branch with its work: check it out or merge it rather than starting again. Read the notes, check them against the actual state of the files and `git log`, tell the user in a few lines where things stand, then carry on from the next step. If there are no notes, say so and ask what to do.

## Limits

- You can move this instance only. Launching, stopping, signing in and everything else is done by a person on the Superdeck page.
- Do not move to get around a limit the user has not asked you to get around. Move when the user asks.
