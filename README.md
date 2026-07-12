<p align="center">
  <img src="./assets/cux-banner.webp" alt="cux — Run multiple Claude Code Pro/Max accounts as one" width="100%">
</p>

<p align="center">
  Claude Code account switcher for uninterrupted sessions.
</p>

<p align="center">
  <a href="https://github.com/inulute/cux/releases/latest">
    <img alt="latest release"
         src="https://img.shields.io/github/v/release/inulute/cux?style=for-the-badge&label=Release&color=c8763a&labelColor=0e1116&logo=github&logoColor=f0ead6">
  </a>
  &nbsp;
  <a href="https://www.npmjs.com/package/@inulute/cux">
    <img alt="npm version"
         src="https://img.shields.io/npm/v/@inulute/cux?style=for-the-badge&label=npm&color=c8763a&labelColor=0e1116&logo=npm&logoColor=f0ead6">
  </a>
  &nbsp;
  <a href="https://github.com/inulute/cux/blob/main/LICENSE">
    <img alt="license"
         src="https://img.shields.io/badge/License-GPL--3.0-c8763a?style=for-the-badge&labelColor=0e1116&logo=gnu&logoColor=f0ead6">
  </a>
  &nbsp;
  <a href="https://support.inulute.com">
    <img alt="support"
         src="https://img.shields.io/badge/Support-%E2%99%A5-c8763a?style=for-the-badge&labelColor=0e1116&logo=githubsponsors&logoColor=f0ead6">
  </a>
  &nbsp;
  <a href="https://cux.inulute.com/docs">
    <img alt="docs"
         src="https://img.shields.io/badge/Docs-cux.inulute.com-c8763a?style=for-the-badge&labelColor=0e1116&logo=readthedocs&logoColor=f0ead6">
  </a>
</p>

---

**`cux`** is a CLI tool for Claude Code that pools multiple Pro/Max
accounts behind a single live session. When the active account hits
a rate limit, `cux` switches to a healthy account and continues the
same conversation. For proactive threshold swaps, it waits for the
current turn to finish first. No logout, no reload, no lost context.


```text
$ cux
cux: rate limit on alice@example.com → swapped to bob@example.com, resuming…
> What number did I tell you to remember?
4729.
```

---

## Contents

