# Changelog

Starts at v0.3.10. Earlier releases are on the
[releases page](https://github.com/inulute/cux/releases).

## v0.3.11 — 2026-08-21

### Fixed

- **A background usage refresh could file one account's token into another
  account's slot.** After the usage API rejected a stored token, cux fell back
  to the live credentials and adopted them into the slot — the behaviour that
  makes a `claude login` without a `cux add` repair itself. The check gating it
  compared Claude Code's identity file against the slot's email, which never
  established that the token belonged to that account. Same corruption
  v0.3.10 closed in `cux add` and `cux switch`, reached without any command
  being run.

  The self-repair is kept. Before adopting a token, cux now checks it is not
  already stored under a different slot; on a match it keeps the usage reading
  and skips only the write, leaving the slot repairable by `cux add`. A
  re-login for the same account still repairs itself with no command.

### Added

- **`cux status` reports slots that share one login.** The write paths refuse
  to create that state, but a pool captured by an earlier version can already
  be in it, reporting one account's usage under several names.

### Known limitation

The check above is a negative proof: it catches a token that another *managed*
slot already holds, but not one belonging to an account cux has never seen.
Anthropic's OAuth tokens are opaque, so nothing local can attribute one to an
account. Tracked in [#47](https://github.com/inulute/cux/issues/47).

## v0.3.10 — 2026-08-21

### Fixed

- **A stale usage cache was rendered as live data.** A failed poll leaves the
  previous reading in place, and nothing distinguished one fetched a second ago
  from one twelve days old. A pool could look healthy while every number in it
  was frozen — a cancelled account shown `READY` and offered as `NEXT USABLE`,
  and no threshold swap able to fire again for as long as polling stayed
  broken. Reported in
  [#46](https://github.com/inulute/cux/issues/46).
- **`cux add` could file one account's token under another's name.** The
  account identity came from Claude Code's `oauthAccount` block and the token
  from its credential store, and nothing checked the two described the same
  account. They can legitimately disagree when `claude auth login` writes the
  token somewhere other than where `CLAUDE_CONFIG_DIR` points. Both slots then
  polled with the same token and each reported the other's limits, so a swap
  could move onto an account that was actually exhausted.
- **`cux switch` could corrupt a slot the same way**, by re-backing-up live
  credentials under whatever the identity file named. It now leaves the stored
  login alone when the pair does not agree, and still switches.
- **A window that had already reset still reported its pre-reset usage.**
  Threshold checks read utilization without consulting `resets_at`, so for a
  while after a reset cux could move a session off an account that had just
  recovered.

### Added

- **Readings too old to trust say so.** Past 30 minutes an account renders as
  `STALE` with `?` in place of its figures, and a banner names how old the
  oldest reading is. `RESET` no longer renders an elapsed instant as `now`.
  This appears on `cux list`, `cux status` and the slash commands alike —
  previously the slash-command view was the one surface that showed no warning
  at all.
- **`cux status` reports stored logins that cannot be used**, distinguishing a
  missing login, one carrying no account token, and a credential store that
  refused the read — they need different repairs. Prints nothing when every
  slot is healthy.
- **`cux usage refresh` exits non-zero when it polled nothing**, so a cron job
  or wrapper can detect the condition instead of reading a success it did not
  get.
- **`cux add --force`** captures a login even when the token looks like another
  account's, for the case where that check is wrong.

### Changed

- **Swap decisions no longer act on readings they cannot vouch for.** Leaving
  an account now needs fresh data, because a preemptive swap costs a process
  restart. Declaring a pool exhausted fails open instead — a stale reading is
  not evidence of exhaustion, and that verdict can block a prompt or end an
  unattended session. Choosing where to go, once a move is decided, is
  unchanged and still never refuses, so a live session is not stranded.
