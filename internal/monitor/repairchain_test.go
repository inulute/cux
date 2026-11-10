package monitor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/hookinstall"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/switcher"
)

// issueInOrder makes every refresh return the next of issued.
func issueInOrder(t *testing.T, issued ...string) *int {
	t.Helper()
	n := 0
	orig := refreshBlob
	t.Cleanup(func() { refreshBlob = orig })
	refreshBlob = func(string) (string, error) {
		if n >= len(issued) {
			t.Fatalf("refresh %d: no token left to issue", n+1)
		}
		n++
		return issued[n-1], nil
	}
	return &n
}

// chainSeats logs the shared directory in as A (slot 1) and gives B (slot
// 2) a backup and an identity backup. B's profile gets only credentials:
// without a .claude.json, Claude Code has not set it up and ConfigDirs does
// not see it.
func chainSeats(t *testing.T, home, backupB, profileCreds string) string {
	t.Helper()
	holderSeats(t,
		store.Account{Slot: 1, Email: "a@example.com", UUID: "u-a"},
		store.Account{Slot: 2, Email: "b@example.com", UUID: "u-b", Alias: "bb"})
	sharedA := holderBlob("ta", time.Now().Add(8*time.Hour))
	if err := creds.WriteBackup(1, "a@example.com", sharedA); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, "", "a@example.com", "u-a", sharedA)
	if err := creds.WriteBackup(2, "b@example.com", backupB); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteOAuthBlockBackup(2, "b@example.com",
		json.RawMessage(`{"emailAddress":"b@example.com","accountUuid":"u-b"}`)); err != nil {
		t.Fatal(err)
	}
	profileB := filepath.Join(home, ".claude-bb")
	if err := os.MkdirAll(profileB, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClaudeCredentialsIn(profileB), []byte(profileCreds), 0o600); err != nil {
		t.Fatal(err)
	}
	return profileB
}

