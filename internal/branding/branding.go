// Package branding holds the cux ASCII banner shown at install / setup
// time. Centralising the literal here means install.sh, the npm
// postinstall, and `cux setup` can never drift from each other.
//
// The art is the "ANSI Shadow" / FIGlet block style. When updating it,
// keep the Unicode box-drawing characters intact (they are not ASCII
// "dashes"), and verify it still renders correctly on Windows
// terminals that don't auto-detect UTF-8 — `cux setup` falls back to
// a plain "cux" header when stdout looks like it can't render the
// glyphs.
package branding

import (
	"io"
	"os"
	"runtime"
	"strings"
)

// Banner is the multi-line ASCII art logo. Leading newline omitted so
// callers can decide whether to pad the output above and below.
const Banner = "" +
	"  ██████╗ ██╗   ██╗ ██╗  ██╗\n" +
	" ██╔════╝ ██║   ██║ ╚██╗██╔╝\n" +
	" ██║      ██║   ██║  ╚███╔╝ \n" +
	" ██║      ██║   ██║  ██╔██╗ \n" +
	" ╚██████╗ ╚██████╔╝ ██╔╝ ██╗\n" +
	"  ╚═════╝  ╚═════╝  ╚═╝  ╚═╝\n"

// FallbackBanner is what we emit when the terminal can't be trusted
// to render the box-drawing glyphs. Plain ASCII so it's safe everywhere.
const FallbackBanner = "" +
	" ##### ##  ##  ##  ##\n" +
	"##     ##  ##   ####\n" +
	"##     ##  ##    ##\n" +
	"##     ##  ##   ####\n" +
	" #####  ####    ##  ##\n"

// Tagline is the one-line strapline shown under the banner.
const Tagline = "CUX: Run multiple Claude Code Pro/Max accounts as one"

// Print writes the banner + tagline to w, using the fallback when the
// runtime/locale suggests Unicode might not render. Always ends with a
// trailing newline so callers can keep printing without an extra "".
func Print(w io.Writer) {
	art := Banner
	if !canRenderUnicode() {
		art = FallbackBanner
	}
	_, _ = io.WriteString(w, "\n"+art+"\n "+Tagline+"\n\n")
}

// canRenderUnicode is conservative: on Windows we render the fallback
// unless the user explicitly opted in via $CUX_BANNER=unicode. The
// modern Windows Terminal handles the glyphs fine, but the legacy
// conhost / older PowerShell often does not, and an ASCII fallback is
// preferable to mojibake on a user's first impression.
func canRenderUnicode() bool {
	if v := os.Getenv("CUX_BANNER"); v != "" {
		return strings.EqualFold(v, "unicode")
	}
	if runtime.GOOS == "windows" {
		return false
	}
	// On POSIX, trust the locale. UTF-8 in any of LC_ALL / LC_CTYPE /
	// LANG is good enough — these are the variables Go itself, glibc,
	// and most terminals consult.
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(k); strings.Contains(strings.ToUpper(v), "UTF-8") || strings.Contains(strings.ToUpper(v), "UTF8") {
			return true
		}
	}
	// Default to Unicode on POSIX when locale is unset — most modern
	// Linux/macOS terminals are UTF-8 even without explicit env vars.
	return true
}
