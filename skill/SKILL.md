---
name: switcharoo
description: Run Switcharoo, the launcher that started this Claude session, on the user's Mac. Use when the user asks to move this work to another Claude account, says an account is out of usage or rate limited, asks to pick up where another account or an earlier session left off, or asks to see what is running, launch Claude in another folder, make a new project folder, stop or relaunch an instance, add a Claude account or sign one in.
---

# Switcharoo

This session was started by Switcharoo, a launcher on this Mac. It runs Claude under one of several Claude accounts, each with its own usage, one "instance" per account and folder. The user normally works it from a web page. You can work it for them with one command, whose path is in `$SWITCHAROO`. Everything below is that command.

If `$SWITCHAROO` is empty but `$SUPERDECK` is not, this instance was started when Switcharoo was still called Superdeck: use `"$SUPERDECK"` wherever this says `"$SWITCHAROO"`. If both are empty, this session was started before Switcharoo could be reached from inside it. Say so, and ask the user to relaunch this instance from the Switcharoo page.

Do these things when the user asks for them, not on your own initiative. Each one changes what is running on their Mac.

## Asking the user

Several things below need an answer from the user first: a yes or no, or which account. If you have a tool that puts a question to the user with choices to tap (AskUserQuestion), use it, with the question in one line and the choices as short labels such as "Yes, trust it" and "No". They are often on a phone, and a tap is easier than typing. Without such a tool, ask in one short sentence. Either way, only what the user answers counts: never choose for them.

## Which Claude login the user needs

Each account is signed in as a Claude login, an email address that `accounts` shows. The Claude app lists only the instances of the login the app itself is signed in as. So when you launch under, or move to, an account whose login is not this session's, the user will not see the new instance until they switch account in the Claude app. Switcharoo says so in its reply when this applies. Pass it on plainly, with the address to switch to, as the last thing you tell them.

## Looking

```bash
"$SWITCHAROO" status
"$SWITCHAROO" accounts
"$SWITCHAROO" folders [WORD]
```

`status` lists the instances, marks the one this session is in, and shows anything waiting on a person, with the command that answers it. `accounts` shows which accounts are signed in; only those can run anything. `folders` lists where Claude can be launched. Use account and folder names as these list them.

## Moving this work to another account

Moving stops this instance and starts it again under the other account. **This conversation ends when that happens**, and it cannot be carried across: it belongs to this account. What carries the work across is the state of the files and the notes you leave. So, in this order:

1. If the user did not say which account, run `"$SWITCHAROO" accounts` and ask.
2. Finish or park what you are doing. In a Git repository, commit work in progress on the current branch (a commit marked WIP is fine). Do not push unless the user asked.
3. Tell the user, in one or two sentences, that you are moving the work now, that this conversation will end, and that they should start a new session for this folder in the Claude app and ask it to pick up where this left off. If the other account has not worked in this folder before, add that Claude will first ask whether to trust the folder, and that the new session's first job is to put that question to them. If that account's login, as `accounts` shows it, is not this session's, say they must first switch the Claude app to that address. Say all this before the command: you will not get to say anything after it.
4. Run the move as your last action, with handoff notes on standard input:

```bash
"$SWITCHAROO" move ACCOUNT <<'NOTES'
What the user asked for, in their words where it matters.
What is done, and what is left, in order.
The branch and the last commit, and anything not committed.
Decisions made and why, things tried that did not work, and anything the
user said about how they want it done.
The exact next step.
NOTES
```

Write the notes for someone with none of this conversation's context: they are all the next session will have. Be specific; name files, commands and branches.

Switcharoo checks the move can work before stopping anything. If it refuses (the account is signed out, or already running this folder), nothing has changed: tell the user what it said.

## Picking up handed-over work

When the user asks you to continue or pick up where things left off, or you start in a folder and it is unclear what was going on:

```bash
"$SWITCHAROO" handoff
```

It prints the notes the last session left for this folder, when it left them, and the directory it was working in. That directory may be a different Git worktree of this repository from yours, holding the branch with its work: check it out or merge it rather than starting again. Read the notes, check them against the actual state of the files and `git log`, tell the user in a few lines where things stand, then carry on from the next step. If there are no notes, say so and ask what to do.

## Launching, and questions Claude asks

```bash
"$SWITCHAROO" launch ACCOUNT FOLDER
"$SWITCHAROO" folder NAME            # a new, empty Git repository to launch in
"$SWITCHAROO" answer INSTANCE yes    # or no
```

`launch` starts Claude under an account in a folder and tells you how it went. Often the new instance stops to ask something first, usually whether to trust the folder. (Switcharoo itself says yes when Claude asks whether to turn on Remote Control for an account, since launching here is asking for that; you should not normally see that question.) **You do not answer these yourself.** They are the user's decisions about their own Mac. Put the question to the user as Switcharoo reported it, including the folder's path, as a yes or no they can tap (see Asking the user), and run `answer` only with what they tell you in this conversation. A trust question for a folder the user did not ask you to launch in is a reason to stop and check, not to answer.

Once it is connected, the user finds it in the Claude app under Code; `status` gives its address.

## Stopping and relaunching

```bash
"$SWITCHAROO" relaunch [INSTANCE]
"$SWITCHAROO" stop [INSTANCE]
```

Both end every Claude session the instance is hosting, so say that and get a clear yes first. With no instance named they act on the one this session is in, which ends this conversation a few seconds later: say what you need to say before the command. A stopped instance stays stopped until someone launches it again. `relaunch` is how an instance picks up a new version of Claude.

## Adding an account, and signing in

```bash
"$SWITCHAROO" account add NAME      # lowercase letters, digits and dashes
"$SWITCHAROO" signin ACCOUNT        # sign an existing account in again
"$SWITCHAROO" code ACCOUNT CODE
```

`account add` and `signin` print the address of a sign-in page on Anthropic's site. Give the user that address exactly as printed, and tell them to open it in a browser that is signed in to the Claude account they want this one to use; if their browser is signed in to a different Claude account, a private window lets them choose. The page shows them a code. When they send you the code, run `code` with it. Never ask for a password, and never make up or guess a code. A sign-in left alone is cancelled after 15 minutes.

Each account should be a different Claude login. Two accounts signed in as the same person share the same usage, so moving between them gains nothing; if `accounts` shows the same address twice, say so.

## Limits

- Removing an account, renaming one and updating Claude are done on the Switcharoo page, not from here.
- Switcharoo refuses anything outside the folders its owner allowed, and limits how many instances and accounts there can be. When it refuses, it says why and changes nothing: pass that on, do not look for a way around it.
- Do not move, launch or sign in to get around a usage limit the user has not asked you to get around.
