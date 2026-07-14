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
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
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
	hist    history // recent output, replayed to new clients for scrollback
}

type clientState struct {
	size winsize
	wmu  sync.Mutex // serializes writes to this client's conn (replay, broadcast, ping)
}

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

// SysProcAttr returns the attributes for launching claude on the PTY.
//
// Setsid puts claude in its own session (isolated from cux's own
// terminal). We deliberately do NOT set Setctty: making the slave the
// child's controlling terminal means the child, as session leader,
// revokes that terminal when it exits — which lands on the shared master
// as EOF and kills Pump(), so the next relaunch (a rate-limit resume)
// writes into a master nobody reads and hangs. Without Setctty the
// master stays alive across launches. claude still sees a real tty on
// its stdio (isatty true; size/raw handled via the master), and Ctrl-C /
// input arrive as bytes written into the PTY, so no controlling terminal
// is needed.
func SysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// Pump mirrors the user's terminal into and out of the PTY. It returns
// when the PTY closes. Call from Run once; it owns os.Stdin/os.Stdout.
func (h *Host) Pump() {
	go func() { _, _ = io.Copy(h.ptmx, os.Stdin) }()
	buf := make([]byte, 32*1024)
	for {
		n, err := h.ptmx.Read(buf)
		if n > 0 {
			h.hist.record(buf[:n])
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

// writeClient sends one frame to a client, serialized with every other
// write to the same conn via its clientState.wmu — writeFrame emits a
// header then a payload as two writes, so without this lock a broadcast
// could interleave between another writer's header and payload and corrupt
// the stream. On error the client is dropped.
func (h *Host) writeClient(conn net.Conn, cs *clientState, typ byte, p []byte) {
	cs.wmu.Lock()
	err := writeFrame(conn, typ, p)
	cs.wmu.Unlock()
	if err != nil {
		h.mu.Lock()
		delete(h.clients, conn)
		h.mu.Unlock()
		_ = conn.Close()
	}
}

func (h *Host) broadcast(p []byte) {
	// Snapshot the client set, then write outside h.mu so one slow client
	// can't stall the whole broadcast (or block accept) while holding it.
	h.mu.Lock()
	type ref struct {
		conn net.Conn
		cs   *clientState
	}
	refs := make([]ref, 0, len(h.clients))
	for c, cs := range h.clients {
		refs = append(refs, ref{c, cs})
	}
	h.mu.Unlock()
	for _, r := range refs {
		h.writeClient(r.conn, r.cs, FrameOut, p)
	}
}

func (h *Host) acceptLoop() {
	for {
		conn, err := h.ln.Accept()
		if err != nil {
			return
		}
		cs := &clientState{}
		// Hold the client's write lock across registration and replay so
		// the backlog is the first thing written to this conn: any
		// broadcast that races the new client blocks on wmu until the
		// replay is out, keeping scrollback strictly before live output.
		cs.wmu.Lock()
		h.mu.Lock()
		h.clients[conn] = cs
		replay := h.hist.replay()
		h.mu.Unlock()
		if len(replay) > 0 {
			_ = writeFrame(conn, FrameOut, replay)
		}
		cs.wmu.Unlock()
		go h.serve(conn, cs)
		h.redraw()
	}
}

// serve reads one client's frames until it disconnects. The backlog replay
// (scrollback) already went out in acceptLoop; here we only read.
func (h *Host) serve(conn net.Conn, cs *clientState) {
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
				cs.size = winsize{
					rows: binary.BigEndian.Uint16(payload[0:2]),
					cols: binary.BigEndian.Uint16(payload[2:4]),
				}
				h.mu.Unlock()
				h.recomputeSize()
			}
		case FramePing:
			h.writeClient(conn, cs, FramePing, nil)
		}
	}
}

// recomputeSize applies the tmux rule: the effective PTY size is the
// smallest of every participant that has declared one, so no viewer
// ever sees a clipped frame.
// recomputeSize holds h.mu across the ioctl. pty.Setsize reads the ptmx
// fd via Fd(), which bypasses os.File's own lock, so it must be serialized
// against Close() closing that fd — both happen under h.mu, and the closed
// guard makes it a no-op once torn down.
func (h *Host) recomputeSize() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
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
	if eff.rows == 0 {
		return
	}
	_ = pty.Setsize(h.ptmx, &pty.Winsize{Rows: eff.rows, Cols: eff.cols})
}

func (h *Host) applySize() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	_ = pty.Setsize(h.ptmx, &pty.Winsize{Rows: h.local.rows, Cols: h.local.cols})
}

// redraw nudges the PTY one row smaller and back so full-screen
// programs repaint — a fresh attach sees the current frame.
func (h *Host) redraw() {
	h.mu.Lock()
	rows, cols := h.local.rows, h.local.cols
	if h.closed || rows < 2 {
		h.mu.Unlock()
		return
	}
	_ = pty.Setsize(h.ptmx, &pty.Winsize{Rows: rows - 1, Cols: cols})
	h.mu.Unlock()
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
	// Close the PTY under h.mu so it can't race an in-flight Setsize ioctl.
	_ = h.ptmx.Close()
	_ = h.tty.Close()
	h.mu.Unlock()
	if h.ln != nil {
		_ = h.ln.Close()
	}
	_ = os.Remove(h.sockPath)
}
