package switcher

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
)

// blobFor builds a credentials blob carrying one account token, the shape
// Claude Code writes to its credential store.
func blobFor(token string) string {
	return `{"claudeAiOauth":{"accessToken":"` + token + `","refreshToken":"r-` + token + `"}}`
}

// isolate points HOME, the data root and the credential backend at a temp
// directory so a test never reads — or writes — the real login (issue #7).
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CUX_CREDS_BACKEND", "file")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// setLive writes the live credential blob plus the oauthAccount block that
// names who is logged in. Passing a token that disagrees with email is how
// the two-files-disagree case is reproduced.
func setLive(t *testing.T, email, uuid, token string) {
	t.Helper()
	if err := os.WriteFile(paths.ClaudeCredentials(), []byte(blobFor(token)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"oauthAccount": map[string]any{
			"emailAddress": email,
			"accountUuid":  uuid,
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClaudeConfig(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAddCurrentRefusesATokenAlreadyStoredUnderAnotherSlot is the issue #46
// follow-up: the identity file names one account while the credential store
// holds another's token, so the add would file B's token under A's name.
func TestAddCurrentRefusesATokenAlreadyStoredUnderAnotherSlot(t *testing.T) {
	isolate(t)

	// Slot 1: account A, captured cleanly.
	setLive(t, "a@example.com", "uuid-a", "token-a")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("first add: %v", err)
	}

	// Now the identity file names B, but the credential store still holds
	// A's token — the exact state `claude auth login` left behind when
	// CLAUDE_CONFIG_DIR pointed elsewhere.
	setLive(t, "b@example.com", "uuid-b", "token-a")
	_, _, err := AddCurrent(0, "", true, false)
	if !errors.Is(err, ErrTokenIdentityMismatch) {
		t.Fatalf("AddCurrent error = %v, want ErrTokenIdentityMismatch", err)
	}

	// And it must refuse before writing anything: no second slot.
	state, loadErr := store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(state.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1 — the refused add left state behind", len(state.Accounts))
	}
	if state.FindByEmail("b@example.com") != 0 {
		t.Fatal("b@example.com was registered despite the refusal")
	}
}

// TestAddCurrentRefreshesTheSameAccountWithAnUnchangedToken guards the
// skipSlot exclusion: re-running `cux add` for an account already managed is
// the normal token-refresh path and must not trip over its own stored token.
func TestAddCurrentRefreshesTheSameAccountWithAnUnchangedToken(t *testing.T) {
	isolate(t)
	setLive(t, "a@example.com", "uuid-a", "token-a")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("first add: %v", err)
	}

	acct, refreshed, err := AddCurrent(0, "", true, false)
	if err != nil {
		t.Fatalf("re-add of the same account: %v", err)
	}
	if !refreshed {
		t.Fatal("re-adding a managed account should report refreshed")
	}
	if acct.Email != "a@example.com" {
		t.Fatalf("refreshed %s, want a@example.com", acct.Email)
	}
}

// TestAddCurrentAcceptsASecondAccountWithItsOwnToken is the ordinary path —
// the guard must not stand in the way of a genuine second seat.
func TestAddCurrentAcceptsASecondAccountWithItsOwnToken(t *testing.T) {
	isolate(t)
	setLive(t, "a@example.com", "uuid-a", "token-a")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("first add: %v", err)
	}
	setLive(t, "b@example.com", "uuid-b", "token-b")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("second add: %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(state.Accounts))
	}
}

// TestSlotSharingTokenSkipsSlotsItCannotRead keeps the guard best-effort:
// unreadable credentials are a live failure mode (issue #46) and must not
// become a refusal to add a good account.
func TestSlotSharingTokenSkipsSlotsItCannotRead(t *testing.T) {
	isolate(t)
	setLive(t, "a@example.com", "uuid-a", "token-a")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("first add: %v", err)
	}

	// Delete slot 1's stored credentials, leaving the slot registered —
	// the state the reporter's machine was in when the keyring emptied.
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.DeleteBackup(1, state.Accounts[1].Email); err != nil {
		t.Fatal(err)
	}

	if slot, _ := slotSharingToken(state, 0, blobFor("token-a")); slot != 0 {
		t.Fatalf("slotSharingToken = %d, want 0 for an unreadable backup", slot)
	}
	setLive(t, "b@example.com", "uuid-b", "token-b")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("add with an unreadable sibling slot: %v", err)
	}
}

