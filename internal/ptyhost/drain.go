package ptyhost

import "time"

// DrainQuiet waits until the PTY has been silent for quiet, or until max
// elapses, whichever comes first.
//
// The wrapper needs this before it writes the last thing a user reads. With
// attach on, claude's output does not go to the terminal directly — Pump
// carries it, a goroutine behind a 32 KiB read. The child exiting does not
// mean its bytes have arrived: the tail of them, claude's own `\e[?1049l`
// among them, can still be sitting in the PTY buffer unread. Writing the
// `cux --resume <id>` line to stdout at that moment races that tail, and if
// the alternate-screen exit lands after the line, the screen switch discards
// it — the same disappearance as before, now intermittent and dependent on
// scheduling, which is the worse version of the bug.
//
// Quiet is the signal available here. Waiting for Pump to return instead
// would deadlock: the wrapper keeps the slave fd open, so the master read
// blocks rather than reporting EOF, and Pump only exits once Close has run —
// which is after the point the line needs writing.
//
// Lives outside both platform files because the logic is identical; each of
// them supplies the lastOutput its own Pump updates.
//
// Both bounds are deliberately short. This sits on the exit path of an
// interactive session, so the cost of waiting is paid by a human watching a
// terminal that has stopped.
func (h *Host) DrainQuiet(quiet, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		last := h.lastOutput.Load()
		if last != 0 && time.Since(time.Unix(0, last)) >= quiet {
			return
		}
		if last == 0 {
			// Nothing ever came through; there is no tail to wait for.
			return
		}
		time.Sleep(quiet / 4)
	}
}
