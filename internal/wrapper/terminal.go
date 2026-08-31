package wrapper

import (
	"io"
	"os"

	"golang.org/x/term"
)

// Claude Code turns mouse reporting on while it runs (DECSET 1000 button
// tracking, 1002 drag, 1003 any-motion, 1006 SGR coordinates) and turns it
// off again in its own teardown. When the wrapper terminates the child that
// teardown never runs, and the terminal keeps encoding every mouse movement
// as an escape sequence — `<35;78;37M` and friends typed straight into the
// shell prompt, until the user thinks to run `reset` (issue #48).
//
// The wrapper is the only thing still attached to the terminal at that
// point, so it is the only thing that can undo them. Disabling a mode that
// is already off is a no-op, which is what makes it safe to send on paths
// where claude may well have cleaned up after itself.
const (
	mouseOff   = "\x1b[?1006l\x1b[?1003l\x1b[?1002l\x1b[?1000l"
	cursorOn   = "\x1b[?25h"
	mainScreen = "\x1b[?1049l"
)

// restoreMouse undoes what a terminated child left on, between launches.
// It deliberately leaves the screen buffer alone: the wrapper's own
// narration — the wait-for-reset countdown especially — is drawn wherever
// claude left the terminal, and switching buffers underneath it would take
// the countdown with it.
func restoreMouse(w io.Writer) {
	if !stdoutIsTerminal() {
		return
	}
	_, _ = io.WriteString(w, mouseOff+cursorOn)
}

// restoreTerminal is the last write of the process. Nothing else will be
// drawn, so this one also returns to the main screen buffer in case the
// child died inside the alternate one.
func restoreTerminal(w io.Writer) {
	if !stdoutIsTerminal() {
		return
	}
	_, _ = io.WriteString(w, mouseOff+mainScreen+cursorOn)
}

// stdoutIsTerminal asks the real terminal, not the wrapper's writer: with
// `attach` on, w is a MultiWriter fanning out to mirror sockets and would
// answer for none of them.
func stdoutIsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}
