package monitor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/profile"
	"github.com/inulute/cux/internal/store"
)

// holderEnv sets up an isolated home with a usage endpoint that answers and
// counts every token refresh the monitor makes.
func holderEnv(t *testing.T) (home string, refreshes *int32) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_USAGE_ENDPOINT", srv.URL)
	t.Setenv(paths.EnvClaudeConfigDir, "")
	t.Setenv(profile.EnvScope, "")
	t.Setenv(profile.EnvSharedConfigDir, "")
	var n int32
	refreshes = &n
	orig := refreshBlob
	refreshBlob = func(blob string) (string, error) {
		k := atomic.AddInt32(&n, 1)
		return holderBlob(fmt.Sprintf("fresh%d", k), time.Now().Add(8*time.Hour)), nil
	}
	t.Cleanup(func() { refreshBlob = orig })
	return home, refreshes
}

func holderBlob(token string, expires time.Time) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"r-%s","expiresAt":%d}}`,
		token, token, expires.UnixMilli())
}

func holderLogin(t *testing.T, configDir, email, uuid, blob string) {
	t.Helper()
	if err := os.MkdirAll(paths.ClaudeDirIn(configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClaudeCredentialsIn(configDir), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":%q}}`, email, uuid)
	if err := os.WriteFile(paths.ClaudeConfigIn(configDir), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func holderSeats(t *testing.T, accounts ...store.Account) {
	t.Helper()
	st := &store.State{ActiveSlot: accounts[0].Slot, Accounts: map[int]store.Account{}}
	for _, a := range accounts {
		st.Sequence = append(st.Sequence, a.Slot)
		st.Accounts[a.Slot] = a
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
}

func readLogin(t *testing.T, configDir string) string {
	t.Helper()
	b, err := os.ReadFile(paths.ClaudeCredentialsIn(configDir))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A terminal scoped on B's profile polls every seat, A included. A's backup
// holds the token the shared login runs on; a refresh from here would
// revoke it there, and this process cannot write the shared login. So A is
// polled without a refresh, and both copies keep the one token.
func TestScopedPollDoesNotRefreshTheSharedLogin(t *testing.T) {
	home, refreshes := holderEnv(t)
	holderSeats(t,
		store.Account{Slot: 1, Email: "a@example.com", UUID: "u-a"},
		store.Account{Slot: 2, Email: "b@example.com", UUID: "u-b", Alias: "bb"})
	sharedA := holderBlob("ta", time.Now().Add(-time.Minute))
	if err := creds.WriteBackup(1, "a@example.com", sharedA); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, "", "a@example.com", "u-a", sharedA)
	profileB := filepath.Join(home, ".claude-bb")
	ownB := holderBlob("tb", time.Now().Add(time.Hour))
	if err := creds.WriteBackup(2, "b@example.com", ownB); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, profileB, "b@example.com", "u-b", ownB)
	t.Setenv(paths.EnvClaudeConfigDir, profileB)
	t.Setenv(profile.EnvScope, profile.ScopeTerminal)

	RefreshAll()

	if *refreshes != 0 {
		t.Fatalf("%d refreshes from a scoped terminal; the shared login's token would be revoked", *refreshes)
	}
	backupA, _ := creds.ReadBackup(1, "a@example.com")
	if shared := readLogin(t, ""); shared != sharedA || backupA != sharedA {
		t.Fatalf("shared = %s, backup = %s; want both left on %s", shared, backupA, sharedA)
	}
}

// The shared login runs B on the backup's token while B's profile ran its
// own /login. The profile's login says it is B, but it does not hold the
// backup's token, so it does not stop cux refreshing the shared login — and
// the shared login must be handed the new token.
func TestRefreshSyncsTheSharedLoginWhenTheProfileHasItsOwnLogin(t *testing.T) {
	home, refreshes := holderEnv(t)
	holderSeats(t, store.Account{Slot: 2, Email: "b@example.com", UUID: "u-b", Alias: "bb"})
	sharedB := holderBlob("ts", time.Now().Add(-time.Minute))
	if err := creds.WriteBackup(2, "b@example.com", sharedB); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, "", "b@example.com", "u-b", sharedB)
	ownProfile := holderBlob("tp", time.Now().Add(-2*time.Minute))
	profileB := filepath.Join(home, ".claude-bb")
	holderLogin(t, profileB, "b@example.com", "u-b", ownProfile)

	if _, errs := RefreshAll(); len(errs) != 0 {
		t.Fatalf("RefreshAll: %v", errs)
	}

	if *refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", *refreshes)
	}
	backup, _ := creds.ReadBackup(2, "b@example.com")
	if shared := readLogin(t, ""); !creds.SameLogin(shared, backup) || creds.SameLogin(backup, sharedB) {
		t.Fatalf("shared = %s, backup = %s; the shared login must get the refreshed token", shared, backup)
	}
	if got := readLogin(t, profileB); got != ownProfile {
		t.Fatalf("the profile's own login was replaced: %s", got)
	}
}

