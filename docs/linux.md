# Linux

## Installation

**npm:**
```bash
npm install -g @inulute/cux
cux setup
```

**Shell installer:**
```bash
curl -fsSL https://raw.githubusercontent.com/inulute/cux/main/scripts/install.sh | sh
```

**Manual binary:**
```bash
# Download cux-linux-amd64 or cux-linux-arm64 from releases
chmod +x cux-linux-amd64
mv cux-linux-amd64 ~/.local/bin/cux
```

---

## PATH — `~/.local/bin` not on PATH

If you used the shell installer or placed the binary at `~/.local/bin/cux` and the command is not found:

```bash
echo $PATH | tr ':' '\n' | grep local
```

If nothing prints, add `~/.local/bin` to your PATH:

```bash
# Add to ~/.bashrc (bash) or ~/.zshrc (zsh)
export PATH="$HOME/.local/bin:$PATH"
```

Then reload: `source ~/.bashrc` or open a new terminal.

---

## nvm PATH ordering

If Node.js was installed via **nvm**, the npm global bin resolves inside nvm's version directory (e.g. `~/.nvm/versions/node/v22.x.x/bin`). This directory is added to `PATH` by nvm's init script.

**If hooks fail to find `cux`** (Claude Code hooks run in a non-interactive shell):

```bash
# Add to ~/.profile (login shells — sourced by non-interactive login shells)
export NVM_DIR="$HOME/.nvm"
[ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh"
```

Note: `~/.bashrc` is sourced for interactive shells only. `~/.profile` or `~/.bash_profile` is sourced for login shells, which hooks use.

---

## WSL (Windows Subsystem for Linux)

WSL behaves like Linux. Use the Linux npm install or the shell installer inside WSL — do not use the Windows cux binary from within WSL.

**Common issue:** `npm` inside WSL might resolve to the Windows npm if the Windows `PATH` leaks into WSL. Check:

```bash
which npm
# Should be /usr/bin/npm or ~/.nvm/..., NOT /mnt/c/...
```

If it shows `/mnt/c/...`, Windows PATH is leaking. You can suppress it in `/etc/wsl.conf`:

```ini
[interop]
appendWindowsPath = false
```

Then restart the WSL instance: `wsl --shutdown` in a Windows terminal, then reopen WSL.

---

## File permissions

If you placed the binary manually and get `Permission denied`:

```bash
chmod +x ~/.local/bin/cux
```

---

## Systemd user session notes

If Claude Code is launched via a systemd user service or from a desktop application (not a login shell), environment variables set in `~/.bashrc` or `~/.zshrc` may not be available.

Fix: set `PATH` in `~/.profile` or configure the systemd unit to set `Environment=PATH=...`.

---

## Credential storage on Linux

On Linux, cux stores credentials in plain files under `~/.local/share/cux/accounts/` with `0600` permissions (owner read/write only). No keychain daemon is required.

---

## Distro notes

| Distro | Notes |
|---|---|
| Ubuntu / Debian | `apt` node is often old; install via nvm or NodeSource for Node 18+ |
| Fedora / RHEL | `dnf` node may be Node 16; upgrade via nvm |
| Arch | `pacman -S nodejs npm` installs a current version; global bin at `/usr/bin` |
| Alpine (Docker) | Use the shell installer or manual binary; npm global bin at `/usr/local/bin` |
| NixOS | Install via npm in a dev shell or use the manual binary; PATH set by Nix environment |
