//go:build windows

package wrapper

import (
	"os"

	"golang.org/x/sys/windows"
)

// consoleState holds the console modes of the wrapper's stdin and stdout.
type consoleState struct {
	in, out         windows.Handle
	inMode, outMode uint32
	haveIn, haveOut bool
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
	return s
}

func (s consoleState) restore() {
	if s.haveIn {
		_ = windows.SetConsoleMode(s.in, s.inMode)
	}
	if s.haveOut {
		_ = windows.SetConsoleMode(s.out, s.outMode)
	}
}
