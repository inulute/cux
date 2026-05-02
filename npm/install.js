// Postinstall: detect platform and download the matching cux binary
// from the GitHub release that matches this package's version.
//
// Pattern follows esbuild / swc / biome — the npm package is a thin
// wrapper around a precompiled Go binary. No native compile, no extra
// runtime deps; Node 18+ is enough thanks to global fetch.
//
// Failure here does not abort `npm install` for the rest of the
// dependency tree; we exit 0 so peers don't blow up. The bin shim
// detects a missing binary at runtime and prints a clear error.

import { createWriteStream, existsSync, mkdirSync, chmodSync, createReadStream, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { createHash } from "node:crypto";
import { pipeline } from "node:stream/promises";

const here = dirname(fileURLToPath(import.meta.url));
const pkg = JSON.parse(readFileSync(join(here, "package.json"), "utf8"));
const version = pkg.version;

// Banner. Mirrors internal/branding/branding.go and scripts/install.sh.
// Suppressed by:
//   - npm's --silent / loglevel < info (we go through console.error so
//     it lands in postinstall output by default)
//   - the user setting CUX_NO_BANNER=1
//   - non-TTY stderr — we don't want this leaking into CI logs unprompted
function printBanner() {
  if (process.env.CUX_NO_BANNER) return;
  if (!process.stderr.isTTY) return;
  // Use ASCII fallback on Windows unless the user opted in: legacy
  // conhost can mangle the Unicode box-drawing glyphs.
  const unicode =
    process.platform !== "win32" ||
    (process.env.CUX_BANNER || "").toLowerCase() === "unicode";
  const art = unicode
    ? [
        "",
        "  ██████╗ ██╗   ██╗ ██╗  ██╗",
        " ██╔════╝ ██║   ██║ ╚██╗██╔╝",
        " ██║      ██║   ██║  ╚███╔╝",
        " ██║      ██║   ██║  ██╔██╗",
        " ╚██████╗ ╚██████╔╝ ██╔╝ ██╗",
        "  ╚═════╝  ╚═════╝  ╚═╝  ╚═╝",
        "",
      ]
    : [
        "",
        " ##### ##  ##  ##  ##",
        "##     ##  ##   ####",
        "##     ##  ##    ##",
        "##     ##  ##   ####",
        " #####  ####    ##  ##",
        "",
      ];
  console.error(art.join("\n"));
  console.error(" CUX: Run multiple Claude Code Pro/Max accounts as one\n");
}

printBanner();

// Map Node's platform / arch identifiers to the Go release artefact
// names produced by the cux release workflow.
const targets = {
  "linux-x64": "cux-linux-amd64",
  "linux-arm64": "cux-linux-arm64",
  "darwin-x64": "cux-darwin-amd64",
  "darwin-arm64": "cux-darwin-arm64",
  "win32-x64": "cux-windows-amd64.exe",
};

const key = `${process.platform}-${process.arch}`;
const asset = targets[key];

if (!asset) {
  console.error(
    `cux: no prebuilt binary for ${key}. ` +
    "Open an issue at https://github.com/inulute/cux/issues, " +
    "or build from source: https://github.com/inulute/cux#building-from-source",
  );
  process.exit(0); // soft-fail; peers shouldn't break
}

const repo = process.env.CUX_RELEASE_REPO || "inulute/cux";
const baseURL = `https://github.com/${repo}/releases/download/v${version}`;

const binDir = join(here, "bin");
const binPath = join(binDir, process.platform === "win32" ? "cux.exe" : "cux");
const sumPath = `${binPath}.sha256`;

if (!existsSync(binDir)) {
  mkdirSync(binDir, { recursive: true });
}

async function download(url, dest) {
  const r = await fetch(url, { redirect: "follow" });
  if (!r.ok) {
    throw new Error(`HTTP ${r.status} fetching ${url}`);
  }
  await pipeline(r.body, createWriteStream(dest));
}

async function sha256(path) {
  const hash = createHash("sha256");
  await pipeline(createReadStream(path), hash);
  return hash.digest("hex");
}

async function main() {
  const binURL = `${baseURL}/${asset}`;
  const sumURL = `${binURL}.sha256`;
  console.error(`cux: downloading ${asset} from ${binURL}`);
  await download(binURL, binPath);

  // The release workflow publishes a sibling .sha256. Verifying it is
  // a cheap defence against transport corruption and a basic supply-chain
  // sanity check; if the sidecar is missing we still proceed (older
  // releases may not have it) but warn.
  try {
    await download(sumURL, sumPath);
    const expected = readFileSync(sumPath, "utf8").trim().split(/\s+/)[0];
    const actual = await sha256(binPath);
    if (expected && actual !== expected) {
      console.error(
        `cux: SHA256 mismatch on downloaded binary (expected ${expected}, got ${actual}).`,
      );
      process.exit(0); // soft-fail; bin shim will surface a clearer error
    }
  } catch (e) {
    console.error(`cux: skipping checksum verification (${e.message})`);
  }

  if (process.platform !== "win32") {
    chmodSync(binPath, 0o755);
  }
  console.error(`cux ${version} installed at ${binPath}`);
  console.error("");
  console.error("cux: next steps");
  console.error("  1. Run `cux setup` to install /switch, /cux:* and Claude Code hooks.");
  console.error("  2. Run `cux add` while logged in to each Claude account.");
  console.error("  3. Start Claude with `cux` instead of `claude`.");
}

main().catch((e) => {
  console.error(`cux: postinstall failed: ${e.message || e}`);
  console.error(
    "If this persists, you can grab the binary directly from " +
    `https://github.com/${repo}/releases/tag/v${version}`,
  );
  process.exit(0); // soft-fail per pattern above
});
