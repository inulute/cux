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
	if err := store.WriteOAuthBlockBackup(a.Slot, a.Email, json.RawMessage(`{"emailAddress":"b@example.com"}`)); err != nil {
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

// revoked records that cux refreshed a's stored login from old, as the
// monitor does: old's refresh token is then a revoked one.
func revoked(t *testing.T, a store.Account, old string) {
	t.Helper()
	if err := creds.RecordRevoked(a.Slot, a.Email, old, blob("rotated-away", time.Now())); err != nil {
		t.Fatal(err)
	}
}

// A profile still holding the token cux's refresh of the backup revoked
// cannot refresh it. The backup's newer token, live nowhere else, replaces
// it.
func TestNeedsCopyRevivesADeadProfileLogin(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
	dead := blob("dead", now.Add(-time.Hour))
	login(t, Dir("bb"), a.Email, dead)
	revoked(t, a, dead)
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || !need {
		t.Fatalf("NeedsCopy = %v, %v; want the backup copied over the dead login", need, err)
	}
	if plan, err := PlanCopy(state, a, Dir("bb")); err != nil || plan != RepairLogin {
		t.Fatalf("PlanCopy = %v, %v; want RepairLogin", plan, err)
	}
}

// A profile that logged in on its own and sat idle past its access token's
// expiry is not dead: its claude refreshes it. cux never revoked it, so it
// is kept, as it was before the repair existed.
func TestNeedsCopyKeepsAnIdleOwnLogin(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
	login(t, Dir("bb"), a.Email, blob("idle", now.Add(-10*time.Minute)))
	revoked(t, a, blob("some-other-old-token", now.Add(-2*time.Hour)))
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
		t.Fatalf("NeedsCopy = %v, %v; want the profile's own login kept", need, err)
	}
}

// Even a login cux revoked is kept when the repair has no usable
// replacement: the access token has not expired yet, the backup has not
// moved on, or it cannot refresh.
func TestNeedsCopyKeepsTheProfilesOwnLogin(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name, own, stored string
	}{
		{"live, older backup", blob("own", now.Add(time.Hour)), blob("old", now.Add(-time.Hour))},
		{"live, newer backup", blob("own", now.Add(time.Hour)), blob("fresh", now.Add(8*time.Hour))},
		{"expired, backup expired too", blob("own", now.Add(-time.Hour)), blob("old", now.Add(-2*time.Hour))},
		{"expired, backup holds the same token", blob("own", now.Add(-time.Hour)), blob("own", now.Add(-time.Hour))},
		{"expired, backup still on the revoked refresh token", blob("own", now.Add(-time.Hour)),
			fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"later","refreshToken":"r-own","expiresAt":%d}}`, now.Add(8*time.Hour).UnixMilli())},
		{"no expiry recorded", `{"claudeAiOauth":{"accessToken":"own","refreshToken":"r-own"}}`, blob("fresh", now.Add(8*time.Hour))},
		{"expired, backup cannot refresh", blob("own", now.Add(-time.Hour)),
			fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"fresh","expiresAt":%d}}`, now.Add(8*time.Hour).UnixMilli())},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, a := seat(t, c.stored)
			login(t, Dir("bb"), a.Email, c.own)
			revoked(t, a, c.own)
			if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
				t.Fatalf("NeedsCopy = %v, %v; want the profile's own login kept", need, err)
			}
		})
	}
}

// A dead login whose repair would put a refresh token in two places is left
// as it is, without an error: the switch lands on the profile's own login,
// as it did before the repair existed. That covers the backup's token live
// in another directory, and the seat logged in elsewhere on another token,
// which the backup may follow.
func TestNeedsCopyLeavesADeadLoginWhenTheSeatIsLiveElsewhere(t *testing.T) {
	now := time.Now()
	fresh := blob("fresh", now.Add(8*time.Hour))
	dead := blob("dead", now.Add(-time.Hour))

	state, a := seat(t, fresh)
	login(t, Dir("bb"), a.Email, dead)
	revoked(t, a, dead)
	login(t, Dir("spare"), "someone-else@example.com", fresh)
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
		t.Fatalf("token live elsewhere: NeedsCopy = %v, %v; want no copy and no error", need, err)
	}

	state, a = seat(t, fresh)
	login(t, Dir("bb"), a.Email, dead)
	revoked(t, a, dead)
	login(t, "", a.Email, blob("other", now.Add(9*time.Hour)))
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
		t.Fatalf("seat live elsewhere: NeedsCopy = %v, %v; want no copy and no error", need, err)
	}
}

