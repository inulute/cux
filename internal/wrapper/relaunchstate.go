package wrapper

// Handing the terminal to a relaunched claude in the state a fresh start sees.
//
// A killed claude runs no teardown. Before this, the wrapper undid only mouse
// tracking and the cursor (restoreMouse), so a relaunched claude started in a
// terminal that differed from the one it got on the first launch:
//
//   - still in the alternate screen, so its own ?1049h became a nested entry;
//   - focus events (1004), bracketed paste (2004) and theme notifications
//     (2031) still on;
//   - the kitty keyboard flags it pushed (CSI > 5 u) and modifyOtherKeys
//     (CSI > 4 ; 2 m) never popped;
//   - a synchronized update (2026) possibly left open mid-frame;
//   - on Windows the console input/output modes, on Unix the termios state,
//     as the killed process had set them (raw input, no newline translation).
//
// The visible symptom was a relaunched session with its input bar missing
// until the window was resized - a resize makes claude re-measure and repaint
// from scratch, which is exactly what its first frame should have been able
// to do on its own.
//
// resetAfterKill restores all of it, in this order: escape sequences first
// (while the console still processes VT sequences the way claude left it),
// then the console/termios state captured when the wrapper started. Turning
// off a mode that is already off is a no-op, and popping an empty keyboard
// stack is ignored, which is what makes this safe on every path.

import (
	"io"
	"sync/atomic"
)

const (
	// Everything claude turns on that the terminal would otherwise keep.
	childModesOff = "\x1b[?2026l" + // end a synchronized update left open mid-frame
		"\x1b[?1006l\x1b[?1003l\x1b[?1002l\x1b[?1000l" + // mouse
		"\x1b[?1004l" + // focus events
		"\x1b[?2004l" + // bracketed paste
		"\x1b[?2031l" + // theme change notifications
		"\x1b[<u" + // pop the kitty keyboard flags claude pushed
		"\x1b[>4m" + // modifyOtherKeys back to default
		"\x1b[r" + // full-screen scroll region
		"\x1b[0m" // attributes
)

// consoleAtStart is the console/termios state when the wrapper started, i.e.
// the state the first claude launch inherited. Set once in Run.
var consoleAtStart consoleState

// lastChildKilled is set when the wrapper had to kill claude, so the next
// launch knows the terminal was left in claude's state rather than cleaned up.
var lastChildKilled atomic.Bool

// resetAfterKill returns the terminal to the state a fresh claude launch sees.
func resetAfterKill(w io.Writer) {
	if !stdoutIsTerminal() {
		return
	}
	_, _ = io.WriteString(w, childModesOff+mainScreen+cursorOn)
	consoleAtStart.restore()
}