// A seat that lives in its own profile is that profile's claude's to
// refresh. Polled from the shared terminal it is never refreshed, even with
// its token about to expire: the reading waits for that claude.
func TestPollFromTheSharedTerminalDoesNotRefreshAProfileSeat(t *testing.T) {
	home, refreshes := holderEnv(t)
	holderSeats(t,
		store.Account{Slot: 1, Email: "a@example.com", UUID: "u-a"},
		store.Account{Slot: 2, Email: "b@example.com", UUID: "u-b", Alias: "bb"})
	sharedA := holderBlob("ta", time.Now().Add(time.Hour))
	if err := creds.WriteBackup(1, "a@example.com", sharedA); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, "", "a@example.com", "u-a", sharedA)
	nearExpiry := holderBlob("tb", time.Now().Add(time.Minute))
	if err := creds.WriteBackup(2, "b@example.com", nearExpiry); err != nil {
		t.Fatal(err)
	}
	profileB := filepath.Join(home, ".claude-bb")
	holderLogin(t, profileB, "b@example.com", "u-b", nearExpiry)

	RefreshAll()

	if *refreshes != 0 {
		t.Fatalf("%d refreshes of a seat its profile's claude holds", *refreshes)
	}
	backupB, _ := creds.ReadBackup(2, "b@example.com")
	if got := readLogin(t, profileB); got != nearExpiry || backupB != nearExpiry {
		t.Fatalf("profile = %s, backup = %s; want both left on %s", got, backupB, nearExpiry)
	}
}

// A seat no other directory holds is still refreshed here, as before.
func TestRefreshStillRunsForASeatNoOtherDirectoryHolds(t *testing.T) {
	_, refreshes := holderEnv(t)
	holderSeats(t, store.Account{Slot: 1, Email: "a@example.com", UUID: "u-a"})
	stale := holderBlob("ta", time.Now().Add(-time.Minute))
	if err := creds.WriteBackup(1, "a@example.com", stale); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, "", "a@example.com", "u-a", stale)

	if _, errs := RefreshAll(); len(errs) != 0 {
		t.Fatalf("RefreshAll: %v", errs)
	}
	if *refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", *refreshes)
	}
	backup, _ := creds.ReadBackup(1, "a@example.com")
	if shared := readLogin(t, ""); !creds.SameLogin(shared, backup) || creds.SameLogin(backup, stale) {
		t.Fatalf("shared = %s, backup = %s; want both on the refreshed token", shared, backup)
	}
}

