package wrapper

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Reaping orphaned descendants.
//
// claudeBin is not always the claude binary itself: users wrap it in
// scripts that chain other tools (cux → headroom → claude, issue #3).
// gracefulExit signals only the direct child, and a dying shell does
// not forward signals to its children — so the grandchildren survive
// the swap, stay attached to the terminal, and fight the relaunched
// claude for stdin: keystrokes split between the two, characters
// appear and vanish. The fix is to snapshot the child's descendants
// before signalling and reap whatever is still alive after it exits.

const (
	strayTermGrace = 2 * time.Second
	strayScanDelay = 500 * time.Millisecond
)

// descendantPIDs returns every live descendant of pid by walking the
// ps table. Best-effort: returns nil on any error (including
// platforms without ps, like Windows) — the caller then simply keeps
// today's direct-child-only behavior.
func descendantPIDs(pid int) []int {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	return collectDescendants(parsePSTable(out), pid)
}

// parsePSTable turns `ps -axo pid=,ppid=` output into a parent → children map.
func parsePSTable(out []byte) map[int][]int {
	children := make(map[int][]int)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	return children
}

// collectDescendants walks the children map depth-first from root.
// The root itself is not included.
func collectDescendants(children map[int][]int, root int) []int {
	var out []int
	stack := append([]int(nil), children[root]...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		out = append(out, pid)
		stack = append(stack, children[pid]...)
	}
	return out
}

// processAlive reports whether pid still exists, via the null signal.
// On platforms where that probe is unsupported it reports false, which
// degrades to a no-op reap.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// reapStrays terminates any of the snapshotted descendants that
// outlived the direct child: SIGTERM first, a short grace period,
// then SIGKILL for whatever remains. Processes that already exited
// (the common case — claude cleans up after itself) are untouched.
func reapStrays(pids []int, w io.Writer) {
	if len(pids) == 0 {
		return
	}
	// Give the tree a moment to fall on its own after the parent died.
	time.Sleep(strayScanDelay)

	var strays []int
	for _, pid := range pids {
		if processAlive(pid) {
			strays = append(strays, pid)
		}
	}
	if len(strays) == 0 {
		return
	}
	fmt.Fprintf(w, "cux: reaping %d stray child process(es) left behind by the previous claude\n", len(strays))
	for _, pid := range strays {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}

	deadline := time.After(strayTermGrace)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			for _, pid := range strays {
				if processAlive(pid) {
					if p, err := os.FindProcess(pid); err == nil {
						_ = p.Kill()
					}
				}
			}
			return
		case <-tick.C:
			anyAlive := false
			for _, pid := range strays {
				if processAlive(pid) {
					anyAlive = true
					break
				}
			}
			if !anyAlive {
				return
			}
		}
	}
}
