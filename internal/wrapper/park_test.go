package wrapper

import (
	"errors"
	"testing"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
)

func noTarget(string, history.Trigger, *config.Config, map[int]bool) (string, error) {
	return "", errors.New("all managed accounts are exhausted")
}

func haveTarget(string, history.Trigger, *config.Config, map[int]bool) (string, error) {
	return "2", nil
}

// The decision that matters: claude is only ever stopped for a rate limit
// that has somewhere to go. Everything else about parking follows from
// leaving the child alone (#48, #50).
func TestShouldPark(t *testing.T) {
	limit := func() *pending { return &pending{trigger: history.TriggerRateLimit} }

	cases := []struct {
		name     string
		cfg      *config.Config
		p        *pending
		resolve  func(string, history.Trigger, *config.Config, map[int]bool) (string, error)
		liveRoom bool
		want     bool
	}{
		{"nowhere to go and the live seat is out", &config.Config{}, limit(), noTarget, false, true},
		{"another seat has room", &config.Config{}, limit(), haveTarget, false, false},
		{"live seat still has room, so retry in place", &config.Config{}, limit(), noTarget, true, false},
		{"api-error retry is not a limit", &config.Config{}, &pending{retryOnly: true}, noTarget, false, false},
		{"manual switch is not a limit", &config.Config{}, &pending{trigger: history.TriggerManual}, noTarget, false, false},
		{
			"a model cap can relaunch on another model",
			&config.Config{ModelFallback: []string{"sonnet"}},
			&pending{trigger: history.TriggerRateLimit, refusedModel: "opus"},
			noTarget, false, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refreshed := false
			defer swapSeams(&refreshed, tc.resolve, tc.liveRoom)()
			if got := shouldPark(tc.cfg, tc.p); got != tc.want {
				t.Errorf("shouldPark = %v, want %v", got, tc.want)
			}
		})
	}
}

// A parked session nobody came back to has to end up somewhere that can
// carry auto_message: another seat if one freed up, otherwise a plain
// relaunch on the live seat once it recovers.
func TestParkResumeTarget(t *testing.T) {
	parked := &pending{trigger: history.TriggerRateLimit, reason: "5h limit"}
	var unused bool

	t.Run("another seat freed up: swap", func(t *testing.T) {
		defer swapSeams(&unused, haveTarget, false)()
		got := parkResumeTarget(&config.Config{}, parked)
		if got != parked {
			t.Fatalf("want the parked swap handed on, got %+v", got)
		}
	})

	t.Run("only the live seat came back: relaunch in place", func(t *testing.T) {
		defer swapSeams(&unused, noTarget, true)()
		got := parkResumeTarget(&config.Config{}, parked)
		if got == nil || !got.retryOnly {
			t.Fatalf("want a retry on the live seat, got %+v", got)
		}
		if got.reason != parked.reason {
			t.Errorf("reason = %q, want it carried over", got.reason)
		}
	})

	t.Run("still nothing: keep waiting", func(t *testing.T) {
		defer swapSeams(&unused, noTarget, false)()
		if got := parkResumeTarget(&config.Config{}, parked); got != nil {
			t.Fatalf("want nil while every seat is out, got %+v", got)
		}
	})
}

func swapSeams(refreshed *bool, resolve func(string, history.Trigger, *config.Config, map[int]bool) (string, error), liveRoom bool) func() {
	or, ol, of := parkResolve, parkLiveRoom, parkRefresh
	parkResolve = resolve
	parkLiveRoom = func(*config.Config) bool { return liveRoom }
	parkRefresh = func() { *refreshed = true }
	return func() { parkResolve, parkLiveRoom, parkRefresh = or, ol, of }
}
