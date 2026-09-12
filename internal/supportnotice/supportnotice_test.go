package supportnotice

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func isolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, ".local", "share"))
}

// A fresh install says nothing; the first run only starts the clock.
func TestAFreshInstallIsSilentAndOnlyStartsTheClock(t *testing.T) {
	isolate(t)
	now := time.Now()

	if Due(now) {
		t.Fatal("the very first run must not ask")
	}
	// Still inside the first week, however many sessions have been exited.
	for _, d := range []time.Duration{time.Minute, 24 * time.Hour, FirstDelay - time.Hour} {
		if Due(now.Add(d)) {
			t.Errorf("asked after %v, want silence until %v", d, FirstDelay)
		}
	}
}

func TestTheFirstNoticeLandsAfterAWeekAndThenMonthly(t *testing.T) {
	isolate(t)
	start := time.Now()
	Due(start) // start the clock

	if !Due(start.Add(FirstDelay)) {
		t.Fatal("no notice after the first week")
	}
	// Having just shown one, the next window is a month — not a week.
	for _, d := range []time.Duration{time.Second, 24 * time.Hour, Interval - time.Hour} {
		if Due(start.Add(FirstDelay + d)) {
			t.Errorf("asked again %v later, want a %v gap", d, Interval)
		}
	}
	if !Due(start.Add(FirstDelay + Interval)) {
		t.Error("no notice after the monthly interval elapsed")
	}
	if Due(start.Add(FirstDelay + Interval + time.Hour)) {
		t.Error("asked twice inside one monthly window")
	}
}

// cux exits once per wrapped session and a heavy user closes many at once;
// whichever claims the window, the rest stay quiet.
func TestOnlyOneCallerInAWindowGetsTheNotice(t *testing.T) {
	isolate(t)
	start := time.Now()
	Due(start)

	shown := 0
	for i := 0; i < 14; i++ {
		if Due(start.Add(FirstDelay + time.Duration(i)*time.Millisecond)) {
			shown++
		}
	}
	if shown != 1 {
		t.Errorf("%d of 14 simultaneous exits asked, want exactly 1", shown)
	}
}

// Anything unprovable is not due.
func TestAnUnreadableStateStaysSilent(t *testing.T) {
	isolate(t)
	p := path()
	if err := os.MkdirAll(p, 0o700); err != nil { // a directory where the file goes
		t.Fatal(err)
	}
	if Due(time.Now()) {
		t.Error("asked despite being unable to read or record state")
	}
}

// A corrupt file restarts the clock rather than asking.
func TestACorruptStateRestartsTheClockRatherThanAsking(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Due(time.Now()) {
		t.Error("a corrupt state file must not produce a notice")
	}
}
