---
description: Switch the active Claude Code account without losing this conversation
argument-hint: "[slot | email | alias]"
allowed-tools: Bash(cux __slash-switch:*)
---

# /switch

Hands off the current Claude Code session to a different managed
account and reconnects to the same conversation on the new account.

- With no argument: rotates to the next account in your sequence.
- With a slot number, email, or alias: switches to that specific account.

Aliases are set with `cux alias <slot> <name>` (e.g. `cux alias 1 work`).
Once set, use `/switch work` instead of a slot number or email.

Requires the session to have been started via `cux` (not `claude`
directly). The handoff is implemented by `cux __slash-switch`, which
writes a switch-requested signal the cux wrapper picks up. The
wrapper waits for this turn to end cleanly (so the transcript is
flushed) before swapping accounts and reconnecting with `--resume`.

```bash
cux __slash-switch $ARGUMENTS
```
