---
description: Switch this session's cux-managed Claude Code account (--all for every session)
argument-hint: "[slot-number-or-email] [--all]"
allowed-tools: Bash(cux __slash-switch:*)
---

# /cux:switch

Moves this session to another cux-managed account: Claude Code restarts
on the seat's own profile (`~/.claude-<alias>`) and the conversation
resumes there. Other sessions keep their account. With no argument,
rotates to the next account. Add `--all` to swap the shared login for
every session in place instead.

```bash
cux __slash-switch $ARGUMENTS
```
