package wrapper

import (
	"strings"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
)

// Anthropic's rate limits are not one thing. Claude Code distinguishes six,
// and only two of them — the 5-hour session limit and the account-wide
// weekly limit — describe the seat. The rest are per model:
//
//	five_hour                  "session limit"      account-wide
//	seven_day                  "weekly limit"       account-wide
//	seven_day_opus             "Opus limit"         model
//	seven_day_sonnet           "Sonnet limit"       model
//	seven_day_overage_included "Fable limit"        model
//	overage                    "usage credit limit" account-wide
//
// For a model limit an account swap is a category error. The constraint is
// not "this seat is out of budget", it is "this seat is out of budget for
// this model", and the same user on the same seat can keep working on
// another one immediately. Rotating instead usually lands on a seat carrying
// the same model cap, so the pool spends a second account to arrive back
// where it started — measured at a four second round trip (#52, #57).
//
// Anthropic says as much in the rejection itself: "You've reached your Fable
// limit. Run /usage-credits to continue or switch models with /model." cux is
// the only component positioned to act on that — Claude Code never changes
// model by itself, and cux already relaunches the process on every swap.
//
// knownModels are the names that appear in those labels. An unknown one is
// simply not matched, which falls back to today's account swap.
var knownModels = []string{"opus", "sonnet", "haiku", "fable"}

// modelLimit reports which model a rejection refused, or "" when the limit is
// account-wide (or the message is not a limit at all).
//
// The model is read from the refusal rather than from the session's flags on
// purpose. strategy.go declines to treat model windows as an eligibility gate
// because cux cannot know which model a session will ask for next — which is
// true, and is exactly why they only sort candidates there. It stops being
// true at this one instant: the API has just refused a call, so the model it
// refused is the model the session asked for. Known, not guessed.
func modelLimit(message string) string {
	lower := strings.ToLower(message)
	for _, m := range knownModels {
		// Matched as the label Anthropic prints ("Opus limit"), not as a
		// bare mention: a session whose transcript happens to discuss Opus
		// must not read as a model cap.
		if strings.Contains(lower, m+" limit") {
			return m
		}
	}
	return ""
}

// nextModel picks the model to relaunch on, given the configured chain, the
// model that was just refused, and the models this session has already tried.
// It returns "" when the chain is spent, which is the caller's signal to fall
// back to an account swap.
//
// tried is what makes this terminate. Without it a two-entry chain whose
// second model is also capped would hand the session back and forth between
// them for as long as the caps last — the same ping-pong in a smaller loop.
func nextModel(chain []string, refused string, tried map[string]bool) string {
	for _, m := range chain {
		m = strings.TrimSpace(m)
		if m == "" || strings.EqualFold(m, refused) || tried[strings.ToLower(m)] {
			continue
		}
		return m
	}
	return ""
}

// modelSwitchTarget returns the model to relaunch on when p is a rate-limit
// rejection that named a model and the fallback chain still has somewhere to
// go. "" means "handle this as an account swap", which covers every
// account-wide cap, an unconfigured chain, and a chain that is spent.
func modelSwitchTarget(p *pending, cfg *config.Config, tried map[string]bool) string {
	if p == nil || cfg == nil || len(cfg.ModelFallback) == 0 {
		return ""
	}
	if p.trigger != history.TriggerRateLimit || p.refusedModel == "" {
		return ""
	}
	// The refused model is spent for this session too, whether or not the
	// chain names it — otherwise a chain listing it would offer it straight
	// back on the next rejection.
	tried[strings.ToLower(p.refusedModel)] = true
	return nextModel(cfg.ModelFallback, p.refusedModel, tried)
}

// withModel substitutes the model on a relaunch argv.
//
// Substitutes rather than appends: relaunchFlags deliberately preserves the
// user's original flags, `--model` among them, so appending would hand claude
// two of them and let it pick. Both spellings have to go — `--model opus` and
// `--model=opus` — and the value token with the first.
func withModel(argv []string, model string) []string {
	out := make([]string, 0, len(argv)+2)
	for i := 0; i < len(argv); i++ {
		name, _, hasEq := strings.Cut(argv[i], "=")
		if name != "--model" {
			out = append(out, argv[i])
			continue
		}
		if !hasEq && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
			i++ // drop the flag's value too
		}
	}
	return append(out, "--model", model)
}
