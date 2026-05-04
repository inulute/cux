# Windows

## The most common problem: `cux` not recognised after npm install

```
C:\Users\Admin>cux setup
'cux' is not recognized as an internal or external command,
operable program or batch file.
```

**Root cause:** npm's global bin directory is not on your `Path`. This is common on fresh Windows installs — Windows does not add it automatically.

### Step 1 — find your npm global bin directory

Open cmd or PowerShell:

```cmd
npm prefix -g
```

Typical output:

```
C:\Users\Admin\AppData\Roaming\npm
```

### Step 2 — add it to Path

Pick one method:

**Option A — Current session only (quick test)**

```powershell
$env:Path += ";$(npm prefix -g)"
cux --version
```

If `cux --version` prints the version, the fix works. Open a new terminal to make it permanent (Option B or C).

**Option B — GUI (permanent)**

1. Press `Win + R`, type `sysdm.cpl`, press Enter.
2. Go to **Advanced** → **Environment Variables**.
3. Under **User variables**, select `Path` and click **Edit**.
4. Click **New** and paste the path from Step 1 (e.g. `C:\Users\Admin\AppData\Roaming\npm`).
5. Click OK on all dialogs.
6. Open a **new** terminal window and run `cux --version`.

**Option C — PowerShell one-liner (permanent)**

```powershell
[Environment]::SetEnvironmentVariable(
  "Path",
  "$([Environment]::GetEnvironmentVariable('Path','User'));$(npm prefix -g)",
  "User"
)
```

Open a new terminal after running this — existing windows keep the old `Path`.

---

## cmd.exe vs PowerShell vs Windows Terminal

All three work once `Path` is set. There is no cux-specific difference between them.

If you use **Windows Terminal** with multiple profiles (cmd, PowerShell, Git Bash), each profile reads `Path` from the environment at startup, so you only need to set it once via Option B or C.

---

## Native Windows vs WSL

| | Native Windows (cmd / PowerShell) | WSL (Ubuntu, Debian, etc.) |
|---|---|---|
| Install via npm | `npm install -g @inulute/cux` | `npm install -g @inulute/cux` |
| Binary downloaded | `cux-windows-amd64.exe` | `cux-linux-amd64` or `cux-linux-arm64` |
| PATH to fix | Windows `Path` (see above) | Linux `PATH` (see [Linux guide](./linux.md)) |
| Credential storage | Windows Credential Manager | File (`~/.local/share/cux/`) |
| `curl … \| sh` installer | Does not work | Works |

**Do not mix** a Windows npm install with WSL usage. Install cux inside WSL with the Linux npm or the shell installer — do not try to call the Windows binary from WSL.

---

## Manual binary install on Windows

If you cannot use npm or the shell installer:

1. Download `cux-windows-amd64.exe` from the [releases page](https://github.com/inulute/cux/releases/latest).
2. Rename it to `cux.exe`.
3. Move it to a directory that is already on your `Path`, for example:
   - `C:\Windows\System32\` (system-wide, requires admin)
   - `C:\Users\<YourName>\bin\` (create this folder and add it to your user `Path`)
4. Open a new terminal and run `cux --version`.

---

## Postinstall failed to download the binary

If npm install succeeds but running `cux` prints:

```
cux: binary not found at C:\...\cux.exe.
Postinstall may have failed; try `npm install -g @inulute/cux --force`
```

Re-trigger the postinstall:

```cmd
npm install -g @inulute/cux --force
```

If you are behind a corporate proxy that blocks GitHub, set the proxy first:

```cmd
set HTTPS_PROXY=http://proxy.corp.example.com:8080
npm install -g @inulute/cux --force
```

Or download the binary manually from the releases page and drop it at the path shown in the error.

---

## Windows Credential Manager

On Windows, `cux add` stores Claude credentials in **Windows Credential Manager** (not a plain file). You can view or remove entries there:

1. Open **Control Panel** → **Credential Manager** → **Windows Credentials**.
2. Look for entries named `cux/<email>`.

Removing a credential here is equivalent to `cux remove <email>`.

---

## Auto-resume on native Windows

On native Windows, `cux` hard-terminates `claude.exe` on a swap because Go cannot send `SIGINT` cross-process on Windows. The `Stop` hook still flushes the transcript before the wrapper acts, so the resumed conversation is intact. If you see unexpected behaviour, [open an issue](https://github.com/inulute/cux/issues).

On WSL, normal `SIGINT` is used and behaviour matches Linux/macOS exactly.