// The shared claude refreshed A on its own, so A's backup holds a token
// the server has already revoked. A terminal scoped on B's profile does not
// retry that dead token on every poll: it reads A's usage with the shared
// login's newer one and leaves the backup for the shared terminal to adopt.
func TestScopedPollDoesNotRetryABackupTheSharedLoginOutgrew(t *testing.T) {
	home, refreshes := holderEnv(t)
	holderSeats(t,
		store.Account{Slot: 1, Email: "a@example.com", UUID: "u-a"},
		store.Account{Slot: 2, Email: "b@example.com", UUID: "u-b", Alias: "bb"})
	staleA := holderBlob("ta", time.Now().Add(-time.Hour))
	if err := creds.WriteBackup(1, "a@example.com", staleA); err != nil {
		t.Fatal(err)
	}
	rotatedA := holderBlob("ta2", time.Now().Add(7*time.Hour))
	holderLogin(t, "", "a@example.com", "u-a", rotatedA)
	profileB := filepath.Join(home, ".claude-bb")
	ownB := holderBlob("tb", time.Now().Add(time.Hour))
	if err := creds.WriteBackup(2, "b@example.com", ownB); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, profileB, "b@example.com", "u-b", ownB)
	t.Setenv(paths.EnvClaudeConfigDir, profileB)
	t.Setenv(profile.EnvScope, profile.ScopeTerminal)

	if _, errs := RefreshAll(); len(errs) != 0 {
		t.Fatalf("RefreshAll: %v", errs)
	}
	if *refreshes != 0 {
		t.Fatalf("%d refreshes of a backup the shared login has outgrown", *refreshes)
	}
	backupA, _ := creds.ReadBackup(1, "a@example.com")
	if shared := readLogin(t, ""); shared != rotatedA || backupA != staleA {
		t.Fatalf("shared = %s, backup = %s; a scoped poll must write neither", shared, backupA)
	}
}

// The shared terminal runs B on the backup's token, which has expired, and
// B's profile holds a newer login of its own. The backup's token is this
// process's, so it is still refreshed here and handed to the shared login;
// the profile's newer login does not stand in for it.
func TestSharedTerminalStillRefreshesItsOwnLoginBesideANewerProfileLogin(t *testing.T) {
	home, refreshes := holderEnv(t)
	holderSeats(t, store.Account{Slot: 2, Email: "b@example.com", UUID: "u-b", Alias: "bb"})
	sharedB := holderBlob("ts", time.Now().Add(-time.Minute))
	if err := creds.WriteBackup(2, "b@example.com", sharedB); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, "", "b@example.com", "u-b", sharedB)
	ownProfile := holderBlob("tp", time.Now().Add(5*time.Hour))
	profileB := filepath.Join(home, ".claude-bb")
	holderLogin(t, profileB, "b@example.com", "u-b", ownProfile)

	if _, errs := RefreshAll(); len(errs) != 0 {
		t.Fatalf("RefreshAll: %v", errs)
	}
	if *refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", *refreshes)
	}
	backup, _ := creds.ReadBackup(2, "b@example.com")
	if shared := readLogin(t, ""); !creds.SameLogin(shared, backup) || creds.SameLogin(backup, sharedB) {
		t.Fatalf("shared = %s, backup = %s; the shared login must get the refreshed token", shared, backup)
	}
	if got := readLogin(t, profileB); got != ownProfile {
		t.Fatalf("the profile's own login was replaced: %s", got)
	}
}

// A refresh records the refresh token it rotated away, and only that one:
// a switch later repairs a profile still holding it, and leaves alone any
// login cux did not revoke.
func TestRefreshRecordsTheTokenItRevoked(t *testing.T) {
	_, refreshes := holderEnv(t)
	holderSeats(t, store.Account{Slot: 1, Email: "a@example.com", UUID: "u-a"})
	stale := holderBlob("ta", time.Now().Add(-time.Minute))
	if err := creds.WriteBackup(1, "a@example.com", stale); err != nil {
		t.Fatal(err)
	}
	holderLogin(t, "", "a@example.com", "u-a", stale)

	if _, errs := RefreshAll(); len(errs) != 0 {
		t.Fatalf("RefreshAll: %v", errs)
	}
	if *refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", *refreshes)
	}
	backup, _ := creds.ReadBackup(1, "a@example.com")
	for _, c := range []struct {
		name, blob string
		want       bool
	}{
		{"the rotated-away token", stale, true},
		{"the refreshed token", backup, false},
		{"a login cux never saw", holderBlob("tz", time.Now().Add(-time.Minute)), false},
	} {
		if got, err := creds.RevokedByRefresh(1, "a@example.com", c.blob); err != nil || got != c.want {
			t.Fatalf("%s: RevokedByRefresh = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}
