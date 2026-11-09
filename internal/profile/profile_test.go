package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/registry"
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
	if h, held, err := HeldElsewhere("", blob("t1", time.Now())); err != nil || !held || h.Dir != Dir("bb") {
		t.Fatalf("HeldElsewhere = %+v, %v, %v", h, held, err)
	}
	if _, held, _ := HeldElsewhere(Dir("bb"), blob("t1", time.Now())); held {
		t.Fatal("the profile itself is not elsewhere")
	}
}

// A directory logged in as the seat with its own token does not hold the
// stored login: refreshing the stored one cannot sign it out.
func TestHeldElsewhereMatchesByTokenNotByIdentity(t *testing.T) {
	stored := blob("t1", time.Now())
	_, a := seat(t, stored)
	login(t, Dir("bb"), a.Email, blob("own", time.Now()))
	if _, held, err := HeldElsewhere("", stored); err != nil || held {
		t.Fatalf("HeldElsewhere = %v, %v; the profile holds another token", held, err)
	}
	login(t, "", a.Email, stored)
	if h, held, err := HeldElsewhere(Dir("bb"), stored); err != nil || !held || h.Dir != "" {
		t.Fatalf("HeldElsewhere from the profile = %+v, %v, %v; want the shared login", h, held, err)
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

// A directory whose identity file names someone else while its credentials
// carry the stored token (a switch caught half way, issue #46) still runs on
// that token.
func TestHeldElsewhereIgnoresALaggingIdentityFile(t *testing.T) {
	stored := blob("t1", time.Now())
	_, a := seat(t, stored)
	login(t, Dir("spare"), "someone-else@example.com", stored)
	if h, held, err := HeldElsewhere(Dir(a.Alias), stored); err != nil || !held || h.Dir != Dir("spare") {
		t.Fatalf("HeldElsewhere = %+v, %v, %v; want %s", h, held, err, Dir("spare"))
	}
}

// A seat's claude in another directory refreshed on its own: its login
// outlives the backup, and is reported. An older one is not.
func TestNewerElsewhereComparesExpiry(t *testing.T) {
	now := time.Now()
	stored := blob("t1", now)
	state, a := seat(t, stored)
	login(t, "", a.Email, blob("t0", now.Add(-time.Hour)))
	if _, ok, err := NewerElsewhere(state, a.Slot, Dir("bb"), stored); err != nil || ok {
		t.Fatalf("NewerElsewhere = %v, %v; the shared login is older than the backup", ok, err)
	}
	login(t, "", a.Email, blob("t2", now.Add(time.Hour)))
	if h, ok, err := NewerElsewhere(state, a.Slot, Dir("bb"), stored); err != nil || !ok || h.Dir != "" {
		t.Fatalf("NewerElsewhere = %+v, %v, %v; want the shared login", h, ok, err)
	}
	if _, ok, _ := NewerElsewhere(state, a.Slot, "", stored); ok {
		t.Fatal("this process's own directory is not elsewhere")
	}
}

// A running wrapper's directory is looked in even when it is outside
// $HOME/.claude and $HOME/.claude-*.
func TestConfigDirsIncludesARunningWrappersDirectory(t *testing.T) {
	seat(t, blob("t1", time.Now()))
	custom := t.TempDir()
	registry.UpdateSelf(func(e *registry.Entry) { e.ConfigDir = custom })
	t.Cleanup(registry.RemoveSelf)
	for _, d := range ConfigDirs() {
		if d == custom {
			return
		}
	}
	t.Fatalf("ConfigDirs() = %v, want %s in it", ConfigDirs(), custom)
}

// The terminal's own shared directory: its CLAUDE_CONFIG_DIR until a
// scoped switch, and the one recorded by that switch afterwards.
func TestSharedDirFollowsTheScope(t *testing.T) {
	seat(t, blob("t1", time.Now()))
	t.Setenv(paths.EnvClaudeConfigDir, Dir("bb"))
	if got := SharedDir(); got != Dir("bb") {
		t.Fatalf("unscoped SharedDir() = %q", got)
	}
	t.Setenv(EnvScope, ScopeTerminal)
	t.Setenv(EnvSharedConfigDir, "/somewhere")
	if got := SharedDir(); got != "/somewhere" {
		t.Fatalf("scoped SharedDir() = %q", got)
	}
}

// A profile logged in as the seat whose token expired after the backup was
// refreshed from it holds a revoked login its claude cannot refresh. The
// backup's newer token, live nowhere else, replaces it.
func TestNeedsCopyRevivesADeadProfileLogin(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
	login(t, Dir("bb"), a.Email, blob("dead", now.Add(-time.Hour)))
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || !need {
		t.Fatalf("NeedsCopy = %v, %v; want the backup copied over the dead login", need, err)
	}
}

// A profile whose login is still live, or expired without the backup having
// moved on, keeps its own login.
func TestNeedsCopyKeepsTheProfilesOwnLogin(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name, own, stored string
	}{
		{"live, older backup", blob("own", now.Add(time.Hour)), blob("old", now.Add(-time.Hour))},
		{"live, newer backup", blob("own", now.Add(time.Hour)), blob("fresh", now.Add(8*time.Hour))},
		{"expired, backup expired too", blob("own", now.Add(-time.Hour)), blob("old", now.Add(-2*time.Hour))},
		{"expired, backup holds the same token", blob("own", now.Add(-time.Hour)), blob("own", now.Add(-time.Hour))},
		{"no expiry recorded", `{"claudeAiOauth":{"accessToken":"own","refreshToken":"r-own"}}`, blob("fresh", now.Add(8*time.Hour))},
		{"expired, backup cannot refresh", blob("own", now.Add(-time.Hour)),
			fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"fresh","expiresAt":%d}}`, now.Add(8*time.Hour).UnixMilli())},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, a := seat(t, c.stored)
			login(t, Dir("bb"), a.Email, c.own)
			if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
				t.Fatalf("NeedsCopy = %v, %v; want the profile's own login kept", need, err)
			}
		})
	}
}

// The backup's newer token is live in another directory: copying it over
// the dead login would leave it in two places, so the copy is refused. So is
// a seat logged in elsewhere on another token, which the backup may follow.
func TestNeedsCopyRefusesToReviveWithATokenLiveElsewhere(t *testing.T) {
	now := time.Now()
	fresh := blob("fresh", now.Add(8*time.Hour))
	state, a := seat(t, fresh)
	login(t, Dir("bb"), a.Email, blob("dead", now.Add(-time.Hour)))
	login(t, Dir("spare"), "someone-else@example.com", fresh)
	if _, err := NeedsCopy(state, a, Dir("bb")); err == nil || !strings.Contains(err.Error(), Dir("spare")) ||
		!strings.Contains(err.Error(), "two places") {
		t.Fatalf("NeedsCopy = %v, want a refusal naming %s", err, Dir("spare"))
	}

	state, a = seat(t, fresh)
	login(t, Dir("bb"), a.Email, blob("dead", now.Add(-time.Hour)))
	login(t, "", a.Email, blob("other", now.Add(9*time.Hour)))
	if _, err := NeedsCopy(state, a, Dir("bb")); err == nil || !strings.Contains(err.Error(), "also logged in") {
		t.Fatalf("NeedsCopy = %v, want a refusal naming the shared login", err)
	}
}

// The identity file names the seat but the credentials are another managed
// seat's stored login (an identity file lagging its credentials, issue #46).
// That is not the seat's dead login, and it is not overwritten.
func TestNeedsCopyRefusesToReviveAnotherSeatsLogin(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
	other := store.Account{Slot: 2, Email: "c@example.com", Alias: "cc"}
	state.Accounts[2] = other
	state.Sequence = append(state.Sequence, 2)
	foreign := blob("c-login", now.Add(-time.Hour))
	if err := creds.WriteBackup(other.Slot, other.Email, foreign); err != nil {
		t.Fatal(err)
	}
	login(t, Dir("bb"), a.Email, foreign)
	if _, err := NeedsCopy(state, a, Dir("bb")); err == nil || !strings.Contains(err.Error(), other.Email) {
		t.Fatalf("NeedsCopy = %v, want a refusal naming %s", err, other.Email)
	}
}

// The terminal's own shared directory is never repaired by a terminal
// switch, even when it is the seat's profile and its login is dead.
func TestNeedsCopyLeavesADeadSharedLoginAlone(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
	login(t, Dir("bb"), a.Email, blob("dead", now.Add(-time.Hour)))
	t.Setenv(paths.EnvClaudeConfigDir, Dir("bb"))
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
		t.Fatalf("NeedsCopy = %v, %v; want the shared login left alone", need, err)
	}
}
