package usage

import (
	"testing"
	"time"
)

func ptr(w Window) *Window { return &w }

func TestIsOverThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cases := []struct {
		name     string
		u        AccountUsage
		t        Thresholds
		wantOver bool
		wantSub  string // substring expected in reason
	}{
		{
			name: "7d crosses",
			u: AccountUsage{
				SevenDay: ptr(Window{Utilization: 96, ResetsAt: &now}),
				FiveHour: ptr(Window{Utilization: 20}),
				PolledAt: now,
			},
			t:        Thresholds{FiveHour: 90, SevenDay: 95},
			wantOver: true,
			wantSub:  "7d",
		},
		{
			name: "5h crosses, 7d safe",
			u: AccountUsage{
				FiveHour: ptr(Window{Utilization: 92}),
				SevenDay: ptr(Window{Utilization: 50}),
			},
			t:        Thresholds{FiveHour: 90, SevenDay: 95},
			wantOver: true,
			wantSub:  "5h",
		},
		{
			name: "both under",
			u: AccountUsage{
				FiveHour: ptr(Window{Utilization: 20}),
				SevenDay: ptr(Window{Utilization: 50}),
			},
			t:        Thresholds{FiveHour: 90, SevenDay: 95},
			wantOver: false,
		},
		{
			name: "100 means reactive-only — even at 99% we don't fire",
			u: AccountUsage{
				FiveHour: ptr(Window{Utilization: 99}),
				SevenDay: ptr(Window{Utilization: 99}),
			},
			t:        Thresholds{FiveHour: 100, SevenDay: 100},
			wantOver: false,
		},
		{
			name: "threshold=100 but 5h at hard limit — must trigger",
			u: AccountUsage{
				FiveHour: ptr(Window{Utilization: 100}),
				SevenDay: ptr(Window{Utilization: 31}),
			},
			t:        Thresholds{FiveHour: 100, SevenDay: 100},
			wantOver: true,
			wantSub:  "hard limit",
		},
		{
			name: "threshold=100 but 7d at hard limit — must trigger",
			u: AccountUsage{
				FiveHour: ptr(Window{Utilization: 0}),
				SevenDay: ptr(Window{Utilization: 100}),
			},
			t:        Thresholds{FiveHour: 100, SevenDay: 100},
			wantOver: true,
			wantSub:  "hard limit",
		},
		{
			name: "missing window is not 'safe'; we just don't decide on it",
			u: AccountUsage{
				FiveHour: ptr(Window{Utilization: 92}),
				// SevenDay nil
			},
			t:        Thresholds{FiveHour: 90, SevenDay: 95},
			wantOver: true, // 5h still crosses
			wantSub:  "5h",
		},
		{
			name:     "all windows missing → no decision",
			u:        AccountUsage{},
			t:        Thresholds{FiveHour: 90, SevenDay: 95},
			wantOver: false,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			over, reason := IsOverThreshold(c.u, c.t)
			if over != c.wantOver {
				t.Fatalf("IsOverThreshold over=%v reason=%q want=%v", over, reason, c.wantOver)
			}
			if c.wantSub != "" && !contains(reason, c.wantSub) {
				t.Fatalf("reason %q does not mention %q", reason, c.wantSub)
			}
		})
	}
}

func TestDefaultThresholds(t *testing.T) {
	t.Parallel()
	d := DefaultThresholds()
	if d.FiveHour != 100 || d.SevenDay != 100 {
		t.Fatalf("unexpected defaults %+v", d)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0)
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestStaleReadingNeedsAProvenAge separates the two conditions that both
// used to render as a confident percentage: a reading that is old, and a
// reading whose age is unknown. Only the first is stale (issue #46).
func TestStaleReadingNeedsAProvenAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		polledAt  time.Time
		wantStale bool
	}{
		{"never polled — unknown age, not stale", time.Time{}, false},
		{"just polled", now.Add(-30 * time.Second), false},
		{"inside the idle coalescing window", now.Add(-2 * time.Minute), false},
		{"just inside the bound", now.Add(-StaleAfter + time.Minute), false},
		{"just past the bound", now.Add(-StaleAfter - time.Minute), true},
		{"the reported case: 12.8 days", now.Add(-307 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := AccountUsage{PolledAt: tc.polledAt}
			if got := u.StaleReading(now); got != tc.wantStale {
				t.Fatalf("StaleReading = %v, want %v", got, tc.wantStale)
			}
		})
	}
}