// The identity file names the seat but the credentials are another managed
// seat's stored login (an identity file lagging its credentials, issue #46).
// That is not the seat's dead login: it is not overwritten, and the switch
// is not refused either.
func TestNeedsCopyLeavesAnotherSeatsLoginAlone(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
	other := store.Account{Slot: 2, Email: "c@example.com", Alias: "cc"}
	state.Accounts[2] = other
	// A drifted sequence of the same length (slot 9 does not exist) hides
	// slot 2 from the rotation order; its stored login still counts.
	state.Sequence = []int{1, 9}
	foreign := blob("c-login", now.Add(-time.Hour))
	if err := creds.WriteBackup(other.Slot, other.Email, foreign); err != nil {
		t.Fatal(err)
	}
	login(t, Dir("bb"), a.Email, foreign)
	revoked(t, a, foreign)
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
		t.Fatalf("NeedsCopy = %v, %v; want another seat's login left alone", need, err)
	}
}

// An unreadable backup or revocation record does not block a switch onto
// the profile's own login: the repair is skipped, as if it did not exist.
func TestNeedsCopyKeepsAnExpiredLoginWhenTheRecordsCannotBeRead(t *testing.T) {
	now := time.Now()
	dead := blob("dead", now.Add(-time.Hour))
	for _, broken := range []string{"credentials.json", "revoked-refresh-tokens.json", "oauth.json"} {
		t.Run(broken, func(t *testing.T) {
			state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
			login(t, Dir("bb"), a.Email, dead)
			revoked(t, a, dead)
			p := filepath.Join(paths.AccountDir(a.Slot, a.Email), broken)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
				t.Fatalf("NeedsCopy = %v, %v; want the profile's own login kept", need, err)
			}
		})
	}
}

// The terminal's own shared directory is never repaired by a terminal
// switch, even when it is the seat's profile and its login is dead.
func TestNeedsCopyLeavesADeadSharedLoginAlone(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
	dead := blob("dead", now.Add(-time.Hour))
	login(t, Dir("bb"), a.Email, dead)
	revoked(t, a, dead)
	t.Setenv(paths.EnvClaudeConfigDir, Dir("bb"))
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
		t.Fatalf("NeedsCopy = %v, %v; want the shared login left alone", need, err)
	}
}

// Another managed seat's stored login that cannot be read might be the
// profile's login. The repair is skipped, and the switch is not refused.
func TestNeedsCopyLeavesADeadLoginWhenAnotherSeatsBackupCannotBeRead(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
	other := store.Account{Slot: 2, Email: "c@example.com", Alias: "cc"}
	state.Accounts[2] = other
	state.Sequence = []int{1, 2}
	if err := creds.WriteBackup(other.Slot, other.Email, blob("c-login", now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(paths.AccountDir(other.Slot, other.Email), "credentials.json")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	dead := blob("dead", now.Add(-time.Hour))
	login(t, Dir("bb"), a.Email, dead)
	revoked(t, a, dead)
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
		t.Fatalf("NeedsCopy = %v, %v; want the profile's own login kept", need, err)
	}
}

// A stored login edited by hand to carry no access token cannot be copied.
// The repair is skipped, and the switch is not refused.
func TestNeedsCopyKeepsAnExpiredLoginWhenTheBackupHasNoAccessToken(t *testing.T) {
	now := time.Now()
	state, a := seat(t, blob("fresh", now.Add(8*time.Hour)))
	dead := blob("dead", now.Add(-time.Hour))
	login(t, Dir("bb"), a.Email, dead)
	revoked(t, a, dead)
	tokenless := fmt.Sprintf(`{"claudeAiOauth":{"refreshToken":"r-fresh","expiresAt":%d}}`, now.Add(8*time.Hour).UnixMilli())
	p := filepath.Join(paths.AccountDir(a.Slot, a.Email), "credentials.json")
	if err := os.WriteFile(p, []byte(tokenless), 0o600); err != nil {
		t.Fatal(err)
	}
	if need, err := NeedsCopy(state, a, Dir("bb")); err != nil || need {
		t.Fatalf("NeedsCopy = %v, %v; want the profile's own login kept", need, err)
	}
}
