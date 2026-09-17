//go:build windows

package wrapper

import (
	"os"

	"golang.org/x/sys/windows"
)

// consoleState holds the console modes of the wrapper's stdin and stdout.
type consoleState struct {
	ok      bool
	in, out windows.Handle
	inMode  uint32
	outMode uint32
	haveIn  bool
	haveOut bool
}

func captureConsoleState() consoleState {
	s := consoleState{
		in:  windows.Handle(os.Stdin.Fd()),
		out: windows.Handle(os.Stdout.Fd()),
	}
	if windows.GetConsoleMode(s.in, &s.inMode) == nil {
		s.haveIn = true
	}
	if windows.GetConsoleMode(s.out, &s.outMode) == nil {
		s.haveOut = true
	}
	s.ok = s.haveIn || s.haveOut
	return s
}

func (s consoleState) restore() {
	if !s.ok {
		return
	}
	if s.haveIn {
		_ = windows.SetConsoleMode(s.in, s.inMode)
	}
	if s.haveOut {
		_ = windows.SetConsoleMode(s.out, s.outMode)
	}
}

// nudgeRepaint is a no-op on Windows: a console size cannot be changed from
// the client side of a pseudo console without desynchronising the terminal
// that owns it. The state reset in resetAfterKill is what applies here.
func nudgeRepaint(child) {}
