---
description: Switch this session's Claude Code account without losing the conversation (--all for every session)
argument-hint: "[slot-number-or-email] [--all]"
allowed-tools: Bash(cux __slash-switch:*)
---

# /switch

Moves this session to another managed account and keeps the conversation.

- With no argument: rotates to the next account in your sequence.
- With a slot number or email: switches to that specific account.

By default only this session moves. cux restarts Claude Code on the
seat's own profile (`~/.claude-<alias>`) and resumes this conversation
there; other sessions keep their account. The seat needs an alias and
that profile directory must already exist.

Add `--all` to swap the shared login instead: cux rewrites the live
credentials in place, and every session using the same config directory
continues on the new account from its next request.

Requires the session to have been started via `cux` (not `claude`
directly).

```bash
cux __slash-switch $ARGUMENTS
```
