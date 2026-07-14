//go:build !windows

// Package ptyhost makes a cux session attachable. The wrapper runs
// claude on a pseudo-terminal it owns instead of inheriting the user's
// terminal directly; the host then mirrors bytes between the real
// terminal, the PTY, and any number of attached clients on a Unix
// socket — the dtach/tmux model, in-process.
//
// Because the PTY belongs to the wrapper (not to any single claude
// child), it survives the kill+resume relaunches cux performs on
// account swaps: attached viewers stay connected straight through a
// seat change.
//
// The socket speaks a minimal framed protocol, shared with `cux attach`
// and remote surfaces like cuxdeck:
//
//	[1 byte type][4 bytes big-endian length][payload]
//
//	0 out    — PTY output (host → client)
//	1 input  — keystrokes for the PTY (client → host)
//	2 resize — payload rows,cols as two big-endian uint16 (client → host)
//	3 ping   — keepalive, either direction, empty payload
//
// On attach the host nudges the PTY size (shrink one row, restore).
// Full-screen programs repaint on SIGWINCH, so the new client gets a
// clean frame without the host having to emulate a terminal.
package ptyhost

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const (
	FrameOut byte = iota
	FrameInput
	FrameResize
	FramePing
)

// Host owns the PTY pair and the attach socket for one wrapper.
type Host struct {
	ptmx *os.File // master: host reads output, writes input
	tty  *os.File // slave: children run on this

	ln       net.Listener
	sockPath string
	inputOK  bool

	mu      sync.Mutex
	clients map[net.Conn]*clientState
	local   winsize // the user's real terminal, zero when unknown
	closed  bool
}

type clientState struct{ size winsize }

type winsize struct{ rows, cols uint16 }

// New opens the PTY pair, sizes it to the current terminal, and starts
// serving attach clients on sockPath (created 0600). inputOK gates
// whether client keystrokes are forwarded to the PTY.
func New(sockPath string, inputOK bool) (*Host, error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	h := &Host{ptmx: ptmx, tty: tty, sockPath: sockPath, inputOK: inputOK,
		clients: map[net.Conn]*clientState{}}

	if w, hgt, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
		h.local = winsize{rows: uint16(hgt), cols: uint16(w)}
	}
	if h.local.rows == 0 {
		h.local = winsize{rows: 24, cols: 80}
	}
	h.applySize()

	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		return nil, err
	}
	_ = os.Chmod(sockPath, 0o600)
	h.ln = ln

	go h.acceptLoop()
	go h.watchWinch()
	return h, nil
}

// TTY exposes the slave for exec.Cmd wiring: set it as the child's
// stdin/stdout/stderr together with SysProcAttr(), once per (re)launch;
// the PTY persists across children.
func (h *Host) TTY() *os.File { return h.tty }

// TTYDup opens a FRESH slave handle for one exec.Cmd, from the same
// pts path as the persistent slave. This is what a child must run on.
//
// Why reopen instead of reuse h.tty: os/exec's Wait() closes the stdio
// files it was handed, and with Setctty the first child becoming the
// terminal's session leader tears that slave fd down when it exits. Both
// mean the shared slave can't survive a second launch — the next
// relaunch (e.g. after a rate-limit account swap) failed with "bad file
// descriptor". A brand-new open per launch keeps the master (ptmx) and
// all future relaunches unaffected; the caller wires the returned file
// as stdin/stdout/stderr and lets exec own its lifecycle.
func (h *Host) TTYDup() (*os.File, error) {
	return os.OpenFile(h.tty.Name(), os.O_RDWR, 0)
}

// SysProcAttr returns the attributes a child needs to adopt the PTY as
// its controlling terminal.
func SysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true}
}

