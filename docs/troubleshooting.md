# Troubleshooting

Full docs online: **https://cux.inulute.com/docs**

Common problems and how to fix them. For platform-specific detail see [Windows](./windows.md), [macOS](./macos.md), [Linux](./linux.md).

---

## `cux` is not recognized after `npm install -g`

**Symptom**

```
C:\Users\Admin>cux setup
'cux' is not recognized as an internal or external command,
operable program or batch file.
```

**Cause**

npm's global bin directory is not on your `Path`. This is common on fresh Windows installs.

**Fix**

See [Windows PATH fix](./installation.md#windows-path-fix) in the installation guide.

---

## `cux` command exists but postinstall failed to download the binary

**Symptom**

```
cux: binary not found at C:\...\node_modules\@inulute\cux\bin\cux.exe.
Postinstall may have failed; try `npm install -g @inulute/cux --force`
```

**Cause**

The npm postinstall script downloads the native binary from GitHub Releases. If your network blocked the request or GitHub was temporarily unavailable, the download was skipped.

**Fix**

Re-run the install with `--force` to trigger the postinstall again:

```bash
npm install -g @inulute/cux --force
```

If that still fails (e.g. corporate proxy, offline environment), download the binary manually from the [releases page](https://github.com/inulute/cux/releases/latest) and place it at the path shown in the error message (rename it to `cux.exe` on Windows or `cux` on Linux/macOS).

---

## `cux setup` says hooks already installed but `/switch` does not work

**Fix**

Restart Claude Code after running `cux setup`. Hooks are only loaded at startup.

```bash
cux setup   # re-run to be sure
# then restart Claude Code
```

---

## `cux add` says "no active Claude login found"

**Cause**

You need to be logged into Claude Code before adding an account.

**Fix**

```bash
claude login   # log in first
cux add        # then register the account
```

---

## Rate-limit swap triggers but Claude does not resume

**Cause**

`auto_resume` may be disabled, or the session ID was not captured.

**Check**

```bash
cux config show
```

Look for `auto_resume: false`. If it is false, re-enable it:

```bash
cux config set auto_resume true
```

Also confirm the wrapper is running — swap and resume only work when Claude Code was started with `cux`, not `claude` directly.

---

## `/switch` responds with "not running under cux"

**Cause**

The slash command is gated on the `CUX_WRAPPED` environment variable. It only acts when Claude Code was started via `cux`.

**Fix**

Start Claude Code through the wrapper:

```bash
cux   # instead of: claude
```

---

## `cux upgrade` reports "unknown install method"

**Cause**

`cux upgrade` auto-detects whether you installed via npm or the shell installer. If neither marker is found (e.g. you installed a manual binary), it cannot upgrade automatically.

**Fix**

Download the latest binary manually from the [releases page](https://github.com/inulute/cux/releases/latest) and replace your existing binary, or re-install via npm:

```bash
npm install -g @inulute/cux
```

---

## macOS: Gatekeeper blocks the binary

**Symptom**

```
"cux" cannot be opened because the developer cannot be verified.
```

**Fix**

```bash
xattr -d com.apple.quarantine /path/to/cux
```

Or: **System Settings** → **Privacy & Security** → scroll down → **Allow Anyway**.

Only affects manual binary downloads. npm and shell installer installs are not quarantined.

---

## macOS / Linux: `cux` not found inside Claude Code hooks

**Symptom**

`cux` works in your terminal but hooks fail with "command not found" or silently do nothing.

**Cause**

Claude Code hooks run in a non-interactive shell. Shell config files like `~/.zshrc` or `~/.bashrc` are not sourced. If `cux` lives in a directory that is only on PATH for interactive shells (e.g. via nvm), hooks cannot find it.

**Fix**

Add the PATH export to your login shell profile instead:

- macOS zsh: `~/.zprofile`
- Linux bash: `~/.profile` or `~/.bash_profile`

```bash
# Example for nvm — add to ~/.zprofile (macOS) or ~/.profile (Linux)
export NVM_DIR="$HOME/.nvm"
[ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh"
```

Then open a new terminal and re-run `cux setup` to reinstall hooks.

---

## Linux: `cux` not found after shell installer

**Symptom**

```bash
$ cux setup
bash: cux: command not found
```

**Fix**

The shell installer places the binary at `~/.local/bin/cux`. Add `~/.local/bin` to PATH:

```bash
# Add to ~/.bashrc or ~/.zshrc
export PATH="$HOME/.local/bin:$PATH"
```

Then reload: `source ~/.bashrc` or open a new terminal.

---

## Still stuck?

Open an issue at [github.com/inulute/cux/issues](https://github.com/inulute/cux/issues) and include:

- Your OS and version
- Output of `cux --version` (if it runs)
- Output of `npm prefix -g` (if you installed via npm)
- The full error message
