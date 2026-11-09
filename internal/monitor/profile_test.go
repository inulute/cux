package monitor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inulute/cux/internal/creds"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/store"
)

func loginAt(t *testing.T, configDir, email, blob string) {
	t.Helper()
	if err := os.MkdirAll(paths.ClaudeDirIn(configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ClaudeCredentialsIn(configDir), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":"uuid-1"}}`, email)
	if err := os.WriteFile(paths.ClaudeConfigIn(configDir), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A seat living in its own profile: a poll takes the login the profile's
// claude refreshed into the backup, and does not copy that login into the
// shared directory just because the shared identity file names the seat.
func TestRefreshKeepsAProfileSeatInStepAndOutOfTheSharedLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_USAGE_ENDPOINT", srv.URL)

	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Add(1, "seat@example.com", "uuid-1", ""); err != nil {
		t.Fatal(err)
	}
	a := state.Accounts[1]
	a.Alias = "bb"
	state.Accounts[1] = a
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stored := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t1","refreshToken":"r1","expiresAt":%d}}`, now.Add(time.Hour).UnixMilli())
	if err := creds.WriteBackup(1, "seat@example.com", stored); err != nil {
		t.Fatal(err)
	}
	newer := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t2","refreshToken":"r2","expiresAt":%d}}`, now.Add(2*time.Hour).UnixMilli())
	loginAt(t, filepath.Join(home, ".claude-bb"), "seat@example.com", newer)
	staleShared := `{"claudeAiOauth":{"accessToken":"old"}}`
	loginAt(t, "", "seat@example.com", staleShared)

	if _, errs := RefreshAll(); len(errs) != 0 {
		t.Fatalf("RefreshAll: %v", errs)
	}
	backup, err := creds.ReadBackup(1, "seat@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !creds.SameLogin(backup, newer) {
		t.Fatalf("backup = %s, want the profile's refreshed login", backup)
	}
	if b, _ := os.ReadFile(paths.ClaudeCredentialsIn("")); string(b) != staleShared {
		t.Fatalf("the profile's login was copied into the shared directory: %s", b)
	}
}
