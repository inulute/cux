# Installation

Full docs online: **https://cux.inulute.com/docs**

Three ways to install `cux`. Pick the one that fits your setup.

---

## Option 1 — npm (recommended)

Works on Linux, macOS, and Windows. Requires Node.js 18 or newer.

```bash
npm install -g @inulute/cux
```

After install, run the setup command once:

```bash
cux setup
```

> **Windows users:** If `cux` is not recognized after install, see [Windows PATH fix](#windows-path-fix) below.

> **Corporate proxy / offline:** If the postinstall download fails, see [Postinstall failed](#postinstall-failed-to-download-the-binary).

> **nvm / fnm users:** If `cux` is not found inside Claude Code hooks, see the [macOS](./macos.md#nvm--fnm-path-ordering) or [Linux](./linux.md#nvm-path-ordering) guide.

---

## Option 2 — Shell installer

Works on Linux, macOS, WSL, and Git Bash on Windows. Does **not** work in native PowerShell or cmd.exe.

```bash
curl -fsSL https://raw.githubusercontent.com/inulute/cux/main/scripts/install.sh | sh
```

---

## Option 3 — Manual binary

Works everywhere, including native Windows without Node.js.

1. Go to the [releases page](https://github.com/inulute/cux/releases/latest) and download the binary for your platform:

   | Platform | File |
   |---|---|
   | Linux x64 | `cux-linux-amd64` |
   | Linux ARM64 | `cux-linux-arm64` |
   | macOS x64 | `cux-darwin-amd64` |
   | macOS ARM64 (Apple Silicon) | `cux-darwin-arm64` |
   | Windows x64 | `cux-windows-amd64.exe` |

2. **Linux / macOS:** make it executable and move it onto your PATH:
   ```bash
   chmod +x cux-linux-amd64        # or cux-darwin-arm64, etc.
   mv cux-linux-amd64 ~/.local/bin/cux
   ```

3. **Windows:** rename the file to `cux.exe` and place it in a directory that is on your `Path` environment variable (e.g. `C:\Windows\System32` or a custom tools folder you already have on `Path`).

---

## After install

Run `cux setup` once to register the Claude Code hooks and slash commands:

```bash
cux setup
```

Then restart Claude Code so it picks up the new hooks.

---

## Windows PATH fix

If you installed via npm and `cux` is not recognised, npm's global bin directory is most likely missing from your `Path`.

**Step 1 — Find the npm global bin directory**

Open cmd or PowerShell and run:

```cmd
npm prefix -g
```

The output is your npm global prefix, for example:

```
C:\Users\Admin\AppData\Roaming\npm
```

**Step 2 — Add it to your Path**

Option A — PowerShell (current session only, good for a quick test):

```powershell
$env:Path += ";$(npm prefix -g)"
cux --version
```

Option B — Permanently via System Properties:

1. Press `Win + R`, type `sysdm.cpl`, press Enter.
2. Go to **Advanced** → **Environment Variables**.
3. Under **User variables**, find `Path` and click **Edit**.
4. Click **New** and paste the path from Step 1 (e.g. `C:\Users\Admin\AppData\Roaming\npm`).
5. Click OK on all dialogs.
6. Open a **new** terminal window and run `cux --version`.

Option C — PowerShell (permanent, one-liner):

```powershell
[Environment]::SetEnvironmentVariable(
  "Path",
  "$([Environment]::GetEnvironmentVariable('Path','User'));$(npm prefix -g)",
  "User"
)
```

Open a new terminal after running this.

**Step 3 — Verify**

```cmd
cux --version
```

If it prints the version, the fix worked. Now run `cux setup` to finish the installation.

---

## Postinstall failed to download the binary

If `cux` runs but prints "binary not found", the postinstall script could not reach GitHub.

Re-trigger it:

```bash
npm install -g @inulute/cux --force
```

**Corporate proxy:**

```bash
# Set proxy before install
export HTTPS_PROXY=http://proxy.corp.example.com:8080   # Linux/macOS
set HTTPS_PROXY=http://proxy.corp.example.com:8080      # Windows cmd
npm install -g @inulute/cux --force
```

**Fully offline:** Download the binary directly from the [releases page](https://github.com/inulute/cux/releases/latest) and drop it at the path shown in the error message (`…/bin/cux` on Linux/macOS, `…\bin\cux.exe` on Windows).

---

## WSL

Run `npm install -g @inulute/cux` inside WSL — it downloads the Linux binary. Do not use the Windows cux binary from within WSL. See [Linux guide](./linux.md#wsl-windows-subsystem-for-linux) for PATH and interop notes.

---

## Verify the installation end-to-end

After `cux setup`, check that everything is wired up:

```bash
cux status
```

You should see the current login state. If you see an error, check the [troubleshooting guide](./troubleshooting.md).

---

## Per-platform deep dives

- [Windows](./windows.md) — PATH fix, cmd/PS/WSL, Credential Manager
- [macOS](./macos.md) — Gatekeeper, nvm, Keychain
- [Linux](./linux.md) — PATH, nvm, WSL, distro notes
