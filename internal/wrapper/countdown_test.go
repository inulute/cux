package wrapper

import (
	"testing"
	"time"
)

func TestCountdownRemaining(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{3*time.Hour + 11*time.Minute, "3h 11m"}, // >=10m -> minute granularity
		{45 * time.Minute, "45m"},
		{10 * time.Minute, "10m"},                  // boundary: exactly 10m stays minutes
		{9*time.Minute + 53*time.Second, "9m 53s"}, // <10m -> seconds appear
		{3*time.Minute + 7*time.Second, "3m 07s"},  // seconds zero-padded
		{9 * time.Minute, "9m 00s"},
		{45 * time.Second, "45s"}, // sub-minute drops the minutes field
		{0, "0s"},
		{-5 * time.Second, "0s"},                                          // negatives clamp, never "-5s"
		{9*time.Minute + 52*time.Second + 600*time.Millisecond, "9m 53s"}, // rounds to nearest second
	}
	for _, c := range cases {
		if got := countdownRemaining(c.d); got != c.want {
			t.Errorf("countdownRemaining(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}
