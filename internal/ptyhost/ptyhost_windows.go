//go:build windows

// Windows attach host, built on ConPTY (the Windows pseudo-console).
// Mirrors the Unix host's contract — New/Pump/BroadcastWriter/Close plus
// the socket + frame protocol from frame.go — so cux attach and cuxdeck
// treat both platforms identically. The one shape difference is process
// launch: os/exec can't attach a ConPTY, so StartAttached spawns claude
// with CreateProcess + a PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE attribute
// and returns a `child` the wrapper drives like any other.
package ptyhost

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrUnsupported is retained for callers that referenced it; ConPTY is
// now implemented, so New no longer returns it.
var ErrUnsupported = errors.New("ptyhost: not supported")

const procThreadAttributePseudoConsole = 0x00020016

type winsize struct{ rows, cols uint16 }

type clientState struct {
	size winsize
	wmu  sync.Mutex // serializes writes to this client's conn (replay, broadcast, ping)
}

// Host owns the ConPTY and the attach socket for one wrapper.
type Host struct {
	hpc  windows.Handle // pseudo console
	inW  *os.File       // write end → ConPTY input (host writes keystrokes)
	outR *os.File       // read end ← ConPTY output (host reads screen)

	ln       net.Listener
	sockPath string
	inputOK  bool

	mu      sync.Mutex
	clients map[net.Conn]*clientState
	local   winsize
	closed  bool
	hist    history // recent output, replayed to new clients for scrollback
}

// New creates the ConPTY (sized to the current console), keeps the host
// ends of its pipes, and serves attach clients on sockPath.
func New(sockPath string, inputOK bool) (*Host, error) {
	// Two pipes: input (host → console) and output (console → host).
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, err
	}

	local := consoleSize()
	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(
		windows.Coord{X: int16(local.cols), Y: int16(local.rows)},
		inR, outW, 0, &hpc,
	); err != nil {
		for _, h := range []windows.Handle{inR, inW, outR, outW} {
			windows.CloseHandle(h)
		}
		return nil, err
	}
	// The console dup'd the read/write ends it needs; the host only keeps
	// the opposite ends.
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)

	h := &Host{
		hpc:      hpc,
		inW:      os.NewFile(uintptr(inW), "conpty-in"),
		outR:     os.NewFile(uintptr(outR), "conpty-out"),
		sockPath: sockPath,
		inputOK:  inputOK,
		clients:  map[net.Conn]*clientState{},
		local:    local,
	}

	// The attach socket is best-effort: if AF_UNIX is unavailable (older
	// Windows, or a Wine layer), claude still runs on the ConPTY — it
	// just isn't mirrored to cux attach / cuxdeck. The ConPTY itself is
	// the essential part, so a socket error must not sink the session.
	_ = os.Remove(sockPath)
	if ln, err := net.Listen("unix", sockPath); err == nil {
		_ = os.Chmod(sockPath, 0o600)
		h.ln = ln
		go h.acceptLoop()
	}
	return h, nil
}

func consoleSize() winsize {
	// Best effort: query the real console; fall back to 80x24.
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Stdout, &info); err == nil {
		cols := uint16(info.Window.Right - info.Window.Left + 1)
		rows := uint16(info.Window.Bottom - info.Window.Top + 1)
		if cols > 0 && rows > 0 {
			return winsize{rows: rows, cols: cols}
		}
	}
	return winsize{rows: 24, cols: 80}
}

// TTY / TTYDup are the Unix wiring points; on Windows the child attaches
// via StartAttached instead, so these are unused.
func (h *Host) TTY() *os.File             { return nil }
func (h *Host) TTYDup() (*os.File, error) { return nil, ErrUnsupported }

// SysProcAttr is unused on Windows (StartAttached builds its own
// STARTUPINFOEX); returning nil keeps the shared signature satisfied.
func SysProcAttr() *syscall.SysProcAttr { return nil }

// Pump mirrors the console output to the real stdout and to attached
// clients, and forwards local stdin into the console. Returns when the
// console output closes.
func (h *Host) Pump() {
	go func() { _, _ = io.Copy(h.inW, os.Stdin) }()
	buf := make([]byte, 32*1024)
	for {
		n, err := h.outR.Read(buf)
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

// SetChildPID is a no-op on Windows: ResizePseudoConsole notifies the
// attached process of size changes itself, so there is no out-of-band
// signal to deliver as there is on Unix. Present for API parity.
func (h *Host) SetChildPID(pid int) {}

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
				_, _ = h.inW.Write(payload)
			}
		case FrameResize:
			if len(payload) == 4 {
				h.mu.Lock()
				cs.size = winsize{
					rows: uint16(payload[0])<<8 | uint16(payload[1]),
					cols: uint16(payload[2])<<8 | uint16(payload[3]),
				}
				h.mu.Unlock()
				h.recomputeSize()
			}
		case FramePing:
			h.writeClient(conn, cs, FramePing, nil)
		}
	}
}

