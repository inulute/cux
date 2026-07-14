//go:build !windows

package wrapper

import (
	"io"
	"os/exec"
	"reflect"
	"runtime"
	"sort"
	"syscall"
	"testing"
	"time"
)

func TestParsePSTable(t *testing.T) {
	out := []byte("  1     0\n  10    1\n  11    1\n  20   10\ngarbage line\n  30\n")
	got := parsePSTable(out)
	want := map[int][]int{0: {1}, 1: {10, 11}, 10: {20}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsePSTable = %v, want %v", got, want)
	}
}

func TestCollectDescendants(t *testing.T) {
	children := map[int][]int{1: {10, 11}, 10: {20}, 20: {30}, 11: {}}
	got := collectDescendants(children, 1)
	sort.Ints(got)
	want := []int{10, 11, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collectDescendants = %v, want %v", got, want)
	}
	if desc := collectDescendants(children, 99); desc != nil {
		t.Errorf("unknown root should have no descendants, got %v", desc)
	}
}

// TestReapStraysKillsOrphanedGrandchildren reproduces issue #3's shape:
// a wrapper script whose children outlive it. The direct child dies
// without forwarding anything; reapStrays must clean up the orphans.
func TestReapStraysKillsOrphanedGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ps-based reaping is a no-op on windows")
	}

	// sh spawns two background sleeps (grandchildren) and waits.
	cmd := exec.Command("sh", "-c", "sleep 300 & sleep 300 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Let the shell fork its children, then snapshot the tree.
	var strays []int
	for i := 0; i < 50; i++ {
		strays = descendantPIDs(cmd.Process.Pid)
		if len(strays) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(strays) < 2 {
		t.Fatalf("expected at least 2 descendants, got %v", strays)
	}

	// Kill the direct child the hard way — nothing is forwarded, the
	// sleeps become orphans still holding their file descriptors.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()

	reapStrays(strays, io.Discard)

	for _, pid := range strays {
		if processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL) // don't leak on failure
			t.Errorf("pid %d survived reapStrays", pid)
		}
	}
}