// TestTokenFingerprintIgnoresBlobsWithoutAnAccountToken covers the MCP-only
// blob from issue #42: no token means no fingerprint, so such a slot can
// never collide with anything.
func TestTokenFingerprintIgnoresBlobsWithoutAnAccountToken(t *testing.T) {
	for _, blob := range []string{
		"",
		"not json",
		`{"mcpOAuth":{"srv":{"accessToken":"x"}}}`,
		`{"claudeAiOauth":{"refreshToken":"r"}}`,
	} {
		if _, ok := tokenFingerprint(blob); ok {
			t.Fatalf("tokenFingerprint(%q) reported a usable token", blob)
		}
	}
	a, ok := tokenFingerprint(blobFor("token-a"))
	if !ok {
		t.Fatal("tokenFingerprint rejected a valid blob")
	}
	if b, _ := tokenFingerprint(blobFor("token-b")); a == b {
		t.Fatal("distinct tokens produced the same fingerprint")
	}
}

// TestAddCurrentForceCapturesTheMismatchAnyway keeps the escape hatch open:
// the guard is a heuristic about tokens, and being wrong about it must not
// leave a user unable to repair their pool.
func TestAddCurrentForceCapturesTheMismatchAnyway(t *testing.T) {
	isolate(t)
	setLive(t, "a@example.com", "uuid-a", "token-a")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("first add: %v", err)
	}
	setLive(t, "b@example.com", "uuid-b", "token-a")
	if _, _, err := AddCurrent(0, "", true, true); err != nil {
		t.Fatalf("forced add: %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.FindByEmail("b@example.com") == 0 {
		t.Fatal("--force did not capture the account")
	}
}

// TestSwitchToKeepsAGoodBackupWhenTheLivePairIsMismatched covers the second
// door onto the same corruption: SwitchTo opportunistically re-backs-up the
// live credentials under whatever account the identity file names, so a
// mismatched pair there would overwrite a good slot with another account's
// token — without anyone running `cux add` at all.
func TestSwitchToKeepsAGoodBackupWhenTheLivePairIsMismatched(t *testing.T) {
	isolate(t)
	setLive(t, "a@example.com", "uuid-a", "token-a")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("add a: %v", err)
	}
	setLive(t, "b@example.com", "uuid-b", "token-b")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("add b: %v", err)
	}

	// The mismatch: the identity file names b, the credential store holds
	// a's token. Slot 2 is b, and its stored token must survive.
	setLive(t, "b@example.com", "uuid-b", "token-a")
	if _, _, err := SwitchTo("1"); err != nil {
		t.Fatalf("SwitchTo should still succeed: %v", err)
	}

	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	slotB := state.FindByEmail("b@example.com")
	if slotB == 0 {
		t.Fatal("b@example.com is no longer managed")
	}
	stored, err := creds.ReadBackup(slotB, "b@example.com")
	if err != nil {
		t.Fatalf("reading b's backup: %v", err)
	}
	if stored != blobFor("token-b") {
		t.Fatalf("b's backup was overwritten: %s", stored)
	}
}

// TestSwitchToStillRefreshesAMatchingPair is the positive control: the skip
// above must not cost the ordinary token rotation SwitchTo exists to do.
func TestSwitchToStillRefreshesAMatchingPair(t *testing.T) {
	isolate(t)
	setLive(t, "a@example.com", "uuid-a", "token-a")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("add a: %v", err)
	}
	setLive(t, "b@example.com", "uuid-b", "token-b")
	if _, _, err := AddCurrent(0, "", true, false); err != nil {
		t.Fatalf("add b: %v", err)
	}

	// b is live and its token has rotated since the add — a consistent pair.
	setLive(t, "b@example.com", "uuid-b", "token-b2")
	if _, _, err := SwitchTo("1"); err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}

	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	slotB := state.FindByEmail("b@example.com")
	stored, err := creds.ReadBackup(slotB, "b@example.com")
	if err != nil {
		t.Fatalf("reading b's backup: %v", err)
	}
	if stored != blobFor("token-b2") {
		t.Fatalf("rotated token was not backed up, got: %s", stored)
	}
}
