#!/usr/bin/env node
// Bin shim: forward all argv + stdio to the postinstall-downloaded
// cux binary. Keeps the npm wrapper transparent — the user types
// `cux` and gets the native binary's behaviour (interactive TTY, exit
// codes, signals) untouched.

import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const binName = process.platform === "win32" ? "cux.exe" : "cux";
const binPath = join(here, binName);

if (!existsSync(binPath)) {
  console.error(
    `cux: binary not found at ${binPath}.\n` +
    "Postinstall may have failed; try `npm install -g @inulute/cux --force`, " +
    "or download the binary manually from " +
    "https://github.com/inulute/cux/releases",
  );
  process.exit(1);
}

const result = spawnSync(binPath, process.argv.slice(2), {
  stdio: "inherit",
  // Inherit env so CUX_*, CLAUDE_*, etc. propagate to the child.
});

if (result.error) {
  console.error("cux:", result.error.message);
  process.exit(1);
}
process.exit(result.status ?? 0);
