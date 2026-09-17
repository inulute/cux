//go:build !windows

package wrapper

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
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

// Timings for the repaint nudge after a relaunch. The first comes once claude
// has had time to install its resize handling; the second covers a slow start
// (MCP servers, hooks).
var nudgeDelays = []time.Duration{3 * time.Second, 8 * time.Second}

var nudgeHold = 150 * time.Millisecond

// nudgeRepaint makes a relaunched claude re-measure and repaint, the same
// thing a user does by resizing the window. A bare SIGWINCH is not enough:
// Bun only emits "resize" when the size actually changed. So the tty is
// narrowed by one column and restored - unless something else changed the
// size in between, in which case the real size is left alone.
func nudgeRepaint(ch child) {
	fd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) {
		return
	}
	go nudgeFd(fd, ch)
}

// nudgeFd does the work on a given terminal fd; split out so it can be tested
// on a real pseudo terminal.
func nudgeFd(fd int, ch child) {
	for _, d := range nudgeDelays {
		time.Sleep(d)
		if ch.Exited() {
			return
		}
		ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		if err != nil || ws.Col < 2 {
			return
		}
		orig := *ws
		narrow := orig
		narrow.Col--
		if unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &narrow) != nil {
			return
		}
		time.Sleep(nudgeHold)
		if now, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err == nil &&
			now.Col == narrow.Col && now.Row == narrow.Row {
			_ = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &orig)
		}
	}
}
