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
