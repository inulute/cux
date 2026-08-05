package wrapper

import (
	"sync"
	"testing"
	"time"

	"github.com/inulute/cux/internal/config"
)

func idleTestConfig() *config.Config {
	c := config.Defaults()
	c.AutoSwapIdleAfterSeconds = 900
	c.AutoSwitchOnThreshold = true
	c.AutoResume = true
	return &c
}

// idleFor is the guard that separates "parked at an empty prompt" from
// "twenty minutes into a turn". Without it a long tool call reads as idle
// and the migration kills live work.
func TestActivityIdleForNeverReportsATurnInFlightAsIdle(t *testing.T) {
	start := time.Now()
	act := newActivity(start)
	long := start.Add(30 * time.Minute)

	if d, ok := act.idleFor(long); !ok || d < 30*time.Minute {
		t.Fatalf("untouched session: idleFor = (%v, %v), want ~30m and ok", d, ok)
	}

	// A prompt that reached the model. However long it runs, not idle.
	act.lastAt = start
	act.turnInFlight = true
	if _, ok := act.idleFor(long); ok {
		t.Error("a turn in flight must never be reported as idle")
	}

	// The Stop that ends it makes the session idle-eligible again.
	act.turnInFlight = false
	act.lastAt = long
	if d, ok := act.idleFor(long.Add(20 * time.Minute)); !ok || d < 20*time.Minute {
		t.Errorf("after turn end: idleFor = (%v, %v), want ~20m and ok", d, ok)
	}
}

func TestIdleSwapDue(t *testing.T) {
	const window = 900 * time.Second

	cases := []struct {
		name    string
		mutate  func(*config.Config)
		prepare func(*activity)
		swap    *pending
		want    bool
	}{
		{
			name:    "idle past the window",
			prepare: func(a *activity) { a.lastAt = time.Now().Add(-window - time.Minute) },
			want:    true,
		},
		{
			name:    "idle but not yet past the window",
			prepare: func(a *activity) { a.lastAt = time.Now().Add(-window / 2) },
			want:    false,
		},
		{
			// The gate that matters most: a long turn is not an idle session.
			name: "long turn in flight",
			prepare: func(a *activity) {
				a.lastAt = time.Now().Add(-2 * window)
				a.turnInFlight = true
			},
			want: false,
		},
		{
			name:   "disabled by config",
			mutate: func(c *config.Config) { c.AutoSwapIdleAfterSeconds = 0 },
			prepare: func(a *activity) {
				a.lastAt = time.Now().Add(-2 * window)
			},
			want: false,
		},
		{
			// Restarting without --resume would silently drop the user's
			// conversation. Not a trade cux may make on its own initiative.
			name:   "auto_resume off",
			mutate: func(c *config.Config) { c.AutoResume = false },
			prepare: func(a *activity) {
				a.lastAt = time.Now().Add(-2 * window)
			},
			want: false,
		},
		{
			name:   "threshold switching off entirely",
			mutate: func(c *config.Config) { c.AutoSwitchOnThreshold = false },
			prepare: func(a *activity) {
				a.lastAt = time.Now().Add(-2 * window)
			},
			want: false,
		},
		{
			// Another branch of step already queued a swap; do not stack.
			name: "a swap is already pending",
			prepare: func(a *activity) {
				a.lastAt = time.Now().Add(-2 * window)
			},
			swap: &pending{reason: "already queued"},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := idleTestConfig()
			if c.mutate != nil {
				c.mutate(cfg)
			}
			act := newActivity(time.Now())
			act.nextCheck = time.Now().Add(-time.Second) // throttle already elapsed
			if c.prepare != nil {
				c.prepare(act)
			}
			var mu sync.Mutex
			swap := c.swap
			if got := idleSwapDue(cfg, act, &mu, &swap); got != c.want {
				t.Errorf("idleSwapDue = %v, want %v", got, c.want)
			}
		})
	}
}

// The signal poll runs every 100ms; the idle evaluation must not.
func TestIdleSwapDueThrottles(t *testing.T) {
	cfg := idleTestConfig()
	act := newActivity(time.Now().Add(-2 * time.Hour))
	act.nextCheck = time.Now().Add(-time.Second)
	var mu sync.Mutex
	var swap *pending

	if !idleSwapDue(cfg, act, &mu, &swap) {
		t.Fatal("first check should be due")
	}
	if idleSwapDue(cfg, act, &mu, &swap) {
		t.Error("second check immediately after must be throttled")
	}
	if act.nextCheck.Before(time.Now().Add(idleCheckInterval / 2)) {
		t.Errorf("nextCheck = %v, want ~%v out", act.nextCheck, idleCheckInterval)
	}
}

// A check that did no work must not consume the once-a-minute slot, or a
// turn ending mid-window would push the real evaluation a minute further
// out for no reason.
func TestIdleSwapDueDoesNotBurnTheThrottleOnChecksThatDidNothing(t *testing.T) {
	cfg := idleTestConfig()
	var mu sync.Mutex
	var swap *pending

	// Long turn in flight, idle clock well past the window.
	act := newActivity(time.Now().Add(-2 * time.Hour))
	act.nextCheck = time.Now().Add(-time.Second)
	before := act.nextCheck
	act.turnInFlight = true

	if idleSwapDue(cfg, act, &mu, &swap) {
		t.Fatal("a turn in flight must not be due")
	}
	if !act.nextCheck.Equal(before) {
		t.Error("a mid-turn check must not advance the throttle")
	}

	// The turn ends. The very next tick should be able to act, not wait
	// out a slot the mid-turn checks consumed.
	act.turnInFlight = false
	if !idleSwapDue(cfg, act, &mu, &swap) {
		t.Error("should be due immediately once the turn ends")
	}
	if !act.nextCheck.After(before) {
		t.Error("the check that did the work should advance the throttle")
	}
}

// Coalescing is the only thing between this path and one usage poll per
// idle terminal per check, which is the endpoint pressure in #39.
func TestIdleRefreshCoalesceCoversTheCheckInterval(t *testing.T) {
	if idleRefreshCoalesce < idleCheckInterval {
		t.Errorf("idleRefreshCoalesce (%v) must be >= idleCheckInterval (%v)",
			idleRefreshCoalesce, idleCheckInterval)
	}
}

func TestDefaultIdleWindowIsFifteenMinutes(t *testing.T) {
	if got := config.Defaults().AutoSwapIdleAfterSeconds; got != 900 {
		t.Errorf("default auto_swap_idle_after_seconds = %d, want 900", got)
	}
}
