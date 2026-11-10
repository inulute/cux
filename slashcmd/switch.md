---
description: Switch this session's Claude Code account without losing the conversation (--all for every session)
argument-hint: "[slot | email | alias] [--all]"
allowed-tools: Bash(cux __slash-switch:*)
---

# /switch

Hands off the current Claude Code session to a different managed
account and reconnects to the same conversation on the new account.

- With no argument: rotates to the next account in your sequence.
- With a slot number, email, or alias: switches to that specific account.
- With `--all`: swaps the shared login, so every session using the same
  config directory moves to that account.

Aliases are set with `cux alias <slot> <name>` (e.g. `cux alias 1 work`).
Once set, use `/switch work` instead of a slot number or email.

Requires the session to have been started via `cux` (not `claude`
directly). Without `--all` only this session moves: `cux __slash-switch`
writes a switch-requested signal the cux wrapper picks up, and the
wrapper restarts Claude Code on the seat's own profile
(`~/.claude-<alias>`), carries the transcript over and reconnects with
`--resume`. Other sessions keep their account.

```bash
cux __slash-switch $ARGUMENTS
```
