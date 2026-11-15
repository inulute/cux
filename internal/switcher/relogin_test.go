package switcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

// writeLogin writes what Claude Code leaves in a config directory after a
// login: the credentials file and the oauthAccount block in .claude.json.
func writeLogin(t *testing.T, dir, email, uuid, org, token string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blobFor(token)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"oauthAccount": map[string]any{
		"emailAddress":     email,
		"accountUuid":      uuid,
		"organizationUuid": org,
	}}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// addSeat makes (email, uuid, org) the live login and captures it.
func addSeat(t *testing.T, email, uuid, org, token string) store.Account {
	t.Helper()
	writeLogin(t, paths.ClaudeDir(), email, uuid, org, token)
	acct, _, err := AddCurrent(0, "", true, false)
	if err != nil {
		t.Fatalf("add %s: %v", email, err)
	}
	return acct
}

// twoSeats builds a pool with one email in two organizations — the case an
// email-only check gets wrong — and leaves slot 1 live and active.
func twoSeats(t *testing.T) (live, other store.Account) {
	t.Helper()
	isolate(t)
	live = addSeat(t, "user@example.com", "uuid-1", "org-work", "token-work")
	other = addSeat(t, "user@example.com", "uuid-1", "org-personal", "token-personal")
	addSeat(t, "user@example.com", "uuid-1", "org-work", "token-work")
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveSlot != live.Slot {
		t.Fatalf("setup: active slot = %d, want %d", st.ActiveSlot, live.Slot)
	}
	return live, other
}

func readLiveCreds(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(paths.ClaudeCredentials())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertNoStagingLeft(t *testing.T) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(paths.RuntimeDir(), "login-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("staging directories left behind: %v", left)
	}
}

func TestReloginRefreshesAnInactiveSlotAndLeavesTheActiveOneAlone(t *testing.T) {
	live, other := twoSeats(t)
	if err := usage.SaveCache(usage.Cache{
		other.CacheKey(): {TokenExpired: true},
		live.CacheKey():  {Failures: 0},
	}); err != nil {
		t.Fatal(err)
	}
	liveBefore := readLiveCreds(t)

	var out bytes.Buffer
	res, err := Relogin("2", func(dir, email string) error {
		if email != other.Email {
			t.Errorf("login pre-filled %q, want %q", email, other.Email)
		}
		if !strings.HasPrefix(dir, paths.RuntimeDir()) {
			t.Errorf("login dir %s is outside cux's runtime dir", dir)
		}
		writeLogin(t, dir, other.Email, other.UUID, other.OrgUUID, "token-personal-new")
		// A running session refreshing the live login meanwhile must not
		// make the import fail: the check is on identity, not file bytes.
		writeLogin(t, paths.ClaudeDir(), live.Email, live.UUID, live.OrgUUID, "token-work-refreshed")
		return nil
	}, &out)
	if err != nil {
		t.Fatalf("Relogin: %v", err)
	}
	if res.Live || res.Account.Slot != other.Slot {
		t.Errorf("result = %+v, want slot %d, not live", res, other.Slot)
	}

	got, err := creds.ReadBackup(other.Slot, other.Email)
	if err != nil || got != blobFor("token-personal-new") {
		t.Errorf("slot %d backup = %q, %v; want the new login", other.Slot, got, err)
	}
	if live := readLiveCreds(t); live == liveBefore || live != blobFor("token-work-refreshed") {
		t.Errorf("live credentials = %q; want the session's own refresh, untouched by relogin", live)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveSlot != live.Slot {
		t.Errorf("active slot = %d, want %d unchanged", st.ActiveSlot, live.Slot)
	}

	cache, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cache[other.CacheKey()]; ok {
		t.Error("the expired reading for the relogged slot is still cached")
	}
	if _, ok := cache[live.CacheKey()]; !ok {
		t.Error("the reading for another slot was dropped")
	}

	if !strings.Contains(out.String(), "Login finished") {
		t.Errorf("no line confirming the login returned; output:\n%s", out.String())
	}
	assertNoStagingLeft(t)
}

func TestReloginRefusesALoginInTheWrongOrganization(t *testing.T) {
	_, other := twoSeats(t)
	before, _ := creds.ReadBackup(other.Slot, other.Email)

	_, err := Relogin("2", func(dir, email string) error {
		// Same email, but the browser picked the other organization.
		writeLogin(t, dir, other.Email, other.UUID, "org-work", "token-wrong")
		return nil
	}, &bytes.Buffer{})
	if !errors.Is(err, ErrReloginWrongAccount) {
		t.Fatalf("Relogin error = %v, want ErrReloginWrongAccount", err)
	}
	if after, _ := creds.ReadBackup(other.Slot, other.Email); after != before {
		t.Errorf("slot backup changed on a refused login: %q", after)
	}
	assertNoStagingLeft(t)
}

func TestReloginRefusesAnotherEmail(t *testing.T) {
	_, other := twoSeats(t)
	_, err := Relogin("2", func(dir, email string) error {
		writeLogin(t, dir, "someone@example.com", "uuid-9", other.OrgUUID, "token-x")
		return nil
	}, &bytes.Buffer{})
	if !errors.Is(err, ErrReloginWrongAccount) {
		t.Fatalf("Relogin error = %v, want ErrReloginWrongAccount", err)
	}
	assertNoStagingLeft(t)
}

func TestReloginChangesNothingWhenTheLoginFails(t *testing.T) {
	_, other := twoSeats(t)
	before, _ := creds.ReadBackup(other.Slot, other.Email)

	var out bytes.Buffer
	_, err := Relogin("2", func(dir, email string) error {
		return errors.New("exit status 1")
	}, &out)
	if err == nil {
		t.Fatal("Relogin succeeded after a failed login")
	}
	if after, _ := creds.ReadBackup(other.Slot, other.Email); after != before {
		t.Errorf("slot backup changed after a failed login: %q", after)
	}
	if !strings.Contains(out.String(), "did not complete") {
		t.Errorf("no line saying the login failed; output:\n%s", out.String())
	}
	assertNoStagingLeft(t)
}

func TestReloginOfTheLiveSlotReplacesTheLiveLogin(t *testing.T) {
	live, _ := twoSeats(t)

	res, err := Relogin("1", func(dir, email string) error {
		writeLogin(t, dir, live.Email, live.UUID, live.OrgUUID, "token-work-new")
		return nil
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Relogin: %v", err)
	}
	if !res.Live {
		t.Error("the live slot was not reported as live")
	}
	if got := readLiveCreds(t); got != blobFor("token-work-new") {
		t.Errorf("live credentials = %q, want the new login", got)
	}
	if got, _ := creds.ReadBackup(live.Slot, live.Email); got != blobFor("token-work-new") {
		t.Errorf("slot backup = %q, want the new login", got)
	}
	assertNoStagingLeft(t)
}
