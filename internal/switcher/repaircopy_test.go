package switcher

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
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

func skipUnlessPermissionsBite(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop writes here")
	}
}

// readOnlyDir makes dir unwritable until the test ends.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	skipUnlessPermissionsBite(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// stubOAuthWrite makes writing the identity file fail, calling before
// first when it is not nil.
func stubOAuthWrite(t *testing.T, before func(dir string)) {
	t.Helper()
	real := writeOAuthBlockIn
	t.Cleanup(func() { writeOAuthBlockIn = real })
	writeOAuthBlockIn = func(dir string, _ json.RawMessage) error {
		if before != nil {
			before(dir)
		}
		return errors.New("identity file unwritable")
	}
}

// revokedProfileLogin puts a login of seat bb that cux's refresh of the
// backup revoked, now expired, into bb's profile.
func revokedProfileLogin(t *testing.T) string {
	t.Helper()
	b, fresh, profileB := deadProfileSeat(t)
	own := blobExpiring("token-b-own", time.Now().Add(-10*time.Minute).UnixMilli())
	loginIn(t, profileB, "b@example.com", "uuid-b", own)
	if err := creds.RecordRevoked(b.Slot, b.Email, own, fresh); err != nil {
		t.Fatal(err)
	}
	return profileB
}

// A planned repair that cannot be written is skipped, not refused, as long
// as the profile is left as it was: the switch lands on the profile's own
// login. It is refused when the copied login cannot be removed again, and a
// copy into a profile without the seat's login is refused on any write
// error, as before.
func TestSwitchTerminalSkipsARepairItCannotWrite(t *testing.T) {
	watch := func(profileB string) []string {
		return []string{paths.ClaudeCredentialsIn(profileB), paths.ClaudeConfigIn(profileB),
			paths.ClaudeCredentials(), paths.ClaudeConfig(), paths.StateFile()}
	}
	t.Run("repair into an unwritable profile", func(t *testing.T) {
		profileB := revokedProfileLogin(t)
		files := watch(profileB)
		before := snapshot(t, files...)
		readOnlyDir(t, profileB)
		if _, _, err := SwitchTerminal("bb"); err != nil {
			t.Fatalf("SwitchTerminal(bb): %v", err)
		}
		assertUnchanged(t, files, before)
		assertScopedTo(t, profileB)
	})
	t.Run("repair whose identity write fails", func(t *testing.T) {
		profileB := revokedProfileLogin(t)
		files := watch(profileB)
		before := snapshot(t, files...)
		stubOAuthWrite(t, nil)
		if _, _, err := SwitchTerminal("bb"); err != nil {
			t.Fatalf("SwitchTerminal(bb): %v", err)
		}
		assertUnchanged(t, files, before)
		assertScopedTo(t, profileB)
	})
	t.Run("repair whose copied login cannot be removed", func(t *testing.T) {
		skipUnlessPermissionsBite(t)
		profileB := revokedProfileLogin(t)
		stubOAuthWrite(t, func(dir string) { readOnlyDir(t, dir) })
		if _, _, err := SwitchTerminal("bb"); err == nil ||
			!strings.Contains(err.Error(), "removing the copied login failed") {
			t.Fatalf("SwitchTerminal(bb) = %v; want the switch refused", err)
		}
		if TerminalScoped() && os.Getenv(paths.EnvClaudeConfigDir) == profileB {
			t.Fatal("the terminal moved onto a profile left holding the copied login")
		}
	})
	t.Run("copy into an unwritable profile without the seat's login", func(t *testing.T) {
		_, _, profileB := deadProfileSeat(t)
		readOnlyDir(t, profileB)
		if _, _, err := SwitchTerminal("bb"); err == nil || !strings.Contains(err.Error(), "credentials") {
			t.Fatalf("SwitchTerminal(bb) = %v; want the copy refused", err)
		}
	})
	t.Run("copy whose identity write fails", func(t *testing.T) {
		deadProfileSeat(t)
		stubOAuthWrite(t, nil)
		if _, _, err := SwitchTerminal("bb"); err == nil || !strings.Contains(err.Error(), "identity file unwritable") {
			t.Fatalf("SwitchTerminal(bb) = %v; want the copy refused", err)
		}
	})
}
