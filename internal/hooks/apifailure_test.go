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
