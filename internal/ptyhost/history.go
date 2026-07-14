package ptyhost

import (
	"bytes"
	"sync"
)

// maxHistory is how much recent PTY output the host keeps so a freshly
// attached client can be handed the backlog (scrollback) instead of just
// the current frame. ~512 KiB covers thousands of lines — enough to
// scroll back through, cheap to hold per session.
const maxHistory = 512 * 1024

// altScreenCarry is how many trailing bytes of one write we re-scan with
// the next, so an alt-screen toggle split across two writes is still
// seen. It only needs to cover the longest toggle sequence below.
const altScreenCarry = 8

// The DEC private toggles for the alternate screen buffer. A full-screen
// TUI enters the alt screen and repaints via absolute cursor addressing
// sized to the current terminal.
var (
	altEnter = [][]byte{[]byte("\x1b[?1049h"), []byte("\x1b[?1047h"), []byte("\x1b[?47h")}
	altExit  = [][]byte{[]byte("\x1b[?1049l"), []byte("\x1b[?1047l"), []byte("\x1b[?47l")}
)

// history is a byte ring of the most recent PTY output, replayed to each
// new client on attach so scrollback is not empty. It also tracks whether
// the app is currently on the alternate screen buffer: for a full-screen
// TUI (claude, vim, less) the raw history is a stream of absolute-cursor
// repaints sized to the host terminal, so replaying it to a differently
// sized client renders off-screen — blank. While the alt screen is
// active the host suppresses replay and lets the redraw nudge repaint the
// current screen at the client's own size instead.
type history struct {
	mu   sync.Mutex
	buf  []byte
	alt  bool   // app is currently on the alternate screen buffer
	tail []byte // trailing bytes of the last write, for split-toggle detection
}

// record appends output, trimming to the last maxHistory bytes. The trim
// re-backs the slice so the dropped prefix can be collected rather than
// pinned by a growing underlying array.
func (h *history) record(p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scanAlt(p)
	h.buf = append(h.buf, p...)
	if len(h.buf) > maxHistory {
		h.buf = append([]byte(nil), h.buf[len(h.buf)-maxHistory:]...)
	}
}

// scanAlt updates h.alt from the alt-screen toggles in p. The last toggle
// in the joined window wins, so a chunk that enters then exits (or vice
// versa) lands on its final state.
func (h *history) scanAlt(p []byte) {
	window := p
	if len(h.tail) > 0 {
		window = append(append([]byte(nil), h.tail...), p...)
	}
	lastEnter, lastExit := -1, -1
	for _, s := range altEnter {
		if i := bytes.LastIndex(window, s); i > lastEnter {
			lastEnter = i
		}
	}
	for _, s := range altExit {
		if i := bytes.LastIndex(window, s); i > lastExit {
			lastExit = i
		}
	}
	if lastEnter > lastExit {
		h.alt = true
	} else if lastExit > lastEnter {
		h.alt = false
	}
	if len(window) > altScreenCarry {
		h.tail = append(h.tail[:0], window[len(window)-altScreenCarry:]...)
	} else {
		h.tail = append(h.tail[:0], window...)
	}
}

// replay returns the backlog to hand a newly attached client, or nil when
// the app is on the alternate screen — there the redraw nudge repaints the
// current screen at the client's size, which raw history cannot do.
func (h *history) replay() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.alt {
		return nil
	}
	return h.copyLocked()
}

// snapshot returns a copy of the recorded backlog regardless of screen
// mode. Unlike replay it is not gated on the alt screen.
func (h *history) snapshot() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.copyLocked()
}

func (h *history) copyLocked() []byte {
	out := make([]byte, len(h.buf))
	copy(out, h.buf)
	return out
}
