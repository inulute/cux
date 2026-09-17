package wrapper

import (
	"strings"
	"testing"
)

// Every mode Claude Code switches on while it runs has to be switched off
// again after a kill, or the relaunch starts in a different terminal than the
// first launch did (relaunchstate.go). The list mirrors Claude Code's own
// mode table (CURSOR_VISIBLE, ALT_SCREEN_CLEAR, MOUSE_*, FOCUS_EVENTS,
// BRACKETED_PASTE, THEME_NOTIFY, SYNCHRONIZED_UPDATE) plus the keyboard
// protocol stacks it pushes at startup.
func TestResetAfterKillCoversEveryModeClaudeTurnsOn(t *testing.T) {
	seq := childModesOff + mainScreen + cursorOn
	for name, want := range map[string]string{
		"synchronized update": "\x1b[?2026l",
		"mouse normal":        "\x1b[?1000l",
		"mouse button":        "\x1b[?1002l",
		"mouse any":           "\x1b[?1003l",
		"mouse sgr":           "\x1b[?1006l",
		"focus events":        "\x1b[?1004l",
		"bracketed paste":     "\x1b[?2004l",
		"theme notify":        "\x1b[?2031l",
		"kitty keyboard pop":  "\x1b[<u",
		"modifyOtherKeys":     "\x1b[>4m",
		"scroll region":       "\x1b[r",
		"alternate screen":    "\x1b[?1049l",
		"cursor visible":      "\x1b[?25h",
	} {
		if !strings.Contains(seq, want) {
			t.Errorf("%s is not reset after a kill (want %q)", name, want)
		}
	}
	// Win32 input mode (9001) belongs to the pseudo console, not to claude:
	// turning it off would break keyboard input in Windows Terminal.
	if strings.Contains(seq, "9001") {
		t.Error("the reset must not touch win32-input-mode (9001)")
	}
	// The synchronized update has to end before anything else is drawn.
	if !strings.HasPrefix(seq, "\x1b[?2026l") {
		t.Error("a synchronized update left open must be closed first")
	}
}
