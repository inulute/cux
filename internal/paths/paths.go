// Package paths centralises every on-disk location cux reads or writes,
// so the rest of the codebase never builds a path string by hand.
//
// Two roots matter:
//   - Claude Code's own state, under $HOME/.claude (and a sibling fallback).
//     cux never invents these — it follows whatever Claude Code itself uses.
//   - cux's own state, under a backup root that follows XDG on Linux and
//     falls back to $HOME/.cux elsewhere.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// OS reports a coarse platform tag for code that needs to branch on it.
type OS string

const (
	Linux   OS = "linux"
	MacOS   OS = "macos"
	Windows OS = "windows"
)

func Detect() OS {
	switch runtime.GOOS {
	case "darwin":
		return MacOS
	case "windows":
		return Windows
	default:
		return Linux
	}
}

// Home returns the user's home directory or panics — every path in this
// package is derived from it, so an unresolvable home is fatal anyway.
func Home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		panic("cux: cannot resolve home directory: " + err.Error())
	}
	return h
}

// ClaudeDir is Claude Code's state directory, $HOME/.claude.
func ClaudeDir() string { return filepath.Join(Home(), ".claude") }

// ClaudeConfig returns the path Claude Code reads its global config from.
// Newer versions use $HOME/.claude/.claude.json; older ones use $HOME/.claude.json.
// Match cc-account-switcher's resolution: prefer the new location only if
// it exists and parses; otherwise fall back to the legacy sibling.
func ClaudeConfig() string {
	primary := filepath.Join(ClaudeDir(), ".claude.json")
	if fi, err := os.Stat(primary); err == nil && !fi.IsDir() {
		return primary
	}
	return filepath.Join(Home(), ".claude.json")
}

// ClaudeCredentials returns the path to Claude Code's live credentials file
// on Linux. On macOS/Windows credentials live in the OS keystore; this path
// is informational only there.
func ClaudeCredentials() string {
	return filepath.Join(ClaudeDir(), ".credentials.json")
}

// ClaudeProjectsDir is where Claude Code persists session transcripts.
// Each cwd gets a subdir whose name is the cwd with separators replaced by '-'.
func ClaudeProjectsDir() string {
	return filepath.Join(ClaudeDir(), "projects")
}

// ProjectTranscriptDir returns the transcript directory for a given working
// directory, matching the encoding Claude Code uses (path with '/' → '-').
func ProjectTranscriptDir(cwd string) string {
	encoded := strings.ReplaceAll(cwd, string(os.PathSeparator), "-")
	if !strings.HasPrefix(encoded, "-") {
		encoded = "-" + encoded
	}
	return filepath.Join(ClaudeProjectsDir(), encoded)
}

// BackupRoot is cux's own data directory.
//
// On Linux it follows XDG: $XDG_DATA_HOME/cux, defaulting to
// $HOME/.local/share/cux. On macOS/Windows we keep things simple at
// $HOME/.cux. No legacy migration code — cux is unreleased before
// this rename, so no on-disk state under the old name exists in the
// wild.
func BackupRoot() string {
	if Detect() == Linux {
		xdg := os.Getenv("XDG_DATA_HOME")
		if xdg == "" {
			xdg = filepath.Join(Home(), ".local", "share")
		}
		return filepath.Join(xdg, "cux")
	}
	return filepath.Join(Home(), ".cux")
}

func StateFile() string     { return filepath.Join(BackupRoot(), "state.json") }
func AccountsDir() string   { return filepath.Join(BackupRoot(), "accounts") }
func RuntimeDir() string    { return filepath.Join(BackupRoot(), "runtime") }
func ClaudePIDFile() string { return filepath.Join(RuntimeDir(), "claude.pid") }
func PendingFile() string   { return filepath.Join(RuntimeDir(), "pending.json") }
func LockFile() string      { return filepath.Join(BackupRoot(), ".lock") }

// ReplayFlagFile is a one-shot file written by the wrapper before it relaunches
// Claude with a replayed prompt. The UserPromptSubmit hook reads and deletes it
// so the replayed prompt is never re-evaluated for threshold switching.
func ReplayFlagFile(pid int) string {
	return filepath.Join(RuntimeDir(), "replay-"+itoa(pid)+".flag")
}

// AccountDir is the per-account backup directory. The slot number is the
// stable identifier; the email is included only as a human-readable hint.
func AccountDir(slot int, email string) string {
	// Sanitise the email so it cannot escape the directory or contain
	// platform-illegal characters. Slot is the source of truth — email is
	// purely cosmetic.
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '@', r == '.', r == '-', r == '_', r == '+':
			return r
		default:
			return '_'
		}
	}, email)
	return filepath.Join(AccountsDir(), formatSlot(slot)+"-"+safe)
}

func formatSlot(n int) string {
	// Two-digit zero-padded so directory listings sort naturally up to 99
	// accounts. We don't expect anyone to manage that many.
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
