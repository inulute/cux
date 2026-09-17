//go:build !windows

package wrapper

import (
	"os"

	"golang.org/x/term"
)

// consoleState holds the termios state of the wrapper's stdin.
type consoleState struct {
	state *term.State
}

func captureConsoleState() consoleState {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return consoleState{}
	}
	st, err := term.GetState(fd)
	if err != nil {
		return consoleState{}
	}
	return consoleState{state: st}
}

func (s consoleState) restore() {
	if s.state == nil {
		return
	}
	_ = term.Restore(int(os.Stdin.Fd()), s.state)
}
