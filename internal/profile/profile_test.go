package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
)

func blob(token string, expires time.Time) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"r-%s","expiresAt":%d}}`,
		token, token, expires.UnixMilli())
}

func login(t *testing.T, configDir, email, credentials string) {
	t.Helper()
	if err := os.MkdirAll(paths.ClaudeDirIn(configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClaudeCredentialsIn(configDir), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"oauthAccount": map[string]any{"emailAddress": email}})
	if err := os.WriteFile(paths.ClaudeConfigIn(configDir), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
}

// seat sets up one managed seat "bb" with a stored login and returns it.
func seat(t *testing.T, stored string) (*store.State, store.Account) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv(paths.EnvClaudeConfigDir, "")
	t.Setenv(EnvScope, "")
	t.Setenv(EnvSharedConfigDir, "")
	a := store.Account{Slot: 1, Email: "b@example.com", Alias: "bb"}
	state := &store.State{ActiveSlot: 1, Sequence: []int{1}, Accounts: map[int]store.Account{1: a}}
	if err := creds.WriteBackup(a.Slot, a.Email, stored); err != nil {
		t.Fatal(err)
	}
	return state, a
}

func backupOf(t *testing.T, a store.Account) string {
	t.Helper()
	b, err := creds.ReadBackup(a.Slot, a.Email)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A cux refresh hands the new token to the profile that held the old one,
// so the profile is not left with the token the refresh revoked.
func TestSyncRefreshedFollowsTheProfileHoldingTheOldToken(t *testing.T) {
	now := time.Now()
	old := blob("t1", now)
	state, a := seat(t, old)
	login(t, Dir("bb"), a.Email, `{"claudeAiOauth":{"accessToken":"t1","refreshToken":"r-t1"},"mcpOAuth":{"m":{"refreshToken":"keep"}}}`)
	fresh := blob("t2", now.Add(time.Hour))
	if err := SyncRefreshed(state, a, old, fresh); err != nil {
		t.Fatal(err)
	}
	got, err := creds.ReadLiveIn(Dir("bb"))
	if err != nil {
		t.Fatal(err)
	}
	if !creds.SameLogin(got, fresh) {
		t.Fatalf("profile login = %s, want the refreshed token", got)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got), &doc); err != nil || doc["mcpOAuth"] == nil {
		t.Fatalf("the profile's own MCP login was dropped: %s", got)
	}
}

// A profile logged in on its own holds another login of the seat; cux's
// refresh is none of its business.
func TestSyncRefreshedLeavesAnIndependentProfileLoginAlone(t *testing.T) {
	now := time.Now()
	old := blob("t1", now)
	state, a := seat(t, old)
	own := blob("own", now.Add(2*time.Hour))
	login(t, Dir("bb"), a.Email, own)
	if err := SyncRefreshed(state, a, old, blob("t2", now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got, _ := creds.ReadLiveIn(Dir("bb")); got != own {
		t.Fatalf("an independent profile login was overwritten: %s", got)
	}
}

// When the profile's claude refreshed the login, the backup takes it over;
// its own token was revoked by that refresh.
func TestReconcileTakesTheProfilesNewerLogin(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("t1", now))
	newer := blob("t2", now.Add(time.Hour))
	login(t, Dir("bb"), a.Email, newer)
	if _, err := Reconcile(state, a); err != nil {
		t.Fatal(err)
	}
	if got := backupOf(t, a); !creds.SameLogin(got, newer) {
		t.Fatalf("backup = %s, want the profile's login", got)
	}
}

// A seat also live in the shared login: the backup follows that one, and
// the profile's separate login does not replace it.
func TestReconcileLeavesTheBackupWhenTheSeatIsLiveElsewhere(t *testing.T) {
	now := time.Now()
	stored := blob("shared", now)
	state, a := seat(t, stored)
	login(t, "", a.Email, stored)
	login(t, Dir("bb"), a.Email, blob("own", now.Add(time.Hour)))
	if _, err := Reconcile(state, a); err != nil {
		t.Fatal(err)
	}
	if got := backupOf(t, a); got != stored {
		t.Fatalf("backup = %s, want it left on the shared login", got)
	}
}

// An older profile login never replaces a newer backup.
func TestReconcileKeepsANewerBackup(t *testing.T) {
	now := time.Now()
	stored := blob("t2", now.Add(time.Hour))
	state, a := seat(t, stored)
	login(t, Dir("bb"), a.Email, blob("t1", now))
	if _, err := Reconcile(state, a); err != nil {
		t.Fatal(err)
	}
	if got := backupOf(t, a); got != stored {
		t.Fatalf("backup = %s, want it unchanged", got)
	}
}

func TestHoldersSkipsLoginsThatCannotRefresh(t *testing.T) {
	state, a := seat(t, blob("t1", time.Now()))
	login(t, "", a.Email, `{"claudeAiOauth":{"accessToken":"only"}}`)
	login(t, Dir("bb"), a.Email, blob("t1", time.Now()))
	hs, err := Holders(state, a.Slot)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 || hs[0].Dir != Dir("bb") {
		t.Fatalf("holders = %+v, want only the profile", hs)
	}
	if held, err := HeldByOwnProfile(state, a, ""); err != nil || !held {
		t.Fatalf("HeldByOwnProfile = %v, %v", held, err)
	}
	if held, _ := HeldByOwnProfile(state, a, Dir("bb")); held {
		t.Fatal("the profile itself is not elsewhere")
	}
}

// $HOME/.claude named explicitly is the default directory on a file
// backend, so it is listed once.
func TestConfigDirsCountsOneLocationOnce(t *testing.T) {
	seat(t, blob("t1", time.Now()))
	t.Setenv(paths.EnvClaudeConfigDir, paths.ClaudeDirIn(""))
	n := 0
	for _, d := range ConfigDirs() {
		if creds.LiveLocation(d) == creds.LiveLocation("") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the default login is listed %d times", n)
	}
}
