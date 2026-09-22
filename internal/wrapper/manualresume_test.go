package wrapper

import (
	"slices"
	"testing"

	"github.com/inulute/cux/internal/history"
)

// A hand-driven /switch must come back to an empty prompt. The user is at the
// keyboard; auto_message would start a turn they never asked for.
func TestManualSwitchDoesNotAutoContinue(t *testing.T) {
	out, replay := resumeArgv(nil, "sid-1", &pending{trigger: history.TriggerManual}, "Go continue.")
	if slices.Contains(out, "Go continue.") {
		t.Errorf("manual /switch resumed with auto_message: %v", out)
	}
	if replay {
		t.Error("nothing to replay for a plain /switch")
	}

	// A prompt the threshold hook intercepted is still replayed: the user did
	// type it, they just never saw it run.
	out, replay = resumeArgv(nil, "sid-1",
		&pending{trigger: history.TriggerManual, resumeMessage: "fix the tests"}, "Go continue.")
	if !replay || !slices.Contains(out, "fix the tests") {
		t.Errorf("intercepted prompt was swallowed: %v (replay=%v)", out, replay)
	}

	// A rate-limit swap nobody is watching still auto-continues.
	out, _ = resumeArgv(nil, "sid-1", &pending{trigger: history.TriggerRateLimit}, "Go continue.")
	if !slices.Contains(out, "Go continue.") {
		t.Errorf("rate-limit swap lost auto_message: %v", out)
	}
}
