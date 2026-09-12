package wrapper

import (
	"io"
	"strings"
	"testing"
)

// claude exits with the terminal still on the alternate screen buffer, and
// restoreTerminal's last act is to leave that buffer — which discards
// everything drawn on it. So the resume line has to be written after the
// restore, not before. Printed first it renders for no time at all and the
// user is returned to a bare shell prompt with no way back into the
// conversation, which is the whole failure #48 set out to end.
func TestFinishSessionPrintsTheResumeLineAfterRestoringTheScreen(t *testing.T) {
	var out strings.Builder
	restore := func(w io.Writer) { _, _ = io.WriteString(w, mainScreen) }

	finishSession(&out, func() {}, restore, func(io.Writer) {}, "sid-1", false, 4242)

	got := out.String()
	restoreAt := strings.Index(got, mainScreen)
	resumeAt := strings.Index(got, "cux --resume sid-1")
	if restoreAt < 0 {
		t.Fatalf("the screen was never restored: %q", got)
	}
	if resumeAt < 0 {
		t.Fatalf("no resume line was printed: %q", got)
	}
	if restoreAt > resumeAt {
		t.Errorf("resume line was written before leaving the alternate screen and would be discarded: %q", got)
	}
}

func TestFinishSessionStaysQuietWhenThereIsNoWayBack(t *testing.T) {
	cases := []struct {
		name          string
		sessionID     string
		startupFailed bool
		wantLine      bool
	}{
		{name: "a session to resume", sessionID: "sid-1", wantLine: true},
		{name: "no session id at all", sessionID: ""},
		{
			// The line would read as reassurance directly beneath the error
			// explaining that claude never started.
			name:          "claude never started",
			sessionID:     "sid-1",
			startupFailed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			restored := false
			finishSession(&out, func() {}, func(io.Writer) { restored = true }, func(io.Writer) {}, tc.sessionID, tc.startupFailed, 1)

			if !restored {
				t.Error("the terminal must be restored on every exit path, whatever else is skipped")
			}
			if got := strings.Contains(out.String(), "cux --resume"); got != tc.wantLine {
				t.Errorf("resume line printed = %v, want %v (output %q)", got, tc.wantLine, out.String())
			}
		})
	}
}

// With attach on, claude's output reaches the terminal through a pump
// goroutine, so the child exiting does not mean its bytes have landed — its
// own alternate-screen exit can still be in flight. Arriving after the resume
// line, that switch discards it. Draining has to come first, and before the
// restore, or the ordering is merely likely rather than true.
func TestFinishSessionDrainsBeforeTouchingTheTerminal(t *testing.T) {
	var order []string
	var out strings.Builder

	finishSession(&out,
		func() { order = append(order, "drain") },
		func(io.Writer) { order = append(order, "restore") },
		func(io.Writer) { order = append(order, "support") },
		"sid-1", false, 1)

	// The support line comes last of all, after the way back is on screen.
	want := []string{"drain", "restore", "support"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("exit sequence = %v, want %v", order, want)
		}
	}
}

// A failed startup skips the resume line, and must skip the support line with
// it: asking for money directly beneath "claude could not start" is the worst
// moment there is to ask.
func TestFinishSessionDoesNotAskForSupportAfterAFailedStartup(t *testing.T) {
	asked := false
	var out strings.Builder

	finishSession(&out, func() {}, func(io.Writer) {}, func(io.Writer) { asked = true }, "sid-1", true, 1)

	if asked {
		t.Error("support was mentioned beneath a startup failure")
	}
}
