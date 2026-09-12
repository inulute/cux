package wrapper

import (
	"reflect"
	"testing"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
)

// Only two of Anthropic's six limit types describe the seat. Reading a model
// limit as an account limit is what sends the pool rotating to a second seat
// carrying the same cap, and back again four seconds later (#52).
func TestModelLimitSeparatesModelCapsFromAccountCaps(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "the rejection quoted in #52, verbatim",
			message: "You've reached your Fable limit. Run /usage-credits to continue or switch models with /model.",
			want:    "fable",
		},
		{name: "opus", message: "You've reached your Opus limit.", want: "opus"},
		{name: "sonnet", message: "You've reached your Sonnet limit.", want: "sonnet"},

		// Account-wide: a different model cannot help, so these must keep
		// rotating the account exactly as they do today.
		{name: "five_hour", message: "You've reached your session limit."},
		{name: "seven_day", message: "You've reached your weekly limit."},
		{name: "overage", message: "You've reached your usage credit limit."},
		{name: "a plain 429", message: "rate_limit_error: Rate limited. Please try again later."},

		// The label, not a bare mention. A session that spent the turn
		// discussing models must not read as a model cap.
		{name: "model named in passing", message: "I'll use opus for this refactor, then check the limit."},
		{name: "empty", message: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelLimit(tc.message); got != tc.want {
				t.Errorf("modelLimit(%q) = %q, want %q", tc.message, got, tc.want)
			}
		})
	}
}

func TestNextModelWalksTheChainAndStops(t *testing.T) {
	chain := []string{"fable", "opus", "sonnet"}

	if got := nextModel(chain, "fable", map[string]bool{}); got != "opus" {
		t.Errorf("after fable = %q, want opus", got)
	}
	// The refused model is never offered back, wherever it sits in the chain.
	if got := nextModel(chain, "opus", map[string]bool{"fable": true}); got != "sonnet" {
		t.Errorf("after opus with fable spent = %q, want sonnet", got)
	}
	// Spent chain: the account swap is the fallback, so "" must mean "".
	if got := nextModel(chain, "sonnet", map[string]bool{"fable": true, "opus": true}); got != "" {
		t.Errorf("spent chain = %q, want empty", got)
	}
	// A refused model outside the chain still starts at the top.
	if got := nextModel(chain, "haiku", map[string]bool{}); got != "fable" {
		t.Errorf("refused model outside the chain = %q, want fable", got)
	}
	if got := nextModel([]string{" ", "", "opus"}, "", map[string]bool{}); got != "opus" {
		t.Errorf("blank entries = %q, want opus", got)
	}
}

func modelSwitchConfig(chain ...string) *config.Config {
	c := config.Defaults()
	c.ModelFallback = chain
	return &c
}

func TestModelSwitchTarget(t *testing.T) {
	rateLimit := func(refused string) *pending {
		return &pending{trigger: history.TriggerRateLimit, refusedModel: refused}
	}

	cases := []struct {
		name string
		p    *pending
		cfg  *config.Config
		want string
	}{
		{
			name: "model cap with a chain configured",
			p:    rateLimit("fable"),
			cfg:  modelSwitchConfig("fable", "opus"),
			want: "opus",
		},
		{
			// The documented default. An absent chain must leave every
			// existing install behaving exactly as it does today.
			name: "no chain configured",
			p:    rateLimit("fable"),
			cfg:  modelSwitchConfig(),
		},
		{
			name: "account-wide cap is not ours to answer",
			p:    rateLimit(""),
			cfg:  modelSwitchConfig("fable", "opus"),
		},
		{
			name: "a threshold swap is a seat decision, not a model one",
			p:    &pending{trigger: history.TriggerThreshold, refusedModel: "fable"},
			cfg:  modelSwitchConfig("fable", "opus"),
		},
		{
			name: "no pending at all",
			cfg:  modelSwitchConfig("fable", "opus"),
		},
		{
			name: "chain holds only the model that was just refused",
			p:    rateLimit("opus"),
			cfg:  modelSwitchConfig("opus"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelSwitchTarget(tc.p, tc.cfg, map[string]bool{}); got != tc.want {
				t.Errorf("modelSwitchTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

// The loop has to terminate. Two capped models in a chain must hand over to
// the account swap, not bounce the session between them.
func TestModelSwitchTargetGivesUpOnceTheChainIsSpent(t *testing.T) {
	cfg := modelSwitchConfig("fable", "opus")
	tried := map[string]bool{}

	first := modelSwitchTarget(&pending{trigger: history.TriggerRateLimit, refusedModel: "fable"}, cfg, tried)
	if first != "opus" {
		t.Fatalf("first switch = %q, want opus", first)
	}
	tried[first] = true

	if got := modelSwitchTarget(&pending{trigger: history.TriggerRateLimit, refusedModel: "opus"}, cfg, tried); got != "" {
		t.Errorf("second rejection = %q, want the account swap to take over", got)
	}
}

// relaunchFlags preserves the user's original flags by design, --model among
// them, so a substitution that only appended would hand claude two.
func TestWithModelSubstitutesRatherThanAppends(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "space-separated form",
			in:   []string{"--model", "opus", "--verbose"},
			want: []string{"--verbose", "--model", "sonnet"},
		},
		{
			name: "equals form",
			in:   []string{"--model=opus", "--verbose"},
			want: []string{"--verbose", "--model", "sonnet"},
		},
		{
			name: "no model flag to replace",
			in:   []string{"--verbose"},
			want: []string{"--verbose", "--model", "sonnet"},
		},
		{
			name: "bare --model followed by a flag eats nothing",
			in:   []string{"--model", "--verbose"},
			want: []string{"--verbose", "--model", "sonnet"},
		},
		{
			// withModel operates on the flag list alone, before --resume and
			// the injected turn are appended, so the substituted flag can
			// never land after a positional argument.
			name: "operates on flags only",
			in:   []string{"--dangerously-skip-permissions", "--model=opus"},
			want: []string{"--dangerously-skip-permissions", "--model", "sonnet"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withModel(tc.in, "sonnet"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("withModel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Pins the composition the wrapper actually performs. The substituted model
// has to reach claude as a flag, ahead of --resume and ahead of any injected
// first turn — a --model that lands after the positional prompt may be read
// as part of it, and the switch would silently relaunch onto the capped model.
func TestModelSwitchRelaunchOrdersFlagsBeforeTheInjectedTurn(t *testing.T) {
	argv := []string{"--dangerously-skip-permissions", "--model", "opus", "fix the bug"}

	got, _ := resumeArgv(withModel(relaunchFlags(argv), "sonnet"), "sid-1", &pending{}, "Go continue.")
	want := []string{"--dangerously-skip-permissions", "--model", "sonnet", "--resume", "sid-1", "Go continue."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("model-switch relaunch argv = %q, want %q", got, want)
	}

	// The original --model must be gone, not merely outranked.
	for i, tok := range got {
		if tok == "opus" {
			t.Errorf("the capped model survived the substitution at %d: %q", i, got)
		}
	}
}
