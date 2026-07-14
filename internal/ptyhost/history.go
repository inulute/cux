package ptyhost

import "sync"

// maxHistory is how much recent PTY output the host keeps so a freshly
// attached client can be handed the backlog (scrollback) instead of just
// the current frame. ~512 KiB covers thousands of lines — enough to
// scroll back through, cheap to hold per session.
const maxHistory = 512 * 1024

// history is a byte ring of the most recent PTY output, replayed to each
// new client on attach. Without it a viewer only ever sees output that
// arrives after it connects, so scrollback is empty.
type history struct {
	mu  sync.Mutex
	buf []byte
}

// record appends output, trimming to the last maxHistory bytes. The trim
// re-backs the slice so the dropped prefix can be collected rather than
// pinned by a growing underlying array.
func (h *history) record(p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf = append(h.buf, p...)
	if len(h.buf) > maxHistory {
		h.buf = append([]byte(nil), h.buf[len(h.buf)-maxHistory:]...)
	}
}

// snapshot returns a copy of the current backlog for replay.
func (h *history) snapshot() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]byte, len(h.buf))
	copy(out, h.buf)
	return out
}