- [Install](#install)
  - [Option 1 — npm](#option-1--npm)
  - [Option 2 — shell installer](#option-2--shell-installer)
  - [Option 3 — manual binary](#option-3--manual-binary)
  - [After install](#after-install)
  - [What works on which platform](#what-works-on-which-platform)
- [Quick start](#quick-start)
  - [Verify your setup once](#verify-your-setup-once)
- [How it works](#how-it-works)
- [Daily usage](#daily-usage)
- [Configuration](#configuration)
  - [Strategies](#strategies)
- [Swap history](#swap-history)
- [Data layout](#data-layout)
- [Security](#security)
- [Building from source](#building-from-source)
- [License](#license)
- [Socials](#socials)

## Install

Three install methods. Pick the one that fits your platform — they
all install the same `cux` binary.

### Option 1 — npm

Works on Linux, macOS and Windows. Requires Node.js 18 or newer.

```bash
npm install -g @inulute/cux
```

### Option 2 — shell installer

Works on Linux, macOS, WSL and Git Bash on Windows.

```bash
curl -fsSL https://raw.githubusercontent.com/inulute/cux/main/scripts/install.sh | sh
```

### Option 3 — manual binary

Works everywhere. Useful if you don't want Node.js and can't run
shell scripts (e.g. native Windows PowerShell or cmd.exe).

1. Download the matching artefact from the
   [releases page](https://github.com/inulute/cux/releases):
   - `cux-linux-amd64`, `cux-linux-arm64`
   - `cux-darwin-amd64`, `cux-darwin-arm64`
   - `cux-windows-amd64.exe`
2. On Linux/macOS, `chmod +x cux-<os>-<arch>` and rename to `cux`.
3. Move it somewhere on your `PATH`:
   - Linux/macOS: `~/.local/bin/cux`
   - Windows: any directory listed in your `Path` environment variable.

### After install

Run `cux setup` once. That installs the `/switch` and `/cux:*` slash
commands plus the four Claude Code hooks. Restart Claude Code
afterwards so it picks them up.

### What works on which platform

| | Linux | macOS | WSL / Git Bash | native Windows |
|---|:---:|:---:|:---:|:---:|
| Account commands (`add`, `list`, `switch`, `status`, …) | ✅ | ✅ | ✅ | ✅ |
| Credential storage | file (0600) | Keychain | file (0600) | Credential Manager |
| Hooks + `/switch` and `/cux:*` slash commands | ✅ | ✅ | ✅ | ✅ |
| Auto-resume on swap | ✅ | ✅ | ✅ | ✅ † |
| `npm install -g @inulute/cux` | ✅ | ✅ | ✅ | ✅ |
| `curl … \| sh` shell installer | ✅ | ✅ | ✅ | ❌ |
| Manual binary download | ✅ | ✅ | ✅ | ✅ |

† On native Windows the wrapper hard-terminates `claude` on swap (Go
can't send `SIGINT` cross-process there). The `Stop` hook still
flushes the transcript before the wrapper acts, so the resumed
conversation is intact — but if you see anything unexpected, please
[open an issue](https://github.com/inulute/cux/issues).

## Quick start

Website: https://cux.inulute.com

```bash
cux setup           # install /switch, /cux:* + Claude Code hooks
cux add             # register the currently-logged-in account
claude login        # log into your second account — just log in again, no logout
cux add             # register it
cux                 # launch claude under cux instead of `claude` directly
```

> **Don't run `claude logout` to switch accounts.** `claude login` on its
> own re-authenticates and replaces the active credentials — that's all
> cux needs to capture the next account. `claude logout` *clears and
> revokes* the stored token, which invalidates the backup cux keeps for
> that account (it will show up as `EXPRD` in `cux list`). Once both
> accounts are added, let cux do all switching — it swaps credentials
> directly and never logs out, so nothing gets revoked.

After `cux setup`, restart Claude Code so it picks up the newly
installed hooks. From then on:

- `/switch` from inside a Claude Code session rotates accounts.
- `/switch <slot|email>` switches to a specific one.
- `/cux:add`, `/cux:list`, `/cux:status`, `/cux:config`, `/cux:remove`,
  `/cux:switch`, `/cux:usage-refresh`, and `/cux:support` run
  account-management commands in-session.
- A rate-limit response from the API auto-triggers the same flow and
  does not wait for another Stop hook before reconnecting.

### Verify your setup once

A 30-second check that proves end-to-end context preservation:

1. Send: *"Please remember the number 4729."*
2. Wait for the reply.
3. Send `/switch`.
4. After the ~2-second reconnect, ask: *"What number did I tell you to remember?"*

If the answer is `4729`, swap-and-resume is working.

## How it works

```
   user types     ┌────── claude (running, account A) ──────┐
   /switch ──────►│  hooks: UserPromptSubmit, Stop,         │
   or rate-limit  │         SessionStart, PostToolUseFailure│
   ───────────────┴──┬──────────────────────────────────────┘
                     │ writes signal files
                     │ runtime/signals/{wrapperPID}-{name}
                     ▼
             ┌──────────────────────────────────────┐
             │  cux wrapper                         │  polls signals
             │   on rate-limit OR threshold OR      │  every 100 ms
             │   /switch:                           │
             │     rate-limit/manual: exit now      │  avoids hard-limit stall
             │     threshold: wait for Stop signal  │  guarantees flush
             │     ask claude to exit cleanly       │
             │     swap creds (transactional)       │
             │     append history.Entry             │
             │     relaunch claude <orig. flags>    │
             │       --resume <id> [auto_message]   │
             └──────────────────────────────────────┘
```

`cux` writes its hooks into `~/.claude/settings.json` by signature,
so it never modifies entries owned by other tools and
`cux uninstall-hooks` removes only its own. Every cux-owned file goes
through atomic writes (`tmp + fsync + rename`) and state mutations
are serialised with file locks (`flock` / `LockFileEx`).

## Daily usage

```bash
cux                          # launch claude under the wrapper
cux list                     # accounts with 5h / 7d utilisation
cux list --refresh           # refresh usage before listing
cux status                   # current login + cux state
cux switch <slot|email>      # manual swap (no auto-resume)
cux remove <slot|email>      # forget an account
cux history                  # recent swaps with reasons
cux usage refresh            # poll all account usage
cux config show              # current settings
cux config edit              # interactive settings editor
cux upgrade                  # update cux (npm or installer; auto-detects)
```

From inside a session started with `cux`:

```text
/switch                      # rotate per the configured strategy
/switch 2                    # by slot number
/switch alt@example.com      # by email
/cux:switch 2                # same switch flow under the /cux namespace
/cux:add                     # add/refresh the current login
/cux:list --refresh          # list accounts from inside Claude Code
/cux:status                  # show live login + cux state
/cux:support                 # show support URL
/cux:config show             # show cux configuration
/cux:remove 2                # remove an account
/cux:usage-refresh           # refresh account usage
```

If Claude is already hard-blocked, `/switch` is handled by cux's
`UserPromptSubmit` hook before Claude processes the prompt. If you are
on an older session that was started before that hook was installed,
run this from another terminal:

```bash
cux force-switch             # rotate the active cux-wrapped session
cux force-switch 2           # force a specific slot/email
```

## Configuration

```bash
cux config keys                                      # discover everything
cux config show
cux config set thresholds.five_hour 85
cux config set strategy.kind balanced
cux config set strategy.order alice@x,bob@x         # drain priority
cux config set auto_message ""                      # silent resume
cux config set update_check.enabled true            # opt in to update checks
```

| Key | Default | Description |
|---|---|---|
| `thresholds.five_hour`        | `100`          | Auto-swap when 5h utilisation hits this %. `100` = reactive only. |
| `thresholds.seven_day`        | `100`          | Auto-swap when 7d utilisation hits this %. `100` = reactive only. |
| `strategy.kind`               | `drain`        | `drain` / `balanced` / `manual` |
| `strategy.order`              | `[]`           | Drain mode priority (emails); empty = auto by highest 7d |
| `auto_switch_on_threshold`    | `true`         | Master toggle for pre-emptive threshold swap |
| `auto_switch_on_rate_limit`   | `true`         | Master toggle for swap on rate-limit hook |
| `auto_resume`                 | `true`         | Pass `--resume <id>` to the relaunched claude |
| `auto_message`                | `Go continue.` | First user turn after auto-swap; `""` = silent |
| `wait_for_reset`              | `true`         | When every account is exhausted, sleep until the earliest reset and resume |
| `retry_on_api_error`          | `true`         | Relaunch and auto-continue after a non-rate-limit API failure (fibonacci backoff, capped at 2 min) |
| `update_check.enabled`        | `false`        | Check GitHub for newer cux releases on startup |
| `update_check.cadence_hours`  | `6`            | Minimum hours between update checks (cached locally) |

Config file: `~/.config/cux/config.json` (XDG-aware).

### Strategies

- **drain** — Use one account until its 7-day cap is near, then move
  on. Set `order` for explicit priority, or leave empty to auto-drain
  the highest-7d account first.
- **balanced** — Always pick the account with the lowest 7-day
  utilisation (tiebreak by lowest 5h).
- **manual** — Never swap automatically. `/switch` and `cux switch`
  still work.

Both automatic strategies also prefer accounts whose model-specific
weekly windows (Opus/Sonnet, reported on some plans) still have room.
A model-capped account is never made ineligible — cux cannot know
which model the session will ask for next — it just sorts behind
model-clear candidates, so a heavy-Opus session is not swapped onto a
seat that would rate-limit its very next call.

## Projects

A machine that hosts several codebases often wants them on different
seats — one client's work billed to their org, a personal side project
kept off the company pool — while related projects share accounts.
Projects bind a directory to a subset of seats:

```bash
cux project create clientwork --dir ~/code/client
cux project assign clientwork 1 2        # seats by slot, email, or alias
cux project create side --dir ~/code/side
cux project assign side 2 3              # seat 2 is shared between both
cux project list --refresh               # projects + live usage per seat
cux project unassign side 3
cux project stats clientwork --days 7    # tokens & time from Claude Code transcripts
cux project remove side                  # unbind only; accounts untouched
```

When cux starts, the working directory picks the project (longest,
path-boundary-aware match — nested projects work) and every automatic
decision draws from that project's seats only: threshold and
rate-limit swaps, rotation, and `wait_for_reset`'s availability math.

- **No projects, or an unclaimed directory** → the full pool. Existing
  setups behave exactly as before.
- **A project with no seats assigned yet** → the full pool, until you
  assign some.
- **Explicit targets are never restricted** — `/switch <seat>` and
  `cux switch <seat>` go wherever you point them; a human naming a
  seat outranks the project boundary.
- Removing an account (`cux remove`) also removes it from every
  project pool.

`cux project stats` reads the session transcripts Claude Code already
writes under `~/.claude/projects` — cux collects nothing itself — and
sums sessions, active time, turns, and input/output/cache tokens for
the project's directory tree, optionally windowed with `--days N`.

## Swap history

```text
$ cux history
2026-05-01 14:12:33  alice@x → bob@x  [threshold]
    reason: 7d utilization 95% ≥ threshold 95%
    usage: alice@x 5h:34% 7d:95% → bob@x 5h:8% 7d:30%
    session: 143eec0f-277e-4ce1-95f1-58eb56331874

2026-05-01 13:55:08  bob@x → alice@x  [manual]
    reason: user requested via /switch
```

`cux history -n 5` for the last five, `cux history --json` to pipe,
`cux history --clear` to wipe. History is capped at 1000 entries.

## Data layout

```
~/.local/share/cux/                  (~/.cux/ on macOS/Windows)
├── state.json                      # accounts, sequence, active slot
├── .lock                           # flock target for state mutations
├── accounts/
│   └── 01-user@example.com/
│       ├── credentials.json        # Linux only; macOS/Win uses keystore
│       └── oauth.json              # the oauthAccount block, raw JSON
└── runtime/
    ├── signals/                    # hook → wrapper signal files
    ├── usage-cache.json            # per-account 5h / 7d snapshot
    ├── update-cache.json           # update-check cadence cache
    └── swap-history.json           # capped at 1000 entries

~/.config/cux/config.json           # XDG_CONFIG_HOME-aware
~/.claude/settings.json             # hooks upserted here
~/.claude/commands/switch.md        # /switch slash command
~/.claude/commands/cux/*.md         # /cux:* account commands
```

## Security

- **Tokens are never logged.** Credential blobs are opaque to
  logging; the helper that extracts a token never surfaces it in any
  error message.
- **All cux-owned directories and files are 0700 / 0600.**
- **The installer refuses to run as root** unless inside a container.
- **`/switch` is gated by `CUX_WRAPPED`** — the slash command refuses
  to act unless cux is the parent process, so it cannot accidentally
  signal an unrelated `claude`.
- **Hook upsert is signature-keyed.** `cux install-hooks` only ever
  modifies entries whose `command` field contains the literal string
  `cux ` or `/cux ` — every other tool's hooks are preserved.

## Building from source

```bash
git clone https://github.com/inulute/cux
cd cux
go build -o cux ./cmd/cux
./cux help
```

Requires Go 1.21+.

`go test ./...` is safe to run next to a real cux setup: the suite
redirects HOME/XDG into temp dirs and forces the file credential
backend (`CUX_CREDS_BACKEND=file`), so it never touches your actual
state, usage cache, or OS keychain.

## License

[GPL-3.0-only](./LICENSE). Modifying and redistributing `cux` is
welcome; if you do, your changes need to ship under GPL-3.0 too.

## Socials

All inulute social channels live at
**[socials.inulute.com](https://socials.inulute.com)** — one place
for updates, other projects, and how to reach me.

---

If `cux` saves you time, you can support development at
[support.inulute.com](https://support.inulute.com).
