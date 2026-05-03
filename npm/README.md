# @inulute/cux

Multi-account switcher for Claude Code with hook-driven auto-resume.

This package is a thin npm wrapper. On install it downloads the right
prebuilt `cux` binary for your platform (Linux x64/arm64, macOS
x64/arm64, Windows x64) from the
[GitHub release matching its version](https://github.com/inulute/cux/releases)
and exposes it as the `cux` command on your `PATH`.

```bash
npm install -g @inulute/cux
cux setup
cux add        # or /cux:add inside Claude while logged in
```

For full documentation (auto-swap on rate-limit, threshold-based
swaps, strategies, history, configuration), see the main repository:

- **Website:** https://cux.inulute.com
- **README:** https://github.com/inulute/cux#readme
- **Releases:** https://github.com/inulute/cux/releases
- **Issues:** https://github.com/inulute/cux/issues

## What it does

- `/switch` from inside a Claude session to swap accounts and resume
  the same conversation.
- `/cux:switch`, `/cux:add`, `/cux:list`, `/cux:support`, `/cux:config`, and related commands for in-session
  account management.
- `cux support` and `/cux:support` show the support URL.
- Automatic swap when an account hits a rate limit, with auto-resume.
- Threshold-based pre-emptive swap before a window caps.
- One Go binary, no Python, no Bash version requirement, no `jq`.

## How the npm wrapper works

`postinstall` runs `install.js`, which:

1. Reads `process.platform` and `process.arch` to pick the right
   release artefact name (e.g. `cux-darwin-arm64`).
2. Downloads the binary from
   `https://github.com/inulute/cux/releases/download/v<version>/<asset>`.
3. Verifies the sidecar `<asset>.sha256` if present.
4. `chmod +x` on POSIX, then exposes it as the `cux` command via `bin`.

Override the source repo with `CUX_RELEASE_REPO=<owner>/<repo>` if you
maintain a fork. To skip the network entirely, install with
`npm install -g @inulute/cux --ignore-scripts` and drop the binary at
`node_modules/@inulute/cux/bin/cux` (or `cux.exe` on Windows) yourself.

## License

GPL-3.0-only — see https://github.com/inulute/cux/blob/main/LICENSE.

---

If cux saves you time, you can support development at
[support.inulute.com](https://support.inulute.com).
