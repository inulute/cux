package monitor

import (
	"testing"
	"time"
)

func TestFreshEnough(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	maxAge := 20 * time.Second

	cases := []struct {
		name     string
		polledAt time.Time
		maxAge   time.Duration
		want     bool
	}{
		{"polled just now is fresh", now.Add(-2 * time.Second), maxAge, true},
		{"polled inside the window is fresh", now.Add(-19 * time.Second), maxAge, true},
		{"polled at the window boundary is stale", now.Add(-maxAge), maxAge, false},
		{"polled before the window is stale", now.Add(-90 * time.Second), maxAge, false},
		{"never polled is never fresh", time.Time{}, maxAge, false},
		{"coalescing off (maxAge 0) never fresh", now, 0, false},
		{"coalescing off (negative) never fresh", now, -time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := freshEnough(c.polledAt, c.maxAge, now); got != c.want {
				t.Errorf("freshEnough(%v, %v) = %v, want %v", c.polledAt, c.maxAge, got, c.want)
			}
		})
	}
}
