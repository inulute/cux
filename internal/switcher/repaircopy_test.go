package switcher

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
)

func stubCopySource(t *testing.T) {
	t.Helper()
	real := readCopySource
	t.Cleanup(func() { readCopySource = real })
	readCopySource = func(store.Account) (string, json.RawMessage, error) {
		return "", nil, errors.New("backup vanished")
	}
}

// A planned repair whose copy can no longer be made is skipped, not
// refused: the switch lands on the profile's own login, as before the
// repair existed, and writes nothing. A copy into a profile that holds no
// login of the seat is still refused when it cannot be made.
func TestSwitchTerminalSkipsARepairItCannotCopy(t *testing.T) {
	t.Run("repair", func(t *testing.T) {
		b, fresh, profileB := deadProfileSeat(t)
		own := blobExpiring("token-b-own", time.Now().Add(-10*time.Minute).UnixMilli())
		loginIn(t, profileB, "b@example.com", "uuid-b", own)
		if err := creds.RecordRevoked(b.Slot, b.Email, own, fresh); err != nil {
			t.Fatal(err)
		}
		state, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if _, need, err := PrepareTerminal(state, b); err != nil || !need {
			t.Fatalf("PrepareTerminal = %v, %v; want a repair planned", need, err)
		}
		watched := []string{paths.ClaudeCredentialsIn(profileB), paths.ClaudeConfigIn(profileB),
			paths.ClaudeCredentials(), paths.ClaudeConfig(), paths.StateFile()}
		before := snapshot(t, watched...)
		stubCopySource(t)
		if _, _, err := SwitchTerminal("bb"); err != nil {
			t.Fatalf("SwitchTerminal(bb): %v", err)
		}
		assertUnchanged(t, watched, before)
		assertScopedTo(t, profileB)
	})
	t.Run("copy into a profile without the seat's login", func(t *testing.T) {
		deadProfileSeat(t)
		stubCopySource(t)
		if _, _, err := SwitchTerminal("bb"); err == nil || !strings.Contains(err.Error(), "backup vanished") {
			t.Fatalf("SwitchTerminal(bb) = %v; want the copy refused", err)
		}
	})
}
