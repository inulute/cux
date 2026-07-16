//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/ptyhost"
	"github.com/inulute/cux/internal/registry"
	"github.com/inulute/cux/internal/transcripts"
	"golang.org/x/term"
)

const detachKey = 0x1c // Ctrl+\

// cmdAttach mirrors a running cux session into this terminal — the
// tmux-attach experience without tmux. With no argument it attaches to
// the only attachable session; with one it takes the wrapper PID shown
// by `cux sessions`.
func cmdAttach(args []string) int {
	// Attach is opt-in: it runs claude on a wrapper-owned PTY, which adds
	// terminal overhead to every session, so it's off by default. If it's
	// disabled, offer to turn it on rather than failing with an opaque
	// "no attachable sessions".
	if cfg, _ := config.Load(); !cfg.Attach {
		return enableAttachPrompt()
	}
	pid, err := pickSession(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cux:", err)
		return 1
	}
	conn, err := net.Dial("unix", paths.AttachSock(pid))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cux: cannot attach to %d: %v\n(is the session running a cux build with attach support?)\n", pid, err)
		return 1
	}
	defer conn.Close()

	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cux: attach needs a terminal:", err)
		return 1
	}
	defer term.Restore(fd, old)
	fmt.Printf("cux: attached to %d — detach with Ctrl+\\\r\n", pid)

	sendSize := func() {
		if cols, rows, err := term.GetSize(fd); err == nil {
			p := []byte{byte(rows >> 8), byte(rows), byte(cols >> 8), byte(cols)}
			_ = ptyhost.WriteFrame(conn, ptyhost.FrameResize, p)
		}
	}
	sendSize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			sendSize()
		}
	}()

	done := make(chan struct{})   // host closed the connection (session ended / dropped)
	detach := make(chan struct{}) // user pressed the detach key
	go func() {                   // socket → terminal
		defer close(done)
		for {
			typ, payload, err := ptyhost.ReadFrame(conn)
			if err != nil {
				return
			}
			if typ == ptyhost.FrameOut {
				_, _ = os.Stdout.Write(payload)
			}
		}
	}()

	go func() { // terminal → socket, scanning for the detach key
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				if buf[i] == detachKey {
					close(detach)
					return
				}
			}
			if err := ptyhost.WriteFrame(conn, ptyhost.FrameInput, buf[:n]); err != nil {
				return
			}
		}
	}()

	// Exit on whichever fires first. Detach must signal directly rather
	// than wait for the output goroutine to notice conn.Close(): under
	// heavy output that goroutine is busy writing to the terminal, so a
	// detach keypress would otherwise sit undetected until the stream
	// paused.
	select {
	case <-detach:
		finishAttach(fd, old, conn, "detached")
	case <-done:
		finishAttach(fd, old, conn, "session ended")
	}
	return 0
}

// finishAttach restores the terminal and exits. The confirmation write
// is best-effort with a short timeout: under heavy output the terminal
// is backed up, so a blocking write to stdout would wedge the detach —
// the exact hang this guards against. Restoring the tty is a
// non-blocking ioctl; the returning shell prompt is the real signal.
func finishAttach(fd int, old *term.State, conn net.Conn, why string) {
	_ = term.Restore(fd, old)
	_ = conn.Close()
	printed := make(chan struct{})
	go func() {
		fmt.Printf("\r\ncux: %s\r\n", why)
		close(printed)
	}()
	select {
	case <-printed:
	case <-time.After(150 * time.Millisecond):
	}
	os.Exit(0)
}

// pickSession resolves the target wrapper PID: an explicit argument
// wins; otherwise the registry must hold exactly one attachable entry.
// enableAttachPrompt runs when `cux attach` is used while attach is
// disabled. It explains the trade-off and offers to turn it on. Enabling
// only affects sessions started afterwards — a session already running
// can't be moved onto a PTY, so it must be restarted to become
// attachable.
func enableAttachPrompt() int {
	fmt.Fprint(os.Stderr,
		"cux: attach is disabled.\n"+
			"     It runs Claude on a wrapper-owned PTY so another terminal can mirror\n"+
			"     the session — but that adds a little rendering overhead to every\n"+
			"     session, so it's off by default.\n"+
			"Enable it now? [y/N]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
		fmt.Fprintln(os.Stderr, "cux: left disabled. Enable any time with `cux config set attach true`.")
		return 0
	}
	c, err := config.Load()
	if err == nil {
		c, err = config.Set(c, "attach", "true")
	}
	if err == nil {
		err = config.Save(c)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cux: could not enable attach: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr,
		"cux: attach enabled. Sessions started from now on are attachable.\n"+
			"     A session that's already running must be restarted (a fresh `cux`) to\n"+
			"     become attachable — a running Claude can't be moved onto a PTY. Then run\n"+
			"     `cux attach` again.")
	return 0
}

func pickSession(args []string) (int, error) {
	if len(args) > 0 {
		var pid int
		if _, err := fmt.Sscanf(args[0], "%d", &pid); err != nil {
			return 0, fmt.Errorf("attach: %q is not a pid (see `cux sessions`)", args[0])
		}
		return pid, nil
	}
	var attachable []registry.Entry
	for _, e := range registry.List() {
		if e.Attachable {
			attachable = append(attachable, e)
		}
	}
	switch len(attachable) {
	case 0:
		return 0, fmt.Errorf("no attachable sessions (see `cux sessions`)")
	case 1:
		return attachable[0].PID, nil
	default:
		return promptSession(attachable)
	}
}

// promptSession lists the attachable sessions and reads a choice from
// stdin. Used when `cux attach` is run with no pid and more than one
// session is attachable, so the user can pick from names/pids directly
// instead of first running `cux sessions` to copy a pid.
func promptSession(sessions []registry.Entry) (int, error) {
	fmt.Fprintln(os.Stderr, "cux: multiple attachable sessions — select one:")
	for i, e := range sessions {
		state := e.State
		if e.Detail != "" {
			state += " (" + e.Detail + ")"
		}
		name := transcripts.FirstPrompt(e.CWD, e.SessionID, 60)
		if name == "" {
			name = filepath.Base(e.CWD)
		}
		fmt.Fprintf(os.Stderr, "  %d) %s\n        [%d] %s  seat %s  %s\n", i+1, name, e.PID, e.CWD, e.Seat, state)
	}
	fmt.Fprintf(os.Stderr, "Select 1-%d (q to cancel): ", len(sessions))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("attach: no selection made")
	}
	line = strings.TrimSpace(line)
	if line == "" || strings.EqualFold(line, "q") {
		return 0, fmt.Errorf("attach: cancelled")
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(sessions) {
		return 0, fmt.Errorf("attach: %q is not a choice between 1 and %d", line, len(sessions))
	}
	return sessions[n-1].PID, nil
}
