package wrapper

import (
	"errors"
	"testing"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
)

// The decision that matters: claude is only ever stopped for a rate limit
// that has somewhere to go. Everything else about parking follows from
// leaving the child alone (#48, #50).
func TestShouldPark(t *testing.T) {
	noTarget := func(string, history.Trigger, *config.Config, map[int]bool) (string, error) {
		return "", errors.New("all managed accounts are exhausted")
	}
	haveTarget := func(string, history.Trigger, *config.Config, map[int]bool) (string, error) {
		return "2", nil
	}
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

// A reading taken before the limit cannot say the limit is over, so the
// caller refreshes and shouldPark must not do it a second time.
func TestShouldParkDoesNotRefresh(t *testing.T) {
	refreshed := false
	defer swapSeams(&refreshed, func(string, history.Trigger, *config.Config, map[int]bool) (string, error) {
		return "", errors.New("exhausted")
	}, false)()
	shouldPark(&config.Config{}, &pending{trigger: history.TriggerRateLimit})
	if refreshed {
		t.Error("shouldPark refreshed; its callers already did")
	}
}

func swapSeams(refreshed *bool, resolve func(string, history.Trigger, *config.Config, map[int]bool) (string, error), liveRoom bool) func() {
	or, ol, of := parkResolve, parkLiveRoom, parkRefresh
	parkResolve = resolve
	parkLiveRoom = func(*config.Config) bool { return liveRoom }
	parkRefresh = func() { *refreshed = true }
	return func() { parkResolve, parkLiveRoom, parkRefresh = or, ol, of }
}
