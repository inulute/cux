package hooks

import (
	"encoding/json"
	"testing"

	"github.com/inulute/cux/internal/signals"
)

func rawStr(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name string
		in   rateLimitHookInput
		want signals.Name
	}{
		{
			name: "rate limit stays a rate-limit swap",
			in: rateLimitHookInput{
				HookEventName: "StopFailure",
				Error:         rawStr("rate_limit"),
			},
			want: signals.RateLimited,
		},
		{
			// The usage-cap wording without the word "usage": treating
			// this as a generic API failure sent the wrapper into an
			// endless fixed-backoff retry loop on an account with no
			// capacity, instead of swapping / sleeping until the reset.
			name: "hit-your-limit wording is a rate limit, not a retry",
			in: rateLimitHookInput{
				HookEventName:        "StopFailure",
				Error:                rawStr("error"),
				LastAssistantMessage: rawStr("You've hit your limit · resets 7pm"),
			},
			want: signals.RateLimited,
		},
		{
			name: "weekly limit reached wording is a rate limit",
			in: rateLimitHookInput{
				HookEventName: "StopFailure",
				Error:         rawStr("Weekly limit reached — your limit will reset on Thursday"),
			},
			want: signals.RateLimited,
		},
		{
			name: "API error in the error field triggers a retry",
			in: rateLimitHookInput{
				HookEventName: "StopFailure",
				Error:         rawStr("api_error: connection reset by peer"),
			},
			want: signals.TurnFailed,
		},
		{
			name: "timeout wording in the assistant text alone must NOT trigger",
			in: rateLimitHookInput{
				HookEventName:        "StopFailure",
				Error:                rawStr("prompt is too long"),
				LastAssistantMessage: rawStr("the request timed out, let's add a 500ms timeout to the connection pool"),
			},
			want: "",
		},
		{
			name: "tool failure with API-looking stderr must NOT trigger",
			in: rateLimitHookInput{
				HookEventName: "PostToolUseFailure",
				Error:         rawStr("curl: (28) connection timed out after 5000 ms"),
			},
			want: "",
		},
		{
			name: "rate limit wording in assistant text still swaps (existing behavior)",
			in: rateLimitHookInput{
				HookEventName:        "StopFailure",
				Error:                rawStr("stopped"),
				LastAssistantMessage: rawStr("You've hit your session limit."),
			},
			want: signals.RateLimited,
		},
		{
			// Issue #39 case 1: a Claude Code concurrency refusal, not a
			// cap. Dropped by event scoping (no strong prose, and the weak
			// tiers require StopFailure) *and* by the denylist. The account
			// was at 9%/36% — a swap here restarted 20 live subagents.
			name: "concurrent subagent limit refusal is NOT a rate limit",
			in: rateLimitHookInput{
				HookEventName: "PostToolUseFailure",
				Error: rawStr("Concurrent subagent limit reached. You can run 20 subagents at once. " +
					"Do not retry. If the user wants more concurrent subagents, ask them to increase " +
					"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS."),
			},
			want: "",
		},
		{
			// Issue #39 case 2: a WebFetch of a docs page that documents its
			// own "Rate limits". The "URL: http" header marks it as fetched
			// content, so no substring in it is trusted.
			name: "fetched page documenting rate limits is NOT a rate limit",
			in: rateLimitHookInput{
				HookEventName: "PostToolUseFailure",
				Error: rawStr("Exit code 1\nURL: https://developers.pinterest.com/apps/\r\n" +
					"Entwicklerplattform\r\nRate limits\r\n1000/Tag\r\n"),
			},
			want: "",
		},
		{
			// Guard ordering: foreign-content is checked before strong
			// signals, so a fetched page that literally contains "usage
			// limit" still does not swap — even on a StopFailure.
			name: "foreign content beats a strong signal inside it",
			in: rateLimitHookInput{
				HookEventName: "StopFailure",
				Error:         rawStr("URL: https://docs.example.com/pricing\r\nYour plan usage limit is 5000 calls."),
			},
			want: "",
		},
		{
			// A genuine account cap surfaced through a tool call: strong
			// prose, no foreign marker, no denylist term → still swaps.
			name: "genuine usage-limit prose swaps even via a tool failure",
			in: rateLimitHookInput{
				HookEventName: "PostToolUseFailure",
				Error:         rawStr("Claude usage limit reached. Your limit will reset at 3pm."),
			},
			want: signals.RateLimited,
		},
		{
			// A bare 429 in a failed tool's own stderr must not swap: the
			// weak tiers require a turn-level StopFailure.
			name: "bare 429 from a tool failure does NOT swap",
			in: rateLimitHookInput{
				HookEventName: "PostToolUseFailure",
				Error:         rawStr("HTTP 429 Too Many Requests from api.github.com"),
			},
			want: "",
		},
		{
			// The same 429 on a StopFailure, with API context present, is
			// a real cap and swaps.
			name: "429 with API context on a StopFailure swaps",
			in: rateLimitHookInput{
				HookEventName: "StopFailure",
				Error:         rawStr("HTTP 429 too many requests from api.anthropic.com"),
			},
			want: signals.RateLimited,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := classifyFailure(c.in)
			if got != c.want {
				t.Errorf("classifyFailure(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestIsAPIFailure(t *testing.T) {
	matches := []string{
		"api_error: internal server error",
		"connection refused",
		"request timed out after 600000ms",
		"fetch failed: socket hang up",
		"502 bad gateway",
		"getaddrinfo enotfound api.anthropic.com",
	}
	for _, s := range matches {
		if !isAPIFailure(s) {
			t.Errorf("isAPIFailure(%q) = false, want true", s)
		}
	}

	nonMatches := []string{
		"user aborted the request",
		"prompt is too long: 215000 tokens > 200000 maximum",
		"invalid_request_error: model not found",
		"credit balance is too low",
	}
	for _, s := range nonMatches {
		if isAPIFailure(s) {
			t.Errorf("isAPIFailure(%q) = true, want false", s)
		}
	}
}
