package hooks

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/signals"
	"github.com/inulute/cux/internal/store"
)

// The wrapper's idle migration hinges on StartsTurn: a prompt the hook
// handles itself is blocked and never reaches the model, so no Stop will
// follow it. Recording one as the start of a turn would leave the session
// looking permanently busy and it would never migrate (#39).
func TestUserPromptSubmitReportsPromptsToTheWrapper(t *testing.T) {
	cases := []struct {
		name           string
		prompt         string
		wantSignal     bool
		wantStartsTurn bool
	}{
		{
			name:           "an ordinary prompt goes to the model",
			prompt:         "refactor the parser",
			wantSignal:     true,
			wantStartsTurn: true,
		},
		{
			// Handled by the hook: user is present, but no turn starts.
			name:           "/switch is handled by the hook",
			prompt:         "/switch work",
			wantSignal:     true,
			wantStartsTurn: false,
		},
		{
			// Not a prompt at all — nothing to report.
			name:       "empty prompt reports nothing",
			prompt:     "   ",
			wantSignal: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("CUX_WRAPPED", "1")
			t.Setenv("CUX_WRAPPER_PID", "424242")
			t.Setenv("HOME", tmp)
			t.Setenv("XDG_DATA_HOME", tmp)
			t.Setenv("CUX_CREDS_BACKEND", "file")
			t.Setenv("CUX_CONFIG_FILE", filepath.Join(tmp, "config.json"))

			// A minimal state file so the /switch path has somewhere to look
			// instead of erroring before it reports the prompt.
			state := &store.State{
				ActiveSlot: 1,
				Sequence:   []int{1},
				Accounts:   map[int]store.Account{1: {Slot: 1, Email: "a@x.test", Alias: "work"}},
			}
			if err := state.Save(); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			body := `{"prompt":` + jsonString(c.prompt) + `}`
			if err := UserPromptSubmit(strings.NewReader(body), &out); err != nil {
				t.Fatalf("UserPromptSubmit: %v", err)
			}

			b, ok, _ := signals.Read(424242, signals.PromptSubmitted)
			if ok != c.wantSignal {
				t.Fatalf("signal written = %v, want %v", ok, c.wantSignal)
			}
			if !c.wantSignal {
				return
			}
			p, err := signals.DecodePromptSubmitted(b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if p.StartsTurn != c.wantStartsTurn {
				t.Errorf("StartsTurn = %v, want %v", p.StartsTurn, c.wantStartsTurn)
			}
			if p.Timestamp.IsZero() {
				t.Error("Timestamp is zero; the wrapper needs it for the idle clock")
			}
		})
	}
}

// An unwrapped session (plain `claude`, no cux) must stay silent.
func TestUserPromptSubmitReportsNothingWhenUnwrapped(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CUX_WRAPPED", "")
	t.Setenv("CUX_WRAPPER_PID", "424243")
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)

	var out bytes.Buffer
	if err := UserPromptSubmit(strings.NewReader(`{"prompt":"hello"}`), &out); err != nil {
		t.Fatalf("UserPromptSubmit: %v", err)
	}
	if _, ok, _ := signals.Read(424243, signals.PromptSubmitted); ok {
		t.Error("unwrapped session must not write signals")
	}
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
