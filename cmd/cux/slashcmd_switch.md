---
description: Switch the active Claude Code account without losing this conversation
argument-hint: "[slot-number-or-email]"
allowed-tools: Bash(cux __slash-switch:*)
---

# /switch

Switches the active managed account in place — your conversation keeps
going on the new account with no restart and no lost context.

- With no argument: rotates to the next account in your sequence.
- With a slot number or email: switches to that specific account.

Requires the session to have been started via `cux` (not `claude`
directly). The swap is done by `cux __slash-switch`, which rewrites the
live credentials. Claude Code reads credentials on every request, so
your next message continues seamlessly on the new account.

```bash
cux __slash-switch $ARGUMENTS
```