// recomputeSize applies the tmux rule: smallest declared participant
// wins, so no viewer sees a clipped frame.
// recomputeSize holds h.mu across the ConPTY resize so it can't race
// Close() destroying the pseudo-console; the closed guard makes it a
// no-op once torn down. The client set is snapshotted before the lock is
// released so the post-negotiation FrameSize broadcast (below) can't
// deadlock against writeClient's own h.mu on a failed write.
func (h *Host) recomputeSize() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
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
		h.mu.Unlock()
		return
	}
	_ = windows.ResizePseudoConsole(h.hpc, windows.Coord{X: int16(eff.cols), Y: int16(eff.rows)})
	type ref struct {
		conn net.Conn
		cs   *clientState
	}
	refs := make([]ref, 0, len(h.clients))
	for c, cs := range h.clients {
		refs = append(refs, ref{c, cs})
	}
	h.mu.Unlock()

	// Every attached client learns the size the console actually settled
	// on — see the Unix host's recomputeSize for why.
	p := []byte{byte(eff.rows >> 8), byte(eff.rows), byte(eff.cols >> 8), byte(eff.cols)}
	for _, r := range refs {
		h.writeClient(r.conn, r.cs, FrameSize, p)
	}
}

// Close tears the socket and ConPTY down.
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
	// Destroy the console under h.mu so it can't race an in-flight resize.
	if h.hpc != 0 {
		windows.ClosePseudoConsole(h.hpc)
	}
	h.mu.Unlock()
	if h.ln != nil {
		_ = h.ln.Close()
	}
	_ = os.Remove(h.sockPath)
	if h.inW != nil {
		_ = h.inW.Close()
	}
	if h.outR != nil {
		_ = h.outR.Close()
	}
}

// StartAttached launches claude attached to this ConPTY and returns a
// child the wrapper can wait on / signal. Each launch gets a fresh
// process on the same console, so attached viewers ride through account
// swaps just like on Unix.
func (h *Host) StartAttached(claudeBin string, argv, env []string) (*winChild, error) {
	// Build the command line: "bin" arg1 arg2 … (quoted).
	parts := make([]string, 0, len(argv)+1)
	parts = append(parts, quoteArg(claudeBin))
	for _, a := range argv {
		parts = append(parts, quoteArg(a))
	}
	cmdline, err := windows.UTF16PtrFromString(strings.Join(parts, " "))
	if err != nil {
		return nil, err
	}

	// STARTUPINFOEX carrying the pseudo-console attribute.
	attrList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrList.Delete()
	if err := attrList.Update(
		procThreadAttributePseudoConsole,
		unsafe.Pointer(h.hpc),
		unsafe.Sizeof(h.hpc),
	); err != nil {
		return nil, err
	}

	var si windows.StartupInfoEx
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attrList.List()

	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(
		nil, cmdline, nil, nil, false, flags,
		envBlock(env), nil, &si.StartupInfo, &pi,
	); err != nil {
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	return &winChild{proc: pi.Process, pid: int(pi.ProcessId), host: h}, nil
}

// envBlock turns "K=V" strings into a UTF-16, double-null-terminated
// environment block for CreateProcess.
func envBlock(env []string) *uint16 {
	var b []uint16
	for _, e := range env {
		u, err := windows.UTF16FromString(e)
		if err != nil {
			continue
		}
		b = append(b, u...) // u is already NUL-terminated
	}
	b = append(b, 0) // final NUL → double-NUL terminator
	return &b[0]
}

// quoteArg applies minimal CommandLineToArgv-compatible quoting.
func quoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
		case '"':
			for i := 0; i < slashes*2+1; i++ {
				b.WriteByte('\\')
			}
			slashes = 0
			b.WriteByte('"')
			continue
		default:
			slashes = 0
		}
		b.WriteRune(r)
	}
	for i := 0; i < slashes*2; i++ {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

// winChild is a ConPTY-attached claude process, satisfying wrapper.child.
type winChild struct {
	proc   windows.Handle
	pid    int
	host   *Host
	exited atomic.Bool
}

func (c *winChild) Pid() int { return c.pid }

// Signal maps os.Interrupt to a Ctrl-C byte written into the console —
// ConPTY delivers it to the child as a real ^C. Other signals are no-ops
// (Windows has no general kill-by-signal for another process group here).
func (c *winChild) Signal(os.Signal) error {
	_, err := c.host.inW.Write([]byte{0x03})
	return err
}

func (c *winChild) Kill() error { return windows.TerminateProcess(c.proc, 1) }

func (c *winChild) Exited() bool { return c.exited.Load() }

func (c *winChild) Wait() error {
	_, err := windows.WaitForSingleObject(c.proc, windows.INFINITE)
	c.exited.Store(true)
	if err != nil {
		windows.CloseHandle(c.proc)
		return err
	}
	var code uint32
	_ = windows.GetExitCodeProcess(c.proc, &code)
	windows.CloseHandle(c.proc)
	if code != 0 {
		return &exitError{code: int(code)}
	}
	return nil
}

// exitError mirrors *exec.ExitError enough for the wrapper's ExitCode
// handling (errors.As on *exec.ExitError won't match, but the wrapper
// only reads the code via its own path on Unix; on Windows a non-zero
// exit returns this and the loop treats it as a normal exit code).
type exitError struct{ code int }

func (e *exitError) Error() string { return "exit status " + itoa(e.code) }
func (e *exitError) ExitCode() int { return e.code }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
