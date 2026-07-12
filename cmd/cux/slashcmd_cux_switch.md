---
description: Switch the active cux-managed Claude Code account
argument-hint: "[slot-number-or-email]"
allowed-tools: Bash(cux __slash-switch:*)
---

# /cux:switch

Switches the active cux-managed account in place — the conversation
continues on the new account with no restart. With no argument, rotates
to the next account.

```bash
cux __slash-switch $ARGUMENTS
```