// setUpProfile writes what Claude Code and `cux setup` leave in a profile:
// its identity file, cux's hooks and /switch.
func setUpProfile(t *testing.T, dir string) {
	t.Helper()
	cfg := `{"oauthAccount":{"emailAddress":"b@example.com","accountUuid":"u-b"}}`
	if err := os.WriteFile(paths.ClaudeConfigIn(dir), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := os.Getenv(paths.EnvClaudeConfigDir)
	_ = os.Setenv(paths.EnvClaudeConfigDir, dir)
	_, err := hookinstall.Install()
	_ = os.Setenv(paths.EnvClaudeConfigDir, prev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "commands", "switch.md"), []byte("/switch"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func snapshot(t *testing.T, files ...string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = b
	}
	return out
}

func assertSame(t *testing.T, before map[string][]byte) {
	t.Helper()
	for f, b := range before {
		if got, _ := os.ReadFile(f); !bytes.Equal(got, b) {
			t.Fatalf("%s changed:\n%s\nwas:\n%s", f, got, b)
		}
	}
}

// The whole repair chain: a poll refreshes B's backup and revokes the token
// B's profile still holds (the profile was not set up yet, so the handover
// could not reach it), the profile is set up, the backup's access token
// runs out, the next poll still refreshes the backup, and a terminal switch
// to B repairs the profile. The shared login and state.json stay as they
// were.
func TestPollAndSwitchRepairAProfileTheRefreshRevoked(t *testing.T) {
	home, _ := holderEnv(t)
	first := holderBlob("tf", time.Now().Add(-2*time.Hour))
	profileB := chainSeats(t, home, first, first)
	// The second refresh's access token has expired by the next poll.
	second := holderBlob("ts", time.Now().Add(-time.Minute))
	third := holderBlob("tt", time.Now().Add(8*time.Hour))
	refreshes := issueInOrder(t, second, third)

	if _, errs := RefreshAll(); len(errs) != 0 {
		t.Fatalf("first poll: %v", errs)
	}
	if *refreshes != 1 {
		t.Fatalf("first poll refreshed %d times, want 1", *refreshes)
	}
	if revoked, err := creds.RevokedByRefresh(2, "b@example.com", first); err != nil || !revoked {
		t.Fatalf("the token the first poll rotated away is not on the record: %v, %v", revoked, err)
	}
	if got := readLogin(t, profileB); got != first {
		t.Fatalf("profile = %s, want it still on the revoked login", got)
	}

	setUpProfile(t, profileB)
	if _, errs := RefreshAll(); len(errs) != 0 {
		t.Fatalf("second poll, with the profile holding the revoked login: %v", errs)
	}
	if *refreshes != 2 {
		t.Fatalf("refreshed %d times in all, want 2: the backup must keep being refreshed", *refreshes)
	}
	if backup, _ := creds.ReadBackup(2, "b@example.com"); backup != third {
		t.Fatalf("backup = %s, want the newest login", backup)
	}

	shared := snapshot(t, paths.ClaudeCredentialsIn(""), paths.ClaudeConfigIn(""), paths.StateFile())
	if _, _, err := switcher.SwitchTerminal("bb"); err != nil {
		t.Fatalf("SwitchTerminal: %v", err)
	}
	if got := readLogin(t, profileB); !creds.SameLogin(got, third) {
		t.Fatalf("profile = %s, want the backup's newest login", got)
	}
	assertSame(t, shared)
	if os.Getenv(paths.EnvClaudeConfigDir) != profileB {
		t.Fatalf("terminal not scoped to %s: %q", profileB, os.Getenv(paths.EnvClaudeConfigDir))
	}
}

// What worked before the repair still does: a profile holding an older
// login it made itself (cux never revoked it) is left alone while the poll
// refreshes the backup, and a switch to the seat lands on that login with
// nothing written.
func TestPollRefreshesTheBackupBesideAnOlderOwnLoginAndTheSwitchKeepsIt(t *testing.T) {
	home, _ := holderEnv(t)
	own := holderBlob("to", time.Now().Add(-3*time.Hour))
	profileB := chainSeats(t, home, holderBlob("tb", time.Now().Add(-time.Hour)), own)
	setUpProfile(t, profileB)
	refreshes := issueInOrder(t, holderBlob("tn", time.Now().Add(8*time.Hour)))
	watched := snapshot(t, paths.ClaudeCredentialsIn(profileB), paths.ClaudeConfigIn(profileB))

	if _, errs := RefreshAll(); len(errs) != 0 {
		t.Fatalf("RefreshAll: %v", errs)
	}
	if *refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", *refreshes)
	}
	if revoked, _ := creds.RevokedByRefresh(2, "b@example.com", own); revoked {
		t.Fatal("the profile's own login is on the record of tokens cux revoked")
	}
	assertSame(t, watched)

	shared := snapshot(t, paths.ClaudeCredentialsIn(""), paths.ClaudeConfigIn(""), paths.StateFile())
	if _, _, err := switcher.SwitchTerminal("bb"); err != nil {
		t.Fatalf("SwitchTerminal: %v", err)
	}
	assertSame(t, watched)
	assertSame(t, shared)
}

// A record that cannot be written does not fail the poll: the refreshed
// token is saved and handed to the shared login, and the reading stands.
// Only a later repair is lost.
func TestPollKeepsItsReadingWhenTheRevokedRecordCannotBeWritten(t *testing.T) {
	_, refreshes := holderEnv(t)
	holderSeats(t, store.Account{Slot: 1, Email: "a@example.com", UUID: "u-a"})
	stale := holderBlob("ta", time.Now().Add(-time.Minute))
	if err := creds.WriteBackup(1, "a@example.com", stale); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, "", "a@example.com", "u-a", stale)
	record := filepath.Join(paths.AccountDir(1, "a@example.com"), "revoked-refresh-tokens.json")
	if err := os.MkdirAll(filepath.Join(record, "blocked"), 0o700); err != nil {
		t.Fatal(err)
	}

	cache, errs := RefreshAll()
	if len(errs) != 0 {
		t.Fatalf("RefreshAll: %v", errs)
	}
	if *refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", *refreshes)
	}
	backup, _ := creds.ReadBackup(1, "a@example.com")
	if creds.SameLogin(backup, stale) {
		t.Fatal("the refreshed login was not saved")
	}
	if shared := readLogin(t, ""); !creds.SameLogin(shared, backup) {
		t.Fatalf("shared = %s, want the refreshed login handed over", shared)
	}
	if _, ok := cache[store.Account{Slot: 1, Email: "a@example.com", UUID: "u-a"}.CacheKey()]; !ok {
		t.Fatalf("cache = %+v, want the poll's reading", cache)
	}
}