// TestStaleAfterClearsTheCoalescingWindows is the regression guard the
// bound exists for: the wrapper collapses sibling polls into one API sweep
// (issue #39), and a bound near those windows would mark the whole pool
// unknown during exactly the burst they absorb.
func TestStaleAfterClearsTheCoalescingWindows(t *testing.T) {
	const widestCoalesceWindow = 2 * time.Minute
	if StaleAfter < 10*widestCoalesceWindow {
		t.Fatalf("StaleAfter = %s, too close to the %s coalescing window", StaleAfter, widestCoalesceWindow)
	}
}

func TestStalenessSummarisesOnlyDatedEntries(t *testing.T) {
	now := time.Now()
	c := Cache{
		"fresh":     {PolledAt: now.Add(-time.Minute)},
		"stale":     {PolledAt: now.Add(-4 * time.Hour)},
		"staler":    {PolledAt: now.Add(-48 * time.Hour)},
		"undated":   {},
		"unrelated": {PolledAt: now.Add(-99 * time.Hour)},
	}
	sn := c.Staleness([]string{"fresh", "stale", "staler", "undated", "missing"}, now)
	if sn.Total != 3 {
		t.Fatalf("Total = %d, want 3 (undated and missing keys excluded)", sn.Total)
	}
	if sn.Stale != 2 {
		t.Fatalf("Stale = %d, want 2", sn.Stale)
	}
	if sn.Oldest < 47*time.Hour || sn.Oldest > 49*time.Hour {
		t.Fatalf("Oldest = %s, want ~48h", sn.Oldest)
	}
	if !sn.Any() {
		t.Fatal("Any() = false with two stale entries")
	}
	if sn.All() {
		t.Fatal("All() = true but one entry is fresh")
	}

	allStale := Cache{"a": {PolledAt: now.Add(-4 * time.Hour)}}
	if sn := allStale.Staleness([]string{"a"}, now); !sn.All() {
		t.Fatal("All() = false when every dated entry is stale")
	}
	if sn := c.Staleness(nil, now); sn.Any() || sn.All() {
		t.Fatal("an empty key set reported staleness")
	}
}

// TestSettledClearsWindowsThatHaveRolledOver covers the finding that is
// independent of staleness: a *freshly polled* reading can still carry a
// window whose reset instant has passed, and its recorded utilization
// describes a period that is over.
func TestSettledClearsWindowsThatHaveRolledOver(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-5*time.Minute), now.Add(2*time.Hour)
	u := AccountUsage{
		FiveHour:     &Window{Utilization: 100, ResetsAt: &past},
		SevenDay:     &Window{Utilization: 42, ResetsAt: &future},
		SevenDayOpus: &Window{Utilization: 100, ResetsAt: &past},
		PolledAt:     now.Add(-time.Minute),
	}
	got := u.Settled(now)
	if got.FiveHour != nil {
		t.Error("an elapsed 5h window should read as unknown")
	}
	if got.SevenDayOpus != nil {
		t.Error("an elapsed model window should read as unknown")
	}
	if got.SevenDay == nil || got.SevenDay.Utilization != 42 {
		t.Error("a window still running must be left alone")
	}
	if !got.PolledAt.Equal(u.PolledAt) {
		t.Error("Settled must preserve PolledAt so staleness stays knowable")
	}
	// A window with no reset stamp has no elapsed-ness to judge.
	noStamp := AccountUsage{FiveHour: &Window{Utilization: 100}}
	if noStamp.Settled(now).FiveHour == nil {
		t.Error("a window with no resets_at must not be cleared")
	}
}

// TestIsOverThresholdAtIgnoresAnElapsedWindow is the consequence that matters:
// cux must not move a session off an account whose window has just reset.
func TestIsOverThresholdAtIgnoresAnElapsedWindow(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	th := Thresholds{FiveHour: 98, SevenDay: 98}

	reset := AccountUsage{FiveHour: &Window{Utilization: 100, ResetsAt: &past}}
	if over, why := IsOverThresholdAt(reset, th, now); over {
		t.Fatalf("a window that already reset reported over threshold: %s", why)
	}
	// The plain predicate is unchanged — rendering still sees the raw number.
	if over, _ := IsOverThreshold(reset, th); !over {
		t.Fatal("IsOverThreshold should still report the recorded utilization")
	}

	live := AccountUsage{FiveHour: &Window{Utilization: 100, ResetsAt: &future}}
	if over, _ := IsOverThresholdAt(live, th, now); !over {
		t.Fatal("a window still running at 100% must remain over threshold")
	}
}
