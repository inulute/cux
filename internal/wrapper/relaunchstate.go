package wrapper

import "io"

// childModesOff turns off everything claude enables that the terminal would
// otherwise keep. A killed claude runs no teardown, so without this the
// relaunched one starts in the alternate screen with the keyboard stacks
// still pushed, and comes up without its input bar (#58). Turning off a mode
// that is already off is a no-op, so this is safe on every path. 9001 is left
// alone: it belongs to the pseudo console, not to claude.
const childModesOff = "\x1b[?2026l" + // close a synchronized update left open mid-frame
	"\x1b[?1006l\x1b[?1003l\x1b[?1002l\x1b[?1000l" + // mouse
	"\x1b[?1004l\x1b[?2004l\x1b[?2031l" + // focus events, bracketed paste, theme notify
	"\x1b[<u\x1b[>4m" + // pop kitty flags, modifyOtherKeys back to default
	"\x1b[r\x1b[0m" // scroll region, attributes

// consoleAtStart is the termios/console state the first claude launch
// inherited. Only captured when claude runs on the wrapper's own stdin: with
// a pty host the child gets the pty slave and never touches it.
var consoleAtStart consoleState

// resetAfterKill returns the terminal to the state a fresh claude sees.
// Escape sequences first, while the console still processes them the way
// claude left it, then the captured console state.
func resetAfterKill(w io.Writer) {
	if !stdoutIsTerminal() {
		return
	}
	_, _ = io.WriteString(w, childModesOff+mainScreen+cursorOn)
	consoleAtStart.restore()
}