// Pump mirrors the user's terminal into and out of the PTY. It returns
// when the PTY closes. Call from Run once; it owns os.Stdin/os.Stdout.
func (h *Host) Pump() {
	go func() { _, _ = io.Copy(h.ptmx, os.Stdin) }()
	buf := make([]byte, 32*1024)
	for {
		n, err := h.ptmx.Read(buf)
		if n > 0 {
			_, _ = os.Stdout.Write(buf[:n])
			h.broadcast(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// BroadcastWriter returns a writer that mirrors wrapper status messages
// (the "cux: …" lines printed between launches) to attached clients so
// remote viewers see the same narration the local terminal does.
func (h *Host) BroadcastWriter() io.Writer { return broadcastWriter{h} }

type broadcastWriter struct{ h *Host }

func (b broadcastWriter) Write(p []byte) (int, error) { b.h.broadcast(p); return len(p), nil }

func (h *Host) broadcast(p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if err := writeFrame(c, FrameOut, p); err != nil {
			_ = c.Close()
			delete(h.clients, c)
		}
	}
}

func (h *Host) acceptLoop() {
	for {
		conn, err := h.ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		h.clients[conn] = &clientState{}
		h.mu.Unlock()
		go h.serve(conn)
		h.redraw()
	}
}

// serve reads one client's frames until it disconnects.
func (h *Host) serve(conn net.Conn) {
	defer func() {
		h.mu.Lock()
		delete(h.clients, conn)
		h.mu.Unlock()
		_ = conn.Close()
		h.recomputeSize()
	}()
	for {
		typ, payload, err := readFrame(conn)
		if err != nil {
			return
		}
		switch typ {
		case FrameInput:
			if h.inputOK {
				_, _ = h.ptmx.Write(payload)
			}
		case FrameResize:
			if len(payload) == 4 {
				h.mu.Lock()
				h.clients[conn].size = winsize{
					rows: binary.BigEndian.Uint16(payload[0:2]),
					cols: binary.BigEndian.Uint16(payload[2:4]),
				}
				h.mu.Unlock()
				h.recomputeSize()
			}
		case FramePing:
			_ = writeFrame(conn, FramePing, nil)
		}
	}
}

// recomputeSize applies the tmux rule: the effective PTY size is the
// smallest of every participant that has declared one, so no viewer
// ever sees a clipped frame.
func (h *Host) recomputeSize() {
	h.mu.Lock()
	eff := h.local
	for _, c := range h.clients {
		if c.size.rows == 0 || c.size.cols == 0 {
			continue
		}
		if eff.rows == 0 || c.size.rows < eff.rows {
			eff.rows = c.size.rows
		}
		if eff.cols == 0 || c.size.cols < eff.cols {
			eff.cols = c.size.cols
		}
	}
	h.mu.Unlock()
	if eff.rows == 0 {
		return
	}
	_ = pty.Setsize(h.ptmx, &pty.Winsize{Rows: eff.rows, Cols: eff.cols})
}

func (h *Host) applySize() {
	_ = pty.Setsize(h.ptmx, &pty.Winsize{Rows: h.local.rows, Cols: h.local.cols})
}

// redraw nudges the PTY one row smaller and back so full-screen
// programs repaint — a fresh attach sees the current frame.
func (h *Host) redraw() {
	h.mu.Lock()
	rows, cols := h.local.rows, h.local.cols
	h.mu.Unlock()
	if rows < 2 {
		return
	}
	_ = pty.Setsize(h.ptmx, &pty.Winsize{Rows: rows - 1, Cols: cols})
	h.recomputeSize()
}

func (h *Host) watchWinch() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	for range ch {
		if w, hgt, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
			h.mu.Lock()
			h.local = winsize{rows: uint16(hgt), cols: uint16(w)}
			h.mu.Unlock()
			h.recomputeSize()
		}
	}
}

// Close tears the socket and PTY down.
func (h *Host) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	for c := range h.clients {
		_ = c.Close()
	}
	h.mu.Unlock()
	if h.ln != nil {
		_ = h.ln.Close()
	}
	_ = os.Remove(h.sockPath)
	_ = h.ptmx.Close()
	_ = h.tty.Close()
}

/* ---------- frame protocol ---------- */

var errFrameTooBig = errors.New("ptyhost: frame exceeds limit")

const maxFrame = 1 << 20

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	hdr := [5]byte{typ}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, errFrameTooBig
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// WriteFrame / ReadFrame are exported for `cux attach` and bridges.
func WriteFrame(w io.Writer, typ byte, payload []byte) error { return writeFrame(w, typ, payload) }
func ReadFrame(r io.Reader) (byte, []byte, error)            { return readFrame(r) }
